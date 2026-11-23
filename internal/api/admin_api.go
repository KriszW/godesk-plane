package api

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/KriszW/godesk-plane/internal/relay"
	"github.com/KriszW/godesk-plane/internal/store"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

func (s *Server) enrichedPeers() ([]store.Peer, error) {
	peers, err := s.store.ListPeers()
	if err != nil {
		return nil, err
	}
	var liveOnline map[string]bool
	if s.rendezvous != nil {
		liveOnline = s.rendezvous.OnlinePeerIDs()
	}
	for i := range peers {
		if liveOnline[peers[i].ID] {
			peers[i].Online = true
		}
	}
	// Sort online peers first, preserving last-seen order within each group
	online := make([]store.Peer, 0, len(peers))
	offline := make([]store.Peer, 0, len(peers))
	for _, p := range peers {
		if p.Online {
			online = append(online, p)
		} else {
			offline = append(offline, p)
		}
	}
	return append(online, offline...), nil
}

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	u, err := s.store.AuthenticateUser(req.Username, req.Password)
	if err != nil {
		if m := s.metrics(); m != nil {
			m.AuthFailures.Add(r.Context(), 1, metric.WithAttributes(attribute.String("surface", "admin")))
		}
		slog.WarnContext(r.Context(), "console login failed",
			"component", "admin", "username", req.Username, "client_ip", clientIP(r))
		_ = s.store.AddAuditLog(&store.AuditLog{
			Category:  "security",
			Action:    "admin_login_failed",
			Severity:  "warn",
			ActorName: req.Username,
			SourceIP:  clientIP(r),
			Summary:   fmt.Sprintf("Failed console login for %q from %s", req.Username, clientIP(r)),
		})
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Invalid username or password"})
		return
	}
	tok, err := s.store.CreateToken(u, "", "", 7*24*time.Hour)
	if err != nil {
		slog.ErrorContext(r.Context(), "failed to create console session token", "component", "admin", "username", u.Username, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to issue session token"})
		return
	}
	slog.InfoContext(r.Context(), "console user signed in",
		"component", "admin", "username", u.Username, "is_admin", u.IsAdmin, "client_ip", clientIP(r))
	_ = s.store.AddAuditLog(&store.AuditLog{
		Category:  "admin",
		Action:    "admin_login",
		Severity:  "info",
		ActorName: u.Username,
		SourceIP:  clientIP(r),
		Summary:   fmt.Sprintf("User %s signed into GoDesk Plane console from %s", u.Username, clientIP(r)),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"token": tok,
		"user":  u,
	})
}

func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if tok := s.extractBearer(r); tok != "" {
		_ = s.store.RevokeToken(tok)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleAdminMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"user": currentUser(r)})
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	peers, err := s.enrichedPeers()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	onlineCount := 0
	for _, p := range peers {
		if p.Online {
			onlineCount++
		}
	}
	analytics, _ := s.store.GetAnalytics(r.Context(), "24h")
	recentAudits, _, _ := s.store.QueryAuditLogs(store.AuditFilter{Page: 1, PageSize: 12})
	if recentAudits == nil {
		recentAudits = []store.AuditLog{}
	}
	activeRelays := []relay.ActiveRelayInfo{}
	if s.relay != nil {
		activeRelays = s.relay.ActiveSessions()
	}

	pubKey := ""
	if s.kp != nil {
		pubKey = s.kp.PublicB64
	}
	host := s.resolveHost(r)

	writeJSON(w, http.StatusOK, map[string]any{
		"total_peers":    len(peers),
		"online_peers":   onlineCount,
		"offline_peers":  len(peers) - onlineCount,
		"active_relays":  activeRelays,
		"analytics_24h":  analytics,
		"recent_audits":  recentAudits,
		"uptime_seconds": int64(time.Since(s.startedAt).Seconds()),
		"server": map[string]any{
			"public_host":     host,
			"public_key":      pubKey,
			"rendezvous_port": s.cfg.RendezvousPort,
			"nat_port":        s.cfg.NATPort,
			"relay_port":      s.cfg.RelayPort,
			"http_port":       s.cfg.HTTPPort,
			"require_key":     s.rendezvous != nil && s.rendezvous.RequireKey(),
			"always_relay":    s.rendezvous != nil && s.rendezvous.AlwaysUseRelay(),
			"config_string":   s.buildRustDeskConfigString(host),
		},
	})
}

