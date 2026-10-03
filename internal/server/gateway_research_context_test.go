package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// CLI 0.155 consumes these from output_item.done. Completed only supplies
// response identity/usage/end_turn and cannot erase previously received history.
var researchContextDoneItems = []string{
	`{"id":"rs_context","type":"reasoning","encrypted_content":"opaque-context+/=","summary":[],"future":{"integer":9007199254740993,"empty":null}}`,
	`{"id":"fc_context","type":"function_call","call_id":"call_context","name":"inspect","arguments":"{\"key\":\"value\"}","encrypted_function_args":["opaque-args+/="],"namespace":"functions"}`,
	`{"id":"msg_context","type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Inspecting the saved state."}],"future":null}`,
}

var researchContextSlimItems = []string{
	`{"id":"rs_context","type":"reasoning"}`,
	`{"id":"fc_context","type":"function_call","call_id":"call_context","name":"inspect","arguments":"{\"key\":\"value\"}"}`,
	`{"id":"msg_context","type":"message","role":"assistant","content":[{"type":"output_text","text":"Inspecting the saved state."}]}`,
}

func researchContextWire(items, terminalItems []string) string {
	var wire strings.Builder
	for index, item := range items {
		wire.WriteString(outputFixtureEvent(int64(index), item))
	}
	fmt.Fprintf(&wire, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_context\",\"status\":\"completed\",\"output\":[%s]}}\n\n", strings.Join(terminalItems, ","))
	return wire.String()
}

func researchContextOutput(t *testing.T, raw []byte) []json.RawMessage {
	t.Helper()
	var result struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result.Output
}

