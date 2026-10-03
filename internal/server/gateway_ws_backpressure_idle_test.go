package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGatewayWSBackpressureDoesNotBecomeUpstreamIdle(t *testing.T) {
	for _, scenario := range []struct{ terminal, failWrite bool }{{true, false}, {false, false}, {true, true}} {
		terminal, failWrite := scenario.terminal, scenario.failWrite
		t.Run(fmt.Sprintf("terminal=%t/write_failure=%t", terminal, failWrite), func(t *testing.T) {
			var fail atomic.Bool
			gate := &gatewayWSDeliveryGate{entered: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
			firstRead := make(chan struct{})
			sendBlocked := make(chan struct{})
			secondReady := make(chan struct{})
			handlerDone := make(chan struct{})
			var blocked []byte
			if terminal {
				blocked = []byte(`{"type":"response.completed","response":{"id":"slow-first","status":"completed","usage":{"input_tokens":11,"output_tokens":1}}}`)
			} else {
				blocked = []byte(`{"type":"response.output_text.delta","response_id":"slow-first","delta":"` + strings.Repeat("x", (4<<20)+17) + `"}`)
			}
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				if _, _, err = conn.Read(r.Context()); err != nil {
					return
				}
				close(firstRead)
				if err = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.created","response":{"id":"slow-first","status":"in_progress"}}`)); err != nil {
					return
				}
				select {
				case <-sendBlocked:
				case <-r.Context().Done():
					return
				}
				if err = conn.Write(r.Context(), websocket.MessageText, blocked); err != nil {
					return
				}
				if _, _, err = conn.Read(r.Context()); err != nil {
					return
				}
				// The server has computed the next response while the gateway is
				// blocked on its previous downstream write; it is not upstream idle.
				if !terminal {
					_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"slow-first","status":"completed","usage":{"input_tokens":11,"output_tokens":1}}}`))
				}
				if err = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"slow-second","status":"completed","usage":{"input_tokens":22,"output_tokens":2}}}`)); err != nil {
					return
				}
				close(secondReady)
				_, _, _ = conn.Read(r.Context())
			})
			enableGatewayWS(t, p, "")
			settings, err := p.Settings.Get()
			if err != nil {
				t.Fatal(err)
			}
			settings.UpstreamStreamIdleTimeoutSeconds = 10
			if err = p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(handlerDone)
				p.ServeHTTP(gatewayWSDeliveryFaultWriter{ResponseWriter: w, fail: &fail, gate: gate}, r)
			}))
			t.Cleanup(server.Close)
			client, _ := gatewayWSDial(t, server.URL, "backpressure-clock")
			client.SetReadLimit(maxBody)
			ctx, cancel := context.WithTimeout(t.Context(), 16*time.Second)
			defer cancel()
			if err = client.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"first","input":[]}`)); err != nil {
				t.Fatal(err)
			}
			gatewayWSRead(t, client)
			<-firstRead
			gate.enabled.Store(true)
			close(sendBlocked)
			var once sync.Once
			unblock := func() { once.Do(func() { close(gate.release) }) }
			defer unblock()
			select {
			case <-gate.entered:
			case <-ctx.Done():
				t.Fatal("downstream write did not block")
			}
			if err = client.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"second","input":[]}`)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-secondReady:
			case <-ctx.Done():
				t.Fatal("upstream next response did not reach write barrier")
			}
			// Exceed the genuine configured upstream idle timeout while remaining
			// well below the independent 60s downstream writer deadline.
			select {
			case <-gate.closed:
				unblock()
				<-handlerDone
				for _, record := range p.Metrics.RecentForAccount("test", 10) {
					t.Logf("model=%s status=%s phase=%s", record.Model, record.Status, record.ErrorPhase)
				}
				t.Fatal("downstream backpressure was charged as upstream idle")
			case <-time.After(10500 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if failWrite {
				fail.Store(true)
			}
			unblock()
			if failWrite {
				if _, _, err := client.Read(ctx); err == nil || ctx.Err() != nil {
					t.Fatalf("fixture did not fail the blocked downstream write: %v", err)
				}
				select {
				case <-handlerDone:
				case <-ctx.Done():
					t.Fatal("failed writer retained handler")
				}
				waitGatewayWSReleased(t, p)
				records := p.Metrics.RecentForAccount("test", 10)
				if len(records) != 2 {
					t.Fatalf("failed writer records=%d", len(records))
				}
				for _, record := range records {
					if record.Status != "aborted" || record.ErrorSource != "client" || record.ErrorPhase != "response_write" {
						t.Fatalf("failed write raced upstream idle provenance: model=%s source=%s phase=%s", record.Model, record.ErrorSource, record.ErrorPhase)
					}
				}
				return
			}
			if got := gatewayWSRead(t, client); string(got) != string(blocked) {
				t.Fatal("blocked response bytes changed")
			}
			if !terminal {
				gatewayWSRead(t, client)
			}
			gatewayWSRead(t, client)
			_ = client.CloseNow()
			select {
			case <-handlerDone:
			case <-ctx.Done():
				t.Fatal("gateway did not finish")
			}
			waitGatewayWSReleased(t, p)
			records := p.Metrics.RecentForAccount("test", 10)
			if len(records) != 2 {
				t.Fatalf("records=%d", len(records))
			}
			for _, record := range records {
				if record.Status != "ok" || record.ErrorSource != "" {
					t.Fatalf("slow consumer polluted upstream state: %+v", record)
				}
			}
		})
	}
}

