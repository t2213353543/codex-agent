package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the Go adapter against a controllable IPC peer. It performs no
// upstream requests and deliberately does not rely on Rust's own validation.
func nativeAuditHTTPPeer(t *testing.T, serve func(net.Conn, map[string]json.RawMessage)) (*gatewayNativeRoundTripper, *gatewayNativeProcess) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	key := gatewayNativeKey{origin: "http://audit.invalid"}
	child := &gatewayNativeProcess{key: key, address: listener.Addr().String(), token: "synthetic",
		done: make(chan struct{}), lastUsed: time.Now()}
	pool := &gatewayNativePool{entries: map[gatewayNativeKey]*gatewayNativeProcess{key: child},
		options: gatewayNativeOptions{network: "tcp", idleProcessLimit: 16}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		var size uint32
		if binary.Read(conn, binary.BigEndian, &size) != nil || size > gatewayNativeHeadLimit {
			return
		}
		raw := make([]byte, size)
		if _, err := io.ReadFull(conn, raw); err != nil {
			return
		}
		var head map[string]json.RawMessage
		if json.Unmarshal(raw, &head) != nil {
			return
		}
		var length int
		if json.Unmarshal(head["body_length"], &length) != nil || length < 0 || length > gatewayNativeBodyLimit {
			return
		}
		if _, err := io.CopyN(io.Discard, conn, int64(length)); err != nil {
			return
		}
		serve(conn, head)
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			t.Error("synthetic IPC peer did not terminate")
		}
		pool.mu.Lock()
		defer pool.mu.Unlock()
		if child.active != 0 {
			t.Errorf("request leaked %d active reservations", child.active)
		}
	})
	return &gatewayNativeRoundTripper{pool: pool}, child
}

func nativeAuditFrame(conn io.Writer, kind byte, data []byte) {
	var head [5]byte
	head[0] = kind
	binary.BigEndian.PutUint32(head[1:], uint32(len(data)))
	_, _ = conn.Write(head[:])
	_, _ = conn.Write(data)
}

func nativeAuditResponse(conn io.Writer, status int, headers any, body string, end bool) {
	raw, _ := json.Marshal(map[string]any{"status": status, "version": "HTTP/2", "headers": headers})
	nativeAuditFrame(conn, 1, raw)
	if body != "" {
		nativeAuditFrame(conn, 2, []byte(body))
	}
	if end {
		nativeAuditFrame(conn, 3, nil)
	}
}

func TestGatewayNativeAuditResponseLengthAndNoBody(t *testing.T) {
	for _, scenario := range []struct {
		name, method, length, body string
		status                     int
		wantLength                 int64
		want                       error
	}{
		{name: "exact", length: "3", body: "abc", wantLength: 3},
		{name: "zero", length: "0", wantLength: 0},
		{name: "leading-zeros", length: "003", body: "abc", wantLength: 3},
		{name: "plus", length: "+3", body: "abc", want: errGatewayNativeProtocol},
		{name: "minus-zero", length: "-0", want: errGatewayNativeProtocol},
		{name: "empty", length: "", want: errGatewayNativeProtocol},
		{name: "space", length: " 3", body: "abc", want: errGatewayNativeProtocol},
		{name: "overflow", length: "9223372036854775808", want: errGatewayNativeProtocol},
		{name: "unicode", length: "３", want: errGatewayNativeProtocol},
		{name: "short", length: "4", body: "abc", wantLength: 4, want: io.ErrUnexpectedEOF},
		{name: "long", length: "2", body: "abc", wantLength: 2, want: errGatewayNativeProtocol},
		{name: "head", method: "HEAD", length: "9", wantLength: 9},
		{name: "head-data", method: "HEAD", length: "9", body: "x", wantLength: 9, want: errGatewayNativeProtocol},
		{name: "no-content", status: 204, length: "0", wantLength: 0},
		{name: "not-modified", status: 304, length: "9", wantLength: 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			status := scenario.status
			if status == 0 {
				status = 200
			}
			rt, _ := nativeAuditHTTPPeer(t, func(conn net.Conn, _ map[string]json.RawMessage) {
				headers := [][2]string{{"content-length", base64.StdEncoding.EncodeToString([]byte(scenario.length))}}
				nativeAuditResponse(conn, status, headers, scenario.body, true)
			})
			req, _ := http.NewRequestWithContext(t.Context(), scenario.method, "http://audit.invalid/length", nil)
			res, err := rt.RoundTrip(req)
			if err == nil {
				if res.ContentLength != scenario.wantLength {
					t.Errorf("ContentLength=%d want %d", res.ContentLength, scenario.wantLength)
				}
				_, err = io.ReadAll(res.Body)
				_ = res.Body.Close()
			}
			if !errors.Is(err, scenario.want) {
				t.Fatalf("result=%v want %v", err, scenario.want)
			}
		})
	}
}

