package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

func transportStateWait(t *testing.T, done <-chan struct{}, boundary string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not stop", boundary)
	}
}

func transportStateBridgeDrained(t *testing.T, bridge *gatewayNativeSOCKSBridge) {
	t.Helper()
	done := make(chan struct{})
	go func() { bridge.handlers.Wait(); close(done) }()
	transportStateWait(t, done, "bridge tunnel handler")
	// Before Hijack, net/http owns connection cleanup after ServeHTTP returns.
	// Joining the bridge handler alone is therefore not its closed-connection
	// barrier; wait for the tracked server wrapper to actually leave the map.
	deadline := time.Now().Add(2 * time.Second)
	for {
		bridge.mu.Lock()
		count := len(bridge.clients)
		bridge.mu.Unlock()
		if count == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("bridge retained %d connection wrappers", count)
		}
		time.Sleep(time.Millisecond)
	}
}

// A complete CONNECT is followed by an actual standard-library ClientHello.
// The target stalls before ServerHello; cancellation/deadline must close the
// existing tunnel without waiting for the bridge's long-lived owner to close.
func TestTransportStateSOCKSSlowTLSHandshakeReleasesTunnel(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint("deadline=", deadline), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			helloSeen, targetDone := make(chan struct{}), make(chan struct{})
			targetErrors := make(chan error, 1)
			go func() {
				defer close(targetDone)
				conn, e := listener.Accept()
				if e != nil {
					targetErrors <- e
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				var head [5]byte
				if _, e = io.ReadFull(conn, head[:]); e != nil {
					targetErrors <- e
					return
				}
				if head[0] != 22 {
					targetErrors <- errors.New("target did not receive TLS handshake record")
					return
				}
				if _, e = io.CopyN(io.Discard, conn, int64(binary.BigEndian.Uint16(head[3:]))); e != nil {
					targetErrors <- e
					return
				}
				close(helloSeen)
				_, e = io.Copy(io.Discard, conn)
				targetErrors <- e
			}()
			socks, proxy := nativeSOCKSTestServer(t, listener.Addr().String(), "", "")
			bridge := nativeSOCKSTestBridge(t, proxy, "https://localhost")
			conn, _, response := nativeSOCKSTestConnect(t, bridge, "localhost:443", bridge.auth)
			if response.StatusCode != 200 {
				t.Fatal("CONNECT failed")
			}
			defer conn.Close()
			ctx, cancel := context.WithCancel(t.Context())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), 150*time.Millisecond)
			}
			defer cancel()
			tlsClient := tls.Client(conn, &tls.Config{ServerName: "localhost", MinVersion: tls.VersionTLS12})
			result := make(chan error, 1)
			go func() { result <- tlsClient.HandshakeContext(ctx) }()
			transportStateWait(t, helloSeen, "actual ClientHello")
			if !deadline {
				cancel()
			}
			select {
			case e := <-result:
				if !errors.Is(e, ctx.Err()) || ctx.Err() == nil {
					t.Fatalf("wrong handshake cancellation: %v", e)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("TLS handshake ignored cancellation")
			}
			transportStateWait(t, targetDone, "TLS target")
			if e := <-targetErrors; e != nil {
				t.Fatalf("target saw error rather than client closure: %v", e)
			}
			transportStateBridgeDrained(t, bridge)
			done := make(chan struct{})
			go func() { socks.wg.Wait(); close(done) }()
			transportStateWait(t, done, "SOCKS proxy handler")
			if socks.dials.Load() != 1 {
				t.Fatal("TLS failure replayed CONNECT")
			}
			if target := <-socks.requests; target != "3:localhost:443" {
				t.Fatal("TLS route changed domain authority")
			}
		})
	}
}

// Caller disappearance while CONNECT is still negotiating must propagate
// through net/http request cancellation, not only a later global bridge.Close.
func TestTransportStateSOCKSConnectCallerGoneCancelsNegotiation(t *testing.T) {
	for _, stage := range []string{"greeting", "authentication", "connect"} {
		t.Run(stage, func(t *testing.T) {
			ready := make(chan struct{})
			proxy, done := audit15SOCKSServer(t, func(conn net.Conn) error {
				if err := advanceSOCKS5Handshake(conn, stage); err != nil {
					return err
				}
				close(ready)
				_, err := io.Copy(io.Discard, conn)
				return err
			})
			if stage == "authentication" {
				proxy.Username, proxy.Password = "synthetic-user", "synthetic-pass"
			}
			bridge := nativeSOCKSTestBridge(t, proxy, "https://127.0.0.1")
			client, err := net.DialTimeout("tcp", bridge.listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			_, err = io.WriteString(client, "CONNECT 127.0.0.1:443 HTTP/1.1\r\nHost: 127.0.0.1:443\r\nProxy-Authorization: "+bridge.auth+"\r\n\r\n")
			if err != nil {
				t.Fatal(err)
			}
			transportStateWait(t, ready, "SOCKS negotiation stage")
			client.Close()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("caller disconnect left SOCKS negotiation live")
			}
			transportStateBridgeDrained(t, bridge)
		})
	}
}

