package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBPSCompactionDoesNotAcquireGenerationOnlyCacheFields(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
		for _, cache := range []string{"", `,"prompt_cache_key":"explicit-client-key"`} {
			t.Run(path+cache, func(t *testing.T) {
				calls := 0
				p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
					calls++
					body, _ := io.ReadAll(r.Body)
					var fields map[string]json.RawMessage
					if json.Unmarshal(body, &fields) != nil {
						t.Fatal("invalid request JSON")
					}
					if r.URL.Path != "/responses/compact" || len(fields) != 3 || fields["input"] == nil || fields["model"] == nil || fields["metadata"] == nil {
						t.Errorf("compact request schema changed after admission: %s %s", r.URL.Path, body)
						w.WriteHeader(422)
						_, _ = io.WriteString(w, `{"error":{"message":"Invalid request body"}}`)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"compact-cache-fixture","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"opaque-fixture"}],"usage":{"input_tokens":100,"output_tokens":20}}`)
				})
				settings, _ := p.Settings.Get()
				settings.PromptCacheKeyEnabled = true
				if err := p.Settings.Set(settings); err != nil {
					t.Fatal(err)
				}
				input := `[{"role":"user","content":"preserve this context"}]`
				if path == "/v1/responses" {
					input = `[{"role":"user","content":"preserve this context"},{"type":"compaction_trigger"}]`
				}
				w := httptest.NewRecorder()
				p.gatewayBPS(w, proxyRequest("POST", path, `{"model":"gpt-6-astra","input":`+input+cache+`}`))
				if w.Code != 200 || calls != 1 {
					t.Fatalf("compaction failed: status=%d calls=%d body=%s", w.Code, calls, w.Body)
				}
			})
		}
	}
}
