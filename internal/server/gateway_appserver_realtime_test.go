package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func appServerRealtimeTestContext(ctx context.Context) context.Context {
	return withGatewayUnboundedPayload(withInferenceMode(ctx, inferenceCodexCLI), true)
}

func appServerRealtimeTestRequest(path, contentType, body string) *http.Request {
	r := proxyRequest(http.MethodPost, path, body)
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("Session-Id", "synthetic-realtime-thread")
	return r.WithContext(appServerRealtimeTestContext(r.Context()))
}

func TestAppServerRealtimeOfficialWriterVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/appserver_realtime_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Binary string `json:"official_binary_sha256"`
		Cases  []struct {
			Case     string `json:"case"`
			Outbound []struct {
				Kind    string            `json:"kind"`
				Method  string            `json:"method"`
				Path    string            `json:"path"`
				Headers map[string]string `json:"headers"`
				Raw     string            `json:"body_utf8"`
				SHA     string            `json:"body_sha256"`
				Body    json.RawMessage   `json:"body"`
				Parts   json.RawMessage   `json:"multipart_parts"`
			} `json:"outbound"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if vectors.Binary != "660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501" || len(vectors.Cases) != 7 {
		t.Fatal("wrong official writer oracle")
	}
	postCount := 0
	for _, vector := range vectors.Cases {
		t.Run(vector.Case, func(t *testing.T) {
			for _, outbound := range vector.Outbound {
				sum := sha256.Sum256([]byte(outbound.Raw))
				if hex.EncodeToString(sum[:]) != outbound.SHA {
					t.Fatal("recorded body hash mismatch")
				}
				if outbound.Method != "POST" || outbound.Kind != "http" {
					continue
				}
				postCount++
				converted, model, err := appServerRealtimeBackendBody([]byte(outbound.Raw), outbound.Headers["content-type"])
				if err != nil || model != "gpt-realtime-1.5" {
					t.Fatalf("captured call conversion: %s %v", model, err)
				}
				expected := outbound.Body
				if len(outbound.Parts) > 0 {
					expected = outbound.Parts
				}
				var got, want any
				if json.Unmarshal(converted, &got) != nil || json.Unmarshal(expected, &want) != nil || !reflect.DeepEqual(got, want) {
					t.Fatal("sdp/session changed from actual official vector")
				}
				if outbound.Headers["content-type"] == "application/json" && string(converted) != outbound.Raw {
					t.Fatal("existing backend JSON changed")
				}
			}
		})
	}
	if postCount != 4 {
		t.Fatal("missing realtime call formats")
	}
}

func TestAppServerRealtimeBodyPreservesUnknownAndRejectsAmbiguity(t *testing.T) {
	const session = `{ "model":"fixture", "n":9007199254740993, "opaque":{"nested":[false,null]} }`
	const multipart = "--b\r\nContent-Disposition: form-data; name=\"sdp\"\r\n\r\nv=offer\r\n\r\n--b\r\nContent-Disposition: form-data; name=\"session\"\r\n\r\n" + session + "\r\n--b--\r\n"
	got, _, err := appServerRealtimeBackendBody([]byte(multipart), "multipart/form-data; boundary=b")
	if err != nil || !bytes.Contains(got, []byte(session)) {
		t.Fatalf("opaque session bytes changed %s %v", got, err)
	}
	for _, bad := range []string{`{}`, `{"sdp":"offer","session":null}`, `{"sdp":"offer","session":[]}`, `{"sdp":7,"session":{}}`} {
		if _, _, err = appServerRealtimeBackendBody([]byte(bad), "application/json"); err == nil {
			t.Fatalf("accepted malformed call %s", bad)
		}
	}
	duplicate := strings.Replace(multipart, "--b--", "--b\r\nContent-Disposition: form-data; name=\"sdp\"\r\n\r\nextra\r\n--b--", 1)
	if _, _, err = appServerRealtimeBackendBody([]byte(duplicate), "multipart/form-data; boundary=b"); err == nil {
		t.Fatal("duplicate part accepted")
	}
}

func TestAppServerRealtimeCallFailoverAndLocationOwner(t *testing.T) {
	p := appServerAuxiliaryTestProxy(t)
	_ = p.Store.UpdateAccount("test", func(a *Account) error { a.AccessToken = "bad"; a.RefreshToken = ""; a.Priority = 100; return nil })
	raw, _ := json.Marshal(map[string]any{"access_token": "good", "chatgpt_account_id": "good-account", "expires_at": time.Now().Add(time.Hour).Unix()})
	if err := p.Store.ImportWith("good", raw, AccountDefaults{Concurrency: 1, Priority: 0}); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	var attempts []string
	body := `{ "sdp":"v=offer\r\n", "session":{"model":"fixture","opaque":9007199254740993} }`
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		attempts = append(attempts, r.Header.Get("Authorization"))
		if r.URL.Host != "chatgpt.com" || r.URL.Path != "/backend-api/codex/realtime/calls" || r.URL.Query().Get("intent") != "quicksilver" || r.URL.Query().Get("architecture") != "avas" {
			t.Errorf("wrong backend %s", r.URL)
		}
		got, _ := io.ReadAll(r.Body)
		if string(got) != body {
			t.Error("failover changed body")
		}
		if r.Header.Get("Authorization") == "Bearer bad" {
			return &http.Response{StatusCode: 401, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"unauthorized"}}`))}, nil
		}
		if r.Header.Get("ChatGPT-Account-Id") != "good-account" {
			t.Error("wrong account identity")
		}
		return &http.Response{StatusCode: 201, Header: http.Header{"Content-Type": {"application/sdp"}, "Location": {"https://api.openai.com/v1/realtime/calls/rtc_test"}}, Body: io.NopCloser(strings.NewReader("v=answer\r\n"))}, nil
	})}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, appServerRealtimeTestRequest("/v1/live", "application/json", body))
	if w.Code != 201 || w.Body.String() != "v=answer\r\n" || w.Header().Get("Location") != "/v1/live/rtc_test" {
		t.Fatalf("bad call response %d %s %v", w.Code, w.Body, w.Header())
	}
	if !reflect.DeepEqual(attempts, []string{"Bearer bad", "Bearer good"}) {
		t.Fatalf("priority/failover %v", attempts)
	}
	owner, err := p.sessionAccount(t.Context(), "admin", "realtime:rtc_test", "")
	account, _ := p.Store.Get("good")
	if err != nil || owner != gatewayAccountRef(account) {
		t.Fatal("call owner not committed")
	}
}

