package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestBPSUpstreamWebSocketPreference(t *testing.T) {
	for _, mode := range []string{"http_json", "http_sse", "websocket", "old_native_helper", "preference_off"} {
		t.Run(mode, func(t *testing.T) {
			var upgrades, generations, posts atomic.Int32
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "http-result", "[]"))
					return
				}
				upgrades.Add(1)
				if r.URL.Path != "/responses" || r.URL.Query().Get("bps_auth_mode") != "chatgpt" || r.URL.Query().Get("bps_client_info") != bpsWebSocketClientInfo || !strings.Contains(r.URL.Query().Get("bps_control_frames"), "cancel") {
					t.Error("BPS handshake profile missing")
				}
				if r.Header.Get("Authorization") != "Bearer test-upstream-token" || !strings.Contains(r.Header.Get("Sec-WebSocket-Protocol"), "openai-bearer.test-upstream-token") {
					t.Error("BPS handshake credential missing")
				}
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"responses"}, InsecureSkipVerify: true})
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				_, raw, err := conn.Read(r.Context())
				if err != nil {
					t.Error("dial context cancellation closed live WS:", err)
					return
				}
				generations.Add(1)
				var fields map[string]json.RawMessage
				if json.Unmarshal(raw, &fields) != nil || string(fields["type"]) != `"response.create"` || len(fields["basispoints_request_id"]) == 0 || len(fields["basispoints_resume_token"]) == 0 || len(fields["previous_response_id"]) > 0 || len(fields["generate"]) > 0 || len(fields["stream_id"]) > 0 {
					t.Error("BPS generation frame was not prepared/isolated")
				}
				if !bytes.Contains(raw, []byte("9007199254740993")) {
					t.Error("request precision changed")
				}
				control, _ := json.Marshal(map[string]any{"type": "basispoints.response.resume_token", "request_id": json.RawMessage(fields["basispoints_request_id"]), "resume_token": "private-resume-sentinel"})
				for _, event := range [][]byte{control, []byte(`{"type":"basispoints.response.heartbeat"}`), []byte(`{"type":"response.output_text.delta","delta":"WS_SENTINEL"}`), []byte(`{"type":"response.completed","response":{"id":"ws-result","status":"completed","output":[{"type":"message","id":"msg_fixture","role":"assistant","content":[{"type":"output_text","text":"WS_SENTINEL"}]}],"usage":{"input_tokens":12,"output_tokens":3,"total_tokens":15}}}`)} {
					if err := conn.Write(r.Context(), websocket.MessageText, event); err != nil {
						t.Error(err)
						return
					}
				}
				// No cancellation control may follow an acknowledged terminal.
				if _, raw, err := conn.Read(r.Context()); err == nil && bytes.Contains(raw, []byte("basispoints.response.cancel")) {
					t.Error("completed generation was cancelled")
				}
			})
			settings, _ := p.Settings.Get()
			settings.PreferWebsocket = mode != "preference_off"
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			if mode == "old_native_helper" {
				// A v1-only helper must not impose a payload limit or switch the
				// generation to HTTP when BPS requires unbounded messages.
				p.NativeHTTP = nativeHTTPFixturePool(t, nil)
				p.NativeWebSocket = true
			}
			server := httptest.NewServer(p)
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var result []byte
			if mode == "websocket" {
				conn, _, err := websocket.Dial(ctx, server.URL+"/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer test-client-key"}}})
				if err != nil {
					t.Fatal(err)
				}
				defer conn.CloseNow()
				if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","stream_id":"lane","model":"gpt-6-astra","input":"9007199254740993","generate":true}`)); err != nil {
					t.Fatal(err)
				}
				for {
					_, frame, err := conn.Read(ctx)
					if err != nil {
						t.Fatal(err)
					}
					result = append(result, frame...)
					if bytes.Contains(frame, []byte(`"type":"response.completed"`)) {
						break
					}
				}
			} else {
				raw := `{"model":"gpt-6-astra","input":"9007199254740993","stream":` + map[bool]string{true: "true", false: "false"}[mode == "http_sse"] + `}`
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/responses", strings.NewReader(raw))
				req.Header.Set("Authorization", "Bearer test-client-key")
				res, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				result, err = io.ReadAll(res.Body)
				_ = res.Body.Close()
				if err != nil || res.StatusCode != http.StatusOK {
					t.Fatalf("downstream failed: status=%d err=%v body=%s", res.StatusCode, err, result)
				}
			}
			if mode == "preference_off" {
				if posts.Load() != 1 || upgrades.Load() != 0 {
					t.Fatal("disabled preference changed HTTP transport")
				}
			} else if upgrades.Load() != 1 || generations.Load() != 1 || posts.Load() != 0 || !bytes.Contains(result, []byte("WS_SENTINEL")) {
				t.Fatalf("preference did not use one WS generation: upgrades=%d generations=%d posts=%d", upgrades.Load(), generations.Load(), posts.Load())
			}
			if bytes.Contains(result, []byte("private-resume-sentinel")) || bytes.Contains(result, []byte("basispoints_resume_token")) || bytes.Contains(result, []byte("test-upstream-token")) {
				t.Fatal("WS control credential leaked")
			}
			if mode != "websocket" {
				records := p.Metrics.RecentForAccount("test", 5)
				if len(records) != 1 || len(records[0].Attempts) != 1 {
					t.Fatalf("upstream attempt not recorded: %+v", records)
				}
				want := "websocket"
				if mode == "preference_off" {
					want = "request"
				}
				if records[0].Attempts[0].Phase != want || records[0].RequestType == "websocket" {
					t.Fatal("upstream transport or downstream request type misreported")
				}
				if want == "websocket" && (records[0].Attempts[0].ObservedHTTPStatusCode == nil || *records[0].Attempts[0].ObservedHTTPStatusCode != http.StatusSwitchingProtocols) {
					t.Fatal("actual WS 101 was replaced by the downstream HTTP 200")
				}
			}
		})
	}
}

