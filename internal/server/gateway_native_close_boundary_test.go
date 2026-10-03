package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A consumer may close a streamed response after inspecting a prefix. Compare
// the adapter's retained IPC chunk with the standard HTTP transport contract.
func TestGatewayNativeClosedResponseDiscardsBufferedRemainder(t *testing.T) {
	for _, transport := range []string{"standard_http", "native_ipc"} {
		t.Run(transport, func(t *testing.T) {
			var response *http.Response
			if transport == "standard_http" {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.WriteString(w, strings.Repeat("x", 4096))
				}))
				defer server.Close()
				var err error
				response, err = server.Client().Get(server.URL)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				rt, _ := nativeAuditHTTPPeer(t, func(conn net.Conn, _ map[string]json.RawMessage) {
					nativeAuditResponse(conn, 200, nil, strings.Repeat("x", 4096), true)
				})
				request, _ := http.NewRequestWithContext(t.Context(), "GET", "http://audit.invalid/close", nil)
				var err error
				response, err = rt.RoundTrip(request)
				if err != nil {
					t.Fatal(err)
				}
			}
			one := make([]byte, 1)
			if n, err := response.Body.Read(one); n != 1 || err != nil {
				t.Fatalf("initial prefix: n=%d err=%v", n, err)
			}
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if n, err := response.Body.Read(one); n != 0 || err == nil {
				t.Fatalf("closed body delivered retained output: n=%d err=%v", n, err)
			}
		})
	}
}

func TestGatewayNativeActualBufferedBodyClose(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(map[bool]string{false: "close", true: "cancel_and_close"}[cancelRequest], func(t *testing.T) {
			var calls atomic.Int32
			upstreamCancelled := make(chan struct{})
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.WriteString(w, strings.Repeat("x", 32768))
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(upstreamCancelled)
			})
			realGatewayNativePool(t, p)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "GET", p.BaseURL+"/stream", nil)
			response, err := p.clientFor(nil).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if n, err := response.Body.Read(make([]byte, 1)); n != 1 || err != nil {
				t.Fatalf("prefix: %d %v", n, err)
			}
			if cancelRequest {
				cancel()
			}
			_ = response.Body.Close()
			if n, err := response.Body.Read(make([]byte, 16)); n != 0 || err == nil || cancelRequest && !errors.Is(err, context.Canceled) {
				t.Fatalf("closed real helper body: %d %v", n, err)
			}
			select {
			case <-upstreamCancelled:
			case <-time.After(3 * time.Second):
				t.Fatal("close did not cancel real upstream stream")
			}
			if calls.Load() != 1 {
				t.Fatal("close/cancel replayed upstream request")
			}
			nativeProxyDispatchAssertIdle(t, p.NativeHTTP)
		})
	}
}

func TestGatewayNativeWSTotalDeadlineClassification(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		phase := "handshake"
		if accepted {
			phase = "message"
		}
		t.Run(phase, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"phase": phase, "accepted": accepted, "code": "total_timeout"})
			err := nativeWSRemoteError(raw, accepted)
			var timeout net.Error
			if !errors.As(err, &timeout) || !timeout.Timeout() {
				t.Fatalf("lost native total deadline: %v", err)
			}
			if !errors.Is(nativeWSRemoteError(raw, !accepted), errNativeWSProtocol) {
				t.Fatal("deadline bypassed phase validation")
			}
		})
	}
	ws, peer, released := nativeWSTestPair(t)
	go func() {
		_ = nativeWSTestPacket(peer, nativeWSMessage, nativeWSTestMessage(1, websocket.MessageText, "completed-before-deadline"))
		_ = nativeWSTestPacket(peer, nativeWSError, []byte(`{"phase":"message","accepted":true,"code":"total_timeout"}`))
	}()
	<-ws.pumpDone
	_, data, err := ws.Read(t.Context())
	if err != nil || string(data) != "completed-before-deadline" {
		t.Fatalf("deadline discarded preceding message: %q %v", data, err)
	}
	_, _, err = ws.Read(t.Context())
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() || released.Load() != 1 {
		t.Fatalf("deadline/release: %v releases=%d", err, released.Load())
	}
}

func TestGatewayNativeBodyConcurrentCloseUnblocksRead(t *testing.T) {
	peerEntered := make(chan struct{})
	rt, _ := nativeAuditHTTPPeer(t, func(conn net.Conn, _ map[string]json.RawMessage) {
		nativeAuditResponse(conn, 200, nil, "", false)
		close(peerEntered)
		_, _ = io.Copy(io.Discard, conn)
	})
	request, _ := http.NewRequestWithContext(t.Context(), "GET", "http://audit.invalid/blocked", nil)
	response, err := rt.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	<-peerEntered
	var readers sync.WaitGroup
	readers.Go(func() {
		if n, err := response.Body.Read(make([]byte, 1)); n != 0 || err == nil {
			t.Errorf("abandoned read: n=%d err=%v", n, err)
		}
	})
	var closers sync.WaitGroup
	for range 8 {
		closers.Go(func() { _ = response.Body.Close() })
	}
	closers.Wait()
	readers.Wait()
}

// Completion is different from explicit abandonment: a fully drained response
// keeps EOF, and request cancellation must remain identifiable after Close.
func TestGatewayNativeBodyTerminalAndCancellationRemainTyped(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "cancelled"}[cancelRequest], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			rt, _ := nativeAuditHTTPPeer(t, func(conn net.Conn, _ map[string]json.RawMessage) {
				nativeAuditResponse(conn, 200, nil, "body", !cancelRequest)
				if cancelRequest {
					_, _ = io.Copy(io.Discard, conn)
				}
			})
			request, _ := http.NewRequestWithContext(ctx, "GET", "http://audit.invalid/terminal", nil)
			response, err := rt.RoundTrip(request)
			if err != nil {
				t.Fatal(err)
			}
			if cancelRequest {
				one := make([]byte, 1)
				if _, err := response.Body.Read(one); err != nil {
					t.Fatal(err)
				}
				cancel()
				_ = response.Body.Close()
				if n, err := response.Body.Read(one); n != 0 || !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled body delivered output or lost cancellation: n=%d err=%v", n, err)
				}
			} else {
				body, err := io.ReadAll(response.Body)
				if err != nil || string(body) != "body" {
					t.Fatalf("read completion: %q %v", body, err)
				}
				if n, err := response.Body.Read(make([]byte, 1)); n != 0 || err != io.EOF {
					t.Fatalf("terminal lost: n=%d err=%v", n, err)
				}
			}
			_ = response.Body.Close()
		})
	}
}
