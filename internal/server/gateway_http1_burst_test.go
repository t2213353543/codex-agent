package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Two barrier-synchronized bursts distinguish retained idle connections from
// active request limits. This is a local HTTP/1 experiment, not a TLS or native
// client fingerprint comparison and not a latency estimate for production.
func TestGatewayHTTP1BurstIdleExperiment(t *testing.T) {
	const width = 8
	for _, tc := range []struct {
		name    string
		perHost int
		wantNew int32
	}{
		{"go_default_two_idle", 0, 2*width - http.DefaultMaxIdleConnsPerHost},
		{"gateway_retain_up_to_global_limit", -1, width},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newHTTPClient()
			transport := client.Transport.(*http.Transport)
			if tc.perHost >= 0 {
				transport.MaxIdleConnsPerHost = tc.perHost
			}
			transport.Proxy = nil
			defer client.CloseIdleConnections()
			first, total := measureGatewayHTTP1Bursts(t, client, width)
			if first != width || total != tc.wantNew {
				t.Fatalf("two %d-request bursts: first TCP=%d total TCP=%d want=%d", width, first, total, tc.wantNew)
			}
			t.Logf("HTTP/1: %d+%d barrier-synchronized requests; first burst TCP=%d, second burst new TCP=%d, total TCP=%d; MaxIdleConns=%d, MaxIdleConnsPerHost=%d, IdleConnTimeout=%s", width, width, first, total-first, total, transport.MaxIdleConns, transport.MaxIdleConnsPerHost, transport.IdleConnTimeout)
		})
	}
}

func TestGatewayHTTP1IdleCapacityKeepsRetentionAndAdmissionLimits(t *testing.T) {
	client := newHTTPClient()
	defer client.CloseIdleConnections()
	transport := client.Transport.(*http.Transport)
	if transport.MaxIdleConns != 100 || transport.MaxIdleConnsPerHost != 100 || transport.IdleConnTimeout != 90*time.Second {
		t.Fatalf("idle retention limits changed unexpectedly: global=%d host=%d timeout=%s", transport.MaxIdleConns, transport.MaxIdleConnsPerHost, transport.IdleConnTimeout)
	}
	defaults := http.DefaultTransport.(*http.Transport)
	if transport.MaxConnsPerHost != defaults.MaxConnsPerHost || transport.ForceAttemptHTTP2 != defaults.ForceAttemptHTTP2 {
		t.Fatal("idle tuning changed the active transport limit or HTTP/2 policy")
	}
	entered, unblock := make(chan struct{}), make(chan struct{})
	var release sync.Once
	var upstreamRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if upstreamRequests.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-unblock:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer upstream.Close()
	defer release.Do(func() { close(unblock) })
	p := newTestProxy(t, 1)
	p.BaseURL, p.Client = upstream.URL, client
	makeRequest := func() *http.Request {
		r := proxyRequest(http.MethodPost, "/responses", `{"model":"test","input":[],"stream":true}`)
		r.Header.Set("Session-Id", "burst-admission-session")
		return r.WithContext(t.Context())
	}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, makeRequest())
		firstDone <- w
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first admitted request did not reach fixture")
	}
	second := httptest.NewRecorder()
	p.ServeHTTP(second, makeRequest())
	if second.Code != http.StatusTooManyRequests || upstreamRequests.Load() != 1 {
		t.Fatalf("idle pool expanded active account admission: HTTP=%d upstream=%d", second.Code, upstreamRequests.Load())
	}
	release.Do(func() { close(unblock) })
	select {
	case first := <-firstDone:
		if first.Code != http.StatusOK {
			t.Fatalf("first admitted request failed: HTTP=%d", first.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first admitted request did not finish")
	}
}

func measureGatewayHTTP1Bursts(t *testing.T, client *http.Client, width int) (int32, int32) {
	t.Helper()
	var connections atomic.Int32
	arrivals := [2]atomic.Int32{}
	barriers := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		batch, err := strconv.Atoi(r.URL.Query().Get("batch"))
		if err != nil || batch < 0 || batch >= len(barriers) {
			http.Error(w, "invalid fixture batch", http.StatusBadRequest)
			return
		}
		if r.ProtoMajor != 1 {
			t.Error("fixture must measure HTTP/1 only")
		}
		if arrivals[batch].Add(1) == int32(width) {
			close(barriers[batch])
		}
		select {
		case <-barriers[batch]:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Length", "2")
		_, _ = io.WriteString(w, "ok")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var first int32
	for batch := range 2 {
		var group sync.WaitGroup
		errors := make(chan error, width)
		for range width {
			group.Go(func() {
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/?batch=%d", server.URL, batch), nil)
				if err != nil {
					errors <- err
					return
				}
				response, err := client.Do(request)
				if err != nil {
					errors <- err
					return
				}
				_, readErr := io.Copy(io.Discard, response.Body)
				closeErr := response.Body.Close()
				if readErr != nil {
					errors <- readErr
				} else if closeErr != nil {
					errors <- closeErr
				} else if response.StatusCode != http.StatusOK {
					errors <- fmt.Errorf("fixture status %d", response.StatusCode)
				}
			})
		}
		group.Wait()
		close(errors)
		for err := range errors {
			t.Fatalf("burst %d: %v", batch, err)
		}
		if arrivals[batch].Load() != int32(width) {
			t.Fatalf("burst %d did not reach its full barrier", batch)
		}
		if batch == 0 {
			first = connections.Load()
		}
	}
	return first, connections.Load()
}
