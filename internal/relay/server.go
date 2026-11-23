package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KriszW/godesk-plane/internal/pb"
	"github.com/KriszW/godesk-plane/internal/protocol"
	"github.com/KriszW/godesk-plane/internal/store"
	"github.com/KriszW/godesk-plane/internal/telemetry"
	"github.com/gorilla/websocket"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
)

const (
	waiterTimeout    = 30 * time.Second
	relayIdleTimeout = 5 * time.Minute
	maxUUIDLen       = 128
)

// RelayMeta holds optional peer metadata registered by the Rendezvous server
// when forwarding a RequestRelay so relay sessions and audits include peer IDs.
type RelayMeta struct {
	FromIP    string
	ToID      string
	ToIP      string
	ConnType  string
	CreatedAt time.Time
}

// ActiveRelayInfo describes a currently active relayed session for live monitoring.
type ActiveRelayInfo struct {
	UUID      string `json:"uuid"`
	FromIP    string `json:"from_ip"`
	ToIP      string `json:"to_ip"`
	ToID      string `json:"to_id"`
	ConnType  string `json:"conn_type"`
	BytesSent int64  `json:"bytes_sent"`
	BytesRecv int64  `json:"bytes_recv"`
	StartedAt int64  `json:"started_at"`
}

// Config configures the RustDesk-compatible Relay Server (hbbr).
type Config struct {
	BindHost   string
	TCPPort    int
	WSPort     int
	Key        string
	RequireKey func() bool
}

type leg interface {
	isWS() bool
	remoteAddr() netip.AddrPort
	close() error
	rawConn() net.Conn
	readFramed(maxLen int) ([]byte, error)
	writeFramed(data []byte) error
}

type tcpLeg struct {
	conn net.Conn
	addr netip.AddrPort
}

func (l *tcpLeg) isWS() bool                 { return false }
func (l *tcpLeg) remoteAddr() netip.AddrPort { return l.addr }
func (l *tcpLeg) close() error               { return l.conn.Close() }
func (l *tcpLeg) rawConn() net.Conn          { return l.conn }
func (l *tcpLeg) readFramed(maxLen int) ([]byte, error) {
	_ = l.conn.SetReadDeadline(time.Now().Add(relayIdleTimeout))
	return protocol.ReadFrame(l.conn, maxLen)
}
func (l *tcpLeg) writeFramed(data []byte) error {
	_ = l.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return protocol.WriteFrame(l.conn, data)
}

type wsLeg struct {
	conn *websocket.Conn
	addr netip.AddrPort
	wmu  sync.Mutex
}

func (l *wsLeg) isWS() bool                 { return true }
func (l *wsLeg) remoteAddr() netip.AddrPort { return l.addr }
func (l *wsLeg) close() error               { return l.conn.Close() }
func (l *wsLeg) rawConn() net.Conn          { return nil }
func (l *wsLeg) readFramed(maxLen int) ([]byte, error) {
	if maxLen > 0 {
		l.conn.SetReadLimit(int64(maxLen))
	} else {
		l.conn.SetReadLimit(protocol.MaxFrameLength)
	}
	_ = l.conn.SetReadDeadline(time.Now().Add(relayIdleTimeout))
	for {
		mt, data, err := l.conn.ReadMessage()
		if err != nil {
			return nil, err
		}
		if mt == websocket.BinaryMessage {
			return data, nil
		}
	}
}
func (l *wsLeg) writeFramed(data []byte) error {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	_ = l.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return l.conn.WriteMessage(websocket.BinaryMessage, data)
}

type waitingLeg struct {
	gen  uint64
	leg  leg
	done chan struct{}
}

type activeTracker struct {
	uuid      string
	fromIP    string
	toIP      string
	toID      string
	connType  string
	startedAt int64
	bytesSent atomic.Int64
	bytesRecv atomic.Int64
}

// Server implements a full RustDesk hbbr-compatible Relay Server (TCP + WebSocket).
type Server struct {
	cfg     Config
	store   *store.Store
	metrics *telemetry.Metrics

	tcpListener net.Listener
	wsListener  net.Listener
	wsServer    *http.Server

	mu         sync.Mutex
	waiters    map[string]*waitingLeg
	nextGen    uint64
	metaCache  map[string]RelayMeta
	active     map[string]*activeTracker
	totalBytes atomic.Int64
}

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// NewServer creates a new Relay Server instance.
func NewServer(cfg Config, st *store.Store) *Server {
	return &Server{
		cfg:       cfg,
		store:     st,
		waiters:   make(map[string]*waitingLeg),
		metaCache: make(map[string]RelayMeta),
		active:    make(map[string]*activeTracker),
	}
}

