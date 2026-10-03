package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPGGroupAndProxyDeleteRollback(t *testing.T) {
	s := accountPGTestStore(t)
	ctx := context.Background()
	g := NewPGGroupStore(s.DB)
	group, err := g.Create("transaction", "test")
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewPGProxyStore(s.DB)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := p.Create(ProxyEntry{Name: "proxy", Type: "http", Host: "127.0.0.1", Port: 1080, Password: "hidden-pass"})
	if err != nil {
		t.Fatal(err)
	}
	a := pgImportForTest(t, s, "linked", "synthetic-token")
	a, err = s.UpdateAccountVersion(a.ID, a.Version, func(v *Account) error { v.Groups = []string{group.ID}; v.Proxies = []string{proxy.ID}; return nil })
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB.Pool.Exec(ctx, `CREATE FUNCTION reject_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected failure'; END $$; CREATE TRIGGER reject_audit BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_audit()`)
	if err != nil {
		t.Fatal(err)
	}
	if err = g.Delete(group.ID); err == nil {
		t.Fatal("group deletion falsely succeeded")
	}
	if err = p.Delete(proxy.ID); err == nil {
		t.Fatal("proxy deletion falsely succeeded")
	}
	after, err := s.GetMetadata(a.ID)
	if err != nil || len(after.Groups) != 1 || len(after.Proxies) != 1 || after.Version != a.Version {
		t.Fatalf("transaction partially committed: %+v %v", after, err)
	}
	if _, err = s.DB.Pool.Exec(ctx, "DROP TRIGGER reject_audit ON audit_logs"); err != nil {
		t.Fatal(err)
	}
	if err = g.Delete(group.ID); err != nil {
		t.Fatal(err)
	}
	if err = p.Delete(proxy.ID); err != nil {
		t.Fatal(err)
	}
	after, err = s.GetMetadata(a.ID)
	if err != nil || len(after.Groups) != 0 || len(after.Proxies) != 0 {
		t.Fatalf("associations not detached: %v", err)
	}
}

func TestPGProxyMetadataAndVersion(t *testing.T) {
	s := accountPGTestStore(t)
	p, err := NewPGProxyStore(s.DB)
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.Create(ProxyEntry{Name: "secret", Type: "http", Host: "localhost", Port: 8080, Password: "never-list-me"})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := p.ListMetadata()
	if err != nil || len(entries) != 1 || entries[0].Password != "" || !entries[0].HasPassword {
		t.Fatalf("metadata leak: %v", err)
	}
	updated, err := p.UpdateVersion(v.ID, v.Version, func(v *ProxyEntry) error { v.Name = "new"; return nil })
	if err != nil || updated.Version != v.Version+1 {
		t.Fatal("version did not advance", err)
	}
	if _, err = p.UpdateVersion(v.ID, v.Version, func(v *ProxyEntry) error { v.Name = "stale"; return nil }); !errors.Is(err, errVersionConflict) {
		t.Fatal("stale version accepted", err)
	}
}

