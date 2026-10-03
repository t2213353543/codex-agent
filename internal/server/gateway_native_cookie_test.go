package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Routes a synthetic certificate for the official hostname through a local
// CONNECT fixture; no public DNS or request is made to ChatGPT.
func TestGatewayNativeActualInfrastructureCookieScope(t *testing.T) {
	t.Setenv("CODEX_NATIVE_HTTP_TEST_LIFECYCLE", "pooled")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Synthetic cookie root"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	root, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "chatgpt.com"}, DNSNames: []string{"chatgpt.com", "example.invalid"}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(t.TempDir(), "root.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_CA_CERTIFICATE", caPath)
	var mu sync.Mutex
	var cookies []string
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cookies = append(cookies, r.Header.Get("Cookie"))
		mu.Unlock()
		switch r.URL.Path {
		case "/backend-api/set":
			w.Header().Add("Set-Cookie", "__oailb=route-value; Path=/backend-api; Secure; Max-Age=3600")
			w.Header().Add("Set-Cookie", "__cf_bm=infrastructure; Path=/backend-api; Secure; Max-Age=3600")
			w.Header().Add("Set-Cookie", "session=never-store; Path=/; Secure")
			w.Header().Add("Set-Cookie", "access_token=never-store; Path=/; Secure")
		case "/backend-api/expire":
			w.Header().Add("Set-Cookie", "__oailb=; Path=/backend-api; Secure; Max-Age=0")
			w.Header().Add("Set-Cookie", "__cf_bm=; Path=/backend-api; Secure; Max-Age=0")
		}
		_, _ = io.WriteString(w, "ok")
	}))
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der, root}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	t.Cleanup(upstream.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "chatgpt.com:443" && r.Host != "example.invalid:443" {
			w.WriteHeader(403)
			return
		}
		remote, err := (&net.Dialer{}).DialContext(r.Context(), "tcp", upstream.Listener.Addr().String())
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer remote.Close()
		local, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer local.Close()
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffered.Flush()
		done := make(chan struct{})
		go func() { _, _ = io.Copy(remote, buffered); _ = remote.Close(); close(done) }()
		_, _ = io.Copy(local, remote)
		_ = local.Close()
		<-done
	}))
	t.Cleanup(proxy.Close)
	p := newTestProxy(t, 1)
	realGatewayNativePool(t, p)
	entry := poolProxyEntry(t, p, proxy.URL)
	client := p.clientFor(&entry)
	for _, target := range []string{"https://chatgpt.com/backend-api/set", "https://chatgpt.com/backend-api/check", "https://chatgpt.com/outside", "https://example.invalid/backend-api/check", "https://chatgpt.com/backend-api/expire", "https://chatgpt.com/backend-api/check"} {
		req, _ := http.NewRequestWithContext(context.Background(), "GET", target, nil)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal("synthetic native cookie request failed:", err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(cookies) != 6 || cookies[0] != "" || !strings.Contains(cookies[1], "__oailb=route-value") || !strings.Contains(cookies[1], "__cf_bm=infrastructure") || cookies[2] != "" || cookies[3] != "" || cookies[5] != "" {
		t.Fatal("official infrastructure cookie scope/expiry not preserved")
	}
	for _, value := range cookies {
		if strings.Contains(value, "never-store") {
			t.Fatal("authentication cookie persisted by native transport")
		}
	}
}
