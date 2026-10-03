package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func startManagedGatewayTest(t *testing.T, p *Proxy) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stopped := make(chan error, 1)
	webDir := t.TempDir()
	go func() { stopped <- serveManaged(ctx, p, listener, webDir) }()
	return "http://" + listener.Addr().String(), cancel, stopped
}

func TestGatewayShutdownClosesIdleAndActiveWebSockets(t *testing.T) {
	for _, active := range []bool{false, true} {
		name := "idle"
		if active {
			name = "active"
		}
		t.Run(name, func(t *testing.T) {
			upstreamClosed := make(chan struct{})
			turnStarted := make(chan struct{})
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				defer close(upstreamClosed)
				if _, _, err = conn.Read(r.Context()); err != nil {
					return
				}
				close(turnStarted)
				_, _, _ = conn.Read(r.Context())
			})
			enableGatewayWS(t, p, "")
			address, cancel, stopped := startManagedGatewayTest(t, p)
			conn, _ := gatewayWSDial(t, address, "shutdown-session")
			// No client Read/Close participates in shutdown; an unresponsive peer
			// must not delay server exit or retain an account inference slot.
			if active {
				if err := gatewayWSWrite(context.Background(), conn, websocket.MessageText, []byte(`{"type":"response.create","model":"test","input":[]}`)); err != nil {
					t.Fatal(err)
				}
				select {
				case <-turnStarted:
				case <-time.After(5 * time.Second):
					t.Fatal("WebSocket turn did not start")
				}
				if p.inFlight("test") != 1 {
					t.Fatal("active WebSocket turn was not admitted")
				}
			}
			cancel()
			select {
			case err := <-stopped:
				if err != nil {
					t.Fatalf("managed shutdown: %v", err)
				}
			case <-time.After(3 * time.Second):
				_ = conn.CloseNow()
				t.Fatal("shutdown retained a hijacked WebSocket handler")
			}
			select {
			case <-upstreamClosed:
			case <-time.After(time.Second):
				t.Fatal("shutdown did not close upstream WebSocket")
			}
			if p.inFlight("test") != 0 {
				t.Fatal("shutdown retained an inference slot")
			}
			persisted, err := NewMetrics(p.Metrics.dir)
			if err != nil {
				t.Fatal(err)
			}
			defer persisted.Close()
			records := persisted.RecentForAccount("test", 10)
			if active {
				if len(records) != 1 || records[0].Status != "aborted" {
					t.Fatalf("final WebSocket outcome was not persisted: %+v", records)
				}
			} else if len(records) != 0 {
				t.Fatalf("idle socket produced request usage: %+v", records)
			}
		})
	}
}

func TestGatewayShutdownPreservesHTTPDrain(t *testing.T) {
	started := make(chan struct{})
	complete := make(chan struct{})
	var completeOnce sync.Once
	finishHTTP := func() { completeOnce.Do(func() { close(complete) }) }
	defer finishHTTP()
	upstreamCancelled := make(chan struct{})
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
			close(upstreamCancelled)
			return
		case <-complete:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_drain\",\"status\":\"completed\"}}\n\n")
		}
	})
	address, cancel, stopped := startManagedGatewayTest(t, p)
	request, err := http.NewRequest(http.MethodPost, address+"/v1/responses", strings.NewReader(`{"model":"test","input":[],"stream":false}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer test-client-key")
	clientDone := make(chan error, 1)
	go func() {
		response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
		if err == nil {
			defer response.Body.Close()
			_, err = io.Copy(io.Discard, response.Body)
			if response.StatusCode != http.StatusOK {
				err = fmt.Errorf("HTTP status %d", response.StatusCode)
			}
		}
		clientDone <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP turn did not start")
	}
	cancel()
	select {
	case <-upstreamCancelled:
		t.Fatal("shutdown canceled an HTTP turn instead of draining it")
	case err := <-stopped:
		t.Fatalf("shutdown returned before HTTP turn completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	finishHTTP()
	select {
	case err := <-clientDone:
		if err != nil {
			t.Fatalf("HTTP drain did not finish successfully: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP drain did not finish")
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("managed shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish after HTTP drain")
	}
}
