package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/klauspost/compress/zstd"
)

var researchInferenceControls = []string{
	`"model":"fixture-first","reasoning":{"effort":"medium","summary":"auto"},"include":["reasoning.encrypted_content"],"text":{"verbosity":"low"},"parallel_tool_calls":true,"tool_choice":"auto"`,
	`"model":"fixture-second","reasoning":{"effort":"xhigh","summary":"detailed","context":"all_turns"},"service_tier":"priority","include":["reasoning.encrypted_content"],"text":{"verbosity":"high","format":{"type":"json_schema","name":"result","strict":true,"schema":{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"],"additionalProperties":false}}},"parallel_tool_calls":false,"tool_choice":"auto"`,
}

func researchInferenceRequest(index int, ws bool, large string) []byte {
	kind := `"stream":true,`
	if ws {
		kind = `"type":"response.create",`
	}
	input := `[{"role":"user","content":[{"type":"input_text","text":"Read this source and explain it."}]}]`
	previous := ""
	if index == 1 {
		previous = `,"previous_response_id":"controls-first"`
		input = `[{"type":"reasoning","id":"rs_controls","encrypted_content":"opaque+/=","summary":[]},{"type":"function_call","call_id":"call_controls","name":"inspect","arguments":"{}","encrypted_function_args":["opaque-args"]},{"type":"function_call_output","call_id":"call_controls","output":"` + large + `"},{"role":"user","content":[{"type":"input_text","text":"Continue with the saved tool result."}]}]`
	}
	return []byte(`{` + kind + researchInferenceControls[index] + `,"instructions":"Use the original task and tool history.","store":false,"input":` + input + previous + `,"tools":[{"type":"function","name":"inspect","parameters":{"type":"object","properties":{}}}],"prompt_cache_key":"controls-session","client_metadata":{"session_id":"controls-session","thread_id":"controls-thread"}}`)
}

func researchInferenceAssert(t *testing.T, actual, expected []byte, large string, cacheScoped bool) {
	t.Helper()
	var got, want map[string]json.RawMessage
	if json.Unmarshal(actual, &got) != nil || json.Unmarshal(expected, &want) != nil {
		t.Fatal("invalid request JSON")
	}
	if cacheScoped {
		want["client_metadata"] = json.RawMessage(`{"session_id":"` + gatewayFixtureScopedValue("session", "controls-session") + `","thread_id":"` + gatewayFixtureScopedValue("thread", "controls-thread") + `"}`)
		want["prompt_cache_key"] = json.RawMessage(`"` + gatewayFixtureScopedValue("session", "controls-session") + `"`)
	}
	for _, field := range []string{"model", "reasoning", "service_tier", "include", "text", "parallel_tool_calls", "tool_choice", "previous_response_id", "tools", "client_metadata", "prompt_cache_key"} {
		if !bytes.Equal(got[field], want[field]) {
			t.Errorf("inference control %s changed: got=%s want=%s", field, got[field], want[field])
		}
	}
	if large != "" {
		var items []struct {
			Type   string `json:"type"`
			Output string `json:"output"`
		}
		if json.Unmarshal(got["input"], &items) != nil || len(items) != 4 || items[2].Type != "function_call_output" || sha256.Sum256([]byte(items[2].Output)) != sha256.Sum256([]byte(large)) {
			t.Error("full large tool result was truncated or transformed")
		}
	}
}

func TestResearchInferenceControlsHTTPNativeAndZstdTwoTurns(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, compressed := range []bool{false, true} {
			t.Run(fmt.Sprintf("native=%t/zstd=%t", native, compressed), func(t *testing.T) {
				var calls atomic.Int32
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "Bearer test-upstream-token" || r.Header.Get("ChatGPT-Account-Id") != "test-account" {
						t.Error("JSON encoding change altered request media type or account identity")
					}
					index := int(calls.Add(1)) - 1
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					if compressed {
						decoder, e := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
						if e != nil {
							t.Error(e)
							return
						}
						defer decoder.Close()
						raw, err = decoder.DecodeAll(raw, nil)
						if err != nil {
							t.Error(err)
							return
						}
						if r.Header.Get("Content-Encoding") != "zstd" {
							t.Error("native CLI compression choice was lost")
						}
					}
					if index > 1 {
						t.Error("request was replayed")
						return
					}
					researchInferenceAssert(t, raw, researchInferenceRequest(index, false, "<tag>&value</tag>"), "", true)
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"controls-%s\",\"status\":\"completed\",\"output\":[]}}\n\n", []string{"first", "second"}[index])
				})
				if native {
					realGatewayNativePool(t, p)
				}
				settings := defaultSettings()
				settings.BuiltinPromptEnabled = false
				if err := p.Settings.Set(settings); err != nil {
					t.Fatal(err)
				}
				for index := 0; index < 2; index++ {
					body := researchInferenceRequest(index, false, "<tag>&value</tag>")
					if compressed {
						encoder, err := zstd.NewWriter(nil)
						if err != nil {
							t.Fatal(err)
						}
						body = encoder.EncodeAll(body, nil)
						encoder.Close()
					}
					r := proxyRequest(http.MethodPost, "/responses", string(body))
					r.Header.Set("Session-Id", "controls-session")
					if compressed {
						r.Header.Set("Content-Encoding", "zstd")
					}
					w := httptest.NewRecorder()
					p.ServeHTTP(w, r)
					if w.Code != http.StatusOK {
						t.Fatalf("turn%d HTTP%d: %s", index, w.Code, w.Body.String())
					}
				}
				if calls.Load() != 2 {
					t.Fatal("expected exactly two upstream requests")
				}
			})
		}
	}
}

