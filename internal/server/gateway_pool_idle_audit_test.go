package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestGatewayAuditSSEEventProgressAcrossChunkBoundaries(t *testing.T) {
	cases := []struct {
		name, data string
		complete   bool
	}{
		{"event", "data: {}\n\n", true},
		{"empty_data", "data:\n\n", true},
		{"bare_data", "data\n\n", true},
		{"multiline", "data: one\ndata: two\n\n", true},
		{"CRLF", "data: {}\r\n\r\n", true},
		{"CR_lookahead", "data: {}\r\rx", true},
		{"CR_incomplete", "data: {}\r\r", false},
		{"BOM_unicode", "\ufeffdata: 唐\n\n", true},
		{"late_BOM", "\n\ufeffdata: {}\n\n", false},
		{"double_BOM", "\ufeff\ufeffdata: {}\n\n", false},
		{"comment", ": heartbeat\n\n: heartbeat\n\n", false},
		{"nondata", "event: message\nid: 1\nretry: 1\n\n", false},
		{"blank", "\n\r\n\r\nx", false},
		{"partial", "data: not finished", false},
		{"no_blank_line", "data: {}\n", false},
		{"nonexact_field", "database: {}\nData: {}\n data: {}\n\n", false},
		{"invalid_utf8", "data: \xff\n\n", false},
		{"invalid_before_event", "\xff\n\ndata: {}\n\n", false},
		{"valid_replacement", "data: \ufffd\n\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for size := 1; size <= len(tc.data); size++ {
				var progress gatewaySSEIdleProgress
				got := false
				for offset := 0; offset < len(tc.data); offset += size {
					got = progress.consume([]byte(tc.data[offset:min(offset+size, len(tc.data))])) || got
				}
				if got != tc.complete {
					t.Fatalf("chunk size %d progress=%v want=%v", size, got, tc.complete)
				}
			}
		})
	}
}

type auditPacedStream struct {
	chunks []string
	delay  time.Duration
	done   chan struct{}
	once   sync.Once
	closes atomic.Int32
}

func (b *auditPacedStream) Read(p []byte) (int, error) {
	if len(b.chunks) == 0 {
		return 0, io.EOF
	}
	select {
	case <-b.done:
		return 0, io.ErrClosedPipe
	case <-time.After(b.delay):
	}
	n := copy(p, b.chunks[0])
	b.chunks[0] = b.chunks[0][n:]
	if b.chunks[0] == "" {
		b.chunks = b.chunks[1:]
	}
	return n, nil
}

func (b *auditPacedStream) Close() error {
	b.once.Do(func() { b.closes.Add(1); close(b.done) })
	return nil
}

func TestGatewayAuditSSEIdleTracksEventsAndPreservesBytes(t *testing.T) {
	for _, tc := range []struct {
		name, contentType string
		chunks            []string
		wantTimeout       bool
		acceptSSE         bool
	}{
		{"half_frame", "text/event-stream", []string{"data: {", "\"a\":", "1", "}"}, true, false},
		{"comments", "text/event-stream", []string{": keepalive\n\n", ": keepalive\n\n", ": keepalive\n\n"}, true, false},
		{"metadata", "text/event-stream", []string{"id: 1\n\n", "event: ping\n\n", "retry: 1\n\n"}, true, false},
		{"complete_events", "text/event-stream; charset=utf-8", []string{"data: {}\n\n", "data:\n\n", "data\n\n", "data: x\r\n\r\n"}, false, false},
		{"reset_after_event", "text/event-stream", []string{"data:", " {}\n\n", "data:", " {}\n\n"}, false, false},
		{"non_SSE_body", "application/json", []string{"{", "\"a\":", "1", "}"}, false, false},
		{"missing_type", "", []string{"data: {", "\"a\":", "1", "}"}, true, true},
		{"mislabeled_type", "application/json", []string{"data: {", "\"a\":", "1", "}"}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				original := strings.Join(tc.chunks, "")
				raw := &auditPacedStream{chunks: append([]string(nil), tc.chunks...), delay: 20 * time.Millisecond, done: make(chan struct{})}
				var upstreamCtx context.Context
				client := &http.Client{Timeout: time.Nanosecond, Transport: sourceRefreshRoundTripper(func(req *http.Request) (*http.Response, error) {
					upstreamCtx = req.Context()
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {tc.contentType}}, Body: raw}, nil
				})}
				req, _ := http.NewRequest(http.MethodGet, "http://idle-audit.invalid", nil)
				if tc.acceptSSE {
					req.Header.Set("Accept", "text/event-stream")
				}
				res, err := doStreamingRequestWithIdleTimeout(client, req, 50*time.Millisecond)
				if err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				data, err := io.ReadAll(res.Body)
				if tc.wantTimeout {
					if !errors.Is(err, errUpstreamStreamIdleTimeout) || time.Since(start) != 50*time.Millisecond {
						t.Fatalf("dripping stream duration=%s error=%v", time.Since(start), err)
					}
					if !strings.HasPrefix(original, string(data)) || upstreamCtx.Err() != context.Canceled {
						t.Fatal("timeout altered bytes or left request alive")
					}
				} else if err != nil || string(data) != original {
					t.Fatalf("stream changed: %q error=%v", data, err)
				}
				_ = res.Body.Close()
				_ = res.Body.Close()
				if raw.closes.Load() != 1 || client.Timeout != time.Nanosecond {
					t.Fatal("stream close/client deadline ownership changed")
				}
			})
		})
	}
}

