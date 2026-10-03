package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBPSRejectedTransportAndParameter(t *testing.T) {
	for _, tc := range []struct{ name, path, stream, options, wantType, wantParam string }{
		{"json", "/v1/responses", `false`, `{"include_usage":"wrong"}`, "http_json", "stream_options.include_usage"},
		{"sse", "/v1/responses", `true`, `{"include_usage":"wrong"}`, "http_sse", "stream_options.include_usage"},
		{"compact", "/v1/responses/compact", `true`, `{"include_usage":"wrong"}`, "compact", "stream_options.include_usage"},
		{"unknown_option", "/v1/responses", `true`, `{"PRIVATE_SECRET_KEY":true}`, "http_sse", "stream_options.unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := bpsGatewayFixture(t, func(http.ResponseWriter, *http.Request) { calls++ })
			w := httptest.NewRecorder()
			body := fmt.Sprintf(`{"model":"gpt-6-astra","stream":%s,"input":[],"stream_options":%s}`, tc.stream, tc.options)
			p.gatewayBPS(w, proxyRequest(http.MethodPost, tc.path, body))
			rows := p.Metrics.RecentForAccount("", 5)
			if len(rows) != 1 {
				t.Fatalf("records=%d", len(rows))
			}
			r := rows[0]
			raw, _ := json.Marshal(r)
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(raw, &fields)
			var param string
			_ = json.Unmarshal(fields["error_param"], &param)
			t.Logf("request_type=%q error_param=%q", r.RequestType, param)
			if w.Code != 400 || calls != 0 || r.RequestType != tc.wantType || r.Stream != (tc.wantType == "http_sse") || param != tc.wantParam || r.ErrorSource != "client" || r.ErrorPhase != "request_validation" {
				t.Fatalf("misclassified local rejection http=%d calls=%d type=%q param=%q", w.Code, calls, r.RequestType, param)
			}
			if r.UsageKnown != nil || r.CachedTokensKnown != nil || r.InputTokens != 0 || r.OutputTokens != 0 || r.ReturnedModel != "" || r.SentModel != "" || len(r.Attempts) != 0 || r.TTFTMs != nil {
				t.Fatal("local rejection invented upstream usage/dispatch")
			}
			if strings.Contains(string(fields["error_param"]), "PRIVATE") {
				t.Fatal("untrusted parameter name persisted")
			}
		})
	}
}
func TestBPSRejectedWebSocketIngress(t *testing.T) {
	calls := 0
	p := bpsGatewayFixture(t, func(http.ResponseWriter, *http.Request) { calls++ })
	server := httptest.NewServer(p)
	defer server.Close()
	conn, _ := gatewayWSDial(t, server.URL, "invalid-options-ws")
	defer conn.CloseNow()
	bpsGatewayWSSend(t, conn, `{"type":"response.create","model":"gpt-6-astra","input":[],"stream_options":{"include_usage":"wrong"}}`)
	event := bpsGatewayWSUntilTerminal(t, conn, "")
	var kind string
	_ = json.Unmarshal(event["type"], &kind)
	if kind != "error" {
		t.Fatal("WS rejection changed event")
	}
	deadline := time.Now().Add(time.Second)
	var rows []RequestRecord
	for {
		rows = p.Metrics.RecentForAccount("", 5)
		if len(rows) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(rows) != 1 {
		t.Fatal("WS rejection log missing")
	}
	r := rows[0]
	raw, _ := json.Marshal(r)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	var param string
	_ = json.Unmarshal(fields["error_param"], &param)
	if calls != 0 || r.RequestType != "websocket" || !r.Stream || param != "stream_options.include_usage" || r.UsageKnown != nil || len(r.Attempts) != 0 || r.SentModel != "" {
		t.Fatalf("WS local rejection type=%q param=%q calls=%d", r.RequestType, param, calls)
	}
}

func TestBPSIngressInvalidBooleanAndFixedParameter(t *testing.T) {
	for _, tc := range []struct {
		body string
		want string
	}{
		{`{"stream":true}`, "http_sse"}, {`{"stream":false}`, "http_json"}, {`{}`, "http_json"}, {`{"stream":null}`, ""}, {`{"stream":"true"}`, ""}, {`{"stream":1}`, ""}, {`{"stream":true} trailing`, ""}, {`{"Stream":true}`, "http_json"}, {`{"stream":true,"stream":false}`, "http_json"},
	} {
		if got := bpsIngressRequestType(false, "/responses", []byte(tc.body)); got != tc.want {
			t.Fatalf("bad stream classification %q want%q", got, tc.want)
		}
	}
	if bpsIngressRequestType(true, "/responses/compact", []byte(`{"stream":false}`)) != "websocket" {
		t.Fatal("WS entry did not take priority")
	}
	for _, param := range []string{"service_tier", "stream_options.include_usage", "stream_options.include_obfuscation", "text.format.schema"} {
		if bpsObservedErrorParameter(param) != param {
			t.Fatal("fixed known parameter omitted")
		}
	}
	if bpsObservedErrorParameter("stream_options.PRIVATE_TOKEN") != "stream_options.unknown" || bpsObservedErrorParameter("arbitrary.PRIVATE_TOKEN") != "" {
		t.Fatal("arbitrary parameter content escaped allowlist")
	}
	var legacy RequestRecord
	raw, _ := json.Marshal(legacy)
	if strings.Contains(string(raw), "error_param") {
		t.Fatal("legacy absent parameter invented")
	}
}
