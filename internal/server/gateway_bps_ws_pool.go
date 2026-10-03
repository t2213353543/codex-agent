package server

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// The official Basispoints client keeps one in-flight generation per socket
// and reuses that socket sequentially, rotating it after four idle minutes or
// eight completed requests, with the affinity token retained for the socket's
// life (verified live: two response.create turns on one socket both reached
// response.completed). The gateway serves many downstream users per account,
// so this pool applies the same per-socket sequential reuse: a request checks
// out an idle healthy socket, streams exactly one generation, and returns the
// socket for the next request. Cancellation, mid-stream failures and draining
// never reuse a socket, mirroring the official client, and no generation is
// ever replayed on a reused socket.
//
// Each pooled socket owns one reader goroutine for its whole life. WebSocket
// reads must never overlap and cancelling a read context kills the
// connection, so the reader is never cancelled per request: while a
// generation is in flight it routes frames to that request's inbox, and while
// idle it consumes heartbeat/metadata controls and notices upstream closure
// (evicting the socket before it can be handed out again).

const (
	bpsWSSocketMaxRequests  = 8
	bpsWSSocketIdleRotation = 4 * time.Minute
	bpsWSPoolMaxIdlePerKey  = 4
	// One buffered frame keeps the reader ahead of the consumer without
	// buffering an unbounded response in the gateway.
	bpsWSInboxBuffer = 1
)

type bpsWSPool struct {
	mu   sync.Mutex
	idle map[string][]*bpsWSSocket
}

// bpsWSSocketRoute points the socket reader at one in-flight generation.
type bpsWSSocketRoute struct {
	inbox chan []byte
	done  chan struct{}
}

type bpsWSSocket struct {
	conn gatewayWSUpstream
	// stopLifetime ends the dial-time keep-alive context that keeps an
	// established upstream socket writable between requests.
	stopLifetime context.CancelFunc
	key          string
	affinity     string

	// state transitions happen under pool.mu.
	pool      *bpsWSPool
	inFlight  bool
	completed int
	idleSince time.Time

	route  atomic.Pointer[bpsWSSocketRoute]
	dead   atomic.Bool
	deadCh chan struct{}
}

// bpsWSPoolKey scopes reuse to one account credential epoch and egress. A
// rotated credential or a different proxy must never inherit a socket that
// was authenticated or routed differently.
func bpsWSPoolKey(route gatewayWSAccountRoute, proxy *ProxyEntry) string {
	proxyID := "direct"
	if proxy != nil && proxy.ID != "" {
		proxyID = proxy.ID
	}
	return route.ID + "\x00" + strconv.FormatInt(route.CredentialVersion, 10) + "\x00" + proxyID
}

// checkout returns an exclusive idle socket for the key, or nil when no
// reusable socket is available. Rotation policy and liveness apply here: an
// expired or dead socket is closed instead of handed out.
func (p *bpsWSPool) checkout(key string) *bpsWSSocket {
	if p == nil || key == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		queue := p.idle[key]
		if len(queue) == 0 {
			return nil
		}
		socket := queue[0]
		p.idle[key] = queue[1:]
		if len(p.idle[key]) == 0 {
			delete(p.idle, key)
		}
		if socket.expiredLocked() || socket.dead.Load() {
			socket.closeLocked()
			continue
		}
		socket.inFlight = true
		return socket
	}
}

// adopt registers a freshly established socket for its first (already
// in-flight) generation and starts its lifelong reader.
func (p *bpsWSPool) adopt(conn gatewayWSUpstream, stopLifetime context.CancelFunc, key, affinity string, route *bpsWSSocketRoute) *bpsWSSocket {
	if p == nil || key == "" {
		return nil
	}
	socket := &bpsWSSocket{conn: conn, stopLifetime: stopLifetime, key: key, affinity: affinity, pool: p, inFlight: true, deadCh: make(chan struct{})}
	socket.route.Store(route)
	go socket.readLoop()
	return socket
}

// attach routes the reader to a checked-out socket's generation.
func (socket *bpsWSSocket) attach(route *bpsWSSocketRoute) {
	socket.route.Store(route)
}

// detach stops routing; the reader returns to idle consumption.
func (socket *bpsWSSocket) detach() {
	socket.route.Store(nil)
}

