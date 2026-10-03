package server

// Session D (2026-09-30): BPS WebSocket socket-reuse regression tests. The
// official client runs one in-flight generation per socket and reuses it
// sequentially (rotation after 4 idle minutes or 8 completed requests); these
// tests pin the gateway's pooled equivalent against a synthetic upstream.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// bpsReuseUpstream counts handshakes, generation frames, live sockets,
// cancellations and affinity values observed by the synthetic upstream.
type bpsReuseUpstream struct {
	accepted    atomic.Int64
	frames      atomic.Int64
	openSockets atomic.Int64
	cancels     atomic.Int64
	affinities  sync.Map
	server      *httptest.Server

	connsMu sync.Mutex
	conns   map[*websocket.Conn]struct{}
}

// closeUpstream closes every accepted websocket from the server side;
// httptest's CloseClientConnections does not reach hijacked sockets.
func (u *bpsReuseUpstream) closeUpstream() {
	u.connsMu.Lock()
	defer u.connsMu.Unlock()
	for conn := range u.conns {
		_ = conn.CloseNow()
	}
	u.conns = nil
}

func (u *bpsReuseUpstream) url() string { return "ws" + strings.TrimPrefix(u.server.URL, "http") + "/responses" }

// newBPSReuseUpstreamSimple serves the complete generation lifecycle: for
// every response.create frame it writes one heartbeat control, one delta and
// a completed terminal, then keeps the socket open for the next generation.
func newBPSReuseUpstreamSimple(t *testing.T) *bpsReuseUpstream {
	t.Helper()
	upstream := &bpsReuseUpstream{}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		if value := r.URL.Query().Get("bps_ws_affinity"); value != "" {
			count, _ := upstream.affinities.LoadOrStore(value, 0)
			upstream.affinities.Store(value, count.(int)+1)
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"responses"}, InsecureSkipVerify: true})
		if err != nil {
			return
		}
		upstream.accepted.Add(1)
		upstream.openSockets.Add(1)
		upstream.connsMu.Lock()
		if upstream.conns == nil {
			upstream.conns = map[*websocket.Conn]struct{}{}
		}
		upstream.conns[conn] = struct{}{}
		upstream.connsMu.Unlock()
		defer func() {
			upstream.openSockets.Add(-1)
			upstream.connsMu.Lock()
			delete(upstream.conns, conn)
			upstream.connsMu.Unlock()
			conn.CloseNow()
		}()
		writer := make(chan []byte, 8)
		go func() {
			for frame := range writer {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := conn.Write(ctx, websocket.MessageText, frame)
				cancel()
				if err != nil {
					return
				}
			}
		}()
		defer close(writer)
		for {
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			_, raw, err := conn.Read(ctx)
			cancel()
			if err != nil {
				return
			}
			if strings.Contains(string(raw), "basispoints.response.cancel") {
				upstream.cancels.Add(1)
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(raw, &fields) != nil || string(fields["type"]) != `"response.create"` {
				continue
			}
			upstream.frames.Add(1)
			writer <- []byte(`{"type":"basispoints.response.heartbeat"}`)
			writer <- []byte(`{"type":"response.output_text.delta","delta":"REUSED"}`)
			writer <- []byte(`{"type":"response.completed","response":{"id":"resp_reuse","status":"completed","output":[{"type":"message","id":"msg","role":"assistant","content":[{"type":"output_text","text":"REUSED"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
		}
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

// bpsReuseProxy builds a proxy wired for WS preference plus the chosen reuse
// mode against the synthetic upstream.
func bpsReuseProxy(t *testing.T, upstreamURL string, reuse bool) *Proxy {
	t.Helper()
	p := newTestProxy(t, 1)
	p.BaseURL = strings.TrimPrefix(upstreamURL, "ws")
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.PreferWebsocket = true
	settings.BPSWebSocketReuse = reuse
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	return p
}

func bpsReuseSettings(t *testing.T, p *Proxy) Settings {
	t.Helper()
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

func bpsReuseGenerate(t *testing.T, p *Proxy, upstreamURL, accountID string) (string, error) {
	t.Helper()
	req, _ := http.NewRequestWithContext(contextWithWSAccount(t.Context(), accountID), http.MethodPost, upstreamURL, nil)
	req.Header.Set("Authorization", "Bearer synthetic-reuse-token")
	response, err := p.bpsWebSocketRequest(req, nil, []byte(`{"model":"fixture","input":[]}`), bpsReuseSettings(t, p))
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func TestBPSWSReuseSequentialRequestsShareOneSocket(t *testing.T) {
	upstream := newBPSReuseUpstreamSimple(t)
	p := bpsReuseProxy(t, upstream.url(), true)
	key := bpsWSPoolKey(gatewayWSAccountRoute{ID: "reuse-account"}, nil)
	for i := 0; i < 3; i++ {
		body, err := bpsReuseGenerate(t, p, upstream.url(), "reuse-account")
		if err != nil {
			t.Fatalf("generation %d failed: %v", i, err)
		}
		if !strings.Contains(body, "response.completed") {
			t.Fatalf("generation %d missed its terminal: %s", i, body)
		}
	}
	if got := upstream.accepted.Load(); got != 1 {
		t.Fatalf("sequential generations must reuse one socket, dialed=%d", got)
	}
	if got := p.bpsWSSockets.idleCount(key); got != 1 {
		t.Fatalf("one idle socket expected after sequential use, got %d", got)
	}
	p.bpsWSSockets.closeAll()
}

func TestBPSWSReuseRotatesAfterEightRequests(t *testing.T) {
	upstream := newBPSReuseUpstreamSimple(t)
	p := bpsReuseProxy(t, upstream.url(), true)
	for i := 0; i < bpsWSSocketMaxRequests; i++ {
		if _, err := bpsReuseGenerate(t, p, upstream.url(), "rotate-account"); err != nil {
			t.Fatalf("generation %d failed: %v", i, err)
		}
	}
	if got := upstream.accepted.Load(); got != 1 {
		t.Fatalf("first %d generations share one socket, dialed=%d", bpsWSSocketMaxRequests, got)
	}
	if _, err := bpsReuseGenerate(t, p, upstream.url(), "rotate-account"); err != nil {
		t.Fatal(err)
	}
	if got := upstream.accepted.Load(); got != 2 {
		t.Fatalf("rotation after %d completed requests expected, dialed=%d", bpsWSSocketMaxRequests, got)
	}
	p.bpsWSSockets.closeAll()
}

func TestBPSWSReuseExpiresAfterIdleWindow(t *testing.T) {
	upstream := newBPSReuseUpstreamSimple(t)
	p := bpsReuseProxy(t, upstream.url(), true)
	key := bpsWSPoolKey(gatewayWSAccountRoute{ID: "idle-account"}, nil)
	if _, err := bpsReuseGenerate(t, p, upstream.url(), "idle-account"); err != nil {
		t.Fatal(err)
	}
	if p.bpsWSSockets.idleCount(key) != 1 {
		t.Fatal("precondition: one idle socket pooled")
	}
	// Age the idle socket past the official rotation window.
	p.bpsWSSockets.mu.Lock()
	for _, socket := range p.bpsWSSockets.idle[key] {
		socket.idleSince = time.Now().Add(-bpsWSSocketIdleRotation - time.Second)
	}
	p.bpsWSSockets.mu.Unlock()
	if _, err := bpsReuseGenerate(t, p, upstream.url(), "idle-account"); err != nil {
		t.Fatal(err)
	}
	if got := upstream.accepted.Load(); got != 2 {
		t.Fatalf("expired idle socket must rotate to a fresh dial, dialed=%d", got)
	}
	p.bpsWSSockets.closeAll()
}

func TestBPSWSReuseScopesSocketsToCredentialEpochAndProxy(t *testing.T) {
	keyA := bpsWSPoolKey(gatewayWSAccountRoute{ID: "epoch-account", CredentialVersion: 3}, nil)
	keyB := bpsWSPoolKey(gatewayWSAccountRoute{ID: "epoch-account", CredentialVersion: 4}, nil)
	if keyA == keyB {
		t.Fatal("credential epoch must participate in the pool key")
	}
	proxyKey := bpsWSPoolKey(gatewayWSAccountRoute{ID: "epoch-account", CredentialVersion: 3}, &ProxyEntry{ID: "px"})
	if proxyKey == keyA {
		t.Fatal("proxy must participate in the pool key")
	}
}

func TestBPSWSReuseDisabledKeepsPerRequestSockets(t *testing.T) {
	upstream := newBPSReuseUpstreamSimple(t)
	p := bpsReuseProxy(t, upstream.url(), false)
	for i := 0; i < 3; i++ {
		if _, err := bpsReuseGenerate(t, p, upstream.url(), "noreuse-account"); err != nil {
			t.Fatalf("generation %d failed: %v", i, err)
		}
	}
	if got := upstream.accepted.Load(); got != 3 {
		t.Fatalf("reuse disabled must dial per request, dialed=%d", got)
	}
}

func TestBPSWSReuseKeepsStableAffinityPerSocket(t *testing.T) {
	upstream := newBPSReuseUpstreamSimple(t)
	p := bpsReuseProxy(t, upstream.url(), true)
	for i := 0; i < 3; i++ {
		if _, err := bpsReuseGenerate(t, p, upstream.url(), "affinity-account"); err != nil {
			t.Fatalf("generation %d failed: %v", i, err)
		}
	}
	distinct := 0
	upstream.affinities.Range(func(_, value any) bool {
		distinct++
		return true
	})
	if distinct != 1 {
		t.Fatalf("one reused socket must present exactly one affinity, saw %d", distinct)
	}
	p.bpsWSSockets.closeAll()
}

func TestBPSWSReuseConcurrentGenerationsUseDistinctSockets(t *testing.T) {
	upstream := newBPSReuseUpstreamSimple(t)
	p := bpsReuseProxy(t, upstream.url(), true)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := bpsReuseGenerate(t, p, upstream.url(), "parallel-account"); err != nil {
				t.Errorf("parallel generation failed: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := upstream.accepted.Load(); got != 3 {
		t.Fatalf("single-flight per socket requires one dial each, dialed=%d", got)
	}
	key := bpsWSPoolKey(gatewayWSAccountRoute{ID: "parallel-account"}, nil)
	if got := p.bpsWSSockets.idleCount(key); got > bpsWSPoolMaxIdlePerKey {
		t.Fatalf("idle pool exceeded its bound: %d", got)
	}
	p.bpsWSSockets.closeAll()
}

// A mid-generation read failure must evict the socket; the next generation
// dials fresh instead of inheriting broken protocol state.
func TestBPSWSReuseMidStreamFailureEvictsSocket(t *testing.T) {
	upstream := newBPSReuseUpstreamSimple(t)
	p := bpsReuseProxy(t, upstream.url(), true)
	req, _ := http.NewRequestWithContext(contextWithWSAccount(t.Context(), "broken-account"), http.MethodPost, upstream.url(), nil)
	req.Header.Set("Authorization", "Bearer synthetic-reuse-token")
	response, err := p.bpsWebSocketRequest(req, nil, []byte(`{"model":"fixture","input":[]}`), bpsReuseSettings(t, p))
	if err != nil {
		t.Fatal(err)
	}
	// Kill the upstream side before the terminal is consumed.
	upstream.closeUpstream()
	if _, err := io.ReadAll(response.Body); err == nil {
		t.Fatal("expected the in-flight generation to fail")
	}
	response.Body.Close()
	key := bpsWSPoolKey(gatewayWSAccountRoute{ID: "broken-account"}, nil)
	if got := p.bpsWSSockets.idleCount(key); got != 0 {
		t.Fatalf("failed socket must not be pooled, idle=%d", got)
	}
	if _, err := bpsReuseGenerate(t, p, upstream.url(), "broken-account"); err != nil {
		t.Fatal(err)
	}
	if got := upstream.accepted.Load(); got != 2 {
		t.Fatalf("next generation must dial a fresh socket, dialed=%d", got)
	}
	p.bpsWSSockets.closeAll()
}

// An idle socket the server closes is evicted by the janitor before the next
// checkout can hand it out.
func TestBPSWSReuseJanitorEvictsClosedIdleSocket(t *testing.T) {
	upstream := newBPSReuseUpstreamSimple(t)
	p := bpsReuseProxy(t, upstream.url(), true)
	if _, err := bpsReuseGenerate(t, p, upstream.url(), "janitor-account"); err != nil {
		t.Fatal(err)
	}
	upstream.closeUpstream()
	key := bpsWSPoolKey(gatewayWSAccountRoute{ID: "janitor-account"}, nil)
	deadline := time.Now().Add(5 * time.Second)
	for p.bpsWSSockets.idleCount(key) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := p.bpsWSSockets.idleCount(key); got != 0 {
		t.Fatalf("janitor must evict a server-closed idle socket, idle=%d", got)
	}
	p.bpsWSSockets.closeAll()
}

// An explicitly cancelled generation sends the official cancel control and
// closes its socket; the socket is never reused afterwards.
func TestBPSWSReuseClientAbortCancelsAndDiscards(t *testing.T) {
	upstream := newBPSReuseUpstreamSimple(t)
	p := bpsReuseProxy(t, upstream.url(), true)
	req, _ := http.NewRequestWithContext(contextWithWSAccount(t.Context(), "abort-account"), http.MethodPost, upstream.url(), nil)
	req.Header.Set("Authorization", "Bearer synthetic-reuse-token")
	response, err := p.bpsWebSocketRequest(req, nil, []byte(`{"model":"fixture","input":[]}`), bpsReuseSettings(t, p))
	if err != nil {
		t.Fatal(err)
	}
	// Abort the downstream before the terminal is consumed.
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for upstream.cancels.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := upstream.cancels.Load(); got == 0 {
		t.Fatal("client abort must send the official cancellation control")
	}
	key := bpsWSPoolKey(gatewayWSAccountRoute{ID: "abort-account"}, nil)
	if got := p.bpsWSSockets.idleCount(key); got != 0 {
		t.Fatalf("cancelled socket must not be pooled, idle=%d", got)
	}
	if _, err := bpsReuseGenerate(t, p, upstream.url(), "abort-account"); err != nil {
		t.Fatal(err)
	}
	if got := upstream.accepted.Load(); got != 2 {
		t.Fatalf("next generation must dial fresh after cancel, dialed=%d", got)
	}
	p.bpsWSSockets.closeAll()
}
