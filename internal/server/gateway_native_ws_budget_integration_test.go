package server

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGatewayNativeActualWebSocketBudgetResume(t *testing.T) {
	const size = 20 << 20
	serverDone := make(chan struct{})
	p, _ := nativeWSFixture(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(serverDone)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for _, fill := range []byte{'a', 'b'} {
			// Keep the 20 MiB application message above half the IPC budget, but
			// fragment it below the official CLI's independent 16 MiB frame cap.
			writer, err := conn.Writer(r.Context(), websocket.MessageBinary)
			if err != nil {
				return
			}
			chunk := bytes.Repeat([]byte{fill}, size/2)
			for range 2 {
				if _, err := writer.Write(chunk); err != nil {
					return
				}
			}
			if err := writer.Close(); err != nil {
				return
			}
		}
		_, payload, err := conn.Read(r.Context())
		if err != nil || string(payload) != "tool-result" {
			return
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte("complete"))
		_, _, _ = conn.Read(r.Context())
	})
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	upstream, _, err := p.dialNativeGatewayWebSocket(ctx, make(http.Header), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.CloseNow()
	ws := upstream.(*gatewayNativeWS)
	deadline := time.Now().Add(5 * time.Second)
	for ws.queued.Load() < size && time.Now().Before(deadline) {
		select {
		case <-ws.done:
			t.Fatalf("native fixture failed before queueing its first message: %v", ws.failure())
		default:
		}
		time.Sleep(time.Millisecond)
	}
	if ws.queued.Load() != size {
		t.Fatal("native fixture did not reach its first queued large message")
	}
	writeDone := make(chan error, 1)
	go func() { writeDone <- ws.Write(ctx, websocket.MessageText, []byte("tool-result")) }()
	// Allow the second complete message to reach the IPC budget boundary while
	// the application deliberately pauses reading the first one.
	select {
	case <-ws.done:
		t.Fatal("legal large-message burst was rejected")
	case <-time.After(50 * time.Millisecond):
	}
	for _, fill := range []byte{'a', 'b'} {
		typ, payload, err := ws.Read(ctx)
		if err != nil || typ != websocket.MessageBinary || len(payload) != size {
			t.Fatal("native budget resume lost a complete large message")
		}
		for _, actual := range payload {
			if actual != fill {
				t.Fatal("native budget resume changed message bytes")
			}
		}
	}
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("resumed reader did not unblock write acknowledgement")
	}
	typ, payload, err := ws.Read(ctx)
	if err != nil || typ != websocket.MessageText || string(payload) != "complete" {
		t.Fatal("tool continuation did not complete after budget resumed")
	}
	_ = ws.CloseNow()
	if ws.queued.Load() != 0 {
		t.Fatal("explicit native close retained unread payload budget")
	}
	select {
	case <-serverDone:
	case <-ctx.Done():
		t.Fatal("native close retained upstream connection")
	}
}
