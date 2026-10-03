package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func outputParityEvent(raw string) string { return "data: " + raw + "\n\n" }

const outputParityFinal = `{"type":"response.completed","response":{"id":"response-a","status":"completed","output":[],"usage":{"input_tokens":19,"output_tokens":7,"total_tokens":26}}}`

// Both network hops are real TCP. Unknown differently-cased protocol members
// must remain opaque, not replace an executable item or hide encrypted history.
func TestOutputFieldParityRealHTTP(t *testing.T) {
	cases := []struct {
		name, wire string
		items      []string
	}{
		{"event_type_alias", outputParityEvent(`{"type":"response.output_item.done","Type":"future.event","output_index":0,"item":{"type":"reasoning","id":"r1","encrypted_content":"OPAQUE+/="}}`), []string{`{"type":"reasoning","id":"r1","encrypted_content":"OPAQUE+/="}`}},
		{"event_item_alias", outputParityEvent(`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"r1","encrypted_content":"OPAQUE+/="},"Item":{"type":"message","id":"wrong","content":[]}}`), []string{`{"type":"reasoning","id":"r1","encrypted_content":"OPAQUE+/="}`}},
		{"opaque_type_object", outputParityEvent(`{"type":"response.output_item.done","Type":{"future":true},"output_index":0,"item":{"type":"reasoning","id":"r1","encrypted_content":"OPAQUE+/="}}`), []string{`{"type":"reasoning","id":"r1","encrypted_content":"OPAQUE+/="}`}},
		{"item_id_alias_collision", outputParityEvent(`{"type":"response.output_item.done","item":{"type":"reasoning","id":"r1","ID":"same","encrypted_content":"FIRST"}}`) + outputParityEvent(`{"type":"response.output_item.done","item":{"type":"reasoning","id":"r2","ID":"same","encrypted_content":"SECOND"}}`), []string{`{"type":"reasoning","id":"r1","ID":"same","encrypted_content":"FIRST"}`, `{"type":"reasoning","id":"r2","ID":"same","encrypted_content":"SECOND"}`}},
		{"call_id_alias_collision", outputParityEvent(`{"type":"response.output_item.done","item":{"type":"function_call","call_id":"call-a","Call_ID":"same","name":"inspect","arguments":"{}"}}`) + outputParityEvent(`{"type":"response.output_item.done","item":{"type":"function_call","call_id":"call-b","Call_ID":"same","name":"inspect","arguments":"{}"}}`), []string{`{"type":"function_call","call_id":"call-a","Call_ID":"same","name":"inspect","arguments":"{}"}`, `{"type":"function_call","call_id":"call-b","Call_ID":"same","name":"inspect","arguments":"{}"}`}},
		{"fake_case_only_event", outputParityEvent(`{"Type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"fake","name":"inspect","arguments":"{}"}}`), nil},
		{"created_response_alias", outputParityEvent(`{"type":"response.created","response":{"id":"response-a"},"Response":{"id":"other"}}`), nil},
		{"created_id_alias", outputParityEvent(`{"type":"response.created","response":{"id":"response-a","ID":"other"}}`), nil},
		{"output_index_alias_collision", outputParityEvent(`{"type":"response.output_item.done","output_index":0,"Output_Index":8,"item":{"type":"reasoning","id":"r1","encrypted_content":"FIRST"}}`) + outputParityEvent(`{"type":"response.output_item.done","output_index":1,"Output_Index":8,"item":{"type":"reasoning","id":"r2","encrypted_content":"SECOND"}}`), []string{`{"type":"reasoning","id":"r1","encrypted_content":"FIRST"}`, `{"type":"reasoning","id":"r2","encrypted_content":"SECOND"}`}},
		{"opaque_response_id_alias", outputParityEvent(`{"type":"response.created","response":{"id":"response-a"}}`) + outputParityEvent(`{"type":"response.output_item.done","response_id":"response-a","Response_ID":"foreign","output_index":0,"item":{"type":"reasoning","id":"r1","encrypted_content":"OPAQUE"}}`), []string{`{"type":"reasoning","id":"r1","encrypted_content":"OPAQUE"}`}},
	}
	for _, native := range []bool{false, true} {
		for _, test := range cases {
			t.Run(fmt.Sprintf("native=%v/%s", native, test.name), func(t *testing.T) {
				var calls atomic.Int32
				wire := test.wire + outputParityEvent(outputParityFinal)
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, wire)
				})
				if native {
					realGatewayNativePool(t, p)
				}
				server := httptest.NewServer(p)
				t.Cleanup(server.Close)
				request := proxyRequest("POST", "/responses", `{"model":"test","input":[],"stream":false}`)
				request.URL.Scheme, request.URL.Host, request.RequestURI = "http", strings.TrimPrefix(server.URL, "http://"), ""
				res, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := io.ReadAll(res.Body)
				res.Body.Close()
				if err != nil || res.StatusCode != 200 {
					t.Fatalf("HTTP=%d error=%v body=%s", res.StatusCode, err, raw)
				}
				var result struct {
					Output []json.RawMessage `json:"output"`
				}
				if json.Unmarshal(raw, &result) != nil || len(result.Output) != len(test.items) {
					t.Fatalf("lost or fabricated output: %s", raw)
				}
				for i, want := range test.items {
					assertJSONEqual(t, result.Output[i], []byte(want))
				}
				if calls.Load() != 1 {
					t.Fatal("unexpected replay")
				}
				rec, ok := p.Metrics.LastFor("test")
				if !ok || rec.Status != "ok" || rec.InputTokens != 19 || rec.OutputTokens != 7 {
					t.Fatalf("accounting changed: %+v", rec)
				}
			})
		}
	}
}