func (s *Server) resolveHost(r *http.Request) string {
	if s.cfg.PublicHost != "" {
		return s.cfg.PublicHost
	}
	if v, ok := s.store.GetSetting("public_host"); ok && v != "" {
		return v
	}
	h := r.Host
	if idx := strings.LastIndex(h, ":"); idx > 0 && !strings.Contains(h[idx:], "]") {
		h = h[:idx]
	}
	if h == "" {
		return "localhost"
	}
	return h
}

func (s *Server) handleAdminPeers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		peers, err := s.enrichedPeers()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("search")))
		status := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
		tagFilter := strings.TrimSpace(r.URL.Query().Get("tag"))
		osFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("os")))
		groupFilter := strings.TrimSpace(r.URL.Query().Get("group"))

		filtered := make([]store.Peer, 0, len(peers))
		for _, p := range peers {
			if status == "online" && !p.Online {
				continue
			}
			if status == "offline" && p.Online {
				continue
			}
			if groupFilter != "" && groupFilter != "all" && p.GroupName != groupFilter {
				continue
			}
			if osFilter != "" && osFilter != "all" && !strings.Contains(strings.ToLower(p.OS), osFilter) {
				continue
			}
			if tagFilter != "" && tagFilter != "all" {
				found := false
				for _, t := range p.Tags {
					if t == tagFilter {
						found = true
						break
					}
				}
				if !found {
					continue
				}
			}
			if q != "" {
				hay := strings.ToLower(strings.Join([]string{p.ID, p.Alias, p.Hostname, p.Username, p.IP, p.OS, p.Note, p.GroupName, strings.Join(p.Tags, " ")}, " "))
				if !strings.Contains(hay, q) {
					continue
				}
			}
			filtered = append(filtered, p)
		}
		tags, _ := s.store.ListTags()
		if tags == nil {
			tags = []store.Tag{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"peers": filtered,
			"total": len(filtered),
			"tags":  tags,
		})

	case http.MethodPost:
		var req struct {
			ID        string   `json:"id"`
			Alias     string   `json:"alias"`
			Hostname  string   `json:"hostname"`
			Username  string   `json:"username"`
			OS        string   `json:"os"`
			Note      string   `json:"note"`
			GroupName string   `json:"group_name"`
			Tags      []string `json:"tags"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
			return
		}
		req.ID = strings.ReplaceAll(strings.TrimSpace(req.ID), " ", "")
		if len(req.ID) < 6 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "valid RustDesk Peer ID required (at least 6 characters)"})
			return
		}
		if err := s.store.UpsertPeerManual(req.ID, req.Alias, req.Hostname, req.Username, req.OS, req.Note, req.GroupName, req.Tags); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		for _, t := range req.Tags {
			_ = s.store.UpsertTag(t, 0)
		}
		u := currentUser(r)
		_ = s.store.AddAuditLog(&store.AuditLog{
			Category:  "admin",
			Action:    "peer_added",
			Severity:  "info",
			ActorName: u.Username,
			TargetID:  req.ID,
			SourceIP:  clientIP(r),
			Summary:   fmt.Sprintf("User %s added peer %s (%s) to address book", u.Username, req.ID, fallback(req.Alias, req.Hostname)),
		})
		p, _ := s.store.GetPeer(req.ID)
		writeJSON(w, http.StatusCreated, p)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleAdminPeerByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/admin/api/peers/")
	id = strings.TrimSpace(id)
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing peer id"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		p, err := s.store.GetPeer(id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "peer not found"})
			return
		}
		if s.rendezvous != nil && s.rendezvous.IsOnline(id) {
			p.Online = true
		}
		audits, _, _ := s.store.QueryAuditLogs(store.AuditFilter{PeerID: id, Page: 1, PageSize: 25})
		if audits == nil {
			audits = []store.AuditLog{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"peer":   p,
			"audits": audits,
		})

	case http.MethodPut:
		var req struct {
			Alias     string   `json:"alias"`
			Note      string   `json:"note"`
			GroupName string   `json:"group_name"`
			Tags      []string `json:"tags"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
			return
		}
		if err := s.store.UpdatePeerMeta(id, req.Alias, req.Note, req.GroupName, req.Tags); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "peer not found"})
			return
		}
		for _, t := range req.Tags {
			_ = s.store.UpsertTag(t, 0)
		}
		u := currentUser(r)
		_ = s.store.AddAuditLog(&store.AuditLog{
			Category:  "admin",
			Action:    "peer_updated",
			Severity:  "info",
			ActorName: u.Username,
			TargetID:  id,
			SourceIP:  clientIP(r),
			Summary:   fmt.Sprintf("User %s updated address book metadata for peer %s", u.Username, id),
		})
		p, _ := s.store.GetPeer(id)
		if p != nil && s.rendezvous != nil && s.rendezvous.IsOnline(id) {
			p.Online = true
		}
		writeJSON(w, http.StatusOK, p)

	case http.MethodDelete:
		if err := s.store.DeletePeer(id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if s.rendezvous != nil {
			s.rendezvous.RemovePeer(id)
		}
		u := currentUser(r)
		_ = s.store.AddAuditLog(&store.AuditLog{
			Category:  "admin",
			Action:    "peer_deleted",
			Severity:  "warn",
			ActorName: u.Username,
			TargetID:  id,
			SourceIP:  clientIP(r),
			Summary:   fmt.Sprintf("User %s deleted peer %s", u.Username, id),
		})
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleAdminTags(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		tags, err := s.store.ListTags()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if tags == nil {
			tags = []store.Tag{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"tags": tags})

	case http.MethodPost:
		var req struct {
			Name  string `json:"name"`
			Color uint32 `json:"color"`
		}
		if err := decodeJSON(r, &req); err != nil || strings.TrimSpace(req.Name) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tag name required"})
			return
		}
		if err := s.store.UpsertTag(req.Name, req.Color); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"name": req.Name, "color": req.Color})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleAdminTagByName(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/admin/api/tags/"))
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tag name required"})
		return
	}
	if r.Method == http.MethodDelete {
		_ = s.store.DeleteTag(name)
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func (s *Server) handleAdminAnalytics(w http.ResponseWriter, r *http.Request) {
	rangeKey := r.URL.Query().Get("range")
	data, err := s.store.GetAnalytics(r.Context(), rangeKey)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// Overlay live UDP online peer count
	if peers, pErr := s.enrichedPeers(); pErr == nil {
		online := 0
		for _, p := range peers {
			if p.Online {
				online++
			}
		}
		data.TotalPeers = len(peers)
		data.OnlinePeers = online
		data.OfflinePeers = len(peers) - online
	}
	activeRelays := []relay.ActiveRelayInfo{}
	if s.relay != nil {
		activeRelays = s.relay.ActiveSessions()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"analytics":     data,
		"active_relays": activeRelays,
	})
}

