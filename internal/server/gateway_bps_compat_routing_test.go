package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBPSCompatibilityRoutesMissingModelAndEffortBeforeGeneration(t *testing.T) {
	catalogCalls, generationCalls := 0, 0
	var sent []map[string]json.RawMessage
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			catalogCalls++
			t.Error("static routing unexpectedly queried the upstream catalog")
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/responses" {
			t.Errorf("unexpected static route dispatch: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		generationCalls++
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Fatal("invalid generated request")
		}
		sent = append(sent, body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"route-%d\",\"model\":%s,\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":100,\"output_tokens\":1}}}\n\n", generationCalls, body["model"])
	})
	for index, tc := range []struct{ requested, effort, model, sentEffort string }{
		{"gpt-5.5", "max", "gpt-5.6-sol", "xhigh"},
		{"codex-auto-review", "ultra", "gpt-6-astra", "xhigh"},
		{"gpt-6-astra", "none", "gpt-6-astra", "low"},
	} {
		w := httptest.NewRecorder()
		r := proxyRequest(http.MethodPost, "/v1/responses", fmt.Sprintf(`{"model":%q,"reasoning":{"effort":%q},"stream":false,"input":"check"}`, tc.requested, tc.effort))
		p.gatewayBPS(w, r)
		if w.Code != 200 {
			t.Fatalf("routed request failed: %s %d %s", tc.requested, w.Code, w.Body)
		}
		var model, effort string
		_ = json.Unmarshal(sent[len(sent)-1]["model"], &model)
		_ = json.Unmarshal(sent[len(sent)-1]["reasoning_effort"], &effort)
		if model != tc.model || effort != tc.sentEffort {
			t.Fatalf("wrong route: %s %s", model, effort)
		}
		if generationCalls != index+1 || catalogCalls != 0 {
			t.Fatal("request probed or tried multiple routing targets")
		}
		var response struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Model != tc.model {
			t.Fatal("response model does not reflect actual target")
		}
		record, ok := p.Metrics.LastFor("test")
		if !ok || record.Model != tc.requested || record.SentModel != tc.model || record.RequestedReasoningEffort != tc.effort || record.SentReasoningEffort != tc.sentEffort || record.ReturnedModel != tc.model {
			t.Fatalf("route observations lost: %+v", record)
		}
	}
	if catalogCalls != 0 || generationCalls != 3 {
		t.Fatalf("static dispatch count mismatch: catalog=%d generation=%d", catalogCalls, generationCalls)
	}
}

func TestBPSCompatibilityStaticSettingsOverridesApplyNextRequestWithoutProbing(t *testing.T) {
	catalogCalls, generationCalls := 0, 0
	var sentModels []string
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			catalogCalls++
			t.Error("static setting override queried remote models")
			w.WriteHeader(502)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/responses" {
			t.Error("unexpected request outside generation")
			w.WriteHeader(500)
			return
		}
		generationCalls++
		var request struct {
			Model  string `json:"model"`
			Effort string `json:"reasoning_effort"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("invalid generated request")
			w.WriteHeader(400)
			return
		}
		sentModels = append(sentModels, request.Model)
		if request.Effort != "xhigh" {
			t.Error("static routing failed max effort compatibility")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"configured-%d\",\"model\":%q,\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":100,\"output_tokens\":1}}}\n\n", generationCalls, request.Model)
	})
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	if settings.BPSModelRoutes != defaultBPSModelRoutes() {
		t.Fatal("new settings did not use fixed built-in routing rules")
	}
	send := func(requested, want string) {
		t.Helper()
		before := generationCalls
		request := proxyRequest(http.MethodPost, "/v1/responses", fmt.Sprintf(`{"model":%q,"reasoning":{"effort":"max"},"stream":false,"input":"check fixed routing"}`, requested))
		response := httptest.NewRecorder()
		p.gatewayBPS(response, request)
		if response.Code != 200 || generationCalls != before+1 || catalogCalls != 0 || sentModels[len(sentModels)-1] != want {
			t.Fatalf("static route %s => %s failed: HTTP=%d POST=%d GET=%d", requested, want, response.Code, generationCalls-before, catalogCalls)
		}
		var result struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Model != want {
			t.Fatal("returned target identity changed")
		}
		record, ok := p.Metrics.LastFor("test")
		if !ok || record.Model != requested || record.SentModel != want || record.ReturnedModel != want || record.RequestedReasoningEffort != "max" || record.SentReasoningEffort != "xhigh" {
			t.Fatal("requested/sent/returned identities not preserved")
		}
	}
	send("gpt-5.6-sol", "gpt-5.6-sol")
	// Both source and target have explicit routes. One lookup must finish at
	// Astra, not recursively follow Astra's separate route back to Sol.
	settings.BPSModelRoutes = "gpt-5.6-sol=gpt-6-astra\ngpt-6-astra=gpt-5.6-sol\n*=gpt-5.6-terra"
	if err = p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	send("gpt-5.6-sol", "gpt-6-astra")
	send("unknown-client-model", "gpt-5.6-terra")
	settings, err = p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.BPSModelRoutes = "gpt-5.6-sol=gpt-6-sol\n*=gpt-5.6-luna"
	if err = p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	send("gpt-5.6-sol", "gpt-6-sol")
	send("unknown-client-model", "gpt-5.6-luna")
	if generationCalls != 5 || catalogCalls != 0 {
		t.Fatal("settings updates caused catalog probes or extra generations")
	}
}

func TestBPSCompatibilityStaticUpstreamRejectionDoesNotTryCatalogOrAnotherModel(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusUnprocessableEntity} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			gets, posts := 0, 0
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					gets++
					t.Error("rejected static target triggered dynamic catalog probe")
					w.WriteHeader(502)
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/responses" {
					t.Error("unexpected retry endpoint")
					w.WriteHeader(500)
					return
				}
				posts++
				data, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(data), `"model":"gpt-6-astra"`) {
					t.Error("request did not use fixed wildcard target")
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"code":"unsupported_parameter","message":"synthetic target rejection"}}`)
			})
			request := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"unknown-client-model","reasoning":{"effort":"max"},"stream":false,"input":"no probing"}`)
			response := httptest.NewRecorder()
			p.gatewayBPS(response, request)
			if response.Code != status || posts != 1 || gets != 0 {
				t.Fatalf("static rejection retried/probed: HTTP=%d POST=%d GET=%d", response.Code, posts, gets)
			}
		})
	}
}
