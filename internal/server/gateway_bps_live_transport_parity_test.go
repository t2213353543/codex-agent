package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A real TLS/H2 peer exposes a test observer changing the production fallback
// protocol. The optional H2-only branch is synthetic; it does not claim that
// the live provider rejects every HTTP/1.1 generation.
func TestBPSLiveObserverKeepsProductionHTTPFallbackProtocol(t *testing.T) {
	for _, profile := range []string{"production", "old_observer", "corrected_observer"} {
		for _, requireH2 := range []bool{false, true} {
			name := profile + "/accept_h1"
			if requireH2 {
				name = profile + "/h2_only"
			}
			t.Run(name, func(t *testing.T) {
				protocol := make(chan int, 1)
				upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					protocol <- r.ProtoMajor
					if requireH2 && r.ProtoMajor == 1 {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "fallback", "[]"))
				}))
				upstream.EnableHTTP2 = true
				upstream.StartTLS()
				defer upstream.Close()
				p := newTestProxy(t, 1)
				p.BaseURL = upstream.URL
				p.BPS = &bpsRuntime{}
				base := p.Client.Transport.(*http.Transport).Clone()
				base.Proxy = nil
				base.ForceAttemptHTTP2 = true
				base.TLSClientConfig = upstream.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
				defer base.CloseIdleConnections()
				wire := &bpsLimitedWire{createdSignal: make(chan struct{})}
				switch profile {
				case "production":
					p.Client.Transport = base
				case "old_observer":
					base.ForceAttemptHTTP2 = false
					p.Client.Transport = bpsLimitedTransport{base: base, wire: wire}
				case "corrected_observer":
					ws := websocketHTTPClient(&http.Client{Transport: base}).Transport.(*http.Transport)
					defer ws.CloseIdleConnections()
					p.Client.Transport = bpsLimitedTransport{base: base, wsBase: ws, wire: wire}
				}
				account, err := p.Store.Get("test")
				if err != nil {
					t.Fatal(err)
				}
				settings := defaultSettings()
				settings.PreferWebsocket = true
				r := proxyRequest(http.MethodPost, "/responses", `{}`)
				response, requestErr := p.bpsRequest(t.Context(), r, account, nil, "admin", "/responses", []byte(`{"model":"fixture","input":[],"stream":true}`), settings, &RequestRecord{})
				if response != nil && response.Body != nil {
					_, _ = io.ReadAll(response.Body)
					_ = response.Body.Close()
				}
				want := 2
				if profile == "old_observer" {
					want = 1
				}
				if got := <-protocol; got != want {
					t.Fatalf("HTTP fallback protocol=%d want=%d", got, want)
				}
				if requireH2 && profile == "old_observer" {
					if requestErr == nil || !strings.Contains(wire.transportError, "EOF") {
						t.Fatalf("old observer did not reproduce synthetic H1 rejection: error=%v diagnostic=%q", requestErr, wire.transportError)
					}
				} else if requestErr != nil || response == nil || response.StatusCode != http.StatusOK {
					t.Fatalf("production-compatible fallback failed: %v", requestErr)
				}
				t.Logf("profile=%s h2_only_peer=%t observed_http_major=%d request_succeeded=%t error_diagnostic=%q", profile, requireH2, want, requestErr == nil, wire.transportError)
			})
		}
	}
}
