package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Real Rust helper, local TLS HTTP and local TLS WebSocket only. Every response
// is synthetic; this verifies wire provenance, never upstream model quality.
func TestGatewayRuntimeIdentityNativeActualHTTPAndWebSocket(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN") == "" {
		t.Skip("set CODEX_NATIVE_HTTP_TEST_BIN on Linux for runtime identity Rust HTTP/WS integration")
	}
	identity, identityPath := runtimeIdentityFixture(t)
	configureRuntimeIdentityFixture(t, identityPath)
	const completed = `{"type":"response.completed","response":{"id":"runtime-identity-synthetic","status":"completed","output":[]}}`
	const nativeUA = "codex_exec/0.155.0 (Windows 11; x86_64) WindowsTerminal (caller suffix)"
	metadata := map[string]string{
		"x-codex-installation-id": "caller-installation", "session_id": "caller-session", "thread_id": "caller-thread",
		"x-codex-window-id": "caller-thread:1", "turn_id": "caller-turn", "root_turn_id": "caller-root-turn",
		"x-codex-turn-metadata": `{"installation_id":"caller-installation","turn_id":"caller-turn","root_turn_id":"caller-root-turn"}`,
	}
	identityHeaders := map[string]string{
		"Session-Id": "caller-session", "Thread-Id": "caller-thread", "X-Client-Request-Id": "caller-thread",
		"X-Codex-Installation-Id": "caller-installation", "X-Codex-Window-Id": "caller-thread:1",
		"X-Codex-Turn-Metadata": `{"turn_id":"caller-turn","root_turn_id":"caller-root-turn"}`,
	}
	type observation struct {
		transport string
		header    http.Header
		body      []byte
		tls       bool
	}
	for _, nativeCaller := range []bool{false, true} {
		name := "fallback"
		if nativeCaller {
			name = "native_passthrough"
		}
		t.Run(name, func(t *testing.T) {
			observed := make(chan observation, 3)
			p, _ := nativeWSFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.CloseNow()
					_, body, err := conn.Read(r.Context())
					if err != nil {
						t.Error(err)
						return
					}
					observed <- observation{"ws", r.Header.Clone(), body, r.TLS != nil}
					if err := conn.Write(r.Context(), websocket.MessageText, []byte(completed)); err != nil {
						t.Error(err)
					}
					_, _, _ = conn.Read(r.Context())
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				transport := "http"
				if r.URL.Path == "/models" {
					transport = "managed"
				}
				observed <- observation{transport, r.Header.Clone(), body, r.TLS != nil}
				if transport == "managed" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"models":[]}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+completed+"\n\n")
			})
			setFingerprintTestSettings(t, p, false, "linux")
			guard := &nativeCLIFallbackGuard{}
			p.Client.Transport = guard
			headers := make(http.Header)
			headers.Set("Authorization", "Bearer test-client-key")
			headers.Set("User-Agent", "curl/8.0")
			headers.Set("X-Oai-Attestation", "caller-proof")
			payload := map[string]any{"model": "test", "input": []any{}, "stream": true}
			if nativeCaller {
				headers.Set("User-Agent", nativeUA)
				// Process and thread originator are legitimately different.
				headers.Set("Originator", "caller-thread-originator")
				headers.Set("Version", "0.155.0")
				for key, value := range identityHeaders {
					headers.Set(key, value)
				}
				payload["client_metadata"] = metadata
			}
			body, _ := json.Marshal(payload)
			request := proxyRequest(http.MethodPost, "/v1/responses", string(body))
			request.Header = headers.Clone()
			recorder := httptest.NewRecorder()
			p.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "runtime-identity-synthetic") {
				t.Fatalf("native HTTP response: %d %s", recorder.Code, recorder.Body.String())
			}
			gw := httptest.NewServer(p)
			defer gw.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			conn, response, err := websocket.Dial(ctx, gw.URL+"/v1/responses", &websocket.DialOptions{HTTPHeader: headers.Clone()})
			if err != nil {
				t.Fatalf("gateway WS: %v (%v)", err, response)
			}
			defer conn.CloseNow()
			payload["type"] = "response.create"
			body, _ = json.Marshal(payload)
			if err := conn.Write(ctx, websocket.MessageText, body); err != nil {
				t.Fatal(err)
			}
			if body := gatewayWSRead(t, conn); !strings.Contains(string(body), "runtime-identity-synthetic") {
				t.Fatal("native WS lost synthetic completed event")
			}
			_ = conn.CloseNow()
			waitGatewayWSReleased(t, p)
			account, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			managed, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/models", nil)
			if err := p.managedUpstreamHeaders(managed, account); err != nil {
				t.Fatal(err)
			}
			managedResponse, err := p.clientFor(nil).Do(managed)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, managedResponse.Body)
			_ = managedResponse.Body.Close()
			if managedResponse.StatusCode != http.StatusOK {
				t.Fatal("managed native HTTP failed")
			}
			for _, transport := range []string{"http", "ws", "managed"} {
				var got observation
				select {
				case got = <-observed:
				case <-ctx.Done():
					t.Fatal("missing upstream wire observation")
				}
				if got.transport != transport || !got.tls {
					t.Fatalf("expected native TLS %s, got %+v", transport, got)
				}
				wantUA, wantOriginator, wantProof := identity.UserAgent, identity.Originator, ""
				preserveCaller := nativeCaller && transport != "managed"
				if preserveCaller {
					wantUA, wantOriginator, wantProof = nativeUA, "caller-thread-originator", "caller-proof"
				}
				for key, want := range map[string]string{"User-Agent": wantUA, "Originator": wantOriginator, "Version": clientVersion, "X-Oai-Attestation": wantProof, "Authorization": "Bearer test-upstream-token"} {
					if got.header.Get(key) != want {
						t.Errorf("%s %s changed or contradicted provenance", transport, key)
					}
				}
				for key, value := range identityHeaders {
					want := ""
					if preserveCaller {
						want = value
					}
					if got.header.Get(key) != want {
						t.Errorf("%s injected or changed caller identity %s", transport, key)
					}
				}
				if transport != "managed" {
					var sent map[string]json.RawMessage
					if json.Unmarshal(got.body, &sent) != nil {
						t.Fatal("invalid native body")
					}
					if preserveCaller {
						// The real application/device/turn identities remain intact.
						// Only session/thread fields receive the admitted owner's
						// cache namespace, including metadata-only requests.
						wantMetadata := make(map[string]string, len(metadata))
						for key, value := range metadata {
							wantMetadata[key] = value
						}
						wantMetadata["session_id"] = gatewayScopeCacheIdentityValue(account, "admin", "session", metadata["session_id"])
						wantMetadata["thread_id"] = gatewayScopeCacheIdentityValue(account, "admin", "thread", metadata["thread_id"])
						var actual map[string]string
						if json.Unmarshal(sent["client_metadata"], &actual) != nil || !reflect.DeepEqual(actual, wantMetadata) {
							t.Errorf("%s lost scoped session/thread or changed installation/window/turn metadata", transport)
						}
					} else if _, exists := sent["client_metadata"]; exists {
						t.Errorf("%s fabricated client metadata for a stateless caller", transport)
					}
				}
			}
			if guard.calls.Load() != 0 {
				t.Fatal("wire check silently used the Go transport instead of the Rust helper")
			}
		})
	}
}
