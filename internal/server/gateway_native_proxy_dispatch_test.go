package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestGatewayNativeHTTPProxyChangeBeforeDispatch(t *testing.T) {
	for _, change := range []string{"password", "disabled", "deleted", "unchanged"} {
		t.Run(change, func(t *testing.T) {
			p := newTestProxy(t, 1)
			pool, entered, release, starts := nativeStartupBarrierPool(t)
			p.NativeHTTP = pool
			entry := poolProxyEntry(t, p, "http://127.0.0.1:1")
			// Cleanup is LIFO; unblock before either proxy or pool cleanup waits.
			t.Cleanup(release)
			client := p.clientFor(&entry)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			body := &gatewayNativeTrackedBody{Reader: strings.NewReader("before-proxy-change")}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture.invalid/echo", body)
			if err != nil {
				t.Fatal(err)
			}
			type result struct {
				response *http.Response
				err      error
			}
			done := make(chan result, 1)
			go func() {
				response, err := client.Do(req)
				done <- result{response, err}
			}()
			nativeStartupWaitSignal(t, entered)
			if starts.Load() != 1 {
				t.Fatal("request did not wait for exactly one cold helper start")
			}
			switch change {
			case "password":
				entry, err = p.Proxies.UpdateVersion(entry.ID, entry.Version, func(v *ProxyEntry) error {
					v.Username, v.Password = "rotated-user", "rotated-password"
					return nil
				})
			case "disabled":
				entry, err = p.Proxies.UpdateVersion(entry.ID, entry.Version, func(v *ProxyEntry) error {
					v.Enabled = false
					return nil
				})
			case "deleted":
				err = p.Proxies.Delete(entry.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			release()
			var completed result
			select {
			case completed = <-done:
			case <-ctx.Done():
				t.Fatal("request did not finish after the controlled startup barrier")
			}
			var received []byte
			if completed.response != nil {
				received, err = io.ReadAll(completed.response.Body)
				closeErr := completed.response.Body.Close()
				if err != nil || closeErr != nil {
					t.Fatal("fixture response could not be drained and closed")
				}
			}
			if change == "unchanged" {
				if completed.err != nil || completed.response == nil || completed.response.StatusCode != http.StatusAccepted || string(received) != "before-proxy-change" {
					t.Errorf("unchanged proxy failed controlled cold startup: %v", completed.err)
				}
			} else if completed.response != nil || !errors.Is(completed.err, errGatewayProxyUnavailable) {
				t.Errorf("proxy %s during cold startup dispatched using the stale snapshot: response=%t err=%v", change, completed.response != nil, completed.err)
			}
			if body.closed.Load() != 1 {
				t.Errorf("request body closed %d times, want once", body.closed.Load())
			}
			nativeProxyDispatchAssertIdle(t, pool)

			// A separate request may use a newly selected route. It must not be
			// confused with replaying the rejected request on updated credentials.
			switch change {
			case "disabled":
				entry, err = p.Proxies.UpdateVersion(entry.ID, entry.Version, func(v *ProxyEntry) error {
					v.Enabled = true
					return nil
				})
			case "deleted":
				entry = poolProxyEntry(t, p, "http://127.0.0.1:1")
			}
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture.invalid/echo", strings.NewReader("independent-new-request"))
			if err != nil {
				t.Fatal(err)
			}
			response, err := p.clientFor(&entry).Do(fresh)
			if err != nil {
				t.Fatalf("fresh independent proxy selection failed: %v", err)
			}
			received, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if readErr != nil || closeErr != nil || response.StatusCode != http.StatusAccepted || string(received) != "independent-new-request" {
				t.Fatal("fresh independent request did not preserve its own payload")
			}
			wantStarts := int32(2)
			if change == "unchanged" {
				wantStarts = 1
			}
			if starts.Load() != wantStarts {
				t.Errorf("helper starts=%d, want %d", starts.Load(), wantStarts)
			}
			nativeProxyDispatchAssertIdle(t, pool)
		})
	}
}

func TestGatewayNativeHTTPProxyChangeBeforeIPCHeader(t *testing.T) {
	p := newTestProxy(t, 1)
	pool := nativeHTTPFixturePool(t, nil)
	p.NativeHTTP = pool
	entry := poolProxyEntry(t, p, "http://127.0.0.1:1")
	client := p.clientFor(&entry)
	guard, ok := client.Transport.(*gatewayNativeGuardedTransport)
	if !ok {
		t.Fatal("fixture did not select the guarded native HTTP transport")
	}
	transport, ok := guard.transport.(*gatewayNativeRoundTripper)
	if !ok || transport.checkProxy == nil {
		t.Fatal("native HTTP transport has no send-boundary proxy validator")
	}
	check := transport.checkProxy
	checks := 0
	transport.checkProxy = func() error {
		checks++
		if err := check(); err != nil {
			return err
		}
		if checks == 1 {
			// The first post-acquire check has passed. Rotate before the next
			// checkpoint so the connected IPC stream must not send its header.
			_, err := p.Proxies.UpdateVersion(entry.ID, entry.Version, func(v *ProxyEntry) error {
				v.Username, v.Password = "late-rotated-user", "late-rotated-password"
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	body := &gatewayNativeTrackedBody{Reader: strings.NewReader("never-dispatch-stale-route")}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture.invalid/echo", body)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(req)
	if response != nil {
		_ = response.Body.Close()
	}
	if response != nil || !errors.Is(err, errGatewayProxyUnavailable) || checks != 2 {
		t.Errorf("proxy rotation between dispatch checks was not rejected: response=%t checks=%d err=%v", response != nil, checks, err)
	}
	if body.closed.Load() != 1 {
		t.Errorf("request body closed %d times, want once", body.closed.Load())
	}
	nativeProxyDispatchAssertIdle(t, pool)
}

func nativeProxyDispatchAssertIdle(t *testing.T, pool *gatewayNativePool) {
	t.Helper()
	pool.mu.Lock()
	active := 0
	for _, child := range pool.entries {
		active += child.active
	}
	pool.mu.Unlock()
	if active != 0 {
		t.Errorf("completed request retained %d active helper slots", active)
	}
}