func TestBPSUpstreamWebSocketCancelAndIdle(t *testing.T) {
	for _, cause := range []string{"context", "body_close", "idle"} {
		t.Run(cause, func(t *testing.T) {
			cancelled := make(chan bool, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"responses"}, InsecureSkipVerify: true})
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				_, raw, err := conn.Read(r.Context())
				if err != nil {
					cancelled <- false
					return
				}
				var frame map[string]string
				_ = json.Unmarshal(raw, &frame)
				_, raw, err = conn.Read(r.Context())
				var control map[string]string
				_ = json.Unmarshal(raw, &control)
				cancelled <- err == nil && control["type"] == "basispoints.response.cancel" && control["resume_token"] == frame["basispoints_resume_token"] && control["request_id"] == frame["basispoints_request_id"]
			}))
			defer server.Close()
			p := newTestProxy(t, 1)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/responses", nil)
			req.Header.Set("Authorization", "Bearer synthetic")
			res, err := p.bpsWebSocketRequest(req, nil, []byte(`{"model":"fixture","input":[]}`), defaultSettings())
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			switch cause {
			case "context":
				cancel()
			case "body_close":
				_ = res.Body.Close()
			case "idle":
				res.Body.(*idleStreamBody).idle = 30 * time.Millisecond
				_, err := io.ReadAll(res.Body)
				if !errors.Is(err, errUpstreamStreamIdleTimeout) {
					t.Fatal("idle stream did not expire:", err)
				}
			}
			select {
			case ok := <-cancelled:
				if !ok {
					t.Fatal("cancellation closed socket without the BPS control")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancellation control not received")
			}
		})
	}
}

func TestBPSUpstreamWebSocketDisconnectDoesNotReplay(t *testing.T) {
	var upgrades, frames, posts atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			t.Error("generation replayed through HTTP")
			return
		}
		upgrades.Add(1)
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"responses"}, InsecureSkipVerify: true})
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		if _, _, err := conn.Read(r.Context()); err == nil {
			frames.Add(1)
		}
	})
	settings, _ := p.Settings.Get()
	settings.PreferWebsocket = true
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", `{"model":"gpt-6-astra","input":"test","stream":false}`))
	if upgrades.Load() != 1 || frames.Load() != 1 || posts.Load() != 0 || w.Code < 400 {
		t.Fatalf("disconnect was replayed or treated as completion: ws=%d frames=%d post=%d status=%d", upgrades.Load(), frames.Load(), posts.Load(), w.Code)
	}
}

func TestBPSUpstreamWebSocketFallbackOnlyUnsupportedHandshake(t *testing.T) {
	for _, status := range []int{401, 403, 404, 405, 426, 429, 500, 501} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var upgrades, posts atomic.Int32
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					upgrades.Add(1)
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":"synthetic rejection"}`)
					return
				}
				posts.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "fallback-result", "[]"))
			})
			settings, _ := p.Settings.Get()
			settings.PreferWebsocket, settings.Upstream401RecoveryEnabled = true, false
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", `{"model":"gpt-6-astra","input":"test","stream":false}`))
			fallback := status == 404 || status == 405 || status == 426 || status == 501
			wantPosts := int32(0)
			if fallback {
				wantPosts = 1
			}
			if upgrades.Load() != 1 || posts.Load() != wantPosts || fallback && w.Code != http.StatusOK || !fallback && w.Code < 400 {
				t.Fatalf("wrong handshake fallback: upgrades=%d posts=%d status=%d", upgrades.Load(), posts.Load(), w.Code)
			}
			if fallback {
				records := p.Metrics.RecentForAccount("test", 1)
				if len(records) != 1 || len(records[0].Attempts) != 2 || records[0].Attempts[0].Phase != "websocket" || records[0].Attempts[1].Phase != "request" {
					t.Fatal("fallback did not retain both actual transport attempts")
				}
			}
		})
	}
}
