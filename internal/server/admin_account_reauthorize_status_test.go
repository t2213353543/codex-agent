package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestManagedReauthorizeDoesNotInheritOld401Status(t *testing.T) {
	exerciseManagedReauthorizeDoesNotInheritOld401Status(t, webTestSetup)
}

func TestPGManagedReauthorizeDoesNotInheritOld401Status(t *testing.T) {
	exerciseManagedReauthorizeDoesNotInheritOld401Status(t, adminPGTestProxy)
}

func exerciseManagedReauthorizeDoesNotInheritOld401Status(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	a := managedTestAccount(t, p, "reauth-old-401", func(a *Account) {
		a.LastError = "upstream HTTP 401"
		a.CredentialStatus = "invalid"
		a.LastCheckedAt = time.Now().Unix()
	})
	p.Metrics.Record(RequestRecord{AccountID: a.ID, AccountCredentialVersion: a.CredentialVersion, Account: a.Name, Time: time.Now().Unix(), Status: "error", HTTPCode: 401, Error: "upstream HTTP 401"})
	credentialVersion := a.CredentialVersion
	if err := p.cacheManagedUpstream(context.Background(), managedUpstreamSnapshot{ID: a.ID, Version: a.Version, CredentialVersion: credentialVersion,
		Kind: "usage", Source: "codex_usage", HTTPCode: 401, CheckedAt: time.Now().Unix(), OK: false}); err != nil {
		t.Fatal(err)
	}
	before := managedTestRequest(t, h, "GET", "/api/accounts/"+a.ID, nil, 200)
	var response struct {
		Account accountSummary `json:"account"`
	}
	if json.Unmarshal(before.Body.Bytes(), &response) != nil || response.Account.Status != "error" {
		t.Fatal("initial 401 fixture was not visible")
	}
	managedTestRequest(t, h, "POST", "/api/accounts/"+a.ID+"/reauthorize", map[string]any{
		"version": a.Version, "credentials": map[string]any{"access_token": "synthetic-new-reauthorized-AT", "account_id": a.AccountID, "expires_at": time.Now().Add(time.Hour).Unix()},
	}, 200)
	current, err := p.Store.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.LastError != "" || current.CredentialStatus != "unverified" || current.LastCheckedAt != 0 {
		t.Fatal("reauthorize did not reset credential metadata")
	}
	oldSnapshot := managedTestRequest(t, h, "GET", "/api/accounts/"+a.ID+"/upstream", nil, 200)
	var snapshots struct {
		Snapshots map[string]managedUpstreamSnapshot `json:"snapshots"`
	}
	if json.Unmarshal(oldSnapshot.Body.Bytes(), &snapshots) != nil || !snapshots.Snapshots["usage"].Stale {
		t.Fatal("old credential's 401 snapshot remained current")
	}
	after := managedTestRequest(t, h, "GET", "/api/accounts/"+a.ID, nil, 200)
	if json.Unmarshal(after.Body.Bytes(), &response) != nil {
		t.Fatal("invalid detail response")
	}
	if response.Account.Status != "unverified" {
		t.Fatalf("new credential kept old 401 status: status=%q credential_status=%q last_error=%q", response.Account.Status, response.Account.CredentialStatus, response.Account.LastError)
	}
	// Historical request audits remain; only current credential interpretation changes.
	if last, found := p.Metrics.LastFor(a.Name); !found || last.HTTPCode != 401 {
		t.Fatal("reauthorize erased historical request evidence")
	}
	if current.CredentialVersion <= a.CredentialVersion {
		t.Fatal("replacement did not advance durable credential epoch")
	}
	// A late old-epoch observation must not replace a new epoch's successful
	// status, even if its recorded start timestamp sorts after the success.
	now := time.Now().Unix()
	p.Metrics.Record(RequestRecord{AccountID: a.ID, AccountCredentialVersion: current.CredentialVersion, Account: a.Name, Time: now + 1, Status: "ok", HTTPCode: 200})
	p.Metrics.Record(RequestRecord{AccountID: a.ID, AccountCredentialVersion: a.CredentialVersion, Account: a.Name, Time: now + 2, Status: "error", HTTPCode: 401})
	if err := p.Metrics.HealthError(); err != nil {
		t.Fatal(err)
	}
	check := managedTestRequest(t, h, "GET", "/api/accounts/"+a.ID, nil, 200)
	if json.Unmarshal(check.Body.Bytes(), &response) != nil || response.Account.Status != "ok" {
		t.Fatalf("old epoch completion overwrote current status: %s", check.Body.String())
	}
	// A metadata edit preserves this epoch and its current observation.
	updated, err := p.Store.UpdateAccountVersion(a.ID, current.Version, func(a *Account) error { a.Name = "renamed-after-reauth"; a.Notes = "metadata-only"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if updated.CredentialVersion != current.CredentialVersion {
		t.Fatal("metadata edit changed credential epoch")
	}
	check = managedTestRequest(t, h, "GET", "/api/accounts/"+a.ID, nil, 200)
	if json.Unmarshal(check.Body.Bytes(), &response) != nil || response.Account.Status != "ok" {
		t.Fatal("rename cleared current observation")
	}
	for _, status := range []string{"ok", "error", "unverified"} {
		page := managedTestRequest(t, h, "GET", "/api/accounts/manage?search=renamed-after-reauth&sort_by=status&status="+status, nil, 200)
		var result struct {
			Accounts []accountSummary `json:"accounts"`
			Total    int64            `json:"total"`
		}
		if json.Unmarshal(page.Body.Bytes(), &result) != nil {
			t.Fatal("invalid status page")
		}
		want := int64(0)
		if status == "ok" {
			want = 1
		}
		if result.Total != want {
			t.Fatalf("status filter %s returned %d want %d", status, result.Total, want)
		}
	}
	// Keep the independent last-request audit pointed at the late historical
	// error, while the current-credential status remains the new success.
	last, found := p.Metrics.LastFor(a.Name)
	if !found || last.HTTPCode != 401 || last.AccountCredentialVersion != a.CredentialVersion {
		t.Fatal("current-state filtering modified last-request audit")
	}
	if p.Store.DB != nil {
		var version int64
		if err := p.Store.DB.Pool.QueryRow(context.Background(), "SELECT credential_version FROM account_credential_request_state WHERE account_ref=$1", a.ID).Scan(&version); err != nil || version != current.CredentialVersion {
			t.Fatalf("current state epoch=%d error=%v", version, err)
		}
		if err := p.Store.Delete(a.ID); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := p.Store.DB.Pool.QueryRow(context.Background(), "SELECT count(*) FROM account_credential_request_state WHERE account_ref=$1", a.ID).Scan(&count); err != nil || count != 0 {
			t.Fatal("deleted account retained current credential state")
		}
		if last, found := p.Metrics.LastFor(a.Name); !found || last.HTTPCode != 401 {
			t.Fatal("delete removed historical last-request audit")
		}
	}
}

func TestManagedUnversionedHistoryIsNotCurrentCredentialEvidence(t *testing.T) {
	p, h := webTestSetup(t)
	a := managedTestAccount(t, p, "legacy-no-credential-generation", nil)
	p.Metrics.Record(RequestRecord{AccountID: a.ID, Account: a.Name, Time: time.Now().Unix(), Status: "error", HTTPCode: 401})
	r := managedTestRequest(t, h, "GET", "/api/accounts/"+a.ID, nil, 200)
	var response struct {
		Account accountSummary `json:"account"`
	}
	if json.Unmarshal(r.Body.Bytes(), &response) != nil || response.Account.Status != "unverified" {
		t.Fatal("legacy observation was attributed to current credentials")
	}
	if last, found := p.Metrics.LastFor(a.Name); !found || last.HTTPCode != 401 {
		t.Fatal("legacy evidence was deleted")
	}
}

func TestFileLegacyAccountStatusPaginationUsesCurrentCredentials(t *testing.T) {
	p, h := webTestSetup(t)
	want := map[string]bool{}
	for _, name := range []string{"legacy-page-first", "legacy-page-second", "legacy-page-unknown"} {
		a := managedTestAccount(t, p, name, nil)
		if name != "legacy-page-unknown" {
			want[a.ID] = true
			p.Metrics.Record(RequestRecord{AccountID: a.ID, AccountCredentialVersion: a.CredentialVersion, Account: a.Name, Time: time.Now().Unix(), Status: "ok", HTTPCode: 200})
		}
	}
	cursor := ""
	seen := map[string]bool{}
	for range 3 {
		query := url.Values{"search": {"legacy-page-"}, "status": {"ok"}, "limit": {"1"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		r := managedTestRequest(t, h, "GET", "/api/accounts?"+query.Encode(), nil, 200)
		var page struct {
			Accounts   []accountSummary `json:"accounts"`
			NextCursor string           `json:"next_cursor"`
			HasMore    bool             `json:"has_more"`
		}
		if json.Unmarshal(r.Body.Bytes(), &page) != nil || len(page.Accounts) != 1 {
			t.Fatalf("legacy status page incomplete: %s", r.Body.String())
		}
		row := page.Accounts[0]
		if !want[row.ID] || seen[row.ID] || row.Status != "ok" {
			t.Fatal("legacy status pagination leaked unknown/duplicate status")
		}
		seen[row.ID] = true
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 2 {
		t.Fatalf("status pagination got %d current accounts", len(seen))
	}
}
