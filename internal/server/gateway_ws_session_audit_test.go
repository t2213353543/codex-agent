package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type gatewayWSSessionAudit struct {
	p        *Proxy
	client   *websocket.Conn
	upstream *websocket.Conn
	accepted <-chan *websocket.Conn
	requests <-chan []byte
	closed   <-chan struct{}
}

func newGatewayWSSessionAudit(t *testing.T, session string, change func(*Settings)) *gatewayWSSessionAudit {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	requests := make(chan []byte, 32)
	closed := make(chan struct{})
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer close(closed)
		defer conn.CloseNow()
		accepted <- conn
		for {
			_, payload, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			select {
			case requests <- payload:
			case <-t.Context().Done():
				return
			}
		}
	})
	enableGatewayWS(t, p, "")
	if change != nil {
		settings, err := p.Settings.Get()
		if err != nil {
			t.Fatal(err)
		}
		change(&settings)
		if err := p.Settings.Set(settings); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	client, _ := gatewayWSDial(t, server.URL, session)
	return &gatewayWSSessionAudit{p: p, client: client, accepted: accepted, requests: requests, closed: closed}
}

func (s *gatewayWSSessionAudit) send(t *testing.T, raw string) {
	t.Helper()
	if err := gatewayWSWrite(t.Context(), s.client, websocket.MessageText, []byte(raw)); err != nil {
		t.Fatal(err)
	}
}

func (s *gatewayWSSessionAudit) forwarded(t *testing.T) []byte {
	t.Helper()
	select {
	case raw := <-s.requests:
		return raw
	case <-time.After(5 * time.Second):
		t.Fatal("gateway did not forward the expected request")
		return nil
	}
}

