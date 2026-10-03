package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func adminPGTestProxy(t *testing.T) (*Proxy, http.Handler) {
	t.Helper()
	store := accountPGTestStore(t)
	configureAdminPGAdmissionRedis(t, store.DB)
	groups := NewPGGroupStore(store.DB)
	proxies, err := NewPGProxyStore(store.DB)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := NewPGKeyStore(store.DB)
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := NewPGMetrics(store.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.initializeAdminKey(context.Background(), webTestKey); err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Store: store, Groups: groups, Proxies: proxies, Keys: keys, Settings: NewPGSettingsStore(store.DB), Metrics: metrics, AdminKey: webTestKey, Client: newHTTPClient(), startedAt: time.Now()}
	return p, p.adminRoutes()
}

func TestPGAdminGroupDeletionRollsBackOnAuditFailure(t *testing.T) {
	p, handler := adminPGTestProxy(t)
	group, err := p.Groups.Create("bound", "description")
	if err != nil {
		t.Fatal(err)
	}
	a := pgImportForTest(t, p.Store, "bound-account", "bound-token")
	if _, err := p.Store.UpdateAccountVersion(a.ID, a.Version, func(a *Account) error { a.Groups = []string{group.ID}; return nil }); err != nil {
		t.Fatal(err)
	}
	key, err := p.Keys.Create("bound-key", []string{group.ID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Store.DB.Pool.Exec(context.Background(), `CREATE FUNCTION reject_group_delete_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='group.delete' THEN RAISE EXCEPTION 'injected audit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_group_delete BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_group_delete_audit()`)
	if err != nil {
		t.Fatal(err)
	}
	response := webTestRequest(handler, http.MethodDelete, "/api/groups/"+group.ID, "", "Bearer "+webTestKey)
	if response.Code != 500 {
		t.Fatalf("injected transaction failure returned %d: %s", response.Code, response.Body.String())
	}
	if _, err := p.Groups.Get(group.ID); err != nil {
		t.Fatal("failed transaction removed group")
	}
	current, err := p.Store.GetMetadata(a.ID)
	if err != nil || len(current.Groups) != 1 {
		t.Fatal("failed transaction removed account relation")
	}
	storedKeys, err := p.Keys.List()
	if err != nil || len(storedKeys) != 1 || len(storedKeys[0].Groups) != 1 {
		t.Fatal("failed transaction removed key relation")
	}
	if _, err := p.Store.DB.Pool.Exec(context.Background(), `DROP TRIGGER reject_group_delete ON audit_logs`); err != nil {
		t.Fatal(err)
	}
	response = webTestRequest(handler, http.MethodDelete, "/api/groups/"+group.ID, "", "Bearer "+webTestKey)
	if response.Code != 200 {
		t.Fatalf("group deletion failed: %s", response.Body.String())
	}
	current, err = p.Store.GetMetadata(a.ID)
	if err != nil || len(current.Groups) != 0 {
		t.Fatal("committed deletion did not detach account")
	}
	cached, ok := p.Keys.Lookup(key.Key)
	if !ok || len(cached.Groups) != 0 {
		t.Fatal("committed deletion did not refresh key cache")
	}
}

func TestPGAdminStableAccountLogsSurviveRenameAndRestart(t *testing.T) {
	p, handler := adminPGTestProxy(t)
	a := pgImportForTest(t, p.Store, "before-rename", "rename-token")
	if a.CredentialVersion <= 0 {
		t.Fatal("current credential generation unavailable")
	}
	now := time.Now().Unix()
	// This observation belongs to the current credential snapshot. Keep the
	// name-only import to exercise stable-ID audit resolution across a rename.
	_, _, err := p.Metrics.ImportRecords(context.Background(), []RequestRecord{{EventID: "account-old-name", Time: now, Account: a.Name, AccountCredentialVersion: a.CredentialVersion, Status: "error", Error: "upstream unavailable", InputTokens: 100, CachedTokens: 80}})
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := p.Store.UpdateAccountVersion(a.ID, a.Version, func(a *Account) error { a.Name = "after-rename"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if renamed.CredentialVersion != a.CredentialVersion {
		t.Fatal("rename changed the credential observation generation")
	}
	p.Metrics, err = NewPGMetrics(p.Store.DB)
	if err != nil {
		t.Fatal(err)
	}
	response := webTestRequest(handler, http.MethodGet, "/api/accounts/"+a.ID+"/requests", "", "Bearer "+webTestKey)
	var logs LogPage
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &logs) != nil || len(logs.Items) != 1 {
		t.Fatalf("stable account logs lost after rename: %d %s", response.Code, response.Body.String())
	}
	response = webTestRequest(handler, http.MethodGet, "/api/accounts/"+a.ID, "", "Bearer "+webTestKey)
	var detail struct {
		Account accountSummary `json:"account"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &detail) != nil || detail.Account.Status != "error" {
		t.Fatalf("restart lost persistent recent failure: %s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "rename-token") {
		t.Fatal("detail leaked credentials")
	}
	response = webTestRequest(handler, http.MethodGet, "/api/accounts?status=error", "", "Bearer "+webTestKey)
	var list struct {
		Accounts []accountSummary `json:"accounts"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &list) != nil || len(list.Accounts) != 1 {
		t.Fatalf("status filter disagrees with account summary: %s", response.Body.String())
	}
	t.Run("overview_cache_TTL_invalidation_and_Redis_failure", func(t *testing.T) {
		redisURL := os.Getenv("TEST_REDIS_URL")
		if redisURL == "" {
			t.Skip("TEST_REDIS_URL is not configured")
		}
		d := p.Store.DB
		if err := d.ConfigureRedis(redisURL); err != nil {
			t.Fatal(err)
		}
		d.Namespace += "admin-cache-test:" + newID("") + ":"
		defer d.Redis.Del(context.Background(), d.Namespace+"overview")
		response := webTestRequest(handler, http.MethodGet, "/api/overview", "", "Bearer "+webTestKey)
		if response.Code != 200 {
			t.Fatalf("overview failed: %s", response.Body.String())
		}
		ttl, err := d.Redis.TTL(context.Background(), d.Namespace+"overview").Result()
		if err != nil || ttl <= 0 || ttl > 10*time.Second {
			t.Fatalf("overview cache TTL invalid: %v %v", ttl, err)
		}
		cached, err := d.Redis.Get(context.Background(), d.Namespace+"overview").Bytes()
		if err != nil {
			t.Fatal(err)
		}
		response = webTestRequest(handler, http.MethodGet, "/api/overview", "", "Bearer "+webTestKey)
		if response.Code != 200 || response.Body.String() != string(cached) {
			t.Fatal("default overview did not use cached payload")
		}
		current, err := p.Store.GetMetadata(a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Store.UpdateAccountVersion(a.ID, current.Version, func(a *Account) error { a.Notes = "invalidates overview"; return nil }); err != nil {
			t.Fatal(err)
		}
		count, err := d.Redis.Exists(context.Background(), d.Namespace+"overview").Result()
		if err != nil || count != 0 {
			t.Fatal("account edit did not invalidate overview")
		}
		if err := d.Redis.Close(); err != nil {
			t.Fatal(err)
		}
		response = webTestRequest(handler, http.MethodGet, "/api/overview", "", "Bearer "+webTestKey)
		if response.Code != 200 || !strings.Contains(response.Body.String(), `"cache_rate":0.8`) {
			t.Fatalf("Redis outage did not fall back to durable metrics: %d %s", response.Code, response.Body.String())
		}
	})
}