func TestGatewayNativeAuditResponseHeaders(t *testing.T) {
	encode := func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }
	for _, scenario := range []struct {
		name    string
		headers [][]string
		valid   bool
	}{
		{"opaque-and-tab", [][]string{{"X-Bytes", encode("\t\x80\xff")}}, true},
		{"nul", [][]string{{"X-Bytes", encode("\x00")}}, false},
		{"control", [][]string{{"X-Bytes", encode("\x01")}}, false},
		{"vertical-tab", [][]string{{"X-Bytes", encode("\x0b")}}, false},
		{"delete", [][]string{{"X-Bytes", encode("\x7f")}}, false},
		{"bad-name", [][]string{{"Bad Name", encode("value")}}, false},
		{"missing-value", [][]string{{"X-Bytes"}}, false},
		{"extra-value", [][]string{{"X-Bytes", encode("value"), "ignored"}}, false},
		{"duplicate-length", [][]string{{"Content-Length", encode("0")}, {"content-length", encode("0")}}, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			rt, _ := nativeAuditHTTPPeer(t, func(conn net.Conn, _ map[string]json.RawMessage) {
				nativeAuditResponse(conn, 200, scenario.headers, "", true)
			})
			req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://audit.invalid/headers", nil)
			res, err := rt.RoundTrip(req)
			if res != nil {
				_, _ = io.ReadAll(res.Body)
				_ = res.Body.Close()
			}
			if scenario.valid && err != nil || !scenario.valid && !errors.Is(err, errGatewayNativeProtocol) {
				t.Fatalf("valid=%t err=%v", scenario.valid, err)
			}
		})
	}
}

func TestGatewayNativeAuditRemainingDeadline(t *testing.T) {
	for _, budget := range []time.Duration{0, 2 * time.Second, 25 * time.Hour} {
		t.Run(budget.String(), func(t *testing.T) {
			captured := make(chan map[string]json.RawMessage, 1)
			rt, _ := nativeAuditHTTPPeer(t, func(conn net.Conn, head map[string]json.RawMessage) {
				captured <- head
				nativeAuditResponse(conn, 200, nil, "", true)
			})
			ctx := context.Background()
			if budget != 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, budget)
				defer cancel()
			}
			// Spend budget before the actual dispatch checkpoint.
			checks := 0
			rt.checkProxy = func() error {
				checks++
				if checks == 2 {
					time.Sleep(15 * time.Millisecond)
				}
				return nil
			}
			req, _ := http.NewRequestWithContext(ctx, "POST", "http://audit.invalid/deadline", strings.NewReader("once"))
			res, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.ReadAll(res.Body)
			_ = res.Body.Close()
			head := <-captured
			raw, ok := head["total_timeout_ms"]
			if budget == 0 || budget > 24*time.Hour {
				if ok {
					t.Fatalf("unexpected helper deadline %s", raw)
				}
			} else {
				var milliseconds int64
				if !ok || json.Unmarshal(raw, &milliseconds) != nil || milliseconds <= 0 || milliseconds > budget.Milliseconds()-10 {
					t.Fatalf("deadline did not use remaining budget: %s", raw)
				}
			}
		})
	}
}

type nativeAuditErrorReader struct{ err error }

func (r nativeAuditErrorReader) Read([]byte) (int, error) { return 0, r.err }

type nativeAuditShortWriter struct {
	call, shortAt int
}

