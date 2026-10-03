package server

import (
	"context"
	"net/http"
	"time"
)

// Each request snapshots its timeout in an immutable, bounded pool variant.
// Reuse the same proxy fingerprint guard as requests using the default timeout.
func (p *Proxy) gatewayRequestClientWithHeaderTimeout(ctx context.Context, proxy *ProxyEntry, mode gatewayClientMode, timeout time.Duration) *http.Client {
	return p.inferenceClient(ctx, proxy, mode, &timeout)
}

// Standalone callers without a Proxy owner use a private, explicitly retired
// variant. Gateway requests use the owner's bounded pool above instead.
func gatewayClientWithHeaderTimeout(client *http.Client, timeout time.Duration) (*http.Client, func()) {
	noop := func() {}
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	var original *http.Transport
	var pooled *gatewayClientTransport
	switch t := transport.(type) {
	case *http.Transport:
		original = t
	case *gatewayClientTransport:
		original, pooled = t.transport, t
	default:
		// Injected RoundTrippers own their transport and timeout policy.
		return client, noop
	}
	if original.ResponseHeaderTimeout == timeout {
		return client, noop
	}
	private := original.Clone()
	private.ResponseHeaderTimeout = timeout
	configured := *client
	configured.Transport = private
	cleanup := private.CloseIdleConnections
	if pooled != nil {
		guarded := &gatewayClientTransport{transport: private, store: pooled.store, proxyID: pooled.proxyID, fingerprint: pooled.fingerprint}
		configured.Transport = guarded
		cleanup = guarded.retire
	}
	return &configured, cleanup
}
