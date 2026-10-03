package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-agent/internal/bps"
)

func TestBPSCompletedAccountDeletionPublicAuthClassification(t *testing.T) {
	for _, mode := range []string{"visible_sse", "structured_sse", "json"} {
		t.Run(mode, func(t *testing.T) {
			var p *Proxy
			calls := 0
			p = bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				var payload []byte
				if mode == "structured_sse" {
					reply := httptest.NewRecorder()
					bpsStructuredUpstreamReply(t, reply, r, "completion_deleted", `{"number":9007199254740993}`)
					payload = bytes.Clone(reply.Body.Bytes())
				} else {
					_, _ = io.Copy(io.Discard, r.Body)
					output := `[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"VISIBLE"}]}]`
					payload = []byte(bpsGatewayFrame("response.completed", "completion_deleted", output))
					if mode == "visible_sse" {
						payload = append([]byte("data: "+`{"type":"response.output_text.delta","response_id":"completion_deleted","delta":"VISIBLE"}`+"\n\n"), payload...)
					}
				}
				// Account was valid at dispatch; management removes it while
				// upstream work is in flight, before the completion checkpoint.
				if err := p.Store.Delete("test"); err != nil {
					t.Error(err)
				}
				p.invalidateAccounts()
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(payload)
			})
			body := fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":"synthetic"}`, mode != "json")
			if mode == "structured_sse" {
				body = fmt.Sprintf(`{"model":"gpt-6-astra","stream":true,"input":"synthetic","text":%s}`, bpsStructuredTestText)
			}
			out := httptest.NewRecorder()
			aborted := false
			func() {
				defer func() {
					if value := recover(); value != nil {
						if value != http.ErrAbortHandler {
							panic(value)
						}
						aborted = true
					}
				}()
				p.gatewayBPS(out, proxyRequest("POST", "/v1/responses", body))
			}()
			record, ok := p.Metrics.LastFor("test")
			if !ok || calls != 1 || record.Status != "error" || !strings.Contains(record.Error, "after authentication recovery") {
				t.Fatal("fixture did not reach post-generation account checkpoint")
			}
			var event map[string]json.RawMessage
			for _, line := range strings.Split(out.Body.String(), "\n") {
				if strings.HasPrefix(line, "data: ") {
					var e map[string]json.RawMessage
					if json.Unmarshal([]byte(line[6:]), &e) == nil && bpsGatewayString(e, "type") == "response.failed" {
						event = e
					}
				}
			}
			containsAuth := strings.Contains(out.Body.String(), "authentication recovery")
			t.Logf("mode=%s publicHTTP=%d aborted=%t completed_record_terminal=%s source=%s phase=%s TTFTknown=%t public_failed=%t public_authentication_phrase=%t", mode, out.Code, aborted, record.TerminalStatus, record.ErrorSource, record.ErrorPhase, record.TTFTMs != nil, event != nil, containsAuth)
			if record.ErrorSource != "gateway" || record.ErrorPhase != "authorization" {
				t.Fatal("local completion checkpoint was misclassified as upstream failure")
			}
			if mode != "json" {
				if aborted || out.Code != 200 || event == nil {
					t.Fatal("local completion failure terminal missing")
				}
				var response struct {
					Error struct{ Code, Type, Message string }
				}
				if json.Unmarshal(event["response"], &response) != nil {
					t.Fatal("invalid failure")
				}
				if response.Error.Code != "response_access_changed" || response.Error.Type != "proxy_error" {
					t.Error("local account checkpoint lacks explicit response_access_changed classification")
				}
				if containsAuth {
					t.Error("local deleted-account completion prose matches Sub2API stream authentication401 classifier")
				}
			} else if out.Code != http.StatusForbidden || containsAuth || !strings.Contains(out.Body.String(), "response_access_changed") {
				t.Fatal("ordinary JSON lost the local completion status")
			}
		})
	}
}

func TestBPSCompletionAccessTypedFailuresKeepInternalStatus(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			checkpoint := &gatewayRecoveryAuthorizationError{status: status}
			before := checkpoint.Error()
			wrapped := &bps.StructuredValidationError{Err: checkpoint}
			result := bps.StreamResult{Response: json.RawMessage(`{"id":"completed_before_access_change","usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":20}}}`)}
			raw := bpsStructuredValidationFailure(result, wrapped)
			var event struct {
				Response struct {
					Error struct{ Code, Type, Message string }
					Usage json.RawMessage
				}
			}
			if json.Unmarshal(raw, &event) != nil || event.Response.Error.Code != "response_access_changed" || event.Response.Error.Type != "proxy_error" ||
				event.Response.Error.Message != "gateway response completion is unavailable because its account access changed" {
				t.Fatal("typed completion access failure was not distinguished")
			}
			if checkpoint.status != status || checkpoint.Error() != before || !bytes.Contains(event.Response.Usage, []byte(`"cached_tokens":80`)) {
				t.Fatal("public message rewrite altered original internal status or measured usage")
			}
		})
	}
}