func (w *nativeAuditShortWriter) Write(data []byte) (int, error) {
	w.call++
	if w.call == w.shortAt {
		return len(data) - 1, nil
	}
	return len(data), nil
}

func TestGatewayNativeAuditIPCReadAndWriteFailures(t *testing.T) {
	transportFailure := errors.New("synthetic transport failure")
	for _, prefix := range [][]byte{nil, {2, 0, 0, 0, 3, 'x'}} {
		reader := io.MultiReader(bytes.NewReader(prefix), nativeAuditErrorReader{transportFailure})
		if _, _, err := gatewayNativeReadFrame(reader); !errors.Is(err, transportFailure) {
			t.Fatalf("non-EOF transport failure was replaced: %v", err)
		}
		if _, _, err := gatewayNativeReadFrame(bytes.NewReader(prefix)); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("truncation became successful EOF: %v", err)
		}
	}
	for _, shortAt := range []int{1, 2} {
		writer := &nativeAuditShortWriter{shortAt: shortAt}
		if err := gatewayNativeWriteJSON(writer, map[string]int{"protocol": 1}); !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("short write at %d ignored: %v", shortAt, err)
		}
		if writer.call != shortAt {
			t.Fatal("writer continued after ambiguous partial frame")
		}
	}
	if err := gatewayNativeWriteBytes(&nativeAuditShortWriter{shortAt: 1}, []byte("body")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("short request body write ignored")
	}
	for _, frame := range [][]byte{{5, 0, 0, 0, 0}, {3, 0, 0, 0, 1}, {2, 0, 1, 0, 1}} {
		if _, _, err := gatewayNativeReadFrame(bytes.NewReader(frame)); !errors.Is(err, errGatewayNativeProtocol) {
			t.Fatalf("invalid frame accepted: %v", err)
		}
	}
}

type nativeAuditRequestBody struct {
	reader io.Reader
	reads  atomic.Int32
	closed atomic.Int32
}

func (b *nativeAuditRequestBody) Read(data []byte) (int, error) {
	b.reads.Add(1)
	return b.reader.Read(data)
}
func (b *nativeAuditRequestBody) Close() error { b.closed.Add(1); return nil }

func TestGatewayNativeAuditInvalidMetadataBeforeBodyRead(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		amend func(*http.Request)
	}{
		{"nil-url", func(r *http.Request) { r.URL = nil }},
		{"invalid-method", func(r *http.Request) { r.Method = "G ET" }},
		{"unsupported-connect", func(r *http.Request) { r.Method = "CONNECT" }},
		{"unsupported-trace", func(r *http.Request) { r.Method = "TRACE" }},
		{"opaque-url", func(r *http.Request) { r.URL.Opaque = "//different.invalid/path" }},
		{"transfer-encoding", func(r *http.Request) { r.TransferEncoding = []string{"chunked"} }},
		{"invalid-name", func(r *http.Request) { r.Header["Bad Name"] = []string{"x"} }},
		{"invalid-value", func(r *http.Request) { r.Header.Set("X-Test", "\x7f") }},
		{"many-values", func(r *http.Request) { r.Header["X-Test"] = make([]string, 129) }},
		{"huge-value", func(r *http.Request) { r.Header.Set("X-Test", strings.Repeat("x", gatewayNativeHeadLimit+1)) }},
		{"encoded-limit", func(r *http.Request) { r.Header.Set("X-Test", strings.Repeat("x", gatewayNativeHeadLimit*3/4)) }},
		{"noncanonical-length", func(r *http.Request) { r.Header["content-length"] = []string{"+1"} }},
		{"duplicate-case-length", func(r *http.Request) {
			r.Header["Content-Length"] = []string{"1"}
			r.Header["content-length"] = []string{"1"}
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			body := &nativeAuditRequestBody{reader: strings.NewReader("never-read")}
			req, _ := http.NewRequestWithContext(t.Context(), "POST", "http://audit.invalid/", body)
			scenario.amend(req)
			// A nil pool deliberately makes an unexpected acquisition fail hard.
			_, err := (&gatewayNativeRoundTripper{}).RoundTrip(req)
			if err == nil || body.reads.Load() != 0 || body.closed.Load() != 1 {
				t.Fatalf("invalid metadata: err=%v reads=%d closes=%d", err, body.reads.Load(), body.closed.Load())
			}
		})
	}
}

