package server

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type nativeSOCKSFixture struct {
	listener net.Listener
	requests chan string
	dials    atomic.Int32
	mu       sync.Mutex
	clients  map[net.Conn]struct{}
	closed   bool
	wg       sync.WaitGroup
	done     chan struct{}
}

func nativeSOCKSTestServer(t *testing.T, target, username, password string) (*nativeSOCKSFixture, ProxyEntry) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &nativeSOCKSFixture{listener: listener, requests: make(chan string, 256), clients: make(map[net.Conn]struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				_ = conn.Close()
				return
			}
			s.clients[conn] = struct{}{}
			s.wg.Add(1)
			s.mu.Unlock()
			s.dials.Add(1)
			go func() {
				defer s.wg.Done()
				defer func() { _ = conn.Close(); s.mu.Lock(); delete(s.clients, conn); s.mu.Unlock() }()
				var greeting [2]byte
				if _, err := io.ReadFull(conn, greeting[:]); err != nil || greeting[0] != 5 {
					return
				}
				methods := make([]byte, greeting[1])
				if _, err := io.ReadFull(conn, methods); err != nil {
					return
				}
				if username == "" {
					_, _ = conn.Write([]byte{5, 0})
				} else {
					_, _ = conn.Write([]byte{5, 2})
					if _, err := io.ReadFull(conn, greeting[:]); err != nil || greeting[0] != 1 {
						return
					}
					user := make([]byte, greeting[1])
					if _, err := io.ReadFull(conn, user); err != nil {
						return
					}
					var n [1]byte
					if _, err := io.ReadFull(conn, n[:]); err != nil {
						return
					}
					pass := make([]byte, n[0])
					if _, err := io.ReadFull(conn, pass); err != nil {
						return
					}
					if string(user) != username || string(pass) != password {
						_, _ = conn.Write([]byte{1, 1})
						return
					}
					_, _ = conn.Write([]byte{1, 0})
				}
				var request [4]byte
				if _, err := io.ReadFull(conn, request[:]); err != nil || request[0] != 5 || request[1] != 1 {
					return
				}
				var host string
				switch request[3] {
				case 1, 4:
					length := 4
					if request[3] == 4 {
						length = 16
					}
					ip := make([]byte, length)
					if _, err := io.ReadFull(conn, ip); err != nil {
						return
					}
					host = net.IP(ip).String()
				case 3:
					var length [1]byte
					if _, err := io.ReadFull(conn, length[:]); err != nil {
						return
					}
					name := make([]byte, length[0])
					if _, err := io.ReadFull(conn, name); err != nil {
						return
					}
					host = string(name)
				default:
					return
				}
				var port [2]byte
				if _, err := io.ReadFull(conn, port[:]); err != nil {
					return
				}
				s.requests <- strconv.Itoa(int(request[3])) + ":" + net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:]))))
				var upstream net.Conn
				if target != "" {
					var dialErr error
					upstream, dialErr = net.DialTimeout("tcp", target, time.Second)
					if dialErr != nil {
						_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
						return
					}
					defer upstream.Close()
				}
				_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				if upstream == nil {
					_, _ = io.Copy(conn, conn)
					return
				}
				finished := make(chan struct{})
				go func() { _, _ = io.Copy(upstream, conn); _ = upstream.Close(); close(finished) }()
				_, _ = io.Copy(conn, upstream)
				_ = conn.Close()
				<-finished
			}()
		}
	}()
	t.Cleanup(func() {
		s.mu.Lock()
		s.closed = true
		for conn := range s.clients {
			_ = conn.Close()
		}
		s.mu.Unlock()
		_ = listener.Close()
		<-s.done
		s.wg.Wait()
	})
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	number, _ := strconv.Atoi(port)
	return s, ProxyEntry{ID: "synthetic-socks", Type: "socks5", Host: host, Port: number, Username: username, Password: password, Version: 1, Enabled: true}
}

func nativeSOCKSTestBridge(t *testing.T, proxy ProxyEntry, origin string) *gatewayNativeSOCKSBridge {
	t.Helper()
	bridge, err := newGatewayNativeSOCKSBridge(proxy, origin, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	return bridge
}

func nativeSOCKSTestConnect(t *testing.T, bridge *gatewayNativeSOCKSBridge, target, auth string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", bridge.listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	request := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if auth != "" {
		request += "Proxy-Authorization: " + auth + "\r\n"
	}
	_, err = io.WriteString(conn, request+"\r\n")
	if err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: "CONNECT"})
	if err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	return conn, reader, response
}

