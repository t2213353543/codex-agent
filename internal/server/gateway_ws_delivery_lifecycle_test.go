package server

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A real TCP connection whose downstream data write can fail after Upgrade.
// The wrapper injects only a normal net.Conn error; it does not alter messages
// or call gateway accounting directly.
type gatewayWSDeliveryFaultConn struct {
	net.Conn
	fail *atomic.Bool
	gate *gatewayWSDeliveryGate
}

type gatewayWSDeliveryGate struct {
	enabled   atomic.Bool
	once      sync.Once
	closeOnce sync.Once
	entered   chan struct{}
	release   chan struct{}
	closed    chan struct{}
}

func (c *gatewayWSDeliveryFaultConn) Write(raw []byte) (int, error) {
	if c.gate != nil && c.gate.enabled.Load() {
		c.gate.once.Do(func() { close(c.gate.entered); <-c.gate.release })
	}
	if c.fail.Load() {
		return 0, io.ErrClosedPipe
	}
	return c.Conn.Write(raw)
}

func (c *gatewayWSDeliveryFaultConn) Close() error {
	err := c.Conn.Close()
	if c.gate != nil && c.gate.closed != nil {
		c.gate.closeOnce.Do(func() { close(c.gate.closed) })
	}
	return err
}

type gatewayWSDeliveryFaultWriter struct {
	http.ResponseWriter
	fail *atomic.Bool
	gate *gatewayWSDeliveryGate
}

func (w gatewayWSDeliveryFaultWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err != nil {
		return nil, nil, err
	}
	wrapped := &gatewayWSDeliveryFaultConn{Conn: conn, fail: w.fail, gate: w.gate}
	return wrapped, bufio.NewReadWriter(rw.Reader, bufio.NewWriter(wrapped)), nil
}

func TestGatewayWSDeliveryTerminalWriteFailureIsNotDeliveredSuccess(t *testing.T) {
	for _, kind := range []string{"response.completed", "response.failed", "response.incomplete"} {
		t.Run(kind, func(t *testing.T) { gatewayWSDeliveryTerminalWriteFailure(t, kind) })
	}
}

func gatewayWSDeliveryTerminalWriteFailure(t *testing.T, kind string) {
	var fail atomic.Bool
	sendTerminal := make(chan struct{})
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, _, err := conn.Read(r.Context()); err != nil {
			return
		}
		if err := conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.created","response":{"id":"write-final","status":"in_progress"}}`)); err != nil {
			return
		}
		select {
		case <-sendTerminal:
		case <-r.Context().Done():
			return
		}
		terminal := `{"type":"response.completed","response":{"id":"write-final","status":"completed","usage":{"input_tokens":13,"output_tokens":7}}}`
		if kind == "response.failed" {
			terminal = `{"type":"response.failed","response":{"id":"write-final","status":"failed","error":{"code":"invalid_request_error","message":"synthetic upstream failed"},"usage":{"input_tokens":13,"output_tokens":7}}}`
		}
		if kind == "response.incomplete" {
			terminal = `{"type":"response.incomplete","response":{"id":"write-final","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":13,"output_tokens":7}}}`
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(terminal))
		_, _, _ = conn.Read(r.Context())
	})
	enableGatewayWS(t, p, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.ServeHTTP(gatewayWSDeliveryFaultWriter{ResponseWriter: w, fail: &fail}, r)
	}))
	t.Cleanup(server.Close)
	client, _ := gatewayWSDial(t, server.URL, "delivery-fault")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := client.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"test","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	if raw := gatewayWSRead(t, client); !strings.Contains(string(raw), "response.created") {
		t.Fatal("created barrier not observed")
	}
	fail.Store(true)
	close(sendTerminal)
	if _, _, err := client.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("fixture did not fail downstream terminal write: %v", err)
	}
	waitGatewayWSReleased(t, p)
	rec, ok := p.Metrics.LastFor("test")
	if kind == "response.completed" && (!ok || rec.Status != "aborted" || rec.ErrorSource != "client" || rec.ErrorPhase != "response_write") {
		t.Fatalf("undelivered WS terminal recorded successful: status=%q source=%q phase=%q terminal=%q", rec.Status, rec.ErrorSource, rec.ErrorPhase, rec.TerminalStatus)
	}
	if kind != "response.completed" && (!ok || rec.Status != "error" || rec.ErrorSource != "upstream" || rec.ErrorPhase != "response_event") {
		t.Fatalf("delivery failure covered upstream failure: %+v", rec)
	}
	if rec.TerminalStatus != strings.TrimPrefix(kind, "response.") || rec.ResponseID != "write-final" || rec.InputTokens != 13 || rec.OutputTokens != 7 || rec.HTTPCode != 101 {
		t.Fatal("delivery failure erased actual upstream completion/usage")
	}
}

