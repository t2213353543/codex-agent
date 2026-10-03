package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGatewayNativeActualAccountConcurrency96(t *testing.T) {
	const concurrent = 96
	entered := make(chan struct{}, concurrent+1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var calls atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"synthetic-concurrency\",\"status\":\"completed\",\"output\":[]}}\n\n")
		case <-r.Context().Done():
		}
	})
	defer unblock()
	if err := p.Store.UpdateAccount("test", func(account *Account) error { account.Concurrency = concurrent; return nil }); err != nil {
		t.Fatal(err)
	}
	realGatewayNativePool(t, p)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	type result struct {
		status int
		body   string
	}
	results := make(chan result, concurrent)
	start := make(chan struct{})
	for range concurrent {
		go func() {
			<-start
			w := httptest.NewRecorder()
			r := proxyRequest("POST", "/responses", `{"model":"test","stream":false,"input":[]}`).WithContext(ctx)
			p.ServeHTTP(w, r)
			results <- result{w.Code, w.Body.String()}
		}()
	}
	close(start)
	for i := range concurrent {
		select {
		case <-entered:
		case result := <-results:
			t.Fatalf("request finished before full upstream concurrency: active=%d status=%d body=%s", i, result.status, result.body)
		case <-ctx.Done():
			t.Fatalf("only %d/%d requests reached upstream simultaneously", i, concurrent)
		}
	}
	if inFlight := p.inFlight("test"); inFlight != concurrent {
		t.Fatalf("account in-flight=%d; want %d", inFlight, concurrent)
	}
	// The transport ceiling is gone, but the configured account ceiling is not.
	excess := httptest.NewRecorder()
	p.ServeHTTP(excess, proxyRequest("POST", "/responses", `{"model":"test","input":[]}`).WithContext(ctx))
	if excess.Code != http.StatusTooManyRequests || excess.Header().Get("Retry-After") != "1" || calls.Load() != concurrent {
		t.Fatalf("account limit bypassed: status=%d upstream=%d", excess.Code, calls.Load())
	}
	unblock()
	for range concurrent {
		select {
		case result := <-results:
			if result.status != http.StatusOK {
				t.Fatalf("admitted request failed: status=%d body=%s", result.status, result.body)
			}
		case <-ctx.Done():
			t.Fatal("native burst did not complete")
		}
	}
	if p.inFlight("test") != 0 {
		t.Fatal("native burst retained account admission")
	}
}

func TestGatewayNativeActualWebSocketConcurrency96(t *testing.T) {
	const concurrent = 96
	var handshakes atomic.Int32
	p, _ := nativeWSFixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		handshakes.Add(1)
		kind, message, err := conn.Read(r.Context())
		if err != nil || conn.Write(r.Context(), kind, message) != nil {
			return
		}
		_, _, _ = conn.Read(r.Context())
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	type result struct {
		conn gatewayWSUpstream
		err  error
	}
	results := make(chan result, concurrent)
	start := make(chan struct{})
	for range concurrent {
		go func() {
			<-start
			conn, _, err := p.dialNativeGatewayWebSocket(ctx, http.Header{}, nil)
			if err == nil {
				err = conn.Write(ctx, websocket.MessageText, []byte("synthetic-concurrent"))
			}
			if err == nil {
				_, message, readErr := conn.Read(ctx)
				err = readErr
				if err == nil && string(message) != "synthetic-concurrent" {
					err = fmt.Errorf("unexpected concurrent WebSocket payload")
				}
			}
			results <- result{conn, err}
		}()
	}
	close(start)
	var connections []gatewayWSUpstream
	defer func() {
		for _, conn := range connections {
			_ = conn.CloseNow()
		}
	}()
	for range concurrent {
		result := <-results
		if result.conn != nil {
			connections = append(connections, result.conn)
		}
		if result.err != nil {
			t.Fatal(result.err)
		}
	}
	child := nativeResourceOnlyChild(t, p.NativeHTTP)
	p.NativeHTTP.mu.Lock()
	active := child.active
	p.NativeHTTP.mu.Unlock()
	if active != concurrent || handshakes.Load() != concurrent {
		t.Fatalf("WebSocket hidden cap: active=%d handshakes=%d", active, handshakes.Load())
	}
	for _, conn := range connections {
		_ = conn.CloseNow()
	}
	p.NativeHTTP.mu.Lock()
	active = child.active
	p.NativeHTTP.mu.Unlock()
	if active != 0 {
		t.Fatalf("WebSocket burst retained %d reservations", active)
	}
}
