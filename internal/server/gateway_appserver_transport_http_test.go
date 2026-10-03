package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Exercise the actual HTTP server boundary: httptest.ResponseRecorder cannot
// catch ErrAbortHandler turning a fully buffered upstream failure into EOF.
func TestAppServerTransportTerminalFailureHTTPDelivery(t *testing.T) {
	for _, scenario := range []string{"invalid-request", "failed", "incomplete", "quota-reselect", "auth-reselect", "prewarm-quota-reselect", "model-reselect", "after-output", "socket-close"} {
		t.Run(scenario, func(t *testing.T) {
			p := newTestProxy(t, 1)
			p.RuntimeModes = true
			if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; a.AccessToken = "bad"; a.RefreshToken = ""; return nil }); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"access_token": "good", "expires_at": time.Now().Add(time.Hour).Unix()})
			if err := p.Store.ImportWith("good", raw, AccountDefaults{Concurrency: 1}); err != nil {
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
			var mu sync.Mutex
			var connections []*appServerTransportFakeWS
			var dials []string
			var modelAccounts []string
			p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(r.URL.Path, "/models") {
					t.Error("unexpected HTTP fallback after generation")
				}
				identity := r.Header.Get("Authorization")
				mu.Lock()
				modelAccounts = append(modelAccounts, identity)
				mu.Unlock()
				models := `{"models":[` + appServerCLITestModel + `]}`
				if scenario == "model-reselect" && identity == "Bearer bad" {
					models = `{"models":[]}`
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(models)), Request: r}, nil
			})}
			p.AppServerCLI = newAppServerCLI(p)
			defer p.AppServerCLI.Close()
			p.AppServerCLI.transport.dial = func(ctx context.Context, headers http.Header, _ *ProxyEntry) (gatewayWSUpstream, *http.Response, error) {
				identity := headers.Get("Authorization")
				conn := newAppServerTransportFakeWS()
				mu.Lock()
				connections = append(connections, conn)
				dials = append(dials, identity)
				mu.Unlock()
				conn.onWrite = func(n int, _ []byte) error {
					if n == 1 {
						if scenario == "prewarm-quota-reselect" && identity == "Bearer bad" {
							conn.reads <- []byte(`{"type":"error","status":429,"error":{"code":"rate_limit_exceeded"}}`)
						} else {
							conn.reads <- []byte(`{"type":"response.completed","response":{"id":"warm","output":[]}}`)
						}
						return nil
					}
					if identity == "Bearer good" {
						conn.reads <- []byte(`{"type":"response.output_text.delta","delta":"RECOVERED"}`)
						conn.reads <- []byte(`{"type":"response.completed","response":{"id":"resp_ok","status":"completed","output":[]}}`)
						return nil
					}
					switch scenario {
					case "socket-close":
						_ = conn.CloseNow()
					case "quota-reselect":
						conn.reads <- []byte(`{"type":"error","status":429,"error":{"code":"rate_limit_exceeded"}}`)
					case "auth-reselect":
						conn.reads <- []byte(`{"type":"error","status":401,"error":{"code":"unauthorized"}}`)
					case "after-output":
						conn.reads <- []byte(`{"type":"response.output_text.delta","delta":"VISIBLE"}`)
						conn.reads <- []byte(`{"type":"error","status":429,"error":{"code":"rate_limit_exceeded"}}`)
					case "failed":
						conn.reads <- []byte(`{"type":"response.failed","response":{"id":"resp_bad","status":"failed","error":{"code":"invalid_request_error","message":"synthetic rejection"}}}`)
					case "incomplete":
						conn.reads <- []byte(`{"type":"response.incomplete","response":{"id":"resp_bad","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`)
					default:
						conn.reads <- []byte(`{"type":"error","error":{"code":"invalid_request_error","message":"[ObjectParam] [tool_choice.namespace] [unknown_parameter] Unknown parameter: tool_choice.namespace"}}`)
					}
					return nil
				}
				return conn, &http.Response{StatusCode: 101, Header: http.Header{}, Body: http.NoBody}, nil
			}
			server := httptest.NewServer(p)
			defer server.Close()
			request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"fixture","input":"synthetic request","stream":true}`))
			request.Header.Set("Authorization", "Bearer test-client-key")
			request.Header.Set("Session-Id", "synthetic-terminal-session")
			client := server.Client()
			client.Timeout = 5 * time.Second
			response, err := client.Do(request)
			if scenario == "socket-close" {
				if err == nil {
					_, err = io.ReadAll(response.Body)
					_ = response.Body.Close()
				}
				if err == nil {
					t.Fatal("incomplete socket close was accepted as EOF")
				}
			} else {
				if err != nil {
					t.Fatalf("complete terminal failure broke HTTP delivery: %v", err)
				}
				body, err := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if err != nil {
					t.Fatalf("complete terminal response was truncated: %v", err)
				}
				switch scenario {
				case "quota-reselect", "auth-reselect", "prewarm-quota-reselect", "model-reselect":
					if !strings.Contains(string(body), "RECOVERED") || strings.Contains(string(body), `"type":"error"`) {
						t.Fatalf("account rejection failed to reselect: %s", body)
					}
				case "after-output":
					if !strings.Contains(string(body), "VISIBLE") || !strings.Contains(string(body), "rate_limit_exceeded") {
						t.Fatal("visible output/error not delivered")
					}
				case "incomplete":
					if !strings.Contains(string(body), "response.incomplete") {
						t.Fatal("incomplete terminal swallowed")
					}
				default:
					if !strings.Contains(string(body), "invalid_request_error") {
						t.Fatalf("structured rejection swallowed: %s", body)
					}
				}
			}
			mu.Lock()
			gotDials := append([]string(nil), dials...)
			gotModels := append([]string(nil), modelAccounts...)
			sockets := append([]*appServerTransportFakeWS(nil), connections...)
			mu.Unlock()
			want := 1
			if scenario == "quota-reselect" || scenario == "auth-reselect" || scenario == "prewarm-quota-reselect" {
				want = 2
			}
			if len(gotDials) != want {
				t.Fatalf("unsafe/missing replay: %v", gotDials)
			}
			if scenario == "model-reselect" && (len(gotModels) != 2 || gotDials[0] != "Bearer good") {
				t.Fatalf("missing-model account not replaced: models=%v dials=%v", gotModels, gotDials)
			}
			for _, socket := range sockets {
				appServerTransportAssertClosed(t, socket)
			}
		})
	}
}
