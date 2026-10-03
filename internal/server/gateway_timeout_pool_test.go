package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGatewayTimeoutPoolComparedWithPrivateVariants(t *testing.T) {
	const width = 4
	for _, pooled := range []bool{false, true} {
		t.Run(fmt.Sprintf("pooled=%t", pooled), func(t *testing.T) {
			p := &Proxy{Client: newHTTPClient()}
			t.Cleanup(p.closeGatewayClients)
			// The old strategy cloned once per request and closed idle sockets
			// after its body. Exercise that lifetime against the same barriers.
			client := &http.Client{Transport: sourceRefreshRoundTripper(func(r *http.Request) (*http.Response, error) {
				var configured *http.Client
				cleanup := func() {}
				if pooled {
					configured = p.gatewayRequestClientWithHeaderTimeout(r.Context(), nil, gatewayClientHTTP, 5*time.Second)
				} else {
					configured, cleanup = gatewayClientWithHeaderTimeout(p.Client, 5*time.Second)
				}
				res, err := configured.Transport.RoundTrip(r)
				if err != nil {
					cleanup()
					return res, err
				}
				res.Body = &compactResponseBody{ReadCloser: res.Body, cleanup: cleanup}
				return res, nil
			})}
			first, total := measureGatewayHTTP1Bursts(t, client, width)
			want := int32(2 * width)
			if pooled {
				want = width
			}
			if first != width || total != want {
				t.Fatalf("first TCP=%d total TCP=%d want=%d", first, total, want)
			}
			t.Logf("custom 5s header timeout, pooled=%t: %d+%d requests, first TCP=%d second new TCP=%d total=%d", pooled, width, width, first, total-first, total)
		})
	}
}

// Exercise the actual gateway call sites: selecting the timeout anew for each
// request must reuse the connection after both its body and request complete.
func TestGatewayTimeoutPoolReusesTwoBatches(t *testing.T) {
	const width = 4
	for _, proxied := range []bool{false, true} {
		for _, path := range []string{"responses", "models", "responses/compact"} {
			t.Run(fmt.Sprintf("proxy=%t/%s", proxied, path), func(t *testing.T) {
				var requests atomic.Int32
				barriers := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
				upstream, connections := poolProxyServer(t, func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					count := requests.Add(1)
					batch := (count - 1) / width
					if batch >= 2 {
						t.Error("gateway replayed a request")
						return
					}
					if count%width == 0 {
						close(barriers[batch])
					}
					select {
					case <-barriers[batch]:
					case <-r.Context().Done():
						return
					}
					body := `{"output":[],"models":[]}`
					if path == "responses" {
						w.Header().Set("Content-Type", "text/event-stream")
						body = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"pooled\",\"status\":\"completed\",\"output\":[]}}\n\n"
					} else {
						w.Header().Set("Content-Type", "application/json")
					}
					w.Header().Set("Content-Length", strconv.Itoa(len(body)))
					_, _ = io.WriteString(w, body)
				})
				p := newTestProxy(t, width)
				t.Cleanup(p.closeGatewayClients)
				p.BaseURL = upstream.URL
				if proxied {
					entry := poolProxyEntry(t, p, upstream.URL)
					p.BaseURL = "http://timeout-pool.invalid"
					if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Proxies = []string{entry.ID}; return nil }); err != nil {
						t.Fatal(err)
					}
				}
				settings, _ := p.Settings.Get()
				settings.UpstreamResponseHeaderTimeoutSeconds = 5
				settings.UpstreamCompactTimeoutSeconds = 30
				if err := p.Settings.Set(settings); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				var first int32
				for batch := range 2 {
					results := make(chan *httptest.ResponseRecorder, width)
					var workers sync.WaitGroup
					for i := range width {
						workers.Go(func() {
							method, body := http.MethodPost, `{"model":"pool-model","input":[],"stream":true}`
							if path == "models" {
								method, body = http.MethodGet, ""
							}
							r := proxyRequest(method, "/v1/"+path, body).WithContext(ctx)
							r.Header.Set("Session-Id", fmt.Sprintf("timeout-pool-%d", i))
							w := httptest.NewRecorder()
							p.ServeHTTP(w, r)
							results <- w
						})
					}
					workers.Wait()
					close(results)
					for w := range results {
						if w.Code != http.StatusOK {
							t.Fatalf("batch %d HTTP %d: %s", batch, w.Code, w.Body.String())
						}
					}
					if batch == 0 {
						first = connections.Load()
					}
				}
				if first != width || connections.Load() != width || requests.Load() != 2*width {
					t.Fatalf("first batch TCP=%d total TCP=%d requests=%d", first, connections.Load(), requests.Load())
				}
				t.Logf("custom timeout, two %d-request HTTP/1 batches: first TCP=%d, second new TCP=%d", width, first, connections.Load()-first)
			})
		}
	}
}