func TestResearchContextHTTPReplayRetainsDoneFieldsMissingFromTerminal(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		stream   bool
		terminal []string
	}{
		{"native_sse_control", true, researchContextSlimItems},
		{"json_all_terminal_ids", false, researchContextSlimItems},
		{"json_terminal_subset", false, researchContextSlimItems[2:]},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			wire := researchContextWire(researchContextDoneItems, scenario.terminal)
			var requests atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if requests.Add(1) == 1 {
					_, _ = io.WriteString(w, wire)
					return
				}
				var received struct {
					Input []json.RawMessage `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
					t.Error(err)
					return
				}
				if len(received.Input) != len(researchContextDoneItems)+2 {
					t.Errorf("followup lost history items: %d", len(received.Input))
				} else {
					for index, expected := range researchContextDoneItems {
						q01AssertQualityFields(t, received.Input[index], []byte(expected))
					}
				}
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_followup\",\"status\":\"completed\",\"output\":[]}}\n\n")
			})
			settings := defaultSettings()
			settings.BuiltinPromptEnabled = false
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			first := proxyRequest(http.MethodPost, "/responses", fmt.Sprintf(`{"model":"fixture","stream":%t,"input":[]}`, scenario.stream))
			first.Header.Set("Session-Id", "context-replay-thread")
			firstResponse := httptest.NewRecorder()
			p.ServeHTTP(firstResponse, first)
			if firstResponse.Code != http.StatusOK {
				t.Fatalf("first request failed: %d %s", firstResponse.Code, firstResponse.Body.String())
			}
			var history []json.RawMessage
			if scenario.stream {
				if firstResponse.Body.String() != wire {
					t.Fatal("gateway rewrote the SSE control")
				}
				// Follow the CLI contract: history comes from done items, not
				// response.completed.output. Do not use the gateway collector here.
				scanner := bufio.NewScanner(strings.NewReader(firstResponse.Body.String()))
				for scanner.Scan() {
					var event struct {
						Type string          `json:"type"`
						Item json.RawMessage `json:"item"`
					}
					if strings.HasPrefix(scanner.Text(), "data: ") && json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "data: ")), &event) == nil && event.Type == "response.output_item.done" {
						history = append(history, event.Item)
					}
				}
				if err := scanner.Err(); err != nil {
					t.Fatal(err)
				}
			} else {
				history = researchContextOutput(t, firstResponse.Body.Bytes())
			}
			history = append(history, json.RawMessage(`{"type":"function_call_output","call_id":"call_context","output":"saved result"}`), json.RawMessage(`{"role":"user","content":[{"type":"input_text","text":"Continue using the saved state."}]}`))
			raw, err := json.Marshal(map[string]any{"model": "fixture", "stream": false, "input": history, "reasoning": map[string]string{"effort": "medium", "context": "all_turns"}, "include": []string{"reasoning.encrypted_content"}})
			if err != nil {
				t.Fatal(err)
			}
			second := proxyRequest(http.MethodPost, "/responses", string(raw))
			second.Header.Set("Session-Id", "context-replay-thread")
			secondResponse := httptest.NewRecorder()
			p.ServeHTTP(secondResponse, second)
			if secondResponse.Code != http.StatusOK || requests.Load() != 2 {
				t.Fatalf("followup failed or replayed: HTTP=%d requests=%d", secondResponse.Code, requests.Load())
			}
		})
	}
}

func TestResearchContextCompactRetainsEncryptedDoneSummary(t *testing.T) {
	const summary = `{"id":"cmp_context","type":"compaction","encrypted_content":"opaque-compacted-context+/=","future":{"integer":9007199254740993}}`
	var requests atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if requests.Add(1) == 1 {
			_, _ = io.WriteString(w, researchContextWire([]string{summary}, []string{`{"id":"cmp_context","type":"compaction"}`}))
			return
		}
		var received struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		} else if len(received.Input) != 2 {
			t.Errorf("compacted followup item count=%d", len(received.Input))
		} else {
			q01AssertQualityFields(t, received.Input[1], []byte(summary))
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_after_compact\",\"status\":\"completed\",\"output\":[]}}\n\n")
	})
	p.BaseURL += "/backend-api/codex"
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses/compact", `{"model":"fixture","input":[{"role":"user","content":"Remember the state."}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("valid done compaction was discarded by its slim terminal: %d %s", w.Code, w.Body.String())
	}
	body, _ := json.Marshal(map[string]any{"model": "fixture", "input": researchContextOutput(t, w.Body.Bytes())})
	followup := httptest.NewRecorder()
	p.ServeHTTP(followup, proxyRequest(http.MethodPost, "/responses", string(body)))
	if followup.Code != http.StatusOK || requests.Load() != 2 {
		t.Fatalf("compacted followup failed: %d requests=%d", followup.Code, requests.Load())
	}
}

func TestResearchContextExplicitTerminalValuesRemainAuthoritative(t *testing.T) {
	for _, scenario := range []struct{ name, done, terminal, want string }{
		{"reasoning_null", `{"id":"x","type":"reasoning","encrypted_content":"old-secret","summary":[{"text":"old"}],"future":{"integer":9007199254740993}}`, `{"id":"x","type":"reasoning","encrypted_content":null,"summary":[]}`, `{"id":"x","type":"reasoning","encrypted_content":null,"summary":[],"future":{"integer":9007199254740993}}`},
		{"tool_empty", `{"id":"x","type":"function_call","call_id":"c","name":"inspect","arguments":"old","encrypted_function_args":["old"],"namespace":"functions"}`, `{"id":"x","type":"function_call","call_id":"c","name":"inspect","arguments":"","encrypted_function_args":[]}`, `{"id":"x","type":"function_call","call_id":"c","name":"inspect","arguments":"","encrypted_function_args":[],"namespace":"functions"}`},
		{"tool_null_namespace", `{"id":"x","type":"function_call","call_id":"c","name":"inspect","arguments":"{}","namespace":"functions"}`, `{"id":"x","type":"function_call","call_id":"c","name":"inspect","namespace":null}`, `{"id":"x","type":"function_call","call_id":"c","name":"inspect","arguments":"{}","namespace":null}`},
		{"message_null_phase", `{"id":"x","type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"old"}],"future":{"keep":true}}`, `{"id":"x","type":"message","phase":null,"content":[],"future":{}}`, `{"id":"x","type":"message","role":"assistant","phase":null,"content":[],"future":{}}`},
		{"opaque_duplicate_members", `{"id":"x","type":"reasoning","encrypted_content":"opaque","future":{"v":1,"v":2,"n":9007199254740993}}`, `{"id":"x","type":"reasoning"}`, `{"id":"x","type":"reasoning","encrypted_content":"opaque","future":{"v":1,"v":2,"n":9007199254740993}}`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			result, err := collectResponse(strings.NewReader(researchContextWire([]string{scenario.done}, []string{scenario.terminal})))
			if err != nil {
				t.Fatal(err)
			}
			items := researchContextOutput(t, result)
			if len(items) != 1 {
				t.Fatalf("unexpected output count: %d", len(items))
			}
			q01AssertQualityFields(t, items[0], []byte(scenario.want))
			if scenario.name == "opaque_duplicate_members" && !bytes.Contains(items[0], []byte(`"v":1,"v":2`)) {
				t.Fatal("opaque nested JSON was decoded and duplicate fields collapsed")
			}
		})
	}
}

func TestResearchContextConflictingItemIdentityCannotReceiveOldFields(t *testing.T) {
	const done = `{"id":"x","type":"function_call","call_id":"original","name":"inspect","namespace":"functions","arguments":"original","encrypted_function_args":["opaque"]}`
	for _, terminal := range []string{
		`{"id":"x","type":"message","content":[]}`,
		`{"id":"x","type":"function_call","call_id":"other","name":"inspect","arguments":"replacement"}`,
		`{"id":"x","type":"function_call","call_id":"original","name":"execute"}`,
		`{"id":"x","type":"function_call","call_id":"original","name":"inspect","namespace":"other"}`,
		`{"id":"x","type":"function_call","call_id":"original","name":"inspect","arguments":"replacement"}`,
	} {
		p := proxyFixture(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, researchContextWire([]string{done}, []string{terminal}))
		})
		w := httptest.NewRecorder()
		p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", `{"model":"fixture","stream":false}`))
		if w.Code != http.StatusBadGateway {
			t.Fatalf("conflicting same-ID terminal was reported as usable history: %d %s", w.Code, w.Body.String())
		}
	}
	_, err := collectResponse(strings.NewReader(researchContextWire(
		[]string{`{"id":"x","type":"message","role":"assistant","phase":"commentary","content":[]}`},
		[]string{`{"id":"x","type":"message","role":"user","content":[]}`})))
	if err == nil {
		t.Fatal("terminal role change inherited completed assistant metadata")
	}
}

