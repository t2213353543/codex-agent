package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBPSStreamAuthenticationBeforeOutputPublicStatus(t *testing.T) {
	for _, mode := range []string{"sse_exhausted", "json_exhausted", "sse_opaque", "ws_opaque", "ws_precreated", "type_only", "code_only", "unrecognized_token_code", "unknown", "after_output", "invalid_client"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				detail := `{"code":"invalid_api_key","type":"authentication_error","message":"synthetic expired credential"}`
				if mode == "code_only" {
					detail = `{"code":"invalid_api_key","message":"synthetic expired credential"}`
				}
				if mode == "unrecognized_token_code" {
					detail = `{"code":"token_invalidated","type":"server_error","message":"synthetic expired credential"}`
				}
				if mode == "type_only" {
					detail = `{"code":null,"type":"authentication_error","message":"synthetic expired credential"}`
				}
				if mode == "unknown" {
					detail = `{"code":"unknown","type":"server_error","message":"arbitrary text 401 unauthorized"}`
				}
				if mode != "ws_precreated" {
					_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.created","response":{"id":"stream_auth","status":"in_progress"}}`)
				}
				if mode == "after_output" {
					_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.output_text.delta","response_id":"stream_auth","delta":"VISIBLE"}`)
				}
				_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.failed","response":{"id":"stream_auth","status":"failed","model":"gpt-6-astra","output":[],"error":`+detail+`,"usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":20}}}}`)
			})
			settings, err := p.Settings.Get()
			if err != nil {
				t.Fatal(err)
			}
			settings.Upstream401RecoveryEnabled = false
			settings.AutomaticTokenRefreshEnabled = false
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; return nil }); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"access_token": "other-token", "expires_at": time.Now().Add(time.Hour).Unix()})
			if err := p.Store.ImportWith("other", raw, AccountDefaults{Concurrency: 2, Priority: 1}); err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
			input := json.RawMessage(`[{"role":"user","content":"synthetic"}]`)
			if mode == "sse_opaque" || mode == "ws_opaque" || mode == "ws_precreated" {
				input = json.RawMessage(`[{"type":"reasoning","encrypted_content":"unknown-synthetic-opaque"},{"role":"user","content":"synthetic"}]`)
			}
			var status int
			var public []byte
			if mode == "ws_opaque" || mode == "ws_precreated" {
				server := httptest.NewServer(p)
				defer server.Close()
				conn, handshake := gatewayWSDial(t, server.URL, "stream-auth-session")
				defer conn.CloseNow()
				if handshake.StatusCode != 101 {
					t.Fatal("missing actual WebSocket handshake")
				}
				frame, _ := json.Marshal(map[string]any{"type": "response.create", "stream_id": "auth-lane", "model": "gpt-6-astra", "input": input})
				bpsGatewayWSSend(t, conn, string(frame))
				event := bpsGatewayWSUntilTerminal(t, conn, "auth-lane")
				_ = json.Unmarshal(event["status"], &status)
				public, _ = json.Marshal(event)
			} else {
				body, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "stream": mode != "json_exhausted", "input": input})
				req := proxyRequest("POST", "/v1/responses", string(body))
				req.Header.Set("Session-Id", "stream-auth-session")
				if mode == "invalid_client" {
					req.Header.Set("Authorization", "Bearer invalid-key")
				}
				out := httptest.NewRecorder()
				p.gatewayBPS(out, req)
				status, public = out.Code, out.Body.Bytes()
			}
			wantCalls, wantStatus, recordName := 2, 503, "other"
			switch mode {
			case "sse_opaque", "ws_precreated":
				wantCalls, recordName = 1, "test"
			case "ws_opaque":
				wantCalls, wantStatus, recordName = 1, 0, "test" // created already commits WS protocol identity.
			case "unknown", "unrecognized_token_code", "after_output":
				wantCalls, wantStatus, recordName = 1, 200, "test"
			case "invalid_client":
				wantCalls, wantStatus = 0, 401
			}
			if calls != wantCalls {
				t.Fatalf("wrong replay boundary: calls%d want%d", calls, wantCalls)
			}
			if mode == "invalid_client" {
				if status != 401 {
					t.Fatal("real invalid client key changed")
				}
				return
			}
			var record RequestRecord
			for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
				if r, ok := p.Metrics.LastFor(recordName); ok {
					record = r
					break
				}
				time.Sleep(time.Millisecond)
			}
			t.Logf("mode=%s public_status=%d internal_status=%d terminal=%s calls=%d source=%s phase=%s", mode, status, record.HTTPCode, record.TerminalStatus, calls, record.ErrorSource, record.ErrorPhase)
			if status != wantStatus {
				t.Errorf("upstream stream authentication became client credential failure: got%d want%d", status, wantStatus)
			}
			if record.InputTokens != 100 || record.OutputTokens != 7 || record.CachedTokens != 80 || record.CacheWriteTokens == nil || *record.CacheWriteTokens != 20 {
				t.Fatal("actual terminal usage lost")
			}
			if len(record.Attempts) != wantCalls {
				t.Fatal("actual attempts absent")
			}
			for _, attempt := range record.Attempts {
				logical := 401
				if mode == "unknown" || mode == "unrecognized_token_code" {
					logical = 200
				}
				if attempt.StatusCode == nil || *attempt.StatusCode != logical || attempt.ObservedHTTPStatusCode == nil || *attempt.ObservedHTTPStatusCode != 200 {
					t.Fatal("terminal classification and real HTTP observation collapsed")
				}
			}
			if mode == "unknown" {
				if !strings.Contains(string(public), "unknown") || strings.Contains(string(public), "upstream_authentication_unavailable") {
					t.Fatal("untyped error was reclassified")
				}
			} else if mode == "ws_opaque" || mode == "unrecognized_token_code" {
				if strings.Contains(string(public), "upstream_authentication_unavailable") {
					t.Fatal("committed WS or unrecognized code was changed")
				}
			} else if mode == "after_output" {
				if !strings.Contains(string(public), "VISIBLE") || !strings.Contains(string(public), "invalid_api_key") || strings.Contains(string(public), "upstream_authentication_unavailable") {
					t.Fatal("visible stream was replaced or replayed")
				}
			} else if !strings.Contains(string(public), "upstream_authentication_unavailable") || strings.Contains(string(public), "invalid_api_key") || strings.Contains(string(public), "authentication_error") {
				t.Error("private account auth type/code remained in final client payload")
			}
		})
	}
}
