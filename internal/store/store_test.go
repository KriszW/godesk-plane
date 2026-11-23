package store

import (
	"context"
	"testing"
	"time"
)

func TestStorePeersTagsUsersSessionsAnalyticsAudit(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// Users & Auth
	if err := st.EnsureAdminUser("admin", "pass1234", "admin@local"); err != nil {
		t.Fatalf("EnsureAdminUser: %v", err)
	}
	u, err := st.AuthenticateUser("admin", "pass1234")
	if err != nil || !u.IsAdmin {
		t.Fatalf("AuthenticateUser: u=%+v err=%v", u, err)
	}
	tok, err := st.CreateToken(u, "", "", time.Hour)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if _, err := st.ValidateToken(tok); err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}

	// Peers & Sysinfo & Tags
	if err := st.UpsertPeerPK("100200300", []byte("uuid-1"), []byte("pk-1"), "192.168.1.10", "192.168.1.10:50000"); err != nil {
		t.Fatalf("UpsertPeerPK: %v", err)
	}
	if err := st.UpsertPeerSysinfo("100200300", "192.168.1.10", "box-1", "alice", "Ubuntu 24.04 Linux", "8 cores", "16GB", "1.5.3", []byte("uuid-1"), "", "", "Infra", []string{"prod"}); err != nil {
		t.Fatalf("UpsertPeerSysinfo: %v", err)
	}
	if err := st.UpsertTag("prod", 0xFF2563EB); err != nil {
		t.Fatalf("UpsertTag: %v", err)
	}
	if err := st.UpdatePeerMeta("100200300", "Prod Gateway", "Main node", "Infra", []string{"prod"}); err != nil {
		t.Fatalf("UpdatePeerMeta: %v", err)
	}

	p, err := st.GetPeer("100200300")
	if err != nil {
		t.Fatalf("GetPeer: %v", err)
	}
	if !p.Online || p.Alias != "Prod Gateway" || p.Version != "1.5.3" {
		t.Fatalf("unexpected peer: %+v", p)
	}

	// Heartbeat must not overwrite sysinfo version
	needSys, err := st.TouchPeerHeartbeat("100200300", "192.168.1.10", "1.5.30", nil)
	if err != nil || needSys {
		t.Fatalf("TouchPeerHeartbeat: needSys=%v err=%v", needSys, err)
	}
	p2, _ := st.GetPeer("100200300")
	if p2.Version != "1.5.3" {
		t.Fatalf("expected version 1.5.3 preserved, got %q", p2.Version)
	}

	// Sessions & Analytics
	sid, err := st.StartSession(&Session{
		RelayUUID: "r-1",
		FromIP:    "10.0.0.1",
		ToID:      "100200300",
		ToIP:      "192.168.1.10",
		ConnType:  "DEFAULT_CONN",
		IsRelay:   true,
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := st.FinishSession(sid, 4096, 8192); err != nil {
		t.Fatalf("FinishSession: %v", err)
	}

	// Audit nonce deduplication
	for i := 0; i < 2; i++ {
		if err := st.AddAuditLog(&AuditLog{
			Category: "connection",
			Action:   "session_started",
			TargetID: "100200300",
			Nonce:    "unique-nonce-1",
			Summary:  "started",
		}); err != nil {
			t.Fatalf("AddAuditLog: %v", err)
		}
	}
	logs, total, err := st.QueryAuditLogs(AuditFilter{PeerID: "100200300", Page: 1, PageSize: 20})
	if err != nil || total != 1 || len(logs) != 1 {
		t.Fatalf("expected 1 deduplicated audit log, got total=%d len=%d err=%v", total, len(logs), err)
	}

	// Analytics
	an, err := st.GetAnalytics(context.Background(), "24h")
	if err != nil {
		t.Fatalf("GetAnalytics: %v", err)
	}
	if an.TotalPeers != 1 || an.OnlinePeers != 1 || an.RelayConnections != 1 || an.TotalBytesRelayed != 12288 {
		t.Fatalf("unexpected analytics: %+v", an)
	}
}