func TestResearchInferenceControlsMarkupHistoryBelowBodyLimit(t *testing.T) {
	runResearchInferenceMarkupHTTP(t, false)
}

func TestResearchInferenceControlsNativeMarkupHistory(t *testing.T) {
	runResearchInferenceMarkupHTTP(t, true)
}

func runResearchInferenceMarkupHTTP(t *testing.T, native bool) {
	large := strings.Repeat("<>&", 2<<20) // 6 MiB legal tool output, below 32 MiB even with controls/history.
	for _, policy := range []bool{false, true} {
		t.Run(fmt.Sprintf("policy=%t", policy), func(t *testing.T) {
			var calls atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				index := int(calls.Add(1)) - 1
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				if index > 1 {
					t.Error("request replayed")
					return
				}
				researchInferenceAssert(t, body, researchInferenceRequest(index, false, large), map[bool]string{true: large}[index == 1], policy)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"controls-%s\",\"status\":\"completed\",\"output\":[]}}\n\n", []string{"first", "second"}[index])
			})
			if native {
				realGatewayNativePool(t, p)
			}
			settings := defaultSettings()
			settings.BuiltinPromptEnabled = policy
			settings.PromptCacheKeyEnabled = policy
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			for index := 0; index < 2; index++ {
				body := researchInferenceRequest(index, false, large)
				if len(body) >= maxBody {
					t.Fatal("fixture exceeds gateway input limit")
				}
				r := proxyRequest(http.MethodPost, "/responses", string(body))
				r.Header.Set("Session-Id", "controls-session")
				w := httptest.NewRecorder()
				p.ServeHTTP(w, r)
				if w.Code != http.StatusOK {
					t.Fatalf("valid %d-byte continuation rejected before upstream: HTTP%d %s", len(body), w.Code, w.Body.String())
				}
			}
			if calls.Load() != 2 {
				t.Fatal("large continuation did not reach upstream")
			}
		})
	}
}

func TestResearchInferenceControlsSmallSerializationBoundaries(t *testing.T) {
	markup := strings.Repeat("<>&x", 16) // Exactly 64 bytes before the JSON quotes.
	for _, value := range []any{markup, json.RawMessage(`"` + markup + `"`)} {
		encoded, err := marshalGatewayRequestJSON(value)
		if err != nil || len(encoded) != len(markup)+2 || bytes.Contains(encoded, []byte(`\u003c`)) {
			t.Fatalf("64-byte API text expanded: bytes=%d err=%v", len(encoded), err)
		}
	}
	// The standard encoder still escapes generated U+2028/U+2029 strings for
	// JS compatibility; RawMessage preserves valid incoming spellings. Decode
	// both to prove semantic equality rather than manually unescaping JSON.
	text := "<>&\u2028\u2029\n\"\\"
	generated, err := marshalGatewayRequestJSON(text)
	if err != nil || !bytes.Contains(generated, []byte(`\u2028\u2029`)) || bytes.Contains(generated, []byte(`\u003c`)) {
		t.Fatal("stdlib generated-string escaping contract changed")
	}
	var decoded string
	if json.Unmarshal(generated, &decoded) != nil || decoded != text {
		t.Fatal("standard quoting or control characters changed")
	}
	opaque := json.RawMessage(`{"same":1,"same":2,"n":9007199254740993,"text":"<>&` + "\u2028\u2029" + `"}`)
	encoded, err := marshalGatewayRequestJSON(map[string]json.RawMessage{"opaque": opaque})
	if err != nil || !bytes.Contains(encoded, opaque) {
		t.Fatal("opaque raw JSON members/numbers/Unicode spelling changed")
	}
	if _, err := marshalGatewayRequestJSON(json.RawMessage(`{"invalid":`)); err == nil {
		t.Fatal("invalid RawMessage bypassed JSON validation")
	}
}

