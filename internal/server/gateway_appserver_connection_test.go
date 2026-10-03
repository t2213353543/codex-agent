package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type appServerConnectionFixture struct {
	pool       *appServerConnectionPool
	mu         sync.Mutex
	sockets    []*appServerTransportFakeWS
	generation int
	respond    func(*appServerTransportFakeWS, int, map[string]json.RawMessage)
}

func newAppServerConnectionFixture(t *testing.T) *appServerConnectionFixture {
	t.Helper()
	f := &appServerConnectionFixture{pool: newAppServerConnectionPool(&Proxy{})}
	f.pool.dial = func(ctx, lifetime context.Context, headers http.Header, proxy *ProxyEntry) (gatewayWSUpstream, *http.Response, error) {
		conn := newAppServerTransportFakeWS()
		f.mu.Lock()
		f.sockets = append(f.sockets, conn)
		f.mu.Unlock()
		context.AfterFunc(lifetime, func() { _ = conn.CloseNow() })
		conn.onWrite = func(_ int, raw []byte) error {
			var body map[string]json.RawMessage
			_ = json.Unmarshal(raw, &body)
			if string(body["generate"]) == "false" {
				conn.reads <- []byte(`{"type":"response.completed","response":{"id":"resp_warm","output":[]}}`)
				return nil
			}
			f.mu.Lock()
			f.generation++
			n := f.generation
			f.mu.Unlock()
			if f.respond != nil {
				f.respond(conn, n, body)
			} else {
				conn.reads <- []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","output":[]}}`, n))
			}
			return nil
		}
		return conn, &http.Response{StatusCode: 101, Header: make(http.Header), Body: http.NoBody}, nil
	}
	t.Cleanup(f.pool.close)
	return f
}
func (f *appServerConnectionFixture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sockets)
}
func appServerConnectionRequest(ctx context.Context, key, token string, version int64) (*http.Request, *appServerConnectionResult) {
	result := newAppServerConnectionResult()
	ctx = withAppServerConnectionResult(ctx, result)
	ctx = withGatewayRequestRoute(ctx, key, "session-fixture")
	ctx = withGatewayRequestCredentials(ctx, Account{ID: "account-record", CredentialVersion: version})
	ctx = withGatewayNativeAccountScope(ctx, Account{ID: "account-record", AccountID: "account"})
	req := appServerTransportTestRequest(ctx)
	req.Header.Set("Authorization", "Bearer "+token)
	return req, result
}
func appServerConnectionConsume(t *testing.T, response *http.Response, result *appServerConnectionResult) {
	t.Helper()
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	result.Confirm(true)
}
func appServerConnectionNextInput(in appServerRequestInput, text string) appServerRequestInput {
	in.Input = append(append([]json.RawMessage(nil), in.Input...), json.RawMessage(fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}`, text)))
	return in
}

func TestAppServerConnectionReusesOnlyValidatedDelta(t *testing.T) {
	f := newAppServerConnectionFixture(t)
	in := appServerTransportTestInput()
	for i := 0; i < 3; i++ {
		req, result := appServerConnectionRequest(t.Context(), "key", "token", 1)
		res, err := f.pool.roundTrip(req, nil, in, result)
		if err != nil {
			t.Fatal(err)
		}
		appServerConnectionConsume(t, res, result)
		in = appServerConnectionNextInput(in, fmt.Sprintf("next-%d", i))
	}
	if f.count() != 1 {
		t.Fatalf("opened %d sockets", f.count())
	}
	writes := f.sockets[0].snapshot()
	if len(writes) != 4 {
		t.Fatalf("extra warmup/generation: %d", len(writes))
	}
	for i, raw := range writes[1:] {
		var b map[string]json.RawMessage
		_ = json.Unmarshal(raw, &b)
		var input []json.RawMessage
		_ = json.Unmarshal(b["input"], &input)
		if len(input) != 1 || len(b["generate"]) > 0 {
			t.Fatalf("request %d not delta: %s", i, raw)
		}
		want := "resp_warm"
		if i > 0 {
			want = fmt.Sprintf("resp_%d", i)
		}
		if string(b["previous_response_id"]) != fmt.Sprintf("%q", want) {
			t.Fatalf("wrong continuation %s", raw)
		}
	}
	// Changed client history remains authoritative: same socket, full input.
	in = appServerTransportTestInput()
	in.Input = []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"branch"}]}`)}
	req, result := appServerConnectionRequest(t.Context(), "key", "token", 1)
	res, err := f.pool.roundTrip(req, nil, in, result)
	if err != nil {
		t.Fatal(err)
	}
	appServerConnectionConsume(t, res, result)
	writes = f.sockets[0].snapshot()
	var b map[string]json.RawMessage
	_ = json.Unmarshal(writes[len(writes)-1], &b)
	if len(b["previous_response_id"]) > 0 {
		t.Fatal("changed full history reused previous id")
	}
	if f.count() != 1 {
		t.Fatal("history-only change replaced usable socket")
	}
}