func TestGatewayTimeoutPoolKeepsIndependentDeadlines(t *testing.T) {
	entered := make(chan string, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	upstream, _ := poolProxyServer(t, func(w http.ResponseWriter, r *http.Request) {
		entered <- r.URL.Path
		select {
		case <-r.Context().Done():
			return
		case <-release:
			_, _ = io.WriteString(w, "ok")
		}
	})
	p := &Proxy{Client: newHTTPClient()}
	t.Cleanup(p.closeGatewayClients)
	short := p.gatewayRequestClientWithHeaderTimeout(t.Context(), nil, gatewayClientHTTP, 80*time.Millisecond)
	long := p.gatewayRequestClientWithHeaderTimeout(t.Context(), nil, gatewayClientHTTP, 5*time.Second)
	if short == long || p.gatewayRequestClientWithHeaderTimeout(t.Context(), nil, gatewayClientHTTP, 80*time.Millisecond) != short {
		t.Fatal("distinct timeout variants were mixed or identical variants were not reused")
	}
	results := make(chan error, 2)
	for _, request := range []struct {
		client *http.Client
		path   string
	}{{short, "/short"}, {long, "/long"}} {
		go func() {
			res, err := request.client.Get(upstream.URL + request.path)
			if res != nil {
				_, _ = io.Copy(io.Discard, res.Body)
				_ = res.Body.Close()
			}
			results <- err
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("both timeout variants did not reach the fixture")
		}
	}
	select {
	case err := <-results:
		var timeout interface{ Timeout() bool }
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatalf("short deadline was not enforced: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("short timeout was overwritten by another request")
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-results; err != nil {
		t.Fatalf("long deadline inherited the short timeout: %v", err)
	}
	if p.Client.Transport.(*http.Transport).ResponseHeaderTimeout != 90*time.Second || short.Transport.(*gatewayClientTransport).transport.ResponseHeaderTimeout != 80*time.Millisecond {
		t.Fatal("request snapshot or original transport was mutated")
	}
}

func TestGatewayTimeoutPoolRotationGuardsAllVariants(t *testing.T) {
	var hits atomic.Int32
	upstream, _ := poolProxyServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "ok")
	})
	p := newTestProxy(t, 1)
	entry := poolProxyEntry(t, p, upstream.URL)
	ctx := withGatewayRequestRoute(t.Context(), "key", "session")
	var clients []*http.Client
	for _, mode := range []gatewayClientMode{gatewayClientHTTP, gatewayClientCompact} {
		for _, timeout := range []time.Duration{5 * time.Second, 7 * time.Second} {
			client := p.gatewayRequestClientWithHeaderTimeout(ctx, &entry, mode, timeout)
			if poolFetch(t, client) != "ok" {
				t.Fatal("initial timeout route failed")
			}
			clients = append(clients, client)
		}
	}
	updated, err := p.Proxies.UpdateVersion(entry.ID, 0, func(v *ProxyEntry) error { v.Password = "rotated"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	// A pool lookup may load the newest proxy, but the admitted request must
	// reject it instead of silently changing route between OAuth and inference.
	clients = append(clients, p.gatewayRequestClientWithHeaderTimeout(ctx, &entry, gatewayClientHTTP, 9*time.Second))
	for _, client := range clients {
		res, err := client.Get("http://timeout-pool.invalid/stale")
		if res != nil {
			_ = res.Body.Close()
		}
		if !errors.Is(err, errGatewayProxyUnavailable) {
			t.Fatalf("stale timeout client bypassed proxy guard: %v", err)
		}
	}
	if hits.Load() != 4 {
		t.Fatal("rotated proxy received a stale request")
	}
	for _, client := range clients[:4] {
		if !client.Transport.(*gatewayClientTransport).retired.Load() {
			t.Fatal("proxy rotation retained an older timeout variant")
		}
	}
	fresh := p.gatewayRequestClientWithHeaderTimeout(ctx, &updated, gatewayClientHTTP, 9*time.Second)
	if poolFetch(t, fresh) != "ok" || poolFetch(t, fresh) != "ok" || hits.Load() != 6 {
		t.Fatal("fresh proxy timeout variant did not recover")
	}
}

func TestGatewayTimeoutPoolBoundAndActiveStream(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	upstream, _ := poolProxyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, "data: last\n\n")
		case <-r.Context().Done():
			t.Error("timeout variant retirement interrupted an accepted stream")
		}
	})
	p := &Proxy{Client: newHTTPClient()}
	t.Cleanup(p.closeGatewayClients)
	first := p.gatewayRequestClientWithHeaderTimeout(t.Context(), nil, gatewayClientHTTP, time.Second)
	res, err := first.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	start := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(res.Body, start); err != nil {
		t.Fatal(err)
	}
	for i := range gatewayClientPoolLimit {
		p.gatewayRequestClientWithHeaderTimeout(t.Context(), nil, gatewayClientHTTP, time.Duration(i+2)*time.Millisecond)
	}
	if len(p.clientPool) != gatewayClientPoolLimit || !first.Transport.(*gatewayClientTransport).retired.Load() {
		t.Fatalf("timeout variants exceeded bounded pool or retained LRU: entries=%d", len(p.clientPool))
	}
	p.closeGatewayClients()
	releaseOnce.Do(func() { close(release) })
	rest, err := io.ReadAll(res.Body)
	if err != nil || string(rest) != "data: last\n\n" {
		t.Fatalf("active body did not survive pool eviction/shutdown: %q %v", rest, err)
	}
}