func TestGatewayNativeSOCKSBridgeTunnelAuthTargetAndRemoteDNS(t *testing.T) {
	socks, proxy := nativeSOCKSTestServer(t, "", "fixture-user", "fixture-pass")
	bridge := nativeSOCKSTestBridge(t, proxy, "https://Unresolved-Remote.invalid")
	for _, scenario := range []struct {
		target, auth string
		status       int
	}{
		{"unresolved-remote.invalid:443", "", 407},
		{"unresolved-remote.invalid:443", "Basic wrong", 407},
		{"different.invalid:443", bridge.auth, 403},
		{"unresolved-remote.invalid:444", bridge.auth, 403},
	} {
		conn, _, response := nativeSOCKSTestConnect(t, bridge, scenario.target, scenario.auth)
		_ = response.Body.Close()
		_ = conn.Close()
		if response.StatusCode != scenario.status {
			t.Fatalf("unauthorized target status=%d", response.StatusCode)
		}
	}
	if socks.dials.Load() != 0 {
		t.Fatal("rejected bridge request reached SOCKS")
	}
	conn, reader, response := nativeSOCKSTestConnect(t, bridge, "UNRESOLVED-REMOTE.INVALID:443", bridge.auth)
	defer conn.Close()
	if response.StatusCode != 200 {
		t.Fatalf("CONNECT status=%d", response.StatusCode)
	}
	payload := []byte{22, 3, 1, 0, 5, 0, 0xff, 0x80, 10, 13}
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	actual := make([]byte, len(payload))
	if _, err := io.ReadFull(reader, actual); err != nil || string(actual) != string(payload) {
		t.Fatal("raw tunnel changed payload")
	}
	if target := <-socks.requests; target != "3:unresolved-remote.invalid:443" {
		t.Fatalf("SOCKS did not receive remote domain: %s", target)
	}
}

func TestGatewayNativeSOCKSBridgeHTTPPreservesEntityAndHeaders(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if string(body) != "raw\x00entity" || r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("X-Forwarded-For") != "" || len(r.Header.Values("X-Repeated")) != 2 || r.RequestURI != "/echo?q=a%20b" {
			t.Error("HTTP bridge changed entity/end-to-end headers or leaked proxy metadata")
		}
		w.Header().Add("Set-Cookie", "a=1")
		w.Header().Add("Set-Cookie", "b=2")
		w.WriteHeader(418)
		_, _ = w.Write(body)
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	socks, proxy := nativeSOCKSTestServer(t, target.Host, "", "")
	bridge := nativeSOCKSTestBridge(t, proxy, "http://remote-only.invalid")
	endpoint, _ := url.Parse(bridge.ProxyURL())
	transport := &http.Transport{Proxy: http.ProxyURL(endpoint)}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request, _ := http.NewRequestWithContext(t.Context(), "POST", "http://remote-only.invalid/echo?q=a%20b", strings.NewReader("raw\x00entity"))
	request.Header["X-Repeated"] = []string{"first", "second"}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != 418 || string(body) != "raw\x00entity" || len(response.Header.Values("Set-Cookie")) != 2 || calls.Load() != 1 {
		t.Fatal("HTTP bridge response changed or replayed")
	}
	if target := <-socks.requests; target != "3:remote-only.invalid:80" {
		t.Fatal("HTTP destination resolved locally")
	}
}

func TestGatewayNativeSOCKSBridgeFailureNeverFallsBackOrReturns401(t *testing.T) {
	socks, proxy := nativeSOCKSTestServer(t, "", "fixture-user", "correct")
	proxy.Password = "wrong"
	bridge := nativeSOCKSTestBridge(t, proxy, "https://127.0.0.1:443")
	conn, _, response := nativeSOCKSTestConnect(t, bridge, "127.0.0.1:443", bridge.auth)
	defer conn.Close()
	defer response.Body.Close()
	if response.StatusCode != 502 || socks.dials.Load() != 1 {
		t.Fatalf("SOCKS auth failure status=%d attempts=%d", response.StatusCode, socks.dials.Load())
	}
	select {
	case <-socks.requests:
		t.Fatal("failed SOCKS authentication reached destination")
	default:
	}
}

