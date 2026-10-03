package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func backend(t *testing.T, label string, done chan struct{}, seen chan string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			seen <- r.Header.Get("X-Forwarded-For")
		}
		switch r.URL.Path {
		case "/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s-start\n\n", label)
			w.(http.Flusher).Flush()
			select {
			case <-done:
			case <-r.Context().Done():
				return
			}
			fmt.Fprintf(w, "data: %s-end\n\n", label)
		case "/ws":
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()
			for {
				kind, b, err := c.Read(r.Context())
				if err != nil {
					return
				}
				if err = c.Write(r.Context(), kind, append([]byte(label+":"), b...)); err != nil {
					return
				}
			}
		case "/echo":
			_, _ = io.Copy(w, r.Body)
		default:
			fmt.Fprint(w, label)
		}
	}))
	t.Cleanup(s.Close)
	return s
}
func address(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "http://") }
func waitCount(t *testing.T, p *ingress, generation string, n int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		v := p.generations[generation].active
		p.mu.Unlock()
		if v == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("active generation counter did not settle")
}

func TestRealSSEWebSocketSurviveSwitch(t *testing.T) {
	done := make(chan struct{})
	old, new := backend(t, "old", done, nil), backend(t, "new", done, nil)
	p, _ := newIngress(target{1, "old", address(old), false}, nil)
	front := httptest.NewServer(p)
	defer front.Close()
	response, err := http.Get(front.URL + "/sse")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	first, _ := reader.ReadString('\n')
	if first != "data: old-start\n" {
		t.Fatal(first)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(front.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	if err = p.update(1, target{2, "new", address(new), false}); err != nil {
		t.Fatal(err)
	}
	if err = ws.Write(ctx, websocket.MessageText, []byte("after-switch")); err != nil {
		t.Fatal(err)
	}
	_, answer, err := ws.Read(ctx)
	if err != nil || string(answer) != "old:after-switch" {
		t.Fatalf("WS interrupted: %s %v", answer, err)
	}
	next, err := http.Get(front.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(next.Body)
	next.Body.Close()
	if string(body) != "new" {
		t.Fatal("new request not switched")
	}
	waitCount(t, p, "old", 2)
	close(done)
	tail, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(tail), "old-end") {
		t.Fatalf("SSE interrupted: %s %v", tail, err)
	}
	response.Body.Close()
	ws.CloseNow()
	waitCount(t, p, "old", 0)
}
func TestSameClientKeepAliveNextRequestSwitchesAndXFFCannotSpoof(t *testing.T) {
	seen := make(chan string, 4)
	old, new := backend(t, "old", nil, seen), backend(t, "new", nil, seen)
	p, _ := newIngress(target{1, "old", address(old), false}, nil)
	front := httptest.NewServer(p)
	defer front.Close()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	var reused atomic.Bool
	request := func(want string) {
		r, _ := http.NewRequest("GET", front.URL, nil)
		r.Header.Set("X-Forwarded-For", "192.0.2.99")
		r = r.WithContext(httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused.Store(info.Reused) }}))
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if string(raw) != want {
			t.Fatal(string(raw))
		}
		if x := <-seen; x != "127.0.0.1" {
			t.Fatalf("untrusted XFF survived: %q", x)
		}
	}
	request("old")
	if err := p.update(1, target{2, "new", address(new), false}); err != nil {
		t.Fatal(err)
	}
	request("new")
	if !reused.Load() {
		t.Fatal("test did not reuse same frontend TCP connection")
	}
}
func TestHoldWaitsWithout503AndIsCancellable(t *testing.T) {
	old, new := backend(t, "old", nil, nil), backend(t, "new", nil, nil)
	p, _ := newIngress(target{1, "old", address(old), true}, nil)
	front := httptest.NewServer(p)
	defer front.Close()
	done := make(chan string, 1)
	go func() {
		r, err := http.Get(front.URL)
		if err != nil {
			done <- "error"
			return
		}
		raw, _ := io.ReadAll(r.Body)
		r.Body.Close()
		done <- fmt.Sprint(r.StatusCode) + ":" + string(raw)
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		waiting := p.waiting
		p.mu.Unlock()
		if waiting == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case v := <-done:
		t.Fatal("held request replied early", v)
	default:
	}
	if err := p.update(1, target{2, "new", address(new), false}); err != nil {
		t.Fatal(err)
	}
	if v := <-done; v != "200:new" {
		t.Fatal(v)
	}
	if err := p.update(2, target{3, "new", address(new), true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	r, _ := http.NewRequestWithContext(ctx, "GET", front.URL, nil)
	cancelled := make(chan error, 1)
	go func() { _, err := http.DefaultClient.Do(r); cancelled <- err }()
	deadline = time.Now().Add(time.Second)
	seenWaiting := false
	for time.Now().Before(deadline) {
		p.mu.Lock()
		waiting := p.waiting
		p.mu.Unlock()
		if waiting == 1 {
			seenWaiting = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !seenWaiting {
		cancel()
		t.Fatal("cancellation test did not reach held request")
	}
	cancel()
	if err := <-cancelled; err == nil {
		t.Fatal("cancelled hold unexpectedly succeeded")
	}
	if err := p.update(3, target{4, "new", address(new), false}); err != nil {
		t.Fatal(err)
	}
}
func TestCASAndFailedPersistenceKeepOldRoute(t *testing.T) {
	p, _ := newIngress(target{1, "old", "127.0.0.1:18890", false}, func(target) error { return errors.New("synthetic persistence failure") })
	if p.update(1, target{2, "new", "127.0.0.1:18891", false}) == nil || p.state.Generation != "old" {
		t.Fatal("failed persist changed route")
	}
	p.save = nil
	if p.update(0, target{1, "new", "127.0.0.1:18891", false}) == nil {
		t.Fatal("stale revision accepted")
	}
	if p.update(1, target{2, "new", "192.0.2.1:18891", false}) == nil {
		t.Fatal("nonlocal backend accepted")
	}
}
func TestBodyPreserved(t *testing.T) {
	old := backend(t, "old", nil, nil)
	p, _ := newIngress(target{1, "old", address(old), false}, nil)
	front := httptest.NewServer(p)
	defer front.Close()
	payload := "\x00PRIVATE_SYNTHETIC_BODY\r\n9007199254740993"
	r, err := http.Post(front.URL+"/echo", "application/octet-stream", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if string(raw) != payload {
		t.Fatal("body rewritten")
	}
}

func TestHeldIngressStillServesHealthChecks(t *testing.T) {
	old := backend(t, "old-health", nil, nil)
	p, _ := newIngress(target{1, "old", address(old), true}, nil)
	front := httptest.NewServer(p)
	defer front.Close()
	client := &http.Client{Timeout: time.Second}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, path := range []string{"/health", "/health/live", "/health/ready"} {
			r, _ := http.NewRequest(method, front.URL+path, nil)
			response, err := client.Do(r)
			if err != nil {
				t.Fatalf("held health waited: %s %s %v", method, path, err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatal("held health returned failure")
			}
		}
	}
}