func (s *gatewayWSSessionAudit) event(t *testing.T, raw string) {
	t.Helper()
	if s.upstream == nil {
		select {
		case s.upstream = <-s.accepted:
		case <-time.After(5 * time.Second):
			t.Fatal("upstream socket was not accepted")
		}
	}
	if err := gatewayWSWrite(t.Context(), s.upstream, websocket.MessageText, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	if got := string(gatewayWSRead(t, s.client)); got != raw {
		t.Fatalf("response bytes changed: got %s, want %s", got, raw)
	}
}

func TestGatewayWSSessionAuditScopedErrorsPreserveOtherLanesAndFIFO(t *testing.T) {
	s := newGatewayWSSessionAudit(t, "session-audit", nil)
	for _, req := range []string{
		`{"type":"response.create","model":"first-a","stream_id":"a","input":[]}`,
		`{"type":"response.create","model":"second-a","stream_id":"a","input":[]}`,
		`{"type":"response.create","model":"first-b","stream_id":"b","input":[]}`,
	} {
		s.send(t, req)
		s.forwarded(t)
	}
	if s.p.inFlight("test") != 3 {
		t.Fatal("parallel lanes and queued requests lost admission tracking")
	}
	s.event(t, `{"type":"response.created","stream_id":"a","response":{"id":"audit-a-first","status":"in_progress"}}`)
	s.event(t, `{"type":"error","stream_id":"a","response_id":"audit-a-first","status":400,"error":{"message":"bad first turn"}}`)
	if s.p.inFlight("test") != 2 {
		t.Fatal("a scoped error did not release exactly the failed FIFO head")
	}
	// Duplicate errors, including an unscoped late error carrying a completed
	// response identity, are forwarded without consuming or closing a new turn.
	s.event(t, `{"type":"error","stream_id":"a","response_id":"audit-a-first","error":{"message":"late duplicate"}}`)
	s.event(t, `{"type":"error","response_id":"audit-a-first","error":{"message":"late unscoped duplicate"}}`)
	s.event(t, `{"type":"error","stream_id":"unknown","error":{"message":"unknown lane"}}`)
	if s.p.inFlight("test") != 2 {
		t.Fatal("late/unknown scoped errors consumed an unrelated turn")
	}
	s.event(t, `{"type":"response.completed","stream_id":"b","response":{"id":"audit-b-first","status":"completed","usage":{"input_tokens":22,"output_tokens":1}}}`)
	s.event(t, `{"type":"response.completed","stream_id":"a","response":{"id":"audit-a-first","status":"completed","usage":{"input_tokens":999,"output_tokens":1}}}`)
	if s.p.inFlight("test") != 1 {
		t.Fatal("a repeated terminal completed the next FIFO head")
	}
	s.event(t, `{"type":"response.completed","stream_id":"a","response":{"id":"audit-a-second","status":"completed","usage":{"input_tokens":33,"output_tokens":1}}}`)
	waitGatewayWSReleased(t, s.p)
	records := s.p.Metrics.RecentForAccount("test", 10)
	if len(records) != 3 {
		t.Fatalf("wrong admitted response count: %d", len(records))
	}
	for _, rec := range records {
		switch rec.Model {
		case "first-a":
			if rec.Status != "error" || rec.Error != "bad first turn" {
				t.Fatalf("failed lane did not keep its error: %+v", rec)
			}
		case "second-a", "first-b":
			if rec.Status != "ok" || rec.ErrorSource != "" || rec.InputTokens == 999 {
				t.Fatalf("a different turn's error polluted this record: %+v", rec)
			}
		default:
			t.Fatalf("unexpected record model: %s", rec.Model)
		}
	}
	// Non-create protocol extensions are opaque and do not acquire a slot.
	control := `{"type":"future.control","stream_id":"a","opaque":{"preserve":[1,null,"x"]}}`
	s.send(t, control)
	if got := string(s.forwarded(t)); got != control || s.p.inFlight("test") != 0 {
		t.Fatalf("non-create event was mutated or admitted: %s", got)
	}
}

func TestGatewayWSSessionAuditUnscopedErrorClosesAndReleasesAll(t *testing.T) {
	s := newGatewayWSSessionAudit(t, "unscoped-audit", nil)
	for _, lane := range []string{"a", "b"} {
		s.send(t, fmt.Sprintf(`{"type":"response.create","model":"test","stream_id":%q,"input":[]}`, lane))
		s.forwarded(t)
	}
	s.event(t, `{"type":"error","status":500,"error":{"message":"connection protocol failed"}}`)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, _, err := s.client.Read(ctx); err == nil {
		t.Fatal("connection-scoped error left the socket usable")
	}
	select {
	case <-s.closed:
	case <-ctx.Done():
		t.Fatal("gateway did not close upstream before releasing aborted turns")
	}
	waitGatewayWSReleased(t, s.p)
	records := s.p.Metrics.RecentForAccount("test", 10)
	if len(records) != 2 {
		t.Fatalf("unscoped failure lost an active record: %d", len(records))
	}
	for _, rec := range records {
		if rec.Status != "aborted" || rec.ErrorSource != "upstream" || rec.ErrorPhase != "response_event" {
			t.Fatalf("unscoped failure attribution: %+v", rec)
		}
	}
}

func TestGatewayWSSessionAuditValidationWarmupAndContinuation(t *testing.T) {
	s := newGatewayWSSessionAudit(t, "", nil)
	invalid := []string{
		`{"type":"future.control"}`,
		`{"type":"response.create","model":12}`,
		`{"type":"response.create","model":"test","stream_id":""}`,
		`{"type":"response.create","model":"test","stream_id":"white space"}`,
		`{"type":"response.create","model":"test","stream_id":"中文"}`,
		fmt.Sprintf(`{"type":"response.create","model":"test","stream_id":%q}`, strings.Repeat("a", 257)),
		`{"type":"response.create","model":"test","previous_response_id":42}`,
		`{"type":"response.create","model":"test","generate":"false"}`,
	}
	for _, raw := range invalid {
		s.send(t, raw)
		var got struct{ Status int }
		if err := json.Unmarshal(gatewayWSRead(t, s.client), &got); err != nil || got.Status != 400 {
			t.Fatalf("invalid create was not rejected: %s (%+v, %v)", raw, got, err)
		}
		if s.p.inFlight("test") != 0 {
			t.Fatal("rejected event leaked an admission slot")
		}
	}
	select {
	case raw := <-s.requests:
		t.Fatalf("invalid event reached upstream: %s", raw)
	default:
	}
	lane := "Ab9_.-" + strings.Repeat("x", 250)
	s.send(t, fmt.Sprintf(`{"type":"response.create","model":"warmup","stream_id":%q,"generate":false,"input":[],"client_metadata":{"opaque":"keep"}}`, lane))
	var warmup map[string]json.RawMessage
	if err := json.Unmarshal(s.forwarded(t), &warmup); err != nil || string(warmup["generate"]) != "false" || string(warmup["client_metadata"]) != `{"opaque":"keep"}` {
		t.Fatalf("warmup create changed: %+v, %v", warmup, err)
	}
	s.event(t, fmt.Sprintf(`{"type":"response.completed","stream_id":%q,"response":{"id":"audit-warmup","status":"completed","output":[]}}`, lane))
	waitGatewayWSReleased(t, s.p)
	s.send(t, fmt.Sprintf(`{"type":"response.create","model":"continue","stream_id":%q,"previous_response_id":"audit-warmup","input":[]}`, lane))
	var continuation map[string]json.RawMessage
	if err := json.Unmarshal(s.forwarded(t), &continuation); err != nil || string(continuation["previous_response_id"]) != `"audit-warmup"` || string(continuation["input"]) != `[]` {
		t.Fatalf("continuation identity or empty input changed: %+v, %v", continuation, err)
	}
	// A rejected request between two valid events cannot terminate the current
	// lane or require replay of the accepted continuation.
	s.send(t, `{"type":"response.create","model":"invalid","stream_id":"bad/lane"}`)
	if got := gatewayWSRead(t, s.client); !strings.Contains(string(got), `"status":400`) || s.p.inFlight("test") != 1 {
		t.Fatalf("recoverable create validation aborted active work: %s", got)
	}
	s.event(t, fmt.Sprintf(`{"type":"response.completed","stream_id":%q,"response":{"id":"audit-continued","status":"completed","output":[]}}`, lane))
	waitGatewayWSReleased(t, s.p)
}

func TestGatewayWSSessionAuditSettingsChangesRequireNewAdmission(t *testing.T) {
	for _, name := range []string{"disable", "fingerprint"} {
		t.Run(name, func(t *testing.T) {
			s := newGatewayWSSessionAudit(t, "settings-audit", nil)
			s.send(t, `{"type":"response.create","model":"first","input":[]}`)
			s.forwarded(t)
			s.event(t, `{"type":"response.completed","response":{"id":"settings-first","status":"completed"}}`)
			settings, err := s.p.Settings.Get()
			if err != nil {
				t.Fatal(err)
			}
			wantStatus := 403
			if name == "disable" {
				settings.WebsocketEnabled = false
			} else {
				settings.CodexFingerprintEnabled = !settings.CodexFingerprintEnabled
				wantStatus = 409
			}
			if err := s.p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			s.send(t, `{"type":"response.create","model":"rejected","input":[]}`)
			var got struct{ Status int }
			if err := json.Unmarshal(gatewayWSRead(t, s.client), &got); err != nil || got.Status != wantStatus {
				t.Fatalf("new turn ignored changed setting: %+v, %v", got, err)
			}
			waitGatewayWSReleased(t, s.p)
			select {
			case raw := <-s.requests:
				t.Fatalf("rejected settings turn reached upstream: %s", raw)
			default:
			}
		})
	}
}

func TestGatewayWSSessionAuditLazyHandshakeUsesLatestFingerprintSettings(t *testing.T) {
	s := newGatewayWSSessionAudit(t, "", nil)
	settings, err := s.p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.CodexFingerprintEnabled = !settings.CodexFingerprintEnabled
	if err := s.p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	s.send(t, `{"type":"response.create","model":"lazy-handshake","input":[]}`)
	s.forwarded(t)
	s.event(t, `{"type":"response.completed","response":{"id":"lazy-handshake-response","status":"completed"}}`)
	waitGatewayWSReleased(t, s.p)
}

func TestGatewayWSSessionAuditActiveSocketIdleTimeout(t *testing.T) {
	s := newGatewayWSSessionAudit(t, "idle-audit", func(settings *Settings) {
		settings.UpstreamStreamIdleTimeoutSeconds = 10
	})
	s.send(t, `{"type":"response.create","model":"stalled","input":[]}`)
	s.forwarded(t)
	ctx, cancel := context.WithTimeout(t.Context(), 13*time.Second)
	defer cancel()
	_, _, err := s.client.Read(ctx)
	if err == nil || ctx.Err() != nil {
		t.Fatalf("active socket did not apply configured 10 second stream idle timeout: %v", err)
	}
	select {
	case <-s.closed:
	case <-ctx.Done():
		t.Fatal("idle timeout did not close the upstream socket")
	}
	waitGatewayWSReleased(t, s.p)
	rec, ok := s.p.Metrics.LastFor("test")
	if !ok || rec.Status != "aborted" || rec.Error != errUpstreamStreamIdleTimeout.Error() || rec.ErrorPhase != "response_idle" {
		t.Fatalf("idle timeout provenance lost: %+v", rec)
	}
}

func TestGatewayWSSessionAuditIdleWatcherQueueAndAbort(t *testing.T) {
	p := newTestProxy(t, 3)
	turns := gatewayWSTurns{proxy: p, keyID: "admin", idleChanged: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); turns.watchIdle(ctx, cancel) }()
	t.Cleanup(func() { cancel(); <-done })
	var released atomic.Int32
	start := func(lane string) {
		t.Helper()
		if err := turns.start([]byte(fmt.Sprintf(`{"model":"test","stream_id":%q}`, lane)), Account{Name: "test"}, nil, func() { released.Add(1) }, 80*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	start("a")
	start("a")
	turns.mu.Lock()
	turns.pending["a"][1].lastActivity = time.Now().Add(-time.Hour)
	turns.mu.Unlock()
	turns.observe([]byte(`{"type":"response.completed","stream_id":"a","response":{"id":"queue-first","status":"completed"}}`))
	turns.mu.Lock()
	promoted := turns.pending["a"][0].lastActivity
	turns.mu.Unlock()
	if time.Since(promoted) > time.Second {
		t.Fatal("promoted FIFO head inherited queue wait as stream idle time")
	}
	turns.observe([]byte(`{"type":"response.completed","stream_id":"a","response":{"id":"queue-second","status":"completed"}}`))
	select {
	case <-ctx.Done():
		t.Fatal("completed, idle connection was subject to a global stream timeout")
	case <-time.After(160 * time.Millisecond):
	}
	start("stalled")
	start("busy")
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
wait:
	for {
		select {
		case <-ctx.Done():
			break wait
		case <-ticker.C:
			turns.observe([]byte(`{"type":"response.output_text.delta","stream_id":"busy","delta":"progress"}`))
		case <-deadline.C:
			t.Fatal("busy lane concealed the stalled lane's deadline")
		}
	}
	turns.abort()
	turns.abort()
	if released.Load() != 4 || turns.count != 0 || len(turns.pending) != 0 || len(p.Metrics.RecentForAccount("test", 10)) != 4 {
		t.Fatal("abort retained turns or duplicated record/admission release")
	}
}
