package server

import (
	"bytes"
	"compress/gzip"
	"context"
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

const fieldParityCompleted = `data: {"type":"response.completed","response":{"id":"field-parity","status":"completed","output":[]}}

`
const fieldParitySummary = `{"type":"compaction","encrypted_content":"SYNTHETIC-SUMMARY+/="}`

func fieldParityBody(item string) string {
	return `{"model":"test","instructions":"preserve","input":[` + item + `],"reasoning":{"effort":"medium","mode":"pro","future":1.2500e+02},"parallel_tool_calls":false,"tool_choice":"auto","tools":[{"type":"function","name":"inspect","parameters":{"const":9007199254740993}}],"include":["reasoning.encrypted_content"],"stream":true,"store":false,"unknown":{"Mode":"opaque","large":123456789012345678901234567890}}`
}

func TestCompactionFieldParityInputItemsRemainExact(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, item := range []struct{ name, raw string }{
			{"encrypted_reasoning_alias", `{"type":"reasoning","summary":[],"encrypted_content":"SYNTHETIC-REASONING+/=","Type":"compaction_trigger"}`},
			{"future_opaque_alias_object", `{"type":"future_context","Type":{"category":"compaction_trigger"},"encrypted_content":"SYNTHETIC-FUTURE","number":9007199254740993}`},
			{"tool_output_alias_array", `{"type":"function_call_output","call_id":"c1","output":[{"type":"encrypted_content","encrypted_content":"SYNTHETIC-TOOL"}],"Type":["opaque"]}`},
			{"exact_trigger_with_opaque_alias", `{"type":"compaction_trigger","Type":"future_context","unknown":{"n":1.000000000000000000001}}`},
		} {
			t.Run(fmt.Sprintf("native=%v/%s", native, item.name), func(t *testing.T) {
				capture := make(chan []byte, 1)
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					raw, _ := io.ReadAll(r.Body)
					capture <- raw
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"compact-result","status":"completed","output":[`+fieldParitySummary+`]}}`+"\n\n")
				})
				p.BaseURL += "/backend-api/codex"
				if native {
					realGatewayNativePool(t, p)
				}
				raw := fieldParityBody(item.raw)
				w := httptest.NewRecorder()
				p.ServeHTTP(w, proxyRequest("POST", "/responses/compact", raw))
				if w.Code != 200 {
					t.Fatalf("opaque input field broke valid compact request: HTTP %d %s", w.Code, w.Body.String())
				}
				var got, original map[string]json.RawMessage
				json.Unmarshal(<-capture, &got)
				json.Unmarshal([]byte(raw), &original)
				var input []json.RawMessage
				if json.Unmarshal(got["input"], &input) != nil {
					t.Fatal("invalid upstream input")
				}
				want := 1
				if item.name != "exact_trigger_with_opaque_alias" {
					want = 2
				}
				if len(input) != want {
					t.Fatalf("input item was lost or control duplicated: count=%d want=%d body=%s", len(input), want, got["input"])
				}
				if want == 2 && !bytes.Equal(input[0], json.RawMessage(item.raw)) {
					t.Fatal("opaque original item bytes changed")
				}
				if string(input[len(input)-1]) != `{"type":"compaction_trigger"}` {
					t.Fatal("missing exact final trigger")
				}
				for _, name := range []string{"reasoning", "parallel_tool_calls", "tool_choice", "tools", "include", "unknown"} {
					if !bytes.Equal(got[name], original[name]) {
						t.Fatalf("non-input field changed: %s", name)
					}
				}
			})
		}
	}
}

func TestCompactionFieldParityRetainedUserContextAndSummary(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, extension := range []string{`"Role":"assistant"`, `"role_alias":{},"Type":"function_call"`, `"Role":{"private":"future"},"Type":[]`} {
			t.Run(fmt.Sprintf("native=%v/%s", native, extension), func(t *testing.T) {
				item := `{"type":"message","role":"user","content":[{"type":"input_text","text":"KEEP-QUESTION"}],` + extension + `}`
				var calls atomic.Int32
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					// Unknown uppercase fields must not replace the exact summary tag/content.
					_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"compact-result","status":"completed","output":[{"type":"compaction","encrypted_content":"SYNTHETIC-SUMMARY+/=","Type":"message","Encrypted_Content":"FOREIGN"}]}}`+"\n\n")
				})
				p.BaseURL += "/backend-api/codex"
				if native {
					realGatewayNativePool(t, p)
				}
				w := httptest.NewRecorder()
				p.ServeHTTP(w, proxyRequest("POST", "/responses/compact", fieldParityBody(item)))
				if w.Code != 200 {
					t.Fatalf("unknown summary/input fields rejected: HTTP%d %s", w.Code, w.Body.String())
				}
				var got struct {
					Output []json.RawMessage `json:"output"`
				}
				if json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Output) != 2 || !bytes.Equal(got.Output[0], []byte(item)) {
					t.Fatalf("retained instruction/history lost: %s", w.Body.String())
				}
				var summary map[string]json.RawMessage
				json.Unmarshal(got.Output[1], &summary)
				if string(summary["encrypted_content"]) != `"SYNTHETIC-SUMMARY+/="` || string(summary["Encrypted_Content"]) != `"FOREIGN"` || calls.Load() != 1 {
					t.Fatal("summary opaque bytes changed or request retried")
				}
			})
		}
	}
}

