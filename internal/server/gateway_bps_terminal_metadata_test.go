package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestBPSTerminalRejectedToolRetainsObservedUsage(t *testing.T) {
	for _, structured := range []bool{false, true} {
		t.Run(fmt.Sprint(structured), func(t *testing.T) {
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_usage_rejected","model":"gpt-6-astra","status":"completed","output":[{"type":"function_call","id":"fc_unlisted","call_id":"call_unlisted","name":"UNDECLARED_PRIVATE_TOOL","arguments":"PRIVATE_PAYLOAD"}],"usage":{"input_tokens":100,"output_tokens":7,"total_tokens":107,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":20}}}}`+"\n\n")
			})
			extra := ""
			if structured {
				extra = `,"text":{"format":{"type":"json_object"}}`
			}
			w := httptest.NewRecorder()
			p.gatewayBPS(w, proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","stream":false,"input":[],"tools":[]`+extra+`}`))
			record, ok := p.Metrics.LastFor("test")
			encoded, _ := json.Marshal(record)
			t.Logf("structured=%t http=%d recorded_input=%d recorded_read=%d usage_known=%v returned_model=%s", structured, w.Code, record.InputTokens, record.CachedTokens, record.UsageKnown, record.ReturnedModel)
			if w.Code != 502 || !ok || record.Status != "error" || bytes.Contains(w.Body.Bytes(), []byte(`"function_call"`)) || bytes.Contains(w.Body.Bytes(), []byte("PRIVATE_PAYLOAD")) {
				t.Fatal("invalid output became visible or successful")
			}
			if record.UsageKnown == nil || !*record.UsageKnown || record.InputTokens != 100 || record.OutputTokens != 7 || record.CachedTokens != 80 || record.CacheWriteTokens == nil || *record.CacheWriteTokens != 20 || record.ReturnedModel != "gpt-6-astra" {
				t.Fatalf("validated terminal usage lost before rejected tool; schema=%s", encoded)
			}
		})
	}
}

func TestBPSTerminalUsageAliasPriority(t *testing.T) {
	for _, usage := range []string{
		`{"input_tokens":100,"output_tokens":7,"cached_tokens":80}`,
		`{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":80,"image_tokens":5},"completion_tokens_details":{"image_tokens":2}}`,
		`{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":null},"cached_tokens":80}`,
		`{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":"PRIVATE_INVALID"},"cached_tokens":80}`,
		`{"input_tokens":null,"prompt_tokens":100,"output_tokens":7,"cached_tokens":80}`,
		`{"input_tokens":0,"prompt_tokens":100,"output_tokens":0,"completion_tokens":7,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"cached_tokens":80}`,
		`{"input_tokens":100,"output_tokens":7,"input_tokens_details":"PRIVATE_INVALID","cached_tokens":80}`,
		`{}`,
		`null`,
	} {
		t.Run(fmt.Sprint(len(usage)), func(t *testing.T) {
			terminal := []byte(`{"id":"resp_alias","status":"completed","model":"gpt-6-astra","output":[{"type":"function_call","name":"UNDECLARED_PRIVATE_TOOL","call_id":"call","arguments":"PRIVATE_PAYLOAD"}],"usage":` + usage + `}`)
			expected := RequestRecord{}
			recordBPSTerminalUsage(&expected, terminal)
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, `data: {"type":"response.completed","response":`+string(terminal)+`}`+"\n\n")
			})
			w := httptest.NewRecorder()
			p.gatewayBPS(w, proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","stream":false,"input":[],"tools":[]}`))
			got, ok := p.Metrics.LastFor("test")
			if w.Code != 502 || !ok {
				t.Fatal("rejected fixture did not fail")
			}
			if got.InputTokens != expected.InputTokens || got.OutputTokens != expected.OutputTokens || got.CachedTokens != expected.CachedTokens || !reflect.DeepEqual(got.UsageKnown, expected.UsageKnown) || !reflect.DeepEqual(got.CachedTokensKnown, expected.CachedTokensKnown) || !reflect.DeepEqual(got.CacheWriteTokens, expected.CacheWriteTokens) || !reflect.DeepEqual(got.ImageInputTokens, expected.ImageInputTokens) || !reflect.DeepEqual(got.ImageOutputTokens, expected.ImageOutputTokens) || got.UsageSchemaSource != expected.UsageSchemaSource {
				t.Fatalf("early alias metadata changed original accounting priority: got in%d out%d cache%d known%v; original in%d out%d cache%d known%v", got.InputTokens, got.OutputTokens, got.CachedTokens, got.UsageKnown, expected.InputTokens, expected.OutputTokens, expected.CachedTokens, expected.UsageKnown)
			}
		})
	}
}
