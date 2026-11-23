package rendezvous

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KriszW/godesk-plane/internal/crypto"
	"github.com/KriszW/godesk-plane/internal/pb"
	"github.com/KriszW/godesk-plane/internal/protocol"
	"github.com/KriszW/godesk-plane/internal/relay"
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
	regTimeout   = 30 * time.Second
	maxPeerIDLen = 32
	minPeerIDLen = 6
	maxRelayUUID = 128
	tcpIOTimeout = 30 * time.Second
)

// Config configures the RustDesk hbbs-compatible Rendezvous server.
type Config struct {
	BindHost          string
	Port              int // Default 21116 (UDP + TCP)
	NATPort           int // Default 21115 (TCP)
	WSPort            int // Default 21118 (WebSocket)
	Serial            int32
	SoftwareUpdateURL string
	LocalMask         string
	LocalIP           string
	RendezvousServers []string
	RelayServers      []string
	RequireKey        bool
	AlwaysUseRelay    bool
}

type peerState struct {
	id          string
	uuid        []byte
	pk          []byte
	ip          string
	socketAddr  netip.AddrPort
	lastRegTime time.Time
	regPKCount  int
	regPKLast   time.Time
}

type clientSink struct {
	mu   sync.Mutex
	conn net.Conn
	ws   *websocket.Conn
}

func (s *clientSink) send(msg *pb.RendezvousMessage) error {
	raw, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ws != nil {
		_ = s.ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
		return s.ws.WriteMessage(websocket.BinaryMessage, raw)
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return protocol.WriteFrame(s.conn, raw)
}

// Server implements a full RustDesk hbbs-compatible Rendezvous Server.
type Server struct {
	cfg     Config
	kp      *crypto.KeyPair
	store   *store.Store
	relay   *relay.Server
	metrics *telemetry.Metrics

	udpConn     *net.UDPConn
	tcpListener net.Listener
	natListener net.Listener
	wsListener  net.Listener
	wsServer    *http.Server

	localPrefix *netip.Prefix

	mu                sync.RWMutex
	peers             map[string]*peerState
	tcpSinks          map[netip.AddrPort]*clientSink
	relayServers      []string
	rendezvousServers []string
	requireKey        bool
	alwaysUseRelay    bool

	relayRot atomic.Uint64
}

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// NewServer constructs a RendezvousServer backed by store and optional relay server.
func NewServer(cfg Config, kp *crypto.KeyPair, st *store.Store, relaySrv *relay.Server) *Server {
	s := &Server{
		cfg:               cfg,
		kp:                kp,
		store:             st,
		relay:             relaySrv,
		peers:             make(map[string]*peerState),
		tcpSinks:          make(map[netip.AddrPort]*clientSink),
		relayServers:      append([]string(nil), cfg.RelayServers...),
		rendezvousServers: append([]string(nil), cfg.RendezvousServers...),
		requireKey:        cfg.RequireKey,
		alwaysUseRelay:    cfg.AlwaysUseRelay,
	}
	if cfg.LocalMask != "" {
		if pfx, err := netip.ParsePrefix(cfg.LocalMask); err == nil {
			s.localPrefix = &pfx
		} else {
			slog.Warn("invalid LOCAL_MASK CIDR ignored", "component", "rendezvous", "mask", cfg.LocalMask, "err", err)
		}
	}
	s.loadPeersFromStore()
	return s
}

// SetMetrics attaches OpenTelemetry metrics to the Rendezvous Server.
func (s *Server) SetMetrics(m *telemetry.Metrics) {
	s.metrics = m
}

func (s *Server) tracer() trace.Tracer {
	return otel.Tracer(telemetry.InstrumentationName)
}

func (s *Server) loadPeersFromStore() {
	if s.store == nil {
		return
	}
	peers, err := s.store.ListPeers()
	if err != nil {
		slog.Error("failed to preload peers from database", "component", "rendezvous", "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range peers {
		ps := &peerState{
			id:   p.ID,
			uuid: append([]byte(nil), p.UUID...),
			pk:   append([]byte(nil), p.PK...),
			ip:   p.IP,
		}
		if p.SocketAddr != "" {
			if ap, err := netip.ParseAddrPort(p.SocketAddr); err == nil {
				ps.socketAddr = protocol.NormalizeAddr(ap)
			}
		}
		if p.LastRegAt > 0 {
			ps.lastRegTime = time.UnixMilli(p.LastRegAt)
		}
		s.peers[p.ID] = ps
	}
	if len(peers) > 0 {
		slog.Info("preloaded peers from database", "component", "rendezvous", "count", len(peers))
	}
}

// RequireKey returns whether server key enforcement is active.
func (s *Server) RequireKey() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.requireKey
}

// AlwaysUseRelay returns whether relay-only mode is active.
func (s *Server) AlwaysUseRelay() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.alwaysUseRelay
}

// RelayServers returns the configured relay server list.
func (s *Server) RelayServers() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.relayServers...)
}

// UpdateRuntimeSettings dynamically updates runtime rendezvous settings.
func (s *Server) UpdateRuntimeSettings(requireKey, alwaysUseRelay bool, relayServers []string) {
	s.mu.Lock()
	s.requireKey = requireKey
	s.alwaysUseRelay = alwaysUseRelay
	if relayServers != nil {
		s.relayServers = append([]string(nil), relayServers...)
	}
	s.mu.Unlock()
	slog.Info("updated rendezvous runtime settings",
		"component", "rendezvous",
		"require_key", requireKey,
		"always_use_relay", alwaysUseRelay,
		"relay_servers", relayServers,
	)
}

// RemovePeer removes a peer from memory (e.g. after deletion in UI).
func (s *Server) RemovePeer(id string) {
	s.mu.Lock()
	delete(s.peers, id)
	s.mu.Unlock()
	slog.Info("removed peer from rendezvous memory", "component", "rendezvous", "peer_id", id)
}

