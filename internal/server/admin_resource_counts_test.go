package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestManagedResourceGroupCountsDeduplicateLegacyMembership(t *testing.T) {
	counts := map[string]int{"existing": 2}
	addManagedGroupCounts(counts, []string{"first", "first", "existing", "second", "second"})
	if counts["first"] != 1 || counts["second"] != 1 || counts["existing"] != 3 {
		t.Fatalf("members counted more than once: %v", counts)
	}
}

func exerciseManagedResourceCounts(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, handler := setup(t)
	group, err := p.Groups.Create("counted", "")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := p.Proxies.Create(ProxyEntry{Name: "counted", Type: "http", Host: "192.0.2.1", Port: 9})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Store.Import("counted", []byte(`{"access_token":"synthetic-resource-token"}`)); err != nil {
		t.Fatal(err)
	}
	a, err := p.Store.GetMetadata("counted")
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Store.UpdateAccountVersion(a.ID, a.Version, func(a *Account) error {
		a.Groups, a.Proxies, a.Enabled = []string{group.ID}, []string{proxy.ID}, false
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := p.Keys.Create("counted", []string{group.ID})
	if err != nil {
		t.Fatal(err)
	}
	p.Metrics.Record(RequestRecord{EventID: "resource-count", Time: 1800000000, KeyID: key.ID, Status: "error", InputTokens: 7, OutputTokens: 3})
	if err := p.Metrics.HealthError(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, field, id string
		keys            int
	}{
		{"/api/groups", "groups", group.ID, 1}, {"/api/proxies", "proxies", proxy.ID, 0},
	} {
		w := webTestRequest(handler, http.MethodGet, tc.path, "", "Bearer "+webTestKey)
		var body map[string][]struct {
			ID       string `json:"id"`
			Accounts int    `json:"accounts"`
			Keys     int    `json:"keys"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
		found := false
		for _, item := range body[tc.field] {
			if item.ID == tc.id {
				found = true
				if item.Accounts != 1 || item.Keys != tc.keys {
					t.Fatalf("wrong association count: %+v", item)
				}
			}
		}
		if !found {
			t.Fatalf("missing resource %s", tc.id)
		}
	}
	values, err := p.Metrics.managedKeyUsage(context.Background(), []APIKey{key, {ID: "unused"}})
	if err != nil || values[key.ID] != (usageCounts{Requests: 1, Tokens: 10, Errors: 1, LastAt: 1800000000}) || values["unused"] != (usageCounts{}) {
		t.Fatalf("batch usage mismatch: %+v %v", values, err)
	}
	total, enabled, err := p.managedAccountCounts(context.Background())
	_, accounts, readErr := p.Store.ListMetadata()
	if err != nil || readErr != nil || total != len(accounts) || enabled != total-1 {
		t.Fatalf("account counts %d/%d: %v %v", total, enabled, err, readErr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.managedAssociationCounts(ctx, false); err == nil {
		t.Fatal("canceled relationship read succeeded")
	}
	if _, err := p.Metrics.managedKeyUsage(ctx, []APIKey{key}); err == nil {
		t.Fatal("canceled usage read succeeded")
	}
}

func TestManagedResourceCounts(t *testing.T)   { exerciseManagedResourceCounts(t, webTestSetup) }
func TestPGManagedResourceCounts(t *testing.T) { exerciseManagedResourceCounts(t, adminPGTestProxy) }

func TestPGManagedKeyUsageFailureIsNotZeroSuccess(t *testing.T) {
	p, handler := adminPGTestProxy(t)
	if _, err := p.Keys.Create("existing", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Store.DB.Pool.Exec(context.Background(), "ALTER TABLE usage_totals RENAME TO unavailable_usage_totals"); err != nil {
		t.Fatal(err)
	}
	w := webTestRequest(handler, http.MethodGet, "/api/keys", "", "Bearer "+webTestKey)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("failed statistics reported success: %d %s", w.Code, w.Body.String())
	}
}
