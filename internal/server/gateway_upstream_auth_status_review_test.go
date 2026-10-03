package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBPSUpstreamAuthenticationUnavailablePublicStatus(t *testing.T) {
	for _, mode := range []string{"exhausted_json", "exhausted_sse", "exhausted_compact", "opaque", "recovered", "invalid_client_key", "content_policy", "after_output"} {
		t.Run(mode, func(t *testing.T) {
			var calls []string
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				calls = append(calls, r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				if mode == "content_policy" {
					w.WriteHeader(403)
					_, _ = io.WriteString(w, `{"error":{"type":"policy_error","code":"content_policy_violation","message":"synthetic policy refusal"}}`)
					return
				}
				if mode == "after_output" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","response_id":"auth_visible","delta":"VISIBLE"}`+"\n\n"+
						"data: "+`{"type":"response.failed","response":{"id":"auth_visible","status":"failed","output":[],"error":{"type":"authentication_error","code":"unauthorized","message":"synthetic revoked"}}}`+"\n\n")
					return
				}
				if mode == "recovered" && r.Header.Get("Authorization") == "Bearer next-token" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "auth_recovered", "[]"))
					return
				}
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
				w.WriteHeader(401)
				_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","code":"unauthorized","message":"Encountered invalidated oauth token for user, failing request"}}`)
			})
			settings, err := p.Settings.Get()
			if err != nil {
				t.Fatal(err)
			}
			settings.Upstream401RecoveryEnabled, settings.AutomaticTokenRefreshEnabled = false, false
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; a.Concurrency = 2; return nil }); err != nil {
				t.Fatal(err)
			}
			token, _ := json.Marshal(map[string]any{"access_token": "next-token", "expires_at": time.Now().Add(time.Hour).Unix()})
			if err := p.Store.ImportWith("next", token, AccountDefaults{Concurrency: 2, Priority: 1, Weight: 1}); err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
			input := `[{"role":"user","content":"synthetic"}]`
			if mode == "opaque" {
				input = `[{"type":"reasoning","encrypted_content":"synthetic-opaque"},{"role":"user","content":"synthetic"}]`
			}
			stream := mode == "exhausted_sse" || mode == "after_output"
			path := "/v1/responses"
			if mode == "exhausted_compact" {
				path += "/compact"
			}
			request := proxyRequest(http.MethodPost, path, fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":%s}`, stream, input))
			request.Header.Set("Session-Id", "auth-status-"+mode)
			if mode == "invalid_client_key" {
				request.Header.Set("Authorization", "Bearer invalid-client-key")
			}
			out := httptest.NewRecorder()
			p.gatewayBPS(out, request)
			wantCalls := []string{"Bearer test-upstream-token", "Bearer next-token"}
			status, recordName := 503, "next"
			switch mode {
			case "opaque", "content_policy", "after_output":
				wantCalls, recordName = wantCalls[:1], "test"
				if mode == "content_policy" {
					status = 403
				}
				if mode == "after_output" {
					status = 200
				}
			case "invalid_client_key":
				wantCalls, status = nil, 401
			case "recovered":
				status = 200
			}
			if out.Code != status || !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("public HTTP=%d calls=%v want HTTP=%d calls=%v", out.Code, calls, status, wantCalls)
			}
			if mode == "invalid_client_key" {
				if !strings.Contains(out.Body.String(), "invalid API key") {
					t.Fatal("client authentication response changed")
				}
				return
			}
			record, ok := p.Metrics.LastFor(recordName)
			if !ok || len(record.Attempts) != len(wantCalls) || record.AttemptsTruncated == nil || *record.AttemptsTruncated {
				t.Fatal("actual attempt ledger incomplete")
			}
			if record.HTTPCode != status {
				t.Fatalf("public HTTP not reflected in logical record: %d", record.HTTPCode)
			}
			for index, attempt := range record.Attempts {
				want, observed := 401, 401
				if mode == "after_output" {
					observed = 200 // Native terminal is 401; its real HTTP transport is 200.
				}
				if mode == "recovered" && index == 1 {
					want, observed = 200, 200
				}
				if mode == "content_policy" {
					want, observed = 403, 403
				}
				if attempt.StatusCode == nil || *attempt.StatusCode != want || attempt.ObservedHTTPStatusCode == nil || *attempt.ObservedHTTPStatusCode != observed {
					t.Fatalf("attempt %d real status changed: %+v", index, attempt)
				}
			}
			if status == 503 {
				var payload struct {
					Error struct{ Code, Type, Message string }
				}
				if json.Unmarshal(out.Body.Bytes(), &payload) != nil || payload.Error.Code != "upstream_authentication_unavailable" || payload.Error.Type != "server_error" {
					t.Fatal("stable public account-authentication classification missing")
				}
				if out.Header().Get("WWW-Authenticate") != "" || strings.Contains(out.Body.String(), "invalidated oauth token") {
					t.Fatal("upstream authentication leaked as client credentials failure")
				}
				if record.Status != "error" || record.ErrorSource != "upstream" || record.FinalUpstreamErrorCode != "unauthorized" || record.FinalUpstreamErrorSource != "upstream" || !strings.Contains(record.Error, "invalidated oauth token") {
					t.Fatalf("actual upstream failure provenance changed: %+v", record)
				}
				if record.UsageKnown != nil && *record.UsageKnown || record.TTFTMs != nil {
					t.Fatal("error response invented inference usage or first output")
				}
			}
			if mode == "after_output" && (!strings.Contains(out.Body.String(), "VISIBLE") || !strings.Contains(out.Body.String(), "unauthorized") || strings.Contains(out.Body.String(), "upstream_authentication_unavailable")) {
				t.Fatal("already delivered stream was rewritten")
			}
			if mode == "content_policy" && !strings.Contains(out.Body.String(), "content_policy_violation") {
				t.Fatal("403 policy response changed")
			}
			if mode == "recovered" && record.Status != "ok" {
				t.Fatal("successful replacement changed")
			}
			for _, name := range []string{"test", "next"} {
				account, err := p.Store.Get(name)
				if err != nil || account.Concurrency != 2 || p.inFlight(gatewayAdmissionKey(account)) != 0 {
					t.Fatal("concurrency configuration or slot leaked")
				}
			}
		})
	}
}
