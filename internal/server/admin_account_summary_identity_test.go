package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestManagedAccountSummaryStableIdentity(t *testing.T) {
	exerciseManagedSummaryIdentity(t, webTestSetup)
}
func TestPGManagedAccountSummaryStableIdentity(t *testing.T) {
	exerciseManagedSummaryIdentity(t, adminPGTestProxy)
}

func exerciseManagedSummaryIdentity(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	a := managedTestAccount(t, p, "current-name", nil)
	b := managedTestAccount(t, p, "reused-name", nil)
	stamp := time.Now().Unix() - 60
	p.Metrics.Record(RequestRecord{AccountID: a.ID, AccountCredentialVersion: a.CredentialVersion, Account: "former-name", Time: stamp, Status: "error", HTTPCode: 429})
	p.Metrics.Record(RequestRecord{AccountID: "deleted-account", Account: a.Name, Time: stamp + 2, Status: "ok"})
	p.Metrics.Record(RequestRecord{AccountID: "other-deleted-account", Account: b.Name, Time: stamp + 2, Status: "error"})
	if p.Store.DB == nil {
		p.Metrics.Record(RequestRecord{Account: b.Name, Time: stamp + 3, Status: "error"})
		// Loading a fresh Metrics instance can no longer rely on the request ring
		// to retain a low-traffic account's last business result.
		p.Metrics.mu.Lock()
		p.Metrics.recent = make([]RequestRecord, recentRingCapacity)
		p.Metrics.mu.Unlock()
	}
	if err := p.Metrics.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertDetail := func(account Account, want string) {
		t.Helper()
		response := managedTestRequest(t, h, "GET", "/api/accounts/"+account.ID, nil, 200)
		var result struct {
			Account accountSummary `json:"account"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Account.Status != want {
			t.Fatalf("status got=%s want=%s error=%v", response.Body.String(), want, err)
		}
	}
	assertDetail(a, "error")
	assertDetail(b, "unverified")
	p.Metrics.Record(RequestRecord{AccountID: a.ID, AccountCredentialVersion: a.CredentialVersion, Account: "former-name", Time: stamp, Status: "ok"})
	assertDetail(a, "ok")
	if p.Store.DB != nil {
		if _, err := p.Metrics.Prune(context.Background(), time.Now(), 100); err != nil {
			t.Fatal(err)
		}
		assertDetail(a, "ok")
		assertDetail(b, "unverified")
	} else {
		p.Metrics.Close()
		if err := os.Remove(p.Metrics.logPath()); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(p.Metrics.logPath(), 0700); err != nil {
			t.Fatal(err)
		}
		_, err := p.accountSummaries(httptest.NewRequest("GET", "/", nil), []Account{a})
		if err == nil {
			t.Fatal("unreadable status history became healthy/unknown success")
		}
	}
}

func TestManagedAccountLargeMetadataExport(t *testing.T) {
	exerciseManagedLargeMetadataExport(t, webTestSetup)
}
func TestPGManagedAccountLargeMetadataExport(t *testing.T) {
	exerciseManagedLargeMetadataExport(t, adminPGTestProxy)
}

func exerciseManagedLargeMetadataExport(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	const count = 1003
	if p.Store.DB != nil {
		_, err := p.Store.DB.Pool.Exec(context.Background(), `INSERT INTO accounts(id,display_name,created_at,updated_at) SELECT 'bulk-summary-'||i, 'bulk-summary-'||i, 1700000000,1700000000 FROM generate_series(1,$1::int)i`, count)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		err := p.Store.modify(func(state *accountState) error {
			for i := 1; i <= count; i++ {
				name := fmt.Sprintf("bulk-summary-%d", i)
				state.Accounts = append(state.Accounts, Account{ID: name, Name: name, AccessToken: "synthetic-" + name, Version: 1, Enabled: true, CreatedAt: 1700000000, UpdatedAt: 1700000000})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	response := managedTestRequest(t, h, "POST", "/api/accounts/export", map[string]any{"filters": map[string]any{"search": "bulk-summary-", "sort_by": "name", "sort_order": "asc"}}, 200)
	var result struct {
		Type     string           `json:"type"`
		Accounts []accountSummary `json:"accounts"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Accounts) != count || result.Type != "codex-agent-account-metadata" {
		t.Fatalf("large metadata export: count=%d type=%s err=%v", len(result.Accounts), result.Type, err)
	}
	seen := map[string]bool{}
	for _, a := range result.Accounts {
		if seen[a.ID] || a.ID == "" {
			t.Fatal("duplicate or missing identity")
		}
		seen[a.ID] = true
	}
}