func TestCompactionFieldParityAdjacentHTTPWebSocket(t *testing.T) {
	item := `{"type":"reasoning","summary":[],"encrypted_content":"SYNTHETIC-REASONING+/=","Type":{"future":9007199254740993}}`
	raw := fieldParityBody(item)
	for _, native := range []bool{false, true} {
		for _, mode := range []string{"http", "ws"} {
			t.Run(fmt.Sprintf("native=%v/%s", native, mode), func(t *testing.T) {
				capture := make(chan []byte, 1)
				handler := func(w http.ResponseWriter, r *http.Request) {
					if mode == "ws" {
						c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover})
						if err != nil {
							t.Error(err)
							return
						}
						defer c.CloseNow()
						_, body, err := c.Read(r.Context())
						if err != nil {
							return
						}
						capture <- body
						c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"field-parity","status":"completed","output":[]}}`))
						_, _, _ = c.Read(r.Context())
						return
					}
					body, _ := io.ReadAll(r.Body)
					capture <- body
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, fieldParityCompleted)
				}
				var p *Proxy
				if native && mode == "ws" {
					p, _ = nativeWSFixture(t, handler)
				} else {
					p = proxyFixture(t, handler)
					if native {
						realGatewayNativePool(t, p)
					}
				}
				settings, _ := p.Settings.Get()
				settings.BuiltinPromptEnabled = false
				settings.PromptCacheKeyEnabled = false
				p.Settings.Set(settings)
				if mode == "ws" {
					enableGatewayWS(t, p, "")
					server := httptest.NewServer(p)
					defer server.Close()
					c, _ := gatewayWSDial(t, server.URL, "field-parity")
					defer c.CloseNow()
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					if err := c.Write(ctx, websocket.MessageText, []byte(strings.Replace(raw, `{"model"`, `{"type":"response.create","model"`, 1))); err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(gatewayWSRead(t, c)), "response.completed") {
						t.Fatal("normal WS rejected opaque input")
					}
				} else {
					w := httptest.NewRecorder()
					p.ServeHTTP(w, proxyRequest("POST", "/responses", raw))
					if w.Code != 200 {
						t.Fatal(w.Code)
					}
				}
				var got map[string]json.RawMessage
				json.Unmarshal(<-capture, &got)
				var input []json.RawMessage
				json.Unmarshal(got["input"], &input)
				if len(input) != 1 || !bytes.Equal(input[0], []byte(item)) {
					t.Fatal("normal HTTP/WS altered opaque input")
				}
				var original map[string]json.RawMessage
				json.Unmarshal([]byte(raw), &original)
				for _, k := range []string{"reasoning", "parallel_tool_calls", "tool_choice", "tools", "include", "unknown"} {
					if !bytes.Equal(got[k], original[k]) {
						t.Fatalf("normal field changed: %s", k)
					}
				}
			})
		}
	}
}

func TestCompactionFieldParityExactRetentionAndOpaqueSummary(t *testing.T) {
	user := `{"type":"message","role":"user","Role":"assistant","content":"CANONICAL-USER","unknown":{"n":9007199254740993}}`
	assistant := `{"type":"message","role":"assistant","Role":"user","content":"DO-NOT-RETAIN-ASSISTANT"}`
	tool := `{"type":"function_call_output","Type":"message","role":"user","output":"DO-NOT-RETAIN-TOOL"}`
	request := []byte(`{"input":[` + user + `,` + assistant + `,` + tool + `],"Input":{"future":true}}`)
	for _, summary := range []string{
		`{"type":"compaction","Type":"message","encrypted_content":"SYNTHETIC-SUMMARY+/=","Encrypted_Content":"FOREIGN"}`,
		`{"type":"compaction","Type":{"future":true},"encrypted_content":"SYNTHETIC-SUMMARY+/=","Encrypted_Content":123456789012345678901234567890}`,
		`{"type":"compaction","encrypted_content":"SYNTHETIC-SUMMARY+/=","future":{"type":"message","duplicate":1,"duplicate":2}}`,
	} {
		result, err := responsesCompactionResult(request, []byte(`{"status":"completed","output":[`+summary+`]}`))
		if err != nil {
			t.Fatalf("opaque future summary rejected: %v", err)
		}
		var fields map[string]json.RawMessage
		json.Unmarshal(result, &fields)
		var output []json.RawMessage
		json.Unmarshal(fields["output"], &output)
		if len(output) != 2 || string(output[0]) != user || string(output[1]) != summary {
			t.Fatalf("exact role or summary lost: %s", result)
		}
	}
}

func TestCompactionFieldParityCannotUseAliasAsRequiredField(t *testing.T) {
	for _, item := range []string{
		`{"Type":"compaction","encrypted_content":"alias-type"}`,
		`{"type":"compaction","Encrypted_Content":"alias-content"}`,
		`{"type":"compaction","encrypted_content":null,"Encrypted_Content":"alias-content"}`,
		`{"type":"compaction","encrypted_content":false,"Encrypted_Content":"alias-content"}`,
		`{"type":"message","Type":"compaction","encrypted_content":"wrong-canonical-type"}`,
	} {
		if _, err := responsesCompactionResult([]byte(`{"input":[]}`), []byte(`{"status":"completed","output":[`+item+`]}`)); err == nil {
			t.Fatalf("alias replaced actual compaction member: %s", item)
		}
	}
	for _, item := range []string{`{"type":{} }`, `null`, `"invalid"`} {
		if _, err := prepareResponsesCompaction([]byte(`{"input":[` + item + `]}`)); err == nil {
			t.Fatalf("invalid canonical item admitted: %s", item)
		}
	}
}

func TestCompactionFieldParityRealWebSocketContinuation(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) {
			firstItem := `{"type":"reasoning","encrypted_content":"INITIAL-OPAQUE+/=","summary":[],"Type":{"extension":"keep"}}`
			nextItem := `{"type":"function_call_output","call_id":"fc-continuation","output":[{"type":"encrypted_content","encrypted_content":"TOOL-OPAQUE+/="},{"type":"input_text","text":"done"}],"Type":["future"],"unknown":{"n":9007199254740993}}`
			capture := make(chan []byte, 2)
			handler := func(w http.ResponseWriter, r *http.Request) {
				c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover})
				if err != nil {
					t.Error(err)
					return
				}
				defer c.CloseNow()
				for i := 0; i < 2; i++ {
					_, body, err := c.Read(r.Context())
					if err != nil {
						return
					}
					capture <- body
					c.Write(r.Context(), websocket.MessageText, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"parity-turn-%d","status":"completed","output":[]}}`, i)))
				}
				_, _, _ = c.Read(r.Context())
			}
			var p *Proxy
			if native {
				p, _ = nativeWSFixture(t, handler)
			} else {
				p = proxyFixture(t, handler)
				enableGatewayWS(t, p, "")
			}
			settings, _ := p.Settings.Get()
			settings.BuiltinPromptEnabled = false
			settings.PromptCacheKeyEnabled = false
			p.Settings.Set(settings)
			server := httptest.NewServer(p)
			defer server.Close()
			c, _ := gatewayWSDial(t, server.URL, "field-parity-continuation")
			defer c.CloseNow()
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			for i, item := range []string{firstItem, nextItem} {
				body := strings.Replace(fieldParityBody(item), `{"model"`, `{"type":"response.create","model"`, 1)
				if i == 1 {
					body = strings.TrimSuffix(body, "}") + `,"previous_response_id":"parity-turn-0","client_metadata":{"session_id":"stable","future":"keep"}}`
				}
				if err := c.Write(ctx, websocket.MessageText, []byte(body)); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(gatewayWSRead(t, c)), fmt.Sprintf("parity-turn-%d", i)) {
					t.Fatal("continuation completion missing")
				}
				var expected, actual map[string]json.RawMessage
				json.Unmarshal([]byte(body), &expected)
				json.Unmarshal(<-capture, &actual)
				for _, key := range []string{"input", "reasoning", "parallel_tool_calls", "tools", "unknown", "previous_response_id", "client_metadata"} {
					if !bytes.Equal(expected[key], actual[key]) {
						t.Fatalf("turn%d lost field %s", i, key)
					}
				}
			}
		})
	}
}

