package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func realGatewayNativePool(t *testing.T, p *Proxy) {
	t.Helper()
	bin := os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN")
	if bin == "" || runtime.GOOS != "linux" {
		t.Skip("set CODEX_NATIVE_HTTP_TEST_BIN on Linux for actual Rust HTTP integration")
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal("native test binary unavailable")
	}
	sum := sha256.Sum256(data)
	err = configureGatewayNativeHTTP(p, func(key string) string {
		if key == "CODEX_NATIVE_HTTP_LIFECYCLE" {
			return os.Getenv("CODEX_NATIVE_HTTP_TEST_LIFECYCLE")
		}
		if key == "CODEX_NATIVE_HTTP_HELPER" {
			return bin
		}
		if key == "CODEX_NATIVE_HTTP_SHA256" {
			return hex.EncodeToString(sum[:])
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.closeGatewayClients)
}

func TestGatewayNativeActualRoutesAndOAuth(t *testing.T) {
	const object = `{"id":"native-complete","status":"completed","output":[],"usage":{"input_tokens":8,"output_tokens":2,"input_tokens_details":{"cached_tokens":4}}}`
	const event = "data: {\"type\":\"response.completed\",\"response\":" + object + "}\n\n"
	for _, scenario := range []struct{ name, method, path, body, want string }{
		{"sse", "POST", "/responses", `{"model":"test","stream":true,"input":[]}`, event},
		{"json", "POST", "/responses", `{"model":"test","stream":false,"input":[]}`, object},
		{"compact", "POST", "/responses/compact", `{"model":"test","input":[]}`, `{"output":[]}`},
		{"models", "GET", "/models?client_version=0.154.0", "", `{"models":[]}`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var attempts, refreshes atomic.Int32
			var firstBody string
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/token" {
					refreshes.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"synthetic-rotated","refresh_token":"synthetic-rotated-rt"}`, "test-account", "fixture-user"))
					return
				}
				body, _ := io.ReadAll(r.Body)
				if attempts.Add(1) == 1 {
					firstBody = string(body)
					w.WriteHeader(401)
					_, _ = io.WriteString(w, `{"error":"synthetic-expired"}`)
					return
				}
				if string(body) != firstBody || r.Header.Get("Authorization") != "Bearer synthetic-rotated" || r.Header.Get("ChatGPT-Account-Id") != "test-account" {
					t.Error("native OAuth recovery changed account or request bytes")
				}
				w.Header().Set("X-Request-Id", "native-request")
				if scenario.name == "compact" || scenario.name == "models" {
					_, _ = io.WriteString(w, scenario.want)
				} else {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, event)
				}
			})
			realGatewayNativePool(t, p)
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest(scenario.method, scenario.path, scenario.body))
			if w.Code != 200 || w.Body.String() != scenario.want || attempts.Load() != 2 || refreshes.Load() != 1 {
				t.Fatalf("native route mismatch: status=%d attempts=%d refresh=%d", w.Code, attempts.Load(), refreshes.Load())
			}
			stored, err := p.Store.Get("test")
			if err != nil || stored.RefreshToken != "synthetic-rotated-rt" {
				t.Fatal("native refresh rotation not persisted")
			}
			rec, ok := p.Metrics.LastFor("test")
			if !ok || rec.UpstreamHeaderMs == nil || rec.RequestID != "native-request" || p.inFlight("test") != 0 {
				t.Fatal("native observation or admission lifecycle incomplete")
			}
		})
	}
}

func TestGatewayNativeActualCancellationAndNoReplay(t *testing.T) {
	var calls atomic.Int32
	cancelled := make(chan struct{})
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": active\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	})
	realGatewayNativePool(t, p)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", p.BaseURL+"/responses", strings.NewReader(`{}`))
	res, err := p.clientFor(nil).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := res.Body.Read(buf); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = res.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("native body close did not cancel upstream")
	}
	if calls.Load() != 1 {
		t.Fatal("native cancellation replayed request")
	}
}

func TestGatewayNativeActualProxyReuseAndRotation(t *testing.T) {
	t.Setenv("CODEX_NATIVE_HTTP_TEST_LIFECYCLE", "pooled")
	server, connections := poolProxyServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Length", "2")
		_, _ = io.WriteString(w, "ok")
	})
	p := newTestProxy(t, 1)
	realGatewayNativePool(t, p)
	entry := poolProxyEntry(t, p, server.URL)
	client := p.clientFor(&entry)
	for i := 0; i < 3; i++ {
		if poolFetch(t, client) != "ok" {
			t.Fatal("native proxy body changed")
		}
	}
	if connections.Load() != 1 {
		t.Fatal("native process pool did not reuse upstream connection")
	}
	if _, err := p.Proxies.UpdateVersion(entry.ID, 0, func(v *ProxyEntry) error { v.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get("http://pool-upstream.invalid/rejected"); err == nil {
		t.Fatal("native disabled proxy was used")
	}
	if connections.Load() != 1 {
		t.Fatal("native proxy rejection opened another connection")
	}
}

func TestGatewayNativeActualDefaultCLIRequestLifecycle(t *testing.T) {
	server, connections := poolProxyServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Length", "2")
		_, _ = io.WriteString(w, "ok")
	})
	p := newTestProxy(t, 1)
	t.Setenv("CODEX_NATIVE_HTTP_TEST_LIFECYCLE", "")
	realGatewayNativePool(t, p)
	entry := poolProxyEntry(t, p, server.URL)
	client := p.clientFor(&entry)
	for range 4 {
		if poolFetch(t, client) != "ok" {
			t.Fatal("strict native HTTP changed payload")
		}
	}
	if connections.Load() != 4 {
		t.Fatalf("CLI request lifecycle unexpectedly reused TCP: %d", connections.Load())
	}
	p.NativeHTTP.mu.Lock()
	defer p.NativeHTTP.mu.Unlock()
	if len(p.NativeHTTP.entries) != 1 {
		t.Fatal("strict HTTP must still share its bounded helper process")
	}
}
