package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func poolProxyServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	connections := new(atomic.Int32)
	server := httptest.NewUnstartedServer(handler)
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)
	return server, connections
}

func poolProxyEntry(t *testing.T, p *Proxy, endpoint string) ProxyEntry {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	entry, err := p.Proxies.Create(ProxyEntry{Name: "pool proxy", Type: "http", Host: u.Hostname(), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.closeGatewayClients)
	return entry
}

func poolFetch(t *testing.T, client *http.Client) string {
	t.Helper()
	res, err := client.Get("http://pool-upstream.invalid/probe")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestGatewayClientPoolReusesConnectionsAcrossTurns(t *testing.T) {
	for _, path := range []string{"responses", "models", "responses/compact"} {
		t.Run(path, func(t *testing.T) {
			var requests atomic.Int32
			server, connections := poolProxyServer(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if !r.URL.IsAbs() || r.URL.Host != "pool-upstream.invalid" {
					t.Error("request bypassed its configured HTTP proxy")
				}
				_, _ = io.Copy(io.Discard, r.Body)
				if path == "responses" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"pooled\",\"status\":\"completed\",\"output\":[]}}\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"pooled","output":[],"models":[]}`)
				}
			})
			p := newTestProxy(t, 2)
			entry := poolProxyEntry(t, p, server.URL)
			p.BaseURL = "http://pool-upstream.invalid"
			if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Proxies = []string{entry.ID}; return nil }); err != nil {
				t.Fatal(err)
			}
			for turn := range 12 {
				method, body := http.MethodPost, `{"model":"pool-model","input":"next turn","stream":true}`
				if path == "models" {
					method, body = http.MethodGet, ""
				}
				req := proxyRequest(method, "/v1/"+path, body)
				req.Header.Set("Session-Id", "pooled-session")
				result := httptest.NewRecorder()
				p.ServeHTTP(result, req)
				if result.Code != http.StatusOK {
					t.Fatalf("turn %d returned %d: %s", turn, result.Code, result.Body.String())
				}
			}
			if requests.Load() != 12 || connections.Load() != 1 {
				t.Fatalf("12 turns used requests=%d TCP connections=%d; want one reused connection", requests.Load(), connections.Load())
			}
		})
	}
}

func TestGatewayClientPoolRotatesProxyCredentialsAndEndpoint(t *testing.T) {
	for _, changeEndpoint := range []bool{false, true} {
		t.Run(fmt.Sprint("endpoint=", changeEndpoint), func(t *testing.T) {
			var oldHits, newHits atomic.Int32
			wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("new-user:new-password"))
			handler := func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Proxy-Authorization") == wantAuth {
					newHits.Add(1)
					_, _ = io.WriteString(w, "new")
				} else {
					oldHits.Add(1)
					_, _ = io.WriteString(w, "old")
				}
			}
			first, connections := poolProxyServer(t, handler)
			second, _ := poolProxyServer(t, handler)
			p := newTestProxy(t, 1)
			entry := poolProxyEntry(t, p, first.URL)
			oldClient := p.clientFor(&entry)
			if poolFetch(t, oldClient) != "old" {
				t.Fatal("initial route was not used")
			}
			_, err := p.Proxies.UpdateVersion(entry.ID, 0, func(v *ProxyEntry) error {
				v.Username, v.Password = "new-user", "new-password"
				if changeEndpoint {
					u, _ := url.Parse(second.URL)
					v.Host, v.Port = u.Hostname(), mustPoolPort(t, u.Port())
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if res, err := oldClient.Get("http://pool-upstream.invalid/stale"); !errors.Is(err, errGatewayProxyUnavailable) {
				if res != nil {
					_ = res.Body.Close()
				}
				t.Fatalf("already acquired client used stale proxy credentials: %v", err)
			}
			// This is intentionally the pre-update selection snapshot.
			fresh := p.clientFor(&entry)
			if fresh == oldClient || poolFetch(t, fresh) != "new" || poolFetch(t, fresh) != "new" {
				t.Fatal("stale selection resurrected an obsolete client")
			}
			if oldHits.Load() != 1 || newHits.Load() != 2 {
				t.Fatalf("unexpected routes: old=%d new=%d", oldHits.Load(), newHits.Load())
			}
			wantConnections := int32(2)
			if changeEndpoint {
				wantConnections = 1
			}
			if connections.Load() != wantConnections {
				t.Fatalf("old endpoint TCP connections=%d want=%d", connections.Load(), wantConnections)
			}
		})
	}
}

