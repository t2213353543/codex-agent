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
	"time"
)

type appServerFailoverRoundTripper func(*http.Request) (*http.Response, error)

func (f appServerFailoverRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestAppServerCLIFailoverAccountBoundary(t *testing.T) {
	for _, scenario := range []string{"first-invalid", "all-invalid", "after-output", "quota-before-output", "json", "cas-conflict", "legacy-disabled", "delta-pinned"} {
		t.Run(scenario, func(t *testing.T) {
			p := newTestProxy(t, 1)
			p.RuntimeModes = true
			p.AppServerCLI = &appServerCLI{}
			if scenario == "legacy-disabled" {
				p.AppServerCLI = nil
			}
			if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; a.AccessToken = "bad"; a.RefreshToken = ""; return nil }); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"access_token": "good", "expires_at": time.Now().Add(time.Hour).Unix()})
			if err := p.Store.ImportWith("good", raw, AccountDefaults{Concurrency: 1, Priority: 0}); err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
			settings, _ := p.Settings.Get()
			settings.InferenceMode = inferenceCodexCLI
			settings.BuiltinPromptEnabled = false
			settings.AutomaticTokenRefreshEnabled = false
			settings.Upstream401RecoveryEnabled = false
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			calls := []string{}
			p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
				identity := r.Header.Get("Authorization")
				calls = append(calls, identity)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				if !strings.Contains(string(body), `"additional_tools"`) {
					t.Error("client-owned prepared input was lost")
				}
				if identity == "Bearer good" {
					p.admissionMu.Lock()
					var held int64
					for _, count := range p.admission {
						held += atomic.LoadInt64(count)
					}
					p.admissionMu.Unlock()
					if held != 1 {
						t.Errorf("replacement started before prior account lease released: held=%d", held)
					}
					if scenario == "cas-conflict" {
						owner, _ := p.sessionAccount(r.Context(), "admin", "same-session", "")
						if _, _, err := p.switchSessionAccount(r.Context(), "admin", "same-session", owner, "concurrent-owner"); err != nil {
							t.Error(err)
						}
					}
				}
				status := 200
				payload := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_success\"}}\n\n"
				if identity == "Bearer bad" || scenario == "all-invalid" {
					switch scenario {
					case "after-output":
						payload += "data: {\"type\":\"response.output_text.delta\",\"delta\":\"VISIBLE\"}\n\n"
						fallthrough
					case "quota-before-output":
						payload += "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_success\",\"error\":{\"code\":\"rate_limit_exceeded\"}}}\n\n"
					default:
						status = 401
						payload = `{"error":{"code":"unauthorized","message":"synthetic invalid account"}}`
					}
				} else {
					payload += "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n"
					payload += "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_success\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}],\"usage\":{\"input_tokens\":10,\"output_tokens\":1,\"total_tokens\":11}}}\n\n"
				}
				h := http.Header{}
				if status == 200 {
					h.Set("Content-Type", "text/event-stream")
				} else {
					h.Set("Content-Type", "application/json")
				}
				return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(payload)), Request: r}, nil
			})}
			stream := scenario != "json"
			previous := ""
			if scenario == "delta-pinned" {
				previous = `,"previous_response_id":"resp_owned_by_bad_account"`
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"fixture","stream":%t%s,"input":[{"type":"additional_tools","role":"developer","tools":[]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`, stream, previous)))
			request.Header.Set("Authorization", "Bearer test-client-key")
			request.Header.Set("Session-Id", "same-session")
			response := httptest.NewRecorder()
			p.ServeHTTP(response, request)
			wantCalls := 2
			if scenario == "after-output" || scenario == "legacy-disabled" || scenario == "delta-pinned" {
				wantCalls = 1
			}
			if len(calls) != wantCalls || calls[0] != "Bearer bad" || wantCalls == 2 && calls[1] != "Bearer good" {
				t.Fatalf("wrong account order/replay: %v", calls)
			}
			switch scenario {
			case "all-invalid", "legacy-disabled", "delta-pinned":
				wantStatus := http.StatusServiceUnavailable
				if scenario == "legacy-disabled" {
					wantStatus = http.StatusUnauthorized
				}
				if response.Code != wantStatus {
					t.Fatalf("public account error status: got=%d want=%d %s", response.Code, wantStatus, response.Body.String())
				}
			case "after-output":
				if !strings.Contains(response.Body.String(), "VISIBLE") {
					t.Fatal("visible output lost")
				}
			case "cas-conflict":
				if response.Code != 409 || strings.Contains(response.Body.String(), "OK") {
					t.Fatalf("CAS conflict leaked replacement response: %d %s", response.Code, response.Body.String())
				}
			default:
				if response.Code != 200 || !strings.Contains(response.Body.String(), "OK") || strings.Contains(response.Body.String(), "invalid account") {
					t.Fatalf("replacement failed: %d %s", response.Code, response.Body.String())
				}
			}
			p.Metrics.mu.Lock()
			records := append([]RequestRecord(nil), p.Metrics.recent[:p.Metrics.recentAt]...)
			p.Metrics.mu.Unlock()
			if len(records) != 1 {
				t.Fatalf("failover emitted %d records", len(records))
			}
			if scenario != "legacy-disabled" && len(records[0].Attempts) != wantCalls {
				t.Fatalf("attempt accounting mismatch: %+v", records[0].Attempts)
			}
			if scenario == "all-invalid" || scenario == "delta-pinned" {
				if !strings.Contains(response.Body.String(), "upstream_authentication_unavailable") {
					t.Fatal("public dependency classification missing")
				}
				for _, attempt := range records[0].Attempts {
					// This older official path uses the fallback attempt ledger;
					// preserve its actual classified401 and unknown transport observation.
					if attempt.StatusCode == nil || *attempt.StatusCode != 401 || attempt.ObservedHTTPStatusCode != nil {
						t.Fatal("actual upstream authentication status changed")
					}
				}
			}
			bound, err := p.sessionAccount(t.Context(), "admin", "same-session", "")
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "cas-conflict" && bound != "concurrent-owner" {
				t.Fatal("concurrent owner overwritten")
			}
			if scenario == "first-invalid" || scenario == "quota-before-output" || scenario == "json" {
				_, accounts, err := p.Store.List()
				if err != nil {
					t.Fatal(err)
				}
				for _, a := range accounts {
					if a.Name == "good" && bound != gatewayAccountRef(a) {
						t.Fatal("replacement owner was not committed")
					}
				}
			}
			p.admissionMu.Lock()
			defer p.admissionMu.Unlock()
			for _, count := range p.admission {
				if atomic.LoadInt64(count) != 0 {
					t.Fatal("account concurrency lease leaked")
				}
			}
		})
	}
}