func TestAppServerRealtimeBackendAliasThroughGateway(t *testing.T) {
	p := appServerIdentityBridgeFixture(t)
	identity := httptest.NewRequest(http.MethodGet, appServerClientIdentityPath, nil)
	identity.Header.Set("Authorization", "Bearer test-client-key")
	p.ServeHTTP(httptest.NewRecorder(), identity)
	const body = `{"sdp":"v=offer\r\n","session":{"model":"fixture"}}`
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://chatgpt.com/backend-api/codex/realtime/calls?architecture=avas&intent=quicksilver" {
			t.Fatalf("backend realtime alias target changed: %s", r.URL)
		}
		got, _ := io.ReadAll(r.Body)
		if string(got) != body || r.Header.Get("Authorization") != "Bearer test-upstream-token" || r.Header.Get("ChatGPT-Account-Id") != "test-account" {
			t.Fatal("backend realtime owner/body contract changed")
		}
		return &http.Response{StatusCode: http.StatusCreated, Header: http.Header{"Content-Type": {"application/sdp"}, "Location": {"https://api.openai.com/v1/realtime/calls/rtc_alias"}}, Body: io.NopCloser(strings.NewReader("v=answer\r\n")), Request: r}, nil
	})}
	req := httptest.NewRequest(http.MethodPost, "/backend-api/codex/realtime/calls", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-client-key")
	req.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	p.ServeHTTP(out, req)
	if out.Code != http.StatusCreated || out.Header().Get("Location") != "/v1/realtime/calls/rtc_alias" || out.Body.String() != "v=answer\r\n" {
		t.Fatalf("backend realtime alias failed: %d %s %v", out.Code, out.Body, out.Header())
	}
}

