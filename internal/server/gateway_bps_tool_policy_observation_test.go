package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

type rootGatewayToolPolicyView struct {
	EffectiveChoice                 string `json:"effective_choice"`
	CatalogAtPrepareCount           int    `json:"catalog_at_prepare_count"`
	NamedNamespacesAtPrepareCount   int    `json:"named_namespaces_at_prepare_count"`
	ToolSearchAtPrepare             bool   `json:"tool_search_at_prepare"`
	Structured                      bool   `json:"structured"`
	CatalogAtRejectionCount         *int   `json:"catalog_at_rejection_count"`
	NamedNamespacesAtRejectionCount *int   `json:"named_namespaces_at_rejection_count"`
	RejectionPhase                  string `json:"rejection_phase"`
	RejectedFamily                  string `json:"rejected_family"`
	RejectedIdentity                string `json:"rejected_identity"`
	RejectedMatchesExactDeclaration *bool  `json:"rejected_matches_exact_declaration"`
}

func rootGatewayToolPolicyRead(t *testing.T, record RequestRecord) (rootGatewayToolPolicyView, json.RawMessage) {
	t.Helper()
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	encoded := fields["tool_policy"]
	var view rootGatewayToolPolicyView
	if os.Getenv("ROOT_TOOL_POLICY_BASELINE") == "1" {
		if len(encoded) != 0 {
			t.Fatal("baseline unexpectedly contains tool metadata")
		}
		return view, nil
	}
	if len(encoded) == 0 || bytes.Equal(encoded, []byte("null")) || json.Unmarshal(encoded, &view) != nil {
		t.Fatal("gateway did not attach observed policy metadata")
	}
	for _, secret := range []string{"PRIVATE_TOOL", "PRIVATE_ARGUMENT", "PRIVATE_NAMESPACE", "PRIVATE_PROMPT", "PRIVATE_SCHEMA"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("tool policy stores private payload: %s", secret)
		}
	}
	return view, encoded
}

func rootGatewayToolPolicyRun(p *Proxy, w http.ResponseWriter, r *http.Request) (aborted bool) {
	defer func() {
		if value := recover(); value != nil {
			if value != http.ErrAbortHandler {
				panic(value)
			}
			aborted = true
		}
	}()
	p.gatewayBPS(w, r)
	return false
}

func rootGatewayToolPolicyCall(name, namespace, call string) map[string]any {
	item := map[string]any{"type": "function_call", "id": "fc_" + call, "call_id": call, "name": name, "arguments": "{\"value\":\"PRIVATE_ARGUMENT\"}", "status": "completed"}
	if namespace != "" {
		item["namespace"] = namespace
	}
	return item
}