func (s *Server) handleAdminSessions(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	sessions, err := s.store.ListSessions(limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if sessions == nil {
		sessions = []store.Session{}
	}
	activeRelays := []relay.ActiveRelayInfo{}
	if s.relay != nil {
		activeRelays = s.relay.ActiveSessions()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions":      sessions,
		"active_relays": activeRelays,
	})
}

func parseAuditFilter(r *http.Request) store.AuditFilter {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	pageSize, _ := strconv.Atoi(q.Get("page_size"))
	fromMS, _ := strconv.ParseInt(q.Get("from"), 10, 64)
	toMS, _ := strconv.ParseInt(q.Get("to"), 10, 64)
	return store.AuditFilter{
		Category: q.Get("category"),
		Action:   q.Get("action"),
		Severity: q.Get("severity"),
		Search:   q.Get("search"),
		PeerID:   q.Get("peer_id"),
		FromMS:   fromMS,
		ToMS:     toMS,
		Page:     page,
		PageSize: pageSize,
	}
}

func (s *Server) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	f := parseAuditFilter(r)
	logs, total, err := s.store.QueryAuditLogs(f)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if logs == nil {
		logs = []store.AuditLog{}
	}
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize <= 0 {
		f.PageSize = 50
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"logs":      logs,
		"total":     total,
		"page":      f.Page,
		"page_size": f.PageSize,
	})
}