// SetMetrics attaches OpenTelemetry metrics to the Relay Server.
func (s *Server) SetMetrics(m *telemetry.Metrics) {
	s.metrics = m
}

func (s *Server) tracer() trace.Tracer {
	return otel.Tracer(telemetry.InstrumentationName)
}

// RegisterMeta records rendezvous context for an upcoming relay UUID.
func (s *Server) RegisterMeta(uuid string, meta RelayMeta) {
	if uuid == "" {
		return
	}
	meta.CreatedAt = time.Now()
	s.mu.Lock()
	s.metaCache[uuid] = meta
	if len(s.metaCache) > 256 {
		cutoff := time.Now().Add(-2 * time.Minute)
		for k, v := range s.metaCache {
			if v.CreatedAt.Before(cutoff) {
				delete(s.metaCache, k)
			}
		}
	}
	s.mu.Unlock()
}

func (s *Server) consumeMeta(uuid string) (RelayMeta, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.metaCache[uuid]
	if ok {
		delete(s.metaCache, uuid)
	}
	return m, ok
}

// ActiveSessions returns live snapshots of currently bridged relay sessions.
func (s *Server) ActiveSessions() []ActiveRelayInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ActiveRelayInfo, 0, len(s.active))
	for _, a := range s.active {
		out = append(out, ActiveRelayInfo{
			UUID:      a.uuid,
			FromIP:    a.fromIP,
			ToIP:      a.toIP,
			ToID:      a.toID,
			ConnType:  a.connType,
			BytesSent: a.bytesSent.Load(),
			BytesRecv: a.bytesRecv.Load(),
			StartedAt: a.startedAt,
		})
	}
	return out
}

// ActiveCount returns the number of currently active relay sessions.
func (s *Server) ActiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active)
}

// Start binds TCP (21117) and optional WebSocket (21119) relay listeners.
func (s *Server) Start(ctx context.Context) error {
	tcpAddr := fmt.Sprintf("%s:%d", s.cfg.BindHost, s.cfg.TCPPort)
	ln, err := net.Listen("tcp", tcpAddr)
	if err != nil {
		return fmt.Errorf("relay tcp listen %s: %w", tcpAddr, err)
	}
	s.tcpListener = ln
	slog.Info("relay tcp listener started", "component", "relay", "addr", ln.Addr().String())

	go s.acceptTCPLoop(ctx, ln)

	if s.cfg.WSPort > 0 {
		wsAddr := fmt.Sprintf("%s:%d", s.cfg.BindHost, s.cfg.WSPort)
		wln, err := net.Listen("tcp", wsAddr)
		if err != nil {
			_ = ln.Close()
			return fmt.Errorf("relay ws listen %s: %w", wsAddr, err)
		}
		s.wsListener = wln
		mux := http.NewServeMux()
		mux.HandleFunc("/", s.handleWS)
		s.wsServer = &http.Server{Handler: mux}
		slog.Info("relay websocket listener started", "component", "relay", "addr", wln.Addr().String())
		go func() {
			if err := s.wsServer.Serve(wln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("relay websocket server error", "component", "relay", "err", err)
			}
		}()
	}

	go func() {
		<-ctx.Done()
		s.Close()
	}()
	return nil
}

// TCPAddr returns the bound TCP address (useful in tests with port 0).
func (s *Server) TCPAddr() net.Addr {
	if s.tcpListener == nil {
		return nil
	}
	return s.tcpListener.Addr()
}

// WSAddr returns the bound WebSocket address.
func (s *Server) WSAddr() net.Addr {
	if s.wsListener == nil {
		return nil
	}
	return s.wsListener.Addr()
}

// Close shuts down relay listeners and waiting connections.
func (s *Server) Close() {
	if s.tcpListener != nil {
		_ = s.tcpListener.Close()
	}
	if s.wsServer != nil {
		_ = s.wsServer.Close()
	}
	s.mu.Lock()
	for k, w := range s.waiters {
		_ = w.leg.close()
		delete(s.waiters, k)
	}
	s.mu.Unlock()
}

