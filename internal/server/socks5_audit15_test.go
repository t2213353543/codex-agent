package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Deliberate short writes are permitted by this adversarial net.Conn fixture;
// a real TCP connection is used separately to exercise negotiation and EOF.
type audit15SOCKSConn struct {
	net.Conn
	read       *bytes.Reader
	written    bytes.Buffer
	writeLimit int
	writeErr   error
}

func (c *audit15SOCKSConn) Read(p []byte) (int, error) { return c.read.Read(p) }
func (c *audit15SOCKSConn) Write(p []byte) (int, error) {
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	if c.writeLimit >= 0 && len(p) > c.writeLimit {
		p = p[:c.writeLimit]
	}
	return c.written.Write(p)
}

func TestAudit15SOCKSShortWritesAndSanitizedCauses(t *testing.T) {
	for _, stage := range []string{"greeting", "authentication", "connect"} {
		t.Run(stage, func(t *testing.T) {
			var reply, expected []byte
			var operation func(net.Conn) error
			d := socks5Dialer{username: "user", password: "secret"}
			switch stage {
			case "greeting":
				reply, expected = []byte{5, 0}, []byte{5, 2, 0, 2}
				operation = d.handshake
			case "authentication":
				reply = []byte{1, 0}
				expected = append([]byte{1, 4}, []byte("user")...)
				expected = append(expected, append([]byte{6}, []byte("secret")...)...)
				operation = d.authenticate
			case "connect":
				reply = []byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80}
				expected = []byte{5, 1, 0, 1, 127, 0, 0, 1, 1, 187}
				operation = func(c net.Conn) error { return d.connect(c, "127.0.0.1", 443) }
			}
			conn := &audit15SOCKSConn{read: bytes.NewReader(reply), writeLimit: 1}
			if err := operation(conn); err != nil || !bytes.Equal(conn.written.Bytes(), expected) {
				t.Fatalf("short write negotiation = %x, error %v", conn.written.Bytes(), err)
			}
			conn = &audit15SOCKSConn{read: bytes.NewReader(reply), writeLimit: 0}
			if err := operation(conn); !errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("zero-progress writer = %v", err)
			}
			cause := errors.New("sensitive-proxy-user:secret@proxy.invalid")
			conn = &audit15SOCKSConn{read: bytes.NewReader(reply), writeErr: cause}
			err := operation(conn)
			if !errors.Is(err, cause) || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "proxy.invalid") {
				t.Fatalf("error lost cause or exposed sensitive data: %v", err)
			}
		})
	}
}

func TestAudit15SOCKSTargetEncodingAndReplyConsumption(t *testing.T) {
	for _, tc := range []struct {
		host string
		atyp byte
		data []byte
	}{
		{"192.0.2.9", 1, []byte{192, 0, 2, 9}},
		{"2001:db8::9", 4, net.ParseIP("2001:db8::9").To16()},
		{"unresolved.invalid", 3, append([]byte{18}, []byte("unresolved.invalid")...)},
	} {
		t.Run(tc.host, func(t *testing.T) {
			for _, bound := range [][]byte{
				{1, 192, 0, 2, 1, 0, 80},
				append(append([]byte{4}, net.ParseIP("2001:db8::1").To16()...), 0, 80),
				{3, 1, 'x', 0, 80},
			} {
				reply := append(append([]byte{5, 0, 0}, bound...), []byte("application-data")...)
				conn := &audit15SOCKSConn{read: bytes.NewReader(reply), writeLimit: -1}
				if err := (socks5Dialer{}).connect(conn, tc.host, 65535); err != nil {
					t.Fatal(err)
				}
				expected := append(append([]byte{5, 1, 0, tc.atyp}, tc.data...), 255, 255)
				if !bytes.Equal(conn.written.Bytes(), expected) {
					t.Fatalf("target encoding = %x, want %x", conn.written.Bytes(), expected)
				}
				remaining, _ := io.ReadAll(conn)
				if string(remaining) != "application-data" {
					t.Fatalf("bound address consumed payload: %q", remaining)
				}
			}
		})
	}
}

