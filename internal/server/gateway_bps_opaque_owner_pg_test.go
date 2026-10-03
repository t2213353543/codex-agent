package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPGBPSOpaqueOwnerRecoveryAcrossProxies(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	if p.Store.DB.Redis == nil {
		t.Skip("isolated Redis required with PostgreSQL")
	}
	const opaque = `{"type":"reasoning","id":"rs_pg_owner","encrypted_content":"synthetic-pg-owner-cipher"}`
	var mu sync.Mutex
	var owners []string
	var inbound [][]byte
	fixture := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		owners = append(owners, r.Header.Get("Authorization"))
		inbound = append(inbound, raw)
		n := len(owners)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if r.Header.Get("Authorization") != "Bearer pg-original-token" {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":{"code":"invalid_encrypted_content"}}`)
			return
		}
		output := "[]"
		if n == 1 {
			output = "[" + opaque + "]"
		}
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "opaque_pg_"+string(rune('0'+n)), output))
	})
	p.BaseURL, p.Client, p.BPS = fixture.BaseURL, fixture.Client, fixture.BPS
	settings := defaultSettings()
	settings.AutomaticTokenRefreshEnabled = false
	settings.Upstream401RecoveryEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	importAccount := func(name, token string, priority int) Account {
		raw, _ := json.Marshal(map[string]any{"access_token": token, "expires_at": time.Now().Add(time.Hour).Unix()})
		if err := p.Store.ImportWith(name, raw, AccountDefaults{Concurrency: 2, Priority: priority, Weight: 1}); err != nil {
			t.Fatal(err)
		}
		a, err := p.Store.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	original := importAccount("opaque-original", "pg-original-token", 100)
	other := importAccount("opaque-other", "pg-other-token", 1)
	send := func(proxy *Proxy, session, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+webTestKey)
		req.Header.Set("Session-Id", session)
		out := httptest.NewRecorder()
		proxy.gatewayBPS(out, req)
		return out
	}
	first := send(p, "pg-original-thread", `{"model":"gpt-6-astra","stream":false,"input":[{"role":"user","content":"create opaque history"}]}`)
	if first.Code != 200 || !strings.Contains(first.Body.String(), "synthetic-pg-owner-cipher") {
		t.Fatalf("first actual producer failed: %d %s", first.Code, first.Body.String())
	}
	if _, err := p.Store.UpdateAccountVersion(other.ID, other.Version, func(a *Account) error { a.Priority = 200; return nil }); err != nil {
		t.Fatal(err)
	}
	freshMetrics := newTestPGMetrics(t, p.Store.DB)
	fresh := &Proxy{Store: p.Store, Groups: p.Groups, Keys: p.Keys, Proxies: p.Proxies, Settings: NewPGSettingsStore(p.Store.DB), Metrics: freshMetrics, AdminKey: webTestKey, Client: p.Client, BaseURL: p.BaseURL, BPS: &bpsRuntime{ToolsVersion: p.BPS.ToolsVersion}}
	body := `{"model":"gpt-6-astra","stream":false,"input":[` + opaque + `,{"role":"user","content":"continue original encrypted state"}]}`
	second := send(fresh, "pg-new-thread", body)
	if second.Code != 200 {
		t.Fatalf("fresh proxy routed known ciphertext to different account: %d %s", second.Code, second.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(owners) != 2 || owners[0] != "Bearer pg-original-token" || owners[1] != owners[0] || !bytes.Contains(inbound[1], []byte(opaque)) {
		t.Fatal("two-proxy original account/cipher dispatch proof missing")
	}
	if err := p.Metrics.Save(); err != nil {
		t.Fatal(err)
	}
	if err := freshMetrics.Save(); err != nil {
		t.Fatal(err)
	}
	reader := newTestPGMetrics(t, p.Store.DB)
	page, err := reader.managedLogs(t.Context(), managedQuery(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("expected two logical durable requests, got %d", len(page.Items))
	}
	for _, row := range page.Items {
		if row.AccountID != original.ID || row.Status != "ok" || row.HTTPCode != 200 || len(row.Attempts) != 1 || row.Attempts[0].ObservedHTTPStatusCode == nil || *row.Attempts[0].ObservedHTTPStatusCode != 200 {
			t.Fatal("PG account identity or actual attempt changed")
		}
	}
	if p.inFlight(gatewayAdmissionKey(original)) != 0 || fresh.inFlight(gatewayAdmissionKey(original)) != 0 {
		t.Fatal("shared admission slot leaked")
	}
	t.Log("two complete gateway requests across fresh proxies, actual PG accounts/Redis ownership/shared admission and durable metrics retained original account")
}