func TestGatewayTimeoutPoolDefaultsAndCustomRoundTripper(t *testing.T) {
	p := &Proxy{Client: newHTTPClient()}
	t.Cleanup(p.closeGatewayClients)
	if p.gatewayRequestClientWithHeaderTimeout(t.Context(), nil, gatewayClientHTTP, 90*time.Second) != p.Client {
		t.Fatal("default direct client stopped reusing its original pool")
	}
	compact := p.compactClientFor(nil)
	if compact.Timeout != 20*time.Minute || compact.Transport.(*gatewayClientTransport).transport.ResponseHeaderTimeout != 20*time.Minute {
		t.Fatal("default compact no longer permits twenty minutes")
	}
	if p.gatewayRequestClientWithHeaderTimeout(t.Context(), nil, gatewayClientCompact, 20*time.Minute) != compact {
		t.Fatal("explicit default compact created another pool entry")
	}
	var calls atomic.Int32
	custom := &http.Client{Transport: sourceRefreshRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("custom"))}, nil
	}), Timeout: time.Second}
	p.Client = custom
	for _, timeout := range []time.Duration{time.Millisecond, time.Second} {
		client := p.gatewayRequestClientWithHeaderTimeout(t.Context(), nil, gatewayClientHTTP, timeout)
		if client != custom || poolFetch(t, client) != "custom" {
			t.Fatal("custom RoundTripper was replaced or mutated")
		}
	}
	if calls.Load() != 2 || custom.Timeout != time.Second {
		t.Fatal("custom client contract changed")
	}
	customCompact := p.gatewayRequestClientWithHeaderTimeout(t.Context(), nil, gatewayClientCompact, 30*time.Second)
	if customCompact.Timeout != 30*time.Second || custom.Timeout != time.Second || poolFetch(t, customCompact) != "custom" {
		t.Fatal("custom compact deadline was lost or its transport was replaced")
	}
}