func TestAudit15SOCKSRejectsInvalidInputBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		host string
		port int
	}{
		{"", 443}, {strings.Repeat("a", 256), 443}, {"fe80::1%eth0", 443},
		{"[::1]", 443}, {"host:443", 443}, {"user@host", 443}, {"host\x00suffix", 443},
		{"host\nname", 443}, {"host/path", 443}, {"host?query", 443}, {"host#fragment", 443},
		{"host", -1}, {"host", 0}, {"host", 65536},
	} {
		conn := &audit15SOCKSConn{read: bytes.NewReader(nil), writeLimit: -1}
		if err := (socks5Dialer{}).connect(conn, tc.host, tc.port); err == nil || conn.written.Len() != 0 {
			t.Fatalf("invalid host/port reached wire: host=%q port=%d", tc.host, tc.port)
		}
	}
	for _, address := range []string{"host:0", "host:-1", "host:65536", "host:+443", "user:secret@host", "[fe80::1%eth0]:443", ":443"} {
		_, err := (socks5Dialer{proxyAddr: "never-dial.invalid:1080"}).DialContext(t.Context(), "tcp", address)
		if err == nil || strings.Contains(err.Error(), "never-dial") || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "connection failed") {
			t.Fatalf("invalid address was not rejected before dial: %v", err)
		}
	}
	for _, d := range []socks5Dialer{
		{username: strings.Repeat("u", 256)}, {username: "u", password: strings.Repeat("p", 256)}, {password: "secret"},
	} {
		conn := &audit15SOCKSConn{read: bytes.NewReader([]byte{5, 0}), writeLimit: -1}
		if err := d.handshake(conn); err == nil || conn.written.Len() != 0 {
			t.Fatal("invalid credentials reached wire or silently negotiated no-auth")
		}
	}
}

// This fixture uses a real TCP peer and reports protocol errors to the test.
// Cleanup closes both a pending Accept and an accepted socket before joining.
func audit15SOCKSServer(t *testing.T, handle func(net.Conn) error) (ProxyEntry, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var accepted net.Conn
	closed := false
	result, done := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		mu.Lock()
		if closed {
			mu.Unlock()
			return
		}
		accepted = conn
		mu.Unlock()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		result <- handle(conn)
	}()
	t.Cleanup(func() {
		mu.Lock()
		closed = true
		if accepted != nil {
			_ = accepted.Close()
		}
		mu.Unlock()
		_ = listener.Close()
		<-done
	})
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	number, _ := strconv.Atoi(port)
	return ProxyEntry{Type: "socks5", Host: host, Port: number, Enabled: true}, result
}

func audit15ReadSOCKSConnect(conn net.Conn) (string, error) {
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return "", err
	}
	if header[0] != 5 || header[1] != 1 || header[2] != 0 {
		return "", fmt.Errorf("invalid CONNECT header %x", header)
	}
	length := 0
	switch header[3] {
	case 1:
		length = 4
	case 4:
		length = 16
	case 3:
		var size [1]byte
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return "", err
		}
		length = int(size[0])
	default:
		return "", fmt.Errorf("invalid ATYP %d", header[3])
	}
	address := make([]byte, length+2)
	if _, err := io.ReadFull(conn, address); err != nil {
		return "", err
	}
	host := string(address[:length])
	if header[3] != 3 {
		host = net.IP(address[:length]).String()
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(address[length:])))), nil
}

func audit15AcceptSOCKS(conn net.Conn) error {
	var greeting [3]byte
	if _, err := io.ReadFull(conn, greeting[:]); err != nil {
		return err
	}
	if greeting != [3]byte{5, 1, 0} {
		return fmt.Errorf("unexpected greeting %x", greeting)
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return err
	}
	if _, err := audit15ReadSOCKSConnect(conn); err != nil {
		return err
	}
	_, err := conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
	return err
}

