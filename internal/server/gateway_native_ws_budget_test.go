package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type nativeWSBudgetCountingConn struct {
	net.Conn
	read atomic.Int64
}

func (c *nativeWSBudgetCountingConn) Read(data []byte) (int, error) {
	n, err := c.Conn.Read(data)
	c.read.Add(int64(n))
	return n, err
}

func nativeWSBudgetPair(t *testing.T) (*gatewayNativeWS, net.Conn, *atomic.Int32, *nativeWSBudgetCountingConn) {
	t.Helper()
	client, helper := net.Pipe()
	counted := &nativeWSBudgetCountingConn{Conn: client}
	released := new(atomic.Int32)
	ws := newGatewayNativeWS(t.Context(), counted, func() { released.Add(1) })
	t.Cleanup(func() { _ = helper.Close(); _ = ws.CloseNow() })
	return ws, helper, released, counted
}

func nativeWSBudgetWaitQueued(t *testing.T, ws *gatewayNativeWS, want int64) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if ws.queued.Load() == want {
			return
		}
		select {
		case <-ws.done:
			t.Fatalf("stream failed before expected queue occupancy: %v", ws.failure())
		case <-deadline.C:
			t.Fatalf("queue occupancy=%d want=%d", ws.queued.Load(), want)
		case <-tick.C:
		}
	}
}

func nativeWSBudgetWriteMessage(writer io.Writer, sequence uint64, payload []byte, headerSent chan<- struct{}) error {
	var prefix [5]byte
	prefix[0] = nativeWSMessage
	binary.BigEndian.PutUint32(prefix[1:], uint32(len(payload)+9))
	if err := nativeWSWriteAll(writer, prefix[:]); err != nil {
		return err
	}
	if headerSent != nil {
		close(headerSent)
	}
	var envelope [9]byte
	binary.BigEndian.PutUint64(envelope[:8], sequence)
	envelope[8] = byte(websocket.MessageBinary)
	if err := nativeWSWriteAll(writer, envelope[:]); err != nil {
		return err
	}
	return nativeWSWriteAll(writer, payload)
}