func TestGatewayWSBackpressureIdleTimeExcludesOnlyBlockedInterval(t *testing.T) {
	turns := gatewayWSTurns{idleChanged: make(chan struct{}, 1), pending: make(map[string][]*gatewayWSTurn)}
	paused := time.Now().Add(-2 * time.Second)
	old := &gatewayWSTurn{lastActivity: paused.Add(-6 * time.Second), idleTimeout: 10 * time.Second}
	newHead := &gatewayWSTurn{lastActivity: paused.Add(time.Second), idleTimeout: 10 * time.Second}
	queued := &gatewayWSTurn{lastActivity: paused.Add(-time.Hour), idleTimeout: 10 * time.Second}
	queuedOriginal := queued.lastActivity
	turns.pending["old"] = []*gatewayWSTurn{old, queued}
	turns.pending["new"] = []*gatewayWSTurn{newHead}
	turns.downstreamWriteStarted = paused
	turns.resumeIdleAfterDownstreamWrite()
	now := time.Now()
	remaining := old.lastActivity.Add(old.idleTimeout).Sub(now)
	if remaining < 3900*time.Millisecond || remaining > 4100*time.Millisecond {
		t.Fatalf("6/10s consumed before write became %v remaining", remaining)
	}
	if age := now.Sub(newHead.lastActivity); age < 0 || age > 100*time.Millisecond {
		t.Fatalf("head created during pause got extra pre-pause credit: %v", age)
	}
	if queued.lastActivity != queuedOriginal {
		t.Fatal("queued non-head timestamp was changed instead of promotion semantics")
	}
	before := old.lastActivity
	turns.resumeIdleAfterDownstreamWrite()
	if old.lastActivity != before {
		t.Fatal("second resume credited the interval twice")
	}
}

func TestGatewayWSBackpressureIdleWatcherResumeAndTotalCancel(t *testing.T) {
	for _, cancelDuringWrite := range []bool{true, false} {
		t.Run(fmt.Sprintf("cancel=%t", cancelDuringWrite), func(t *testing.T) {
			turn := &gatewayWSTurn{lastActivity: time.Now().Add(-time.Hour), idleTimeout: time.Second}
			turns := gatewayWSTurns{idleChanged: make(chan struct{}, 1), pending: map[string][]*gatewayWSTurn{"stalled": {turn}}}
			turns.pauseIdleForDownstreamWrite()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); turns.watchIdle(ctx, cancel) }()
			select {
			case <-ctx.Done():
				t.Fatal("idle watcher ran while upstream polling was suspended")
			case <-time.After(20 * time.Millisecond):
			}
			if cancelDuringWrite {
				cancel()
			} else {
				turns.resumeIdleAfterDownstreamWrite()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("watcher missed resume/parent cancellation")
			}
			if cancelDuringWrite && turn.rec.ErrorSource != "" {
				t.Fatal("parent cancellation was attributed to upstream idle")
			}
			if !cancelDuringWrite && turn.rec.ErrorPhase != "response_idle" {
				t.Fatal("preexisting stalled lane received a fresh idle budget")
			}
		})
	}
}

