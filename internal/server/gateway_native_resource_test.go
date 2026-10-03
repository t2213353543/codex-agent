package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func nativeResourceOnlyChild(t *testing.T, pool *gatewayNativePool) *gatewayNativeProcess {
	t.Helper()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.entries) != 1 {
		t.Fatalf("native resource fixture has %d process entries; want one", len(pool.entries))
	}
	for _, child := range pool.entries {
		return child
	}
	return nil
}

func nativeResourceWait(t *testing.T, done <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal(message)
	}
}

func nativeResourceAssertReaped(t *testing.T, child *gatewayNativeProcess) {
	t.Helper()
	nativeResourceWait(t, child.finished, "exited helper retained its route without another request")
	nativeResourceWait(t, child.done, "helper was not collected by Cmd.Wait")
	if child.command.ProcessState == nil {
		t.Fatal("exited helper has no collected OS process state")
	}
	if _, err := os.Stat(child.dir); !os.IsNotExist(err) {
		t.Fatal("exited helper retained its private IPC directory")
	}
	if child.bridge != nil {
		nativeResourceWait(t, child.bridge.done, "exited helper retained its SOCKS listener")
		child.bridge.mu.Lock()
		connections := len(child.bridge.clients)
		child.bridge.mu.Unlock()
		if connections != 0 {
			t.Fatal("exited helper retained SOCKS connection reservations")
		}
	}
}

func TestGatewayNativeResourceExitedChildIsReapedWithoutNewTraffic(t *testing.T) {
	pool := nativeHTTPFixturePool(t, nil)
	proxy := &ProxyEntry{ID: "synthetic-resource", Type: "socks5", Host: "127.0.0.1", Port: 1234, Version: 1, Enabled: true}
	for range 3 {
		response := nativeHTTPFixtureRequest(t, pool.RoundTripper(proxy, time.Second), t.Context(), "POST", "http://fixture.invalid/echo", "once")
		_, err := io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		child := nativeResourceOnlyChild(t, pool)
		if err := child.command.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		nativeResourceWait(t, child.done, "OS did not report fixture process exit")
		nativeResourceAssertReaped(t, child)
		pool.mu.Lock()
		entries, active := len(pool.entries), child.active
		pool.mu.Unlock()
		if entries != 0 || active != 0 {
			t.Fatalf("exited helper resources: processes=%d active=%d", entries, active)
		}
	}
}

func TestGatewayNativeResourceIndependentActiveRouteSurvivesChildCrash(t *testing.T) {
	pool := nativeHTTPFixturePool(t, nil)
	route := pool.RoundTripper(nil, time.Second)
	first := nativeHTTPFixtureRequest(t, route, t.Context(), "GET", "http://first.invalid/hold", "")
	defer first.Body.Close()
	firstChild := nativeResourceOnlyChild(t, pool)
	secondCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	second := nativeHTTPFixtureRequest(t, route, secondCtx, "GET", "http://second.invalid/hold", "")
	defer second.Body.Close()
	buf := make([]byte, 5)
	_, _ = io.ReadFull(first.Body, buf)
	_, _ = io.ReadFull(second.Body, buf)
	if err := firstChild.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Body.Read(buf); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("helper crash became a successful EOF: %v", err)
	}
	nativeResourceAssertReaped(t, firstChild)
	pool.mu.Lock()
	remaining, active := len(pool.entries), 0
	for _, child := range pool.entries {
		active += child.active
		select {
		case <-child.done:
			t.Error("independent helper was stopped by another route's crash")
		default:
		}
	}
	pool.mu.Unlock()
	if remaining != 1 || active != 1 {
		t.Fatalf("unrelated route changed: entries=%d active=%d", remaining, active)
	}
	cancel()
	_, err := second.Body.Read(buf)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("independent stream did not remain active until its own cancellation: %v", err)
	}
	_ = pool.Close()
}

func TestGatewayNativeResourceMixedTerminationsReleaseExactlyOnce(t *testing.T) {
	pool := nativeHTTPFixturePool(t, nil)
	route := pool.RoundTripper(nil, time.Second)
	for range 3 {
		for _, path := range []string{"echo", "fragment", "hold"} {
			ctx, cancel := context.WithCancel(t.Context())
			response := nativeHTTPFixtureRequest(t, route, ctx, "POST", "http://fixture.invalid/"+path, "entity")
			child := nativeResourceOnlyChild(t, pool)
			if path == "hold" {
				prefix := make([]byte, 5)
				if _, err := io.ReadFull(response.Body, prefix); err != nil {
					t.Fatal(err)
				}
				cancel()
			}
			_, err := io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			_ = response.Body.Close()
			cancel()
			if path == "echo" && err != nil || path == "fragment" && !errors.Is(err, io.ErrUnexpectedEOF) || path == "hold" && !errors.Is(err, context.Canceled) {
				t.Fatalf("termination %s result=%v", path, err)
			}
			deadline := time.Now().Add(time.Second)
			for {
				pool.mu.Lock()
				active := child.active
				pool.mu.Unlock()
				if active == 0 {
					break
				}
				if active < 0 || time.Now().After(deadline) {
					t.Fatalf("termination %s leaked or double-released native slot: %d", path, active)
				}
				time.Sleep(time.Millisecond)
			}
		}
	}
	child := nativeResourceOnlyChild(t, pool)
	_ = pool.Close()
	nativeResourceAssertReaped(t, child)
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.entries) != 0 || child.active != 0 {
		t.Fatal("parent Close returned before all test route resources were released")
	}
}