func TestResearchContextUnknownTerminalIdentityStillPreventsGuessing(t *testing.T) {
	const done = `{"id":"known","type":"reasoning","encrypted_content":"opaque"}`
	for _, terminal := range [][]string{
		{`{"id":"known","type":"message"}`, `{"id":"unknown","type":"message"}`},
		{`{"id":"unknown","type":"message"}`, `{"id":"known","type":"message"}`},
	} {
		wire := researchContextWire([]string{done}, terminal)
		result, err := collectResponse(strings.NewReader(wire))
		if err != nil {
			t.Fatal(err)
		}
		items := researchContextOutput(t, result)
		if len(items) != len(terminal) {
			t.Fatal("unknown terminal identities must remain authoritative without inferred history")
		}
		for index := range items {
			if string(items[index]) != terminal[index] {
				t.Fatal("unknown terminal identity received guessed context fields")
			}
		}
	}
}

func TestResearchContextRestorationKeepsFullTerminalOrder(t *testing.T) {
	reversed := []string{researchContextSlimItems[2], researchContextSlimItems[1], researchContextSlimItems[0]}
	result, err := collectResponse(strings.NewReader(researchContextWire(researchContextDoneItems, reversed)))
	if err != nil {
		t.Fatal(err)
	}
	items := researchContextOutput(t, result)
	if len(items) != len(researchContextDoneItems) {
		t.Fatalf("unexpected output count: %d", len(items))
	}
	for index := range items {
		q01AssertQualityFields(t, items[index], []byte(researchContextDoneItems[len(items)-1-index]))
	}
}

func TestResearchContextRestoredFieldsRespectFinalBodyLimit(t *testing.T) {
	const itemPrefix = `{"id":"x","type":"reasoning","encrypted_content":"`
	const terminal = `{"id":"resp_context","status":"completed","output":[{"id":"x","type":"reasoning"}]}`
	// The source item and terminal are independently within the limit, but
	// combining restored context with terminal metadata must still be bounded.
	done := itemPrefix + strings.Repeat("x", maxBody/2) + `"}`
	var accumulator responseOutputAccumulator
	if err := accumulator.observe([]byte(`{"type":"response.output_item.done","output_index":0,"item":` + done + `}`)); err != nil {
		t.Fatal(err)
	}
	largeTerminal := strings.Replace(terminal, `"status":"completed"`, `"status":"completed","metadata":"`+strings.Repeat("y", maxBody/2)+`"`, 1)
	if result, err := accumulator.complete(json.RawMessage(largeTerminal)); err == nil || result != nil {
		t.Fatal("restoring same-ID fields bypassed the aggregate response bound")
	}
	result, err := accumulator.complete(json.RawMessage(terminal))
	if err != nil || !bytes.Contains(result, []byte(`"encrypted_content"`)) {
		t.Fatal("bounded completed context was lost after oversized assembly rejection")
	}
}
