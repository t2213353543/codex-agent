package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

// Native HTTP and WebSocket are separate explicit startup choices. Neither
// transport falls back to Go when its native helper cannot serve the request.
func configureGatewayNativeHTTP(p *Proxy, getenv func(string) string) error {
	if p == nil || getenv == nil {
		return errors.New("native HTTP configuration requires a proxy and environment reader")
	}
	if p.NativeHTTP != nil || p.NativeWebSocket {
		return errors.New("native transport is already configured")
	}
	binary := strings.TrimSpace(getenv("CODEX_NATIVE_HTTP_HELPER"))
	digest := strings.TrimSpace(getenv("CODEX_NATIVE_HTTP_SHA256"))
	websocket := strings.TrimSpace(getenv("CODEX_NATIVE_WEBSOCKET"))
	lifecycle := strings.TrimSpace(getenv("CODEX_NATIVE_HTTP_LIFECYCLE"))
	if lifecycle == "" {
		lifecycle = "cli_request"
	}
	if lifecycle != "cli_request" && lifecycle != "pooled" {
		return errors.New("CODEX_NATIVE_HTTP_LIFECYCLE must be cli_request or pooled")
	}
	if websocket != "" && websocket != "0" && websocket != "1" {
		return errors.New("CODEX_NATIVE_WEBSOCKET must be 0 or 1")
	}
	if binary == "" && digest == "" && websocket != "1" {
		return nil
	}
	if binary == "" || digest == "" {
		return errors.New("native HTTP requires both helper path and SHA256")
	}
	pool, err := newGatewayNativePoolWithLifecycle(binary, digest, lifecycle)
	if err != nil {
		return err
	}
	var required []string
	if getenv("CODEX_APP_SERVER_CLI") == "1" {
		required = []string{"unbounded_payload_v1", "websocket_message_v2", "websocket_no_compression_v1"}
	}
	if err := pool.validateStartup(context.Background(), websocket == "1", required...); err != nil {
		_ = pool.Close()
		return err
	}
	p.NativeHTTP = pool
	p.NativeWebSocket = websocket == "1"
	log.Printf("Native HTTP client lifecycle: %s", lifecycle)
	if p.NativeWebSocket {
		log.Print("Upstream transport: HTTP and WebSocket native Rust")
	} else {
		log.Print("Upstream transport: HTTP native Rust; WebSocket Go (native WebSocket not enabled)")
	}
	return nil
}

func (p *Proxy) gatewayNativeClientForConfigured(selected *ProxyEntry, mode gatewayClientMode, headerTimeout *time.Duration) *http.Client {
	base := p.Client
	if base == nil {
		base = http.DefaultClient
	}
	client := *base
	timeout := time.Duration(0)
	transport := base.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if original, ok := transport.(*http.Transport); ok {
		timeout = original.ResponseHeaderTimeout
	}
	if mode == gatewayClientCompact {
		timeout = upstreamCompactTimeout
	}
	if headerTimeout != nil {
		timeout = *headerTimeout
	}
	if mode == gatewayClientCompact {
		client.Timeout = timeout
	}
	var snapshot *ProxyEntry
	var store *ProxyStore
	var fingerprint [32]byte
	if selected != nil {
		copy := *selected
		snapshot = &copy
		if copy.ID != "" && p.Proxies != nil {
			current, ok := p.Proxies.Get(copy.ID)
			if !ok {
				return &http.Client{Transport: gatewayClientErrorTransport{}}
			}
			snapshot, store = current, p.Proxies
		}
		fingerprint = gatewayProxyFingerprint(*snapshot)
	}
	native := p.NativeHTTP.RoundTripper(snapshot, timeout)
	guard := &gatewayNativeGuardedTransport{
		transport: native,
		store:     store, fingerprint: fingerprint,
	}
	if snapshot != nil {
		guard.proxyID = snapshot.ID
	}
	native.checkProxy = guard.checkProxy
	client.Transport = guard
	return &client
}

type gatewayNativeGuardedTransport struct {
	transport   http.RoundTripper
	store       *ProxyStore
	proxyID     string
	fingerprint [32]byte
}

func gatewayTransportFailureStatus(err error) int {
	var authorization *gatewayRecoveryAuthorizationError
	if errors.As(err, &authorization) {
		return authorization.status
	}
	var ownerChanged *gatewayOwnerChangedError
	if errors.As(err, &ownerChanged) {
		return http.StatusConflict
	}
	if errors.Is(err, errGatewayNativeCapacity) || errors.Is(err, errGatewayNativeClosed) || errors.Is(err, errGatewayNativeProxy) || errors.Is(err, errGatewayCompressionCapability) || errors.Is(err, errGatewayNativeUnboundedReady) || errors.Is(err, errNativeWSNoCompressionReady) {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadGateway
}

func (t *gatewayNativeGuardedTransport) checkProxy() error {
	if t.store != nil {
		current, ok := t.store.Get(t.proxyID)
		if !ok || gatewayProxyFingerprint(*current) != t.fingerprint {
			return errGatewayProxyUnavailable
		}
	}
	return nil
}

func (t *gatewayNativeGuardedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.checkProxy(); err != nil {
		return (gatewayClientErrorTransport{}).RoundTrip(req)
	}
	return t.transport.RoundTrip(req)
}