func TestOutputFieldParityStreamingBytesUnchanged(t *testing.T) {
	wire := outputParityEvent(`{"type":"response.created","response":{"id":"response-a"}}`) + outputParityEvent(`{"type":"response.output_item.done","Type":{"future":true},"output_index":0,"item":{"type":"reasoning","id":"r1","ID":"opaque","encrypted_content":"OPAQUE"}}`) + outputParityEvent(outputParityFinal)
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) {
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, wire)
			})
			if native {
				realGatewayNativePool(t, p)
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest("POST", "/responses", `{"model":"test","stream":true,"input":[]}`))
			if w.Code != 200 || w.Body.String() != wire {
				t.Fatal("streaming passthrough bytes changed")
			}
		})
	}
}

func TestOutputFieldParityCanonicalIdentityStillRejectsConflicts(t *testing.T) {
	for _, items := range [][]string{
		{`{"type":"reasoning","id":"first"}`, `{"type":"reasoning","id":"second"}`},
		{`{"type":"function_call","call_id":"first","name":"inspect","arguments":"{}"}`, `{"type":"function_call","call_id":"second","name":"inspect","arguments":"{}"}`},
	} {
		wire := outputFixtureEvent(0, items[0]) + outputFixtureEvent(0, items[1]) + outputParityEvent(outputParityFinal)
		if _, err := collectResponse(strings.NewReader(wire)); err == nil {
			t.Fatal("canonical conflicting items were merged")
		}
	}
	for _, spelling := range []string{`"type"`, `"t\u0079pe"`} {
		wire := outputParityEvent(`{`+spelling+`:"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"r1","encrypted_content":"KEPT"}}`) + outputParityEvent(outputParityFinal)
		raw, err := collectResponse(strings.NewReader(wire))
		if err != nil || !strings.Contains(string(raw), "KEPT") {
			t.Fatal("escaped canonical name not honored")
		}
	}
}

func TestOutputFieldParityMalformedOtherIdentityDoesNotEraseValidID(t *testing.T) {
	for trial := 0; trial < 100; trial++ {
		item := `{"id":"same-item","type":{"future":true},"call_id":[],"ID":"other"}`
		wire := outputParityEvent(`{"type":"response.output_item.done","item":`+item+`}`) + outputParityEvent(`{"type":"response.output_item.done","item":`+item+`}`) + outputParityEvent(outputParityFinal)
		raw, err := collectResponse(strings.NewReader(wire))
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Output []json.RawMessage `json:"output"`
		}
		json.Unmarshal(raw, &result)
		if len(result.Output) != 1 {
			t.Fatalf("valid ID dedup depends on invalid field iteration: %s", raw)
		}
	}
}

func TestOutputFieldParitySparseTerminalPreservesOpaqueItem(t *testing.T) {
	first := `{"type":"reasoning","id":"r1","ID":"future-id","encrypted_content":"OPAQUE","unknown":{"n":9007199254740993}}`
	sparse := `{"type":"reasoning","id":"r1","ID":"different-extension"}`
	wire := outputFixtureEvent(0, first) + outputParityEvent(`{"type":"response.completed","response":{"id":"response-a","status":"completed","output":[`+sparse+`]}}`)
	raw, err := collectResponse(strings.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Output []map[string]json.RawMessage `json:"output"`
	}
	if json.Unmarshal(raw, &result) != nil || len(result.Output) != 1 || string(result.Output[0]["encrypted_content"]) != `"OPAQUE"` || string(result.Output[0]["ID"]) != `"different-extension"` {
		t.Fatalf("opaque completed fields lost: %s", raw)
	}
}