func TestGatewayAuditSSEIdleBudgetExcludesDownstreamWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		raw := &auditPacedStream{chunks: []string{"data:", " {}\n\n", "data:", " {}\n\n"}, delay: 20 * time.Millisecond, done: make(chan struct{})}
		_, cancel := context.WithCancel(context.Background())
		body := &idleStreamBody{body: raw, cancel: cancel, idle: 50 * time.Millisecond, sse: true}
		defer body.Close()
		buf := make([]byte, 32)
		for range 4 {
			if n, err := body.Read(buf); err != nil || n == 0 {
				t.Fatalf("downstream wait charged to upstream budget: bytes=%d error=%v", n, err)
			}
			time.Sleep(time.Second)
		}
		if raw.closes.Load() != 0 {
			t.Fatal("a stopped idle timer closed an active stream")
		}
	})
}

func TestGatewayAuditSSEZeroIdleCancelsWaitingRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		raw := &auditPacedStream{chunks: []string{"data: x\n\n"}, delay: time.Second, done: make(chan struct{})}
		ctx, cancel := context.WithCancel(context.Background())
		body := &idleStreamBody{body: raw, cancel: cancel, sse: true}
		if _, err := body.Read(make([]byte, 32)); !errors.Is(err, errUpstreamStreamIdleTimeout) || ctx.Err() != context.Canceled {
			t.Fatalf("zero timeout error=%v context=%v", err, ctx.Err())
		}
		if _, err := body.Read(make([]byte, 32)); !errors.Is(err, errUpstreamStreamIdleTimeout) {
			t.Fatalf("second read lost timeout cause: %v", err)
		}
	})
}

func TestGatewayAuditShutdownClosesDirectIdleConnection(t *testing.T) {
	closed := make(chan struct{}, 4)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	server.Start()
	defer server.Close()
	p := &Proxy{Client: server.Client()}
	res, err := p.clientFor(nil).Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	p.closeGatewayClients()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("shutdown retained the unpooled direct connection")
	}
}

func TestGatewayAuditSOCKSDoesNotInheritDirectTLSDialers(t *testing.T) {
	var bypassCalls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "through SOCKS")
	}))
	defer upstream.Close()
	socks, entry := nativeSOCKSTestServer(t, upstream.Listener.Addr().String(), "", "")
	baseTransport := upstream.Client().Transport.(*http.Transport).Clone()
	baseTransport.DialTLSContext = func(context.Context, string, string) (net.Conn, error) {
		bypassCalls.Add(1)
		return nil, errors.New("direct TLS bypass")
	}
	baseTransport.DialTLS = func(string, string) (net.Conn, error) {
		bypassCalls.Add(1)
		return nil, errors.New("legacy direct TLS bypass")
	}
	base := &http.Client{Transport: baseTransport}
	for _, mode := range []gatewayClientMode{gatewayClientHTTP, gatewayClientCompact, gatewayClientWebSocket} {
		client := newGatewayProxyClient(base, &entry, mode)
		transport := client.Transport.(*http.Transport)
		if transport.DialTLSContext != nil || transport.DialTLS != nil {
			t.Fatal("SOCKS transport retained a direct TLS route")
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
		res, err := client.Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		data, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		cancel()
		if err != nil || string(data) != "through SOCKS" {
			t.Fatalf("SOCKS TLS response %q error=%v", data, err)
		}
		client.CloseIdleConnections()
	}
	if bypassCalls.Load() != 0 || socks.dials.Load() != 3 || baseTransport.DialTLSContext == nil || baseTransport.DialTLS == nil {
		t.Fatal("direct TLS hook was called or shared transport mutated")
	}
}

type auditRepeatingEvent struct{}

func (auditRepeatingEvent) Read(p []byte) (int, error) { return copy(p, "data: {}\n\n"), nil }
func (auditRepeatingEvent) Close() error               { return nil }

func BenchmarkGatewayAuditSSEIdleRead(b *testing.B) {
	body := &idleStreamBody{body: auditRepeatingEvent{}, cancel: func() {}, idle: time.Minute, sse: true}
	defer body.Close()
	buf := make([]byte, 4096)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := body.Read(buf); err != nil {
			b.Fatal(err)
		}
	}
}
