package server

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestSOCKS5CancellationInterruptsSilentHandshake(t *testing.T) {
	for _, stage := range []string{"greeting", "authentication", "connect"} {
		t.Run(stage, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ready := make(chan error, 1)
			serverDone := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					ready <- err
					serverDone <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				if err := advanceSOCKS5Handshake(conn, stage); err != nil {
					ready <- err
					serverDone <- err
					return
				}
				ready <- nil
				_, err = io.Copy(io.Discard, conn)
				serverDone <- err
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dialer := socks5Dialer{proxyAddr: listener.Addr().String()}
			if stage == "authentication" {
				dialer.username, dialer.password = "user", "password"
			}
			dialDone := make(chan error, 1)
			go func() {
				conn, err := dialer.DialContext(ctx, "tcp", "127.0.0.1:443")
				if conn != nil {
					_ = conn.Close()
				}
				dialDone <- err
			}()
			select {
			case err := <-ready:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("SOCKS handshake did not reach the silent stage")
			}
			cancel()
			select {
			case err := <-dialDone:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled handshake error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation left the SOCKS handshake running")
			}
			select {
			case err := <-serverDone:
				if err != nil {
					t.Fatalf("cancellation did not close the proxy socket cleanly: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation left the proxy connection open")
			}
		})
	}
}

// Stop after receiving the stage's request, leaving the client awaiting a reply.
func advanceSOCKS5Handshake(conn net.Conn, stage string) error {
	var greeting [2]byte
	if _, err := io.ReadFull(conn, greeting[:]); err != nil {
		return err
	}
	if _, err := io.CopyN(io.Discard, conn, int64(greeting[1])); err != nil {
		return err
	}
	if stage == "greeting" {
		return nil
	}
	method := byte(0)
	if stage == "authentication" {
		method = 2
	}
	if _, err := conn.Write([]byte{5, method}); err != nil {
		return err
	}
	if stage == "authentication" {
		var auth [2]byte
		if _, err := io.ReadFull(conn, auth[:]); err != nil {
			return err
		}
		if _, err := io.CopyN(io.Discard, conn, int64(auth[1])); err != nil {
			return err
		}
		if _, err := io.ReadFull(conn, auth[:1]); err != nil {
			return err
		}
		_, err := io.CopyN(io.Discard, conn, int64(auth[0]))
		return err
	}
	_, err := io.CopyN(io.Discard, conn, 10) // IPv4 CONNECT request.
	return err
}

func TestSOCKS5ConnectedSocketOutlivesDialContext(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if err := advanceSOCKS5Handshake(conn, "connect"); err != nil {
			serverDone <- err
			return
		}
		if _, err := conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80}); err != nil {
			serverDone <- err
			return
		}
		var data [1]byte
		if _, err := io.ReadFull(conn, data[:]); err != nil {
			serverDone <- err
			return
		}
		_, err = conn.Write(data[:])
		serverDone <- err
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, err := (socks5Dialer{proxyAddr: listener.Addr().String()}).DialContext(ctx, "tcp", "127.0.0.1:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cancel() // The dial context must no longer own an established connection.
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	var echoed [1]byte
	if _, err := io.ReadFull(conn, echoed[:]); err != nil || echoed[0] != 'x' {
		t.Fatalf("connected socket was closed by dial cancellation: value=%q err=%v", echoed, err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}
