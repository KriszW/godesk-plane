package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/KriszW/godesk-plane/internal/store"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

func (s *Server) recordClientEvent(r *http.Request, event string) {
	if m := s.metrics(); m != nil {
		m.ClientEvents.Add(r.Context(), 1, metric.WithAttributes(attribute.String("event", event)))
	}
}

// handleSysinfo receives POST /api/sysinfo from RustDesk clients and returns "SYSINFO_UPDATED".
func (s *Server) handleSysinfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var raw map[string]any
	if err := decodeJSON(r, &raw); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	id := asString(raw["id"])
	if id == "" {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ID_NOT_FOUND"))
		return
	}
	uuidBytes, _ := base64.StdEncoding.DecodeString(asString(raw["uuid"]))
	hostname := asString(raw["hostname"])
	username := asString(raw["username"])
	osName := asString(raw["os"])
	cpu := asString(raw["cpu"])
	memory := asString(raw["memory"])
	version := asString(raw["version"])
	alias := asString(raw["preset-address-book-alias"])
	note := asString(raw["preset-address-book-note"])
	if note == "" {
		note = asString(raw["preset-note"])
	}
	groupName := asString(raw["preset-device-group-name"])
	var tags []string
	if presetTag := strings.TrimSpace(asString(raw["preset-address-book-tag"])); presetTag != "" {
		for _, t := range strings.Split(presetTag, ",") {
			if tt := strings.TrimSpace(t); tt != "" {
				tags = append(tags, tt)
			}
		}
	}

	ip := clientIP(r)
	if err := s.store.UpsertPeerSysinfo(id, ip, hostname, username, osName, cpu, memory, version, uuidBytes, alias, note, groupName, tags); err != nil {
		slog.ErrorContext(r.Context(), "failed to store peer sysinfo", "component", "api", "peer_id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.recordClientEvent(r, "sysinfo")
	slog.InfoContext(r.Context(), "updated peer sysinfo",
		"component", "api", "peer_id", id, "hostname", hostname, "os", osName, "version", version, "client_ip", ip)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("SYSINFO_UPDATED"))
}

func (s *Server) handleSysinfoVer(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("1"))
}