func (s *Server) handleAdminAuditExport(w http.ResponseWriter, r *http.Request) {
	f := parseAuditFilter(r)
	f.Page = 1
	f.PageSize = 500
	logs, _, err := s.store.QueryAuditLogs(f)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "json" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="godesk-audit-log.json"`)
		_ = json.NewEncoder(w).Encode(logs)
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="godesk-audit-log.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"ID", "Timestamp", "Category", "Action", "Severity", "ActorID", "ActorName", "TargetID", "SourceIP", "ConnType", "DurationSec", "BytesTransferred", "Summary"})
	for _, l := range logs {
		_ = cw.Write([]string{
			strconv.FormatInt(l.ID, 10),
			time.UnixMilli(l.CreatedAt).UTC().Format(time.RFC3339),
			l.Category,
			l.Action,
			l.Severity,
			l.ActorID,
			l.ActorName,
			l.TargetID,
			l.SourceIP,
			l.ConnType,
			strconv.FormatInt(l.DurationSec, 10),
			strconv.FormatInt(l.BytesTransferred, 10),
			l.Summary,
		})
	}
	cw.Flush()
}

func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		users, err := s.store.ListUsers()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"users": users})

	case http.MethodPost:
		var req struct {
			Username string `json:"username"`
			Email    string `json:"email"`
			Password string `json:"password"`
			IsAdmin  bool   `json:"is_admin"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
			return
		}
		u, err := s.store.CreateUser(req.Username, req.Email, req.Password, req.IsAdmin)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		actor := currentUser(r)
		_ = s.store.AddAuditLog(&store.AuditLog{
			Category:   "admin",
			Action:     "user_created",
			Severity:   "info",
			ActorName:  actor.Username,
			TargetName: u.Username,
			SourceIP:   clientIP(r),
			Summary:    fmt.Sprintf("Admin %s created user %s (admin=%v)", actor.Username, u.Username, u.IsAdmin),
		})
		writeJSON(w, http.StatusCreated, u)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleAdminUserByID(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/admin/api/users/"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid user id"})
		return
	}
	actor := currentUser(r)

	switch r.Method {
	case http.MethodPut:
		var req struct {
			Email    string `json:"email"`
			IsAdmin  bool   `json:"is_admin"`
			Status   int    `json:"status"`
			Password string `json:"password"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
			return
		}
		if err := s.store.UpdateUser(id, req.Email, req.IsAdmin, req.Status, req.Password); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		_ = s.store.AddAuditLog(&store.AuditLog{
			Category:  "admin",
			Action:    "user_updated",
			Severity:  "info",
			ActorName: actor.Username,
			TargetID:  idStr,
			SourceIP:  clientIP(r),
			Summary:   fmt.Sprintf("Admin %s updated user #%d", actor.Username, id),
		})
		u, _ := s.store.GetUserByID(id)
		writeJSON(w, http.StatusOK, u)

	case http.MethodDelete:
		if actor.ID == id {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot delete currently signed-in admin"})
			return
		}
		if err := s.store.DeleteUser(id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		_ = s.store.AddAuditLog(&store.AuditLog{
			Category:  "admin",
			Action:    "user_deleted",
			Severity:  "warn",
			ActorName: actor.Username,
			TargetID:  idStr,
			SourceIP:  clientIP(r),
			Summary:   fmt.Sprintf("Admin %s deleted user #%d", actor.Username, id),
		})
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		host := s.resolveHost(r)
		pubKey := ""
		if s.kp != nil {
			pubKey = s.kp.PublicB64
		}
		relayServers := []string{}
		requireKey := s.cfg.RequireKey
		alwaysRelay := s.cfg.AlwaysUseRelay
		if s.rendezvous != nil {
			relayServers = s.rendezvous.RelayServers()
			requireKey = s.rendezvous.RequireKey()
			alwaysRelay = s.rendezvous.AlwaysUseRelay()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"public_host":        host,
			"public_key":         pubKey,
			"require_key":        requireKey,
			"always_use_relay":   alwaysRelay,
			"relay_servers":      relayServers,
			"http_port":          s.cfg.HTTPPort,
			"nat_port":           s.cfg.NATPort,
			"rendezvous_port":    s.cfg.RendezvousPort,
			"relay_port":         s.cfg.RelayPort,
			"rendezvous_ws_port": s.cfg.RendezvousWS,
			"relay_ws_port":      s.cfg.RelayWS,
			"config_string":      s.buildRustDeskConfigString(host),
			"observability": map[string]any{
				"log_level":        s.cfg.LogLevel,
				"log_format":       s.cfg.LogFormat,
				"otel_enabled":     s.cfg.OTelEnabled && s.cfg.OTelEndpoint != "",
				"otel_endpoint":    s.cfg.OTelEndpoint,
				"otel_protocol":    s.cfg.OTelProtocol,
				"otel_service":     s.cfg.OTelServiceName,
				"metrics_enabled":  s.cfg.MetricsEnabled,
				"metrics_endpoint": "/metrics",
			},
		})

	case http.MethodPut:
		u := currentUser(r)
		if !u.IsAdmin {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin required"})
			return
		}
		var req struct {
			PublicHost     string   `json:"public_host"`
			RequireKey     bool     `json:"require_key"`
			AlwaysUseRelay bool     `json:"always_use_relay"`
			RelayServers   []string `json:"relay_servers"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
			return
		}
		s.cfg.PublicHost = strings.TrimSpace(req.PublicHost)
		_ = s.store.SetSetting("public_host", s.cfg.PublicHost)
		_ = s.store.SetSetting("require_key", strconv.FormatBool(req.RequireKey))
		_ = s.store.SetSetting("always_use_relay", strconv.FormatBool(req.AlwaysUseRelay))
		rsBytes, _ := json.Marshal(req.RelayServers)
		_ = s.store.SetSetting("relay_servers", string(rsBytes))

		if s.rendezvous != nil {
			s.rendezvous.UpdateRuntimeSettings(req.RequireKey, req.AlwaysUseRelay, req.RelayServers)
		}
		slog.InfoContext(r.Context(), "server settings updated",
			"component", "admin",
			"actor", u.Username,
			"public_host", s.cfg.PublicHost,
			"require_key", req.RequireKey,
			"always_use_relay", req.AlwaysUseRelay,
		)
		_ = s.store.AddAuditLog(&store.AuditLog{
			Category:  "admin",
			Action:    "settings_updated",
			Severity:  "info",
			ActorName: u.Username,
			SourceIP:  clientIP(r),
			Summary:   fmt.Sprintf("Admin %s updated server settings (require_key=%v, always_use_relay=%v)", u.Username, req.RequireKey, req.AlwaysUseRelay),
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"status":        "updated",
			"config_string": s.buildRustDeskConfigString(s.resolveHost(r)),
		})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
