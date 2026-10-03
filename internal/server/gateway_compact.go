package server

import (
	"io"
	"net/http"
	"sync"
	"time"
)

// The legacy Codex 0.154.0 JSON compact contract gives compaction four times
// the provider's stream idle timeout. Keep that allowance for gateway JSON
// compact callers, including the v2 adapter. Native Codex 0.155.0 compaction
// instead arrives on /responses and uses the normal SSE idle timeout.
const upstreamCompactTimeout = 4 * upstreamStreamIdleTimeout

func doCompactRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	return doCompactRequestWithTimeout(client, req, upstreamCompactTimeout)
}

func doCompactRequestWithTimeout(client *http.Client, req *http.Request, timeout time.Duration) (*http.Response, error) {
	compactClient := *client
	compactClient.Timeout = timeout
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	var cleanup func()
	if pooled, ok := transport.(*gatewayClientTransport); ok {
		// The normal gateway uses a shared compact variant. Keep the helper's
		// explicit timeout contract for other callers without mutating its pool.
		if pooled.transport.ResponseHeaderTimeout != timeout {
			private := pooled.transport.Clone()
			private.ResponseHeaderTimeout = timeout
			guarded := &gatewayClientTransport{transport: private, store: pooled.store, proxyID: pooled.proxyID, fingerprint: pooled.fingerprint}
			compactClient.Transport = guarded
			cleanup = guarded.retire
		}
	} else if base, ok := transport.(*http.Transport); ok {
		private := base.Clone()
		private.ResponseHeaderTimeout = timeout
		compactClient.Transport = private
		cleanup = private.CloseIdleConnections
	}
	res, err := compactClient.Do(req)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return res, err
	}
	if cleanup != nil {
		res.Body = &compactResponseBody{ReadCloser: res.Body, cleanup: cleanup}
	}
	return res, nil
}

type compactResponseBody struct {
	io.ReadCloser
	cleanup func()
	once    sync.Once
	err     error
}

func (b *compactResponseBody) Close() error {
	b.once.Do(func() {
		b.err = b.ReadCloser.Close()
		b.cleanup()
	})
	return b.err
}