// IsOnline returns whether a peer has registered via UDP within regTimeout.
func (s *Server) IsOnline(id string) bool {
	s.mu.RLock()
	p, ok := s.peers[id]
	var online bool
	if ok && !p.lastRegTime.IsZero() {
		online = time.Since(p.lastRegTime) < regTimeout
	}
	s.mu.RUnlock()
	return online
}

// OnlinePeerIDs returns a set of currently UDP-online peer IDs.
func (s *Server) OnlinePeerIDs() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]bool)
	now := time.Now()
	for id, p := range s.peers {
		if !p.lastRegTime.IsZero() && now.Sub(p.lastRegTime) < regTimeout {
			out[id] = true
		}
	}
	return out
}

// OnlinePeerCount returns the count of currently UDP-online peers.
func (s *Server) OnlinePeerCount() int {
	return len(s.OnlinePeerIDs())
}

// RegisteredPeerCount returns total peers tracked in memory.
func (s *Server) RegisteredPeerCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.peers)
}

// Start binds UDP 21116, TCP 21116, TCP 21115 (NAT/Online), and optional WS 21118.
func (s *Server) Start(ctx context.Context) error {
	// 1. UDP 21116
	udpAddrStr := fmt.Sprintf("%s:%d", s.cfg.BindHost, s.cfg.Port)
	udpAddr, err := net.ResolveUDPAddr("udp", udpAddrStr)
	if err != nil {
		return fmt.Errorf("resolve udp %s: %w", udpAddrStr, err)
	}
	uconn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("listen udp %s: %w", udpAddrStr, err)
	}
	_ = uconn.SetReadBuffer(4 * 1024 * 1024)
	_ = uconn.SetWriteBuffer(4 * 1024 * 1024)
	s.udpConn = uconn
	slog.Info("rendezvous udp listener started", "component", "rendezvous", "addr", uconn.LocalAddr().String())
	go s.udpLoop(ctx, uconn)

	// 2. TCP 21116 (signaling)
	tcpAddrStr := fmt.Sprintf("%s:%d", s.cfg.BindHost, s.cfg.Port)
	tln, err := net.Listen("tcp", tcpAddrStr)
	if err != nil {
		_ = uconn.Close()
		return fmt.Errorf("listen tcp %s: %w", tcpAddrStr, err)
	}
	s.tcpListener = tln
	slog.Info("rendezvous tcp listener started", "component", "rendezvous", "addr", tln.Addr().String())
	go s.acceptTCPLoop(ctx, tln)

	// 3. TCP 21115 (NAT test & OnlineRequest)
	if s.cfg.NATPort >= 0 {
		natAddrStr := fmt.Sprintf("%s:%d", s.cfg.BindHost, s.cfg.NATPort)
		nln, err := net.Listen("tcp", natAddrStr)
		if err != nil {
			_ = uconn.Close()
			_ = tln.Close()
			return fmt.Errorf("listen nat tcp %s: %w", natAddrStr, err)
		}
		s.natListener = nln
		slog.Info("rendezvous nat/online tcp listener started", "component", "rendezvous", "addr", nln.Addr().String())
		go s.acceptNATLoop(ctx, nln)
	}

	// 4. WebSocket 21118 (optional)
	if s.cfg.WSPort > 0 {
		wsAddrStr := fmt.Sprintf("%s:%d", s.cfg.BindHost, s.cfg.WSPort)
		wln, err := net.Listen("tcp", wsAddrStr)
		if err != nil {
			s.Close()
			return fmt.Errorf("listen ws %s: %w", wsAddrStr, err)
		}
		s.wsListener = wln
		mux := http.NewServeMux()
		mux.HandleFunc("/", s.handleWS)
		s.wsServer = &http.Server{Handler: mux}
		slog.Info("rendezvous websocket listener started", "component", "rendezvous", "addr", wln.Addr().String())
		go func() {
			if err := s.wsServer.Serve(wln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("rendezvous websocket server error", "component", "rendezvous", "err", err)
			}
		}()
	}

	go func() {
		<-ctx.Done()
		s.Close()
	}()
	return nil
}

// UDPAddr returns the bound UDP listener address.
func (s *Server) UDPAddr() net.Addr {
	if s.udpConn == nil {
		return nil
	}
	return s.udpConn.LocalAddr()
}

// TCPAddr returns the bound signaling TCP listener address.
func (s *Server) TCPAddr() net.Addr {
	if s.tcpListener == nil {
		return nil
	}
	return s.tcpListener.Addr()
}

// NATAddr returns the bound NAT/Online TCP listener address.
func (s *Server) NATAddr() net.Addr {
	if s.natListener == nil {
		return nil
	}
	return s.natListener.Addr()
}

// Close closes all listeners.
func (s *Server) Close() {
	if s.udpConn != nil {
		_ = s.udpConn.Close()
	}
	if s.tcpListener != nil {
		_ = s.tcpListener.Close()
	}
	if s.natListener != nil {
		_ = s.natListener.Close()
	}
	if s.wsServer != nil {
		_ = s.wsServer.Close()
	}
}

func (s *Server) udpLoop(ctx context.Context, conn *net.UDPConn) {
	buf := make([]byte, 64*1024)
	for {
		n, raddr, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Warn("rendezvous udp read error", "component", "rendezvous", "err", err)
			continue
		}
		if n == 0 {
			continue
		}
		ap := protocol.NormalizeAddr(raddr)
		data := make([]byte, n)
		copy(data, buf[:n])
		s.handleUDP(ctx, data, ap)
	}
}

func (s *Server) sendUDP(msg *pb.RendezvousMessage, to netip.AddrPort) {
	if !to.IsValid() || s.udpConn == nil {
		return
	}
	raw, err := proto.Marshal(msg)
	if err != nil {
		slog.Error("failed to marshal udp message", "component", "rendezvous", "to", to.String(), "err", err)
		return
	}
	if _, err := s.udpConn.WriteToUDPAddrPort(raw, to); err != nil {
		slog.Warn("udp write failed", "component", "rendezvous", "to", to.String(), "err", err)
	}
}