func TestGatewayWSDeliveryBlockedTerminalDoesNotPinAdmissionOrLoseNextTurn(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "client_disconnect"}[disconnect], func(t *testing.T) { gatewayWSDeliveryBlockedTerminal(t, disconnect) })
	}
}

func gatewayWSDeliveryBlockedTerminal(t *testing.T, disconnect bool) {
	var fail atomic.Bool
	gate := &gatewayWSDeliveryGate{entered: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	startTerminal := make(chan struct{})
	secondRequest := make(chan struct{})
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, _, err = conn.Read(r.Context()); err != nil {
			return
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.created","response":{"id":"blocked-first"}}`))
		select {
		case <-startTerminal:
		case <-r.Context().Done():
			return
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"blocked-first","status":"completed","usage":{"input_tokens":11,"output_tokens":1}}}`))
		if _, _, err = conn.Read(r.Context()); err != nil {
			return
		}
		close(secondRequest)
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"blocked-second","status":"completed","usage":{"input_tokens":22,"output_tokens":2}}}`))
		_, _, _ = conn.Read(r.Context())
	})
	enableGatewayWS(t, p, "")
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Concurrency = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.ServeHTTP(gatewayWSDeliveryFaultWriter{ResponseWriter: w, fail: &fail, gate: gate}, r)
	}))
	t.Cleanup(server.Close)
	client, _ := gatewayWSDial(t, server.URL, "blocked-delivery")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	first := `{"type":"response.create","model":"first","input":[]}`
	if err := client.Write(ctx, websocket.MessageText, []byte(first)); err != nil {
		t.Fatal(err)
	}
	gatewayWSRead(t, client)
	gate.enabled.Store(true)
	close(startTerminal)
	var release sync.Once
	unblock := func() { release.Do(func() { close(gate.release) }) }
	defer unblock()
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal("terminal write barrier not reached")
	}
	if p.inFlight("test") != 0 {
		t.Fatal("already-completed inference retained slot while downstream blocked")
	}
	if len(p.Metrics.RecentForAccount("test", 10)) != 0 {
		t.Fatal("terminal record committed before delivery result")
	}
	if err := client.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"second","previous_response_id":"blocked-first","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondRequest:
	case <-ctx.Done():
		t.Fatal("new turn not admitted while prior delivery blocked")
	}
	if disconnect {
		_ = client.CloseNow()
		// A peer close can race with an already buffered successful write. Wait
		// for the gateway's actual downstream Close before testing write failure.
		select {
		case <-gate.closed:
		case <-ctx.Done():
			t.Fatal("gateway did not observe client disconnect while delivery blocked")
		}
	}
	unblock()
	if disconnect {
		waitGatewayWSReleased(t, p)
		// The pump must finish/join and commit the detached terminal before
		// checking the metric; admission release itself precedes delivery.
		deadline := time.Now().Add(2 * time.Second)
		for len(p.Metrics.RecentForAccount("test", 10)) != 2 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		records := p.Metrics.RecentForAccount("test", 10)
		if len(records) != 2 {
			t.Fatalf("disconnect records=%d", len(records))
		}
		for _, record := range records {
			if record.Model == "first" && (record.Status != "aborted" || record.TerminalStatus != "completed" || record.InputTokens != 11) {
				t.Fatal("disconnect erased observed upstream terminal or claimed delivered success")
			}
			if record.Model == "second" && record.Status != "aborted" {
				t.Fatal("undelivered next response was not aborted")
			}
		}
		return
	}
	if raw := gatewayWSRead(t, client); !strings.Contains(string(raw), "blocked-first") {
		t.Fatal("first terminal bytes reordered")
	}
	if raw := gatewayWSRead(t, client); !strings.Contains(string(raw), "blocked-second") {
		t.Fatal("second terminal lost")
	}
	waitGatewayWSReleased(t, p)
	records := p.Metrics.RecentForAccount("test", 10)
	if len(records) != 2 {
		t.Fatalf("record count=%d", len(records))
	}
	for _, record := range records {
		if record.Status != "ok" {
			t.Fatal("normal delivery marked failed")
		}
	}
}

