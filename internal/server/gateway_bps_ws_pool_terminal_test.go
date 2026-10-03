package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codex-agent/internal/bps"
	"github.com/coder/websocket"
)

type remainingProtocolSocket struct{ frames [][]byte }

func (s *remainingProtocolSocket) Read(context.Context) (websocket.MessageType, []byte, error) {
	if len(s.frames) == 0 {
		return websocket.MessageText, nil, io.EOF
	}
	frame := s.frames[0]
	s.frames = s.frames[1:]
	return websocket.MessageText, frame, nil
}
func (*remainingProtocolSocket) Write(context.Context, websocket.MessageType, []byte) error {
	return nil
}
func (*remainingProtocolSocket) CloseNow() error    { return nil }
func (*remainingProtocolSocket) SetReadLimit(int64) {}

func remainingProtocolClosedPooledBody(t *testing.T, frame []byte) *bpsWebSocketBody {
	t.Helper()
	pool := &bpsWSPool{}
	route := &bpsWSSocketRoute{inbox: make(chan []byte, bpsWSInboxBuffer), done: make(chan struct{})}
	conn := &remainingProtocolSocket{frames: [][]byte{frame}}
	socket := pool.adopt(conn, func() {}, "synthetic-account", "synthetic-affinity", route)
	select {
	case <-socket.deadCh:
	case <-time.After(time.Second):
		t.Fatal("real pool reader did not observe peer EOF")
	}
	return &bpsWebSocketBody{conn: conn, reuse: true, pool: pool, socket: socket, inbox: route.inbox, dead: socket.deadCh, requestDone: route.done}
}

func TestRemainingProtocolPooledReceivedTerminalSurvivesPeerEOF(t *testing.T) {
	frame := []byte(`{"type":"response.completed","response":{"id":"synthetic","status":"completed","output":[]}}`)
	lost := 0
	for i := 0; i < 128; i++ {
		body := remainingProtocolClosedPooledBody(t, frame)
		if len(body.inbox) != 1 {
			t.Fatal("completed frame was not first received and queued by real readLoop")
		}
		got, err := body.frame()
		if err != nil || !bytes.Equal(got, frame) {
			lost++
		}
	}
	if lost != 0 {
		t.Fatalf("received terminal discarded after peer EOF: lost=%d/128", lost)
	}
}

func TestRemainingProtocolPooledCancellationTerminalPreserved(t *testing.T) {
	frame := []byte(`{"type":"basispoints.response.cancelled","response":{"id":"synthetic","status":"cancelled","output":[]}}`)
	body := remainingProtocolClosedPooledBody(t, frame)
	if len(body.inbox) != 1 {
		t.Fatal("real pooled reader swallowed cancellation terminal before body/parser")
	}
	var output bytes.Buffer
	result, err := bps.Stream(t.Context(), &output, body, bps.StreamOptions{})
	if err == nil || result.Completed || result.Status != "cancelled" || result.TerminalType != "basispoints.response.cancelled" || !strings.Contains(output.String(), "basispoints.response.cancelled") {
		t.Fatal("actual upstream cancellation lost or treated as success", result, err)
	}
	if body.pool.idleCount("synthetic-account") != 0 {
		t.Fatal("cancelled socket was reused")
	}
}

func TestRemainingProtocolPooledInternalControlsStayPrivate(t *testing.T) {
	for _, kind := range []string{"basispoints.response.metadata", "basispoints.response.heartbeat", "basispoints.response.resume_token", "basispoints.private_extension"} {
		body := remainingProtocolClosedPooledBody(t, []byte(`{"type":"`+kind+`","resume_token":"PRIVATE_SENTINEL"}`))
		if len(body.inbox) != 0 {
			t.Fatal("internal control entered public event queue", kind)
		}
		var output bytes.Buffer
		result, err := bps.Stream(t.Context(), &output, body, bps.StreamOptions{})
		if err == nil || result.Completed || output.Len() != 0 {
			t.Fatal("internal control became response data", kind)
		}
	}
}