func TestPGAdminRotationFailureAndLostResponseReplay(t *testing.T) {
	s := accountPGTestStore(t)
	ctx := context.Background()
	old := strings.Repeat("x", 40)
	if _, err := s.DB.initializeAdminKey(ctx, old); err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Store: s, AdminKey: old}
	if _, err := s.DB.Pool.Exec(ctx, `CREATE FUNCTION reject_rotation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected'; END $$; CREATE TRIGGER reject_rotation BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_rotation()`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.rotateAdminKey(ctx, "failure"); err == nil {
		t.Fatal("failed persistence accepted")
	}
	key, err := p.currentAdminKey(ctx)
	if err != nil || key != old || p.adminKey() != old {
		t.Fatal("old key lost", err)
	}
	if _, err = s.DB.Pool.Exec(ctx, "DROP TRIGGER reject_rotation ON audit_logs"); err != nil {
		t.Fatal(err)
	}
	fresh, err := p.rotateAdminKey(ctx, "retry-me")
	if err != nil || fresh == old {
		t.Fatal("rotation failed", err)
	}
	same, err := p.rotateAdminKey(ctx, "retry-me")
	if err != nil || same != fresh {
		t.Fatal("rotation not idempotent", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/settings/rotate-key", strings.NewReader(`{"request_id":"retry-me"}`))
	r.Header.Set("Authorization", "Bearer "+old)
	w := httptest.NewRecorder()
	if !p.authenticateAdmin(w, r) {
		t.Fatalf("lost response cannot be recovered: %d", w.Code)
	}
	r = httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	r.Header.Set("Authorization", "Bearer "+old)
	if p.authenticateAdmin(httptest.NewRecorder(), r) {
		t.Fatal("old key still grants general access")
	}
	if _, err = p.rotateAdminKey(ctx, "second-rotation"); err != nil {
		t.Fatal(err)
	}
	if _, err = p.rotateAdminKey(ctx, "retry-me"); !errors.Is(err, errConflict) {
		t.Fatal("superseded rotation returned invalid old key", err)
	}

}

func TestPGRedisSessionsFailClosed(t *testing.T) {
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("TEST_REDIS_URL required")
	}
	s := accountPGTestStore(t)
	d := s.DB
	if err := d.ConfigureRedis(redisURL); err != nil {
		t.Fatal(err)
	}
	d.Namespace += newID("test_") + ":"
	d.CookieSecure = false
	d.SessionTTL = time.Minute
	key := strings.Repeat("session-key", 5)
	if _, err := d.initializeAdminKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Store: s, AdminKey: key}
	raw, _ := json.Marshal(map[string]string{"admin_key": key})
	r := httptest.NewRequest(http.MethodPost, "/api/session", bytes.NewReader(raw))
	w := httptest.NewRecorder()
	p.handleSessionLogin(w, r)
	if w.Code != 200 {
		t.Fatalf("login failed: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("cookie flags invalid")
	}
	r = httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	r.AddCookie(cookies[0])
	if !p.authenticateAdmin(httptest.NewRecorder(), r) {
		t.Fatal("session rejected")
	}
	d.Redis.Close()
	w = httptest.NewRecorder()
	if p.authenticateAdmin(w, r) || w.Code != 503 {
		t.Fatal("session failure did not fail closed")
	}
}

func TestLegacyDryRunRejectsBrokenAndDanglingData(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(`{"accounts":[{"name":"one","access_token":"synthetic","groups":["missing"]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLegacyBundle(dir); err == nil {
		t.Fatal("dangling relation accepted")
	}
	file := filepath.Join(dir, "broken.jsonl")
	if err := os.WriteFile(file, []byte("{\n\"tokens\": broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLegacyBundle(file); err == nil {
		t.Fatal("broken JSON accepted")
	}
}

func TestPGLegacyImportExportRoundTrip(t *testing.T) {
	s := accountPGTestStore(t)
	source := t.TempDir()
	account := Account{Name: "legacy", AccessToken: "legacy-synthetic-access", RefreshToken: "legacy-synthetic-refresh", Enabled: true, CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Hour).Unix()}
	raw, _ := json.Marshal(accountState{Active: "legacy", Accounts: []Account{account}})
	if err := os.WriteFile(filepath.Join(source, "accounts.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	b, err := readLegacyBundle(source)
	if err != nil {
		t.Fatal(err)
	}
	if err = importLegacyBundle(context.Background(), s.DB, b); err != nil {
		t.Fatal(err)
	}
	if err = importLegacyBundle(context.Background(), s.DB, b); err != nil {
		t.Fatal("reimport not idempotent", err)
	}
	current, err := s.Get("legacy")
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewPGMetrics(s.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = m.ImportRecords(context.Background(), []RequestRecord{{EventID: "export-rename", Time: time.Now().Unix(), AccountID: current.ID, Account: current.Name, Status: "ok", InputTokens: 17}}); err != nil {
		t.Fatal(err)
	}
	previousAccessToken := current.AccessToken
	current, err = s.UpdateAccountVersion(current.ID, current.Version, func(a *Account) error { a.Name = "renamed"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if current.AccessToken != "" || current.RefreshToken != "" {
		t.Fatal("management update returned credentials")
	}

	if err = s.UpdateTokens(current.ID, previousAccessToken, Account{AccessToken: "rotated-access", RefreshToken: "rotated-refresh"}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "export")
	if err = exportLegacyBundle(context.Background(), s.DB, target); err != nil {
		t.Fatal(err)
	}
	restored := Store{Path: filepath.Join(target, "accounts.json")}
	v, err := restored.Get("renamed")
	if err != nil || v.AccessToken != "rotated-access" || v.RefreshToken != "rotated-refresh" {
		t.Fatal("export lost latest credentials", err)
	}
	restoredMetrics, err := NewMetrics(target)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredMetrics.Close()
	if usage := restoredMetrics.AccountWindow("renamed", time.Now().Add(-time.Hour).Unix()); usage.Input != 17 {
		t.Fatalf("rename lost legacy usage: %+v", usage)
	}
	if last, ok := restoredMetrics.LastFor("renamed"); !ok || last.InputTokens != 17 {
		t.Fatal("rename lost recent state")
	}

}

func TestSettingsVersionConflict(t *testing.T) {
	s := NewSettingsStore(filepath.Join(t.TempDir(), "settings.json"))
	initial, err := s.Get()
	if err != nil {
		t.Fatal(err)
	}
	updated := initial
	updated.AdminAPI = false
	if err = s.Set(updated); err != nil {
		t.Fatal(err)
	}
	if err = s.Set(initial); !errors.Is(err, errVersionConflict) {
		t.Fatal("stale settings accepted", err)
	}
}
