package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAudit15BridgeAuthorityAndOriginValidation(t *testing.T) {
	for _, tc := range []struct {
		authority, scheme, want string
	}{
		{"EXAMPLE.invalid", "https", "example.invalid:443"},
		{"example.invalid:00080", "http", "example.invalid:80"},
		{"[2001:0db8:0:0::1]", "https", "[2001:db8::1]:443"},
		{"[::1]:8080", "http", "[::1]:8080"},
	} {
		got, err := gatewayNativeSOCKSAuthority(tc.authority, tc.scheme)
		if err != nil || got != tc.want {
			t.Fatalf("authority %q = %q, %v", tc.authority, got, err)
		}
	}
	for _, authority := range []string{
		"", "host:", "host:0", "host:65536", "host:+443", "host:443?", "host:443#", "host:443/path", "user:pass@host:443",
		"[fe80::1%25eth0]:443", "[not-an-ip]:443", "[127.0.0.1]:443", "host\\path", "host\tname", "host\x00name", "2001:db8::1", "2001:db8::1:443",
	} {
		if got, err := gatewayNativeSOCKSAuthority(authority, "https"); err == nil {
			t.Errorf("invalid authority %q accepted as %q", authority, got)
		}
	}
	if _, err := gatewayNativeSOCKSAuthority("host", "ftp"); err == nil {
		t.Fatal("unsupported scheme accepted")
	}
	proxy := ProxyEntry{Type: "socks5", Host: "127.0.0.1", Port: 1080}
	for _, origin := range []string{
		"https://host?", "https://host#", "https://host?q=x", "https://host#fragment", "https://user:pass@host", "https://host/path",
		"https://host:", "https://[fe80::1%25eth0]", "https://[not-an-ip]", "ftp://host", "https://host:65536",
	} {
		if bridge, err := newGatewayNativeSOCKSBridge(proxy, origin, time.Second); err == nil {
			_ = bridge.Close()
			t.Errorf("invalid origin accepted: %q", origin)
		}
	}
}

func TestAudit15BridgeRejectsAmbiguousRequestsBeforeDial(t *testing.T) {
	socks, proxy := nativeSOCKSTestServer(t, "", "", "")
	bridge := nativeSOCKSTestBridge(t, proxy, "https://remote.invalid")
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
		status int
	}{
		{"query-delimiter", func(r *http.Request) { r.RequestURI += "?" }, 403},
		{"fragment-delimiter", func(r *http.Request) { r.RequestURI += "#" }, 403},
		{"userinfo", func(r *http.Request) { r.RequestURI = "user:secret@remote.invalid:443" }, 403},
		{"host-mismatch", func(r *http.Request) { r.Host = "different.invalid:443" }, 403},
		{"body", func(r *http.Request) { r.ContentLength = 1 }, 403},
		{"chunked", func(r *http.Request) { r.TransferEncoding = []string{"chunked"} }, 403},
		{"duplicate-auth", func(r *http.Request) { r.Header.Add("Proxy-Authorization", bridge.auth) }, 407},
		{"invalid-auth", func(r *http.Request) { r.Header.Set("Proxy-Authorization", "Basic wrong") }, 407},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodConnect, "http://remote.invalid:443", nil)
			r.RequestURI, r.Host = "remote.invalid:443", "remote.invalid:443"
			r.Header.Set("Proxy-Authorization", bridge.auth)
			tc.change(r)
			w := httptest.NewRecorder()
			bridge.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d", w.Code, tc.status)
			}
		})
	}
	if socks.dials.Load() != 0 {
		t.Fatal("invalid CONNECT request reached SOCKS proxy")
	}
}

