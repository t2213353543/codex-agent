package server

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Route validation must survive cold-start waits before any request envelope
// reaches the real helper. The blocked attempt is never moved to the new proxy.
func TestGatewayNativeActualProxyRotationBeforeDispatch(t *testing.T) {
	if os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN") == "" || runtime.GOOS != "linux" {
		t.Skip("set CODEX_NATIVE_HTTP_TEST_BIN on Linux for actual proxy dispatch guard")
	}
	var oldRequests, newRequests, directRequests atomic.Int32
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directRequests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, "direct routing forbidden by synthetic fixture", http.StatusBadGateway)
	}))
	t.Cleanup(direct.Close)
	oldProxy, oldConnections := poolProxyServer(t, func(w http.ResponseWriter, r *http.Request) {
		oldRequests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, "stale proxy reached", http.StatusBadGateway)
	})
	const requestBody = `{"model":"synthetic-route","input":[{"type":"message","role":"user","content":"new-independent-request"}]}`
	const responseBody = `{"status":"completed","output":[]}`
	const nextUser = "synthetic-current-user"
	const nextPassword = "synthetic-current-password"
	var requestMatched atomic.Bool
	newProxy, newConnections := poolProxyServer(t, func(w http.ResponseWriter, r *http.Request) {
		newRequests.Add(1)
		raw, err := io.ReadAll(r.Body)
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(nextUser+":"+nextPassword))
		matches := err == nil && string(raw) == requestBody && r.Method == http.MethodPost && r.URL.IsAbs() &&
			r.URL.String() == direct.URL+"/responses" && r.Header.Get("Proxy-Authorization") == wantAuth
		requestMatched.Store(matches)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(responseBody)))
		_, _ = io.WriteString(w, responseBody)
	})
	p := newTestProxy(t, 2)
	realGatewayNativePool(t, p)
	entry := poolProxyEntry(t, p, oldProxy.URL)
	guard := &nativeCLIFallbackGuard{}
	p.Client.Transport = guard
	pool := p.NativeHTTP
	gate, entered := make(chan struct{}), make(chan struct{})
	var gateOnce sync.Once
	openGate := func() { gateOnce.Do(func() { close(gate) }) }
	t.Cleanup(openGate)
	var starts atomic.Int32
	original := pool.options.command
	pool.options.command = func(path string) *exec.Cmd {
		if starts.Add(1) == 1 {
			close(entered)
			<-gate
		}
		return original(path)
	}
	client := p.clientFor(&entry)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	rejected := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, direct.URL+"/responses", strings.NewReader(`{"input":"stale-request-must-not-send"}`))
		if err != nil {
			rejected <- err
			return
		}
		res, err := client.Do(req)
		if res != nil && res.Body != nil {
			_ = res.Body.Close()
		}
		rejected <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("native cold-start barrier was not reached")
	}
	pool.mu.Lock()
	starting := 0
	for _, child := range pool.entries {
		if child.starting && child.key.proxyID == entry.ID {
			starting++
		}
	}
	pool.mu.Unlock()
	if starting != 1 {
		t.Fatal("fixture did not pause exactly one selected native proxy startup")
	}
	endpoint, err := url.Parse(newProxy.URL)
	if err != nil {
		t.Fatal("invalid synthetic proxy endpoint")
	}
	port, err := strconv.Atoi(endpoint.Port())
	if err != nil {
		t.Fatal("invalid synthetic proxy port")
	}
	changed, err := p.Proxies.UpdateVersion(entry.ID, entry.Version, func(proxy *ProxyEntry) error {
		proxy.Host, proxy.Port = endpoint.Hostname(), port
		proxy.Username, proxy.Password = nextUser, nextPassword
		return nil
	})
	if err != nil {
		t.Fatal("synthetic proxy configuration rotation failed")
	}
	openGate()
	select {
	case err := <-rejected:
		if !errors.Is(err, errGatewayProxyUnavailable) {
			t.Fatal("native delayed request did not reject the superseded proxy snapshot")
		}
	case <-ctx.Done():
		t.Fatal("stale native dispatch did not terminate after startup")
	}
	if oldRequests.Load() != 0 || newRequests.Load() != 0 || directRequests.Load() != 0 || oldConnections.Load() != 0 || newConnections.Load() != 0 || guard.calls.Load() != 0 {
		t.Fatal("rejected request reached an old/new/direct upstream or Go transport")
	}
	pool.mu.Lock()
	active := 0
	for _, child := range pool.entries {
		active += child.active
	}
	pool.mu.Unlock()
	if active != 0 {
		t.Fatal("stale pre-dispatch rejection retained a native active slot")
	}
	// This is a new independent caller selecting current configuration, not
	// an automatic replay of the rejected request or migration of its body.
	currentClient := p.clientFor(&changed)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, direct.URL+"/responses", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal("cannot create independent synthetic request")
	}
	res, err := currentClient.Do(req)
	if err != nil {
		t.Fatal("new independent request did not use the current native proxy")
	}
	raw, readErr := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if readErr != nil || res.StatusCode != http.StatusOK || string(raw) != responseBody || !requestMatched.Load() {
		t.Fatal("current native proxy altered request body, credentials or response")
	}
	if oldRequests.Load() != 0 || directRequests.Load() != 0 || newRequests.Load() != 1 || newConnections.Load() != 1 || starts.Load() != 2 || guard.calls.Load() != 0 {
		t.Fatal("native dispatch guard replayed, leaked or silently bypassed its proxy")
	}
	if err := pool.Close(); err != nil {
		t.Fatal("native proxy fixture pool did not close")
	}
	pool.mu.Lock()
	remaining := len(pool.entries)
	pool.mu.Unlock()
	if remaining != 0 {
		t.Fatal("native proxy dispatch fixture retained process reservations")
	}
}
