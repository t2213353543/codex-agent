package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestAppServerPreferredWebSocketAllGenerationProfiles(t *testing.T) {
	for _, profile := range []string{"native_lite", "generic", "hosted_tools", "unsupported", "forbidden", "disconnect"} {
		t.Run(profile, func(t *testing.T) {
			var upgrades, generations, posts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/models") {
					_, _ = io.WriteString(w, `{"models":[`+appServerCLITestModel+`]}`)
					return
				}
				if r.Method == http.MethodPost {
					posts.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "http-fallback", "[]"))
					return
				}
				upgrades.Add(1)
				if profile == "unsupported" || profile == "forbidden" {
					status := http.StatusForbidden
					if profile == "unsupported" {
						status = http.StatusUpgradeRequired
					}
					w.WriteHeader(status)
					return
				}
				if r.Header.Get("Authorization") != "Bearer admitted-synthetic-token" || r.Header.Get("Content-Encoding") != "" {
					t.Error("WS request lost authorization or retained HTTP encoding")
				}
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				_, raw, err := conn.Read(r.Context())
				if err != nil {
					t.Error(err)
					return
				}
				generations.Add(1)
				var body map[string]json.RawMessage
				if json.Unmarshal(raw, &body) != nil || string(body["type"]) != `"response.create"` || !bytes.Contains(raw, []byte("9007199254740993")) {
					t.Error("WS body lost profile/precision")
				}
				if profile == "hosted_tools" && !bytes.Contains(body["tools"], []byte(`"web_search"`)) {
					t.Error("WS body lost hosted tool")
				}
				if string(body["stream"]) != "true" {
					t.Error("WS body differs from official ResponseCreateWsRequest stream field")
				}
				if r.Header.Get("X-Openai-Internal-Codex-Responses-Lite") != "" || profile == "native_lite" && !bytes.Contains(body["client_metadata"], []byte("ws_request_header_x_openai_internal_codex_responses_lite")) {
					t.Error("Lite control was not projected into the WS frame")
				}
				if profile == "disconnect" {
					return
				}
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"ws-result","status":"completed","output":[]}}`))
				_, _, _ = conn.Read(r.Context())
			}))
			defer upstream.Close()
			p := newTestProxy(t, 1)
			settings, _ := p.Settings.Get()
			settings.PreferWebsocket = true
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			a := newAppServerCLI(p)
			defer a.Close()
			ctx, observer := withGatewayAttemptObservation(t.Context())
			attempt := observer.Begin("request", "/responses", time.Now())
			ctx = context.WithValue(ctx, gatewayTransportAttemptKey{}, attempt)
			raw := []byte(`{"model":"fixture","input":[{"role":"user","content":"9007199254740993"}],"stream":true}`)
			if profile == "hosted_tools" {
				raw = []byte(`{"model":"fixture","input":"9007199254740993","tools":[{"type":"web_search"}],"stream":true}`)
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL+"/responses", bytes.NewReader(raw))
			req.Header.Set("Authorization", "Bearer admitted-synthetic-token")
			if profile != "generic" && profile != "hosted_tools" {
				req.Header.Set("X-Openai-Internal-Codex-Responses-Lite", "true")
			}
			res, err := a.RoundTrip(req, upstream.Client(), nil, raw)
			if err != nil || res == nil {
				t.Fatal("WS profile failed:", err)
			}
			data, readErr := io.ReadAll(res.Body)
			_ = res.Body.Close()
			finishGatewayObservedAttempt(attempt, res, err)
			if profile == "disconnect" {
				if readErr == nil || posts.Load() != 0 {
					t.Fatal("generation disconnect was replayed or accepted as EOF")
				}
			} else if profile == "forbidden" {
				if res.StatusCode != http.StatusForbidden || posts.Load() != 0 {
					t.Fatal("denied handshake bypassed through HTTP")
				}
			} else if readErr != nil || !bytes.Contains(data, []byte("response.completed")) {
				t.Fatal("WS terminal not delivered:", readErr)
			}
			attempts := observer.Snapshot().Attempts
			if profile == "unsupported" {
				if posts.Load() != 1 || len(attempts) != 2 || attempts[0].Phase != "websocket" || *attempts[0].ObservedHTTPStatusCode != 426 || attempts[1].Phase != "request" || *attempts[1].ObservedHTTPStatusCode != 200 {
					t.Fatal("unsupported handshake/fallback observation changed")
				}
			} else if profile != "forbidden" && (generations.Load() != 1 || posts.Load() != 0 || len(attempts) != 1 || attempts[0].Phase != "websocket" || *attempts[0].ObservedHTTPStatusCode != 101) {
				t.Fatal("preference did not select one WS generation")
			}
			if upgrades.Load() != 1 {
				t.Fatal("extra WS handshake")
			}
		})
	}
}

func TestPreferredWebSocketAttemptAndExactTerminalBounds(t *testing.T) {
	ctx, observer := withGatewayAttemptObservation(t.Context())
	for range gatewayAttemptObservationLimit + 2 {
		handle := observer.Begin("request", "/responses", time.Now())
		observePreferredWebSocketHandshake(context.WithValue(ctx, gatewayTransportAttemptKey{}, handle), &http.Response{StatusCode: 101})
	}
	for _, handle := range []*gatewayAttemptHandle{{}, {collector: observer, index: -1}, {collector: observer, index: gatewayAttemptObservationLimit}} {
		observePreferredWebSocketHandshake(context.WithValue(ctx, gatewayTransportAttemptKey{}, handle), &http.Response{StatusCode: 101})
	}
	conn := newAppServerTransportFakeWS()
	conn.reads <- []byte(`{"Type":"response.completed","response":{"id":"incorrect-case"}}`)
	body := &gatewayPreferredWebSocketBody{conn: conn, ctx: t.Context()}
	defer body.Close()
	if _, err := io.ReadAll(body); err == nil {
		t.Fatal("case-aliased terminal was accepted")
	}
}

func TestGatewayPreferredWebSocketDirectModelRoutes(t *testing.T) {
	for _, endpoint := range []string{"/responses", "/guardian", "/guardian-classifier"} {
		t.Run(endpoint, func(t *testing.T) {
			var upgrades, posts atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					posts.Add(1)
					w.WriteHeader(400)
					return
				}
				upgrades.Add(1)
				if r.URL.Path != endpoint {
					t.Error("WS endpoint changed")
				}
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				if _, _, err := conn.Read(r.Context()); err != nil {
					t.Error(err)
					return
				}
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"ws-direct","status":"completed","output":[]}}`))
				_, _, _ = conn.Read(r.Context())
			})
			settings, _ := p.Settings.Get()
			settings.PreferWebsocket = true
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			p.ServeHTTP(response, proxyRequest(http.MethodPost, endpoint, `{"model":"fixture","input":"test","stream":false}`))
			if response.Code != 200 || upgrades.Load() != 1 || posts.Load() != 0 {
				t.Fatalf("direct model route did not use WS: HTTP%d upgrades=%d posts=%d", response.Code, upgrades.Load(), posts.Load())
			}
		})
	}
}