func TestGatewayNativeAuditConsumedFrameReleasesBuffer(t *testing.T) {
	rt, _ := nativeAuditHTTPPeer(t, func(conn net.Conn, _ map[string]json.RawMessage) {
		nativeAuditResponse(conn, 200, nil, strings.Repeat("x", gatewayNativeHeadLimit), true)
	})
	req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://audit.invalid/large-frame", nil)
	res, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if count, err := io.Copy(io.Discard, res.Body); err != nil || count != gatewayNativeHeadLimit {
		t.Fatalf("response count=%d err=%v", count, err)
	}
	if res.Body.(*gatewayNativeBody).pending != nil {
		t.Fatal("consumed frame retained a 64 KiB backing buffer")
	}
}

type nativeAuditBlockingBody struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
	closes  atomic.Int32
}

func (b *nativeAuditBlockingBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.closed
	return 0, io.ErrClosedPipe
}
func (b *nativeAuditBlockingBody) Close() error {
	if b.closes.Add(1) == 1 {
		close(b.closed)
	}
	return nil
}

func TestGatewayNativeAuditCancellationClosesBodyExactlyOnce(t *testing.T) {
	body := &nativeAuditBlockingBody{started: make(chan struct{}), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://audit.invalid/", body)
	done := make(chan error, 1)
	go func() { _, err := (&gatewayNativeRoundTripper{}).RoundTrip(req); done <- err }()
	select {
	case <-body.started:
	case <-time.After(time.Second):
		t.Fatal("request body was not read")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || body.closes.Load() != 1 {
			t.Fatalf("cancel result=%v closes=%d", err, body.closes.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt body read")
	}
}

func TestGatewayNativeAuditReaperKeepsReadyWaiter(t *testing.T) {
	key := gatewayNativeKey{origin: "http://audit.invalid"}
	child := &gatewayNativeProcess{key: key, done: make(chan struct{}), finished: make(chan struct{}),
		ready: make(chan struct{}), startupWaiters: 1, lastUsed: time.Now().Add(-time.Hour)}
	close(child.ready)
	pool := &gatewayNativePool{entries: map[gatewayNativeKey]*gatewayNativeProcess{key: child},
		options: gatewayNativeOptions{idle: 5 * time.Millisecond}, stop: make(chan struct{}), done: make(chan struct{})}
	pool.workers.Add(1)
	go pool.reapIdle()
	t.Cleanup(func() { _ = pool.Close() })
	time.Sleep(30 * time.Millisecond)
	pool.mu.Lock()
	retired := child.retired
	pool.mu.Unlock()
	if retired {
		t.Fatal("idle reaper evicted helper reserved by a ready startup waiter")
	}
	if err := pool.waitStartup(t.Context(), child); err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	fresh := time.Since(child.lastUsed) < time.Second
	pool.mu.Unlock()
	if !fresh {
		t.Fatal("ready waiter retained startup duration as idle age")
	}
}

func TestGatewayNativeAuditStartupCancellationAndConcurrentClose(t *testing.T) {
	pool, entered, release, _ := nativeStartupBarrierPool(t)
	ctx, cancel := context.WithCancel(t.Context())
	result := nativeStartupAcquireAsync(pool, ctx, gatewayNativeKey{origin: "http://audit.invalid"})
	nativeStartupWaitSignal(t, entered)
	cancel()
	if received := nativeStartupReceive(t, result); !errors.Is(received.err, context.Canceled) {
		t.Fatalf("startup cancellation result=%v", received.err)
	}
	var wait sync.WaitGroup
	for range 8 {
		wait.Go(func() { _ = pool.Close() })
	}
	release()
	done := make(chan struct{})
	go func() { wait.Wait(); close(done) }()
	nativeStartupWaitSignal(t, done)
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.entries) != 0 {
		t.Fatal("concurrent Close retained abandoned startup")
	}
}