func TestBPSGatewayToolPolicyRejectionMetadata(t *testing.T) {
	function := func(name, namespace string) map[string]any {
		tool := map[string]any{"type": "function", "name": name, "description": "PRIVATE_SCHEMA", "parameters": map[string]any{"type": "object"}}
		if namespace != "" {
			tool["namespace"] = namespace
		}
		return tool
	}
	for _, tc := range []struct {
		name                                string
		tools                               []any
		choice                              any
		output                              []any
		parallel                            *bool
		phase, identity, family, choiceName string
		count, namespaces                   int
		exact                               *bool
	}{
		{name: "undeclared_zero_matches", tools: []any{}, choice: "auto", output: []any{rootGatewayToolPolicyCall("PRIVATE_TOOL_UNDECLARED", "", "missing")}, phase: "normalize_item", identity: "undeclared", family: "function_call", choiceName: "auto", exact: rootGatewayPolicyBool(false)},
		{name: "ambiguous_namespace", tools: []any{function("PRIVATE_TOOL_SAME", "PRIVATE_NAMESPACE_A"), function("PRIVATE_TOOL_SAME", "PRIVATE_NAMESPACE_B")}, choice: "auto", output: []any{rootGatewayToolPolicyCall("PRIVATE_TOOL_SAME", "", "ambiguous")}, phase: "normalize_item", identity: "ambiguous_namespace", family: "function_call", choiceName: "auto", count: 2, namespaces: 2, exact: rootGatewayPolicyBool(false)},
		{name: "forced_after_namespace_restore", tools: []any{function("PRIVATE_TOOL_SELECTED", "PRIVATE_NAMESPACE_A"), function("PRIVATE_TOOL_OTHER", "PRIVATE_NAMESPACE_B")}, choice: map[string]string{"type": "function", "name": "PRIVATE_TOOL_SELECTED", "namespace": "PRIVATE_NAMESPACE_A"}, output: []any{rootGatewayToolPolicyCall("PRIVATE_TOOL_OTHER", "", "forced")}, phase: "normalize_item", identity: "unique_namespace", family: "function_call", choiceName: "forced", count: 2, namespaces: 2, exact: rootGatewayPolicyBool(false)},
		{name: "none", tools: []any{function("PRIVATE_TOOL_DECLARED", "")}, choice: "none", output: []any{rootGatewayToolPolicyCall("PRIVATE_TOOL_DECLARED", "", "none")}, phase: "normalize_item", identity: "exact", family: "function_call", choiceName: "none", count: 1, exact: rootGatewayPolicyBool(true)},
		{name: "parallel", tools: []any{function("PRIVATE_TOOL_DECLARED", "")}, choice: "auto", parallel: rootGatewayPolicyBool(false), output: []any{rootGatewayToolPolicyCall("PRIVATE_TOOL_DECLARED", "", "first"), rootGatewayToolPolicyCall("PRIVATE_TOOL_DECLARED", "", "second")}, phase: "parallel_policy", identity: "exact", family: "function_call", choiceName: "auto", count: 1, exact: rootGatewayPolicyBool(true)},
		{name: "required_missing", tools: []any{function("PRIVATE_TOOL_DECLARED", "")}, choice: "required", output: []any{}, phase: "validate_response", choiceName: "required", count: 1},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				var generations atomic.Int32
				p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
					generations.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					terminal, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_policy_fixture", "model": "gpt-6-astra", "status": "completed", "output": tc.output, "usage": map[string]any{"input_tokens": 100, "output_tokens": 10, "total_tokens": 110, "input_tokens_details": map[string]any{"cached_tokens": 80, "cache_write_tokens": 7}}}})
					_, _ = fmt.Fprintf(w, "data: %s\n\n", terminal)
				})
				requestBody := map[string]any{"model": "gpt-6-astra", "stream": stream, "tools": tc.tools, "tool_choice": tc.choice, "input": "PRIVATE_PROMPT"}
				if tc.parallel != nil {
					requestBody["parallel_tool_calls"] = *tc.parallel
				}
				encoded, _ := json.Marshal(requestBody)
				w := httptest.NewRecorder()
				aborted := rootGatewayToolPolicyRun(p, w, proxyRequest(http.MethodPost, "/v1/responses", string(encoded)))
				record, ok := p.Metrics.LastFor("test")
				if !ok || generations.Load() != 1 || record.Status != "error" {
					t.Fatalf("failure semantics changed: record=%t generation=%d status=%s", ok, generations.Load(), record.Status)
				}
				if record.ObservationSchemaVersion != 2 || len(record.Attempts) != 1 || record.Attempts[0].ObservedHTTPStatusCode == nil || *record.Attempts[0].ObservedHTTPStatusCode != 200 {
					t.Fatal("actual HTTP/schema2 observation regressed")
				}
				if bytes.Contains(w.Body.Bytes(), []byte(`"type":"response.completed"`)) {
					t.Fatal("invalid response was delivered as completed")
				}
				if !stream && w.Code != 502 {
					t.Fatalf("JSON policy rejection status=%d", w.Code)
				}
				view, rawPolicy := rootGatewayToolPolicyRead(t, record)
				if rawPolicy != nil {
					if view.EffectiveChoice != tc.choiceName || view.CatalogAtPrepareCount != tc.count || view.NamedNamespacesAtPrepareCount != tc.namespaces || view.Structured || view.RejectionPhase != tc.phase || view.RejectedIdentity != tc.identity || view.RejectedFamily != tc.family {
						t.Fatalf("policy metadata incorrect: %+v", view)
					}
					if view.CatalogAtRejectionCount == nil || *view.CatalogAtRejectionCount != tc.count || view.NamedNamespacesAtRejectionCount == nil || *view.NamedNamespacesAtRejectionCount != tc.namespaces {
						t.Fatal("rejection zero/count observation lost")
					}
					if tc.exact == nil {
						if view.RejectedMatchesExactDeclaration != nil {
							t.Fatal("whole response rejection invented exact item match")
						}
					} else if view.RejectedMatchesExactDeclaration == nil || *view.RejectedMatchesExactDeclaration != *tc.exact {
						t.Fatal("pre-normalize identity was not captured")
					}
				}
				if tc.phase == "validate_response" && (record.InputTokens != 100 || record.OutputTokens != 10 || record.CachedTokens != 80 || record.CacheWriteTokens == nil || *record.CacheWriteTokens != 7) {
					t.Fatal("whole-response policy failure lost real terminal usage")
				}
				contract, _ := json.Marshal(map[string]any{"case": tc.name, "stream": stream, "http": w.Code, "aborted": aborted, "status": record.Status, "error": record.Error, "input": record.InputTokens, "output": record.OutputTokens, "cache_read": record.CachedTokens, "cache_write": record.CacheWriteTokens, "usage_known": record.UsageKnown, "source": record.ErrorSource, "phase": record.ErrorPhase, "attempts": len(record.Attempts), "schema": record.ObservationSchemaVersion})
				t.Log("POLICY_CONTRACT " + string(contract))
			})
		}
	}
}

