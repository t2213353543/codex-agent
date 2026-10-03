package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagedUsageBatchOfficialCache(t *testing.T) {
	runManagedUsageBatchOfficialCache(t, webTestSetup)
}

func TestPGManagedUsageBatchOfficialCache(t *testing.T) {
	runManagedUsageBatchOfficialCache(t, adminPGTestProxy)
}

func runManagedUsageBatchOfficialCache(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	a := managedTestAccount(t, p, "official-usage", nil)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/backend-api/wham/usage" || r.Header.Get("Authorization") != "Bearer "+a.AccessToken || r.Header.Get("ChatGPT-Account-ID") != a.AccountID {
			t.Errorf("incorrect official usage request: %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprintf(w, `{"plan_type":"team","rate_limit":{"primary_window":{"limit_window_seconds":604800,"used_percent":47,"reset_at":%d},"secondary_window":null},"additional_rate_limits":[{"limit_name":"Spark","rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":null}}}]}`, time.Now().Add(time.Hour).Unix())
	}))
	defer upstream.Close()
	p.BaseURL = upstream.URL + "/backend-api/codex"
	read := func(force bool) managedUpstreamSnapshot {
		t.Helper()
		response := managedTestRequest(t, h, "POST", "/api/accounts/upstream/batch", map[string]any{"kind": "usage", "force": force, "items": []managedAccountItem{{ID: a.ID, Version: a.Version}}}, 200)
		var result struct {
			Results []struct {
				OK       bool                    `json:"ok"`
				Snapshot managedUpstreamSnapshot `json:"snapshot"`
			} `json:"results"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Results) != 1 || !result.Results[0].OK {
			t.Fatalf("usage batch failed: %s", response.Body.String())
		}
		return result.Results[0].Snapshot
	}
	snapshot := read(false)
	if calls.Load() != 1 {
		t.Fatal("initial list did not query official usage")
	}
	raw, _ := json.Marshal(snapshot.Data)
	data, err := parseManagedUsage(raw)
	if err != nil {
		t.Fatal(err)
	}
	usage := data.(managedUsage)
	if *usage.RateLimit.PrimaryWindow.LimitWindowSeconds != 604800 || *usage.RateLimit.PrimaryWindow.UsedPercent != 47 || usage.RateLimit.SecondaryWindow != nil || usage.Additional[0].RateLimit.PrimaryWindow.UsedPercent != nil {
		t.Fatal("official windows or unknown values were replaced by local accounting")
	}
	read(false)
	if calls.Load() != 1 {
		t.Fatal("fresh list usage was queried twice")
	}
	read(true)
	if calls.Load() != 2 {
		t.Fatal("explicit refresh did not bypass usage cache")
	}
	for _, reason := range []string{"old", "credential", "failed", "reset"} {
		snapshot.OK = true
		snapshot.CheckedAt = time.Now().Unix()
		snapshot.CredentialVersion = a.CredentialVersion
		snapshot.Data = usage
		switch reason {
		case "old":
			snapshot.CheckedAt -= 301
		case "credential":
			snapshot.CredentialVersion--
		case "failed":
			snapshot.OK = false
		case "reset":
			copy := usage
			rate := *usage.RateLimit
			window := *rate.PrimaryWindow
			reset := time.Now().Unix() - 1
			window.ResetAt = &reset
			rate.PrimaryWindow = &window
			copy.RateLimit = &rate
			snapshot.Data = copy
		}
		// Seed a pre-existing stale cache row directly. The production writer
		// deliberately rejects an older epoch/time arriving after a fresh row.
		if err := p.managementState().Update(context.Background(), "account-upstream", snapshot.ID, func(entry *managementStateEntry) error {
			var rows map[string]managedUpstreamSnapshot
			if err := json.Unmarshal(entry.Payload, &rows); err != nil {
				return err
			}
			rows[snapshot.Kind] = snapshot
			raw, err := json.Marshal(rows)
			entry.Payload = raw
			return err
		}); err != nil {
			t.Fatal(err)
		}
		before := calls.Load()
		read(false)
		if calls.Load() != before+1 {
			t.Fatalf("%s cache was reused", reason)
		}
	}
	// Real token rotation advances both versions. Old page selections must not
	// query with replacement credentials; a refreshed selection must bypass the
	// previously saved successful usage snapshot.
	oldVersion := a.Version
	if _, err := p.Store.reauthorizeManagedAccount(context.Background(), a.ID, oldVersion, Account{AccessToken: "rotated-official-usage", AccountID: a.AccountID}, false); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	conflict := managedTestRequest(t, h, "POST", "/api/accounts/upstream/batch", map[string]any{"kind": "usage", "items": []managedAccountItem{{ID: a.ID, Version: oldVersion}}}, 200)
	var staleSelection struct {
		Results []struct {
			Code string `json:"code"`
		} `json:"results"`
	}
	if json.Unmarshal(conflict.Body.Bytes(), &staleSelection) != nil || len(staleSelection.Results) != 1 || staleSelection.Results[0].Code != "version_conflict" || calls.Load() != before {
		t.Fatal("old page selection queried rotated credentials")
	}
	a, err = p.Store.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	rotated := read(false)
	if calls.Load() != before+1 || rotated.Version != a.Version {
		t.Fatal("new credentials reused the old official snapshot")
	}
}

func TestManagedUsageMissingFieldsStayUnknown(t *testing.T) {
	data, err := parseManagedUsage([]byte(`{"plan_type":"free","rate_limit":{"primary_window":{},"secondary_window":null}}`))
	if err != nil {
		t.Fatal(err)
	}
	usage := data.(managedUsage)
	window := usage.RateLimit.PrimaryWindow
	if window.UsedPercent != nil || window.ResetAt != nil || window.LimitWindowSeconds != nil || usage.RateLimit.SecondaryWindow != nil {
		t.Fatal("missing official data became zero usage")
	}
}