func TestBPSPreferredWebSocketConnectionFailureFallsBackBeforeGeneration(t *testing.T) {
	p := newTestProxy(t, 1)
	p.BPS = &bpsRuntime{}
	p.BaseURL = "https://fixture.invalid"
	var upgrades, posts atomic.Int32
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			upgrades.Add(1)
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		}
		posts.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(bpsGatewayFrame("response.completed", "connection-fallback", "[]")))}, nil
	})}
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	settings := defaultSettings()
	settings.PreferWebsocket = true
	ctx, observer := withGatewayAttemptObservation(t.Context())
	r := proxyRequest(http.MethodPost, "/responses", `{}`).WithContext(ctx)
	res, err := p.bpsRequest(ctx, r, account, nil, "admin", "/responses", []byte(`{"model":"fixture","input":[]}`), settings, &RequestRecord{})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if _, err := io.ReadAll(res.Body); err != nil || upgrades.Load() != 1 || posts.Load() != 1 {
		t.Fatal("connection failure was not followed by exactly one HTTP request:", err)
	}
	attempts := observer.Snapshot().Attempts
	if len(attempts) != 2 || attempts[0].Phase != "websocket" || attempts[0].ObservedHTTPStatusCode != nil || attempts[1].Phase != "request" || *attempts[1].ObservedHTTPStatusCode != 200 {
		t.Fatal("connection failure invented handshake status or lost fallback")
	}
}
