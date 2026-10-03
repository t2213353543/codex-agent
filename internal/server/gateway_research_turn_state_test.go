package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type researchTurnCapture struct {
	header http.Header
	body   []byte
}

func researchTurnSettings(t *testing.T, p *Proxy) {
	t.Helper()
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.BuiltinPromptEnabled, settings.PromptCacheKeyEnabled = false, false
	settings.CompatibilityNormalizationEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
}

func researchTurnHeaders(turn, state string) http.Header {
	metadata := fmt.Sprintf(`{"turn_id":%q,"source":"synthetic-relay","unknown":{"precise":9007199254740993}}`, turn)
	headers := http.Header{"Authorization": {"Bearer test-client-key"}, "Session-Id": {"relay-root"},
		"Thread-Id": {"relay-child"}, "Conversation_id": {"legacy-conversation"},
		"X-Codex-Turn-Metadata": {metadata}, "X-Codex-Routing-Hint": {"model=relay-model;tier=priority;future=preserved"}}
	if state != "" {
		headers.Set("X-Codex-Turn-State", state)
	}
	return headers
}

func researchTurnAssertHeaders(t *testing.T, actual, expected http.Header) {
	t.Helper()
	for _, name := range []string{"Session-Id", "Thread-Id", "Conversation_id", "X-Codex-Turn-State", "X-Codex-Turn-Metadata", "X-Codex-Routing-Hint"} {
		if actual.Get(name) != expected.Get(name) {
			t.Errorf("relay changed current request %s: got=%q want=%q", name, actual.Get(name), expected.Get(name))
		}
	}
}

func researchTurnHTTP(t *testing.T, target, body string, headers http.Header) (int, http.Header, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target+"/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = headers.Clone()
	res, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	return res.StatusCode, res.Header, raw
}

