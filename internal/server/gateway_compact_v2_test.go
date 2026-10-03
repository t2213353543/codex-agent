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

func TestGatewayCompactV2PreservesOpaqueContextAndFollowup(t *testing.T) {
	const opaque = "synthetic-encrypted-summary+/="
	requests := 0
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/backend-api/codex/responses" || r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("unexpected upstream route: %s accept=%s", r.URL.Path, r.Header.Get("Accept"))
		}
		var body struct {
			Stream bool             `json:"stream"`
			Store  bool             `json:"store"`
			Input  []map[string]any `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if !body.Stream || body.Store {
			t.Errorf("wrong upstream streaming flags")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if requests == 1 {
			triggers := 0
			for _, item := range body.Input {
				if item["type"] == "compaction_trigger" {
					triggers++
				}
			}
			if triggers != 1 || body.Input[len(body.Input)-1]["type"] != "compaction_trigger" {
				t.Errorf("expected exactly one final compaction trigger: %v", body.Input)
			}
			fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"compaction\",\"id\":\"cmp_test\",\"encrypted_content\":%q}}\n\n", opaque)
			io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_compact\",\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":100,\"output_tokens\":10}}}\n\n")
		} else {
			if len(body.Input) != 4 || body.Input[2]["encrypted_content"] != opaque {
				t.Errorf("replacement history lost opaque item: %v", body.Input)
			}
			for _, item := range body.Input {
				if item["type"] == "compaction_trigger" || item["type"] == "function_call" {
					t.Errorf("non-durable control/tool leaked into followup")
				}
			}
			io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_after\",\"status\":\"completed\",\"output\":[]}}\n\n")
		}
	})
	p.BaseURL += "/backend-api/codex"
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest(http.MethodPost, "/v1/responses/compact", `{"model":"test","input":[{"role":"user","content":"Remember the release code."},{"role":"developer","content":"Keep the release code private."},{"type":"function_call","name":"remember","call_id":"c1","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"saved"},{"type":"compaction_trigger"}]}`))
	if w.Code != 200 {
		t.Fatalf("compact failed: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		Object string            `json:"object"`
		Output []json.RawMessage `json:"output"`
		Usage  map[string]int    `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Object != "response.compaction" || len(result.Output) != 3 || result.Usage["input_tokens"] != 100 {
		t.Fatalf("incorrect compact result: %s", w.Body.String())
	}
	result.Output = append(result.Output, json.RawMessage(`{"role":"user","content":"Continue."}`))
	body, _ := json.Marshal(map[string]any{"model": "test", "input": result.Output})
	w = httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest(http.MethodPost, "/v1/responses", string(body)))
	if w.Code != 200 || requests != 2 {
		t.Fatalf("followup failed: %d %s requests=%d", w.Code, w.Body.String(), requests)
	}
}

func TestGatewayCompactV2RejectsMissingSummaryAndPreservesFailure(t *testing.T) {
	for _, tc := range []struct{ name, event, want string }{
		{"missing_summary", `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[]}]}}`, "invalid upstream compaction response"},
		{"incomplete", `{"type":"response.incomplete","response":{"status":"incomplete","output":[{"type":"compaction","encrypted_content":"partial"}]}}`, "invalid upstream compaction response"},
		{"failure", `{"type":"error","code":"invalid_encrypted_content","message":"The provided summary is invalid"}`, "invalid_encrypted_content"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\n", tc.event)
			})
			p.BaseURL += "/backend-api/codex"
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest(http.MethodPost, "/v1/responses/compact", `{"model":"test","input":"Remember this."}`))
			if w.Code != 502 || !strings.Contains(w.Body.String(), tc.want) || calls != 1 {
				t.Fatalf("code=%d body=%s calls=%d", w.Code, w.Body.String(), calls)
			}
		})
	}
}

func TestGatewayCompactV2BoundsRetainedHistory(t *testing.T) {
	for _, input := range []string{`{"model":"test"}`, `{"model":"test","input":null}`} {
		if _, err := prepareResponsesCompaction([]byte(input)); err == nil {
			t.Fatal("missing/null input accepted")
		}
	}
	history := []any{map[string]any{"role": "user", "content": strings.Repeat("old long document ", 20000)}, map[string]any{"role": "user", "content": "Keep the latest instruction."}}
	raw, _ := json.Marshal(map[string]any{"model": "test", "input": history})
	prepared, err := prepareResponsesCompaction(raw)
	if err != nil {
		t.Fatal(err)
	}
	result, err := responsesCompactionResult(prepared, []byte(`{"status":"completed","output":[{"type":"compaction","encrypted_content":"opaque"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(result) > 256000 || !strings.Contains(string(result), "latest instruction") || strings.Contains(string(result), "old long document") {
		t.Fatalf("retained context budget failed: bytes=%d", len(result))
	}
}
