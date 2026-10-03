package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type lifetimeRequestBody struct {
	reader io.Reader
	closes atomic.Int32
}

func (b *lifetimeRequestBody) Read(p []byte) (int, error) { return b.reader.Read(p) }
func (b *lifetimeRequestBody) Close() error               { b.closes.Add(1); return nil }

type lifetimeFailRead struct{}

func (lifetimeFailRead) Read([]byte) (int, error) { return 0, io.ErrClosedPipe }

// Request resources belong to the RoundTripper even when Client.Do wraps it.
// The synthetic peer is IPC only; these tests do not start Rust or an upstream.
func TestNativeRequestLifetimeEveryReturnClosesBody(t *testing.T) {
	for _, scenario := range []string{"invalid_url", "cancel_before_read", "read_error", "length_mismatch", "pool_closed", "valid_headers", "bad_headers", "peer_eof", "typed_error"} {
		t.Run(scenario, func(t *testing.T) {
			body := &lifetimeRequestBody{reader: strings.NewReader("body")}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "POST", "http://audit.invalid/", body)
			rt := &gatewayNativeRoundTripper{}
			switch scenario {
			case "invalid_url":
				req.URL = nil
			case "cancel_before_read":
				cancel()
			case "read_error":
				body.reader = lifetimeFailRead{}
			case "length_mismatch":
				req.ContentLength = 5
			case "pool_closed":
				rt.pool = &gatewayNativePool{closed: true}
			default:
				rt, _ = nativeAuditHTTPPeer(t, func(c net.Conn, _ map[string]json.RawMessage) {
					switch scenario {
					case "valid_headers":
						nativeAuditResponse(c, 200, nil, "reply", true)
					case "bad_headers":
						nativeAuditFrame(c, 1, []byte(`{"status":200,"version":"invalid","headers":[]}`))
					case "typed_error":
						nativeAuditFrame(c, 4, []byte("headers_timeout"))
					}
				})
			}
			res, err := rt.RoundTrip(req)
			if (scenario == "valid_headers") != (err == nil) {
				t.Fatalf("unexpected outcome %v", err)
			}
			if body.closes.Load() != 1 {
				t.Fatalf("request Close=%d", body.closes.Load())
			}
			if res != nil {
				_, _ = io.Copy(io.Discard, res.Body)
				_ = res.Body.Close()
			}
			cancel()
			if body.closes.Load() != 1 {
				t.Fatal("late cancellation closed request twice")
			}
		})
	}
}

func TestNativeRequestLifetimeCancellationAndResponseReleaseRace(t *testing.T) {
	for i := 0; i < 30; i++ {
		body := &lifetimeRequestBody{reader: strings.NewReader("body")}
		waiting := make(chan struct{})
		rt, child := nativeAuditHTTPPeer(t, func(c net.Conn, _ map[string]json.RawMessage) {
			nativeAuditResponse(c, 200, nil, "prefix", false)
			close(waiting)
			_, _ = io.Copy(io.Discard, c)
		})
		ctx, cancel := context.WithCancel(t.Context())
		req, _ := http.NewRequestWithContext(ctx, "POST", "http://audit.invalid/", body)
		res, err := (&http.Client{Transport: rt}).Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		<-waiting
		var group sync.WaitGroup
		group.Go(cancel)
		for j := 0; j < 5; j++ {
			group.Go(func() { _ = res.Body.Close() })
		}
		group.Wait()
		if body.closes.Load() != 1 {
			t.Fatalf("request closed %d times", body.closes.Load())
		}
		if n, err := res.Body.Read(make([]byte, 1)); n != 0 || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Read=%d,%v", n, err)
		}
		rt.pool.mu.Lock()
		active := child.active
		rt.pool.mu.Unlock()
		if active != 0 {
			t.Fatalf("reservation leak %d", active)
		}
	}
}

func TestNativeRequestLifetimeClientTimeoutUnblocksBodyRead(t *testing.T) {
	body := &nativeAuditBlockingBody{started: make(chan struct{}), closed: make(chan struct{})}
	req, _ := http.NewRequestWithContext(t.Context(), "POST", "http://audit.invalid/", body)
	// There is no pool: timeout must stop before body preparation finishes.
	client := &http.Client{Transport: &gatewayNativeRoundTripper{}, Timeout: 30 * time.Millisecond}
	_, err := client.Do(req)
	if !errors.Is(err, context.DeadlineExceeded) || body.closes.Load() != 1 {
		t.Fatalf("timeout=%v closes=%d", err, body.closes.Load())
	}
}