// handleHeartbeat receives POST /api/heartbeat from RustDesk clients.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID         string `json:"id"`
		UUID       string `json:"uuid"`
		Ver        int64  `json:"ver"`
		ModifiedAt int64  `json:"modified_at"`
		Conns      []any  `json:"conns"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	if req.ID == "" {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	uuidBytes, _ := base64.StdEncoding.DecodeString(req.UUID)
	verStr := ""
	if req.Ver > 0 {
		verStr = formatRustDeskVer(req.Ver)
	}
	needSysinfo, err := s.store.TouchPeerHeartbeat(req.ID, clientIP(r), verStr, uuidBytes)
	if err != nil {
		slog.WarnContext(r.Context(), "heartbeat db update failed", "component", "api", "peer_id", req.ID, "err", err)
	}
	s.recordClientEvent(r, "heartbeat")
	slog.DebugContext(r.Context(), "peer heartbeat received",
		"component", "api", "peer_id", req.ID, "active_conns", len(req.Conns), "need_sysinfo", needSysinfo)
	resp := map[string]any{}
	if needSysinfo {
		resp["sysinfo"] = true
	}
	writeJSON(w, http.StatusOK, resp)
}

func formatRustDeskVer(v int64) string {
	// RustDesk encodes version as major*1_000_000 + minor*1_000 + patch
	if v < 1000 {
		return strconv.FormatInt(v, 10)
	}
	major := v / 1_000_000
	minor := (v / 1_000) % 1000
	patch := v % 1000
	return fmt.Sprintf("%d.%d.%d", major, minor, patch)
}

// handleAuditConn receives POST /api/audit/conn from RustDesk clients.
func (s *Server) handleAuditConn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var raw map[string]any
	if err := decodeJSON(r, &raw); err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	targetID := asString(raw["id"])
	action := asString(raw["action"])
	sourceIP := asString(raw["ip"])
	if sourceIP == "" {
		sourceIP = clientIP(r)
	}
	connID := asInt64(raw["conn_id"])
	sessionID := asString(raw["session_id"])
	nonce := asString(raw["nonce"])
	connType := connTypeLabel(asInt64(raw["type"]))

	var peerID, peerName string
	if peerArr, ok := raw["peer"].([]any); ok && len(peerArr) >= 1 {
		peerID = asString(peerArr[0])
		if len(peerArr) >= 2 {
			peerName = asString(peerArr[1])
		}
	}

	sessionKey := fmt.Sprintf("%s:%d", targetID, connID)
	metaBytes, _ := json.Marshal(raw)

	switch {
	case action == "new":
		_, _ = s.store.StartSession(&store.Session{
			SessionKey: sessionKey,
			FromIP:     sourceIP,
			ToID:       targetID,
			ConnType:   "DEFAULT_CONN",
			IsRelay:    false,
			Status:     "active",
		})
		_ = s.store.AddAuditLog(&store.AuditLog{
			Category:  "connection",
			Action:    "session_started",
			Severity:  "info",
			TargetID:  targetID,
			SourceIP:  sourceIP,
			ConnID:    connID,
			SessionID: sessionID,
			Nonce:     nonce,
			Summary:   fmt.Sprintf("Incoming connection #%d to peer %s from %s", connID, targetID, sourceIP),
			Metadata:  metaBytes,
		})

	case action == "close":
		_ = s.store.UpdateSessionByKey(sessionKey, "", "", "", "closed")
		_ = s.store.AddAuditLog(&store.AuditLog{
			Category:  "connection",
			Action:    "session_closed",
			Severity:  "info",
			TargetID:  targetID,
			SourceIP:  sourceIP,
			ConnID:    connID,
			SessionID: sessionID,
			Nonce:     nonce,
			Summary:   fmt.Sprintf("Connection #%d to peer %s closed", connID, targetID),
			Metadata:  metaBytes,
		})

	default:
		// Authenticated connection update with peer tuple and session type
		_ = s.store.UpdateSessionByKey(sessionKey, peerID, peerName, connType, "")
		_ = s.store.AddAuditLog(&store.AuditLog{
			Category:  "connection",
			Action:    "session_authenticated",
			Severity:  "info",
			ActorID:   peerID,
			ActorName: peerName,
			TargetID:  targetID,
			SourceIP:  sourceIP,
			ConnID:    connID,
			SessionID: sessionID,
			ConnType:  connType,
			Nonce:     nonce,
			Summary:   fmt.Sprintf("Peer %s (%s) authenticated to %s (%s)", fallback(peerID, "unknown"), fallback(peerName, "-"), targetID, connType),
			Metadata:  metaBytes,
		})
		slog.InfoContext(r.Context(), "client connection audit recorded",
			"component", "api", "action", "authenticated", "target_id", targetID, "actor_id", peerID, "conn_type", connType, "conn_id", connID)
	}
	s.recordClientEvent(r, "audit_conn")
	if m := s.metrics(); m != nil {
		m.AuditEventsTotal.Add(r.Context(), 1, metric.WithAttributes(attribute.String("category", "connection")))
	}

	w.WriteHeader(http.StatusOK)
}

// handleAuditFile receives POST /api/audit/file from RustDesk clients.
func (s *Server) handleAuditFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var raw map[string]any
	if err := decodeJSON(r, &raw); err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	targetID := asString(raw["id"])
	peerID := asString(raw["peer_id"])
	connID := asInt64(raw["conn_id"])
	path := asString(raw["path"])
	nonce := asString(raw["nonce"])
	fileType := asInt64(raw["type"])
	action := "file_transfer"
	if fileType == 0 {
		action = "file_upload"
	} else if fileType == 1 {
		action = "file_download"
	}
	metaBytes, _ := json.Marshal(raw)
	_ = s.store.AddAuditLog(&store.AuditLog{
		Category: "file",
		Action:   action,
		Severity: "info",
		ActorID:  peerID,
		TargetID: targetID,
		SourceIP: clientIP(r),
		ConnID:   connID,
		ConnType: "FILE_TRANSFER",
		Nonce:    nonce,
		Summary:  fmt.Sprintf("File transfer (%s) between %s and %s: %s", action, fallback(peerID, "remote"), targetID, path),
		Metadata: metaBytes,
	})
	s.recordClientEvent(r, "audit_file")
	if m := s.metrics(); m != nil {
		m.AuditEventsTotal.Add(r.Context(), 1, metric.WithAttributes(attribute.String("category", "file")))
	}
	slog.InfoContext(r.Context(), "client file transfer audit recorded",
		"component", "api", "action", action, "actor_id", peerID, "target_id", targetID, "path", path)
	w.WriteHeader(http.StatusOK)
}

// handleAuditAlarm receives POST /api/audit/alarm from RustDesk clients.
func (s *Server) handleAuditAlarm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var raw map[string]any
	if err := decodeJSON(r, &raw); err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	targetID := asString(raw["id"])
	nonce := asString(raw["nonce"])
	metaBytes, _ := json.Marshal(raw)
	_ = s.store.AddAuditLog(&store.AuditLog{
		Category: "alarm",
		Action:   "client_alarm",
		Severity: "warn",
		TargetID: targetID,
		SourceIP: clientIP(r),
		Nonce:    nonce,
		Summary:  fmt.Sprintf("Security alarm reported by peer %s", targetID),
		Metadata: metaBytes,
	})
	s.recordClientEvent(r, "audit_alarm")
	if m := s.metrics(); m != nil {
		m.AuditEventsTotal.Add(r.Context(), 1, metric.WithAttributes(attribute.String("category", "alarm")))
	}
	slog.WarnContext(r.Context(), "client security alarm reported",
		"component", "api", "target_id", targetID, "source_ip", clientIP(r))
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleLoginOptions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, []string{})
}

// handleRustDeskLogin authenticates RustDesk desktop/mobile clients (POST /api/login).
func (s *Server) handleRustDeskLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		ID       string `json:"id"`
		UUID     string `json:"uuid"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request"})
		return
	}
	u, err := s.store.AuthenticateUser(req.Username, req.Password)
	if err != nil {
		s.recordClientEvent(r, "login_failed")
		if m := s.metrics(); m != nil {
			m.AuthFailures.Add(r.Context(), 1, metric.WithAttributes(attribute.String("surface", "client")))
		}
		slog.WarnContext(r.Context(), "rustdesk client login failed",
			"component", "api", "username", req.Username, "peer_id", req.ID, "client_ip", clientIP(r))
		_ = s.store.AddAuditLog(&store.AuditLog{
			Category:  "security",
			Action:    "client_login_failed",
			Severity:  "warn",
			ActorName: req.Username,
			ActorID:   req.ID,
			SourceIP:  clientIP(r),
			Summary:   fmt.Sprintf("Failed RustDesk client login for user %q from %s", req.Username, clientIP(r)),
		})
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Invalid username or password"})
		return
	}
	tok, err := s.store.CreateToken(u, req.ID, req.UUID, 30*24*time.Hour)
	if err != nil {
		slog.ErrorContext(r.Context(), "failed to issue client token", "component", "api", "username", u.Username, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Token creation failed"})
		return
	}
	s.recordClientEvent(r, "login")
	slog.InfoContext(r.Context(), "rustdesk client signed in",
		"component", "api", "username", u.Username, "peer_id", req.ID, "client_ip", clientIP(r))
	_ = s.store.AddAuditLog(&store.AuditLog{
		Category:  "admin",
		Action:    "client_login",
		Severity:  "info",
		ActorID:   req.ID,
		ActorName: u.Username,
		SourceIP:  clientIP(r),
		Summary:   fmt.Sprintf("User %s signed into RustDesk client (%s)", u.Username, fallback(req.ID, "web")),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": tok,
		"type":         "access_token",
		"user": map[string]any{
			"name":     u.Username,
			"email":    u.Email,
			"status":   u.Status,
			"is_admin": u.IsAdmin,
			"info":     map[string]any{},
		},
	})
}