func TestGatewayTimeoutPoolCompactTotalTimeoutLeavesHTTPBodyAlive(t *testing.T) {
	for _, proxied := range []bool{false, true} {
		t.Run(fmt.Sprint("proxy=", proxied), func(t *testing.T) {
			release := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			upstream, _ := poolProxyServer(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "first")
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
				case <-release:
					_, _ = io.WriteString(w, "last")
				}
			})
			p := newTestProxy(t, 2)
			t.Cleanup(p.closeGatewayClients)
			var proxy *ProxyEntry
			target := upstream.URL
			if proxied {
				entry := poolProxyEntry(t, p, upstream.URL)
				proxy = &entry
				target = "http://timeout-pool.invalid"
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			ctx = withGatewayRequestRoute(ctx, "key", "session")
			compact := p.gatewayRequestClientWithHeaderTimeout(ctx, proxy, gatewayClientCompact, 200*time.Millisecond)
			stream := p.gatewayRequestClientWithHeaderTimeout(ctx, proxy, gatewayClientHTTP, 200*time.Millisecond)
			if compact.Timeout != 200*time.Millisecond || stream.Timeout != p.Client.Timeout {
				t.Fatal("compact total deadline leaked into the streaming client")
			}
			compactReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, target+"/compact", nil)
			compactRes, err := compact.Do(compactReq)
			if err != nil {
				t.Fatal(err)
			}
			defer compactRes.Body.Close()
			streamReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, target+"/stream", nil)
			streamRes, err := stream.Do(streamReq)
			if err != nil {
				t.Fatal(err)
			}
			defer streamRes.Body.Close()
			streamResult := make(chan error, 1)
			go func() {
				body, err := io.ReadAll(streamRes.Body)
				if err == nil && string(body) != "firstlast" {
					err = fmt.Errorf("stream body=%q", body)
				}
				streamResult <- err
			}()
			_, err = io.ReadAll(compactRes.Body)
			var timeout interface{ Timeout() bool }
			if !errors.As(err, &timeout) || !timeout.Timeout() || ctx.Err() != nil {
				t.Fatalf("compact body escaped its total client deadline: %v (context=%v)", err, ctx.Err())
			}
			select {
			case err := <-streamResult:
				t.Fatalf("HTTP body inherited compact/header deadline: %v", err)
			default:
			}
			releaseOnce.Do(func() { close(release) })
			if err := <-streamResult; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGatewayTimeoutPoolDisabledOrDeletedGuardsAllVariants(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(fmt.Sprint("delete=", remove), func(t *testing.T) {
			var hits atomic.Int32
			upstream, _ := poolProxyServer(t, func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				_, _ = io.WriteString(w, "ok")
			})
			p := newTestProxy(t, 1)
			entry := poolProxyEntry(t, p, upstream.URL)
			ctx := withGatewayRequestRoute(t.Context(), "key", "session")
			modes := []gatewayClientMode{gatewayClientHTTP, gatewayClientCompact, gatewayClientWebSocket}
			var clients []*http.Client
			for _, mode := range modes {
				for _, timeout := range []time.Duration{5 * time.Second, 7 * time.Second} {
					client := p.gatewayRequestClientWithHeaderTimeout(ctx, &entry, mode, timeout)
					poolFetch(t, client)
					clients = append(clients, client)
				}
			}
			var err error
			if remove {
				err = p.Proxies.Delete(entry.ID)
			} else {
				_, err = p.Proxies.UpdateVersion(entry.ID, 0, func(v *ProxyEntry) error { v.Enabled = false; return nil })
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, mode := range modes {
				clients = append(clients, p.gatewayRequestClientWithHeaderTimeout(ctx, &entry, mode, 9*time.Second))
			}
			assertRejected := func(client *http.Client) {
				t.Helper()
				res, err := client.Get("http://timeout-pool.invalid/stale")
				if res != nil {
					_ = res.Body.Close()
				}
				if !errors.Is(err, errGatewayProxyUnavailable) {
					t.Fatalf("unavailable proxy timeout client did not fail closed: %v", err)
				}
			}
			for _, client := range clients {
				assertRejected(client)
			}
			if hits.Load() != 6 || len(p.clientPool) != 0 {
				t.Fatalf("unavailable proxy retained timeout entries or sent new requests: hits=%d entries=%d", hits.Load(), len(p.clientPool))
			}
			if !remove {
				updated, err := p.Proxies.UpdateVersion(entry.ID, 0, func(v *ProxyEntry) error { v.Enabled = true; return nil })
				if err != nil {
					t.Fatal(err)
				}
				for _, client := range clients[:6] {
					assertRejected(client)
				}
				fresh := p.gatewayRequestClientWithHeaderTimeout(ctx, &updated, gatewayClientHTTP, 5*time.Second)
				if poolFetch(t, fresh) != "ok" || hits.Load() != 7 {
					t.Fatal("re-enabled proxy did not recover using its new version")
				}
			}
		})
	}
}