func (s *Server) recordUDP(ctx context.Context, msgType string) {
	if s.metrics != nil {
		s.metrics.RendezvousUDPPackets.Add(ctx, 1, metric.WithAttributes(attribute.String("message_type", msgType)))
	}
}

func (s *Server) recordTCP(ctx context.Context, msgType, transport string) {
	if s.metrics != nil {
		s.metrics.RendezvousTCPMessages.Add(ctx, 1, metric.WithAttributes(
			attribute.String("message_type", msgType),
			attribute.String("transport", transport),
		))
	}
}

func (s *Server) recordReg(ctx context.Context, regType, result string) {
	if s.metrics != nil {
		s.metrics.RendezvousRegistrations.Add(ctx, 1, metric.WithAttributes(
			attribute.String("type", regType),
			attribute.String("result", result),
		))
	}
}

func (s *Server) recordPunch(ctx context.Context, mode, result, connType string) {
	if s.metrics != nil {
		s.metrics.RendezvousPunchHoles.Add(ctx, 1, metric.WithAttributes(
			attribute.String("mode", mode),
			attribute.String("result", result),
			attribute.String("conn_type", connType),
		))
	}
}

func (s *Server) handleUDP(ctx context.Context, data []byte, addr netip.AddrPort) {
	var msg pb.RendezvousMessage
	if err := proto.Unmarshal(data, &msg); err != nil {
		slog.Debug("rendezvous udp malformed packet", "component", "rendezvous", "addr", addr.String(), "len", len(data))
		s.recordUDP(ctx, "invalid")
		return
	}

	switch u := msg.Union.(type) {
	case *pb.RendezvousMessage_RegisterPeer:
		s.recordUDP(ctx, "register_peer")
		rp := u.RegisterPeer
		if rp == nil || rp.Id == "" || len(rp.Id) > maxPeerIDLen {
			return
		}
		s.handleRegisterPeer(ctx, rp, addr)

	case *pb.RendezvousMessage_RegisterPk:
		s.recordUDP(ctx, "register_pk")
		rk := u.RegisterPk
		if rk == nil {
			return
		}
		s.handleRegisterPK(ctx, rk, addr)

	case *pb.RendezvousMessage_SoftwareUpdate:
		s.recordUDP(ctx, "software_update")
		su := u.SoftwareUpdate
		if su != nil && s.cfg.SoftwareUpdateURL != "" && su.Url != s.cfg.SoftwareUpdateURL {
			s.sendUDP(&pb.RendezvousMessage{
				Union: &pb.RendezvousMessage_SoftwareUpdate{
					SoftwareUpdate: &pb.SoftwareUpdate{Url: s.cfg.SoftwareUpdateURL},
				},
			}, addr)
		}

	case *pb.RendezvousMessage_Hc:
		s.recordUDP(ctx, "health_check")
		if u.Hc != nil {
			s.sendUDP(&pb.RendezvousMessage{
				Union: &pb.RendezvousMessage_Hc{Hc: &pb.HealthCheck{Token: u.Hc.Token}},
			}, addr)
		}

	default:
		s.recordUDP(ctx, "other")
	}
}

func (s *Server) handleRegisterPeer(ctx context.Context, rp *pb.RegisterPeer, addr netip.AddrPort) {
	ipStr := addr.Addr().String()
	isLoopback := addr.Addr().IsLoopback()

	s.mu.Lock()
	peer, exists := s.peers[rp.Id]
	requestPK := true
	ipChanged := false
	oldIP := ""

	if exists {
		if peer.socketAddr.IsValid() && peer.socketAddr.Port() != 0 {
			ipChanged = peer.socketAddr.Addr() != addr.Addr() && !isLoopback
			oldIP = peer.socketAddr.Addr().String()
		} else if peer.ip != "" {
			ipChanged = peer.ip != ipStr && !isLoopback
			oldIP = peer.ip
		}
		requestPK = len(peer.pk) == 0 || ipChanged
		if !requestPK {
			peer.socketAddr = addr
			peer.ip = ipStr
			peer.lastRegTime = time.Now()
		}
	}
	s.mu.Unlock()

	if ipChanged && oldIP != "" {
		slog.Info("peer IP changed on RegisterPeer; requesting PK verification",
			"component", "rendezvous", "peer_id", rp.Id, "old_ip", oldIP, "new_addr", addr.String())
	} else {
		slog.Debug("peer keepalive registered",
			"component", "rendezvous", "peer_id", rp.Id, "addr", addr.String(), "request_pk", requestPK)
	}

	if requestPK {
		s.recordReg(ctx, "peer", "request_pk")
	} else {
		s.recordReg(ctx, "peer", "ok")
		if s.store != nil {
			if err := s.store.TouchPeerReg(rp.Id, ipStr, addr.String()); err != nil {
				slog.Error("db touch peer registration failed", "component", "rendezvous", "peer_id", rp.Id, "err", err)
			}
		}
	}

	s.sendUDP(&pb.RendezvousMessage{
		Union: &pb.RendezvousMessage_RegisterPeerResponse{
			RegisterPeerResponse: &pb.RegisterPeerResponse{RequestPk: requestPK},
		},
	}, addr)

	if s.cfg.Serial > rp.Serial && len(s.rendezvousServers) > 0 {
		s.mu.RLock()
		servers := append([]string(nil), s.rendezvousServers...)
		s.mu.RUnlock()
		s.sendUDP(&pb.RendezvousMessage{
			Union: &pb.RendezvousMessage_ConfigureUpdate{
				ConfigureUpdate: &pb.ConfigUpdate{
					Serial:            s.cfg.Serial,
					RendezvousServers: servers,
				},
			},
		}, addr)
	}
}

func (s *Server) sendRegisterPKResponse(addr netip.AddrPort, res pb.RegisterPkResponse_Result) {
	s.sendUDP(&pb.RendezvousMessage{
		Union: &pb.RendezvousMessage_RegisterPkResponse{
			RegisterPkResponse: &pb.RegisterPkResponse{
				Result: res,
			},
		},
	}, addr)
}

