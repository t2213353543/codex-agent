package server

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The gate pauses the Go process factory, not a shell wrapper or the sealed
// executable. Once opened, the surviving caller uses the actual Rust helper.
func TestGatewayNativeActualSharedStartupCreatorCancellation(t *testing.T) {
	if os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN") == "" || runtime.GOOS != "linux" {
		t.Skip("set CODEX_NATIVE_HTTP_TEST_BIN for actual shared-startup cancellation")
	}
	p := newTestProxy(t, 2)
	cert, _, ca := nativeCaptureCertificate(t)
	private := nativeStartupTempDirectory(t)
	t.Setenv("TMPDIR", private)
	t.Setenv("CODEX_CA_CERTIFICATE", ca)
	var upstreamCalls atomic.Int32
	var actualBody string
	var bodyMu sync.Mutex
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		data, _ := io.ReadAll(r.Body)
		bodyMu.Lock()
		actualBody = string(data)
		bodyMu.Unlock()
		if r.TLS == nil || r.ProtoMajor != 2 {
			t.Error("surviving startup request did not use actual TLS HTTP/2")
		}
		_, _ = io.WriteString(w, "survivor-response")
	}))
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	realGatewayNativePool(t, p)
	pool := p.NativeHTTP
	pool.mu.Lock()
	initialEntries := len(pool.entries)
	pool.mu.Unlock()
	if initialEntries != 0 {
		t.Fatal("startup preflight left a serving helper behind")
	}
	guard := &nativeCLIFallbackGuard{}
	p.Client.Transport = guard
	gate, entered := make(chan struct{}), make(chan struct{})
	var gateOnce sync.Once
	openGate := func() { gateOnce.Do(func() { close(gate) }) }
	t.Cleanup(openGate)
	var starts atomic.Int32
	original := pool.options.command
	pool.options.command = func(path string) *exec.Cmd {
		if starts.Add(1) == 1 {
			close(entered)
		}
		<-gate
		return original(path)
	}
	client := p.clientFor(nil)
	creatorCtx, cancelCreator := context.WithCancel(t.Context())
	defer cancelCreator()
	followerCtx, cancelFollower := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancelFollower()
	type outcome struct {
		body string
		err  error
	}
	do := func(ctx context.Context, body string) outcome {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/responses", strings.NewReader(body))
		if err != nil {
			return outcome{err: err}
		}
		res, err := client.Do(req)
		if err != nil {
			return outcome{err: err}
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		return outcome{body: string(raw), err: err}
	}
	creatorDone, followerDone := make(chan outcome, 1), make(chan outcome, 1)
	go func() { creatorDone <- do(creatorCtx, "creator-must-not-send") }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("actual helper launch gate was not reached")
	}
	go func() { followerDone <- do(followerCtx, "survivor-request") }()
	waiters := func(want int) *gatewayNativeProcess {
		t.Helper()
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			pool.mu.Lock()
			var found *gatewayNativeProcess
			for _, child := range pool.entries {
				if child.starting && child.startupWaiters == want && !child.startupAbandoned {
					found = child
				}
			}
			pool.mu.Unlock()
			if found != nil {
				return found
			}
			select {
			case <-deadline.C:
				t.Fatal("shared startup did not reach the expected waiter state")
			case <-tick.C:
			}
		}
	}
	reserved := waiters(2)
	cancelCreator()
	select {
	case result := <-creatorDone:
		if !errors.Is(result.err, context.Canceled) {
			t.Fatal("startup creator did not retain its own cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("creator cancellation waited for another caller's startup")
	}
	if waiters(1) != reserved {
		t.Fatal("creator cancellation replaced rather than shared the startup")
	}
	openGate()
	select {
	case result := <-followerDone:
		if result.err != nil || result.body != "survivor-response" {
			t.Fatal("live startup waiter inherited creator cancellation or lost its response")
		}
	case <-followerCtx.Done():
		t.Fatal("surviving caller did not finish through the actual Rust helper")
	}
	bodyMu.Lock()
	bodyCorrect := actualBody == "survivor-request"
	bodyMu.Unlock()
	if starts.Load() != 1 || upstreamCalls.Load() != 1 || !bodyCorrect || guard.calls.Load() != 0 {
		t.Fatal("shared startup replayed, relaunched, changed body, or used Go fallback")
	}
	if err := pool.Close(); err != nil {
		t.Fatal("shared startup pool did not close")
	}
	pool.mu.Lock()
	remaining := len(pool.entries)
	pool.mu.Unlock()
	if remaining != 0 || reserved.command == nil || reserved.command.ProcessState == nil || !reserved.command.ProcessState.Success() {
		t.Fatal("shared startup retained an unreaped helper or reservation")
	}
	if entries, err := os.ReadDir(private); err != nil || len(entries) != 0 {
		t.Fatal("shared startup retained its private IPC directory")
	}
	select {
	case <-pool.done:
	default:
		t.Fatal("shared startup pool reaper did not exit")
	}
}
