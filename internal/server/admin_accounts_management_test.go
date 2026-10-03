package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func managedTestRequest(t *testing.T, handler http.Handler, method, path string, body any, status int) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := webTestRequest(handler, method, path, string(data), "Bearer "+webTestKey)
	if r.Code != status {
		t.Fatalf("%s %s: got %d want %d: %s", method, path, r.Code, status, r.Body.String())
	}
	if r.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("management result can be cached")
	}
	return r
}

func managedTestAccount(t *testing.T, p *Proxy, name string, patch func(*Account)) Account {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"access_token": "secret-access-" + name, "refresh_token": "secret-refresh-" + name, "account_id": "identity-" + name, "client_id": "client-" + name, "expires_at": int64(2000000000)})
	if err := p.Store.Import(name, data); err != nil {
		t.Fatal(err)
	}
	a, err := p.Store.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	if patch != nil {
		a, err = p.Store.UpdateAccountVersion(a.ID, a.Version, func(a *Account) error { patch(a); return nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	a, err = p.Store.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func managedTestBindings(t *testing.T, p *Proxy) (Group, ProxyEntry, ProxyEntry) {
	t.Helper()
	g, err := p.Groups.Create("management-test", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	a, err := p.Proxies.Create(ProxyEntry{Name: "first", Type: "http", Host: "127.0.0.1", Port: 12340, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Proxies.Create(ProxyEntry{Name: "second", Type: "socks5", Host: "127.0.0.1", Port: 12341, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return g, a, b
}

func TestManagedAccountsContract(t *testing.T)   { runManagedAccountsContract(t, webTestSetup) }
func TestPGManagedAccountsContract(t *testing.T) { runManagedAccountsContract(t, adminPGTestProxy) }

// Actions must not spend one metadata read per selected item before entering
// the existing per-item CAS transaction.  The bulk metadata preflight is one
// pool checkout, followed by one checkout per update.  Keep this as an
// acquire-count guard rather than a timing assertion so it remains useful on
// busy CI hosts.
func TestPGManagedAccountActionsUseBoundedMetadataPrefetch(t *testing.T) {
	p, h := adminPGTestProxy(t)
	accounts := make([]Account, 100)
	for i := range accounts {
		accounts[i] = managedTestAccount(t, p, fmt.Sprintf("prefetch-%03d", i), nil)
	}
	items := make([]managedAccountItem, len(accounts))
	for i, account := range accounts {
		items[i] = managedAccountItem{ID: account.ID, Version: account.Version}
	}
	before := p.Store.DB.Pool.Stat().AcquireCount()
	r := managedTestRequest(t, h, "POST", "/api/accounts/actions", map[string]any{
		"action": "update",
		"items":  items,
		"patch":  map[string]any{"notes": "prefetch-checked"},
	}, 200)
	acquires := p.Store.DB.Pool.Stat().AcquireCount() - before
	if acquires > int64(len(items))+15 {
		t.Fatalf("actions performed per-item metadata preflight: acquires=%d items=%d body=%s", acquires, len(items), r.Body.String())
	}
	var result struct {
		Succeeded int `json:"succeeded"`
		Failed    int `json:"failed"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Succeeded != len(items) || result.Failed != 0 {
		t.Fatalf("unexpected action result: %+v", result)
	}
}

func runManagedAccountsContract(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	t.Run("filters_sort_total_and_selection", func(t *testing.T) {
		p, h := setup(t)
		g, px, _ := managedTestBindings(t, p)
		a := managedTestAccount(t, p, "filter-alpha", func(a *Account) {
			a.Notes = "Needle in notes"
			a.Tags = []string{"gold"}
			a.Groups = []string{g.ID}
			a.Proxies = []string{px.ID}
			a.Priority = 3
		})
		b := managedTestAccount(t, p, "filter-beta", func(a *Account) { a.Enabled = false; a.Priority = 7; a.Tags = []string{"silver"} })
		c := managedTestAccount(t, p, "filter-gamma", func(a *Account) { a.Priority = 7 })
		record := RequestRecord{EventID: "managed-last-failure", Time: time.Now().Unix(), AccountID: c.ID, AccountCredentialVersion: c.CredentialVersion, Account: c.Name, Status: "error", HTTPCode: 429, Error: "synthetic failure"}
		if p.Store.DB != nil {
			if _, _, err := p.Metrics.ImportRecords(context.Background(), []RequestRecord{record}); err != nil {
				t.Fatal(err)
			}
		} else {
			p.Metrics.Record(record)
		}
		for _, tc := range []struct {
			query string
			names []string
		}{
			{"search=filter-&sort_by=name&sort_order=asc&page_size=2", []string{a.Name, b.Name}},
			{"search=filter-&sort_by=name&sort_order=asc&page=2&page_size=2", []string{c.Name}},
			{"search=NEEDLE", []string{a.Name}},
			{"search=filter-&tag=gold&group_id=" + g.ID + "&proxy_id=" + px.ID + "&enabled=true&has_refresh_token=true&expires_after=1900000000&expires_before=2100000000", []string{a.Name}},
			{"search=filter-&status=disabled", []string{b.Name}},
			{"search=filter-&status=error", []string{c.Name}},
			{"search=filter-&ungrouped=true&no_proxy=true&sort_by=name&sort_order=asc", []string{b.Name, c.Name}},
		} {
			r := managedTestRequest(t, h, "GET", "/api/accounts/manage?"+tc.query, nil, 200)
			var response struct {
				Accounts  []accountSummary `json:"accounts"`
				Total     int              `json:"total"`
				TotalKind string           `json:"total_kind"`
				HasMore   bool             `json:"has_more"`
			}
			if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			names := []string{}
			for _, row := range response.Accounts {
				names = append(names, row.Name)
			}
			if !reflect.DeepEqual(names, tc.names) || response.TotalKind != "exact" {
				t.Fatalf("query %s names=%v total-kind=%s", tc.query, names, response.TotalKind)
			}
			if strings.Contains(tc.query, "page_size") && response.Total != 3 {
				t.Fatalf("pagination lost total: %d", response.Total)
			}
			for _, secret := range []string{"secret-access-", "secret-refresh-", "identity-", "client-", "access_token", "account_id", "client_id"} {
				if strings.Contains(r.Body.String(), secret) {
					t.Fatalf("summary leaked %s", secret)
				}
			}
		}
		var sel struct {
			Items      []managedAccountItem `json:"items"`
			Total      int                  `json:"total"`
			FilterHash string               `json:"filter_hash"`
		}
		r := managedTestRequest(t, h, "POST", "/api/accounts/selection", map[string]any{"filters": map[string]any{"search": "filter-", "sort_by": "priority", "sort_order": "desc"}}, 200)
		if err := json.Unmarshal(r.Body.Bytes(), &sel); err != nil {
			t.Fatal(err)
		}
		if sel.Total != 3 || len(sel.Items) != 3 || sel.Items[2].ID != a.ID || len(sel.FilterHash) != 64 {
			t.Fatalf("bad full selection: %+v", sel)
		}
		if sel.Items[0].ID < sel.Items[1].ID {
			t.Fatal("sort ties are not deterministic by ID")
		}
		for _, query := range []string{"page_size=501", "page=0", "sort_by=access_token", "sort_order=desc%3BDELETE", "status=invalid", "enabled=perhaps", "page=1&page=2", "expires_after=200&expires_before=100", "unknown=1"} {
			managedTestRequest(t, h, "GET", "/api/accounts/manage?"+query, nil, 400)
		}
	})
	t.Run("batch_partial_results_and_versions", func(t *testing.T) {
		p, h := setup(t)
		g, px, _ := managedTestBindings(t, p)
		a := managedTestAccount(t, p, "batch-a", nil)
		b := managedTestAccount(t, p, "batch-b", nil)
		items := []managedAccountItem{{a.ID, a.Version}, {b.ID, b.Version + 1}, {"missing", 1}}
		patch := map[string]any{"enabled": false, "notes": "batch notes", "tags": []string{"new"}, "groups": []string{g.ID}, "proxies": []string{px.ID}, "concurrency": 8, "priority": -4, "weight": 9, "quota": 123456, "window_days": 7}
		r := managedTestRequest(t, h, "POST", "/api/accounts/actions", map[string]any{"action": "update", "items": items, "patch": patch}, 200)
		var result struct {
			Succeeded, Failed int
			Results           []struct {
				ID   string
				OK   bool
				Code string
			}
		}
		if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Succeeded != 1 || result.Failed != 2 || result.Results[1].Code != "version_conflict" || result.Results[2].Code != "not_found" {
			t.Fatalf("wrong per-item result: %s", r.Body.String())
		}
		current, err := p.Store.Get(a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Enabled || current.Notes != "batch notes" || current.Priority != -4 || current.Concurrency != 8 || current.Weight != 9 || current.Quota != 123456 || current.WindowDays != 7 || !reflect.DeepEqual(current.Groups, []string{g.ID}) || !reflect.DeepEqual(current.Proxies, []string{px.ID}) || current.Version != a.Version+1 {
			t.Fatal("batch management values missing")
		}
		managedTestRequest(t, h, "POST", "/api/accounts/actions", map[string]any{"action": "update", "items": []managedAccountItem{{a.ID, current.Version}}, "patch": map[string]any{"groups": []string{}, "proxies": []string{}, "tags": []string{}}}, 200)
		current, _ = p.Store.Get(a.ID)
		if len(current.Groups)+len(current.Proxies)+len(current.Tags) != 0 {
			t.Fatal("empty arrays must clear existing bindings")
		}
		managedTestRequest(t, h, "POST", "/api/accounts/actions", map[string]any{"action": "delete", "items": []managedAccountItem{{a.ID, current.Version}}}, 200)
		if _, err = p.Store.Get(a.ID); !errors.Is(err, errNotFound) {
			t.Fatal("selected account was not deleted")
		}
		for _, body := range []any{
			map[string]any{"action": "delete", "items": []managedAccountItem{{b.ID, 0}}},
			map[string]any{"action": "delete", "items": []managedAccountItem{{b.ID, b.Version}, {b.ID, b.Version}}},
			map[string]any{"action": "update", "items": []managedAccountItem{{b.ID, b.Version}}, "patch": map[string]any{"weight": 0}},
			map[string]any{"action": "update", "items": []managedAccountItem{{b.ID, b.Version}}, "patch": map[string]any{"priority": 2147483648}},
			map[string]any{"action": "refresh", "items": []managedAccountItem{{b.ID, b.Version}}, "patch": map[string]any{}},
		} {
			managedTestRequest(t, h, "POST", "/api/accounts/actions", body, 400)
		}
		// A display name that is not its ID cannot accidentally select a row.
		r = managedTestRequest(t, h, "POST", "/api/accounts/actions", map[string]any{"action": "delete", "items": []managedAccountItem{{b.Name, b.Version}}}, 200)
		if !strings.Contains(r.Body.String(), "not_found") {
			t.Fatal("actions accepted display-name alias")
		}
	})
	t.Run("reauthorize_preserves_identity_and_management", func(t *testing.T) {
		p, h := setup(t)
		g, px, px2 := managedTestBindings(t, p)
		a := managedTestAccount(t, p, "reauthorize-existing", func(a *Account) {
			a.Enabled = false
			a.Notes = "keep notes"
			a.Tags = []string{"keep"}
			a.Groups = []string{g.ID}
			a.Proxies = []string{px2.ID, px.ID}
			a.Concurrency = 9
			a.Priority = -7
			a.Weight = 3
			a.Quota = 999
			a.WindowDays = 6
			a.WindowStart = 1600000000
		})
		before := a
		body := map[string]any{"version": a.Version, "credentials": map[string]any{"access_token": "replacement-secret", "account_id": a.AccountID, "expires_at": 2000000000}}
		r := managedTestRequest(t, h, "POST", "/api/accounts/"+a.ID+"/reauthorize", body, 200)
		if strings.Contains(r.Body.String(), "replacement-secret") {
			t.Fatal("reauthorize response leaked credentials")
		}
		a, err := p.Store.Get(a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if a.ID != before.ID || a.Name != before.Name || a.Enabled != before.Enabled || a.Notes != before.Notes || !reflect.DeepEqual(a.Tags, before.Tags) || !reflect.DeepEqual(a.Groups, before.Groups) || !reflect.DeepEqual(a.Proxies, before.Proxies) || a.Concurrency != before.Concurrency || a.Priority != before.Priority || a.Weight != before.Weight || a.Quota != before.Quota || a.WindowDays != before.WindowDays || a.WindowStart != before.WindowStart || a.CreatedAt != before.CreatedAt || a.Version != before.Version+1 {
			t.Fatal("reauthorize changed stable identity or management values")
		}
		if a.AccessToken != "replacement-secret" || a.RefreshToken != "" || a.ClientID != "" || a.HasRefreshToken || a.CredentialStatus != "unverified" || a.LastCheckedAt != 0 {
			t.Fatal("credentials mixed with previous authorization")
		}
		managedTestRequest(t, h, "POST", "/api/accounts/"+a.ID+"/reauthorize", body, 409)
		body["version"] = a.Version
		body["credentials"] = map[string]any{"access_token": "other-identity-secret", "account_id": "different-identity"}
		r = managedTestRequest(t, h, "POST", "/api/accounts/"+a.ID+"/reauthorize", body, 409)
		if !strings.Contains(r.Body.String(), "identity_change_requires_confirmation") {
			t.Fatal("identity change did not require explicit confirmation")
		}
		body["allow_identity_change"] = true
		managedTestRequest(t, h, "POST", "/api/accounts/"+a.ID+"/reauthorize", body, 200)
		a, _ = p.Store.Get(a.ID)
		other := managedTestAccount(t, p, "duplicate-existing", nil)
		body["version"] = a.Version
		body["credentials"] = map[string]any{"access_token": other.AccessToken, "account_id": "different-identity"}
		managedTestRequest(t, h, "POST", "/api/accounts/"+a.ID+"/reauthorize", body, 409)
		unchanged, _ := p.Store.Get(a.ID)
		if unchanged.AccessToken != a.AccessToken || unchanged.Version != a.Version {
			t.Fatal("failed reauthorization mutated account")
		}
	})
	t.Run("refresh_uses_fake_upstream_and_preconditions", func(t *testing.T) {
		p, h := setup(t)
		a := managedTestAccount(t, p, "refresh-managed", nil)
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"rotated-access-secret","refresh_token":"rotated-refresh-secret","expires_in":3600}`)
		}))
		defer upstream.Close()
		p.TokenURL = upstream.URL
		r := managedTestRequest(t, h, "POST", "/api/accounts/actions", map[string]any{"action": "refresh", "items": []managedAccountItem{{a.ID, a.Version}}}, 200)
		if calls.Load() != 1 || !strings.Contains(r.Body.String(), `"succeeded":1`) || strings.Contains(r.Body.String(), "secret") {
			t.Fatalf("refresh outcome: %s calls=%d", r.Body.String(), calls.Load())
		}
		current, err := p.Store.Get(a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.RefreshToken != "rotated-refresh-secret" || current.ID != a.ID || current.Name != a.Name {
			t.Fatal("refresh rotation lost identity or credential")
		}
		managedTestRequest(t, h, "POST", "/api/accounts/actions", map[string]any{"action": "refresh", "items": []managedAccountItem{{a.ID, a.Version}}}, 200)
		if calls.Load() != 1 {
			t.Fatal("stale selection sent an upstream request")
		}
		_, err = p.Store.Refresh(a.ID, current.AccessToken, func(Account) (Account, error) { return Account{}, errManagedRefreshPrecondition })
		if !errors.Is(err, errVersionConflict) {
			t.Fatal("refresh precondition not reported")
		}
		after, _ := p.Store.Get(a.ID)
		if after.Version != current.Version || after.CredentialStatus != current.CredentialStatus || after.LastCheckedAt != current.LastCheckedAt {
			t.Fatal("precondition changed credential state")
		}
	})
	t.Run("metadata_credentials_export_and_native_restore", func(t *testing.T) {
		p, h := setup(t)
		g, px, px2 := managedTestBindings(t, p)
		a := managedTestAccount(t, p, "backup-managed", func(a *Account) {
			a.Enabled = false
			a.Notes = strings.Repeat("n", 9000)
			a.Tags = []string{strings.Repeat("t", 150)}
			a.Groups = []string{g.ID}
			a.Proxies = []string{px2.ID, px.ID}
			a.Concurrency = 4
			a.Priority = -2
			a.Weight = 5
			a.Quota = 54321
			a.WindowDays = 12
			a.WindowStart = 1600000000
		})
		selection := map[string]any{"items": []managedAccountItem{{a.ID, a.Version}}}
		metadata := managedTestRequest(t, h, "POST", "/api/accounts/export", selection, 200)
		for _, secret := range []string{a.AccessToken, a.RefreshToken, a.AccountID, a.ClientID, `"access_token"`, `"account_id"`, `"client_id"`, `"credentials"`} {
			if strings.Contains(metadata.Body.String(), secret) {
				t.Fatalf("metadata export contains credential %q", secret)
			}
		}
		response := managedTestRequest(t, h, "POST", "/api/accounts/export/credentials", selection, 200)
		if !strings.Contains(response.Header().Get("Content-Disposition"), "attachment") {
			t.Fatal("credential export missing download disposition")
		}
		var bundle managedAccountBundle
		if err := json.Unmarshal(response.Body.Bytes(), &bundle); err != nil {
			t.Fatal(err)
		}
		if bundle.Type != "codex-agent-accounts" || bundle.Version != 1 || len(bundle.Accounts) != 1 {
			t.Fatal("incorrect backup envelope")
		}
		backup := bundle.Accounts[0]
		if backup.Enabled || backup.Notes != a.Notes || !reflect.DeepEqual(backup.Proxies, a.Proxies) || backup.WindowStart != a.WindowStart {
			t.Fatal("backup lost management fields")
		}
		// Separate isolated stores require explicit relation mapping.
		dest, dh := setup(t)
		dg, dpx, dpx2 := managedTestBindings(t, dest)
		body := map[string]any{"data": bundle, "dry_run": true}
		r := managedTestRequest(t, dh, "POST", "/api/accounts/import-bundle", body, 200)
		if !strings.Contains(r.Body.String(), `"failed":1`) {
			t.Fatal("unknown references were silently dropped")
		}
		body["group_mapping"] = map[string]string{g.ID: dg.ID}
		body["proxy_mapping"] = map[string]string{px.ID: dpx.ID, px2.ID: dpx2.ID}
		r = managedTestRequest(t, dh, "POST", "/api/accounts/import-bundle", body, 200)
		if !strings.Contains(r.Body.String(), `"added":1`) {
			t.Fatalf("valid preview failed: %s", r.Body.String())
		}
		if _, err := dest.Store.Get(a.Name); !errors.Is(err, errNotFound) {
			t.Fatal("dry-run persisted an account")
		}
		body["dry_run"] = false
		managedTestRequest(t, dh, "POST", "/api/accounts/import-bundle", body, 200)
		restored, err := dest.Store.Get(a.Name)
		if err != nil {
			t.Fatal(err)
		}
		if restored.AccessToken != a.AccessToken || restored.RefreshToken != a.RefreshToken || restored.AccountID != a.AccountID || restored.ClientID != a.ClientID || restored.ID == a.ID || restored.Enabled || restored.Notes != a.Notes || !reflect.DeepEqual(restored.Tags, a.Tags) || !reflect.DeepEqual(restored.Groups, []string{dg.ID}) || !reflect.DeepEqual(restored.Proxies, []string{dpx2.ID, dpx.ID}) || restored.Quota != a.Quota || restored.WindowDays != a.WindowDays || restored.WindowStart != a.WindowStart || restored.CreatedAt != a.CreatedAt {
			t.Fatal("native restore did not roundtrip credentials and management values")
		}
		r = managedTestRequest(t, dh, "POST", "/api/accounts/import-bundle", body, 200)
		if !strings.Contains(r.Body.String(), `"duplicate":1`) {
			t.Fatal("repeated import was not deduplicated")
		}
		// Metadata is intentionally not a credential backup.
		var meta any
		_ = json.Unmarshal(metadata.Body.Bytes(), &meta)
		managedTestRequest(t, dh, "POST", "/api/accounts/import-bundle", map[string]any{"data": meta}, 400)
		managedTestRequest(t, h, "POST", "/api/accounts/export/credentials", map[string]any{"items": []managedAccountItem{{a.ID, a.Version + 1}}}, 409)
		managedTestRequest(t, h, "POST", "/api/accounts/export/credentials", map[string]any{"items": []managedAccountItem{{a.Name, a.Version}}}, 404)
	})
}

func TestManagedAccountsSelectionBeyondPageAndCap(t *testing.T) {
	p, h := webTestSetup(t)
	items := make([]ImportItem, 620)
	for i := range items {
		raw, _ := json.Marshal(map[string]string{"access_token": fmt.Sprintf("many-secret-%05d", i)})
		items[i] = ImportItem{Name: fmt.Sprintf("many-%05d", i), Credentials: raw}
	}
	if report, err := p.Store.ImportDetailed(items, AccountDefaults{}, ImportOptions{}); err != nil || report.Added != 620 {
		t.Fatal("could not seed bulk fixture", err)
	}
	r := managedTestRequest(t, h, "POST", "/api/accounts/selection", map[string]any{"filters": map[string]any{"search": "many-", "sort_by": "name", "sort_order": "asc"}}, 200)
	var response struct {
		Items []managedAccountItem
		Total int
	}
	if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 620 || response.Total != 620 {
		t.Fatalf("selection truncated to a page: %d/%d", len(response.Items), response.Total)
	}
	if err := p.Store.modify(func(state *accountState) error {
		state.Accounts = nil
		state.Active = ""
		for i := 0; i < 10001; i++ {
			state.Accounts = append(state.Accounts, Account{ID: fmt.Sprintf("a_%05d", i), Name: fmt.Sprintf("cap-%05d", i), AccessToken: fmt.Sprintf("cap-secret-%05d", i), Version: 1, CreatedAt: 1, Enabled: true})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	managedTestRequest(t, h, "POST", "/api/accounts/selection", map[string]any{"filters": map[string]any{}}, 413)
	managedTestRequest(t, h, "POST", "/api/accounts/export/credentials", map[string]any{"filters": map[string]any{}}, 413)
}

func TestManagedAccountsAuthenticationAndFilterEncoding(t *testing.T) {
	_, h := webTestSetup(t)
	for _, path := range []string{"/api/accounts/manage", "/api/accounts/selection", "/api/accounts/actions", "/api/accounts/example/reauthorize", "/api/accounts/export", "/api/accounts/export/credentials", "/api/accounts/import-bundle"} {
		method := "POST"
		if strings.HasSuffix(path, "/manage") {
			method = "GET"
		}
		r := webTestRequest(h, method, path, `{}`, "")
		if r.Code != 401 {
			t.Fatalf("unprotected endpoint: %s returned %d", path, r.Code)
		}
	}
	f, _, _, err := parseManagedAccountQuery(url.Values{"expires_after": {"1900000000"}, "has_refresh_token": {"false"}})
	if err != nil || f.ExpiresAfter != 1900000000 || f.HasRefreshToken == nil || *f.HasRefreshToken {
		t.Fatal("filter values changed meaning")
	}
}