// Complete, authenticated local TLS verifies the same bridge can still carry
// ordinary bidirectional traffic and close cleanly after a graceful TLS close.
func TestTransportStateSOCKSTLSSuccessAndPeerClose(t *testing.T) {
	cert, _, caPath := nativeCaptureCertificate(t)
	ca, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("fixture root unavailable")
	}
	for _, peerCloses := range []bool{false, true} {
		t.Run(fmt.Sprint("peer_closes=", peerCloses), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				conn, e := listener.Accept()
				if e != nil {
					done <- e
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				target := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
				if e = target.Handshake(); e != nil {
					done <- e
					return
				}
				request := make([]byte, 7)
				if _, e = io.ReadFull(target, request); e != nil {
					done <- e
					return
				}
				if string(request) != "request" {
					done <- errors.New("TLS payload changed")
					return
				}
				if _, e = target.Write([]byte("response")); e != nil {
					done <- e
					return
				}
				if !peerCloses {
					_, e = io.Copy(io.Discard, target)
				}
				target.Close()
				done <- e
			}()
			socks, proxy := nativeSOCKSTestServer(t, listener.Addr().String(), "", "")
			bridge := nativeSOCKSTestBridge(t, proxy, "https://localhost")
			conn, _, response := nativeSOCKSTestConnect(t, bridge, "localhost:443", bridge.auth)
			defer conn.Close()
			if response.StatusCode != 200 {
				t.Fatal("CONNECT failed")
			}
			client := tls.Client(conn, &tls.Config{ServerName: "localhost", RootCAs: roots, MinVersion: tls.VersionTLS12})
			if err = client.HandshakeContext(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err = client.Write([]byte("request")); err != nil {
				t.Fatal(err)
			}
			actual := make([]byte, 8)
			if _, err = io.ReadFull(client, actual); err != nil || string(actual) != "response" {
				t.Fatalf("TLS response changed: %v", err)
			}
			if peerCloses {
				if _, err = client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
					t.Fatalf("peer TLS close not forwarded: %v", err)
				}
			}
			client.Close()
			select {
			case err = <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("TLS peer retained connection")
			}
			transportStateBridgeDrained(t, bridge)
			if socks.dials.Load() != 1 {
				t.Fatal("normal tunnel replayed")
			}
		})
	}
}

func TestTransportStateSOCKSFixedIPv6AuthorityOnWire(t *testing.T) {
	for _, target := range []string{"[2001:0db8:0:0::9]:443", "[2001:db8::9]:443"} {
		t.Run(target, func(t *testing.T) {
			socks, proxy := nativeSOCKSTestServer(t, "", "", "")
			bridge := nativeSOCKSTestBridge(t, proxy, "https://[2001:db8::9]")
			conn, reader, response := nativeSOCKSTestConnect(t, bridge, target, bridge.auth)
			defer conn.Close()
			if response.StatusCode != 200 {
				t.Fatal("equivalent legal IPv6 authority rejected")
			}
			_, _ = conn.Write([]byte("x"))
			var one [1]byte
			if _, err := io.ReadFull(reader, one[:]); err != nil || one[0] != 'x' {
				t.Fatal("IPv6 tunnel changed byte")
			}
			if target := <-socks.requests; target != "4:[2001:db8::9]:443" {
				t.Fatalf("IPv6 converted to remote domain: %s", target)
			}
			conn.Close()
			transportStateBridgeDrained(t, bridge)
		})
	}
	// Valid HTTP authority on another host is rejected rather than used as a
	// redirect/dynamic destination, even when all bridge credentials are valid.
	socks, proxy := nativeSOCKSTestServer(t, "", "", "")
	bridge := nativeSOCKSTestBridge(t, proxy, "https://[2001:db8::9]")
	conn, err := net.DialTimeout("tcp", bridge.listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "CONNECT [2001:db8::10]:443 HTTP/1.1\r\nHost: [2001:db8::10]:443\r\nProxy-Authorization: "+bridge.auth+"\r\nConnection: close\r\n\r\n")
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	conn.Close()
	if response.StatusCode != 403 || socks.dials.Load() != 0 {
		t.Fatal("valid foreign IPv6 authority escaped fixed route")
	}
	transportStateBridgeDrained(t, bridge)
}