func TestGatewayNativeWSBudgetBackpressurePreservesLegalLargeMessages(t *testing.T) {
	ws, helper, released, counted := nativeWSBudgetPair(t)
	const size = 20 << 20
	payload := bytes.Repeat([]byte{0x80}, size)
	firstDone := make(chan error, 1)
	go func() { firstDone <- nativeWSBudgetWriteMessage(helper, 1, payload, nil) }()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	nativeWSBudgetWaitQueued(t, ws, size)
	headerSent, secondDone := make(chan struct{}), make(chan error, 1)
	go func() { secondDone <- nativeWSBudgetWriteMessage(helper, 2, payload, headerSent) }()
	select {
	case <-headerSent:
	case <-time.After(3 * time.Second):
		t.Fatal("second frame header did not reach the read pump")
	}
	// The second legal message must wait for byte credits, not consume its
	// body and close the stream because the application paused briefly.
	select {
	case err := <-secondDone:
		t.Fatalf("second legal message bypassed queue byte backpressure: %v", err)
	case <-ws.done:
		t.Fatalf("temporary large-message queue became a stream error: %v", ws.failure())
	case <-time.After(100 * time.Millisecond):
	}
	if queued := ws.queued.Load(); queued > gatewayNativeBodyLimit {
		t.Fatalf("queue exceeded its bounded byte budget: %d", queued)
	}
	if read := counted.read.Load(); read > size+2*(5+9) {
		t.Fatalf("budget wait consumed second message payload before credits: read=%d", read)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for message := range 2 {
		typ, actual, err := ws.Read(ctx)
		if err != nil || actual == nil || typ != websocket.MessageBinary || !bytes.Equal(actual, payload) {
			t.Fatalf("legal message %d failed after consumer resumed: length=%d type=%d err=%v", message+1, len(actual), typ, err)
		}
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	_ = ws.CloseNow()
	if ws.queued.Load() != 0 || released.Load() != 1 {
		t.Fatalf("drained stream retained budget or admission: queued=%d released=%d", ws.queued.Load(), released.Load())
	}
}

func TestGatewayNativeWSBudgetReducedReadLimitWakesPendingFrame(t *testing.T) {
	ws, helper, released, counted := nativeWSBudgetPair(t)
	const size = 20 << 20
	payload := bytes.Repeat([]byte{'x'}, size)
	firstDone := make(chan error, 1)
	go func() { firstDone <- nativeWSBudgetWriteMessage(helper, 1, payload, nil) }()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	nativeWSBudgetWaitQueued(t, ws, size)
	headerSent, producer := make(chan struct{}), make(chan error, 1)
	go func() { producer <- nativeWSBudgetWriteMessage(helper, 2, payload, headerSent) }()
	select {
	case <-headerSent:
	case <-time.After(3 * time.Second):
		t.Fatal("pending budget frame header did not arrive")
	}
	ws.SetReadLimit(16 << 20)
	select {
	case <-ws.pumpDone:
	case <-time.After(time.Second):
		t.Fatal("lowering the read limit did not wake the budget waiter")
	}
	if read := counted.read.Load(); read > size+2*(5+9) {
		t.Fatalf("lowered read limit still consumed the pending oversized payload: read=%d", read)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	typ, raw, err := ws.Read(ctx)
	if !errors.Is(err, errNativeWSLimit) || typ != 0 || raw != nil {
		t.Fatalf("lowered limit returned queued oversized data: type=%d length=%d err=%v", typ, len(raw), err)
	}
	_ = ws.CloseNow()
	_ = ws.CloseNow()
	select {
	case <-producer:
	case <-time.After(time.Second):
		t.Fatal("limit rejection left an IPC producer blocked")
	}
	if ws.queued.Load() != 0 || released.Load() != 1 {
		t.Fatalf("limit rejection retained queued payload or released twice: queued=%d released=%d", ws.queued.Load(), released.Load())
	}
}

func TestGatewayNativeWSBudgetCloseUnblocksWaitingFrame(t *testing.T) {
	ws, helper, released := nativeWSTestPair(t)
	const size = 20 << 20
	payload := bytes.Repeat([]byte{'x'}, size)
	firstDone := make(chan error, 1)
	go func() { firstDone <- nativeWSBudgetWriteMessage(helper, 1, payload, nil) }()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	nativeWSBudgetWaitQueued(t, ws, size)
	headerSent, producer := make(chan struct{}), make(chan error, 1)
	go func() { producer <- nativeWSBudgetWriteMessage(helper, 2, payload, headerSent) }()
	<-headerSent
	started := time.Now()
	_ = ws.CloseNow()
	select {
	case <-producer:
	case <-time.After(time.Second):
		t.Fatal("CloseNow left a producer waiting for byte-budget credits")
	}
	if time.Since(started) > time.Second || ws.queued.Load() != 0 || released.Load() != 1 {
		t.Fatalf("budget close leaked resources: queued=%d released=%d", ws.queued.Load(), released.Load())
	}
}

func TestGatewayNativeWSBudgetEmptyMessageIsSuccessfulMessage(t *testing.T) {
	ws, helper, released := nativeWSTestPair(t)
	var writes atomic.Int32
	producer := make(chan error, 1)
	go func() {
		writes.Add(1)
		producer <- nativeWSBudgetWriteMessage(helper, 1, []byte{}, nil)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	typ, raw, err := ws.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || len(raw) != 0 {
		t.Fatalf("empty valid message was mistaken for EOF: type=%d length=%d err=%v", typ, len(raw), err)
	}
	if err := <-producer; err != nil || writes.Load() != 1 {
		t.Fatal("empty message producer failed")
	}
	_ = ws.CloseNow()
	if ws.queued.Load() != 0 || released.Load() != 1 {
		t.Fatal("empty message retained byte-budget or admission")
	}
}

func TestGatewayNativeWSBudgetConcurrentReadAndExplicitClose(t *testing.T) {
	const count = 8
	payload := bytes.Repeat([]byte{'q'}, 1024)
	for round := range 4 {
		ws, helper, released := nativeWSTestPair(t)
		producer := make(chan error, 1)
		go func() {
			for sequence := uint64(1); sequence <= count; sequence++ {
				if err := nativeWSBudgetWriteMessage(helper, sequence, payload, nil); err != nil {
					producer <- err
					return
				}
			}
			producer <- nil
		}()
		if err := <-producer; err != nil {
			t.Fatal(err)
		}
		nativeWSBudgetWaitQueued(t, ws, count*int64(len(payload)))
		start := make(chan struct{})
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		var group sync.WaitGroup
		for range count {
			group.Go(func() {
				<-start
				typ, actual, err := ws.Read(ctx)
				if err == nil && (typ != websocket.MessageBinary || !bytes.Equal(actual, payload)) {
					t.Error("Read racing explicit close returned a partial or invalid message")
				}
				if errors.Is(err, context.DeadlineExceeded) {
					t.Error("Read racing explicit close waited for its deadline")
				}
			})
		}
		for range 3 {
			group.Go(func() { <-start; _ = ws.CloseNow() })
		}
		close(start)
		group.Wait()
		cancel()
		if queued := ws.queued.Load(); queued != 0 || released.Load() != 1 {
			t.Fatalf("round %d concurrent reads/close corrupted accounting: queued=%d released=%d", round, queued, released.Load())
		}
		select {
		case <-ws.pumpDone:
		default:
			t.Fatal("explicit close returned before its read pump finished")
		}
	}
}