// release returns a socket whose generation reached a normal terminal. The
// official rotation policy decides between keeping it idle and closing it.
func (p *bpsWSPool) release(socket *bpsWSSocket) {
	if socket == nil || p == nil {
		return
	}
	socket.detach()
	p.mu.Lock()
	defer p.mu.Unlock()
	if socket.pool != p || !socket.inFlight || socket.dead.Load() {
		socket.closeLocked()
		return
	}
	socket.completed++
	socket.inFlight = false
	if socket.completed >= bpsWSSocketMaxRequests || len(p.idle[socket.key]) >= bpsWSPoolMaxIdlePerKey {
		socket.closeLocked()
		return
	}
	socket.idleSince = time.Now()
	if p.idle == nil {
		p.idle = map[string][]*bpsWSSocket{}
	}
	p.idle[socket.key] = append(p.idle[socket.key], socket)
}

// discard closes a socket without reusing it (client cancellation, stream
// failure, draining signal, ambiguous write, protocol surprise).
func (p *bpsWSPool) discard(socket *bpsWSSocket) {
	if socket == nil || p == nil {
		return
	}
	socket.detach()
	p.mu.Lock()
	defer p.mu.Unlock()
	socket.closeLocked()
}

func (socket *bpsWSSocket) expiredLocked() bool {
	return !socket.idleSince.IsZero() && time.Since(socket.idleSince) >= bpsWSSocketIdleRotation
}

// closeLocked tears the socket down and drops it from any idle queue. The
// lifelong reader exits through its own read error or the closed connection.
func (socket *bpsWSSocket) closeLocked() {
	pool := socket.pool
	if pool == nil {
		return
	}
	if queue := pool.idle[socket.key]; len(queue) > 0 {
		kept := queue[:0]
		for _, candidate := range queue {
			if candidate != socket {
				kept = append(kept, candidate)
			}
		}
		if len(kept) == 0 {
			delete(pool.idle, socket.key)
		} else {
			pool.idle[socket.key] = kept
		}
	}
	socket.pool = nil
	socket.dead.Store(true)
	socket.detach()
	if socket.deadCh != nil {
		close(socket.deadCh)
	}
	_ = socket.conn.CloseNow()
	socket.stopLifetime()
}

// readLoop is the socket's single lifelong reader. It never runs under a
// cancellable per-request context: cancelling a websocket read would kill the
// connection. Control frames are consumed in every state; response frames go
// to the attached generation's inbox, and anything unexpected on an idle
// socket (or any read error) closes the socket for good.
func (socket *bpsWSSocket) readLoop() {
	// closeLocked nils socket.pool; the reader keeps its own stable reference.
	pool := socket.pool
	if pool == nil {
		return
	}
	shutdown := func() {
		socket.dead.Store(true)
		pool.mu.Lock()
		socket.closeLocked()
		pool.mu.Unlock()
	}
	for {
		typ, raw, err := socket.conn.Read(context.Background())
		if err != nil {
			shutdown()
			return
		}
		if typ != websocket.MessageText {
			// Binary frames are a protocol violation on this exchange.
			shutdown()
			return
		}
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &event) != nil || event.Type == "" {
			shutdown()
			return
		}
		if strings.HasPrefix(event.Type, "basispoints.") && event.Type != "basispoints.response.cancelled" {
			// Heartbeat, metadata and resume-token controls are housekeeping.
			// The resume token itself is bound to a request id that ended with
			// the previous generation and is not reusable without the official
			// resume protocol, which this pool deliberately does not attempt.
			// The cancelled alias is a real terminal understood by the body and
			// transformer; consuming it here would leave a generation hanging.
			continue
		}
		route := socket.route.Load()
		if route == nil {
			// A response event with no in-flight generation means unknown
			// protocol state; the official client would reconnect too.
			shutdown()
			return
		}
		select {
		case route.inbox <- raw:
		case <-route.done:
			// The request finished while this frame was in flight; drop it.
		}
	}
}

// idleCount reports pooled sockets for one key (tests and diagnostics).
func (p *bpsWSPool) idleCount(key string) int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.idle[key])
}

// closeAll drops every pooled socket (tests, shutdown).
func (p *bpsWSPool) closeAll() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// closeLocked mutates the map; iterate defensively over a snapshot.
	for key := range p.idle {
		for _, socket := range append([]*bpsWSSocket(nil), p.idle[key]...) {
			socket.closeLocked()
		}
	}
}