func TestAppServerConnectionCredentialKeyProxyThreadIsolation(t *testing.T) {
	f := newAppServerConnectionFixture(t)
	for _, scenario := range []struct {
		key, token, thread string
		version, proxy     int64
	}{
		{"a", "one", "thread", 1, 1}, {"a", "one", "thread", 1, 1}, {"b", "one", "thread", 1, 1},
		{"a", "two", "thread", 2, 1}, {"a", "one", "thread", 1, 2}, {"a", "one", "different", 1, 1},
	} {
		in := appServerTransportTestInput()
		in.ThreadID = scenario.thread
		req, result := appServerConnectionRequest(t.Context(), scenario.key, scenario.token, scenario.version)
		proxy := &ProxyEntry{ID: "route", Version: scenario.proxy, Host: "127.0.0.1", Port: 99, Type: "http", Enabled: true}
		res, err := f.pool.roundTrip(req, proxy, in, result)
		if err != nil {
			t.Fatal(err)
		}
		appServerConnectionConsume(t, res, result)
	}
	if f.count() != 5 {
		t.Fatalf("identity isolation sockets=%d", f.count())
	}
}

func TestAppServerConnectionTurnStateDoesNotCrossTurns(t *testing.T) {
	f := newAppServerConnectionFixture(t)
	f.respond = func(conn *appServerTransportFakeWS, n int, _ map[string]json.RawMessage) {
		conn.reads <- []byte(fmt.Sprintf(`{"type":"response.metadata","headers":{"x-codex-turn-state":"state_%d"}}`, n))
		conn.reads <- []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","output":[]}}`, n))
	}
	in := appServerTransportTestInput()
	for _, turn := range []string{"turn-a", "turn-a", "turn-b", ""} {
		in.ClientMetadata = map[string]string{"turn_id": turn}
		req, result := appServerConnectionRequest(t.Context(), "key", "token", 1)
		res, err := f.pool.roundTrip(req, nil, in, result)
		if err != nil {
			t.Fatal(err)
		}
		appServerConnectionConsume(t, res, result)
		in = appServerConnectionNextInput(in, "next")
	}
	writes := f.sockets[0].snapshot()
	for index, want := range map[int]string{1: "", 2: "state_1", 3: "", 4: ""} {
		var b struct {
			ClientMetadata map[string]string `json:"client_metadata"`
		}
		_ = json.Unmarshal(writes[index], &b)
		if b.ClientMetadata["x-codex-turn-state"] != want {
			t.Fatalf("turn state at write %d: %q", index, b.ClientMetadata["x-codex-turn-state"])
		}
	}
}

