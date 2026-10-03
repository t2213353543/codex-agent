package server

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// socks5Dialer implements a minimal SOCKS5 client (RFC 1928) with
// username/password authentication (RFC 1929) on top of the standard
// library, so the project keeps its zero-dependency build.
type socks5Dialer struct {
	proxyAddr string
	username  string
	password  string
}

func (d socks5Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("socks5 does not support network %q", network)
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("socks5 target address is invalid")
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, errors.New("socks5 target port is invalid")
	}
	if err := socks5ValidateTarget(host, int(port)); err != nil {
		return nil, err
	}
	if err := d.validateCredentials(); err != nil {
		return nil, err
	}
	// Bound TCP setup and the complete SOCKS negotiation even when streaming
	// callers deliberately have no overall HTTP request deadline.
	deadline := time.Now().Add(30 * time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	dialer := net.Dialer{Deadline: deadline, KeepAlive: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", d.proxyAddr)
	if err != nil {
		return nil, &socks5OperationError{"socks5 proxy connection failed", err}
	}
	success := false
	defer func() {
		if !success {
			_ = conn.Close()
		}
	}()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	// net.Dialer only observes cancellation while opening TCP. Continue to
	// observe it during greeting, authentication, and the CONNECT exchange.
	canceled := make(chan struct{})
	stopCancellation := context.AfterFunc(ctx, func() {
		_ = conn.Close()
		close(canceled)
	})
	err = d.handshake(conn)
	if err == nil {
		err = d.connect(conn, host, int(port))
	}
	if !stopCancellation() {
		<-canceled
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	success = true
	return conn, nil
}

func (d socks5Dialer) handshake(conn net.Conn) error {
	if err := d.validateCredentials(); err != nil {
		return err
	}
	methods := []byte{0x05, 0x01, 0x00}
	if d.username != "" {
		methods = []byte{0x05, 0x02, 0x00, 0x02}
	}
	if err := socks5Write(conn, methods); err != nil {
		return &socks5OperationError{"socks5 greeting failed", err}
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return &socks5OperationError{"socks5 greeting read failed", err}
	}
	if reply[0] != 0x05 {
		return errors.New("socks5 protocol mismatch")
	}
	switch reply[1] {
	case 0x00:
		return nil
	case 0x02:
		if d.username == "" {
			return errors.New("socks5 proxy requires authentication")
		}
		return d.authenticate(conn)
	case 0xFF:
		return errors.New("socks5 proxy rejected all authentication methods")
	default:
		return fmt.Errorf("socks5 proxy chose unsupported auth method 0x%02x", reply[1])
	}
}

func (d socks5Dialer) authenticate(conn net.Conn) error {
	if err := d.validateCredentials(); err != nil {
		return err
	}
	if d.username == "" {
		return errors.New("socks5 authentication requires a username")
	}
	request := []byte{0x01, byte(len(d.username))}
	request = append(request, d.username...)
	request = append(request, byte(len(d.password)))
	request = append(request, d.password...)
	if err := socks5Write(conn, request); err != nil {
		return &socks5OperationError{"socks5 authentication write failed", err}
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return &socks5OperationError{"socks5 authentication read failed", err}
	}
	if reply[0] != 0x01 || reply[1] != 0x00 {
		return errors.New("socks5 authentication rejected")
	}
	return nil
}

func (d socks5Dialer) connect(conn net.Conn, host string, port int) error {
	if err := socks5ValidateTarget(host, port); err != nil {
		return err
	}
	request := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ipv4 := ip.To4(); ipv4 != nil {
			request = append(request, 0x01)
			request = append(request, ipv4...)
		} else {
			request = append(request, 0x04)
			request = append(request, ip.To16()...)
		}
	} else {
		request = append(request, 0x03, byte(len(host)))
		request = append(request, host...)
	}
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], uint16(port))
	request = append(request, portBytes[:]...)
	if err := socks5Write(conn, request); err != nil {
		return &socks5OperationError{"socks5 connect write failed", err}
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return &socks5OperationError{"socks5 connect read failed", err}
	}
	if reply[0] != 0x05 || reply[2] != 0x00 {
		return errors.New("socks5 protocol mismatch")
	}
	if reply[1] != 0x00 {
		return fmt.Errorf("socks5 connect refused: %s", socks5ReplyCode(reply[1]))
	}
	var skip int
	switch reply[3] {
	case 0x01:
		skip = 4 + 2
	case 0x04:
		skip = 16 + 2
	case 0x03:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return &socks5OperationError{"socks5 connect read failed", err}
		}
		if length[0] == 0 {
			return errors.New("socks5 proxy returned an empty domain")
		}
		skip = int(length[0]) + 2
	default:
		return errors.New("socks5 proxy returned an unknown address type")
	}
	if _, err := io.CopyN(io.Discard, conn, int64(skip)); err != nil {
		return &socks5OperationError{"socks5 connect read failed", err}
	}
	return nil
}

func (d socks5Dialer) validateCredentials() error {
	if len(d.username) > 255 || len(d.password) > 255 {
		return errors.New("socks5 credentials are too long")
	}
	if d.username == "" && d.password != "" {
		return errors.New("socks5 password requires a username")
	}
	// Empty passwords remain supported, matching the pinned CLI SOCKS client.
	return nil
}

func socks5ValidateTarget(host string, port int) error {
	if port < 1 || port > 65535 {
		return errors.New("socks5 target port is invalid")
	}
	if host == "" || len(host) > 255 {
		return errors.New("socks5 target host is invalid")
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	// SOCKS has no representation for a scoped IPv6 address. Never turn a
	// malformed address or zone identifier into a remote DNS request.
	if strings.ContainsAny(host, ":%[]/@\\?#") || strings.ContainsFunc(host, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return errors.New("socks5 target host is invalid")
	}
	return nil
}

// Keep transport causes available to errors.Is/As without rendering endpoints
// or credentials supplied by callers in the externally visible error string.
type socks5OperationError struct {
	message string
	cause   error
}

func (e *socks5OperationError) Error() string { return e.message }
func (e *socks5OperationError) Unwrap() error { return e.cause }

func socks5Write(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func socks5ReplyCode(code byte) string {
	codes := map[byte]string{
		0x01: "general failure",
		0x02: "connection not allowed",
		0x03: "network unreachable",
		0x04: "host unreachable",
		0x05: "connection refused",
		0x06: "TTL expired",
		0x07: "command not supported",
		0x08: "address type not supported",
	}
	if text, ok := codes[code]; ok {
		return text
	}
	return fmt.Sprintf("code 0x%02x", code)
}