func TestGatewayNativeActualResourceCrashHTTPAndWSNoReplay(t *testing.T) {
	var posts, handshakes, messages atomic.Int32
	var stopped atomic.Int32
	p, _ := nativeWSFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, ": active\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			stopped.Add(1)
			return
		}
		handshakes.Add(1)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				stopped.Add(1)
				return
			}
			messages.Add(1)
			if err := conn.Write(r.Context(), websocket.MessageText, data); err != nil {
				return
			}
		}
	})
	pool := p.NativeHTTP
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	route := pool.RoundTripper(nil, 90*time.Second)
	var children []*gatewayNativeProcess
	for round := range 3 {
		response := nativeHTTPFixtureRequest(t, route, ctx, "POST", p.BaseURL+"/responses", "synthetic-once")
		firstBytes := make([]byte, len(": active\n\n"))
		if _, err := io.ReadFull(response.Body, firstBytes); err != nil {
			_ = response.Body.Close()
			t.Fatal(err)
		}
		ws, _, err := p.dialNativeGatewayWebSocket(ctx, make(http.Header), nil)
		if err != nil {
			_ = response.Body.Close()
			t.Fatal(err)
		}
		if err := ws.Write(ctx, websocket.MessageText, []byte("synthetic-once")); err != nil {
			t.Fatal(err)
		}
		if _, data, err := ws.Read(ctx); err != nil || string(data) != "synthetic-once" {
			t.Fatal("resource fixture echo failed")
		}
		child := nativeResourceOnlyChild(t, pool)
		if len(children) > 0 && child == children[len(children)-1] {
			t.Fatal("request after helper crash reused the exited process entry")
		}
		children = append(children, child)
		pool.mu.Lock()
		active := child.active
		pool.mu.Unlock()
		if active != 2 {
			t.Fatalf("HTTP and WS did not share one live helper: active=%d", active)
		}
		httpDone, wsDone := make(chan error, 1), make(chan error, 1)
		go func() { _, err := io.Copy(io.Discard, response.Body); _ = response.Body.Close(); httpDone <- err }()
		go func() { _, _, err := ws.Read(ctx); _ = ws.CloseNow(); wsDone <- err }()
		if err := child.command.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		for _, result := range []<-chan error{httpDone, wsDone} {
			select {
			case err := <-result:
				if err == nil || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("helper crash was successful or waited for request timeout: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("helper crash did not release active HTTP/WS")
			}
		}
		nativeResourceAssertReaped(t, child)
		pool.mu.Lock()
		active = child.active
		pool.mu.Unlock()
		if active != 0 || posts.Load() != int32(round+1) || handshakes.Load() != int32(round+1) || messages.Load() != int32(round+1) {
			t.Fatal("helper crash leaked admission or replayed a POST/WS message")
		}
	}
	_ = pool.Close()
	for _, child := range children {
		nativeResourceAssertReaped(t, child)
	}
	deadline := time.Now().Add(3 * time.Second)
	for stopped.Load() != 6 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if stopped.Load() != 6 {
		t.Fatal("helper crash left an upstream HTTP/WS handler")
	}
}

func TestGatewayNativeActualResourceParentCloseCancelsSOCKSDial(t *testing.T) {
	p := newTestProxy(t, 1)
	realGatewayNativePool(t, p)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	proxy := ProxyEntry{ID: "resource-socks", Type: "socks5", Host: host, Port: mustPoolPort(t, port), Enabled: true, Version: 1}
	result := make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	go func() {
		req, _ := http.NewRequestWithContext(ctx, "POST", "https://synthetic-resource.invalid/responses", strings.NewReader("once"))
		res, err := p.NativeHTTP.RoundTripper(&proxy, 90*time.Second).RoundTrip(req)
		if res != nil {
			_ = res.Body.Close()
		}
		result <- err
	}()
	var upstream net.Conn
	select {
	case upstream = <-accepted:
	case <-ctx.Done():
		t.Fatal("actual helper SOCKS dial did not start")
	}
	defer upstream.Close()
	child := nativeResourceOnlyChild(t, p.NativeHTTP)
	started := time.Now()
	_ = p.NativeHTTP.Close()
	if time.Since(started) > 4*time.Second {
		t.Fatal("parent Close waited for full SOCKS negotiation timeout")
	}
	select {
	case err := <-result:
		if err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("parent Close did not terminate pending request: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("parent Close retained pending native request")
	}
	nativeResourceAssertReaped(t, child)
	p.NativeHTTP.mu.Lock()
	active, entries := child.active, len(p.NativeHTTP.entries)
	p.NativeHTTP.mu.Unlock()
	if active != 0 || entries != 0 {
		t.Fatal("parent Close leaked native or SOCKS resources")
	}
}
