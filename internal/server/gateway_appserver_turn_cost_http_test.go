package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func appServerTurnCostHTTPFixture(t *testing.T) *Proxy {
	p := appServerAuxiliaryTestProxy(t)
	p.AppServerCLI.turnCosts = newAppServerTurnCostLedger(nil)
	return p
}

func appServerTurnCostQuery(t *testing.T, p *Proxy, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := proxyRequest("POST", "/v1/analytics/codex/turn-costs", body)
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	p.gatewayAppServerTurnCosts(w, r.WithContext(appServerRealtimeTestContext(r.Context())))
	return w
}

func TestAppServerTurnCostHTTPGroupsOwnersWithoutCrossAccountMixing(t *testing.T) {
	p := appServerTurnCostHTTPFixture(t)
	raw, _ := json.Marshal(map[string]any{"access_token": "second-token", "chatgpt_account_id": "second-account", "expires_at": time.Now().Add(time.Hour).Unix()})
	if err := p.Store.ImportWith("second", raw, AccountDefaults{Concurrency: 1}); err != nil {
		t.Fatal(err)
	}
	first, _ := p.Store.Get("test")
	second, _ := p.Store.Get("second")
	for turn, account := range map[string]Account{"client-first": first, "client-second": second} {
		mapping := appServerTurnCostMapping{Owner: gatewayAccountRef(account), ThreadID: "same-upstream-thread", TurnID: "same-upstream-turn", ResponseIDs: []string{"same-response"}}
		if err := p.AppServerCLI.turnCosts.Record(t.Context(), "admin", turn, mapping); err != nil {
			t.Fatal(err)
		}
	}
	calls := map[string]int{}
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		auth := r.Header.Get("Authorization")
		calls[auth]++
		if r.URL.String() != "https://chatgpt.com/backend-api/wham/usage/thread-estimates/query" {
			t.Errorf("wrong cost URL %s", r.URL)
		}
		body, _ := io.ReadAll(r.Body)
		var query map[string]any
		_ = json.Unmarshal(body, &query)
		want := map[string]any{"threads": []any{map[string]any{"thread_id": "same-upstream-thread", "turn_ids": []any{"same-upstream-turn"}}}, "include_settled_response_ids": true}
		if !reflect.DeepEqual(query, want) {
			t.Errorf("foreign/client IDs entered upstream query %s", body)
		}
		micros, model := 111001, "owner-first"
		if auth == "Bearer second-token" {
			micros, model = 222002, "owner-second"
			if r.Header.Get("ChatGPT-Account-Id") != "second-account" {
				t.Error("wrong second owner")
			}
		} else if auth != "Bearer test-upstream-token" {
			t.Error("wrong first owner")
		}
		response, _ := json.Marshal(map[string]any{"threads": []any{map[string]any{"thread_id": "same-upstream-thread", "turns": []any{map[string]any{"turn_id": "same-upstream-turn", "model": model, "estimated_usage_usd_micros": micros, "settled_response_ids": []string{"same-response"}}}}}})
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(response)))}, nil
	})}
	w := appServerTurnCostQuery(t, p, "test-client-key", `{"turn_ids":["client-second","unknown","client-first"],"owner":"injected-owner","thread_id":"injected-thread"}`)
	if w.Code != 200 {
		t.Fatalf("query failed %d %s", w.Code, w.Body)
	}
	var result struct {
		Turns []appServerProjectedTurnCost `json:"turns"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if len(result.Turns) != 3 || result.Turns[0].TotalUSD == nil || *result.Turns[0].TotalUSD != "0.222002" || result.Turns[1].Status != "pending" || result.Turns[2].TotalUSD == nil || *result.Turns[2].TotalUSD != "0.111001" {
		t.Fatalf("owner projection mixed %s", w.Body)
	}
	if len(calls) != 2 || calls["Bearer test-upstream-token"] != 1 || calls["Bearer second-token"] != 1 {
		t.Fatalf("incorrect owner dispatch %v", calls)
	}
}

func TestAppServerTurnCostHTTPUnknownProbeAndCrossKeyStayPending(t *testing.T) {
	p := appServerTurnCostHTTPFixture(t)
	account, _ := p.Store.Get("test")
	if err := p.AppServerCLI.turnCosts.Record(t.Context(), "admin", "private-turn", appServerTurnCostMapping{Owner: gatewayAccountRef(account), ThreadID: "private-thread", TurnID: "private-upstream-turn", ResponseIDs: []string{"private-response"}}); err != nil {
		t.Fatal(err)
	}
	key, err := p.Keys.Create("other", nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		t.Error("unknown key/turn triggered upstream query")
		return nil, io.EOF
	})}
	for _, identity := range []string{key.Key, "test-client-key"} {
		body := `{"turn_ids":["unknown-probe"]}`
		if identity == key.Key {
			body = `{"turn_ids":["private-turn"]}`
		}
		w := appServerTurnCostQuery(t, p, identity, body)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"pending"`) || strings.Contains(w.Body.String(), "private-thread") {
			t.Fatalf("unknown became priced/leaked %d %s", w.Code, w.Body)
		}
	}
	w := appServerTurnCostQuery(t, p, "bad", `{"turn_ids":[]}`)
	if w.Code != 401 {
		t.Fatal("invalid key admitted")
	}
}

func TestAppServerTurnCostHTTP401DoesNotSwitchExistingOwner(t *testing.T) {
	p := appServerTurnCostHTTPFixture(t)
	account, _ := p.Store.Get("test")
	_ = p.AppServerCLI.turnCosts.Record(t.Context(), "admin", "turn", appServerTurnCostMapping{Owner: gatewayAccountRef(account), ThreadID: "private-thread", TurnID: "upstream-turn", ResponseIDs: []string{"r"}})
	_ = p.Store.ImportWith("alternate", []byte(`{"access_token":"alternate","expires_at":4102444800}`), AccountDefaults{Concurrency: 1})
	calls := 0
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer test-upstream-token" {
			t.Error("cost query changed owner")
		}
		return &http.Response{StatusCode: 401, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":"private-thread internal detail"}`))}, nil
	})}
	w := appServerTurnCostQuery(t, p, "test-client-key", `{"turn_ids":["turn"]}`)
	if w.Code != 401 || calls != 1 || strings.Contains(w.Body.String(), "private-thread") {
		t.Fatalf("existing billing owner replay/leak %d %d %s", w.Code, calls, w.Body)
	}
}

func TestAppServerTurnCostHTTPConflictingTurnNeverPricesOldMapping(t *testing.T) {
	p := appServerTurnCostHTTPFixture(t)
	account, _ := p.Store.Get("test")
	mapping := appServerTurnCostMapping{Owner: gatewayAccountRef(account), ThreadID: "old-thread", TurnID: "old-turn", ResponseIDs: []string{"old-response"}}
	if err := p.AppServerCLI.turnCosts.Record(t.Context(), "admin", "reused-client-turn", mapping); err != nil {
		t.Fatal(err)
	}
	mapping.ThreadID = "new-thread"
	if err := p.AppServerCLI.turnCosts.Record(t.Context(), "admin", "reused-client-turn", mapping); err == nil {
		t.Fatal("conflict not detected")
	}
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		t.Error("ambiguous turn queried stale owner billing")
		return nil, io.EOF
	})}
	w := appServerTurnCostQuery(t, p, "test-client-key", `{"turn_ids":["reused-client-turn"]}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"pending"`) || !strings.Contains(w.Body.String(), `"total_usd":null`) {
		t.Fatalf("conflicting turn priced %d %s", w.Code, w.Body)
	}
}