func TestRemainingProtocolPooledTerminalParsingAfterPeerEOF(t *testing.T) {
	for _, tc := range []struct {
		kind, status string
		complete     bool
	}{
		{"response.completed", "completed", true}, {"response.done", "completed", true},
		{"response.failed", "failed", false}, {"response.incomplete", "incomplete", false}, {"response.cancelled", "cancelled", false},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			frame := []byte(`{"type":"` + tc.kind + `","response":{"id":"synthetic","status":"` + tc.status + `","output":[],"usage":{"input_tokens":2,"output_tokens":1}}}`)
			body := remainingProtocolClosedPooledBody(t, frame)
			var output bytes.Buffer
			result, err := bps.Stream(t.Context(), &output, body, bps.StreamOptions{})
			if result.Completed != tc.complete || (err == nil) != tc.complete || result.Status != tc.status || !bytes.Contains(output.Bytes(), []byte(tc.kind)) {
				t.Fatal("received terminal was overwritten by transport close", result, err)
			}
			if body.pool.idleCount("synthetic-account") != 0 {
				t.Fatal("closed socket was returned to pool")
			}
		})
	}
	// Retaining a real delta must not turn premature peer EOF into success.
	body := remainingProtocolClosedPooledBody(t, []byte(`{"type":"response.output_text.delta","delta":"partial"}`))
	var output bytes.Buffer
	result, err := bps.Stream(t.Context(), &output, body, bps.StreamOptions{})
	if err == nil || result.Completed || !strings.Contains(output.String(), "partial") {
		t.Fatal("incomplete output hidden or falsely completed", result, err)
	}
}

func TestRemainingProtocolPooledActualPeerCloseKeepsTerminal(t *testing.T) {
	for _, kind := range []string{"response.completed", "basispoints.response.cancelled"} {
		t.Run(kind, func(t *testing.T) {
			var frames, handshakes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("unexpected HTTP generation replay")
					w.WriteHeader(500)
					return
				}
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"responses"}, InsecureSkipVerify: true})
				if err != nil {
					return
				}
				defer conn.CloseNow()
				handshakes.Add(1)
				ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
				defer cancel()
				if _, _, err := conn.Read(ctx); err != nil {
					return
				}
				frames.Add(1)
				state := "completed"
				if kind == "basispoints.response.cancelled" {
					state = "cancelled"
				}
				for _, event := range []string{
					`{"type":"basispoints.response.metadata","private":"PRIVATE_SENTINEL"}`,
					`{"type":"basispoints.response.heartbeat"}`,
					`{"type":"` + kind + `","response":{"id":"synthetic_closed_peer","status":"` + state + `","output":[]}}`,
				} {
					if err := conn.Write(ctx, websocket.MessageText, []byte(event)); err != nil {
						t.Error("fixture write failed", err)
						return
					}
				}
				// A real network peer closes immediately after its terminal.
			}))
			defer server.Close()
			url := "ws" + strings.TrimPrefix(server.URL, "http") + "/responses"
			p := bpsReuseProxy(t, url, true)
			request, _ := http.NewRequestWithContext(contextWithWSAccount(t.Context(), "synthetic-closed-peer"), http.MethodPost, url, nil)
			request.Header.Set("Authorization", "Bearer synthetic-token")
			response, err := p.bpsWebSocketRequest(request, nil, []byte(`{"model":"fixture","input":[]}`), bpsReuseSettings(t, p))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body := response.Body.(*idleStreamBody).body.(*bpsWebSocketBody)
			select {
			case <-body.dead:
			case <-time.After(3 * time.Second):
				t.Fatal("real pool did not observe actual peer close")
			}
			var output bytes.Buffer
			result, err := bps.Stream(t.Context(), &output, response.Body, bps.StreamOptions{})
			completed := kind == "response.completed"
			if result.Completed != completed || (err == nil) != completed || !strings.Contains(output.String(), kind) {
				t.Fatal("real peer close discarded observed terminal", result, err)
			}
			if strings.Contains(output.String(), "PRIVATE_SENTINEL") || strings.Contains(output.String(), "basispoints.response.metadata") {
				t.Fatal("internal metadata leaked")
			}
			if frames.Load() != 1 || handshakes.Load() != 1 {
				t.Fatal("generation was replayed or socket unexpectedly retried")
			}
			if p.bpsWSSockets.idleCount(bpsWSPoolKey(gatewayWSAccountRoute{ID: "synthetic-closed-peer"}, nil)) != 0 {
				t.Fatal("closed socket retained for reuse")
			}
		})
	}
}

func TestRemainingProtocolPooledQueuedTerminalDoesNotOverrideClientCancellation(t *testing.T) {
	body := remainingProtocolClosedPooledBody(t, []byte(`{"type":"response.completed","response":{"id":"synthetic","status":"completed","output":[]}}`))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var output bytes.Buffer
	result, err := bps.Stream(ctx, &output, body, bps.StreamOptions{})
	if !errors.Is(err, context.Canceled) || result.Completed || output.Len() != 0 {
		t.Fatal("queued terminal turned explicit caller cancellation into success", result, err)
	}
}
