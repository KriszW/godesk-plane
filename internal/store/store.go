package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

const (
	// PeerOnlineTimeout matches rustdesk-server's REG_TIMEOUT (30s) plus a small grace window for heartbeats.
	PeerRegOnlineWindow       = 45 * time.Second
	PeerHeartbeatOnlineWindow = 60 * time.Second
)

// Store wraps SQLite persistence for peers, users, address book, sessions, analytics, and audit logs.
type Store struct {
	db *sql.DB
}

// Peer represents a RustDesk client registered with Rendezvous and/or Address Book.
type Peer struct {
	ID              string   `json:"id"`
	UUID            []byte   `json:"-"`
	UUIDHex         string   `json:"uuid,omitempty"`
	PK              []byte   `json:"-"`
	HasPK           bool     `json:"has_pk"`
	IP              string   `json:"ip"`
	SocketAddr      string   `json:"socket_addr,omitempty"`
	Hostname        string   `json:"hostname"`
	Username        string   `json:"username"`
	OS              string   `json:"os"`
	CPU             string   `json:"cpu"`
	Memory          string   `json:"memory"`
	Version         string   `json:"version"`
	Alias           string   `json:"alias"`
	Note            string   `json:"note"`
	GroupName       string   `json:"group_name"`
	Tags            []string `json:"tags"`
	NATType         string   `json:"nat_type"`
	InAddressBook   bool     `json:"in_address_book"`
	Online          bool     `json:"online"`
	LastRegAt       int64    `json:"last_reg_at"`
	LastHeartbeatAt int64    `json:"last_heartbeat_at"`
	LastSeenAt      int64    `json:"last_seen_at"`
	CreatedAt       int64    `json:"created_at"`
	UpdatedAt       int64    `json:"updated_at"`
}

// Tag represents an address book tag with ARGB color (compatible with RustDesk client).
type Tag struct {
	Name      string `json:"name"`
	Color     uint32 `json:"color"`
	CreatedAt int64  `json:"created_at"`
}

// User represents a GoDesk Plane / RustDesk API account.
type User struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	Email        string `json:"email"`
	PasswordHash string `json:"-"`
	IsAdmin      bool   `json:"is_admin"`
	Status       int    `json:"status"` // 1 = active, 0 = disabled
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
}

// Session represents a remote desktop or relay session.
type Session struct {
	ID          int64  `json:"id"`
	SessionKey  string `json:"session_key,omitempty"`
	RelayUUID   string `json:"relay_uuid,omitempty"`
	FromID      string `json:"from_id"`
	FromName    string `json:"from_name"`
	FromIP      string `json:"from_ip"`
	ToID        string `json:"to_id"`
	ToIP        string `json:"to_ip"`
	ConnType    string `json:"conn_type"`
	IsRelay     bool   `json:"is_relay"`
	Status      string `json:"status"` // active, closed, failed
	BytesSent   int64  `json:"bytes_sent"`
	BytesRecv   int64  `json:"bytes_recv"`
	DurationSec int64  `json:"duration_sec"`
	StartedAt   int64  `json:"started_at"`
	EndedAt     int64  `json:"ended_at"`
}

// AuditLog represents an event recorded by the server, relay, client API, or admin console.
type AuditLog struct {
	ID               int64           `json:"id"`
	Category         string          `json:"category"` // connection, relay, file, security, peer, admin, alarm
	Action           string          `json:"action"`
	Severity         string          `json:"severity"` // info, warn, error
	ActorID          string          `json:"actor_id"`
	ActorName        string          `json:"actor_name"`
	TargetID         string          `json:"target_id"`
	TargetName       string          `json:"target_name"`
	SourceIP         string          `json:"source_ip"`
	ConnID           int64           `json:"conn_id"`
	SessionID        string          `json:"session_id"`
	ConnType         string          `json:"conn_type"`
	DurationSec      int64           `json:"duration_sec"`
	BytesTransferred int64           `json:"bytes_transferred"`
	Nonce            string          `json:"nonce,omitempty"`
	Summary          string          `json:"summary"`
	Metadata         json.RawMessage `json:"metadata,omitempty"`
	CreatedAt        int64           `json:"created_at"`
}

// AuditFilter controls filtering and pagination on audit log queries.
type AuditFilter struct {
	Category string
	Action   string
	Severity string
	Search   string
	PeerID   string
	FromMS   int64
	ToMS     int64
	Page     int
	PageSize int
}

// AnalyticsData aggregates telemetry across peers, sessions, relay traffic, and audit logs.
type AnalyticsData struct {
	Range             string               `json:"range"`
	TotalPeers        int                  `json:"total_peers"`
	OnlinePeers       int                  `json:"online_peers"`
	OfflinePeers      int                  `json:"offline_peers"`
	ActiveSessions    int                  `json:"active_sessions"`
	TotalConnections  int                  `json:"total_connections"`
	RelayConnections  int                  `json:"relay_connections"`
	DirectConnections int                  `json:"direct_connections"`
	TotalBytesRelayed int64                `json:"total_bytes_relayed"`
	AvgDurationSec    float64              `json:"avg_duration_sec"`
	TotalAuditEvents  int                  `json:"total_audit_events"`
	SecurityWarnings  int                  `json:"security_warnings"`
	ConnectionsSeries []TimeSeriesPoint    `json:"connections_series"`
	TrafficSeries     []TrafficSeriesPoint `json:"traffic_series"`
	OSBreakdown       []LabelCount         `json:"os_breakdown"`
	VersionBreakdown  []LabelCount         `json:"version_breakdown"`
	NATBreakdown      []LabelCount         `json:"nat_breakdown"`
	ConnTypeBreakdown []LabelCount         `json:"conn_type_breakdown"`
	TopDevices        []TopDeviceStat      `json:"top_devices"`
}

