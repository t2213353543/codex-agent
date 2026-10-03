package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestManagedAccountUsageBatchIdentityAndCoverage(t *testing.T) {
	p, handler := webTestSetup(t)
	known := true
	p.Metrics.Record(RequestRecord{EventID: "batch-a", Time: 1700000000, AccountID: "account-a", Account: "same name", Model: "fixture", Status: "ok", InputTokens: 10, OutputTokens: 2, UsageKnown: &known})
	p.Metrics.Record(RequestRecord{EventID: "batch-b", Time: 1700000000, AccountID: "account-b", Account: "same name", Status: "ok", InputTokens: 100})
	p.Metrics.Record(RequestRecord{EventID: "batch-end", Time: 1700000010, AccountID: "account-a", Account: "renamed", Status: "ok", InputTokens: 999})
	call := func(body string, authenticated bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/accounts/usage-batch", strings.NewReader(body))
		if authenticated {
			r.Header.Set("Authorization", "Bearer "+webTestKey)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	request := `{"ids":["account-a","unused","account-a"],"from":1700000000,"to":1700000010}`
	if w := call(request, false); w.Code != 401 {
		t.Fatal("anonymous access", w.Code)
	}
	w := call(request, true)
	var result struct {
		Items []managedUsageResource `json:"items"`
		Scope string                 `json:"scope"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(result.Items) != 2 || result.Items[0].ID != "account-a" || result.Items[0].Requests != 1 || result.Items[0].Tokens != 12 || result.Items[0].UsageKnownRequests != 1 {
		t.Fatalf("identity, exclusive boundary or usage coverage: %+v", result)
	}
	if result.Items[1].ID != "unused" || result.Items[1].Requests != 0 || result.Scope != managedCleanupScope {
		t.Fatalf("empty account must have zero requests: %+v", result)
	}
	for _, invalid := range []string{`{"ids":[],"from":1,"to":2}`, `{"ids":["a"],"from":2,"to":1}`, `{"ids":["a"],"from":1,"to":99999999}`} {
		if w := call(invalid, true); w.Code != 400 {
			t.Fatal("invalid batch accepted", w.Code)
		}
	}
	q := managedQuery(t, "")
	q.AccountIDs = []string{"account-a", "unused"}
	where, args := q.whereSQL()
	if strings.Contains(where, "account-a") || len(args) != 1 {
		t.Fatal("batch IDs must be bound parameters")
	}
	var exported bytes.Buffer
	if _, err := p.Metrics.exportManagedLogsWithPrivacy(httptest.NewRequest("GET", "/", nil).Context(), q, "jsonl", &exported, true); err != nil || strings.Contains(exported.String(), "estimated_cost") {
		t.Fatal("export must contain usage only", err)
	}
}