type gatewayWSBackpressureDeadlineWriter struct{ deadline time.Time }

func (w *gatewayWSBackpressureDeadlineWriter) Read(context.Context) (websocket.MessageType, []byte, error) {
	panic("unused")
}
func (w *gatewayWSBackpressureDeadlineWriter) Write(ctx context.Context, _ websocket.MessageType, _ []byte) error {
	w.deadline, _ = ctx.Deadline()
	<-ctx.Done()
	return ctx.Err()
}
func (w *gatewayWSBackpressureDeadlineWriter) CloseNow() error    { return nil }
func (w *gatewayWSBackpressureDeadlineWriter) SetReadLimit(int64) {}

func TestGatewayWSBackpressureIndependentWriterAndParentDeadline(t *testing.T) {
	for _, parentFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("parent_first=%t", parentFirst), func(t *testing.T) {
			parentBudget, writeBudget := time.Second, 25*time.Millisecond
			if parentFirst {
				parentBudget, writeBudget = 25*time.Millisecond, time.Second
			}
			ctx, cancel := context.WithTimeout(t.Context(), parentBudget)
			defer cancel()
			writer := &gatewayWSBackpressureDeadlineWriter{}
			started := time.Now()
			err := gatewayWSWriteWithTimeout(ctx, writer, websocket.MessageText, []byte(`{}`), writeBudget)
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
				t.Fatalf("write did not honor tighter deadline: %v", err)
			}
			if writer.deadline.IsZero() || writer.deadline.Sub(started) > 100*time.Millisecond {
				t.Fatal("writer context lost effective earliest deadline")
			}
			if (ctx.Err() != nil) != parentFirst {
				t.Fatal("writer-local budget mutated parent cancellation")
			}
		})
	}
}

func TestGatewayWSBackpressureParentCancelClosesBlockedDelivery(t *testing.T) {
	var fail atomic.Bool
	gate := &gatewayWSDeliveryGate{entered: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	start := make(chan struct{})
	requestCancel := make(chan context.CancelFunc, 1)
	finished := make(chan struct{})
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, _, err = conn.Read(r.Context()); err != nil {
			return
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.created","response":{"id":"parent-cancel"}}`))
		<-start
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.output_text.delta","response_id":"parent-cancel","delta":"do not reset parent deadline"}`))
		_, _, _ = conn.Read(r.Context())
	})
	enableGatewayWS(t, p, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		requestCancel <- cancel
		p.ServeHTTP(gatewayWSDeliveryFaultWriter{ResponseWriter: w, fail: &fail, gate: gate}, r.WithContext(ctx))
	}))
	t.Cleanup(server.Close)
	client, _ := gatewayWSDial(t, server.URL, "parent-deadline")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := client.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"parent","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	gatewayWSRead(t, client)
	gate.enabled.Store(true)
	close(start)
	var once sync.Once
	unblock := func() { once.Do(func() { close(gate.release) }) }
	defer unblock()
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal("write did not reach barrier")
	}
	(<-requestCancel)()
	select {
	case <-gate.closed:
	case <-ctx.Done():
		t.Fatal("paused idle state prevented parent cancel from closing downstream")
	}
	unblock()
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("handler retained paused delivery after total cancellation")
	}
	waitGatewayWSReleased(t, p)
	record, ok := p.Metrics.LastFor("test")
	if !ok || record.Status != "aborted" || record.ErrorPhase == "response_idle" {
		t.Fatal("parent cancellation was mislabeled upstream stall")
	}
}
