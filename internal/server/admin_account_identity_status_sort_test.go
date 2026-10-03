package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestManagedAccountIdentityStatusSort(t *testing.T) {
	exerciseManagedAccountIdentityStatusSort(t, webTestSetup)
}
func TestPGManagedAccountIdentityStatusSort(t *testing.T) {
	exerciseManagedAccountIdentityStatusSort(t, adminPGTestProxy)
}

func exerciseManagedAccountIdentityStatusSort(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	ctx := context.Background()
	var accounts []Account
	statuses := map[string]string{}
	byName := map[string]string{}
	now := time.Now().Unix()
	for index, status := range []string{"disabled", "error", "expired", "ok", "unverified"} {
		for variant := 0; variant < 2; variant++ {
			name := fmt.Sprintf("identity-status-%02d", (4-index)*2+variant)
			expires := now + 86400
			if status == "disabled" || status == "expired" {
				expires = now - 3600
			}
			raw, _ := json.Marshal(map[string]any{"access_token": "synthetic-sort-access-" + name, "refresh_token": "synthetic-sort-refresh-" + name, "expires_at": expires})
			if err := p.Store.Import(name, raw); err != nil {
				t.Fatal(err)
			}
			a, err := p.Store.Get(name)
			if err != nil {
				t.Fatal(err)
			}
			a, err = p.Store.UpdateAccountVersion(a.ID, a.Version, func(a *Account) error {
				if status == "disabled" {
					a.Enabled = false
					a.CredentialStatus = "error"
				}
				if status == "error" && variant == 0 {
					a.CredentialStatus = "refresh_failed"
				}
				if status == "ok" && variant == 0 {
					a.CredentialStatus = "valid"
					a.LastCheckedAt = now
				}
				if variant == 0 {
					a.Tags = []string{"subset"}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			accounts = append(accounts, a)
			statuses[a.ID] = status
			byName[a.Name] = a.ID
			if status == "disabled" || status == "expired" || status == "error" && variant == 0 || status == "ok" && variant == 1 {
				p.Metrics.Record(RequestRecord{EventID: "sort-ok-" + name, Time: 1700000001, AccountID: a.ID, AccountCredentialVersion: a.CredentialVersion, Account: "previous-" + name, Status: "ok"})
			}
			if status == "error" && variant == 1 {
				p.Metrics.Record(RequestRecord{EventID: "sort-before-" + name, Time: 1700000002, AccountID: a.ID, AccountCredentialVersion: a.CredentialVersion, Account: "old-name", Status: "ok"})
				p.Metrics.Record(RequestRecord{EventID: "sort-after-" + name, Time: 1700000002, AccountID: a.ID, AccountCredentialVersion: a.CredentialVersion, Account: "renamed-from-source", Status: "aborted"})
			}
			if status == "unverified" {
				p.Metrics.Record(RequestRecord{EventID: "sort-unrelated-" + name, Time: 1900000000, AccountID: "deleted-distinct-" + name, Account: a.Name, Status: "ok"})
			}
		}
	}
	if err := p.Metrics.HealthError(); err != nil {
		t.Fatal(err)
	}
	ids := func(rows []Account) []string {
		out := make([]string, len(rows))
		for i, a := range rows {
			out[i] = a.ID
		}
		return out
	}
	for _, field := range []string{"id", "status"} {
		for _, direction := range []string{"asc", "desc"} {
			for _, scope := range []string{"all", "tag", "error"} {
				t.Run(field+"-"+direction+"-"+scope, func(t *testing.T) {
					f := managedAccountFilters{Search: "identity-status-", SortBy: field, SortOrder: direction}
					if scope == "tag" {
						f.Tag = "subset"
					}
					if scope == "error" {
						f.Status = "error"
					}
					if err := f.normalize(); err != nil {
						t.Fatal(err)
					}
					want := []Account{}
					for _, a := range accounts {
						if scope == "tag" && !accountHas(a.Tags, "subset") {
							continue
						}
						if scope == "error" && statuses[a.ID] != "error" {
							continue
						}
						want = append(want, a)
					}
					sort.Slice(want, func(i, j int) bool {
						cmp := 0
						if field == "status" {
							cmp = strings.Compare(statuses[want[i].ID], statuses[want[j].ID])
						}
						if cmp == 0 {
							cmp = strings.Compare(want[i].ID, want[j].ID)
						}
						if direction == "asc" {
							return cmp < 0
						}
						return cmp > 0
					})
					all := []Account{}
					for offset := 0; offset < len(want); offset += 3 {
						rows, total, err := p.managedAccounts(ctx, f, offset, 3, false)
						if err != nil || total != int64(len(want)) {
							t.Fatalf("page total=%d err=%v", total, err)
						}
						all = append(all, rows...)
					}
					if !reflect.DeepEqual(ids(all), ids(want)) {
						t.Fatalf("global %s sort: got %v want %v", field, ids(all), ids(want))
					}
					query := url.Values{"search": {f.Search}, "sort_by": {field}, "sort_order": {direction}, "page_size": {"3"}, "page": {"1"}}
					if f.Tag != "" {
						query.Set("tag", f.Tag)
					}
					if f.Status != "" {
						query.Set("status", f.Status)
					}
					response := managedTestRequest(t, h, "GET", "/api/accounts/manage?"+query.Encode(), nil, 200)
					var page struct {
						Accounts []accountSummary `json:"accounts"`
						Total    int64            `json:"total"`
					}
					if json.Unmarshal(response.Body.Bytes(), &page) != nil || page.Total != int64(len(want)) {
						t.Fatal("list response")
					}
					for i, a := range page.Accounts {
						if a.ID != want[i].ID || a.Status != statuses[a.ID] {
							t.Fatal("displayed status differs from sorted status", a.ID, a.Status, statuses[a.ID])
						}
					}
					response = managedTestRequest(t, h, "POST", "/api/accounts/selection", map[string]any{"filters": f}, 200)
					var selected struct {
						Items []managedAccountItem `json:"items"`
						Total int64                `json:"total"`
					}
					if json.Unmarshal(response.Body.Bytes(), &selected) != nil || selected.Total != int64(len(want)) || len(selected.Items) != len(want) {
						t.Fatal("selection count")
					}
					for i, a := range selected.Items {
						if a.ID != want[i].ID || a.Version != want[i].Version {
							t.Fatal("selection lost ordered stable versions")
						}
					}
					for _, endpoint := range []string{"/api/accounts/export", "/api/accounts/export/credentials"} {
						response = managedTestRequest(t, h, "POST", endpoint, map[string]any{"filters": f}, 200)
						var exported struct {
							Accounts []struct {
								ID   string `json:"id"`
								Name string `json:"name"`
							} `json:"accounts"`
						}
						if json.Unmarshal(response.Body.Bytes(), &exported) != nil || len(exported.Accounts) != len(want) {
							t.Fatal("full export count")
						}
						for i, a := range exported.Accounts {
							id := a.ID
							if endpoint == "/api/accounts/export/credentials" {
								id = byName[a.Name]
							}
							if id != want[i].ID {
								t.Fatal("full export order differs")
							}
						}
					}
				})
			}
		}
	}
	for _, status := range []string{"disabled", "error", "expired", "ok", "unverified"} {
		f := managedAccountFilters{Search: "identity-status-", SortBy: "status", SortOrder: "asc", Status: status}
		if err := f.normalize(); err != nil {
			t.Fatal(err)
		}
		rows, total, err := p.managedAccounts(ctx, f, 0, 100, false)
		if err != nil || total != 2 || len(rows) != 2 {
			t.Fatalf("filter %s count %d %v", status, total, err)
		}
		for _, a := range rows {
			if statuses[a.ID] != status {
				t.Fatal("filter status mismatch")
			}
		}
	}
	for _, query := range []string{"sort_by=status%3BDROP%20TABLE%20accounts", "sort_by=id&sort_order=sideways", "sort_by=status&status=unknown"} {
		managedTestRequest(t, h, "GET", "/api/accounts/manage?"+query, nil, 400)
	}
}
