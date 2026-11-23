package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds all configuration options for GoDesk Plane.
type Config struct {
	// BindHost is the IP address to bind listeners on (empty = all interfaces).
	BindHost string
	// PublicHost is the public hostname or IP advertised to clients (e.g., for relay & setup UI).
	PublicHost string

	// Standard RustDesk ports:
	// 21114: HTTP API & Management Web UI
	// 21115: NAT Type Test & Online Status (TCP)
	// 21116: Rendezvous Server (TCP + UDP)
	// 21117: Relay Server (TCP)
	// 21118: Rendezvous WebSocket (TCP)
	// 21119: Relay WebSocket (TCP)
	HTTPPort       int
	NATPort        int
	RendezvousPort int
	RelayPort      int
	RendezvousWS   int
	RelayWS        int

	// DataDir stores SQLite DB and Ed25519 keys (id_ed25519, id_ed25519.pub).
	DataDir string

	// RelayServers is the list of relay server addresses advertised in PunchHole/FetchLocalAddr.
	// If empty, defaults to PublicHost (or client-configured relay).
	RelayServers []string
	// RendezvousServers is the list of rendezvous servers sent in ConfigUpdate.
	RendezvousServers []string

	// RequireKey enforces key validation on PunchHoleRequest and RequestRelay (like hbbs -k _).
	RequireKey bool
	// AlwaysUseRelay forces all connections through the relay server.
	AlwaysUseRelay bool
	// LocalMask is an optional CIDR (e.g. 192.168.1.0/24) for LAN detection.
	LocalMask string
	// LocalIP is the LAN IP advertised when LocalMask matches.
	LocalIP string

	// Serial number for ConfigUpdate.
	Serial int32

	// Default initial admin credentials (created on first boot if no admin exists).
	AdminUser     string
	AdminPassword string
	AdminEmail    string

	// SoftwareUpdateURL optional URL for SoftwareUpdate responses.
	SoftwareUpdateURL string

	// Logging configuration.
	LogLevel  string // debug, info, warn, error
	LogFormat string // text, json

	// OpenTelemetry & Prometheus observability configuration.
	OTelEnabled     bool
	OTelServiceName string
	OTelEndpoint    string
	OTelProtocol    string // http/protobuf or grpc
	OTelInsecure    bool
	MetricsEnabled  bool // exposes /metrics in Prometheus format
}

// Load reads configuration from environment variables with sensible defaults.
func Load() *Config {
	otelEndpoint := envStr("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	otelEnabled := envBool("OTEL_ENABLED", otelEndpoint != "")

	cfg := &Config{
		BindHost:          envStr("BIND_HOST", ""),
		PublicHost:        envStr("PUBLIC_HOST", ""),
		HTTPPort:          envInt("HTTP_PORT", 21114),
		NATPort:           envInt("NAT_PORT", 21115),
		RendezvousPort:    envInt("RENDEZVOUS_PORT", 21116),
		RelayPort:         envInt("RELAY_PORT", 21117),
		RendezvousWS:      envInt("RENDEZVOUS_WS_PORT", 21118),
		RelayWS:           envInt("RELAY_WS_PORT", 21119),
		DataDir:           envStr("DATA_DIR", "./data"),
		RelayServers:      envList("RELAY_SERVERS"),
		RendezvousServers: envList("RENDEZVOUS_SERVERS"),
		RequireKey:        envBool("REQUIRE_KEY", true),
		AlwaysUseRelay:    envBool("ALWAYS_USE_RELAY", false),
		LocalMask:         envStr("LOCAL_MASK", ""),
		LocalIP:           envStr("LOCAL_IP", ""),
		Serial:            int32(envInt("CONFIG_SERIAL", 0)),
		AdminUser:         envStr("ADMIN_USER", "admin"),
		AdminPassword:     envStr("ADMIN_PASSWORD", "godesk-admin"),
		AdminEmail:        envStr("ADMIN_EMAIL", "admin@localhost"),
		SoftwareUpdateURL: envStr("SOFTWARE_UPDATE_URL", ""),
		LogLevel:          strings.ToLower(envStr("LOG_LEVEL", "info")),
		LogFormat:         strings.ToLower(envStr("LOG_FORMAT", "text")),
		OTelEnabled:       otelEnabled,
		OTelServiceName:   envStr("OTEL_SERVICE_NAME", "godesk-plane"),
		OTelEndpoint:      otelEndpoint,
		OTelProtocol:      strings.ToLower(envStr("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")),
		OTelInsecure:      envBool("OTEL_EXPORTER_OTLP_INSECURE", !strings.HasPrefix(strings.ToLower(otelEndpoint), "https://")),
		MetricsEnabled:    envBool("METRICS_ENABLED", true),
	}

	if len(cfg.RelayServers) == 0 && cfg.PublicHost != "" {
		if cfg.RelayPort == 21117 {
			cfg.RelayServers = []string{cfg.PublicHost}
		} else {
			cfg.RelayServers = []string{cfg.PublicHost + ":" + strconv.Itoa(cfg.RelayPort)}
		}
	}

	return cfg
}

func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "y", "yes", "on":
			return true
		case "0", "false", "n", "no", "off":
			return false
		}
	}
	return def
}

func envList(key string) []string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	var out []string
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
