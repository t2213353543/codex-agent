package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGatewayNativeConfigurationIsExplicit(t *testing.T) {
	for _, values := range []map[string]string{
		{},
		{"CODEX_NATIVE_HTTP_HELPER": "missing"},
		{"CODEX_NATIVE_HTTP_SHA256": strings.Repeat("0", 64)},
		{"CODEX_NATIVE_HTTP_HELPER": "missing", "CODEX_NATIVE_HTTP_SHA256": "not-a-digest"},
		{"CODEX_NATIVE_WEBSOCKET": "1"},
		{"CODEX_NATIVE_WEBSOCKET": "true"},
		{"CODEX_NATIVE_HTTP_LIFECYCLE": "unknown"},
	} {
		p := &Proxy{}
		err := configureGatewayNativeHTTP(p, func(key string) string { return values[key] })
		if (len(values) == 0) != (err == nil) || p.NativeHTTP != nil {
			t.Fatal("native startup accepted a partial or invalid configuration")
		}
	}
}

func TestGatewayNativeLocalCapacityIsNotAnUpstream502(t *testing.T) {
	for _, failure := range []error{errGatewayNativeCapacity, errGatewayNativeClosed, errGatewayNativeProxy} {
		if gatewayTransportFailureStatus(failure) != 503 {
			t.Fatal("local native capacity/configuration became upstream 502")
		}
		rec := &RequestRecord{}
		observeGatewayTransportError(rec, failure, "response_headers")
		if rec.ErrorSource != "gateway" {
			t.Fatal("native local failure recorded as upstream transport")
		}
	}
	if gatewayTransportFailureStatus(io.ErrUnexpectedEOF) != 502 {
		t.Fatal("ordinary upstream failure status changed")
	}
}

func TestGatewayNativeProxyGuardAndRequestPin(t *testing.T) {
	p := newTestProxy(t, 1)
	entry := poolProxyEntry(t, p, "http://127.0.0.1:1")
	var calls atomic.Int32
	inner := sourceRefreshRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: req}, nil
	})
	guard := &gatewayNativeGuardedTransport{transport: inner, store: p.Proxies, proxyID: entry.ID, fingerprint: gatewayProxyFingerprint(entry)}
	client := &http.Client{Transport: guard}
	ctx := withGatewayRequestRoute(context.Background(), "key", "session")
	if gatewayPinnedRequestClient(ctx, &entry, client) != client {
		t.Fatal("native transport rejected its matching request snapshot")
	}
	res, err := client.Get("http://native.invalid/first")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	changed, err := p.Proxies.UpdateVersion(entry.ID, 0, func(value *ProxyEntry) error { value.Password = "synthetic-new-password"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Get("http://native.invalid/stale"); !errors.Is(err, errGatewayProxyUnavailable) {
		t.Fatal("stale native proxy configuration sent another request")
	}
	newGuard := &gatewayNativeGuardedTransport{transport: inner, store: p.Proxies, proxyID: changed.ID, fingerprint: gatewayProxyFingerprint(changed)}
	stale := gatewayPinnedRequestClient(ctx, &entry, &http.Client{Transport: newGuard})
	if _, err = stale.Get("http://native.invalid/mid-request-rotation"); !errors.Is(err, errGatewayProxyUnavailable) {
		t.Fatal("admitted request silently adopted a newer native proxy")
	}
	if calls.Load() != 1 {
		t.Fatal("rejected native requests reached transport")
	}
}

func TestGatewayNativeFactoryLeavesWebSocketExplicitlyGo(t *testing.T) {
	p := newTestProxy(t, 1)
	p.NativeHTTP = &gatewayNativePool{}
	// No helper process starts while constructing a client. WS must not route
	// through the HTTP-only adapter or lose its duplex 101 body semantics.
	ws := p.gatewayClientFor(nil, gatewayClientWebSocket)
	if _, ok := ws.Transport.(*gatewayNativeGuardedTransport); ok {
		t.Fatal("HTTP-only native selection unexpectedly replaced WebSocket")
	}
	custom := 7 * time.Second
	compact := p.gatewayClientForConfigured(nil, gatewayClientCompact, &custom)
	if compact.Timeout != custom {
		t.Fatal("native compact lost its configured total timeout")
	}
	if _, ok := compact.Transport.(*gatewayNativeGuardedTransport); !ok {
		t.Fatal("native compact silently selected Go transport")
	}
	// Avoid closing an uninitialized synthetic pool; this test starts no child.
	p.NativeHTTP = nil
	p.closeGatewayClients()
}