func TestResearchTurnHTTPToolRecoveryAndNextTurnState(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) {
			if native && (runtime.GOOS != "linux" || os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN") == "") {
				t.Skip("Linux and explicit real Rust helper required")
			}
			captures := make(chan researchTurnCapture, 8)
			var requests, refreshes atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				if r.URL.Path == "/oauth/token" {
					refreshes.Add(1)
					if r.Header.Get("X-Codex-Turn-State") != "" || r.Header.Get("X-Codex-Turn-Metadata") != "" {
						t.Error("inference turn metadata leaked into OAuth request")
					}
					_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"relay-refreshed-at","refresh_token":"relay-refreshed-rt","expires_in":3600}`, "test-account", "fixture-user"))
					return
				}
				captures <- researchTurnCapture{r.Header.Clone(), raw}
				step := requests.Add(1)
				if step == 2 {
					w.Header().Set("X-Codex-Turn-State", "rejected-response-state-must-not-be-adopted")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, `{"error":{"message":"synthetic token expired"}}`)
					return
				}
				state := "http-turn-a"
				if step == 3 {
					state = "later-state-client-oncelock-ignores"
				} else if step >= 4 {
					state = fmt.Sprintf("http-turn-%d", step)
				}
				w.Header().Set("X-Codex-Turn-State", state)
				w.Header().Set("Content-Type", "text/event-stream")
				if step == 5 {
					_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"relay-failed\",\"status\":\"failed\",\"error\":{\"code\":\"invalid_request\",\"message\":\"synthetic failure\"}}}\n\n")
					return
				}
				output := `[]`
				if step == 1 {
					output = `[{"type":"function_call","call_id":"relay-call","name":"inspect","arguments":"{}"}]`
				}
				_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"relay-http-%d\",\"status\":\"completed\",\"output\":%s}}\n\n", step, output)
			})
			researchTurnSettings(t, p)
			if native {
				realGatewayNativePool(t, p)
			}
			gateway := httptest.NewServer(p)
			defer gateway.Close()
			first := `{"model":"relay-model","service_tier":"priority","stream":true,"store":false,"instructions":"fixed","prompt_cache_key":"relay-cache","input":[{"role":"user","content":"first turn"}]}`
			initialHeaders := researchTurnHeaders("user-turn-a", "")
			status, firstResponse, firstWire := researchTurnHTTP(t, gateway.URL, first, initialHeaders)
			if status != 200 || firstResponse.Get("X-Codex-Turn-State") != "http-turn-a" || !strings.Contains(string(firstWire), `"call_id":"relay-call"`) {
				t.Fatal("first HTTP response routing state was not returned to client")
			}
			researchTurnAssertHeaders(t, (<-captures).header, initialHeaders)
			followup := `{"model":"relay-model","service_tier":"priority","stream":true,"store":false,"instructions":"fixed","prompt_cache_key":"relay-cache","input":[{"role":"user","content":"first turn"},{"type":"function_call","call_id":"relay-call","name":"inspect","arguments":"{}"},{"type":"function_call_output","call_id":"relay-call","output":"synthetic result"},{"type":"reasoning","encrypted_content":"opaque+/="}]}`
			followHeaders := researchTurnHeaders("user-turn-a", firstResponse.Get("X-Codex-Turn-State"))
			status, recovered, _ := researchTurnHTTP(t, gateway.URL, followup, followHeaders)
			if status != 200 || recovered.Get("X-Codex-Turn-State") != "later-state-client-oncelock-ignores" || refreshes.Load() != 1 {
				t.Fatal("tool followup 401 recovery lost successful response state or refreshed more than once")
			}
			before, retry := <-captures, <-captures
			researchTurnAssertHeaders(t, before.header, followHeaders)
			researchTurnAssertHeaders(t, retry.header, followHeaders)
			if string(before.body) != string(retry.body) || before.header.Get("Authorization") == retry.header.Get("Authorization") {
				t.Fatal("same-owner retry changed prepared body or failed to rotate only credentials")
			}
			// New ModelClientSession means no old routing token, despite identical
			// thread/cache identity and continuation history in the next turn.
			for index := 0; index < 3; index++ {
				headers := researchTurnHeaders(fmt.Sprintf("new-user-turn-%d", index), "")
				status, _, wire := researchTurnHTTP(t, gateway.URL, first, headers)
				if status != 200 || index == 1 && !strings.Contains(string(wire), "response.failed") {
					t.Fatal("new turn/accepted failure changed stream outcome")
				}
				researchTurnAssertHeaders(t, (<-captures).header, headers)
			}
			if requests.Load() != 6 || refreshes.Load() != 1 {
				t.Fatal("accepted model failure was replayed or another turn was lost")
			}
		})
	}
}

func TestResearchTurnWSMetadataToolContinuationAndFreshConnection(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) {
			if native && (runtime.GOOS != "linux" || os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN") == "") {
				t.Skip("Linux and explicit real Rust helper required")
			}
			var connections atomic.Int32
			captures := make(chan researchTurnCapture, 8)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connection := connections.Add(1)
				w.Header().Set("X-Codex-Turn-State", fmt.Sprintf("handshake-only-%d", connection))
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				for step := 0; ; step++ {
					_, raw, err := conn.Read(r.Context())
					if err != nil {
						return
					}
					captures <- researchTurnCapture{r.Header.Clone(), raw}
					state := fmt.Sprintf("metadata-turn-%d", connection)
					if step > 0 {
						state += "-later-ignore"
					}
					metadata := fmt.Sprintf(`{"type":"response.metadata","response_id":"ws-%d-%d","headers":{"x-codex-turn-state":%q}}`, connection, step, state)
					if conn.Write(r.Context(), websocket.MessageText, []byte(metadata)) != nil {
						return
					}
					output := `[]`
					if step == 0 {
						output = `[{"type":"function_call","call_id":"relay-call","name":"inspect","arguments":"{}"}]`
					}
					terminal := fmt.Sprintf(`{"type":"response.completed","response":{"id":"ws-%d-%d","status":"completed","output":%s}}`, connection, step, output)
					if conn.Write(r.Context(), websocket.MessageText, []byte(terminal)) != nil {
						return
					}
				}
			})
			var p *Proxy
			if native {
				p, _ = nativeWSFixture(t, handler)
			} else {
				p = proxyFixture(t, handler)
				enableGatewayWS(t, p, "")
			}
			researchTurnSettings(t, p)
			gateway := httptest.NewServer(p)
			defer gateway.Close()
			for turn := 1; turn <= 2; turn++ {
				headers := researchTurnHeaders(fmt.Sprintf("ws-user-turn-%d", turn), "")
				conn, res, err := websocket.Dial(t.Context(), gateway.URL+"/responses", &websocket.DialOptions{HTTPHeader: headers})
				if err != nil {
					t.Fatal(err)
				}
				if res.Header.Get("X-Codex-Turn-State") != fmt.Sprintf("handshake-only-%d", turn) {
					_ = conn.CloseNow()
					t.Fatal("upstream handshake header was lost")
				}
				state := ""
				for step := 0; step < 2; step++ {
					input := `[{"role":"user","content":"new turn"}]`
					previous := ""
					if step == 1 {
						input = `[{"type":"function_call_output","call_id":"relay-call","output":"synthetic tool result"}]`
						previous = fmt.Sprintf(`,"previous_response_id":"ws-%d-0"`, turn)
					}
					metadata := map[string]any{"session_id": "relay-root", "thread_id": "relay-child", "turn_id": fmt.Sprintf("ws-user-turn-%d", turn), "x-codex-turn-metadata": headers.Get("X-Codex-Turn-Metadata")}
					if state != "" {
						metadata["x-codex-turn-state"] = state
					}
					encodedMetadata, _ := json.Marshal(metadata)
					request := fmt.Sprintf(`{"type":"response.create","model":"relay-model","service_tier":"priority","instructions":"fixed","prompt_cache_key":"relay-cache","input":%s,"client_metadata":%s%s}`, input, encodedMetadata, previous)
					if conn.Write(t.Context(), websocket.MessageText, []byte(request)) != nil {
						t.Fatal("cannot send WS relay fixture")
					}
					metadataFrame := gatewayWSRead(t, conn)
					var event struct{ Headers map[string]string }
					if json.Unmarshal(metadataFrame, &event) != nil {
						t.Fatal("invalid fixture metadata frame")
					}
					if step == 0 {
						state = event.Headers["x-codex-turn-state"] // CLI OnceLock behavior.
					}
					terminal := gatewayWSRead(t, conn)
					if step == 0 && !strings.Contains(string(terminal), `"call_id":"relay-call"`) {
						t.Fatal("synthetic tool request disappeared before followup")
					}
					actual := <-captures
					researchTurnAssertHeaders(t, actual.header, headers)
					if string(actual.body) != request || state != fmt.Sprintf("metadata-turn-%d", turn) {
						t.Fatal("WS relay changed per-turn metadata, previous ID, exact tool delta or raw request")
					}
					waitGatewayWSReleased(t, p)
				}
				_ = conn.CloseNow()
			}
			if connections.Load() != 2 {
				t.Fatal("new user turn did not get its independent upstream connection")
			}
		})
	}
}

func TestResearchTurnRejectedWSFallbackDoesNotInjectRejectedState(t *testing.T) {
	var handshakes, posts atomic.Int32
	captures := make(chan researchTurnCapture, 2)
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			handshakes.Add(1)
			w.Header().Set("X-Codex-Turn-State", "rejected-upgrade-state")
			w.WriteHeader(http.StatusUpgradeRequired)
			return
		}
		posts.Add(1)
		raw, _ := io.ReadAll(r.Body)
		captures <- researchTurnCapture{r.Header.Clone(), raw}
		w.Header().Set("X-Codex-Turn-State", "accepted-http-state")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"fallback-http\",\"status\":\"completed\",\"output\":[]}}\n\n")
	})
	enableGatewayWS(t, p, "")
	researchTurnSettings(t, p)
	gateway := httptest.NewServer(p)
	defer gateway.Close()
	headers := researchTurnHeaders("fallback-turn", "")
	conn, rejection, err := websocket.Dial(t.Context(), gateway.URL+"/responses", &websocket.DialOptions{HTTPHeader: headers})
	if conn != nil {
		_ = conn.CloseNow()
	}
	if rejection != nil && rejection.Body != nil {
		_ = rejection.Body.Close()
	}
	if err == nil || rejection == nil || rejection.StatusCode != http.StatusUpgradeRequired || rejection.Header.Get("X-Codex-Turn-State") != "rejected-upgrade-state" {
		t.Fatal("controlled WS rejection was not returned intact")
	}
	status, response, _ := researchTurnHTTP(t, gateway.URL, `{"model":"relay-model","service_tier":"priority","stream":true,"store":false,"instructions":"fixed","prompt_cache_key":"relay-cache","input":[{"role":"user","content":"full HTTP input"}]}`, headers)
	if status != 200 || response.Get("X-Codex-Turn-State") != "accepted-http-state" {
		t.Fatal("HTTP fallback lost its own routing response")
	}
	actual := <-captures
	researchTurnAssertHeaders(t, actual.header, headers)
	if strings.Contains(string(actual.body), "previous_response_id") || handshakes.Load() != 1 || posts.Load() != 1 {
		t.Fatal("rejected WS generated an HTTP replay or leaked a continuation link")
	}
}