func mustPoolPort(t *testing.T, raw string) int {
	t.Helper()
	port, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestGatewayClientPoolDisabledOrDeletedFailsClosed(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(fmt.Sprint("delete=", remove), func(t *testing.T) {
			var hits atomic.Int32
			server, _ := poolProxyServer(t, func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				_, _ = io.WriteString(w, "ok")
			})
			p := newTestProxy(t, 1)
			entry := poolProxyEntry(t, p, server.URL)
			old := p.clientFor(&entry)
			poolFetch(t, old)
			var err error
			if remove {
				err = p.Proxies.Delete(entry.ID)
			} else {
				_, err = p.Proxies.UpdateVersion(entry.ID, 0, func(v *ProxyEntry) error { v.Enabled = false; return nil })
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, client := range []*http.Client{old, p.clientFor(&entry), p.compactClientFor(&entry), p.websocketClientFor(&entry)} {
				res, err := client.Get("http://pool-upstream.invalid/must-not-send")
				if res != nil {
					_ = res.Body.Close()
				}
				if !errors.Is(err, errGatewayProxyUnavailable) {
					t.Fatalf("disabled proxy did not fail closed: %v", err)
				}
			}
			if hits.Load() != 1 {
				t.Fatal("disabled/deleted proxy accepted another request")
			}
			p.clientMu.Lock()
			count := len(p.clientPool)
			p.clientMu.Unlock()
			if count != 0 {
				t.Fatalf("disabled route retained %d cached clients", count)
			}
		})
	}
}

func TestGatewayClientPoolConcurrentCreationAndVersionCAS(t *testing.T) {
	server, _ := poolProxyServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Header.Get("Proxy-Authorization"))
	})
	p := newTestProxy(t, 1)
	entry := poolProxyEntry(t, p, server.URL)
	var err error
	entry, err = p.Proxies.UpdateVersion(entry.ID, 0, func(*ProxyEntry) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	const workers = 32
	clients := make(chan *http.Client, workers)
	var group sync.WaitGroup
	for range workers {
		group.Go(func() { clients <- p.clientFor(&entry) })
	}
	group.Wait()
	first := <-clients
	for range workers - 1 {
		if <-clients != first {
			t.Fatal("concurrent pool misses created duplicate clients")
		}
	}
	results := make(chan error, 2)
	for _, username := range []string{"winner-a", "winner-b"} {
		group.Go(func() {
			_, err := p.Proxies.UpdateVersion(entry.ID, entry.Version, func(v *ProxyEntry) error {
				v.Username, v.Password = username, "new-password"
				return nil
			})
			results <- err
		})
	}
	group.Wait()
	left, right := <-results, <-results
	if !((left == nil && errors.Is(right, errVersionConflict)) || (right == nil && errors.Is(left, errVersionConflict))) {
		t.Fatalf("proxy update was not CAS protected: %v / %v", left, right)
	}
	winner, ok := p.Proxies.Get(entry.ID)
	if !ok {
		t.Fatal("winning configuration disappeared")
	}
	for range workers {
		group.Go(func() { clients <- p.clientFor(&entry) })
	}
	group.Wait()
	fresh := <-clients
	if fresh == first {
		t.Fatal("proxy version update retained obsolete pool entry")
	}
	for range workers - 1 {
		if <-clients != fresh {
			t.Fatal("concurrent stale selections replaced the winning client")
		}
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(winner.Username+":"+winner.Password))
	if poolFetch(t, fresh) != want {
		t.Fatal("gateway did not use the winning CAS credentials")
	}
}

