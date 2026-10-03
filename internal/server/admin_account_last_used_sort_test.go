package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestManagedAccountLastUsedSort(t *testing.T) {
	exerciseManagedAccountLastUsedSort(t, webTestSetup)
}
func TestPGManagedAccountLastUsedSort(t *testing.T) {
	exerciseManagedAccountLastUsedSort(t, adminPGTestProxy)
}

func TestPGManagedAccountLastUsedSortIndependentOfEventHistory(t *testing.T) {
	p, handler := adminPGTestProxy(t)
	a := managedTestAccount(t, p, "last-sort-snapshot", nil)
	b := managedTestAccount(t, p, "last-sort-unused", nil)
	p.Metrics.Record(RequestRecord{EventID: "last-sort-current", AccountID: a.ID, Account: a.Name, Time: 1700000200, Status: "ok"})
	p.Metrics.Record(RequestRecord{EventID: "last-sort-delayed", AccountID: a.ID, Account: a.Name, Time: 1700000100, Status: "error"})
	if err := p.Metrics.HealthError(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Event history can be arbitrarily large. The list only needs the durable
	// state maintained by the writer; legacy aggregate timestamps are irrelevant.
	if _, err := p.Store.DB.Pool.Exec(ctx, "ALTER TABLE usage_events RENAME TO unavailable_usage_events"); err != nil {
		t.Fatal(err)
	}
	for _, direction := range []string{"asc", "desc"} {
		filter := managedAccountFilters{Search: "last-sort-", SortBy: "last_used_at", SortOrder: direction}
		accounts, total, err := p.managedAccounts(ctx, filter, 0, 50, false)
		if err != nil || total != 2 || len(accounts) != 2 || accounts[0].ID != a.ID || accounts[1].ID != b.ID {
			t.Fatalf("%s snapshot sort: %v %d %v", direction, accounts, total, err)
		}
	}
	last, scope, err := p.managedAccountLastUsed(ctx, []string{a.ID, b.ID})
	if err != nil || scope != "durable_usage_events" || len(last) != 2 || last[0].At == nil || *last[0].At != 1700000200 || last[1].At != nil {
		t.Fatalf("last-used snapshot: %+v %s %v", last, scope, err)
	}
	response := webTestRequest(handler, http.MethodGet, "/api/accounts/"+a.ID+"/runtime", "", "Bearer "+webTestKey)
	var body struct {
		LastUsed struct {
			At *int64 `json:"at"`
		} `json:"last_used"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil || body.LastUsed.At == nil || *body.LastUsed.At != 1700000200 {
		t.Fatalf("runtime depends on event scan: %d %s", response.Code, response.Body.String())
	}
}
func exerciseManagedAccountLastUsedSort(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	ctx := context.Background()
	var accounts []Account
	times := map[string]int64{}
	for i := 0; i < 7; i++ {
		a := managedTestAccount(t, p, fmt.Sprintf("last-sort-%d", i), nil)
		accounts = append(accounts, a)
		if i < 5 {
			at := int64(1700000000 + (i%3)*60)
			times[a.ID] = at
			p.Metrics.Record(RequestRecord{AccountID: a.ID, Account: "previous-name", Time: at, Status: "error"})
			p.Metrics.Record(RequestRecord{AccountID: a.ID, Account: "previous-name", Time: at - 100, Status: "ok"})
		}
	}
	// Similar names and legacy aggregate timestamps cannot turn an unmatched
	// stable ID into a used account, or replace its actual business timestamp.
	p.Metrics.Record(RequestRecord{AccountID: "deleted-distinct-id", Account: accounts[6].Name, Time: 1900000000, Status: "ok"})
	if p.Store.DB == nil {
		p.Metrics.Record(RequestRecord{Account: accounts[5].Name, Time: 1900000000, Status: "ok"})
	} else {
		if _, err := p.Store.DB.Pool.Exec(ctx, "UPDATE usage_totals SET last_at=2000000000 WHERE scope='account'"); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Metrics.Prune(ctx, time.Unix(1900000001, 0), 100); err != nil {
			t.Fatal(err)
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
	for _, direction := range []string{"asc", "desc"} {
		t.Run(direction, func(t *testing.T) {
			f := managedAccountFilters{Search: "last-sort-", SortBy: "last_used_at", SortOrder: direction}
			if err := f.normalize(); err != nil {
				t.Fatal(err)
			}
			want := append([]Account(nil), accounts...)
			sort.Slice(want, func(i, j int) bool {
				a, b := times[want[i].ID], times[want[j].ID]
				if (a == 0) != (b == 0) {
					return a != 0
				}
				if a == b {
					if direction == "asc" {
						return want[i].ID < want[j].ID
					}
					return want[i].ID > want[j].ID
				}
				if direction == "asc" {
					return a < b
				}
				return a > b
			})
			var got []Account
			for offset := 0; offset < len(accounts); offset += 2 {
				page, total, err := p.managedAccounts(ctx, f, offset, 2, false)
				if err != nil || total != int64(len(accounts)) {
					t.Fatalf("page: %d %v", total, err)
				}
				got = append(got, page...)
			}
			if !reflect.DeepEqual(ids(got), ids(want)) {
				t.Fatalf("global order: got=%v want=%v", ids(got), ids(want))
			}
			response := managedTestRequest(t, h, "GET", "/api/accounts/manage?search=last-sort-&page_size=2&sort_by=last_used_at&sort_order="+direction, nil, 200)
			var first struct {
				Accounts []struct {
					ID string `json:"id"`
				} `json:"accounts"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil || len(first.Accounts) != 2 || first.Accounts[0].ID != want[0].ID {
				t.Fatalf("list: %s %v", response.Body.String(), err)
			}
			response = managedTestRequest(t, h, "POST", "/api/accounts/selection", map[string]any{"filters": f}, 200)
			var selection struct {
				Items []managedAccountItem `json:"items"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &selection); err != nil || len(selection.Items) != len(want) {
				t.Fatalf("selection: %s %v", response.Body.String(), err)
			}
			for i, item := range selection.Items {
				if item.ID != want[i].ID {
					t.Fatal("selection changed order")
				}
			}
			response = managedTestRequest(t, h, "POST", "/api/accounts/export", map[string]any{"filters": f}, 200)
			var exported struct {
				Accounts []struct {
					ID string `json:"id"`
				} `json:"accounts"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &exported); err != nil || len(exported.Accounts) != len(want) {
				t.Fatalf("export: %s %v", response.Body.String(), err)
			}
			for i, item := range exported.Accounts {
				if item.ID != want[i].ID {
					t.Fatal("export changed order")
				}
			}
		})
	}
}