func (s *Server) acceptTCPLoop(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Error("relay tcp accept failed", "component", "relay", "err", err)
			continue
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
			_ = tc.SetKeepAlive(true)
			_ = tc.SetKeepAlivePeriod(30 * time.Second)
		}
		ap := protocol.NetAddrToAddrPort(conn.RemoteAddr())
		go s.handleLeg(&tcpLeg{conn: conn, addr: ap})
	}
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("relay websocket upgrade failed", "component", "relay", "remote", r.RemoteAddr, "err", err)
		return
	}
	ap := protocol.NetAddrToAddrPort(conn.RemoteAddr())
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		if ip, err := netip.ParseAddr(realIP); err == nil {
			ap = netip.AddrPortFrom(ip.Unmap(), ap.Port())
		}
	} else if fwd := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); fwd != "" {
		first := strings.TrimSpace(strings.Split(fwd, ",")[0])
		if ip, err := netip.ParseAddr(first); err == nil {
			ap = netip.AddrPortFrom(ip.Unmap(), ap.Port())
		}
	}
	s.handleLeg(&wsLeg{conn: conn, addr: ap})
}

func (s *Server) requireKey() bool {
	if s.cfg.RequireKey != nil {
		return s.cfg.RequireKey()
	}
	return s.cfg.Key != ""
}

func (s *Server) handleLeg(l leg) {
	transport := "tcp"
	if l.isWS() {
		transport = "ws"
	}
	ctx, span := s.tracer().Start(context.Background(), "relay.handshake",
		trace.WithAttributes(
			attribute.String("relay.transport", transport),
			attribute.String("net.peer.ip", l.remoteAddr().Addr().String()),
		),
	)

	if rc := l.rawConn(); rc != nil {
		_ = rc.SetReadDeadline(time.Now().Add(waiterTimeout))
	}
	payload, err := l.readFramed(protocol.MaxRelayRequestLength)
	if err != nil {
		slog.DebugContext(ctx, "relay handshake frame read failed", "component", "relay", "transport", transport, "addr", l.remoteAddr().String(), "err", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "read failed")
		span.End()
		_ = l.close()
		return
	}
	if rc := l.rawConn(); rc != nil {
		_ = rc.SetReadDeadline(time.Time{})
	}

	var msg pb.RendezvousMessage
	if err := proto.Unmarshal(payload, &msg); err != nil {
		slog.WarnContext(ctx, "relay malformed protobuf frame", "component", "relay", "addr", l.remoteAddr().String(), "err", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "unmarshal failed")
		span.End()
		_ = l.close()
		return
	}

	rf := msg.GetRequestRelay()
	if rf == nil || rf.Uuid == "" || len(rf.Uuid) > maxUUIDLen {
		slog.WarnContext(ctx, "relay rejected invalid RequestRelay", "component", "relay", "addr", l.remoteAddr().String())
		span.SetStatus(codes.Error, "invalid request_relay")
		span.End()
		_ = l.close()
		return
	}
	span.SetAttributes(attribute.String("relay.uuid", rf.Uuid))

	if s.requireKey() && s.cfg.Key != "" && rf.LicenceKey != s.cfg.Key {
		slog.WarnContext(ctx, "relay authentication failed: invalid key",
			"component", "relay", "addr", l.remoteAddr().String(), "uuid", rf.Uuid)
		if s.metrics != nil {
			s.metrics.RelayAuthFailures.Add(ctx, 1)
		}
		if s.store != nil {
			_ = s.store.AddAuditLog(&store.AuditLog{
				Category: "security",
				Action:   "relay_auth_failed",
				Severity: "warn",
				SourceIP: l.remoteAddr().Addr().String(),
				Summary:  fmt.Sprintf("Relay authentication failed from %s (invalid licence key)", l.remoteAddr().Addr()),
			})
		}
		span.SetStatus(codes.Error, "invalid licence key")
		span.End()
		_ = l.close()
		return
	}

	uuid := rf.Uuid

	s.mu.Lock()
	if peerWaiter, ok := s.waiters[uuid]; ok {
		delete(s.waiters, uuid)
		s.mu.Unlock()
		if s.metrics != nil {
			s.metrics.RelayWaitingSessions.Add(ctx, -1)
		}
		close(peerWaiter.done)
		span.End()
		s.bridge(ctx, uuid, peerWaiter.leg, l)
		return
	}

	s.nextGen++
	gen := s.nextGen
	waiter := &waitingLeg{
		gen:  gen,
		leg:  l,
		done: make(chan struct{}),
	}
	s.waiters[uuid] = waiter
	s.mu.Unlock()
	if s.metrics != nil {
		s.metrics.RelayWaitingSessions.Add(ctx, 1)
	}

	slog.DebugContext(ctx, "relay waiting for peer leg",
		"component", "relay", "uuid", uuid, "addr", l.remoteAddr().String(), "transport", transport)
	span.End()

	timer := time.NewTimer(waiterTimeout)
	defer timer.Stop()

	select {
	case <-waiter.done:
		return
	case <-timer.C:
		s.mu.Lock()
		if cur, ok := s.waiters[uuid]; ok && cur.gen == gen {
			delete(s.waiters, uuid)
			s.mu.Unlock()
			if s.metrics != nil {
				s.metrics.RelayWaitingSessions.Add(context.Background(), -1)
				s.metrics.RelayWaiterTimeouts.Add(context.Background(), 1)
			}
			slog.Warn("relay waiter expired without pair",
				"component", "relay", "uuid", uuid, "addr", l.remoteAddr().String())
			_ = l.close()
			return
		}
		s.mu.Unlock()
	}
}