type TimeSeriesPoint struct {
	Bucket string `json:"bucket"`
	Total  int    `json:"total"`
	Relay  int    `json:"relay"`
	Direct int    `json:"direct"`
}

type TrafficSeriesPoint struct {
	Bucket string `json:"bucket"`
	Bytes  int64  `json:"bytes"`
}

type LabelCount struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

type TopDeviceStat struct {
	PeerID      string `json:"peer_id"`
	Label       string `json:"label"`
	OS          string `json:"os"`
	Online      bool   `json:"online"`
	Connections int    `json:"connections"`
	Bytes       int64  `json:"bytes"`
}

// Open initializes the SQLite database in dbPath (or :memory:) and runs migrations.
func Open(dbPath string) (*Store, error) {
	if dbPath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	dsn := dbPath
	if dbPath != ":memory:" {
		dsn = dbPath + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Single writer connection keeps SQLite completely lock-free under high concurrency.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS peers (
		id TEXT PRIMARY KEY,
		uuid BLOB,
		pk BLOB,
		ip TEXT NOT NULL DEFAULT '',
		socket_addr TEXT NOT NULL DEFAULT '',
		hostname TEXT NOT NULL DEFAULT '',
		username TEXT NOT NULL DEFAULT '',
		os TEXT NOT NULL DEFAULT '',
		cpu TEXT NOT NULL DEFAULT '',
		memory TEXT NOT NULL DEFAULT '',
		version TEXT NOT NULL DEFAULT '',
		alias TEXT NOT NULL DEFAULT '',
		note TEXT NOT NULL DEFAULT '',
		group_name TEXT NOT NULL DEFAULT 'Default',
		tags_json TEXT NOT NULL DEFAULT '[]',
		nat_type TEXT NOT NULL DEFAULT 'UNKNOWN',
		in_address_book INTEGER NOT NULL DEFAULT 1,
		last_reg_at INTEGER NOT NULL DEFAULT 0,
		last_heartbeat_at INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS ab_tags (
		name TEXT PRIMARY KEY,
		color INTEGER NOT NULL DEFAULT 4282088182,
		created_at INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT NOT NULL UNIQUE,
		email TEXT NOT NULL DEFAULT '',
		password_hash TEXT NOT NULL,
		is_admin INTEGER NOT NULL DEFAULT 0,
		status INTEGER NOT NULL DEFAULT 1,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS auth_tokens (
		token TEXT PRIMARY KEY,
		user_id INTEGER NOT NULL,
		peer_id TEXT NOT NULL DEFAULT '',
		peer_uuid TEXT NOT NULL DEFAULT '',
		is_admin INTEGER NOT NULL DEFAULT 0,
		expires_at INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS sessions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_key TEXT NOT NULL DEFAULT '',
		relay_uuid TEXT NOT NULL DEFAULT '',
		from_id TEXT NOT NULL DEFAULT '',
		from_name TEXT NOT NULL DEFAULT '',
		from_ip TEXT NOT NULL DEFAULT '',
		to_id TEXT NOT NULL DEFAULT '',
		to_ip TEXT NOT NULL DEFAULT '',
		conn_type TEXT NOT NULL DEFAULT 'DEFAULT_CONN',
		is_relay INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'active',
		bytes_sent INTEGER NOT NULL DEFAULT 0,
		bytes_recv INTEGER NOT NULL DEFAULT 0,
		duration_sec INTEGER NOT NULL DEFAULT 0,
		started_at INTEGER NOT NULL,
		ended_at INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_sessions_started_at ON sessions(started_at);
	CREATE INDEX IF NOT EXISTS idx_sessions_relay_uuid ON sessions(relay_uuid);
	CREATE INDEX IF NOT EXISTS idx_sessions_to_id ON sessions(to_id);

	CREATE TABLE IF NOT EXISTS audit_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		category TEXT NOT NULL,
		action TEXT NOT NULL,
		severity TEXT NOT NULL DEFAULT 'info',
		actor_id TEXT NOT NULL DEFAULT '',
		actor_name TEXT NOT NULL DEFAULT '',
		target_id TEXT NOT NULL DEFAULT '',
		target_name TEXT NOT NULL DEFAULT '',
		source_ip TEXT NOT NULL DEFAULT '',
		conn_id INTEGER NOT NULL DEFAULT 0,
		session_id TEXT NOT NULL DEFAULT '',
		conn_type TEXT NOT NULL DEFAULT '',
		duration_sec INTEGER NOT NULL DEFAULT 0,
		bytes_transferred INTEGER NOT NULL DEFAULT 0,
		nonce TEXT NOT NULL DEFAULT '',
		summary TEXT NOT NULL DEFAULT '',
		metadata_json TEXT NOT NULL DEFAULT '{}',
		created_at INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_audit_created_at ON audit_logs(created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_audit_category ON audit_logs(category);
	CREATE INDEX IF NOT EXISTS idx_audit_target ON audit_logs(target_id);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_audit_nonce ON audit_logs(nonce) WHERE nonce != '';

	CREATE TABLE IF NOT EXISTS settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	`
	_, err := s.db.Exec(schema)
	return err
}

func nowMS() int64 {
	return time.Now().UnixMilli()
}

func computeOnline(lastRegAt, lastHeartbeatAt int64) bool {
	now := time.Now().UnixMilli()
	if lastRegAt > 0 && now-lastRegAt <= PeerRegOnlineWindow.Milliseconds() {
		return true
	}
	if lastHeartbeatAt > 0 && now-lastHeartbeatAt <= PeerHeartbeatOnlineWindow.Milliseconds() {
		return true
	}
	return false
}

// EnsureAdminUser creates an initial admin user if no admin exists yet.
func (s *Store) EnsureAdminUser(username, password, email string) error {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE is_admin = 1`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	_, err := s.CreateUser(username, email, password, true)
	return err
}

// CreateUser inserts a new user with bcrypt-hashed password.
func (s *Store) CreateUser(username, email, password string, isAdmin bool) (*User, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return nil, errors.New("username and password are required")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	now := nowMS()
	adm := 0
	if isAdmin {
		adm = 1
	}
	res, err := s.db.Exec(
		`INSERT INTO users (username, email, password_hash, is_admin, status, created_at, updated_at) VALUES (?, ?, ?, ?, 1, ?, ?)`,
		username, strings.TrimSpace(email), string(hash), adm, now, now,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &User{
		ID:        id,
		Username:  username,
		Email:     strings.TrimSpace(email),
		IsAdmin:   isAdmin,
		Status:    1,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// AuthenticateUser verifies username and password and returns the user.
func (s *Store) AuthenticateUser(username, password string) (*User, error) {
	u, err := s.GetUserByUsername(username)
	if err != nil {
		return nil, err
	}
	if u.Status != 1 {
		return nil, errors.New("account disabled")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return nil, errors.New("invalid credentials")
	}
	return u, nil
}

// GetUserByUsername fetches a user by username.
func (s *Store) GetUserByUsername(username string) (*User, error) {
	var u User
	var isAdmin int
	err := s.db.QueryRow(
		`SELECT id, username, email, password_hash, is_admin, status, created_at, updated_at FROM users WHERE username = ?`,
		username,
	).Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash, &isAdmin, &u.Status, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	u.IsAdmin = isAdmin == 1
	return &u, nil
}

// GetUserByID fetches a user by ID.
func (s *Store) GetUserByID(id int64) (*User, error) {
	var u User
	var isAdmin int
	err := s.db.QueryRow(
		`SELECT id, username, email, password_hash, is_admin, status, created_at, updated_at FROM users WHERE id = ?`,
		id,
	).Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash, &isAdmin, &u.Status, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	u.IsAdmin = isAdmin == 1
	return &u, nil
}

// ListUsers returns all users.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT id, username, email, is_admin, status, created_at, updated_at FROM users ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		var isAdmin int
		if err := rows.Scan(&u.ID, &u.Username, &u.Email, &isAdmin, &u.Status, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		u.IsAdmin = isAdmin == 1
		users = append(users, u)
	}
	return users, rows.Err()
}

// UpdateUser updates email, role, status, and optionally password.
func (s *Store) UpdateUser(id int64, email string, isAdmin bool, status int, newPassword string) error {
	adm := 0
	if isAdmin {
		adm = 1
	}
	now := nowMS()
	if strings.TrimSpace(newPassword) != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		_, err = s.db.Exec(
			`UPDATE users SET email = ?, is_admin = ?, status = ?, password_hash = ?, updated_at = ? WHERE id = ?`,
			strings.TrimSpace(email), adm, status, string(hash), now, id,
		)
		return err
	}
	_, err := s.db.Exec(
		`UPDATE users SET email = ?, is_admin = ?, status = ?, updated_at = ? WHERE id = ?`,
		strings.TrimSpace(email), adm, status, now, id,
	)
	return err
}

// DeleteUser removes a user by ID.
func (s *Store) DeleteUser(id int64) error {
	_, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	return err
}

// CreateToken generates and stores an auth token for a user.
func (s *Store) CreateToken(user *User, peerID, peerUUID string, ttl time.Duration) (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw[:])
	now := nowMS()
	exp := time.Now().Add(ttl).UnixMilli()
	adm := 0
	if user.IsAdmin {
		adm = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO auth_tokens (token, user_id, peer_id, peer_uuid, is_admin, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		token, user.ID, peerID, peerUUID, adm, exp, now,
	)
	if err != nil {
		return "", err
	}
	return token, nil
}

// ValidateToken verifies a bearer token and returns its associated user.
func (s *Store) ValidateToken(token string) (*User, error) {
	if token == "" {
		return nil, errors.New("missing token")
	}
	var userID int64
	var exp int64
	err := s.db.QueryRow(`SELECT user_id, expires_at FROM auth_tokens WHERE token = ?`, token).Scan(&userID, &exp)
	if err != nil {
		return nil, err
	}
	if exp < nowMS() {
		_, _ = s.db.Exec(`DELETE FROM auth_tokens WHERE token = ?`, token)
		return nil, errors.New("token expired")
	}
	u, err := s.GetUserByID(userID)
	if err != nil {
		return nil, err
	}
	if u.Status != 1 {
		return nil, errors.New("account disabled")
	}
	return u, nil
}

// RevokeToken deletes a token.
func (s *Store) RevokeToken(token string) error {
	_, err := s.db.Exec(`DELETE FROM auth_tokens WHERE token = ?`, token)
	return err
}

// UpsertPeerPK registers or updates a peer's cryptographic identity (RegisterPk).
func (s *Store) UpsertPeerPK(id string, uuid, pk []byte, ip, socketAddr string) error {
	now := nowMS()
	_, err := s.db.Exec(`
		INSERT INTO peers (id, uuid, pk, ip, socket_addr, last_reg_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			uuid = excluded.uuid,
			pk = excluded.pk,
			ip = excluded.ip,
			socket_addr = excluded.socket_addr,
			last_reg_at = excluded.last_reg_at,
			updated_at = excluded.updated_at
	`, id, uuid, pk, ip, socketAddr, now, now, now)
	return err
}

// TouchPeerReg updates a peer's last rendezvous registration timestamp and socket address.
func (s *Store) TouchPeerReg(id, ip, socketAddr string) error {
	now := nowMS()
	_, err := s.db.Exec(`
		UPDATE peers SET ip = ?, socket_addr = ?, last_reg_at = ?, updated_at = ? WHERE id = ?
	`, ip, socketAddr, now, now, id)
	return err
}

// UpdatePeerNAT records the observed NAT type of a peer.
func (s *Store) UpdatePeerNAT(id, natType string) error {
	if id == "" || natType == "" {
		return nil
	}
	_, err := s.db.Exec(`UPDATE peers SET nat_type = ?, updated_at = ? WHERE id = ?`, natType, nowMS(), id)
	return err
}

// UpsertPeerSysinfo stores system info reported by RustDesk client (/api/sysinfo).
func (s *Store) UpsertPeerSysinfo(id, ip, hostname, username, osName, cpu, memory, version string, uuid []byte, alias, note, groupName string, tags []string) error {
	now := nowMS()
	var existing Peer
	err := s.db.QueryRow(`SELECT alias, note, group_name, tags_json FROM peers WHERE id = ?`, id).
		Scan(&existing.Alias, &existing.Note, &existing.GroupName, &existing.Hostname)
	// Existing manual edits take priority unless empty
	if err == nil {
		if existing.Alias != "" {
			alias = existing.Alias
		}
		if existing.Note != "" {
			note = existing.Note
		}
		if existing.GroupName != "" && existing.GroupName != "Default" {
			groupName = existing.GroupName
		}
	}
	if groupName == "" {
		groupName = "Default"
	}
	tagsJSON := "[]"
	if len(tags) > 0 {
		if b, mErr := json.Marshal(tags); mErr == nil {
			tagsJSON = string(b)
		}
	}
	_, err = s.db.Exec(`
		INSERT INTO peers (id, uuid, ip, hostname, username, os, cpu, memory, version, alias, note, group_name, tags_json, last_heartbeat_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			ip = CASE WHEN excluded.ip != '' THEN excluded.ip ELSE peers.ip END,
			hostname = CASE WHEN excluded.hostname != '' THEN excluded.hostname ELSE peers.hostname END,
			username = CASE WHEN excluded.username != '' THEN excluded.username ELSE peers.username END,
			os = CASE WHEN excluded.os != '' THEN excluded.os ELSE peers.os END,
			cpu = CASE WHEN excluded.cpu != '' THEN excluded.cpu ELSE peers.cpu END,
			memory = CASE WHEN excluded.memory != '' THEN excluded.memory ELSE peers.memory END,
			version = CASE WHEN excluded.version != '' THEN excluded.version ELSE peers.version END,
			alias = CASE WHEN peers.alias = '' AND excluded.alias != '' THEN excluded.alias ELSE peers.alias END,
			note = CASE WHEN peers.note = '' AND excluded.note != '' THEN excluded.note ELSE peers.note END,
			group_name = CASE WHEN (peers.group_name = '' OR peers.group_name = 'Default') AND excluded.group_name != '' THEN excluded.group_name ELSE peers.group_name END,
			last_heartbeat_at = excluded.last_heartbeat_at,
			updated_at = excluded.updated_at
	`, id, uuid, ip, hostname, username, osName, cpu, memory, version, alias, note, groupName, tagsJSON, now, now, now)
	return err
}

// TouchPeerHeartbeat updates a peer's HTTP heartbeat timestamp and returns whether sysinfo is missing.
func (s *Store) TouchPeerHeartbeat(id, ip, version string, uuid []byte) (needSysinfo bool, err error) {
	now := nowMS()
	var hostname string
	err = s.db.QueryRow(`SELECT hostname FROM peers WHERE id = ?`, id).Scan(&hostname)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = s.db.Exec(`
			INSERT INTO peers (id, uuid, ip, version, last_heartbeat_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, id, uuid, ip, version, now, now, now)
		return true, err
	}
	if err != nil {
		return false, err
	}
	_, err = s.db.Exec(`
		UPDATE peers SET
			ip = CASE WHEN ? != '' THEN ? ELSE ip END,
			version = CASE WHEN version = '' AND ? != '' THEN ? ELSE version END,
			last_heartbeat_at = ?,
			updated_at = ?
		WHERE id = ?
	`, ip, ip, version, version, now, now, id)
	return hostname == "", err
}

// UpsertPeerManual creates or updates a peer entry from the Address Book / Management UI.
func (s *Store) UpsertPeerManual(id, alias, hostname, username, osName, note, groupName string, tags []string) error {
	now := nowMS()
	if groupName == "" {
		groupName = "Default"
	}
	if tags == nil {
		tags = []string{}
	}
	tagsBytes, _ := json.Marshal(tags)
	_, err := s.db.Exec(`
		INSERT INTO peers (id, alias, hostname, username, os, note, group_name, tags_json, in_address_book, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			alias = excluded.alias,
			hostname = CASE WHEN excluded.hostname != '' THEN excluded.hostname ELSE peers.hostname END,
			username = CASE WHEN excluded.username != '' THEN excluded.username ELSE peers.username END,
			os = CASE WHEN excluded.os != '' THEN excluded.os ELSE peers.os END,
			note = excluded.note,
			group_name = excluded.group_name,
			tags_json = excluded.tags_json,
			in_address_book = 1,
			updated_at = excluded.updated_at
	`, id, alias, hostname, username, osName, note, groupName, string(tagsBytes), now, now)
	return err
}

// UpdatePeerMeta updates editable metadata (alias, note, group, tags) on an existing peer.
func (s *Store) UpdatePeerMeta(id, alias, note, groupName string, tags []string) error {
	if groupName == "" {
		groupName = "Default"
	}
	if tags == nil {
		tags = []string{}
	}
	tagsBytes, _ := json.Marshal(tags)
	res, err := s.db.Exec(`
		UPDATE peers SET alias = ?, note = ?, group_name = ?, tags_json = ?, updated_at = ? WHERE id = ?
	`, alias, note, groupName, string(tagsBytes), nowMS(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeletePeer deletes a peer record by ID.
func (s *Store) DeletePeer(id string) error {
	_, err := s.db.Exec(`DELETE FROM peers WHERE id = ?`, id)
	return err
}

func scanPeer(scanner interface{ Scan(...any) error }) (*Peer, error) {
	var p Peer
	var tagsJSON string
	var inAB int
	err := scanner.Scan(
		&p.ID, &p.UUID, &p.PK, &p.IP, &p.SocketAddr,
		&p.Hostname, &p.Username, &p.OS, &p.CPU, &p.Memory, &p.Version,
		&p.Alias, &p.Note, &p.GroupName, &tagsJSON, &p.NATType, &inAB,
		&p.LastRegAt, &p.LastHeartbeatAt, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	p.HasPK = len(p.PK) > 0
	if len(p.UUID) > 0 {
		p.UUIDHex = hex.EncodeToString(p.UUID)
	}
	p.InAddressBook = inAB == 1
	p.Online = computeOnline(p.LastRegAt, p.LastHeartbeatAt)
	p.LastSeenAt = p.LastRegAt
	if p.LastHeartbeatAt > p.LastSeenAt {
		p.LastSeenAt = p.LastHeartbeatAt
	}
	if err := json.Unmarshal([]byte(tagsJSON), &p.Tags); err != nil || p.Tags == nil {
		p.Tags = []string{}
	}
	return &p, nil
}

// GetPeer retrieves a peer by RustDesk ID.
func (s *Store) GetPeer(id string) (*Peer, error) {
	row := s.db.QueryRow(`
		SELECT id, uuid, pk, ip, socket_addr, hostname, username, os, cpu, memory, version,
		       alias, note, group_name, tags_json, nat_type, in_address_book,
		       last_reg_at, last_heartbeat_at, created_at, updated_at
		FROM peers WHERE id = ?
	`, id)
	return scanPeer(row)
}

// ListPeers returns all known peers sorted by online status then last seen descending.
func (s *Store) ListPeers() ([]Peer, error) {
	rows, err := s.db.Query(`
		SELECT id, uuid, pk, ip, socket_addr, hostname, username, os, cpu, memory, version,
		       alias, note, group_name, tags_json, nat_type, in_address_book,
		       last_reg_at, last_heartbeat_at, created_at, updated_at
		FROM peers
		ORDER BY MAX(last_reg_at, last_heartbeat_at) DESC, created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var peers []Peer
	for rows.Next() {
		p, err := scanPeer(rows)
		if err != nil {
			return nil, err
		}
		peers = append(peers, *p)
	}
	return peers, rows.Err()
}

// UpsertTag creates or updates an address book tag.
func (s *Store) UpsertTag(name string, color uint32) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("tag name required")
	}
	if color == 0 {
		color = 0xFF3B82F6
	}
	_, err := s.db.Exec(`
		INSERT INTO ab_tags (name, color, created_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET color = excluded.color
	`, name, color, nowMS())
	return err
}

// RenameTag renames a tag in ab_tags and across all peers.
func (s *Store) RenameTag(oldName, newName string) error {
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == "" || newName == "" {
		return errors.New("old and new tag names required")
	}
	_, err := s.db.Exec(`UPDATE ab_tags SET name = ? WHERE name = ?`, newName, oldName)
	if err != nil {
		return err
	}
	peers, err := s.ListPeers()
	if err != nil {
		return err
	}
	for _, p := range peers {
		changed := false
		for i, t := range p.Tags {
			if t == oldName {
				p.Tags[i] = newName
				changed = true
			}
		}
		if changed {
			_ = s.UpdatePeerMeta(p.ID, p.Alias, p.Note, p.GroupName, p.Tags)
		}
	}
	return nil
}

// DeleteTag removes a tag from ab_tags and strips it from peers.
func (s *Store) DeleteTag(name string) error {
	_, err := s.db.Exec(`DELETE FROM ab_tags WHERE name = ?`, name)
	if err != nil {
		return err
	}
	peers, err := s.ListPeers()
	if err != nil {
		return err
	}
	for _, p := range peers {
		var kept []string
		changed := false
		for _, t := range p.Tags {
			if t == name {
				changed = true
				continue
			}
			kept = append(kept, t)
		}
		if changed {
			_ = s.UpdatePeerMeta(p.ID, p.Alias, p.Note, p.GroupName, kept)
		}
	}
	return nil
}

// ListTags returns all defined tags.
func (s *Store) ListTags() ([]Tag, error) {
	rows, err := s.db.Query(`SELECT name, color, created_at FROM ab_tags ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tags []Tag
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.Name, &t.Color, &t.CreatedAt); err != nil {
			return nil, err
		}
		tags = append(tags, t)
	}
	return tags, rows.Err()
}

// StartSession records a new active connection or relay session.
func (s *Store) StartSession(sess *Session) (int64, error) {
	if sess.StartedAt == 0 {
		sess.StartedAt = nowMS()
	}
	if sess.Status == "" {
		sess.Status = "active"
	}
	if sess.ConnType == "" {
		sess.ConnType = "DEFAULT_CONN"
	}
	isRelay := 0
	if sess.IsRelay {
		isRelay = 1
	}
	res, err := s.db.Exec(`
		INSERT INTO sessions (session_key, relay_uuid, from_id, from_name, from_ip, to_id, to_ip, conn_type, is_relay, status, bytes_sent, bytes_recv, duration_sec, started_at, ended_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, sess.SessionKey, sess.RelayUUID, sess.FromID, sess.FromName, sess.FromIP, sess.ToID, sess.ToIP, sess.ConnType, isRelay, sess.Status, sess.BytesSent, sess.BytesRecv, sess.DurationSec, sess.StartedAt, sess.EndedAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishSession marks a session closed with final traffic and duration counters.
func (s *Store) FinishSession(id int64, bytesSent, bytesRecv int64) error {
	now := nowMS()
	var startedAt int64
	_ = s.db.QueryRow(`SELECT started_at FROM sessions WHERE id = ?`, id).Scan(&startedAt)
	dur := int64(0)
	if startedAt > 0 && now >= startedAt {
		dur = (now - startedAt) / 1000
	}
	_, err := s.db.Exec(`
		UPDATE sessions SET status = 'closed', bytes_sent = ?, bytes_recv = ?, duration_sec = ?, ended_at = ? WHERE id = ?
	`, bytesSent, bytesRecv, dur, now, id)
	return err
}

// UpdateSessionByConnKey updates a client-reported session (e.g. auth peer info or close).
func (s *Store) UpdateSessionByKey(sessionKey, fromID, fromName, connType, status string) error {
	now := nowMS()
	if status == "closed" {
		var id, startedAt int64
		err := s.db.QueryRow(`SELECT id, started_at FROM sessions WHERE session_key = ? ORDER BY id DESC LIMIT 1`, sessionKey).Scan(&id, &startedAt)
		if err != nil {
			return err
		}
		dur := int64(0)
		if startedAt > 0 && now >= startedAt {
			dur = (now - startedAt) / 1000
		}
		_, err = s.db.Exec(`UPDATE sessions SET status = 'closed', duration_sec = ?, ended_at = ? WHERE id = ?`, dur, now, id)
		return err
	}
	_, err := s.db.Exec(`
		UPDATE sessions SET
			from_id = CASE WHEN ? != '' THEN ? ELSE from_id END,
			from_name = CASE WHEN ? != '' THEN ? ELSE from_name END,
			conn_type = CASE WHEN ? != '' THEN ? ELSE conn_type END
		WHERE session_key = ?
	`, fromID, fromID, fromName, fromName, connType, connType, sessionKey)
	return err
}

// ListSessions returns recent sessions.
func (s *Store) ListSessions(limit int) ([]Session, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`
		SELECT id, session_key, relay_uuid, from_id, from_name, from_ip, to_id, to_ip, conn_type, is_relay, status, bytes_sent, bytes_recv, duration_sec, started_at, ended_at
		FROM sessions ORDER BY started_at DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var sess Session
		var isRelay int
		if err := rows.Scan(
			&sess.ID, &sess.SessionKey, &sess.RelayUUID, &sess.FromID, &sess.FromName, &sess.FromIP,
			&sess.ToID, &sess.ToIP, &sess.ConnType, &isRelay, &sess.Status,
			&sess.BytesSent, &sess.BytesRecv, &sess.DurationSec, &sess.StartedAt, &sess.EndedAt,
		); err != nil {
			return nil, err
		}
		sess.IsRelay = isRelay == 1
		out = append(out, sess)
	}
	return out, rows.Err()
}

// AddAuditLog inserts an audit log event (ignoring duplicate non-empty nonces).
func (s *Store) AddAuditLog(entry *AuditLog) error {
	if entry.CreatedAt == 0 {
		entry.CreatedAt = nowMS()
	}
	if entry.Severity == "" {
		entry.Severity = "info"
	}
	metaStr := "{}"
	if len(entry.Metadata) > 0 {
		metaStr = string(entry.Metadata)
	}
	_, err := s.db.Exec(`
		INSERT OR IGNORE INTO audit_logs (
			category, action, severity, actor_id, actor_name, target_id, target_name,
			source_ip, conn_id, session_id, conn_type, duration_sec, bytes_transferred,
			nonce, summary, metadata_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, entry.Category, entry.Action, entry.Severity, entry.ActorID, entry.ActorName,
		entry.TargetID, entry.TargetName, entry.SourceIP, entry.ConnID, entry.SessionID,
		entry.ConnType, entry.DurationSec, entry.BytesTransferred, entry.Nonce,
		entry.Summary, metaStr, entry.CreatedAt)
	return err
}

// QueryAuditLogs queries audit logs with filtering and pagination.
func (s *Store) QueryAuditLogs(f AuditFilter) ([]AuditLog, int, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize <= 0 || f.PageSize > 500 {
		f.PageSize = 50
	}

	var conds []string
	var args []any

	if f.Category != "" && f.Category != "all" {
		conds = append(conds, "category = ?")
		args = append(args, f.Category)
	}
	if f.Action != "" {
		conds = append(conds, "action = ?")
		args = append(args, f.Action)
	}
	if f.Severity != "" && f.Severity != "all" {
		conds = append(conds, "severity = ?")
		args = append(args, f.Severity)
	}
	if f.PeerID != "" {
		conds = append(conds, "(actor_id = ? OR target_id = ?)")
		args = append(args, f.PeerID, f.PeerID)
	}
	if f.FromMS > 0 {
		conds = append(conds, "created_at >= ?")
		args = append(args, f.FromMS)
	}
	if f.ToMS > 0 {
		conds = append(conds, "created_at <= ?")
		args = append(args, f.ToMS)
	}
	if q := strings.TrimSpace(f.Search); q != "" {
		like := "%" + q + "%"
		conds = append(conds, "(actor_id LIKE ? OR actor_name LIKE ? OR target_id LIKE ? OR target_name LIKE ? OR source_ip LIKE ? OR action LIKE ? OR summary LIKE ? OR metadata_json LIKE ?)")
		args = append(args, like, like, like, like, like, like, like, like)
	}

	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	var total int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM audit_logs "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	offset := (f.Page - 1) * f.PageSize
	queryArgs := append(append([]any{}, args...), f.PageSize, offset)
	rows, err := s.db.Query(`
		SELECT id, category, action, severity, actor_id, actor_name, target_id, target_name,
		       source_ip, conn_id, session_id, conn_type, duration_sec, bytes_transferred,
		       nonce, summary, metadata_json, created_at
		FROM audit_logs `+where+`
		ORDER BY created_at DESC, id DESC
		LIMIT ? OFFSET ?
	`, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var logs []AuditLog
	for rows.Next() {
		var l AuditLog
		var metaStr string
		if err := rows.Scan(
			&l.ID, &l.Category, &l.Action, &l.Severity, &l.ActorID, &l.ActorName,
			&l.TargetID, &l.TargetName, &l.SourceIP, &l.ConnID, &l.SessionID,
			&l.ConnType, &l.DurationSec, &l.BytesTransferred, &l.Nonce,
			&l.Summary, &metaStr, &l.CreatedAt,
		); err != nil {
			return nil, 0, err
		}
		l.Metadata = json.RawMessage(metaStr)
		logs = append(logs, l)
	}
	return logs, total, rows.Err()
}

// GetSetting reads a persisted server setting.
func (s *Store) GetSetting(key string) (string, bool) {
	var val string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&val)
	if err != nil {
		return "", false
	}
	return val, true
}

// SetSetting writes a persisted server setting.
func (s *Store) SetSetting(key, val string) error {
	_, err := s.db.Exec(`
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, val)
	return err
}

// GetAnalytics computes comprehensive client, connection, traffic, and audit analytics.
func (s *Store) GetAnalytics(ctx context.Context, rangeKey string) (*AnalyticsData, error) {
	now := time.Now()
	var since time.Time
	var bucketDur time.Duration
	var bucketFmt string
	var bucketCount int

	switch rangeKey {
	case "7d":
		since = now.Add(-7 * 24 * time.Hour).Truncate(24 * time.Hour)
		bucketDur = 24 * time.Hour
		bucketFmt = "Jan 02"
		bucketCount = 8
	case "30d":
		since = now.Add(-30 * 24 * time.Hour).Truncate(24 * time.Hour)
		bucketDur = 24 * time.Hour
		bucketFmt = "Jan 02"
		bucketCount = 31
	default:
		rangeKey = "24h"
		since = now.Add(-23 * time.Hour).Truncate(time.Hour)
		bucketDur = time.Hour
		bucketFmt = "15:00"
		bucketCount = 24
	}
	sinceMS := since.UnixMilli()

	peers, err := s.ListPeers()
	if err != nil {
		return nil, err
	}

	data := &AnalyticsData{
		Range:             rangeKey,
		TotalPeers:        len(peers),
		ConnectionsSeries: make([]TimeSeriesPoint, 0, bucketCount),
		TrafficSeries:     make([]TrafficSeriesPoint, 0, bucketCount),
		OSBreakdown:       []LabelCount{},
		VersionBreakdown:  []LabelCount{},
		NATBreakdown:      []LabelCount{},
		ConnTypeBreakdown: []LabelCount{},
		TopDevices:        []TopDeviceStat{},
	}

	osCounts := map[string]int{}
	verCounts := map[string]int{}
	natCounts := map[string]int{}
	peerMap := map[string]Peer{}

	for _, p := range peers {
		peerMap[p.ID] = p
		if p.Online {
			data.OnlinePeers++
		} else {
			data.OfflinePeers++
		}
		osLabel := normalizeOS(p.OS)
		osCounts[osLabel]++
		verLabel := p.Version
		if verLabel == "" {
			verLabel = "Unknown"
		}
		verCounts[verLabel]++
		natLabel := p.NATType
		if natLabel == "" {
			natLabel = "UNKNOWN"
		}
		natCounts[natLabel]++
	}

	for k, v := range osCounts {
		data.OSBreakdown = append(data.OSBreakdown, LabelCount{Label: k, Count: v})
	}
	for k, v := range verCounts {
		data.VersionBreakdown = append(data.VersionBreakdown, LabelCount{Label: k, Count: v})
	}
	for k, v := range natCounts {
		data.NATBreakdown = append(data.NATBreakdown, LabelCount{Label: k, Count: v})
	}

	// Pre-populate time buckets
	connBuckets := make(map[string]*TimeSeriesPoint, bucketCount)
	trafficBuckets := make(map[string]*TrafficSeriesPoint, bucketCount)
	bucketOrder := make([]string, 0, bucketCount)
	for i := 0; i < bucketCount; i++ {
		ts := since.Add(time.Duration(i) * bucketDur)
		label := ts.Format(bucketFmt)
		if _, exists := connBuckets[label]; !exists {
			bucketOrder = append(bucketOrder, label)
			connBuckets[label] = &TimeSeriesPoint{Bucket: label}
			trafficBuckets[label] = &TrafficSeriesPoint{Bucket: label}
		}
	}

	// Query sessions in window
	rows, err := s.db.QueryContext(ctx, `
		SELECT from_id, to_id, conn_type, is_relay, status, bytes_sent, bytes_recv, duration_sec, started_at
		FROM sessions WHERE started_at >= ?
	`, sinceMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	connTypes := map[string]int{}
	deviceStats := map[string]*TopDeviceStat{}
	var totalDur int64
	var closedCount int

	for rows.Next() {
		var fromID, toID, connType, status string
		var isRelay int
		var bSent, bRecv, dur, startedAt int64
		if err := rows.Scan(&fromID, &toID, &connType, &isRelay, &status, &bSent, &bRecv, &dur, &startedAt); err != nil {
			return nil, err
		}
		data.TotalConnections++
		if status == "active" {
			data.ActiveSessions++
		}
		if isRelay == 1 {
			data.RelayConnections++
		} else {
			data.DirectConnections++
		}
		totalBytes := bSent + bRecv
		data.TotalBytesRelayed += totalBytes
		if status == "closed" && dur >= 0 {
			totalDur += dur
			closedCount++
		}
		if connType == "" {
			connType = "DEFAULT_CONN"
		}
		connTypes[connType]++

		bLabel := time.UnixMilli(startedAt).Truncate(bucketDur).Format(bucketFmt)
		if pt, ok := connBuckets[bLabel]; ok {
			pt.Total++
			if isRelay == 1 {
				pt.Relay++
			} else {
				pt.Direct++
			}
		}
		if tp, ok := trafficBuckets[bLabel]; ok {
			tp.Bytes += totalBytes
		}

		targetKey := toID
		if targetKey == "" {
			targetKey = fromID
		}
		if targetKey != "" {
			st, ok := deviceStats[targetKey]
			if !ok {
				p := peerMap[targetKey]
				label := p.Alias
				if label == "" {
					label = p.Hostname
				}
				if label == "" {
					label = targetKey
				}
				st = &TopDeviceStat{
					PeerID: targetKey,
					Label:  label,
					OS:     normalizeOS(p.OS),
					Online: p.Online,
				}
				deviceStats[targetKey] = st
			}
			st.Connections++
			st.Bytes += totalBytes
		}
	}

	if closedCount > 0 {
		data.AvgDurationSec = float64(totalDur) / float64(closedCount)
	}

	for _, b := range bucketOrder {
		data.ConnectionsSeries = append(data.ConnectionsSeries, *connBuckets[b])
		data.TrafficSeries = append(data.TrafficSeries, *trafficBuckets[b])
	}
	for k, v := range connTypes {
		data.ConnTypeBreakdown = append(data.ConnTypeBreakdown, LabelCount{Label: k, Count: v})
	}
	for _, st := range deviceStats {
		data.TopDevices = append(data.TopDevices, *st)
	}

	// Sort top devices descending by connections then bytes
	for i := 0; i < len(data.TopDevices); i++ {
		for j := i + 1; j < len(data.TopDevices); j++ {
			if data.TopDevices[j].Connections > data.TopDevices[i].Connections ||
				(data.TopDevices[j].Connections == data.TopDevices[i].Connections && data.TopDevices[j].Bytes > data.TopDevices[i].Bytes) {
				data.TopDevices[i], data.TopDevices[j] = data.TopDevices[j], data.TopDevices[i]
			}
		}
	}
	if len(data.TopDevices) > 10 {
		data.TopDevices = data.TopDevices[:10]
	}

	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_logs WHERE created_at >= ?`, sinceMS).Scan(&data.TotalAuditEvents)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_logs WHERE created_at >= ? AND severity IN ('warn', 'error')`, sinceMS).Scan(&data.SecurityWarnings)

	return data, nil
}

func normalizeOS(raw string) string {
	lower := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case lower == "":
		return "Unknown"
	case strings.Contains(lower, "win"):
		return "Windows"
	case strings.Contains(lower, "mac") || strings.Contains(lower, "darwin"):
		return "macOS"
	case strings.Contains(lower, "android"):
		return "Android"
	case strings.Contains(lower, "ios"):
		return "iOS"
	case strings.Contains(lower, "linux") || strings.Contains(lower, "ubuntu") || strings.Contains(lower, "debian") || strings.Contains(lower, "fedora") || strings.Contains(lower, "arch"):
		return "Linux"
	default:
		return raw
	}
}
