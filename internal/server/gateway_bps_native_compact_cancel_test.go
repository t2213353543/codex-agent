package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type bpsCompactReadSignal struct {
	io.ReadCloser
	started chan struct{}
	once    sync.Once
}

func (b *bpsCompactReadSignal) Read(value []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	return b.ReadCloser.Read(value)
}

func TestBPSNativeCompactBodyCancellationIsAborted(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"comp_canceled","output":[`)
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
		close(stopped)
	})
	base := p.Client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	p.Client.Transport = bpsCacheLiveTransport(func(r *http.Request) (*http.Response, error) {
		response, err := base.RoundTrip(r)
		if err == nil {
			response.Body = &bpsCompactReadSignal{ReadCloser: response.Body, started: started}
		}
		return response, err
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := proxyRequest(http.MethodPost, "/v1/responses/compact", `{"model":"gpt-6-astra","input":"synthetic compact"}`).WithContext(ctx)
	response := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() { defer close(finished); p.gatewayBPS(response, request) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("did not reach upstream")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("gateway did not stop")
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not cancel")
	}
	record, ok := p.Metrics.LastFor("test")
	if !ok {
		t.Fatal("record absent")
	}
	t.Logf("status=%s source=%s phase=%s http=%d input=%d usage_known=%v public_bytes=%d", record.Status, record.ErrorSource, record.ErrorPhase, record.HTTPCode, record.InputTokens, record.UsageKnown, response.Body.Len())
	if record.Status != "aborted" || record.ErrorSource != "client" || record.ErrorPhase != "canceled" {
		t.Errorf("actual caller cancellation counted as upstream failure: %s/%s/%s", record.Status, record.ErrorSource, record.ErrorPhase)
	}
	if record.UsageKnown != nil || record.CachedTokensKnown != nil || record.CacheWriteTokens != nil || record.TTFTMs != nil || record.ReturnedModel != "" {
		t.Fatal("partial compact body invented complete metadata")
	}
	if response.Body.Len() != 0 {
		t.Error("canceled client received fabricated 502 payload")
	}
}

func TestBPSUpstreamHeadersCallerCancellationIsAborted(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
		t.Run(path, func(t *testing.T) {
			started, stopped := make(chan struct{}), make(chan struct{})
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(started)
				<-r.Context().Done()
				close(stopped)
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			request := proxyRequest(http.MethodPost, path, `{"model":"gpt-6-astra","stream":false,"input":"synthetic canceled request"}`).WithContext(ctx)
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); p.gatewayBPS(response, request) }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("not dispatched")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("gateway still blocked")
			}
			select {
			case <-stopped:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream still blocked")
			}
			record, ok := p.Metrics.LastFor("test")
			if !ok || record.Status != "aborted" || record.ErrorSource != "client" || record.ErrorPhase != "canceled" || response.Body.Len() != 0 {
				t.Fatalf("caller cancellation misclassified: status=%s source=%s phase=%s publicbytes=%d", record.Status, record.ErrorSource, record.ErrorPhase, response.Body.Len())
			}
			if record.UsageKnown != nil || record.CachedTokensKnown != nil || record.CacheWriteTokens != nil {
				t.Fatal("usage invented without response")
			}
		})
	}
}