func TestAppServerRealtimeCallCannotExposeUnboundSuccess(t *testing.T) {
	for _, location := range []string{"", "https://evil.invalid/no-call", "/v1/live/rtc_bad%2Fsegment"} {
		t.Run(location, func(t *testing.T) {
			p := appServerAuxiliaryTestProxy(t)
			p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 201, Header: http.Header{"Location": {location}}, Body: io.NopCloser(strings.NewReader("synthetic-private-answer"))}, nil
			})}
			w := httptest.NewRecorder()
			p.gatewayAppServerRealtime(w, appServerRealtimeTestRequest("/v1/realtime/calls", "application/json", `{"sdp":"offer","session":{}}`))
			if w.Code != 502 || strings.Contains(w.Body.String(), "synthetic-private-answer") {
				t.Fatalf("unbound call exposed %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestAppServerRealtimeSidebandBinaryIdentityOwnerConcurrency(t *testing.T) {
	p := appServerAuxiliaryTestProxy(t)
	settings, _ := p.Settings.Get()
	settings.WebsocketEnabled = true
	settings.StickySessionsEnabled = false
	_ = p.Settings.Set(settings)
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Groups = []string{"realtime-fixture"}; return nil }); err != nil {
		t.Fatal(err)
	}
	account, _ := p.Store.Get("test")
	first, _ := p.Keys.Create("first", account.Groups)
	second, _ := p.Keys.Create("second", account.Groups)
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer test-upstream-token" {
			t.Error("call-create owner mismatch")
		}
		return &http.Response{StatusCode: 201, Header: http.Header{"Content-Type": {"application/sdp"}, "Location": {"/realtime/calls/rtc_test"}}, Body: io.NopCloser(strings.NewReader("v=answer\r\n"))}, nil
	})}
	var dialCount atomic.Int32
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-upstream-token" || r.Header.Get("ChatGPT-Account-Id") != "test-account" {
			t.Error("sideband did not reuse call owner authentication")
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(-1)
		for {
			typ, payload, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if conn.Write(r.Context(), typ, payload) != nil {
				return
			}
		}
	}))
	defer upstreamServer.Close()
	dial := appServerRealtimeDialer(func(ctx context.Context, headers http.Header, _ *ProxyEntry, target string) (gatewayWSUpstream, *http.Response, error) {
		dialCount.Add(1)
		if target != "https://api.openai.com/v1/live/rtc_test" {
			t.Errorf("sideband target %s", target)
		}
		if headers.Get("OpenAI-Alpha") != "quicksilver=v2" {
			t.Error("wrong protocol version header")
		}
		return websocket.Dial(ctx, upstreamServer.URL, &websocket.DialOptions{HTTPHeader: headers})
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(appServerRealtimeTestContext(r.Context()), appServerRealtimeDialerKey{}, dial)
		p.gatewayAppServerRealtime(w, r.WithContext(ctx))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	create, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/live", strings.NewReader(`{"sdp":"v=offer\r\n","session":{"model":"fixture"}}`))
	create.Header.Set("Authorization", "Bearer "+first.Key)
	create.Header.Set("Content-Type", "application/json")
	created, err := http.DefaultClient.Do(create)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(created.Body)
	_ = created.Body.Close()
	if created.StatusCode != 201 || created.Header.Get("Location") != "/v1/live/rtc_test" || string(body) != "v=answer\r\n" {
		t.Fatalf("call-create failed %d %s", created.StatusCode, body)
	}
	connect := func(key, path string) (*websocket.Conn, *http.Response, error) {
		return websocket.Dial(ctx, server.URL+path, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + key}}})
	}
	_, res, err := connect(second.Key, "/v1/live/rtc_test")
	if err == nil || res.StatusCode != 404 || dialCount.Load() != 0 {
		t.Fatal("cross-key sideband admitted")
	}
	conn, res, err := connect(first.Key, "/v1/live/rtc_test")
	if err != nil {
		var detail []byte
		if res != nil && res.Body != nil {
			detail, _ = io.ReadAll(res.Body)
		}
		t.Fatalf("sideband upgrade %v %v %s", res, err, detail)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(-1)
	for _, message := range []struct {
		typ  websocket.MessageType
		body []byte
	}{{websocket.MessageText, []byte(`{"type":"session.context.append","opaque":9007199254740993}`)}, {websocket.MessageBinary, bytes.Repeat([]byte{0, 255, 128, 33}, 256<<10)}} {
		if err = conn.Write(ctx, message.typ, message.body); err != nil {
			t.Fatal(err)
		}
		typ, payload, err := conn.Read(ctx)
		if err != nil || typ != message.typ || !bytes.Equal(payload, message.body) {
			t.Fatalf("frame payload changed %v", err)
		}
	}
	_, res, err = connect(first.Key, "/v1/live/rtc_test")
	if err == nil || res.StatusCode != 429 || dialCount.Load() != 1 {
		t.Fatal("account concurrency bypassed")
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
}

func TestAppServerRealtimeHeadersScalarAndConnectionFiltering(t *testing.T) {
	settings, _ := appServerAuxiliaryTestProxy(t).Settings.Get()
	account := Account{AccessToken: "synthetic", AccountID: "account"}
	for _, hop := range []bool{false, true} {
		src := http.Header{"Openai-Alpha": {"quicksilver=v2"}, "openai-alpha": {"quicksilver=v1"}, "X-Session-Id": {"official-realtime-session", "shadow"}, "x-session-id": {"shadow-case"}}
		if hop {
			src.Set("Connection", "OpenAI-Alpha, X-Session-Id")
		}
		dst := make(http.Header)
		if err := appServerRealtimeHeaders(dst, src, settings, account, nil, false); err != nil {
			t.Fatal(err)
		}
		if hop {
			if dst.Get("X-Session-Id") != "" || dst.Get("OpenAI-Alpha") != "quicksilver=v1" {
				t.Fatal("hop-only headers crossed boundary")
			}
		} else if dst.Get("X-Session-Id") != "official-realtime-session" || dst.Get("OpenAI-Alpha") != "quicksilver=v2" {
			t.Fatal("official scalar header lost")
		}
		if dst.Get("Authorization") != "Bearer synthetic" || dst.Get("ChatGPT-Account-Id") != "account" {
			t.Fatal("gateway account identity lost")
		}
	}
}

func TestAppServerRealtimeLeaseIgnoresStickyToggleAndCancelsOwnerConflict(t *testing.T) {
	p := appServerAuxiliaryTestProxy(t)
	settings, _ := p.Settings.Get()
	settings.StickySessionsEnabled = false
	_ = p.Settings.Set(settings)
	account, _ := p.Store.Get("test")
	owner := gatewayAccountRef(account)
	_, _ = p.sessionAccount(t.Context(), "admin", "realtime:rtc_lease", owner)
	lease, stop := p.keepAppServerRealtimeCall(t.Context(), "admin", "realtime:rtc_lease", account, time.Millisecond)
	defer stop()
	if _, changed, err := p.switchSessionAccount(t.Context(), "admin", "realtime:rtc_lease", owner, "concurrent-owner"); err != nil || !changed {
		t.Fatal("fixture owner update failed")
	}
	select {
	case <-lease.Done():
	case <-time.After(time.Second):
		t.Fatal("realtime lease ignored owner conflict when sticky disabled")
	}
}

func TestAppServerRealtimeNativeActualSideband(t *testing.T) {
	p, upstream := nativeWSFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/live/rtc_native" || r.Header.Get("Authorization") != "Bearer test-upstream-token" || r.Header.Get("ChatGPT-Account-Id") != "test-account" {
			t.Error("native sideband identity/path changed")
		}
		if r.Header.Get("Sec-WebSocket-Extensions") != "" {
			t.Error("native Realtime unexpectedly offered Responses compression")
		}
		if r.Header.Get("OpenAI-Alpha") != "quicksilver=v2" || r.Header.Get("X-Session-Id") != "official-session" {
			t.Error("native Realtime protocol headers lost")
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(-1)
		for {
			typ, payload, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if conn.Write(r.Context(), typ, payload) != nil {
				return
			}
		}
	})
	p.AppServerCLI = &appServerCLI{}
	account, _ := p.Store.Get("test")
	_, _ = p.sessionAccount(t.Context(), "admin", "realtime:rtc_native", gatewayAccountRef(account))
	dial := appServerRealtimeDialer(func(ctx context.Context, headers http.Header, proxy *ProxyEntry, target string) (gatewayWSUpstream, *http.Response, error) {
		if target != "https://api.openai.com/v1/live/rtc_native" {
			t.Errorf("nonofficial native target %s", target)
		}
		// Test-only target replacement after asserting the production target.
		// Real Rust TLS/IPC/WebSocket bytes still cross the full handler.
		return p.dialNativeGatewayWebSocketTargetLifetime(ctx, ctx, headers, proxy, upstream.URL+"/v1/live/rtc_native")
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(appServerRealtimeTestContext(r.Context()), appServerRealtimeDialerKey{}, dial)
		p.gatewayAppServerRealtime(w, r.WithContext(ctx))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	conn, res, err := websocket.Dial(ctx, server.URL+"/v1/live/rtc_native", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer test-client-key"}, "X-Session-Id": {"official-session"}}})
	if err != nil {
		t.Fatalf("native Realtime upgrade %v %v", res, err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(-1)
	for _, typ := range []websocket.MessageType{websocket.MessageText, websocket.MessageBinary} {
		payload := []byte(`{"type":"session.context.append","opaque":9007199254740993}`)
		if typ == websocket.MessageBinary {
			payload = bytes.Repeat([]byte{0, 255, 128, 33}, 9<<10)
		}
		if err := conn.Write(ctx, typ, payload); err != nil {
			t.Fatal(err)
		}
		gotTyp, got, err := conn.Read(ctx)
		if err != nil || gotTyp != typ || !bytes.Equal(got, payload) {
			t.Fatalf("native Realtime message changed %v", err)
		}
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
	waitGatewayWSReleased(t, p)
}

func TestAppServerRealtimeSideband401RecoversSameOwnerAndObservesUsage(t *testing.T) {
	p := appServerAuxiliaryTestProxy(t)
	settings, _ := p.Settings.Get()
	settings.WebsocketEnabled = true
	settings.Upstream401RecoveryEnabled = true
	_ = p.Settings.Set(settings)
	account, _ := p.Store.Get("test")
	_, _ = p.sessionAccount(t.Context(), "admin", "realtime:rtc_recover", gatewayAccountRef(account))
	newToken := managedIdentityTestJWT("test-account", "fixture-user")
	var refreshed atomic.Int32
	p.TokenURL = "https://refresh.invalid/token"
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != p.TokenURL {
			t.Errorf("unexpected refresh target %s", r.URL)
		}
		refreshed.Add(1)
		body, _ := json.Marshal(map[string]any{"access_token": newToken, "refresh_token": "rotated", "expires_in": 3600})
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}
	const terminal = `{"type":"response.done","response":{"id":"realtime-billed-1","model":"actual-fixture-model","usage":{"input_tokens":9,"output_tokens":3,"input_token_details":{"cached_tokens":4}}}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.output_text.delta","delta":"reply"}`))
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(terminal))
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(terminal))
		_, _, _ = conn.Read(r.Context())
	}))
	defer up.Close()
	var dials atomic.Int32
	dial := appServerRealtimeDialer(func(ctx context.Context, headers http.Header, _ *ProxyEntry, target string) (gatewayWSUpstream, *http.Response, error) {
		if target != "https://api.openai.com/v1/realtime?call_id=rtc_recover&intent=quicksilver" {
			t.Errorf("sideband target changed %s", target)
		}
		if dials.Add(1) == 1 {
			return nil, &http.Response{StatusCode: 401, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"unauthorized"}}`))}, errNativeWSHandshake
		}
		if headers.Get("Authorization") != "Bearer "+newToken || headers.Get("ChatGPT-Account-Id") != "test-account" {
			t.Error("same owner token recovery lost")
		}
		return websocket.Dial(ctx, up.URL, &websocket.DialOptions{HTTPHeader: headers})
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.gatewayAppServerRealtime(w, r.WithContext(context.WithValue(appServerRealtimeTestContext(r.Context()), appServerRealtimeDialerKey{}, dial)))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, res, err := websocket.Dial(ctx, server.URL+"/v1/realtime?call_id=rtc_recover", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer test-client-key"}}})
	if err != nil {
		t.Fatalf("same-owner recovery failed %v %v", res, err)
	}
	defer conn.CloseNow()
	for i := 0; i < 3; i++ {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && string(raw) != terminal {
			t.Fatal("statistics observation changed raw terminal frame")
		}
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
	waitGatewayWSReleased(t, p)
	if dials.Load() != 2 || refreshed.Load() != 1 {
		t.Fatalf("wrong recovery attempts dials=%d refresh=%d", dials.Load(), refreshed.Load())
	}
	var rec RequestRecord
	deadline := time.Now().Add(time.Second)
	for {
		p.Metrics.mu.Lock()
		if p.Metrics.recentAt > 0 {
			rec = p.Metrics.recent[0]
		}
		count := p.Metrics.recentAt
		p.Metrics.mu.Unlock()
		if count == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if rec.Time == 0 {
		t.Fatal("missing sideband request log")
	}
	if rec.InputTokens != 9 || rec.OutputTokens != 3 || rec.CachedTokens != 4 || rec.ReturnedModel != "actual-fixture-model" || rec.TTFTMs == nil || rec.CachedTokensKnown == nil || !*rec.CachedTokensKnown {
		t.Fatalf("actual observed metadata lost %+v", rec)
	}
}

func TestAppServerRealtimeExactPathsQueriesAndStandaloneAuth(t *testing.T) {
	p := appServerAuxiliaryTestProxy(t)
	settings, _ := p.Settings.Get()
	settings.WebsocketEnabled = true
	_ = p.Settings.Set(settings)
	for _, path := range []string{"/v1/realtime/other", "/v1/live/rtc_%2fescape", "/v1/live/rtc_x/extra", "/v1/live/../responses", "/realtime/calls"} {
		w := httptest.NewRecorder()
		p.gatewayAppServerRealtime(w, appServerRealtimeTestRequest(path, "application/json", `{}`))
		if w.Code != 404 {
			t.Errorf("path admitted %s %d", path, w.Code)
		}
	}
	for _, query := range []string{"url=https%3A%2F%2Fevil.invalid", "call_id=rtc_a&call_id=rtc_b", "host=evil.invalid"} {
		if _, err := appServerRealtimeQuery(query, true); err == nil {
			t.Errorf("query admitted %s", query)
		}
	}
	r := appServerRealtimeTestRequest("/v1/realtime?model=fixture", "application/json", "")
	r.Method = "GET"
	r.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	p.gatewayAppServerRealtime(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "requires API key auth") {
		t.Fatalf("ChatGPT-only standalone WS misrepresented %d %s", w.Code, w.Body)
	}
}
