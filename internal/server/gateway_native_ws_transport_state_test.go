package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The IPC peer is local Go net.Pipe, not Rust or a TLS/browser simulator.
func TestNativeWSTransportStateParentCancelFullQueue(t *testing.T) {
	client, peer := net.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	defer peer.Close()
	var releases atomic.Int32
	ws := newGatewayNativeWS(ctx, client, func() { releases.Add(1) })
	defer ws.CloseNow()
	produced := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for id := uint64(1); id <= 17; id++ {
			if nativeWSTestPacket(peer, nativeWSMessage, nativeWSTestMessage(id, websocket.MessageBinary, "bytes")) != nil {
				return
			}
		}
		close(produced)
		_, _ = io.Copy(io.Discard, peer)
	}()
	select {
	case <-produced:
	case <-time.After(time.Second):
		t.Fatal("17th frame did not reach full message queue")
	}
	cancel()
	select {
	case <-ws.pumpDone:
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not stop full-queue read pump")
	}
	typ, raw, err := ws.Read(ctx)
	if !errors.Is(err, context.Canceled) || typ != 0 || raw != nil {
		t.Fatal("parent-canceled Read returned queued output")
	}
	if len(ws.messages) != 16 || ws.queued.Load() != 80 {
		t.Fatalf("pending rejected frame lost/corrupted retained bytes: queued=%d packets=%d", ws.queued.Load(), len(ws.messages))
	}
	var closes sync.WaitGroup
	for range 8 {
		closes.Go(func() { _ = ws.CloseNow() })
	}
	closes.Wait()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("Close left IPC producer alive")
	}
	if ws.queued.Load() != 0 || releases.Load() != 1 {
		t.Fatal("cancel/parallel Close did not drain once")
	}
}

func TestNativeWSTransportStatePeerCloseDrainsExactOrder(t *testing.T) {
	for _, closeCode := range []int{1000, 1001} {
		t.Run(fmt.Sprint(closeCode), func(t *testing.T) {
			ws, peer, releases := nativeWSTestPair(t)
			values := []string{"", `{"type":"response.created"}`, string(bytes.Repeat([]byte{'x'}, (4<<20)+17)), `{"type":"response.completed"}`}
			done := make(chan error, 1)
			go func() {
				for i, value := range values {
					if err := nativeWSTestPacket(peer, nativeWSMessage, nativeWSTestMessage(uint64(i+1), websocket.MessageText, value)); err != nil {
						done <- err
						return
					}
				}
				raw := []byte(`{"code":1000,"reason":"synthetic"}`)
				if closeCode == 1001 {
					raw = []byte(`{"code":1001,"reason":"synthetic"}`)
				}
				done <- nativeWSTestPacket(peer, nativeWSClose, raw)
			}()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			select {
			case <-ws.pumpDone:
			case <-time.After(time.Second):
				t.Fatal("peer close did not end pump")
			}
			for _, value := range values {
				typ, got, err := ws.Read(t.Context())
				if err != nil || typ != websocket.MessageText || string(got) != value {
					t.Fatal("peer close discarded or reordered pre-close complete message")
				}
			}
			_, _, err := ws.Read(t.Context())
			if websocket.CloseStatus(err) != websocket.StatusCode(closeCode) {
				t.Fatal("peer close status lost after queued bytes")
			}
			_ = ws.CloseNow()
			if ws.queued.Load() != 0 || releases.Load() != 1 {
				t.Fatal("peer close drain budget wrong")
			}
		})
	}
}

func TestNativeWSTransportStateLimitShrinkWithFullMessageQueue(t *testing.T) {
	ws, peer, releases := nativeWSTestPair(t)
	full := make(chan struct{})
	go func() {
		for id := uint64(1); id <= 17; id++ {
			if nativeWSTestPacket(peer, nativeWSMessage, nativeWSTestMessage(id, websocket.MessageBinary, "12345678")) != nil {
				return
			}
		}
		close(full)
	}()
	select {
	case <-full:
	case <-time.After(time.Second):
		t.Fatal("full message queue premise missing")
	}
	ws.SetReadLimit(4)
	typ, raw, err := ws.Read(t.Context())
	if !errors.Is(err, errNativeWSLimit) || typ != 0 || raw != nil {
		t.Fatal("Read returned previously queued message above current limit")
	}
	_ = ws.CloseNow()
	if ws.queued.Load() != 0 || len(ws.messages) != 0 || releases.Load() != 1 {
		t.Fatal("shrunk message queue leaked credit/release")
	}
}

func TestNativeWSTransportStateByteBudgetShrinkCancelAndReaders(t *testing.T) {
	for round := 0; round < 6; round++ {
		ws, peer, releases, _ := nativeWSBudgetPair(t)
		payload := bytes.Repeat([]byte{'q'}, 20<<20)
		first := make(chan error, 1)
		go func() { first <- nativeWSBudgetWriteMessage(peer, 1, payload, nil) }()
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		nativeWSBudgetWaitQueued(t, ws, int64(len(payload)))
		header := make(chan struct{})
		second := make(chan error, 1)
		go func() { second <- nativeWSBudgetWriteMessage(peer, 2, payload, header) }()
		select {
		case <-header:
		case <-time.After(time.Second):
			t.Fatal("second byte-budget waiter missing")
		}
		ctx, cancel := context.WithCancel(t.Context())
		start := make(chan struct{})
		var group sync.WaitGroup
		for range 4 {
			group.Go(func() {
				<-start
				_, raw, err := ws.Read(ctx)
				if err == nil && !bytes.Equal(raw, payload) {
					t.Error("concurrent Read produced partial message")
				}
			})
		}
		group.Go(func() { <-start; ws.SetReadLimit(8 << 20) })
		group.Go(func() { <-start; cancel() })
		for range 3 {
			group.Go(func() { <-start; _ = ws.CloseNow() })
		}
		close(start)
		done := make(chan struct{})
		go func() { group.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("shrink/cancel/reader/Close interleaving hung")
		}
		select {
		case <-second:
		case <-time.After(time.Second):
			t.Fatal("closed byte-budget producer hung")
		}
		_ = ws.CloseNow()
		if ws.queued.Load() != 0 || releases.Load() != 1 {
			t.Fatalf("round%d leaked budget/release: %d/%d", round, ws.queued.Load(), releases.Load())
		}
	}
}

// Preserve an actual native transport failure that ended the pump; a later
// explicit cleanup must not replace it with the local Close error.
func TestNativeWSTransportStateFailureCauseSurvivesCleanup(t *testing.T) {
	ws, peer, releases := nativeWSTestPair(t)
	go func() {
		_ = nativeWSTestPacket(peer, nativeWSError, []byte(`{"phase":"message","code":"ws_ipc_read_timeout","accepted":true}`))
	}()
	_, _, first := ws.Read(t.Context())
	if first == nil {
		t.Fatal("native error was not surfaced")
	}
	var timeout net.Error
	if !errors.As(first, &timeout) || !timeout.Timeout() {
		t.Fatal("valid native timeout became an unrelated protocol error")
	}
	_ = ws.CloseNow()
	_, _, after := ws.Read(t.Context())
	if !errors.Is(after, first) {
		t.Fatal("cleanup replaced the established transport error")
	}
	if releases.Load() != 1 || ws.queued.Load() != 0 {
		t.Fatal("failed stream cleanup leaked state")
	}
}