func TestAudit15BridgeRejectsHTTPRouteEscapeBeforeDial(t *testing.T) {
	socks, proxy := nativeSOCKSTestServer(t, "", "", "")
	bridge := nativeSOCKSTestBridge(t, proxy, "http://remote.invalid")
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
	}{
		{"host", func(r *http.Request) { r.Host = "another.invalid" }},
		{"url-host", func(r *http.Request) { r.URL.Host = "another.invalid" }},
		{"scheme", func(r *http.Request) { r.URL.Scheme = "https" }},
		{"userinfo", func(r *http.Request) { r.URL.User = url.UserPassword("user", "secret") }},
		{"fragment", func(r *http.Request) { r.URL.Fragment = "fragment" }},
		{"upgrade", func(r *http.Request) { r.Header.Set("Upgrade", "websocket") }},
		{"trace", func(r *http.Request) { r.Method = http.MethodTrace }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://remote.invalid/responses", nil)
			r.Header.Set("Proxy-Authorization", bridge.auth)
			tc.change(r)
			w := httptest.NewRecorder()
			bridge.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("route escape status = %d", w.Code)
			}
		})
	}
	if socks.dials.Load() != 0 {
		t.Fatal("HTTP route escape reached SOCKS proxy")
	}
}

func TestAudit15BridgePreservesRawQueryChunkedEntityAndTrailers(t *testing.T) {
	const rawQuery = "a=one;two&bad=%zz&dup=first&dup=second"
	const payload = "chunked\x00payload"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != payload || r.RequestURI != "/responses?"+rawQuery || r.Trailer.Get("X-Request-Trailer") != "request-end" {
			t.Errorf("request was rewritten: URI=%q body=%q trailer=%v err=%v", r.RequestURI, body, r.Trailer, err)
		}
		if r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("X-Hop") != "" || r.Header.Get("X-Forwarded-For") != "" || len(r.Header.Values("X-Repeated")) != 2 ||
			r.Trailer.Get("Proxy-Authorization") != "" || r.Trailer.Get("X-Hop") != "" || r.Trailer.Get("X-Forwarded-For") != "" {
			t.Errorf("hop header leaked or end-to-end headers changed: %v", r.Header)
		}
		w.Header().Set("Trailer", "X-Response-Trailer")
		w.Header().Add("X-Repeated", "first")
		w.Header().Add("X-Repeated", "second")
		_, _ = io.WriteString(w, payload)
		w.Header().Set("X-Response-Trailer", "response-end")
	}))
	defer origin.Close()
	upstream, _ := url.Parse(origin.URL)
	socks, proxy := nativeSOCKSTestServer(t, upstream.Host, "", "")
	bridge := nativeSOCKSTestBridge(t, proxy, "http://remote.invalid")
	endpoint, _ := url.Parse(bridge.ProxyURL())
	transport := &http.Transport{Proxy: http.ProxyURL(endpoint), DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	request, _ := http.NewRequestWithContext(t.Context(), "POST", "http://remote.invalid/responses?"+rawQuery, io.MultiReader(strings.NewReader(payload)))
	request.Header.Set("Connection", "X-Hop")
	request.Header.Set("X-Hop", "remove-me")
	request.Header["X-Repeated"] = []string{"first", "second"}
	request.Trailer = http.Header{"X-Request-Trailer": {"request-end"}, "Proxy-Authorization": {"Basic secret"}, "X-Hop": {"remove-me"}, "X-Forwarded-For": {"spoofed"}}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != payload || response.Trailer.Get("X-Response-Trailer") != "response-end" || len(response.Header.Values("X-Repeated")) != 2 {
		t.Fatalf("response entity/headers/trailers changed: body=%q trailer=%v err=%v", body, response.Trailer, err)
	}
	if socks.dials.Load() != 1 {
		t.Fatal("HTTP entity was replayed")
	}
}

func TestAudit15BridgeTunnelPreservesBufferedBytes(t *testing.T) {
	_, proxy := nativeSOCKSTestServer(t, "", "", "")
	bridge := nativeSOCKSTestBridge(t, proxy, "https://remote.invalid")
	conn, err := net.DialTimeout("tcp", bridge.listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	payload := "\x16\x03\x01\x00\x04early-TLS-bytes"
	_, err = io.WriteString(conn, "CONNECT remote.invalid:443 HTTP/1.1\r\nHost: remote.invalid:443\r\nProxy-Authorization: "+bridge.auth+"\r\n\r\n"+payload)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("CONNECT failed: %v", err)
	}
	actual := make([]byte, len(payload))
	if _, err := io.ReadFull(reader, actual); err != nil || string(actual) != payload {
		t.Fatalf("buffered tunnel bytes lost: got=%q err=%v", actual, err)
	}
}