func (s *Server) handleRegisterPK(parentCtx context.Context, rk *pb.RegisterPk, addr netip.AddrPort) {
	ctx, span := s.tracer().Start(parentCtx, "rendezvous.register_pk",
		trace.WithAttributes(
			attribute.String("peer.id", rk.Id),
			attribute.String("net.peer.ip", addr.Addr().String()),
		),
	)
	defer span.End()

	if len(rk.Uuid) == 0 || len(rk.Pk) == 0 {
		slog.WarnContext(ctx, "RegisterPk rejected: empty uuid or pk", "component", "rendezvous", "peer_id", rk.Id, "addr", addr.String())
		s.recordReg(ctx, "pk", "invalid_payload")
		span.SetStatus(codes.Error, "empty uuid or pk")
		return
	}
	id := rk.Id
	ipStr := addr.Addr().String()
	if len(id) < minPeerIDLen || len(id) > maxPeerIDLen {
		slog.WarnContext(ctx, "RegisterPk rejected: invalid peer id length", "component", "rendezvous", "peer_id", id, "addr", addr.String())
		s.recordReg(ctx, "pk", "invalid_id")
		span.SetStatus(codes.Error, "invalid peer id")
		s.sendRegisterPKResponse(addr, pb.RegisterPkResponse_UUID_MISMATCH)
		return
	}

	s.mu.Lock()
	peer, exists := s.peers[id]
	if !exists {
		peer = &peerState{id: id}
		s.peers[id] = peer
	}

	isNew := len(peer.uuid) == 0
	var ipChanged bool
	var oldIP string

	if !isNew {
		if !bytes.Equal(peer.uuid, rk.Uuid) {
			s.mu.Unlock()
			slog.WarnContext(ctx, "RegisterPk rejected: UUID mismatch",
				"component", "rendezvous", "peer_id", id, "source_ip", ipStr)
			s.recordReg(ctx, "pk", "uuid_mismatch")
			span.SetStatus(codes.Error, "uuid mismatch")
			if s.store != nil {
				_ = s.store.AddAuditLog(&store.AuditLog{
					Category: "security",
					Action:   "uuid_mismatch",
					Severity: "warn",
					TargetID: id,
					SourceIP: ipStr,
					Summary:  fmt.Sprintf("Peer %s rejected: UUID mismatch from %s", id, ipStr),
				})
			}
			s.sendRegisterPKResponse(addr, pb.RegisterPkResponse_UUID_MISMATCH)
			return
		}
		if peer.ip != "" && peer.ip != ipStr && !bytes.Equal(peer.pk, rk.Pk) {
			s.mu.Unlock()
			slog.WarnContext(ctx, "RegisterPk rejected: IP and PK mismatch",
				"component", "rendezvous", "peer_id", id, "source_ip", ipStr, "prev_ip", peer.ip)
			s.recordReg(ctx, "pk", "pk_mismatch")
			span.SetStatus(codes.Error, "pk mismatch")
			if s.store != nil {
				_ = s.store.AddAuditLog(&store.AuditLog{
					Category: "security",
					Action:   "pk_mismatch",
					Severity: "warn",
					TargetID: id,
					SourceIP: ipStr,
					Summary:  fmt.Sprintf("Peer %s rejected: public key and IP mismatch from %s", id, ipStr),
				})
			}
			s.sendRegisterPKResponse(addr, pb.RegisterPkResponse_UUID_MISMATCH)
			return
		}
		ipChanged = peer.ip != "" && peer.ip != ipStr && !addr.Addr().IsLoopback()
		oldIP = peer.ip
	}

	if time.Since(peer.regPKLast) > 6*time.Second {
		peer.regPKCount = 0
	} else if peer.regPKCount > 2 {
		s.mu.Unlock()
		slog.WarnContext(ctx, "RegisterPk rate limited",
			"component", "rendezvous", "peer_id", id, "source_ip", ipStr)
		s.recordReg(ctx, "pk", "too_frequent")
		span.SetStatus(codes.Error, "rate limited")
		s.sendRegisterPKResponse(addr, pb.RegisterPkResponse_TOO_FREQUENT)
		return
	}
	peer.regPKCount++
	peer.regPKLast = time.Now()

	peer.uuid = append([]byte(nil), rk.Uuid...)
	peer.pk = append([]byte(nil), rk.Pk...)
	peer.ip = ipStr
	peer.socketAddr = addr
	peer.lastRegTime = time.Now()
	s.mu.Unlock()

	if s.store != nil {
		if err := s.store.UpsertPeerPK(id, rk.Uuid, rk.Pk, ipStr, addr.String()); err != nil {
			slog.ErrorContext(ctx, "db upsert peer pk failed", "component", "rendezvous", "peer_id", id, "err", err)
		}
		if isNew {
			_ = s.store.AddAuditLog(&store.AuditLog{
				Category: "peer",
				Action:   "peer_registered",
				Severity: "info",
				ActorID:  id,
				TargetID: id,
				SourceIP: ipStr,
				Summary:  fmt.Sprintf("New RustDesk client %s registered from %s", id, ipStr),
			})
		} else if ipChanged {
			metaBytes, _ := json.Marshal(map[string]string{"old_ip": oldIP, "new_ip": ipStr})
			_ = s.store.AddAuditLog(&store.AuditLog{
				Category: "peer",
				Action:   "ip_changed",
				Severity: "info",
				ActorID:  id,
				TargetID: id,
				SourceIP: ipStr,
				Summary:  fmt.Sprintf("Peer %s IP changed from %s to %s", id, oldIP, ipStr),
				Metadata: metaBytes,
			})
		}
	}

	s.recordReg(ctx, "pk", "ok")
	span.SetAttributes(attribute.Bool("peer.is_new", isNew), attribute.Bool("peer.ip_changed", ipChanged))
	slog.InfoContext(ctx, "registered peer public key",
		"component", "rendezvous", "peer_id", id, "addr", addr.String(), "is_new", isNew, "ip_changed", ipChanged)
	s.sendRegisterPKResponse(addr, pb.RegisterPkResponse_OK)
}