func TestGatewayClientPoolRetiresWithoutInterruptingStream(t *testing.T) {
	continueBody := make(chan struct{})
	server, _ := poolProxyServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		select {
		case <-continueBody:
			_, _ = io.WriteString(w, "last\n")
		case <-r.Context().Done():
			t.Error("retiring a pooled client canceled an active response")
		}
	})
	p := newTestProxy(t, 1)
	entry := poolProxyEntry(t, p, server.URL)
	client := p.clientFor(&entry)
	res, err := client.Get("http://pool-upstream.invalid/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	first := make([]byte, len("first\n"))
	if _, err := io.ReadFull(res.Body, first); err != nil || string(first) != "first\n" {
		t.Fatalf("initial stream bytes: %q %v", first, err)
	}
	_, err = p.Proxies.UpdateVersion(entry.ID, 0, func(v *ProxyEntry) error { v.Password = "rotated"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if p.clientFor(&entry) == client {
		t.Fatal("configuration update kept the same client")
	}
	// Shutdown/eviction must also leave an already accepted response alive.
	p.closeGatewayClients()
	close(continueBody)
	last, err := io.ReadAll(res.Body)
	if err != nil || string(last) != "last\n" {
		t.Fatalf("stream was interrupted by retirement: %q %v", last, err)
	}
}

func TestGatewayClientPoolIsBoundedAndExpiresIdleEntries(t *testing.T) {
	p := &Proxy{Client: newHTTPClient()}
	t.Cleanup(p.closeGatewayClients)
	firstEntry := ProxyEntry{ID: "first", Type: "http", Host: "127.0.0.1", Port: 9000}
	first := p.clientFor(&firstEntry).Transport.(*gatewayClientTransport)
	for i := range gatewayClientPoolLimit {
		entry := ProxyEntry{ID: fmt.Sprint("proxy-", i), Type: "http", Host: "127.0.0.1", Port: 9001 + i}
		p.clientFor(&entry)
	}
	if len(p.clientPool) != gatewayClientPoolLimit || !first.retired.Load() {
		t.Fatalf("pool exceeded its limit or retained oldest entry: count=%d retired=%t", len(p.clientPool), first.retired.Load())
	}
	var expired *gatewayClientTransport
	for _, entry := range p.clientPool {
		entry.lastUsed = time.Now().Add(-gatewayClientPoolIdle - time.Second)
		expired = entry.transport
	}
	p.clientFor(&firstEntry)
	if len(p.clientPool) != 1 || !expired.retired.Load() {
		t.Fatalf("idle clients were not retired: count=%d", len(p.clientPool))
	}
	p.closeGatewayClients()
	if len(p.clientPool) != 0 {
		t.Fatal("shutdown retained pooled entries")
	}
}

func TestGatewayClientPoolKeepsCustomDirectTransport(t *testing.T) {
	var calls atomic.Int32
	base := &http.Client{Transport: sourceRefreshRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("custom"))}, nil
	}), Timeout: time.Second}
	p := &Proxy{Client: base}
	if p.clientFor(nil) != base {
		t.Fatal("direct client identity changed")
	}
	for _, client := range []*http.Client{p.clientFor(nil), p.compactClientFor(nil), p.websocketClientFor(nil)} {
		if poolFetch(t, client) != "custom" {
			t.Fatal("custom direct RoundTripper was bypassed")
		}
	}
	if calls.Load() != 3 || base.Timeout != time.Second || len(p.clientPool) != 0 {
		t.Fatal("direct custom transport was mutated or put into the proxy pool")
	}
}