func TestAudit15BridgeTunnelSupportsBothHalfCloseDirections(t *testing.T) {
	for _, direction := range []string{"client", "upstream"} {
		t.Run(direction, func(t *testing.T) {
			proxy, serverDone := audit15SOCKSServer(t, func(conn net.Conn) error {
				if err := audit15AcceptSOCKS(conn); err != nil {
					return err
				}
				if direction == "upstream" {
					if _, err := io.WriteString(conn, "response"); err != nil {
						return err
					}
					if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
						return err
					}
				}
				body, err := io.ReadAll(conn)
				if err != nil || string(body) != "request" {
					return fmt.Errorf("request after half-close = %q, %v", body, err)
				}
				if direction == "client" {
					_, err = io.WriteString(conn, "response")
				}
				return err
			})
			bridge := nativeSOCKSTestBridge(t, proxy, "https://remote.invalid")
			conn, reader, response := nativeSOCKSTestConnect(t, bridge, "remote.invalid:443", bridge.auth)
			defer conn.Close()
			if response.StatusCode != 200 {
				t.Fatalf("CONNECT status = %d", response.StatusCode)
			}
			if direction == "upstream" {
				body, err := io.ReadAll(reader)
				if err != nil || string(body) != "response" {
					t.Fatalf("upstream half-close = %q, %v", body, err)
				}
			}
			if _, err := io.WriteString(conn, "request"); err != nil {
				t.Fatalf("request after peer half-close: %v", err)
			}
			if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if direction == "client" {
				body, err := io.ReadAll(reader)
				if err != nil || string(body) != "response" {
					t.Fatalf("response after client half-close = %q, %v", body, err)
				}
			}
			if err := <-serverDone; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAudit15BridgeConcurrentCloseInterruptsHalfClosedUpstream(t *testing.T) {
	upstreamEOF := make(chan struct{})
	proxy, done := audit15SOCKSServer(t, func(conn net.Conn) error {
		if err := audit15AcceptSOCKS(conn); err != nil {
			return err
		}
		_, err := io.Copy(io.Discard, conn)
		close(upstreamEOF)
		// Keep the reverse direction silent after receiving the client FIN.
		<-t.Context().Done()
		return err
	})
	_ = done // The test cleanup closes the held server connection.
	bridge := nativeSOCKSTestBridge(t, proxy, "https://remote.invalid")
	conn, _, response := nativeSOCKSTestConnect(t, bridge, "remote.invalid:443", bridge.auth)
	defer conn.Close()
	if response.StatusCode != 200 {
		t.Fatalf("CONNECT status = %d", response.StatusCode)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-upstreamEOF:
	case <-time.After(time.Second):
		t.Fatal("client FIN did not reach SOCKS peer")
	}
	var wg sync.WaitGroup
	closed := make(chan struct{})
	for range 8 {
		wg.Go(func() { _ = bridge.Close() })
	}
	go func() { wg.Wait(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("concurrent bridge Close left a blocked upstream reader")
	}
	bridge.mu.Lock()
	remaining := len(bridge.clients)
	bridge.mu.Unlock()
	if remaining != 0 {
		t.Fatal("bridge retained half-closed client")
	}
}

func TestAudit15BridgeHTTPBodyCancellationReleasesDial(t *testing.T) {
	started := make(chan struct{})
	proxy, done := audit15SOCKSServer(t, func(conn net.Conn) error {
		if err := advanceSOCKS5Handshake(conn, "greeting"); err != nil {
			return err
		}
		close(started)
		_, err := io.Copy(io.Discard, conn)
		return err
	})
	bridge := nativeSOCKSTestBridge(t, proxy, "http://remote.invalid")
	endpoint, _ := url.Parse(bridge.ProxyURL())
	transport := &http.Transport{Proxy: http.ProxyURL(endpoint)}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", "http://remote.invalid/responses", nil)
	result := make(chan error, 1)
	go func() {
		response, err := client.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("HTTP SOCKS dial did not start")
	}
	cancel()
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("HTTP cancellation did not return")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP cancellation left SOCKS handshake active")
	}
}
