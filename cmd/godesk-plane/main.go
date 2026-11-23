package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/KriszW/godesk-plane/internal/api"
	"github.com/KriszW/godesk-plane/internal/config"
	"github.com/KriszW/godesk-plane/internal/crypto"
	"github.com/KriszW/godesk-plane/internal/relay"
	"github.com/KriszW/godesk-plane/internal/rendezvous"
	"github.com/KriszW/godesk-plane/internal/store"
	"github.com/KriszW/godesk-plane/internal/telemetry"
	"github.com/KriszW/godesk-plane/internal/web"
)

func main() {
	cfg := config.Load()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tel, err := telemetry.Init(ctx, telemetry.Options{
		ServiceName:    cfg.OTelServiceName,
		ServiceVersion: "1.0.0",
		LogLevel:       cfg.LogLevel,
		LogFormat:      cfg.LogFormat,
		OTelEnabled:    cfg.OTelEnabled,
		OTelEndpoint:   cfg.OTelEndpoint,
		OTelProtocol:   cfg.OTelProtocol,
		OTelInsecure:   cfg.OTelInsecure,
		MetricsEnabled: cfg.MetricsEnabled,
	})
	if err != nil {
		slog.Error("failed to initialize telemetry", "err", err)
		os.Exit(1)
	}
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tel.Shutdown(shutCtx); err != nil {
			slog.Warn("telemetry shutdown error", "err", err)
		}
	}()

	slog.Info("starting GoDesk Plane",
		"component", "main",
		"log_level", cfg.LogLevel,
		"log_format", cfg.LogFormat,
		"otel_enabled", cfg.OTelEnabled && cfg.OTelEndpoint != "",
		"otel_endpoint", cfg.OTelEndpoint,
		"otel_protocol", cfg.OTelProtocol,
		"metrics_enabled", cfg.MetricsEnabled,
	)

	kp, err := crypto.LoadOrGenerate(cfg.DataDir)
	if err != nil {
		slog.Error("failed to initialize Ed25519 keypair", "component", "crypto", "data_dir", cfg.DataDir, "err", err)
		os.Exit(1)
	}
	slog.Info("Ed25519 server keypair ready", "component", "crypto", "public_key", kp.PublicB64, "data_dir", cfg.DataDir)

	dbPath := filepath.Join(cfg.DataDir, "godesk.db")
	st, err := store.Open(dbPath)
	if err != nil {
		slog.Error("failed to open sqlite database", "component", "store", "path", dbPath, "err", err)
		os.Exit(1)
	}
	defer st.Close()
	slog.Info("sqlite database initialized", "component", "store", "path", dbPath)

	if err := st.EnsureAdminUser(cfg.AdminUser, cfg.AdminPassword, cfg.AdminEmail); err != nil {
		slog.Error("failed to ensure initial admin user", "component", "store", "err", err)
		os.Exit(1)
	}

	// Apply any persisted runtime settings overrides
	if v, ok := st.GetSetting("require_key"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.RequireKey = b
		}
	}
	if v, ok := st.GetSetting("always_use_relay"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.AlwaysUseRelay = b
		}
	}
	if v, ok := st.GetSetting("public_host"); ok && v != "" && cfg.PublicHost == "" {
		cfg.PublicHost = v
	}
	if v, ok := st.GetSetting("relay_servers"); ok && v != "" {
		var saved []string
		if json.Unmarshal([]byte(v), &saved) == nil && len(saved) > 0 {
			cfg.RelayServers = saved
		}
	}

	var rdvSrv *rendezvous.Server
	relaySrv := relay.NewServer(relay.Config{
		BindHost: cfg.BindHost,
		TCPPort:  cfg.RelayPort,
		WSPort:   cfg.RelayWS,
		Key:      kp.PublicB64,
		RequireKey: func() bool {
			if rdvSrv != nil {
				return rdvSrv.RequireKey()
			}
			return cfg.RequireKey
		},
	}, st)
	relaySrv.SetMetrics(tel.Metrics)

	rdvSrv = rendezvous.NewServer(rendezvous.Config{
		BindHost:          cfg.BindHost,
		Port:              cfg.RendezvousPort,
		NATPort:           cfg.NATPort,
		WSPort:            cfg.RendezvousWS,
		Serial:            cfg.Serial,
		SoftwareUpdateURL: cfg.SoftwareUpdateURL,
		LocalMask:         cfg.LocalMask,
		LocalIP:           cfg.LocalIP,
		RendezvousServers: cfg.RendezvousServers,
		RelayServers:      cfg.RelayServers,
		RequireKey:        cfg.RequireKey,
		AlwaysUseRelay:    cfg.AlwaysUseRelay,
	}, kp, st, relaySrv)
	rdvSrv.SetMetrics(tel.Metrics)

	if err := tel.Metrics.RegisterPeerGauges(rdvSrv.OnlinePeerCount, rdvSrv.RegisteredPeerCount); err != nil {
		slog.Warn("failed to register peer gauges", "component", "telemetry", "err", err)
	}

	webFS, err := web.DistFS()
	if err != nil {
		slog.Error("failed to load embedded web UI", "component", "web", "err", err)
		os.Exit(1)
	}

	apiSrv := api.NewServer(cfg, kp, st, rdvSrv, relaySrv, webFS)
	apiSrv.SetTelemetry(tel)

	if err := relaySrv.Start(ctx); err != nil {
		slog.Error("failed to start relay server", "component", "relay", "err", err)
		os.Exit(1)
	}
	if err := rdvSrv.Start(ctx); err != nil {
		slog.Error("failed to start rendezvous server", "component", "rendezvous", "err", err)
		os.Exit(1)
	}
	if err := apiSrv.Start(ctx); err != nil {
		slog.Error("failed to start HTTP API server", "component", "http", "err", err)
		os.Exit(1)
	}

	slog.Info("GoDesk Plane is ready",
		"component", "main",
		"http_port", cfg.HTTPPort,
		"nat_port", cfg.NATPort,
		"rendezvous_port", cfg.RendezvousPort,
		"relay_port", cfg.RelayPort,
		"admin_user", cfg.AdminUser,
	)

	<-ctx.Done()
	slog.Info("shutdown signal received, draining listeners and flushing telemetry", "component", "main")
	apiSrv.Close()
	rdvSrv.Close()
	relaySrv.Close()
}
