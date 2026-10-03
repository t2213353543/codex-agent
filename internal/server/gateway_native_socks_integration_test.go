package server

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGatewayNativeActualSOCKSTLSAndRemoteDNS(t *testing.T) {
	t.Setenv("CODEX_NATIVE_HTTP_TEST_LIFECYCLE", "pooled")
	p := newTestProxy(t, 1)
	cert, _, caFile := nativeCaptureCertificate(t)
	t.Setenv("CODEX_CA_CERTIFICATE", caFile)
	realGatewayNativePool(t, p)
	p.Client.Transport = sourceRefreshRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Error("native SOCKS silently fell back to Go")
		return nil, errGatewayNativeUnavailable
	})
	var mu sync.Mutex
	var hellos []nativeCaptureHello
	var requests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.ProtoMajor != 2 || r.TLS == nil || r.TLS.Version != tls.VersionTLS13 || r.Header.Get("Proxy-Authorization") != "" || string(body) != "raw\x00entity" {
			t.Error("SOCKS route changed upstream TLS/protocol/headers/body")
		}
		w.Header().Set("Content-Length", "12")
		_, _ = io.WriteString(w, "native-socks")
	}))
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	server.Listener = &nativeCaptureListener{Listener: server.Listener, onHello: func(hello nativeCaptureHello) { mu.Lock(); hellos = append(hellos, hello); mu.Unlock() }}
	server.StartTLS()
	defer server.Close()
	socks, proxy := nativeSOCKSTestServer(t, server.Listener.Addr().String(), "fixture-user", "fixture-pass")
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	target := "https://localhost:" + port + "/responses"
	for _, route := range []*ProxyEntry{nil, &proxy, &proxy, &proxy} {
		req, _ := http.NewRequestWithContext(t.Context(), "POST", target, strings.NewReader("raw\x00entity"))
		client := &http.Client{Transport: p.NativeHTTP.RoundTripper(route, 3*time.Second), Timeout: 5 * time.Second}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil || string(body) != "native-socks" {
			t.Fatalf("actual native SOCKS response: %v", err)
		}
	}
	if requests.Load() != 4 || socks.dials.Load() != 1 {
		t.Fatalf("native SOCKS tunnel not reused: requests=%d SOCKS=%d", requests.Load(), socks.dials.Load())
	}
	if address := <-socks.requests; address != "3:localhost:"+port {
		t.Fatal("native SOCKS failed remote DNS contract")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hellos) != 2 {
		t.Fatalf("TLS handshakes=%d want direct plus one SOCKS tunnel", len(hellos))
	}
	left, right := hellos[0], hellos[1]
	left.Extensions, right.Extensions = slices.Clone(left.Extensions), slices.Clone(right.Extensions)
	slices.Sort(left.Extensions)
	slices.Sort(right.Extensions)
	if !reflect.DeepEqual(left, right) {
		t.Fatal("SOCKS bridge changed parsed Rust ClientHello fields")
	}
}

func TestGatewayNativeActualSOCKSFailureDoesNotRefreshOrFallback(t *testing.T) {
	var directCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directCalls.Add(1)
		_, _ = io.WriteString(w, "must not reach")
	}))
	defer server.Close()
	socks, proxy := nativeSOCKSTestServer(t, strings.TrimPrefix(server.URL, "http://"), "fixture-user", "correct")
	proxy.Password = "wrong"
	proxy.Name = "synthetic SOCKS"
	p := newTestProxy(t, 1)
	realGatewayNativePool(t, p)
	p.BaseURL = server.URL
	entry, err := p.Proxies.Create(proxy)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Proxies = []string{entry.ID}; return nil }); err != nil {
		t.Fatal(err)
	}
	var fallback atomic.Int32
	p.Client.Transport = sourceRefreshRoundTripper(func(*http.Request) (*http.Response, error) { fallback.Add(1); return nil, errGatewayNativeUnavailable })
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest("POST", "/responses", `{"model":"test","stream":true,"input":[]}`))
	if w.Code != http.StatusBadGateway || socks.dials.Load() != 1 || fallback.Load() != 0 || directCalls.Load() != 0 {
		t.Fatalf("failed SOCKS route escaped boundary: HTTP=%d SOCKS=%d direct=%d fallback=%d", w.Code, socks.dials.Load(), directCalls.Load(), fallback.Load())
	}
}