func (s *Server) acceptNATLoop(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Error("rendezvous nat accept failed", "component", "rendezvous", "err", err)
			continue
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}
		go s.handleNATConn(conn)
	}
}

func (s *Server) handleNATConn(conn net.Conn) {
	defer conn.Close()
	ap := protocol.NetAddrToAddrPort(conn.RemoteAddr())
	_ = conn.SetReadDeadline(time.Now().Add(tcpIOTimeout))
	data, err := protocol.ReadFrame(conn, protocol.DefaultMaxPacketLength)
	if err != nil {
		slog.Debug("nat/online read frame error", "component", "rendezvous", "addr", ap.String(), "err", err)
		return
	}
	var msg pb.RendezvousMessage
	if err := proto.Unmarshal(data, &msg); err != nil {
		slog.Warn("nat/online malformed protobuf", "component", "rendezvous", "addr", ap.String(), "err", err)
		return
	}

	sink := &clientSink{conn: conn}
	switch u := msg.Union.(type) {
	case *pb.RendezvousMessage_TestNatRequest:
		ctx, span := s.tracer().Start(context.Background(), "rendezvous.test_nat",
			trace.WithAttributes(attribute.String("net.peer.ip", ap.Addr().String())),
		)
		s.recordTCP(ctx, "test_nat_request", "nat_tcp")
		slog.DebugContext(ctx, "handled TestNatRequest", "component", "rendezvous", "addr", ap.String(), "port", ap.Port())
		s.sendTestNatResponse(sink, ap, u.TestNatRequest)
		span.End()
	case *pb.RendezvousMessage_OnlineRequest:
		if u.OnlineRequest != nil {
			ctx, span := s.tracer().Start(context.Background(), "rendezvous.online_request",
				trace.WithAttributes(attribute.Int("peers.queried", len(u.OnlineRequest.Peers))),
			)
			s.recordTCP(ctx, "online_request", "nat_tcp")
			slog.DebugContext(ctx, "handled OnlineRequest", "component", "rendezvous", "addr", ap.String(), "peer_count", len(u.OnlineRequest.Peers))
			s.sendOnlineResponse(sink, u.OnlineRequest.Peers)
			span.End()
		}
	}
}

func (s *Server) sendTestNatResponse(sink *clientSink, addr netip.AddrPort, tar *pb.TestNatRequest) {
	res := &pb.TestNatResponse{
		Port: int32(addr.Port()),
	}
	if tar != nil && s.cfg.Serial > tar.Serial && len(s.rendezvousServers) > 0 {
		s.mu.RLock()
		servers := append([]string(nil), s.rendezvousServers...)
		s.mu.RUnlock()
		res.Cu = &pb.ConfigUpdate{
			Serial:            s.cfg.Serial,
			RendezvousServers: servers,
		}
	}
	_ = sink.send(&pb.RendezvousMessage{
		Union: &pb.RendezvousMessage_TestNatResponse{TestNatResponse: res},
	})
}

func (s *Server) sendOnlineResponse(sink *clientSink, peerIDs []string) {
	states := make([]byte, (len(peerIDs)+7)/8)
	now := time.Now()
	s.mu.RLock()
	for i, pid := range peerIDs {
		if p, ok := s.peers[pid]; ok && !p.lastRegTime.IsZero() && now.Sub(p.lastRegTime) < regTimeout {
			states[i/8] |= 1 << (7 - (i % 8))
		}
	}
	s.mu.RUnlock()

	_ = sink.send(&pb.RendezvousMessage{
		Union: &pb.RendezvousMessage_OnlineResponse{
			OnlineResponse: &pb.OnlineResponse{States: states},
		},
	})
}

func (s *Server) acceptTCPLoop(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Error("rendezvous tcp accept failed", "component", "rendezvous", "err", err)
			continue
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}
		ap := protocol.NetAddrToAddrPort(conn.RemoteAddr())
		go s.handleTCPConn(conn, ap)
	}
}

func (s *Server) handleTCPConn(conn net.Conn, addr netip.AddrPort) {
	defer conn.Close()
	slog.Debug("rendezvous tcp connection opened", "component", "rendezvous", "addr", addr.String())
	sink := &clientSink{conn: conn}
	registeredSink := false
	defer func() {
		if registeredSink {
			s.mu.Lock()
			if cur, ok := s.tcpSinks[addr]; ok && cur == sink {
				delete(s.tcpSinks, addr)
			}
			s.mu.Unlock()
		}
		slog.Debug("rendezvous tcp connection closed", "component", "rendezvous", "addr", addr.String())
	}()

	for {
		_ = conn.SetReadDeadline(time.Now().Add(tcpIOTimeout))
		data, err := protocol.ReadFrame(conn, protocol.DefaultMaxPacketLength)
		if err != nil {
			return
		}
		keepOpen := s.handleTCPMessage(data, sink, addr, false, &registeredSink)
		if !keepOpen {
			return
		}
	}
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("rendezvous websocket upgrade failed", "component", "rendezvous", "remote", r.RemoteAddr, "err", err)
		return
	}
	defer conn.Close()
	conn.SetReadLimit(protocol.DefaultMaxPacketLength)

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

	slog.Debug("rendezvous websocket connection opened", "component", "rendezvous", "addr", ap.String())
	sink := &clientSink{ws: conn}
	registeredSink := false
	defer func() {
		if registeredSink {
			s.mu.Lock()
			if cur, ok := s.tcpSinks[ap]; ok && cur == sink {
				delete(s.tcpSinks, ap)
			}
			s.mu.Unlock()
		}
		slog.Debug("rendezvous websocket connection closed", "component", "rendezvous", "addr", ap.String())
	}()

	for {
		_ = conn.SetReadDeadline(time.Now().Add(tcpIOTimeout))
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.BinaryMessage {
			continue
		}
		keepOpen := s.handleTCPMessage(data, sink, ap, true, &registeredSink)
		if !keepOpen {
			return
		}
	}
}