func TestGatewayClientPoolVariantsKeepTimeoutAndProtocol(t *testing.T) {
	p := &Proxy{Client: newHTTPClient()}
	t.Cleanup(p.closeGatewayClients)
	entry := &ProxyEntry{ID: "proxy", Type: "http", Host: "127.0.0.1", Port: 9000}
	regular, compact, ws := p.clientFor(entry), p.compactClientFor(entry), p.websocketClientFor(entry)
	if regular == compact || compact == ws || p.compactClientFor(entry) != compact || p.websocketClientFor(entry) != ws {
		t.Fatal("protocol variants are not independently reused")
	}
	regularTransport := regular.Transport.(*gatewayClientTransport).transport
	compactTransport := compact.Transport.(*gatewayClientTransport).transport
	wsTransport := ws.Transport.(*gatewayClientTransport).transport
	if regularTransport.ResponseHeaderTimeout != 90*time.Second || compactTransport.ResponseHeaderTimeout != upstreamCompactTimeout || compact.Timeout != upstreamCompactTimeout {
		t.Fatal("pooled variants changed normal/compact timeout contracts")
	}
	if ws.Timeout != 0 || wsTransport.ForceAttemptHTTP2 || wsTransport.Protocols == nil || !wsTransport.Protocols.HTTP1() || wsTransport.Protocols.HTTP2() {
		t.Fatal("pooled WebSocket transport does not use its HTTP/1.1 contract")
	}
	// The explicit test/helper timeout must still override a pooled variant.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://pool-upstream.invalid/compact", nil)
	_, err := doCompactRequestWithTimeout(compact, req, time.Millisecond)
	if !errors.Is(err, context.Canceled) || compactTransport.ResponseHeaderTimeout != upstreamCompactTimeout {
		t.Fatal("explicit compact timeout mutated shared transport or lost cancellation")
	}
}

func TestGatewayClientPoolReusesHTTP2TunnelAfterTerminalSSE(t *testing.T) {
	finished := make(chan struct{}, 6)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("expected real HTTP/2 upstream, received %s", r.Proto)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"h2\",\"status\":\"completed\",\"output\":[]}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // Completion must cancel only this HTTP/2 stream.
		finished <- struct{}{}
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	t.Cleanup(upstream.Close)
	var connects atomic.Int32
	proxyServer, _ := poolProxyServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		connects.Add(1)
		target, err := net.DialTimeout("tcp", r.Host, 3*time.Second)
		if err != nil {
			t.Error(err)
			return
		}
		defer target.Close()
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer client.Close()
		_, _ = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{})
		go func() { _, _ = io.Copy(target, client); _ = target.Close(); close(done) }()
		_, _ = io.Copy(client, target)
		_ = client.Close()
		<-done
	})
	p := newTestProxy(t, 2)
	p.Client, p.BaseURL = upstream.Client(), upstream.URL
	p.Client.Transport.(*http.Transport).ResponseHeaderTimeout = 90 * time.Second
	entry := poolProxyEntry(t, p, proxyServer.URL)
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Proxies = []string{entry.ID}; return nil }); err != nil {
		t.Fatal(err)
	}
	for turn := range 6 {
		r := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"pool-model","input":"next","stream":true}`)
		r.Header.Set("Session-Id", "h2-pooled-session")
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("turn %d HTTP %d: %s", turn, w.Code, w.Body.String())
		}
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Fatal("gateway did not close a completed HTTP/2 response stream")
		}
	}
	if connects.Load() != 1 {
		t.Fatalf("six SSE turns opened %d TLS tunnels; want one reused HTTP/2 connection", connects.Load())
	}
}

func BenchmarkGatewayProxyConnectionReuse(b *testing.B) {
	for _, pooled := range []bool{false, true} {
		name := "clone_per_request"
		if pooled {
			name = "pooled"
		}
		b.Run(name, func(b *testing.B) {
			var connections atomic.Int64
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "ok")
			}))
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			server.Start()
			defer server.Close()
			u, _ := url.Parse(server.URL)
			port, _ := strconv.Atoi(u.Port())
			entry := &ProxyEntry{ID: "benchmark", Type: "http", Host: u.Hostname(), Port: port}
			p := &Proxy{Client: newHTTPClient()}
			defer p.closeGatewayClients()
			b.ResetTimer()
			for range b.N {
				var client *http.Client
				if pooled {
					client = p.clientFor(entry)
				} else {
					client = newGatewayProxyClient(p.Client, entry, gatewayClientHTTP)
				}
				res, err := client.Get("http://pool-upstream.invalid/probe")
				if err != nil {
					b.Fatal(err)
				}
				_, err = io.Copy(io.Discard, res.Body)
				_ = res.Body.Close()
				if err != nil {
					b.Fatal(err)
				}
				if !pooled {
					client.CloseIdleConnections()
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(connections.Load())/float64(b.N), "connections/op")
		})
	}
}