type gatewayWSRequestWriteFailureBody struct {
	io.ReadWriteCloser
	writes *atomic.Int32
}

func (b *gatewayWSRequestWriteFailureBody) Write(raw []byte) (int, error) {
	b.writes.Add(1)
	return 0, io.ErrClosedPipe
}

func TestGatewayWSDeliveryFirstRequestWriteFailureReleasesOnce(t *testing.T) {
	var writes, requests atomic.Int32
	upstreamClosed := make(chan struct{})
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		defer close(upstreamClosed)
		if _, _, err = conn.Read(r.Context()); err == nil {
			requests.Add(1)
		}
	})
	base := p.Client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	p.Client = &http.Client{Transport: observationFaultTransport(func(r *http.Request) (*http.Response, error) {
		response, err := base.RoundTrip(r)
		if err == nil && response.StatusCode == 101 {
			body, ok := response.Body.(io.ReadWriteCloser)
			if !ok {
				t.Error("real Upgrade response body lacks Write")
				return response, err
			}
			response.Body = &gatewayWSRequestWriteFailureBody{ReadWriteCloser: body, writes: &writes}
		}
		return response, err
	})}
	enableGatewayWS(t, p, "")
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	client, _ := gatewayWSDial(t, server.URL, "upstream-writer-fault")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := client.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"first-write","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("failed request write did not close peers: %v", err)
	}
	select {
	case <-upstreamClosed:
	case <-ctx.Done():
		t.Fatal("request-write failure left upstream alive")
	}
	waitGatewayWSReleased(t, p)
	records := p.Metrics.RecentForAccount("test", 10)
	if len(records) != 1 || records[0].Status != "aborted" || records[0].ErrorSource != "upstream" || records[0].ErrorPhase != "request_write" || records[0].InputTokens != 0 {
		t.Fatalf("ambiguous request write record wrong: %+v", records)
	}
	if writes.Load() != 1 || requests.Load() != 0 {
		t.Fatal("failed request frame was retried or declared delivered")
	}
}

func TestGatewayWSDeliveryFinalizerAndAbortCommitExactlyOnce(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(strings.TrimSpace(map[bool]string{false: "successful", true: "failed"}[failed]), func(t *testing.T) {
			p := newTestProxy(t, 2)
			account, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			turns := gatewayWSTurns{proxy: p, keyID: "admin", idleChanged: make(chan struct{}, 1)}
			var firstReleased, secondReleased atomic.Int32
			gatewayWSIdentityStart(t, &turns, account, "same", "first", "", &firstReleased)
			gatewayWSIdentityStart(t, &turns, account, "same", "second", "", &secondReleased)
			stop, finish := turns.observeFrameDelivery(t.Context(), []byte(`{"type":"response.completed","stream_id":"same","response":{"id":"first-final","status":"completed","usage":{"input_tokens":31,"output_tokens":4}}}`), account)
			if stop || finish == nil || firstReleased.Load() != 1 || secondReleased.Load() != 0 || p.inFlight(account.ID) != 1 {
				t.Fatal("terminal detach did not release only first slot")
			}
			turns.mu.Lock()
			promoted := turns.pending["same"][0].lastActivity
			turns.mu.Unlock()
			if time.Since(promoted) > time.Second {
				t.Fatal("promoted queue inherited old idle age")
			}
			var writeErr error
			if failed {
				writeErr = io.ErrClosedPipe
			}
			var group sync.WaitGroup
			for i := 0; i < 10; i++ {
				group.Add(2)
				go func() { defer group.Done(); finish(writeErr) }()
				go func() { defer group.Done(); turns.abort() }()
			}
			group.Wait()
			if firstReleased.Load() != 1 || secondReleased.Load() != 1 || p.inFlight(account.ID) != 0 {
				t.Fatal("terminal finalizer/abort duplicated resource release")
			}
			records := p.Metrics.RecentForAccount("test", 10)
			if len(records) != 2 {
				t.Fatalf("records=%d", len(records))
			}
			for _, record := range records {
				if record.Model == "first" && (record.InputTokens != 31 || record.TerminalStatus != "completed" || (record.Status == "aborted") != failed) {
					t.Fatal("detached terminal outcome corrupted")
				}
				if record.Model == "second" && (record.Status != "aborted" || record.InputTokens != 0) {
					t.Fatal("abort contaminated queued turn")
				}
			}
		})
	}
}