func (s *Server) registerSink(addr netip.AddrPort, sink *clientSink, registered *bool) {
	if !*registered {
		s.mu.Lock()
		s.tcpSinks[addr] = sink
		s.mu.Unlock()
		*registered = true
	}
}

func (s *Server) sendToTCP(addr netip.AddrPort, msg *pb.RendezvousMessage) {
	addr = protocol.NormalizeAddr(addr)
	s.mu.RLock()
	sink := s.tcpSinks[addr]
	s.mu.RUnlock()
	if sink != nil {
		if err := sink.send(msg); err != nil {
			slog.Warn("failed to send rendezvous tcp response", "component", "rendezvous", "to", addr.String(), "err", err)
		}
	} else {
		slog.Debug("no active tcp sink found for peer response", "component", "rendezvous", "to", addr.String())
	}
}

func (s *Server) handleTCPMessage(data []byte, sink *clientSink, addr netip.AddrPort, isWS bool, registered *bool) bool {
	transport := "tcp"
	if isWS {
		transport = "ws"
	}
	var msg pb.RendezvousMessage
	if err := proto.Unmarshal(data, &msg); err != nil {
		slog.Warn("rendezvous malformed tcp/ws protobuf", "component", "rendezvous", "transport", transport, "addr", addr.String(), "err", err)
		return false
	}

	ctx := context.Background()
	switch u := msg.Union.(type) {
	case *pb.RendezvousMessage_PunchHoleRequest:
		s.recordTCP(ctx, "punch_hole_request", transport)
		if u.PunchHoleRequest == nil {
			return false
		}
		s.registerSink(addr, sink, registered)
		s.handlePunchHoleRequest(addr, u.PunchHoleRequest, sink, isWS)
		return true

	case *pb.RendezvousMessage_RequestRelay:
		s.recordTCP(ctx, "request_relay", transport)
		rf := u.RequestRelay
		if rf == nil || rf.Uuid == "" || len(rf.Uuid) > maxRelayUUID {
			slog.Warn("dropped invalid RequestRelay", "component", "rendezvous", "addr", addr.String())
			return false
		}
		s.registerSink(addr, sink, registered)
		s.handleRequestRelay(addr, rf)
		return true

	case *pb.RendezvousMessage_RelayResponse:
		s.recordTCP(ctx, "relay_response", transport)
		rr := u.RelayResponse
		if rr == nil || rr.Uuid == "" || len(rr.Uuid) > maxRelayUUID {
			slog.Warn("dropped invalid RelayResponse", "component", "rendezvous", "addr", addr.String())
			return false
		}
		s.handleRelayResponse(addr, rr)
		return false

	case *pb.RendezvousMessage_PunchHoleSent:
		s.recordTCP(ctx, "punch_hole_sent", transport)
		if u.PunchHoleSent != nil {
			s.handlePunchHoleSent(addr, u.PunchHoleSent)
		}
		return false

	case *pb.RendezvousMessage_LocalAddr:
		s.recordTCP(ctx, "local_addr", transport)
		if u.LocalAddr != nil {
			s.handleLocalAddr(addr, u.LocalAddr)
		}
		return false

	case *pb.RendezvousMessage_TestNatRequest:
		s.recordTCP(ctx, "test_nat_request", transport)
		s.sendTestNatResponse(sink, addr, u.TestNatRequest)
		return false

	case *pb.RendezvousMessage_OnlineRequest:
		s.recordTCP(ctx, "online_request", transport)
		if u.OnlineRequest != nil {
			s.sendOnlineResponse(sink, u.OnlineRequest.Peers)
		}
		return false

	case *pb.RendezvousMessage_RegisterPk:
		s.recordTCP(ctx, "register_pk_tcp", transport)
		_ = sink.send(&pb.RendezvousMessage{
			Union: &pb.RendezvousMessage_RegisterPkResponse{
				RegisterPkResponse: &pb.RegisterPkResponse{
					Result: pb.RegisterPkResponse_NOT_SUPPORT,
				},
			},
		})
		return false

	case *pb.RendezvousMessage_IceCandidate:
		s.recordTCP(ctx, "ice_candidate", transport)
		if ic := u.IceCandidate; ic != nil && ic.Id != "" {
			s.mu.RLock()
			peer := s.peers[ic.Id]
			var peerAddr netip.AddrPort
			if peer != nil {
				peerAddr = peer.socketAddr
			}
			s.mu.RUnlock()
			if peerAddr.IsValid() {
				ic.SocketAddr = protocol.EncodeAddr(addr)
				s.sendUDP(&pb.RendezvousMessage{
					Union: &pb.RendezvousMessage_IceCandidate{IceCandidate: ic},
				}, peerAddr)
			}
		}
		return true
	}

	return false
}