func rootGatewayPolicyBool(value bool) *bool { return &value }

func TestBPSGatewayToolPolicySuccessZeroAndStructuredObservation(t *testing.T) {
	for _, structured := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("structured=%t/stream=%t", structured, stream), func(t *testing.T) {
				p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
					var request struct{ Input []map[string]json.RawMessage }
					_ = json.NewDecoder(r.Body).Decode(&request)
					output := []any{}
					if structured {
						toolName := ""
						for _, item := range request.Input {
							var kind string
							_ = json.Unmarshal(item["type"], &kind)
							if kind != "additional_tools" {
								continue
							}
							var tools []struct{ Name string }
							_ = json.Unmarshal(item["tools"], &tools)
							for _, tool := range tools {
								if strings.HasPrefix(tool.Name, "emit_structured_") {
									toolName = tool.Name
								}
							}
						}
						if toolName == "" {
							t.Error("formatter declaration missing")
						}
						output = append(output, map[string]any{"type": "function_call", "id": "fc_formatter", "call_id": "call_formatter", "name": toolName, "arguments": "{\"answer\":\"OK\"}", "status": "completed"})
					}
					terminal, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_policy_success", "model": "gpt-6-astra", "status": "completed", "output": output, "usage": map[string]any{"input_tokens": 100, "output_tokens": 10, "total_tokens": 110, "input_tokens_details": map[string]any{"cached_tokens": 80, "cache_write_tokens": 7}}}})
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: %s\n\n", terminal)
				})
				body := map[string]any{"model": "gpt-6-astra", "stream": stream, "tools": []any{}, "tool_choice": "auto", "input": "PRIVATE_PROMPT"}
				if structured {
					body["text"] = map[string]any{"format": map[string]string{"type": "json_object"}}
				}
				encoded, _ := json.Marshal(body)
				w := httptest.NewRecorder()
				aborted := rootGatewayToolPolicyRun(p, w, proxyRequest(http.MethodPost, "/v1/responses", string(encoded)))
				record, ok := p.Metrics.LastFor("test")
				if !ok || aborted || w.Code != 200 || record.Status != "ok" || record.InputTokens != 100 || record.OutputTokens != 10 || record.CachedTokens != 80 || record.CacheWriteTokens == nil || *record.CacheWriteTokens != 7 || record.ObservationSchemaVersion != 2 {
					t.Fatal("successful generation/usage/schema regressed")
				}
				view, rawPolicy := rootGatewayToolPolicyRead(t, record)
				if rawPolicy != nil {
					if view.EffectiveChoice != "auto" || view.CatalogAtPrepareCount != 0 || view.NamedNamespacesAtPrepareCount != 0 || view.Structured != structured || view.RejectionPhase != "" || view.CatalogAtRejectionCount != nil {
						t.Fatalf("observed zero/success formatter metadata wrong: %+v", view)
					}
					for _, exact := range []string{`"catalog_at_prepare_count":0`, `"named_namespaces_at_prepare_count":0`, `"tool_search_at_prepare":false`} {
						if !bytes.Contains(rawPolicy, []byte(exact)) {
							t.Fatal("known zero/false omitted")
						}
					}
				}
				contract, _ := json.Marshal(map[string]any{"case": "success", "structured": structured, "stream": stream, "http": w.Code, "aborted": aborted, "status": record.Status, "input": record.InputTokens, "output": record.OutputTokens, "cache_read": record.CachedTokens, "cache_write": record.CacheWriteTokens, "schema": record.ObservationSchemaVersion})
				t.Log("POLICY_CONTRACT " + string(contract))
			})
		}
	}
}

func TestBPSGatewayToolPolicyLegacyUnknownRemainsOmitted(t *testing.T) {
	raw, err := json.Marshal(RequestRecord{Status: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"tool_policy"`)) {
		t.Fatal("old unobserved record invents policy metadata")
	}
}
