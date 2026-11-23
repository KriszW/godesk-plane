package api

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/KriszW/godesk-plane/internal/config"
	"github.com/KriszW/godesk-plane/internal/crypto"
	"github.com/KriszW/godesk-plane/internal/relay"
	"github.com/KriszW/godesk-plane/internal/rendezvous"
	"github.com/KriszW/godesk-plane/internal/store"
	"github.com/KriszW/godesk-plane/internal/telemetry"
	"github.com/KriszW/godesk-plane/internal/webclient"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Server hosts the RustDesk Client HTTP API (/api/*), the GoDesk Plane Management API (/admin/api/*),
// health check (/healthz), Prometheus metrics (/metrics), and the embedded Web Management SPA (/).
type Server struct {
	cfg        *config.Config
	kp         *crypto.KeyPair
	store      *store.Store
	rendezvous *rendezvous.Server
	relay      *relay.Server
	telemetry  *telemetry.Provider
	webFS      fs.FS

	httpServer *http.Server
	listener   net.Listener
	startedAt  time.Time
}

// NewServer constructs the unified HTTP API and Web UI server.
func NewServer(cfg *config.Config, kp *crypto.KeyPair, st *store.Store, rdv *rendezvous.Server, rel *relay.Server, webFS fs.FS) *Server {
	s := &Server{
		cfg:        cfg,
		kp:         kp,
		store:      st,
		rendezvous: rdv,
		relay:      rel,
		webFS:      webFS,
		startedAt:  time.Now(),
	}
	s.httpServer = &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// SetTelemetry attaches OpenTelemetry tracing, metrics, and Prometheus /metrics exposition.
func (s *Server) SetTelemetry(tp *telemetry.Provider) {
	s.telemetry = tp
	s.httpServer.Handler = s.routes()
}

func (s *Server) metrics() *telemetry.Metrics {
	if s.telemetry == nil {
		return nil
	}
	return s.telemetry.Metrics
}

// Start binds the HTTP port (default 21114) and serves requests.
func (s *Server) Start(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", s.cfg.BindHost, s.cfg.HTTPPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("http listen %s: %w", addr, err)
	}
	s.listener = ln
	slog.Info("http api and console listener started",
		"component", "http",
		"addr", ln.Addr().String(),
		"metrics_enabled", s.cfg.MetricsEnabled && s.telemetry != nil && s.telemetry.MetricsHandler != nil,
	)

	go func() {
		if err := s.httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server error", "component", "http", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.httpServer.Shutdown(shutCtx)
	}()
	return nil
}

// Addr returns the bound HTTP listener address.
func (s *Server) Addr() net.Addr {
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// Close shuts down the HTTP server immediately.
func (s *Server) Close() {
	if s.httpServer != nil {
		_ = s.httpServer.Close()
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Healthcheck & Prometheus metrics
	mux.HandleFunc("/healthz", s.handleHealthz)
	if s.cfg.MetricsEnabled && s.telemetry != nil && s.telemetry.MetricsHandler != nil {
		mux.Handle("/metrics", s.telemetry.MetricsHandler)
	}

	// RustDesk open-source client HTTP API (/api/*)
	mux.HandleFunc("/api/sysinfo", s.handleSysinfo)
	mux.HandleFunc("/api/sysinfo_ver", s.handleSysinfoVer)
	mux.HandleFunc("/api/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("/api/audit/conn", s.handleAuditConn)
	mux.HandleFunc("/api/audit/file", s.handleAuditFile)
	mux.HandleFunc("/api/audit/alarm", s.handleAuditAlarm)
	mux.HandleFunc("/api/login-options", s.handleLoginOptions)
	mux.HandleFunc("/api/login", s.handleRustDeskLogin)
	mux.HandleFunc("/api/logout", s.handleRustDeskLogout)
	mux.HandleFunc("/api/currentUser", s.handleCurrentUser)
	mux.HandleFunc("/api/ab", s.handleLegacyAB)
	mux.HandleFunc("/api/ab/settings", s.handleABSettings)
	mux.HandleFunc("/api/ab/personal", s.handleABPersonal)
	mux.HandleFunc("/api/ab/shared/profiles", s.handleABSharedProfiles)
	mux.HandleFunc("/api/ab/peers", s.handleABPeers)
	mux.HandleFunc("/api/ab/peer/add/", s.handleABPeerAdd)
	mux.HandleFunc("/api/ab/peer/update/", s.handleABPeerUpdate)
	mux.HandleFunc("/api/ab/peers/", s.handleABPeersDelete)
	mux.HandleFunc("/api/ab/tags/", s.handleABTags)
	mux.HandleFunc("/api/ab/tag/add/", s.handleABTagAdd)
	mux.HandleFunc("/api/ab/tag/rename/", s.handleABTagRename)
	mux.HandleFunc("/api/ab/tag/update/", s.handleABTagUpdate)
	mux.HandleFunc("/api/peers", s.handleRustDeskPeers)
	mux.HandleFunc("/api/users", s.handleRustDeskUsers)
	mux.HandleFunc("/api/device-group/accessible", s.handleDeviceGroups)

	// GoDesk Plane Management Console API (/admin/api/*)
	mux.HandleFunc("/admin/api/login", s.handleAdminLogin)
	mux.HandleFunc("/admin/api/logout", s.requireAuth(s.handleAdminLogout))
	mux.HandleFunc("/admin/api/me", s.requireAuth(s.handleAdminMe))
	mux.HandleFunc("/admin/api/overview", s.requireAuth(s.handleOverview))
	mux.HandleFunc("/admin/api/peers", s.requireAuth(s.handleAdminPeers))
	mux.HandleFunc("/admin/api/peers/", s.requireAuth(s.handleAdminPeerByID))
	mux.HandleFunc("/admin/api/tags", s.requireAuth(s.handleAdminTags))
	mux.HandleFunc("/admin/api/tags/", s.requireAuth(s.handleAdminTagByName))
	mux.HandleFunc("/admin/api/analytics", s.requireAuth(s.handleAdminAnalytics))
	mux.HandleFunc("/admin/api/sessions", s.requireAuth(s.handleAdminSessions))
	mux.HandleFunc("/admin/api/audit", s.requireAuth(s.handleAdminAudit))
	mux.HandleFunc("/admin/api/audit/export", s.requireAuth(s.handleAdminAuditExport))
	mux.HandleFunc("/admin/api/users", s.requireAdmin(s.handleAdminUsers))
	mux.HandleFunc("/admin/api/users/", s.requireAdmin(s.handleAdminUserByID))
	mux.HandleFunc("/admin/api/settings", s.requireAuth(s.handleAdminSettings))
	mux.HandleFunc("/admin/api/webclient/ws", s.requireAuth(s.handleWebClientWS))
	mux.HandleFunc("/admin/api/webclient/sse", s.requireAuth(s.handleWebClientSSE))
	mux.HandleFunc("/admin/api/webclient/open", s.requireAuth(s.handleWebClientOpen))
	mux.HandleFunc("/admin/api/webclient/poll", s.requireAuth(s.handleWebClientPoll))
	mux.HandleFunc("/admin/api/webclient/send", s.requireAuth(s.handleWebClientSend))
	mux.HandleFunc("/admin/api/webclient/close", s.requireAuth(s.handleWebClientClose))

	// Embedded Web Management UI SPA
	mux.Handle("/", s.spaHandler())

	handler := withCORS(s.withRequestTelemetry(mux))
	return otelhttp.NewHandler(handler, "godesk.http",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + normalizeRouteLabel(r.URL.Path)
		}),
	)
}

func (s *Server) webclientConfig(r *http.Request) (webclient.Config, string) {
	u := currentUser(r)
	actor := "web-user"
	if u != nil && u.Username != "" {
		actor = u.Username
	}
	relayAdv := s.resolveHost(r)
	if s.rendezvous != nil {
		if rs := s.rendezvous.RelayServers(); len(rs) > 0 {
			relayAdv = rs[0]
		}
	}
	return webclient.Config{
		RendezvousAddr: fmt.Sprintf("127.0.0.1:%d", s.cfg.RendezvousPort),
		RelayAddr:      fmt.Sprintf("127.0.0.1:%d", s.cfg.RelayPort),
		RelayAdvertise: relayAdv,
		KeyPair:        s.kp,
	}, actor
}

func (s *Server) handleWebClientWS(w http.ResponseWriter, r *http.Request) {
	peerID := strings.ReplaceAll(strings.TrimSpace(r.URL.Query().Get("peer_id")), " ", "")
	if peerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "peer_id required"})
		return
	}
	cfg, actor := s.webclientConfig(r)
	webclient.ServeWS(w, r, cfg, peerID, actor)
}

func (s *Server) handleWebClientSSE(w http.ResponseWriter, r *http.Request) {
	peerID := strings.ReplaceAll(strings.TrimSpace(r.URL.Query().Get("peer_id")), " ", "")
	if peerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "peer_id required"})
		return
	}
	cfg, actor := s.webclientConfig(r)
	webclient.ServeSSE(w, r, cfg, peerID, actor)
}

func (s *Server) handleWebClientOpen(w http.ResponseWriter, r *http.Request) {
	peerID := strings.ReplaceAll(strings.TrimSpace(r.URL.Query().Get("peer_id")), " ", "")
	if peerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "peer_id required"})
		return
	}
	cfg, actor := s.webclientConfig(r)
	webclient.HandleHTTPOpen(w, r, cfg, peerID, actor)
}

func (s *Server) handleWebClientPoll(w http.ResponseWriter, r *http.Request) {
	webclient.HandleHTTPPoll(w, r)
}

func (s *Server) handleWebClientSend(w http.ResponseWriter, r *http.Request) {
	webclient.HandleHTTPSend(w, r)
}

func (s *Server) handleWebClientClose(w http.ResponseWriter, r *http.Request) {
	webclient.HandleHTTPClose(w, r)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	n, err := sr.ResponseWriter.Write(b)
	sr.bytes += n
	return n, err
}

func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (sr *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := sr.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijacker not supported")
	}
	return h.Hijack()
}

func (sr *statusRecorder) Unwrap() http.ResponseWriter {
	return sr.ResponseWriter
}

func normalizeRouteLabel(path string) string {
	switch {
	case strings.HasPrefix(path, "/admin/api/peers/"):
		return "/admin/api/peers/:id"
	case strings.HasPrefix(path, "/admin/api/tags/"):
		return "/admin/api/tags/:name"
	case strings.HasPrefix(path, "/admin/api/users/"):
		return "/admin/api/users/:id"
	case strings.HasPrefix(path, "/api/ab/peer/add/"):
		return "/api/ab/peer/add/:guid"
	case strings.HasPrefix(path, "/api/ab/peer/update/"):
		return "/api/ab/peer/update/:guid"
	case strings.HasPrefix(path, "/api/ab/peers/"):
		return "/api/ab/peers/:guid"
	case strings.HasPrefix(path, "/api/ab/tags/"):
		return "/api/ab/tags/:guid"
	case strings.HasPrefix(path, "/assets/"):
		return "/assets/:file"
	default:
		return path
	}
}

func (s *Server) withRequestTelemetry(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		dur := time.Since(start)
		route := normalizeRouteLabel(r.URL.Path)

		if m := s.metrics(); m != nil {
			attrs := metric.WithAttributes(
				attribute.String("method", r.Method),
				attribute.String("route", route),
				attribute.Int("status", rec.status),
			)
			m.HTTPRequests.Add(r.Context(), 1, attrs)
			m.HTTPDuration.Record(r.Context(), dur.Seconds(), attrs)
		}

		// Choose appropriate log level so high-frequency probes don't flood INFO logs
		noisy := route == "/api/heartbeat" || route == "/healthz" || route == "/metrics" ||
			route == "/admin/api/webclient/poll" || route == "/admin/api/webclient/send" ||
			route == "/assets/:file" || (!strings.HasPrefix(route, "/api/") && !strings.HasPrefix(route, "/admin/api/"))

		attrs := []any{
			"component", "http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", dur.Milliseconds(),
			"bytes", rec.bytes,
			"client_ip", clientIP(r),
		}
		switch {
		case rec.status >= 500:
			slog.ErrorContext(r.Context(), "http request failed", attrs...)
		case rec.status >= 400:
			slog.WarnContext(r.Context(), "http request rejected", attrs...)
		case noisy:
			slog.DebugContext(r.Context(), "http request", attrs...)
		default:
			slog.InfoContext(r.Context(), "http request", attrs...)
		}
	})
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) spaHandler() http.Handler {
	if s.webFS == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"service": "godesk-plane", "status": "ok"})
		})
	}
	fileServer := http.FileServer(http.FS(s.webFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/admin/api/") {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "endpoint not found"})
			return
		}
		clean := strings.TrimPrefix(r.URL.Path, "/")
		if clean == "" || clean == "index.html" {
			data, err := fs.ReadFile(s.webFS, "index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(data)
			return
		}
		if f, err := s.webFS.Open(clean); err == nil {
			_ = f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}
		data, err := fs.ReadFile(s.webFS, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	online := 0
	if s.rendezvous != nil {
		online = s.rendezvous.OnlinePeerCount()
	}
	relays := 0
	if s.relay != nil {
		relays = s.relay.ActiveCount()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"uptime_seconds": int64(time.Since(s.startedAt).Seconds()),
		"online_peers":   online,
		"active_relays":  relays,
	})
}

type ctxKey string

const ctxUserKey ctxKey = "auth_user"

func (s *Server) extractBearer(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	if q := strings.TrimSpace(r.URL.Query().Get("token")); q != "" {
		return q
	}
	return ""
}

func (s *Server) authenticateRequest(r *http.Request) (*store.User, error) {
	tok := s.extractBearer(r)
	if tok == "" {
		return nil, errors.New("missing authorization token")
	}
	return s.store.ValidateToken(tok)
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.authenticateRequest(r)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		ctx := context.WithValue(r.Context(), ctxUserKey, u)
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := currentUser(r)
		if u == nil || !u.IsAdmin {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin privileges required"})
			return
		}
		next(w, r)
	})
}

func currentUser(r *http.Request) *store.User {
	u, _ := r.Context().Value(ctxUserKey).(*store.User)
	return u
}

func clientIP(r *http.Request) string {
	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		return xri
	}
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 2*1024*1024))
	if err != nil {
		return err
	}
	if len(bytesTrimSpace(body)) == 0 {
		return nil
	}
	return json.Unmarshal(body, dst)
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

// buildRustDeskConfigString builds the exact reversed URL-safe base64 JSON string
// accepted by RustDesk client's "Import Server Config" / `rustdesk --config <str>`.
func (s *Server) buildRustDeskConfigString(host string) string {
	if host == "" {
		host = s.cfg.PublicHost
	}
	if host == "" {
		host = "localhost"
	}
	idHost := host
	if s.cfg.RendezvousPort != 21116 {
		idHost = fmt.Sprintf("%s:%d", host, s.cfg.RendezvousPort)
	}
	relayHost := host
	if s.rendezvous != nil {
		if rs := s.rendezvous.RelayServers(); len(rs) > 0 {
			relayHost = rs[0]
		}
	} else if s.cfg.RelayPort != 21117 {
		relayHost = fmt.Sprintf("%s:%d", host, s.cfg.RelayPort)
	}
	apiURL := fmt.Sprintf("http://%s:%d", host, s.cfg.HTTPPort)
	key := ""
	if s.kp != nil {
		key = s.kp.PublicB64
	}
	payload, _ := json.Marshal(map[string]string{
		"host":  idHost,
		"relay": relayHost,
		"api":   apiURL,
		"key":   key,
	})
	b64 := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(payload)
	runes := []rune(b64)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}