func TestResearchInferenceControlsGeneratedAndNormalizedMarkup(t *testing.T) {
	markup := strings.Repeat("<>&x", 16)
	settings := defaultSettings()
	settings.PromptCacheKeyEnabled = false
	settings.BuiltinPrompt = "<gateway>& policy"
	request := proxyRequest(http.MethodPost, "/responses", "")
	for _, input := range []string{`"` + markup + `"`, `[{"role":"system","content":"` + markup + `"}]`} {
		raw := []byte(`{"model":"fixture","instructions":"<client>& instruction","input":` + input + `}`)
		prepared, _, err := applyGatewayPolicy(request, raw, "admin", settings)
		if err != nil {
			t.Fatal(err)
		}
		prepared, _, err = prepareBody(prepared, false)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(prepared, []byte(`\u003c`)) || bytes.Contains(prepared, []byte(`\u003e`)) || bytes.Contains(prepared, []byte(`\u0026`)) {
			t.Fatal("prompt splice, string input or system normalization pre-escaped source context")
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(prepared, &fields)
		var instructions string
		_ = json.Unmarshal(fields["instructions"], &instructions)
		if instructions != settings.BuiltinPrompt+"\n\n<client>& instruction" {
			t.Fatal("configured instruction concatenation changed")
		}
		compact, err := prepareResponsesCompaction(prepared)
		if err != nil || bytes.Contains(compact, []byte(`\u003c`)) {
			t.Fatal("compact adapter inflated retained context")
		}
	}
	stringInput := []byte(`{"model":"fixture","input":"` + markup + `"}`)
	compact, err := prepareResponsesCompaction(stringInput)
	if err != nil || bytes.Contains(compact, []byte(`\u003c`)) {
		t.Fatal("compact string input inflated")
	}
	result, err := responsesCompactionResult(compact, []byte(`{"status":"completed","output":[{"type":"compaction","encrypted_content":"synthetic+/="}]}`))
	if err != nil || bytes.Contains(result, []byte(`\u003c`)) {
		t.Fatal("legacy compact public JSON inflated retained context")
	}
}

func TestResearchInferenceControlsWSMarkupContinuationPolicy(t *testing.T) {
	large := strings.Repeat("<>&", 2<<20)
	for _, policy := range []bool{false, true} {
		t.Run(fmt.Sprintf("policy=%t", policy), func(t *testing.T) {
			var calls atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				conn.SetReadLimit(maxBody)
				for index := 0; index < 2; index++ {
					_, body, err := conn.Read(r.Context())
					if err != nil {
						return
					}
					calls.Add(1)
					researchInferenceAssert(t, body, researchInferenceRequest(index, true, large), map[bool]string{true: large}[index == 1], policy)
					result := fmt.Sprintf(`{"type":"response.completed","response":{"id":"controls-%s","status":"completed","output":[]}}`, []string{"first", "second"}[index])
					if err := conn.Write(r.Context(), websocket.MessageText, []byte(result)); err != nil {
						return
					}
				}
				_, _, _ = conn.Read(r.Context())
			})
			enableGatewayWS(t, p, "")
			settings, err := p.Settings.Get()
			if err != nil {
				t.Fatal(err)
			}
			settings.BuiltinPromptEnabled = policy
			settings.BuiltinPrompt = "Gateway context."
			settings.PromptCacheKeyEnabled = policy
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(p)
			defer server.Close()
			client, _ := gatewayWSDial(t, server.URL, "controls-session")
			defer client.CloseNow()
			for index := 0; index < 2; index++ {
				body := researchInferenceRequest(index, true, large)
				if err := client.Write(t.Context(), websocket.MessageText, body); err != nil {
					t.Fatal(err)
				}
				// The generic WS fixture allows five seconds for tiny events.
				// Race instrumentation on the bounded two-CPU Linux runner
				// needs more time to inspect this complete 6 MiB tool history.
				readCtx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				typ, result, err := client.Read(readCtx)
				cancel()
				if err != nil || typ != websocket.MessageText {
					t.Fatalf("read continuation reply: type=%v err=%v", typ, err)
				}
				if !bytes.Contains(result, []byte(`"type":"response.completed"`)) {
					t.Fatalf("valid %d-byte WS continuation rejected: %s", len(body), result)
				}
			}
			if calls.Load() != 2 {
				t.Fatal("WS continuation was dropped or replayed")
			}
		})
	}
}