func (s *Server) handleRustDeskLogout(w http.ResponseWriter, r *http.Request) {
	tok := s.extractBearer(r)
	if tok != "" {
		_ = s.store.RevokeToken(tok)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleCurrentUser(w http.ResponseWriter, r *http.Request) {
	u, err := s.authenticateRequest(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":     u.Username,
		"email":    u.Email,
		"status":   u.Status,
		"is_admin": u.IsAdmin,
		"info":     map[string]any{},
	})
}

func (s *Server) handleABSettings(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticateRequest(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"max_peer_one_ab": 0})
}

func (s *Server) handleABPersonal(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticateRequest(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"guid": "personal", "rule": 3})
}

func (s *Server) handleABSharedProfiles(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticateRequest(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": 0, "data": []any{}})
}

func (s *Server) handleABPeers(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticateRequest(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	peers, err := s.enrichedPeers()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	data := make([]map[string]any, 0, len(peers))
	for _, p := range peers {
		data = append(data, map[string]any{
			"id":       p.ID,
			"username": p.Username,
			"hostname": p.Hostname,
			"alias":    p.Alias,
			"platform": p.OS,
			"tags":     p.Tags,
			"note":     p.Note,
			"hash":     "",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(data), "data": data})
}

func (s *Server) handleABPeerAdd(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticateRequest(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	var req struct {
		ID       string   `json:"id"`
		Username string   `json:"username"`
		Hostname string   `json:"hostname"`
		Alias    string   `json:"alias"`
		Platform string   `json:"platform"`
		Note     string   `json:"note"`
		Tags     []string `json:"tags"`
	}
	if err := decodeJSON(r, &req); err != nil || req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid peer"})
		return
	}
	_ = s.store.UpsertPeerManual(req.ID, req.Alias, req.Hostname, req.Username, req.Platform, req.Note, "Default", req.Tags)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleABPeerUpdate(w http.ResponseWriter, r *http.Request) {
	s.handleABPeerAdd(w, r)
}

func (s *Server) handleABPeersDelete(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticateRequest(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	var ids []string
	if err := decodeJSON(r, &ids); err == nil {
		for _, id := range ids {
			_ = s.store.DeletePeer(id)
			if s.rendezvous != nil {
				s.rendezvous.RemovePeer(id)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleABTags(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticateRequest(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	if r.Method == http.MethodDelete {
		var names []string
		if err := decodeJSON(r, &names); err == nil {
			for _, name := range names {
				_ = s.store.DeleteTag(name)
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	tags, _ := s.store.ListTags()
	out := make([]map[string]any, 0, len(tags))
	for _, t := range tags {
		out = append(out, map[string]any{"name": t.Name, "color": t.Color})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleABTagAdd(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticateRequest(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	var req struct {
		Name  string `json:"name"`
		Color uint32 `json:"color"`
	}
	if err := decodeJSON(r, &req); err == nil && req.Name != "" {
		_ = s.store.UpsertTag(req.Name, req.Color)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleABTagRename(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticateRequest(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	var req struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := decodeJSON(r, &req); err == nil && req.Old != "" && req.New != "" {
		_ = s.store.RenameTag(req.Old, req.New)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleABTagUpdate(w http.ResponseWriter, r *http.Request) {
	s.handleABTagAdd(w, r)
}

func (s *Server) handleLegacyAB(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticateRequest(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	if r.Method == http.MethodPost {
		var body struct {
			Data string `json:"data"`
		}
		if err := decodeJSON(r, &body); err == nil && body.Data != "" {
			var ab struct {
				Tags      []string          `json:"tags"`
				TagColors map[string]uint32 `json:"tag_colors"`
				Peers     []struct {
					ID       string   `json:"id"`
					Username string   `json:"username"`
					Hostname string   `json:"hostname"`
					Alias    string   `json:"alias"`
					Platform string   `json:"platform"`
					Tags     []string `json:"tags"`
					Note     string   `json:"note"`
				} `json:"peers"`
			}
			if json.Unmarshal([]byte(body.Data), &ab) == nil {
				for _, t := range ab.Tags {
					_ = s.store.UpsertTag(t, ab.TagColors[t])
				}
				for _, p := range ab.Peers {
					if p.ID != "" {
						_ = s.store.UpsertPeerManual(p.ID, p.Alias, p.Hostname, p.Username, p.Platform, p.Note, "Default", p.Tags)
					}
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	peers, _ := s.enrichedPeers()
	tags, _ := s.store.ListTags()
	tagNames := make([]string, 0, len(tags))
	tagColors := make(map[string]uint32, len(tags))
	for _, t := range tags {
		tagNames = append(tagNames, t.Name)
		tagColors[t.Name] = t.Color
	}
	abPeers := make([]map[string]any, 0, len(peers))
	for _, p := range peers {
		abPeers = append(abPeers, map[string]any{
			"id":       p.ID,
			"username": p.Username,
			"hostname": p.Hostname,
			"alias":    p.Alias,
			"platform": p.OS,
			"tags":     p.Tags,
			"note":     p.Note,
		})
	}
	dataBytes, _ := json.Marshal(map[string]any{
		"tags":       tagNames,
		"tag_colors": tagColors,
		"peers":      abPeers,
	})
	writeJSON(w, http.StatusOK, map[string]any{"data": string(dataBytes)})
}

func (s *Server) handleRustDeskPeers(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticateRequest(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	peers, _ := s.enrichedPeers()
	data := make([]map[string]any, 0, len(peers))
	for _, p := range peers {
		data = append(data, map[string]any{
			"id":   p.ID,
			"info": map[string]any{"username": p.Username, "device_name": p.Hostname, "os": p.OS},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(data), "data": data})
}

func (s *Server) handleRustDeskUsers(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticateRequest(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	users, _ := s.store.ListUsers()
	data := make([]map[string]any, 0, len(users))
	for _, u := range users {
		data = append(data, map[string]any{"name": u.Username, "email": u.Email, "status": u.Status, "is_admin": u.IsAdmin})
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(data), "data": data})
}

func (s *Server) handleDeviceGroups(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticateRequest(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": 0, "data": []any{}})
}

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	default:
		return ""
	}
}

func asInt64(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	default:
		return 0
	}
}

func connTypeLabel(code int64) string {
	switch code {
	case 0:
		return "DEFAULT_CONN"
	case 1:
		return "FILE_TRANSFER"
	case 2:
		return "PORT_FORWARD"
	case 3:
		return "RDP"
	case 4:
		return "VIEW_CAMERA"
	case 5:
		return "TERMINAL"
	default:
		return "DEFAULT_CONN"
	}
}

func fallback(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