func (s *Server) bridge(parentCtx context.Context, uuid string, legA, legB leg) {
	defer legA.close()
	defer legB.close()

	meta, _ := s.consumeMeta(uuid)
	fromIP := legA.remoteAddr().Addr().String()
	toIP := legB.remoteAddr().Addr().String()
	if meta.FromIP != "" {
		fromIP = meta.FromIP
	}
	if meta.ToIP != "" {
		toIP = meta.ToIP
	}
	connType := meta.ConnType
	if connType == "" {
		connType = "DEFAULT_CONN"
	}
	isWS := legA.isWS() || legB.isWS()
	transport := "tcp"
	if isWS {
		transport = "ws"
	}

	ctx, span := s.tracer().Start(parentCtx, "relay.bridge",
		trace.WithAttributes(
			attribute.String("relay.uuid", uuid),
			attribute.String("relay.transport", transport),
			attribute.String("relay.from_ip", fromIP),
			attribute.String("relay.to_ip", toIP),
			attribute.String("peer.target_id", meta.ToID),
			attribute.String("rustdesk.conn_type", connType),
		),
	)
	defer span.End()

	startedAt := time.Now()
	tracker := &activeTracker{
		uuid:      uuid,
		fromIP:    fromIP,
		toIP:      toIP,
		toID:      meta.ToID,
		connType:  connType,
		startedAt: startedAt.UnixMilli(),
	}

	s.mu.Lock()
	s.active[uuid] = tracker
	s.mu.Unlock()

	metricAttrs := metric.WithAttributes(
		attribute.String("transport", transport),
		attribute.String("conn_type", connType),
	)
	if s.metrics != nil {
		s.metrics.RelayActiveSessions.Add(ctx, 1)
		s.metrics.RelaySessionsTotal.Add(ctx, 1, metricAttrs)
	}

	var sessID int64
	if s.store != nil {
		var err error
		sessID, err = s.store.StartSession(&store.Session{
			RelayUUID: uuid,
			FromIP:    fromIP,
			ToID:      meta.ToID,
			ToIP:      toIP,
			ConnType:  connType,
			IsRelay:   true,
			Status:    "active",
			StartedAt: startedAt.UnixMilli(),
		})
		if err != nil {
			slog.ErrorContext(ctx, "failed to persist relay session start", "component", "relay", "uuid", uuid, "err", err)
		}
		metaBytes, _ := json.Marshal(map[string]any{
			"relay_uuid": uuid,
			"leg_a":      legA.remoteAddr().String(),
			"leg_b":      legB.remoteAddr().String(),
			"ws":         isWS,
		})
		_ = s.store.AddAuditLog(&store.AuditLog{
			Category:  "relay",
			Action:    "relay_paired",
			Severity:  "info",
			TargetID:  meta.ToID,
			SourceIP:  fromIP,
			SessionID: uuid,
			ConnType:  connType,
			Summary:   fmt.Sprintf("Relay paired %s <-> %s (target %s)", fromIP, toIP, fallbackStr(meta.ToID, "unknown")),
			Metadata:  metaBytes,
		})
	}

	slog.InfoContext(ctx, "relay session paired",
		"component", "relay",
		"uuid", uuid,
		"leg_a", legA.remoteAddr().String(),
		"leg_b", legB.remoteAddr().String(),
		"target_id", meta.ToID,
		"conn_type", connType,
		"transport", transport,
	)

	var wg sync.WaitGroup
	wg.Add(2)

	if !isWS {
		connA := legA.rawConn()
		connB := legB.rawConn()
		_ = connA.SetDeadline(time.Time{})
		_ = connB.SetDeadline(time.Time{})

		go func() {
			defer wg.Done()
			_, _ = copyCounting(connB, connA, &tracker.bytesSent, &s.totalBytes)
			if tc, ok := connB.(*net.TCPConn); ok {
				_ = tc.CloseWrite()
			}
			_ = connB.SetReadDeadline(time.Now().Add(2 * time.Second))
		}()
		go func() {
			defer wg.Done()
			_, _ = copyCounting(connA, connB, &tracker.bytesRecv, &s.totalBytes)
			if tc, ok := connA.(*net.TCPConn); ok {
				_ = tc.CloseWrite()
			}
			_ = connA.SetReadDeadline(time.Now().Add(2 * time.Second))
		}()
	} else {
		go func() {
			defer wg.Done()
			for {
				data, err := legA.readFramed(protocol.MaxFrameLength)
				if err != nil {
					_ = legB.close()
					return
				}
				if len(data) == 0 {
					continue
				}
				if err := legB.writeFramed(data); err != nil {
					_ = legA.close()
					return
				}
				tracker.bytesSent.Add(int64(len(data)))
				s.totalBytes.Add(int64(len(data)))
			}
		}()
		go func() {
			defer wg.Done()
			for {
				data, err := legB.readFramed(protocol.MaxFrameLength)
				if err != nil {
					_ = legA.close()
					return
				}
				if len(data) == 0 {
					continue
				}
				if err := legA.writeFramed(data); err != nil {
					_ = legB.close()
					return
				}
				tracker.bytesRecv.Add(int64(len(data)))
				s.totalBytes.Add(int64(len(data)))
			}
		}()
	}

	wg.Wait()

	s.mu.Lock()
	delete(s.active, uuid)
	s.mu.Unlock()

	sent := tracker.bytesSent.Load()
	recv := tracker.bytesRecv.Load()
	elapsed := time.Since(startedAt)
	dur := int64(elapsed.Seconds())

	span.SetAttributes(
		attribute.Int64("relay.bytes_sent", sent),
		attribute.Int64("relay.bytes_recv", recv),
		attribute.Int64("relay.bytes_total", sent+recv),
		attribute.Float64("relay.duration_s", elapsed.Seconds()),
	)

	if s.metrics != nil {
		s.metrics.RelayActiveSessions.Add(ctx, -1)
		s.metrics.RelayBytesTransferred.Add(ctx, sent, metric.WithAttributes(attribute.String("direction", "a_to_b"), attribute.String("transport", transport)))
		s.metrics.RelayBytesTransferred.Add(ctx, recv, metric.WithAttributes(attribute.String("direction", "b_to_a"), attribute.String("transport", transport)))
		s.metrics.RelaySessionDuration.Record(ctx, elapsed.Seconds(), metricAttrs)
	}

	slog.InfoContext(ctx, "relay session closed",
		"component", "relay",
		"uuid", uuid,
		"target_id", meta.ToID,
		"duration_s", dur,
		"bytes_sent", sent,
		"bytes_recv", recv,
		"bytes_total", sent+recv,
	)

	if s.store != nil {
		if sessID > 0 {
			if err := s.store.FinishSession(sessID, sent, recv); err != nil {
				slog.ErrorContext(ctx, "failed to finish relay session in db", "component", "relay", "session_id", sessID, "err", err)
			}
		}
		metaBytes, _ := json.Marshal(map[string]any{
			"relay_uuid": uuid,
			"bytes_sent": sent,
			"bytes_recv": recv,
			"leg_a":      legA.remoteAddr().String(),
			"leg_b":      legB.remoteAddr().String(),
		})
		_ = s.store.AddAuditLog(&store.AuditLog{
			Category:         "relay",
			Action:           "relay_closed",
			Severity:         "info",
			TargetID:         meta.ToID,
			SourceIP:         fromIP,
			SessionID:        uuid,
			ConnType:         connType,
			DurationSec:      dur,
			BytesTransferred: sent + recv,
			Summary:          fmt.Sprintf("Relay closed %s <-> %s (%ds, %s transferred)", fromIP, toIP, dur, humanBytes(sent+recv)),
			Metadata:         metaBytes,
		})
	}
}

func copyCounting(dst io.Writer, src io.Reader, localCounter, globalCounter *atomic.Int64) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, rErr := src.Read(buf)
		if n > 0 {
			wn, wErr := dst.Write(buf[:n])
			if wn > 0 {
				total += int64(wn)
				localCounter.Add(int64(wn))
				globalCounter.Add(int64(wn))
			}
			if wErr != nil {
				return total, wErr
			}
			if wn != n {
				return total, io.ErrShortWrite
			}
		}
		if rErr != nil {
			if errors.Is(rErr, io.EOF) {
				return total, nil
			}
			return total, rErr
		}
	}
}

func fallbackStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