func TestGatewayNativeSOCKSBridgeCapacityRefillAndClose(t *testing.T) {
	const concurrent = 96
	socks, proxy := nativeSOCKSTestServer(t, "", "", "")
	bridge := nativeSOCKSTestBridge(t, proxy, "https://remote.invalid")
	var clients []net.Conn
	for round := range 2 {
		for range concurrent {
			conn, _, response := nativeSOCKSTestConnect(t, bridge, "remote.invalid:443", bridge.auth)
			if response.StatusCode != 200 {
				t.Fatal("bridge rejected within-capacity CONNECT")
			}
			clients = append(clients, conn)
		}
		if round == 0 {
			for _, conn := range clients {
				_ = conn.Close()
			}
			clients = nil
		}
	}
	if socks.dials.Load() != 2*concurrent {
		t.Fatal("close/refill dropped or replayed a tunnel")
	}
	closed := make(chan struct{})
	go func() { _ = bridge.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("bridge Close left active tunnels")
	}
	for _, conn := range clients {
		var byte [1]byte
		if _, err := conn.Read(byte[:]); err == nil {
			t.Fatal("bridge Close left usable connection")
		}
		_ = conn.Close()
	}
	bridge.mu.Lock()
	remaining := len(bridge.clients)
	bridge.mu.Unlock()
	if remaining != 0 {
		t.Fatal("bridge Close leaked connections")
	}
}

func TestGatewayNativeSOCKSBridgeShutdownCancelsDial(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	number, _ := strconv.Atoi(port)
	bridge := nativeSOCKSTestBridge(t, ProxyEntry{Type: "socks5", Host: host, Port: number}, "https://remote.invalid")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", bridge.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "CONNECT remote.invalid:443 HTTP/1.1\r\nHost: remote.invalid:443\r\nProxy-Authorization: "+bridge.auth+"\r\n\r\n")
	var upstream net.Conn
	select {
	case upstream = <-accepted:
	case <-ctx.Done():
		t.Fatal("SOCKS dial did not start")
	}
	defer upstream.Close()
	started := time.Now()
	_ = bridge.Close()
	if time.Since(started) > time.Second {
		t.Fatal("bridge shutdown waited for SOCKS handshake timeout")
	}
}

func TestGatewayNativeSOCKSBridgeFollowsHelperLifecycle(t *testing.T) {
	p := nativeHTTPFixturePool(t, nil)
	proxy := &ProxyEntry{ID: "synthetic-socks", Type: "socks5", Host: "127.0.0.1", Port: 1234, Version: 1, Enabled: true}
	response := nativeHTTPFixtureRequest(t, p.RoundTripper(proxy, time.Second), t.Context(), "GET", "https://fixture.invalid/echo", "")
	_, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	p.mu.Lock()
	var child *gatewayNativeProcess
	for _, entry := range p.entries {
		child = entry
	}
	p.mu.Unlock()
	if child.bridge == nil {
		t.Fatal("SOCKS helper did not own its bridge")
	}
	address := child.bridge.listener.Addr().String()
	_ = p.Close()
	if conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("helper shutdown left its SOCKS bridge listening")
	}
	select {
	case <-child.bridge.done:
	default:
		t.Fatal("helper shutdown returned before bridge stopped")
	}
}

func TestGatewayNativeSOCKSBridgePlainHTTPOriginAllowsFixedCONNECT(t *testing.T) {
	socks, proxy := nativeSOCKSTestServer(t, "", "", "")
	bridge := nativeSOCKSTestBridge(t, proxy, "http://remote.invalid")
	conn, reader, response := nativeSOCKSTestConnect(t, bridge, "REMOTE.INVALID:80", bridge.auth)
	defer conn.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("plaintext WS CONNECT rejected: %d", response.StatusCode)
	}
	payload := "GET /responses HTTP/1.1\r\nUpgrade: websocket\r\n\r\n"
	_, _ = io.WriteString(conn, payload)
	actual := make([]byte, len(payload))
	if _, err := io.ReadFull(reader, actual); err != nil || string(actual) != payload {
		t.Fatal("plaintext WS tunnel changed message bytes")
	}
	if target := <-socks.requests; target != "3:remote.invalid:80" {
		t.Fatal("plaintext WS CONNECT escaped its fixed authority")
	}
	rejected, _, forbidden := nativeSOCKSTestConnect(t, bridge, "remote.invalid:443", bridge.auth)
	_ = forbidden.Body.Close()
	_ = rejected.Close()
	if forbidden.StatusCode != http.StatusForbidden || socks.dials.Load() != 1 {
		t.Fatal("plaintext WS CONNECT accepted an unrelated port")
	}
}
