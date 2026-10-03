package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAppServerTurnCostGatewayRecordsActualScopedTurn(t *testing.T) {
	p := appServerAuxiliaryTestProxy(t)
	p.AppServerCLI = newAppServerCLI(p)
	defer p.AppServerCLI.Close()
	settings, _ := p.Settings.Get()
	settings.PromptCacheKeyEnabled = true
	settings.BuiltinPromptEnabled = false
	_ = p.Settings.Set(settings)
	upstreamThread, upstreamTurn := "", ""
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		content := "application/json"
		response := ""
		switch r.URL.Path {
		case "/backend-api/codex/responses":
			var valid bool
			upstreamThread, upstreamTurn, valid = appServerCostIDs(body, r.Header)
			if !valid || upstreamThread == "" || upstreamThread == "client-thread" || upstreamTurn != "client-turn" {
				t.Fatalf("account-scoped actual turn not available: %s/%s", upstreamThread, upstreamTurn)
			}
			content = "text/event-stream"
			response = "data: " + `{"type":"response.created","response":{"id":"resp_cost","status":"in_progress"}}` + "\n\n" + "data: " + `{"type":"response.completed","response":{"id":"resp_cost","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"
		case "/backend-api/wham/usage/thread-estimates/query":
			var query struct {
				Threads []struct {
					ThreadID string   `json:"thread_id"`
					TurnIDs  []string `json:"turn_ids"`
				}
				Include bool `json:"include_settled_response_ids"`
			}
			if json.Unmarshal(body, &query) != nil || len(query.Threads) != 1 || query.Threads[0].ThreadID != upstreamThread || len(query.Threads[0].TurnIDs) != 1 || query.Threads[0].TurnIDs[0] != upstreamTurn || !query.Include {
				t.Fatalf("query not based on actual sent metadata: %s", body)
			}
			encoded, _ := json.Marshal(map[string]any{"threads": []any{map[string]any{"thread_id": upstreamThread, "turns": []any{map[string]any{"turn_id": upstreamTurn, "model": "fixture", "estimated_usage_usd_micros": 125001, "settled_response_ids": []string{"resp_cost"}}}}}})
			response = string(encoded)
		default:
			t.Fatalf("unexpected cost route %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{content}}, Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
	})}
	input := `{"model":"fixture","stream":false,"prompt_cache_key":"client-thread","client_metadata":{"thread_id":"client-thread","session_id":"client-thread","turn_id":"client-turn"},"input":[{"type":"additional_tools","role":"developer","tools":[]}]}`
	r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(input))
	r.Header.Set("Authorization", "Bearer test-client-key")
	out := httptest.NewRecorder()
	p.ServeHTTP(out, r)
	if out.Code != 200 {
		t.Fatalf("generation failed %d %s", out.Code, out.Body)
	}
	stored, known, err := p.AppServerCLI.turnCosts.Lookup(t.Context(), "admin", "client-turn")
	if err != nil || !known || stored.ThreadID != upstreamThread || len(stored.ResponseIDs) != 1 || stored.ResponseIDs[0] != "resp_cost" {
		t.Fatalf("completed provenance not saved: %+v %v", stored, err)
	}
	query := httptest.NewRequest("POST", "/v1/analytics/codex/turn-costs", strings.NewReader(`{"turn_ids":["client-turn","unknown-turn"]}`))
	query.Header.Set("Authorization", "Bearer test-client-key")
	out = httptest.NewRecorder()
	p.ServeHTTP(out, query)
	if out.Code != 200 {
		t.Fatalf("price query failed %d %s", out.Code, out.Body)
	}
	var priced struct{ Turns []appServerProjectedTurnCost }
	_ = json.Unmarshal(out.Body.Bytes(), &priced)
	if len(priced.Turns) != 2 || priced.Turns[0].TotalUSD == nil || *priced.Turns[0].TotalUSD != "0.125001" || priced.Turns[1].Status != "pending" || priced.Turns[1].TotalUSD != nil {
		t.Fatalf("wrong settled/unknown prices %s", out.Body)
	}
}