func TestAppServerConnectionAwaitingSameSessionCancellationDoesNotKillOwner(t *testing.T) {
	f := newAppServerConnectionFixture(t)
	in := appServerTransportTestInput()
	req, result := appServerConnectionRequest(t.Context(), "key", "token", 1)
	res, err := f.pool.roundTrip(req, nil, in, result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadAll(res.Body); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close() // Outer CAS deliberately still pending.
	waitCtx, cancel := context.WithCancel(t.Context())
	waitReq, waitResult := appServerConnectionRequest(waitCtx, "key", "token", 1)
	done := make(chan error, 1)
	go func() { _, err := f.pool.roundTrip(waitReq, nil, in, waitResult); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter not canceled")
	}
	select {
	case <-f.sockets[0].done:
		t.Fatal("waiter closed active owner's connection")
	default:
	}
	result.Confirm(true)
	req, nextResult := appServerConnectionRequest(t.Context(), "key", "token", 1)
	next, err := f.pool.roundTrip(req, nil, in, nextResult)
	if err != nil {
		t.Fatal(err)
	}
	appServerConnectionConsume(t, next, nextResult)
	if f.count() != 1 {
		t.Fatal("owner socket lost")
	}
}

func TestAppServerConnectionRejectedCompletionAndShortReadEvict(t *testing.T) {
	for _, kind := range []string{"CAS-failed", "short-read", "missing-id", "wrong-id", "terminal-error"} {
		t.Run(kind, func(t *testing.T) {
			f := newAppServerConnectionFixture(t)
			f.respond = func(conn *appServerTransportFakeWS, n int, _ map[string]json.RawMessage) {
				if n > 1 {
					conn.reads <- []byte(`{"type":"response.completed","response":{"id":"next","output":[]}}`)
					return
				}
				switch kind {
				case "missing-id":
					conn.reads <- []byte(`{"type":"response.completed","response":{"output":[]}}`)
				case "wrong-id":
					conn.reads <- []byte(`{"type":"response.created","response":{"id":"one"}}`)
					conn.reads <- []byte(`{"type":"response.completed","response":{"id":"two","output":[]}}`)
				case "terminal-error":
					conn.reads <- []byte(`{"type":"error","status":429,"error":{"code":"rate_limit_exceeded"}}`)
				default:
					conn.reads <- []byte(`{"type":"response.completed","response":{"id":"first","output":[]}}`)
				}
			}
			req, result := appServerConnectionRequest(t.Context(), "key", "token", 1)
			res, err := f.pool.roundTrip(req, nil, appServerTransportTestInput(), result)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "short-read" {
				_, _ = res.Body.Read(make([]byte, 3))
			} else {
				_, _ = io.ReadAll(res.Body)
			}
			_ = res.Body.Close()
			result.Confirm(kind != "CAS-failed" && kind != "short-read")
			appServerTransportAssertClosed(t, f.sockets[0])
			req, result = appServerConnectionRequest(t.Context(), "key", "token", 1)
			res, err = f.pool.roundTrip(req, nil, appServerTransportTestInput(), result)
			if err != nil {
				t.Fatal(err)
			}
			appServerConnectionConsume(t, res, result)
			if f.count() != 2 {
				t.Fatal("unusable connection returned to pool")
			}
		})
	}
}

func TestAppServerConnectionLeaseCancelAfterTerminalBeforeOuterConfirm(t *testing.T) {
	f := newAppServerConnectionFixture(t)
	req, result := appServerConnectionRequest(t.Context(), "key", "token", 1)
	lease, stopLease := context.WithCancel(req.Context())
	req = req.WithContext(lease)
	res, err := f.pool.roundTrip(req, nil, appServerTransportTestInput(), result)
	if err != nil {
		t.Fatal(err)
	}
	// Existing collectors return at terminal, without requiring HTTP EOF.
	_, err = collectResponse(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	stopLease()
	result.Confirm(true)
	nextReq, nextResult := appServerConnectionRequest(t.Context(), "key", "token", 1)
	next, err := f.pool.roundTrip(nextReq, nil, appServerTransportTestInput(), nextResult)
	if err != nil {
		t.Fatal(err)
	}
	appServerConnectionConsume(t, next, nextResult)
	if f.count() != 1 {
		t.Fatalf("natural lease cancellation killed reusable socket: %d", f.count())
	}
}

func TestAppServerConnectionLiveGuardAndIdleExpiry(t *testing.T) {
	f := newAppServerConnectionFixture(t)
	f.pool.idle = 15 * time.Millisecond
	in := appServerTransportTestInput()
	req, result := appServerConnectionRequest(t.Context(), "key", "token", 1)
	res, err := f.pool.roundTrip(req, nil, in, result)
	if err != nil {
		t.Fatal(err)
	}
	appServerConnectionConsume(t, res, result)
	appServerTransportAssertClosed(t, f.sockets[0])
	req, result = appServerConnectionRequest(t.Context(), "key", "token", 1)
	res, err = f.pool.roundTrip(req, nil, in, result)
	if err != nil {
		t.Fatal(err)
	}
	appServerConnectionConsume(t, res, result)
	rejected := errors.New("authority revoked")
	req, result = appServerConnectionRequest(t.Context(), "key", "token", 1)
	req = req.WithContext(withGatewayNativeDispatchGuard(req.Context(), func(context.Context) error { return rejected }))
	_, err = f.pool.roundTrip(req, nil, in, result)
	if !errors.Is(err, rejected) {
		t.Fatal("reuse bypassed latest authority guard")
	}
}

func TestAppServerConnectionOutputPrefixHashesAndNoHistoryRetention(t *testing.T) {
	f := newAppServerConnectionFixture(t)
	output := json.RawMessage(`{"type":"message","id":"msg_answer","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`)
	f.respond = func(conn *appServerTransportFakeWS, n int, _ map[string]json.RawMessage) {
		conn.reads <- []byte(fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":%s}`, output))
		conn.reads <- []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","output":[%s]}}`, n, output))
	}
	in := appServerTransportTestInput()
	req, result := appServerConnectionRequest(t.Context(), "key", "token", 1)
	res, err := f.pool.roundTrip(req, nil, in, result)
	if err != nil {
		t.Fatal(err)
	}
	appServerConnectionConsume(t, res, result)
	in.Input = append(in.Input, output)
	in = appServerConnectionNextInput(in, "followup")
	req, result = appServerConnectionRequest(t.Context(), "key", "token", 1)
	res, err = f.pool.roundTrip(req, nil, in, result)
	if err != nil {
		t.Fatal(err)
	}
	appServerConnectionConsume(t, res, result)
	writes := f.sockets[0].snapshot()
	var sent map[string]json.RawMessage
	_ = json.Unmarshal(writes[2], &sent)
	if strings.Contains(string(sent["input"]), "answer") || !strings.Contains(string(sent["input"]), "followup") || len(sent["previous_response_id"]) == 0 {
		t.Fatal("output prefix was replayed instead of hashed continuation")
	}
}

func TestAppServerConnectionOriginalClientCancelAndShutdownSlowReader(t *testing.T) {
	for _, action := range []string{"client-cancel", "pool-close"} {
		t.Run(action, func(t *testing.T) {
			f := newAppServerConnectionFixture(t)
			f.respond = func(conn *appServerTransportFakeWS, n int, _ map[string]json.RawMessage) {
				conn.reads <- []byte(`{"type":"response.output_text.delta","delta":"client not consuming"}`)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			req, result := appServerConnectionRequest(ctx, "key", "token", 1)
			response, err := f.pool.roundTrip(req, nil, appServerTransportTestInput(), result)
			if err != nil {
				t.Fatal(err)
			}
			if action == "client-cancel" {
				cancel()
			} else {
				f.pool.close()
			}
			done := make(chan error, 1)
			go func() { _, err := io.ReadAll(response.Body); done <- err }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("canceled generation looked complete")
				}
			case <-time.After(time.Second):
				t.Fatal("slow response remained blocked")
			}
			_ = response.Body.Close()
			result.Confirm(false)
			appServerTransportAssertClosed(t, f.sockets[0])
		})
	}
}

func TestAppServerConnectionAndPrivatePrewarmProfileProjection(t *testing.T) {
	for _, pooled := range []bool{false, true} {
		t.Run(fmt.Sprintf("pooled-%t", pooled), func(t *testing.T) {
			f := newAppServerConnectionFixture(t)
			in := appServerTransportTestInput()
			in.ClientMetadata = map[string]string{
				"turn_id":               "client-turn",
				"root_turn_id":          "client-turn",
				"x-codex-turn-metadata": `{"request_kind":"turn","turn_id":"client-turn","root_turn_id":"client-turn","turn_started_at_unix_ms":1700000000000,"tool_namespaces_info":[{"name":"functions"}]}`,
			}
			in.ExtraHeaders = http.Header{}
			in.ExtraHeaders.Set("x-codex-turn-metadata", in.ClientMetadata["x-codex-turn-metadata"])
			var handshake http.Header
			dial := f.pool.dial
			f.pool.dial = func(ctx, lifetime context.Context, headers http.Header, proxy *ProxyEntry) (gatewayWSUpstream, *http.Response, error) {
				handshake = headers.Clone()
				return dial(ctx, lifetime, headers, proxy)
			}
			req, result := appServerConnectionRequest(t.Context(), "key", "token", 1)
			var response *http.Response
			var err error
			if pooled {
				response, err = f.pool.roundTrip(req, nil, in, result)
			} else {
				transport := newAppServerTransport(&Proxy{})
				defer transport.Close()
				transport.dial = func(ctx context.Context, headers http.Header, proxy *ProxyEntry) (gatewayWSUpstream, *http.Response, error) {
					return f.pool.dial(ctx, ctx, headers, proxy)
				}
				response, err = transport.RoundTripPrepared(req, nil, in)
			}
			if err != nil {
				t.Fatal(err)
			}
			appServerConnectionConsume(t, response, result)
			writes := f.sockets[0].snapshot()
			for index, raw := range writes {
				var body struct {
					Metadata map[string]string `json:"client_metadata"`
				}
				_ = json.Unmarshal(raw, &body)
				var canonical map[string]json.RawMessage
				_ = json.Unmarshal([]byte(body.Metadata["x-codex-turn-metadata"]), &canonical)
				if index == 0 {
					if body.Metadata["turn_id"] != "" || len(canonical["root_turn_id"]) > 0 || string(canonical["request_kind"]) != `"prewarm"` || len(canonical["turn_started_at_unix_ms"]) > 0 {
						t.Fatalf("generation metadata leaked into prewarm: %s", raw)
					}
					if len(canonical["tool_namespaces_info"]) == 0 {
						t.Fatal("prewarm lost tool metadata")
					}
				} else if body.Metadata["turn_id"] != "client-turn" || string(canonical["request_kind"]) != `"turn"` {
					t.Fatal("generation metadata mutated")
				}
			}
			var header map[string]json.RawMessage
			_ = json.Unmarshal([]byte(handshake.Get("x-codex-turn-metadata")), &header)
			if string(header["request_kind"]) != `"prewarm"` || len(header["tool_namespaces_info"]) > 0 {
				t.Fatal("handshake did not use projected prewarm metadata")
			}
			if in.ClientMetadata["turn_id"] != "client-turn" {
				t.Fatal("caller snapshot mutated")
			}
		})
	}
}