func (s *Server) handlePunchHoleRequest(addr netip.AddrPort, ph *pb.PunchHoleRequest, sink *clientSink, isWS bool) {
	connType := ph.ConnType.String()
	ctx, span := s.tracer().Start(context.Background(), "rendezvous.punch_hole_request",
		trace.WithAttributes(
			attribute.String("peer.target_id", ph.Id),
			attribute.String("net.peer.ip", addr.Addr().String()),
			attribute.String("rustdesk.conn_type", connType),
			attribute.String("rustdesk.nat_type", ph.NatType.String()),
			attribute.Bool("rustdesk.force_relay", ph.ForceRelay),
			attribute.Bool("rustdesk.ws", isWS),
		),
	)
	defer span.End()

	s.mu.RLock()
	requireKey := s.requireKey
	alwaysRelay := s.alwaysUseRelay
	s.mu.RUnlock()

	if requireKey && s.kp != nil && ph.LicenceKey != s.kp.PublicB64 {
		slog.WarnContext(ctx, "punch hole authentication failed: invalid key",
			"component", "rendezvous", "caller", addr.String(), "target_id", ph.Id, "conn_type", connType)
		s.recordPunch(ctx, "none", "license_mismatch", connType)
		span.SetStatus(codes.Error, "license mismatch")
		if s.store != nil {
			_ = s.store.AddAuditLog(&store.AuditLog{
				Category: "security",
				Action:   "punch_auth_failed",
				Severity: "warn",
				TargetID: ph.Id,
				SourceIP: addr.Addr().String(),
				ConnType: connType,
				Summary:  fmt.Sprintf("Connection to %s rejected from %s: invalid server key", ph.Id, addr.Addr()),
			})
		}
		_ = sink.send(&pb.RendezvousMessage{
			Union: &pb.RendezvousMessage_PunchHoleResponse{
				PunchHoleResponse: &pb.PunchHoleResponse{
					Failure: pb.PunchHoleResponse_LICENSE_MISMATCH,
				},
			},
		})
		return
	}

	s.mu.RLock()
	peer, exists := s.peers[ph.Id]
	var peerAddr netip.AddrPort
	var lastReg time.Time
	if exists {
		peerAddr = peer.socketAddr
		lastReg = peer.lastRegTime
	}
	s.mu.RUnlock()

	if !exists {
		slog.WarnContext(ctx, "punch hole target does not exist",
			"component", "rendezvous", "caller", addr.String(), "target_id", ph.Id)
		s.recordPunch(ctx, "none", "id_not_exist", connType)
		span.SetStatus(codes.Error, "id not exist")
		_ = sink.send(&pb.RendezvousMessage{
			Union: &pb.RendezvousMessage_PunchHoleResponse{
				PunchHoleResponse: &pb.PunchHoleResponse{
					Failure: pb.PunchHoleResponse_ID_NOT_EXIST,
				},
			},
		})
		return
	}

	if lastReg.IsZero() || time.Since(lastReg) >= regTimeout || !peerAddr.IsValid() {
		slog.InfoContext(ctx, "punch hole target offline",
			"component", "rendezvous", "caller", addr.String(), "target_id", ph.Id)
		s.recordPunch(ctx, "none", "offline", connType)
		span.SetAttributes(attribute.Bool("peer.offline", true))
		_ = sink.send(&pb.RendezvousMessage{
			Union: &pb.RendezvousMessage_PunchHoleResponse{
				PunchHoleResponse: &pb.PunchHoleResponse{
					Failure: pb.PunchHoleResponse_OFFLINE,
				},
			},
		})
		return
	}

	if s.store != nil {
		if ph.NatType != pb.NatType_UNKNOWN_NAT {
			_ = s.store.UpdatePeerNAT(ph.Id, ph.NatType.String())
		}
		metaBytes, _ := json.Marshal(map[string]any{
			"caller_addr": addr.String(),
			"target_addr": peerAddr.String(),
			"nat_type":    ph.NatType.String(),
			"force_relay": ph.ForceRelay || alwaysRelay,
			"version":     ph.Version,
		})
		_ = s.store.AddAuditLog(&store.AuditLog{
			Category: "connection",
			Action:   "punch_hole_request",
			Severity: "info",
			TargetID: ph.Id,
			SourceIP: addr.Addr().String(),
			ConnType: connType,
			Summary:  fmt.Sprintf("Connection request from %s to peer %s (%s)", addr.Addr(), ph.Id, connType),
			Metadata: metaBytes,
		})
	}

	peerIsLAN := s.isLAN(peerAddr)
	callerIsLAN := s.isLAN(addr)
	relayServer := s.pickRelayServer()
	natType := ph.NatType
	forceRelay := ph.ForceRelay || alwaysRelay

	if alwaysRelay || (peerIsLAN != callerIsLAN) {
		if peerIsLAN && s.cfg.LocalIP != "" {
			relayServer = s.cfg.LocalIP
		}
		natType = pb.NatType_SYMMETRIC
	}

	sameIntranet := !isWS && !forceRelay && ((peerIsLAN && callerIsLAN) || peerAddr.Addr() == addr.Addr())
	encodedAddr := protocol.EncodeAddr(addr)

	if sameIntranet {
		s.recordPunch(ctx, "local", "forwarded", connType)
		span.SetAttributes(attribute.String("rustdesk.routing_mode", "local"))
		slog.InfoContext(ctx, "forwarding FetchLocalAddr to peer",
			"component", "rendezvous", "target_id", ph.Id, "target_addr", peerAddr.String(), "caller", addr.String())
		s.sendUDP(&pb.RendezvousMessage{
			Union: &pb.RendezvousMessage_FetchLocalAddr{
				FetchLocalAddr: &pb.FetchLocalAddr{
					SocketAddr:   encodedAddr,
					RelayServer:  relayServer,
					SocketAddrV6: ph.SocketAddrV6,
				},
			},
		}, peerAddr)
	} else {
		mode := "direct"
		if forceRelay {
			mode = "relay"
		}
		s.recordPunch(ctx, mode, "forwarded", connType)
		span.SetAttributes(attribute.String("rustdesk.routing_mode", mode), attribute.String("rustdesk.relay_server", relayServer))
		slog.InfoContext(ctx, "forwarding PunchHole to peer",
			"component", "rendezvous", "target_id", ph.Id, "target_addr", peerAddr.String(), "caller", addr.String(),
			"relay_server", relayServer, "force_relay", forceRelay, "conn_type", connType)
		s.sendUDP(&pb.RendezvousMessage{
			Union: &pb.RendezvousMessage_PunchHole{
				PunchHole: &pb.PunchHole{
					SocketAddr:     encodedAddr,
					RelayServer:    relayServer,
					NatType:        natType,
					UdpPort:        ph.UdpPort,
					ForceRelay:     forceRelay,
					UpnpPort:       ph.UpnpPort,
					SocketAddrV6:   ph.SocketAddrV6,
					WebrtcSdpOffer: ph.WebrtcSdpOffer,
				},
			},
		}, peerAddr)
	}
}