func TestCompactionFieldParityCompressedRequestAndStructuredHistory(t *testing.T) {
	item := `{"type":"reasoning","encrypted_content":"OPAQUE-REASONING+/=","summary":[],"Type":{"content":"keep"},"unknown":{"decimal":1.2500e+02,"int":9007199254740993}}`
	for _, native := range []bool{false, true} {
		for _, encoding := range []string{"identity", "gzip", "zstd"} {
			t.Run(fmt.Sprintf("native=%v/%s", native, encoding), func(t *testing.T) {
				capture := make(chan []byte, 1)
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					raw, err := io.ReadAll(r.Body)
					if err == nil && r.Header.Get("Content-Encoding") == "zstd" {
						d, e := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
						if e != nil {
							err = e
						} else {
							raw, err = d.DecodeAll(raw, nil)
							d.Close()
						}
					}
					if err != nil {
						t.Error(err)
						http.Error(w, "local fixture decode", 400)
						return
					}
					capture <- raw
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, `data: {"type":"response.completed","response":{"id":"compact-mixed","status":"completed","output":[{"type":"reasoning","summary":[],"Type":[]},{"type":"compaction_summary","encrypted_content":"SUMMARY","Type":{"future":true},"future":{"same":1,"same":2}}]}}`+"\n\n")
				})
				p.BaseURL += "/backend-api/codex"
				if native {
					realGatewayNativePool(t, p)
				}
				raw := []byte(fieldParityBody(item))
				wire := raw
				if encoding == "gzip" {
					var b bytes.Buffer
					g := gzip.NewWriter(&b)
					g.Write(raw)
					g.Close()
					wire = b.Bytes()
				}
				if encoding == "zstd" {
					z, e := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
					if e != nil {
						t.Fatal(e)
					}
					wire = z.EncodeAll(raw, nil)
					z.Close()
				}
				r := httptest.NewRequest("POST", "/responses/compact", bytes.NewReader(wire))
				r.Header.Set("Authorization", "Bearer test-client-key")
				if encoding != "identity" {
					r.Header.Set("Content-Encoding", encoding)
				}
				w := httptest.NewRecorder()
				p.ServeHTTP(w, r)
				if encoding == "gzip" {
					// The current public gateway explicitly supports identity/zstd
					// ingress only. Do not broaden its transport contract here.
					if w.Code != http.StatusUnsupportedMediaType || len(capture) != 0 {
						t.Fatal("unsupported compression dispatched or changed status")
					}
					return
				}
				if w.Code != 200 {
					t.Fatalf("compressed compaction rejected opaque data: %d %s", w.Code, w.Body.String())
				}
				var got map[string]json.RawMessage
				json.Unmarshal(<-capture, &got)
				var input []json.RawMessage
				json.Unmarshal(got["input"], &input)
				if len(input) != 2 || string(input[0]) != item || string(input[1]) != `{"type":"compaction_trigger"}` {
					t.Fatal("compressed transport lost encrypted item or trigger")
				}
				var result struct {
					Output []json.RawMessage `json:"output"`
				}
				json.Unmarshal(w.Body.Bytes(), &result)
				if len(result.Output) != 1 || !strings.Contains(string(result.Output[0]), `"type":"compaction_summary"`) || !strings.Contains(string(result.Output[0]), `"same":1,"same":2`) {
					t.Fatal("mixed summary output lost opaque original bytes")
				}
			})
		}
	}
}
