package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KriszW/godesk-plane/internal/config"
	"github.com/KriszW/godesk-plane/internal/crypto"
	"github.com/KriszW/godesk-plane/internal/relay"
	"github.com/KriszW/godesk-plane/internal/rendezvous"
	"github.com/KriszW/godesk-plane/internal/store"
)

func setupTestAPI(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	kp, err := crypto.LoadOrGenerate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.EnsureAdminUser("admin", "secret123", "admin@test.local"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Load()
	cfg.PublicHost = "desk.example.com"
	rel := relay.NewServer(relay.Config{Key: kp.PublicB64}, st)
	rdv := rendezvous.NewServer(rendezvous.Config{RequireKey: true}, kp, st, rel)
	srv := NewServer(cfg, kp, st, rdv, rel, nil)
	ts := httptest.NewServer(srv.routes())
	t.Cleanup(ts.Close)
	return srv, ts
}

func doJSON(t *testing.T, method, url, token string, body any) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestRustDeskClientAndAdminAPIs(t *testing.T) {
	_, ts := setupTestAPI(t)

	// 1. Client posts /api/sysinfo -> expects "SYSINFO_UPDATED"
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/sysinfo", "", map[string]any{
		"id":       "555666777",
		"uuid":     "dGVzdC11dWlk",
		"hostname": "dev-workstation",
		"username": "alice",
		"os":       "Ubuntu 24.04 Linux",
		"cpu":      "AMD Ryzen 9",
		"memory":   "32GB",
		"version":  "1.5.3",
	})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "SYSINFO_UPDATED" {
		t.Fatalf("sysinfo failed: status=%d body=%q", resp.StatusCode, string(body))
	}

	// 2. Client posts /api/heartbeat
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/heartbeat", "", map[string]any{
		"id":  "555666777",
		"ver": 1005003,
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat failed: %d", resp.StatusCode)
	}

	// 3. Client posts /api/audit/conn (new, auth, close) + /api/audit/file
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/audit/conn", "", map[string]any{
		"id": "555666777", "conn_id": 42, "action": "new", "ip": "10.0.0.5", "nonce": "n-1",
	})
	resp.Body.Close()
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/audit/conn", "", map[string]any{
		"id": "555666777", "conn_id": 42, "peer": []string{"111222333", "bob"}, "type": 0, "nonce": "n-2",
	})
	resp.Body.Close()
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/audit/conn", "", map[string]any{
		"id": "555666777", "conn_id": 42, "action": "close", "nonce": "n-3",
	})
	resp.Body.Close()
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/audit/file", "", map[string]any{
		"id": "555666777", "peer_id": "111222333", "conn_id": 42, "type": 0, "path": "/tmp/report.pdf", "nonce": "n-4",
	})
	resp.Body.Close()

	// 4. RustDesk client login (/api/login) and Address Book pull (/api/ab/peers)
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/login", "", map[string]any{
		"username": "admin", "password": "secret123", "id": "111222333",
	})
	var rdLogin struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&rdLogin)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || rdLogin.AccessToken == "" {
		t.Fatalf("rustdesk login failed: %d", resp.StatusCode)
	}

	resp = doJSON(t, http.MethodGet, ts.URL+"/api/ab/peers", rdLogin.AccessToken, nil)
	var abResp struct {
		Total int `json:"total"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&abResp)
	resp.Body.Close()
	if abResp.Total != 1 {
		t.Fatalf("expected 1 peer in /api/ab/peers, got %d", abResp.Total)
	}

	// 5. Admin Console login (/admin/api/login)
	resp = doJSON(t, http.MethodPost, ts.URL+"/admin/api/login", "", map[string]any{
		"username": "admin", "password": "secret123",
	})
	var admLogin struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&admLogin)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || admLogin.Token == "" {
		t.Fatalf("admin login failed: %d", resp.StatusCode)
	}

	// 6. Update peer alias & tags via Admin API
	resp = doJSON(t, http.MethodPut, ts.URL+"/admin/api/peers/555666777", admLogin.Token, map[string]any{
		"alias":      "Alice Linux Box",
		"note":       "Primary engineering workstation",
		"group_name": "Engineering",
		"tags":       []string{"linux", "prod"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update peer failed: %d", resp.StatusCode)
	}

	// 7. Query Analytics
	resp = doJSON(t, http.MethodGet, ts.URL+"/admin/api/analytics?range=24h", admLogin.Token, nil)
	var anResp struct {
		Analytics store.AnalyticsData `json:"analytics"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&anResp)
	resp.Body.Close()
	if anResp.Analytics.TotalPeers != 1 || anResp.Analytics.OnlinePeers != 1 || anResp.Analytics.TotalConnections != 1 {
		t.Fatalf("unexpected analytics: %+v", anResp.Analytics)
	}

	// 8. Query Audit logs + CSV Export
	resp = doJSON(t, http.MethodGet, ts.URL+"/admin/api/audit?page=1&page_size=20", admLogin.Token, nil)
	var audResp struct {
		Total int              `json:"total"`
		Logs  []store.AuditLog `json:"logs"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&audResp)
	resp.Body.Close()
	if audResp.Total < 5 {
		t.Fatalf("expected >= 5 audit logs, got %d", audResp.Total)
	}

	resp = doJSON(t, http.MethodGet, ts.URL+"/admin/api/audit/export?format=csv", admLogin.Token, nil)
	csvBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(csvBytes), "555666777") {
		t.Fatalf("CSV export failed: status=%d len=%d", resp.StatusCode, len(csvBytes))
	}

	// 9. Settings & Config String
	resp = doJSON(t, http.MethodGet, ts.URL+"/admin/api/settings", admLogin.Token, nil)
	var setResp struct {
		PublicKey    string `json:"public_key"`
		ConfigString string `json:"config_string"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&setResp)
	resp.Body.Close()
	if setResp.PublicKey == "" || setResp.ConfigString == "" {
		t.Fatalf("expected non-empty key and config_string: %+v", setResp)
	}
}