func (s *Server) handlePunchHoleSent(addr netip.AddrPort, phs *pb.PunchHoleSent) {
	addrA := protocol.DecodeAddr(phs.SocketAddr)
	signedPK := s.signPeerPK(phs.Version, phs.Id)
	if s.store != nil && phs.Id != "" && phs.NatType != pb.NatType_UNKNOWN_NAT {
		_ = s.store.UpdatePeerNAT(phs.Id, phs.NatType.String())
	}
	slog.Debug("forwarding PunchHoleResponse to caller",
		"component", "rendezvous", "target_id", phs.Id, "caller", addrA.String(), "has_signed_pk", len(signedPK) > 0)

	resp := &pb.PunchHoleResponse{
		SocketAddr:      protocol.EncodeAddr(addr),
		Pk:              signedPK,
		RelayServer:     phs.RelayServer,
		Union:           &pb.PunchHoleResponse_NatType{NatType: phs.NatType},
		UpnpPort:        phs.UpnpPort,
		SocketAddrV6:    phs.SocketAddrV6,
		WebrtcSdpAnswer: phs.WebrtcSdpAnswer,
	}
	s.sendToTCP(addrA, &pb.RendezvousMessage{
		Union: &pb.RendezvousMessage_PunchHoleResponse{PunchHoleResponse: resp},
	})
}

func (s *Server) handleLocalAddr(addr netip.AddrPort, la *pb.LocalAddr) {
	addrA := protocol.DecodeAddr(la.SocketAddr)
	signedPK := s.signPeerPK(la.Version, la.Id)
	slog.Debug("forwarding LocalAddr PunchHoleResponse to caller",
		"component", "rendezvous", "target_id", la.Id, "caller", addrA.String())

	resp := &pb.PunchHoleResponse{
		SocketAddr:   la.LocalAddr,
		Pk:           signedPK,
		RelayServer:  la.RelayServer,
		Union:        &pb.PunchHoleResponse_IsLocal{IsLocal: true},
		SocketAddrV6: la.SocketAddrV6,
	}
	s.sendToTCP(addrA, &pb.RendezvousMessage{
		Union: &pb.RendezvousMessage_PunchHoleResponse{PunchHoleResponse: resp},
	})
}

func (s *Server) handleRequestRelay(addr netip.AddrPort, rf *pb.RequestRelay) {
	ctx, span := s.tracer().Start(context.Background(), "rendezvous.request_relay",
		trace.WithAttributes(
			attribute.String("relay.uuid", rf.Uuid),
			attribute.String("peer.target_id", rf.Id),
			attribute.String("rustdesk.conn_type", rf.ConnType.String()),
		),
	)
	defer span.End()

	s.mu.RLock()
	peer, exists := s.peers[rf.Id]
	var peerAddr netip.AddrPort
	if exists {
		peerAddr = peer.socketAddr
	}
	s.mu.RUnlock()

	if !exists || !peerAddr.IsValid() {
		slog.WarnContext(ctx, "RequestRelay dropped: target peer unknown",
			"component", "rendezvous", "target_id", rf.Id, "uuid", rf.Uuid, "caller", addr.String())
		span.SetStatus(codes.Error, "target peer unknown")
		return
	}

	if s.relay != nil {
		s.relay.RegisterMeta(rf.Uuid, relay.RelayMeta{
			FromIP:   addr.Addr().String(),
			ToID:     rf.Id,
			ToIP:     peerAddr.Addr().String(),
			ConnType: rf.ConnType.String(),
		})
	}

	rf.SocketAddr = protocol.EncodeAddr(addr)
	slog.InfoContext(ctx, "forwarding RequestRelay to target peer",
		"component", "rendezvous", "uuid", rf.Uuid, "target_id", rf.Id, "target_addr", peerAddr.String(), "caller", addr.String())
	s.sendUDP(&pb.RendezvousMessage{
		Union: &pb.RendezvousMessage_RequestRelay{RequestRelay: rf},
	}, peerAddr)
}

func (s *Server) handleRelayResponse(addr netip.AddrPort, rr *pb.RelayResponse) {
	addrA := protocol.DecodeAddr(rr.SocketAddr)
	rr.SocketAddr = nil

	if id := rr.GetId(); id != "" {
		if s.relay != nil {
			s.relay.RegisterMeta(rr.Uuid, relay.RelayMeta{
				FromIP: addrA.Addr().String(),
				ToID:   id,
				ToIP:   addr.Addr().String(),
			})
		}
		signedPK := s.signPeerPK(rr.Version, id)
		rr.Union = &pb.RelayResponse_Pk{Pk: signedPK}
		slog.Debug("forwarding RelayResponse with signed IdPk",
			"component", "rendezvous", "uuid", rr.Uuid, "target_id", id, "caller", addrA.String())
	}

	if rr.RelayServer != "" && s.cfg.LocalIP != "" {
		if s.isLAN(addrA) {
			rr.RelayServer = s.cfg.LocalIP
		} else if rr.RelayServer == s.cfg.LocalIP {
			rr.RelayServer = s.pickRelayServer()
		}
	}

	s.sendToTCP(addrA, &pb.RendezvousMessage{
		Union: &pb.RendezvousMessage_RelayResponse{RelayResponse: rr},
	})
}

func (s *Server) signPeerPK(version, id string) []byte {
	if version == "" || s.kp == nil || id == "" {
		return nil
	}
	s.mu.RLock()
	peer, ok := s.peers[id]
	var pk []byte
	if ok {
		pk = append([]byte(nil), peer.pk...)
	}
	s.mu.RUnlock()
	if !ok || len(pk) == 0 {
		return nil
	}
	signed, err := s.kp.SignIdPk(id, pk)
	if err != nil {
		slog.Error("failed to sign IdPk", "component", "rendezvous", "peer_id", id, "err", err)
		return nil
	}
	return signed
}

func (s *Server) isLAN(addr netip.AddrPort) bool {
	if s.localPrefix == nil || !addr.IsValid() {
		return false
	}
	return s.localPrefix.Contains(addr.Addr())
}

func (s *Server) pickRelayServer() string {
	s.mu.RLock()
	servers := s.relayServers
	s.mu.RUnlock()
	if len(servers) == 0 {
		return ""
	}
	if len(servers) == 1 {
		return servers[0]
	}
	idx := s.relayRot.Add(1) - 1
	return servers[idx%uint64(len(servers))]
}