func TestAudit15SOCKSRejectsMalformedPeerOnLoopback(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage string
		reply []byte
	}{
		{"greeting-version", "greeting", []byte{4, 0}},
		{"greeting-unsupported", "greeting", []byte{5, 1}},
		{"greeting-no-methods", "greeting", []byte{5, 255}},
		{"greeting-unoffered-auth", "greeting", []byte{5, 2}},
		{"greeting-truncated", "greeting", []byte{5}},
		{"auth-version", "authentication", []byte{5, 0}},
		{"auth-rejected", "authentication", []byte{1, 1}},
		{"auth-truncated", "authentication", []byte{1}},
		{"connect-version", "connect", []byte{4, 0, 0, 1, 0, 0, 0, 0, 0, 0}},
		{"connect-rsv", "connect", []byte{5, 0, 1, 1, 0, 0, 0, 0, 0, 0}},
		{"connect-atyp", "connect", []byte{5, 0, 0, 2}},
		{"connect-empty-domain", "connect", []byte{5, 0, 0, 3, 0, 0, 0}},
		{"connect-truncated-ip", "connect", []byte{5, 0, 0, 4, 0}},
		{"connect-truncated-domain", "connect", []byte{5, 0, 0, 3, 2, 'x'}},
		{"connect-refused", "connect", []byte{5, 5, 0, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy, done := audit15SOCKSServer(t, func(conn net.Conn) error {
				if err := advanceSOCKS5Handshake(conn, tc.stage); err != nil {
					return err
				}
				_, err := conn.Write(tc.reply)
				return err
			})
			d := socks5Dialer{proxyAddr: net.JoinHostPort(proxy.Host, strconv.Itoa(proxy.Port))}
			if tc.stage == "authentication" {
				d.username, d.password = "user", "secret"
			}
			conn, err := d.DialContext(t.Context(), "tcp", "127.0.0.1:443")
			if conn != nil {
				_ = conn.Close()
			}
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("malformed peer accepted or credential exposed: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAudit15SOCKSDeadlineAndCancellationBeforeDial(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := (socks5Dialer{proxyAddr: "never-dial.invalid:1080"}).DialContext(ctx, "tcp", "remote.invalid:443"); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled dial = %v", err)
	}
	for _, stage := range []string{"greeting", "authentication", "connect"} {
		t.Run(stage, func(t *testing.T) {
			proxy, done := audit15SOCKSServer(t, func(conn net.Conn) error {
				if err := advanceSOCKS5Handshake(conn, stage); err != nil {
					return err
				}
				_, err := io.Copy(io.Discard, conn)
				return err
			})
			d := socks5Dialer{proxyAddr: net.JoinHostPort(proxy.Host, strconv.Itoa(proxy.Port))}
			if stage == "authentication" {
				d.username, d.password = "user", "secret"
			}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:443")
			if conn != nil {
				_ = conn.Close()
			}
			var timeout net.Error
			if !errors.Is(err, context.DeadlineExceeded) && !(errors.As(err, &timeout) && timeout.Timeout()) {
				t.Fatalf("deadline did not retain timeout identity: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatalf("deadline did not close proxy socket: %v", err)
			}
		})
	}
}

func TestAudit15SOCKSEstablishedSocketClearsHandshakeDeadline(t *testing.T) {
	proxy, done := audit15SOCKSServer(t, func(conn net.Conn) error {
		if err := audit15AcceptSOCKS(conn); err != nil {
			return err
		}
		var payload [1]byte
		if _, err := io.ReadFull(conn, payload[:]); err != nil {
			return err
		}
		_, err := conn.Write(payload[:])
		return err
	})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	conn, err := (socks5Dialer{proxyAddr: net.JoinHostPort(proxy.Host, strconv.Itoa(proxy.Port))}).DialContext(ctx, "tcp", "remote.invalid:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	<-ctx.Done()
	if _, err := conn.Write([]byte{'x'}); err != nil {
		t.Fatalf("established socket retained dial deadline: %v", err)
	}
	var echoed [1]byte
	if _, err := io.ReadFull(conn, echoed[:]); err != nil || echoed[0] != 'x' {
		t.Fatalf("socket failed after dial deadline: %q, %v", echoed, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
