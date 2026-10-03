package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const bpsWebSocketClientInfo = `{"product":"basispoints-powerpoint-plugin","platform":"powerpoint","agent_profile":"powerpoint","editor":"powerpoint","host":"office","runtime":"web","client_platform":"web"}`

// The BPS protocol accepts the fully prepared BPS request, not a raw Codex WS
// frame. History expansion, local warmup and tool policy remain in gatewayBPS.
func (p *Proxy) bpsWebSocketRequest(req *http.Request, proxy *ProxyEntry, prepared []byte, settings Settings) (*http.Response, error) {
	route := gatewayWSAccountFrom(req.Context())
	availabilityKey := gatewayWSAvailabilityKey(route.ID, proxy, gatewayObservedRoute(req.URL.Path))
	if p.wsAvailability.blocked(availabilityKey) || p.wsAvailability.dialLimited(availabilityKey) {
		// A recently refused (or handshake-rate-limited) dial is a known-wasted
		// round trip for this account and egress; report the same pre-generation
		// unavailability the real refusal would produce so callers keep their
		// HTTP fallback.
		return nil, errPreferredWebSocketUnavailable
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(prepared, &body) != nil || body == nil {
		return nil, errNativeWSProtocol
	}
	requestID, resumeToken := bpsUUID(), "resume_"+strings.ReplaceAll(bpsUUID(), "-", "")
	body["type"] = json.RawMessage(`"response.create"`)
	body["basispoints_request_id"], _ = json.Marshal(requestID)
	body["basispoints_resume_token"], _ = json.Marshal(resumeToken)
	wire, err := marshalGatewayRequestJSON(body)
	if err != nil {
		return nil, err
	}
	// The official client reuses one socket sequentially across generations
	// (single in-flight per socket, rotated after 4 idle minutes or 8
	// completed requests, affinity kept for the socket's life). When reuse is
	// enabled, a healthy idle socket skips the dial entirely.
	reuseEnabled := settings.PreferWebsocket && settings.BPSWebSocketReuse
	poolKey := bpsWSPoolKey(route, proxy)
	if !reuseEnabled {
		poolKey = ""
	}
	var socket *bpsWSSocket
	var exchangeRoute *bpsWSSocketRoute
	if reuseEnabled {
		socket = p.bpsWSSockets.checkout(poolKey)
		if socket != nil {
			exchangeRoute = &bpsWSSocketRoute{inbox: make(chan []byte, bpsWSInboxBuffer), done: make(chan struct{})}
			socket.attach(exchangeRoute)
		}
	}
	var lifetime context.Context
	var stopLifetime context.CancelFunc
	var conn gatewayWSUpstream
	var handshake *http.Response
	var affinity string
	if socket == nil {
		target := *req.URL
		query := target.Query()
		query.Set("bps_client_info", bpsWebSocketClientInfo)
		query.Set("bps_auth_mode", "chatgpt")
		affinity = "affinity_" + strings.ReplaceAll(bpsUUID(), "-", "")
		query.Set("bps_ws_affinity", affinity)
		query.Set("bps_control_frames", "upstream_sent,heartbeat,resume,cancel,server_draining")
		target.RawQuery = query.Encode()
		headers := req.Header.Clone()
		for _, name := range []string{"Content-Type", "Content-Length", "Content-Encoding", "Accept", "Connection", "Upgrade"} {
			headers.Del(name)
		}
		token := strings.TrimPrefix(headers.Get("Authorization"), "Bearer ")
		if token == "" || !gatewayNativeHeaderValue(token) {
			return nil, errNativeWSProtocol
		}
		protocols := []string{"responses", "openai-bearer." + token}
		headers.Set("Sec-WebSocket-Protocol", strings.Join(protocols, ", "))
		headers.Set("Origin", target.Scheme+"://"+target.Host)
		// Keep the socket alive briefly on request cancellation so BPS receives its
		// explicit cancellation control before the connection is closed.
		lifetime, stopLifetime = context.WithCancel(context.WithoutCancel(req.Context()))
		dialPending := true
		defer func() {
			if dialPending {
				stopLifetime()
			}
		}()
		dialCtx, stopDial := context.WithTimeout(req.Context(), time.Duration(settings.UpstreamResponseHeaderTimeoutSeconds)*time.Second)
		useNative := p.nativeWebSocketFor(req.Context())
		if useNative {
			conn, handshake, err = p.dialNativeGatewayWebSocketTargetLifetime(dialCtx, lifetime, headers, proxy, target.String())
		}
		if !useNative || errors.Is(err, errGatewayNativeUnboundedReady) {
			// Older native helpers cannot frame unbounded BPS messages. That
			// capability failure precedes upstream IPC: keep actual WS transport
			// with the same proxy and headers, without lowering payload limits.
			headers.Del("Sec-WebSocket-Protocol")
			if err = checkGatewayNativeDispatch(dialCtx); err == nil {
				var websocketConn *websocket.Conn
				websocketConn, handshake, err = websocket.Dial(dialCtx, target.String(), &websocket.DialOptions{
					HTTPClient: p.gatewayRequestClient(req.Context(), proxy, gatewayClientWebSocket),
					HTTPHeader: headers, Subprotocols: protocols, CompressionMode: websocket.CompressionDisabled,
				})
				if websocketConn != nil {
					conn = websocketConn
				}
			}
		}
		stopDial()
		if handshake != nil {
			if attempt, _ := req.Context().Value(gatewayTransportAttemptKey{}).(*gatewayAttemptHandle); attempt != nil {
				attempt.Headers(handshake.StatusCode, time.Now())
			}
		}
		if err != nil || conn == nil || handshake == nil || handshake.StatusCode != http.StatusSwitchingProtocols {
			if conn != nil {
				_ = conn.CloseNow()
			}
			if gatewayWebSocketUnavailableBeforeGeneration(req.Context(), handshake, err) {
				p.wsAvailability.observeUnavailable(availabilityKey)
				return handshake, errors.Join(errPreferredWebSocketUnavailable, err)
			}
			if handshake != nil && handshake.StatusCode == http.StatusTooManyRequests {
				// The upstream refused the connection itself. The request keeps
				// its normal HTTP recovery; the pairing just pauses further
				// dials briefly instead of paying this refusal on every request
				// while the pool stays empty.
				p.wsAvailability.observeHandshakeLimited(availabilityKey)
			}
			if handshake != nil && handshake.StatusCode >= 300 {
				// Only a rejected handshake reaches normal HTTP auth recovery. No
				// generation frame has been written at this point.
				return handshake, nil
			}
			if err == nil {
				err = errNativeWSProtocol
			}
			return nil, err
		}
		if handshake.Body != nil {
			_ = handshake.Body.Close()
		}
		if handshake.Header.Get("Sec-WebSocket-Protocol") != "responses" {
			_ = conn.CloseNow()
			return nil, errNativeWSProtocol
		}
		dialPending = false
		conn.SetReadLimit(-1)
		if reuseEnabled {
			exchangeRoute = &bpsWSSocketRoute{inbox: make(chan []byte, bpsWSInboxBuffer), done: make(chan struct{})}
			socket = p.bpsWSSockets.adopt(conn, stopLifetime, poolKey, affinity, exchangeRoute)
		}
		// The existing authoritative pre-dispatch guard also covers the window
		// between the handshake and the first model frame.
		if err := checkGatewayNativeDispatch(req.Context()); err != nil {
			if socket != nil {
				p.bpsWSSockets.discard(socket)
			} else {
				stopLifetime()
				_ = conn.CloseNow()
			}
			return nil, err
		}
	}
	if err := gatewayWSWrite(req.Context(), socketConn(socket, conn), websocket.MessageText, wire); err != nil {
		if socket != nil {
			p.bpsWSSockets.discard(socket)
		} else {
			stopLifetime()
			_ = conn.CloseNow()
		}
		return nil, err // Ambiguous generation writes never fall back or replay.
	}
	var responseHeaders http.Header
	if handshake != nil {
		responseHeaders = gatewayHTTPResponseView(handshake).Header
	} else {
		responseHeaders = http.Header{}
	}
	for name := range responseHeaders {
		if strings.HasPrefix(strings.ToLower(name), "sec-websocket-") {
			responseHeaders.Del(name)
		}
	}
	responseHeaders.Del("Content-Encoding")
	responseHeaders.Del("Content-Length")
	responseHeaders.Set("Content-Type", "text/event-stream")
	responseHeaders.Set("Cache-Control", "no-cache")
	exchange := &bpsWebSocketBody{conn: socketConn(socket, conn), lifetime: lifetime, stopLifetime: stopLifetime,
		requestID: requestID, resumeToken: resumeToken, pool: p.bpsWSPoolPointer(), socket: socket, reuse: socket != nil}
	if socket != nil && exchangeRoute != nil {
		exchange.inbox = exchangeRoute.inbox
		exchange.requestDone = exchangeRoute.done
		exchange.dead = socket.deadCh
	}
	stopRequest := context.AfterFunc(req.Context(), func() { _ = exchange.Close() })
	response := &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: responseHeaders,
		Body:          &idleStreamBody{body: exchange, cancel: func() { stopRequest() }, idle: time.Duration(settings.UpstreamStreamIdleTimeoutSeconds) * time.Second, sse: true},
		ContentLength: -1, Request: req}
	p.wsAvailability.observeEstablished(availabilityKey)
	return response, nil
}

func socketConn(socket *bpsWSSocket, conn gatewayWSUpstream) gatewayWSUpstream {
	if socket != nil {
		return socket.conn
	}
	return conn
}

func (p *Proxy) bpsWSPoolPointer() *bpsWSPool {
	return &p.bpsWSSockets
}

// Read pulls one complete WS event at a time. It inherits HTTP backpressure and
// the same idle budget; no producer can buffer an unbounded response in memory.
type bpsWebSocketBody struct {
	conn         gatewayWSUpstream
	lifetime     context.Context
	stopLifetime context.CancelFunc
	requestID    string
	mu           sync.Mutex
	resumeToken  string
	terminal     atomic.Bool
	reusable     atomic.Bool
	once         sync.Once
	pending      []byte // Owned by the sole body reader.
	// Pool state: nil keeps the historical per-request close semantics. A
	// pooled socket delivers frames through its lifelong reader's inbox;
	// requestDone retires the routing when this generation ends, and dead is
	// closed by the pool when the socket itself dies.
	pool        *bpsWSPool
	socket      *bpsWSSocket
	inbox       chan []byte
	dead        chan struct{}
	requestDone chan struct{}
	reuse       bool
}

// frameSource yields the next upstream frame for either transport shape: a
// pooled socket's reader inbox or a direct connection read. It reports socket
// death as an unexpected-EOF error, matching the unpooled behaviour; the idle
// stream timeout above this body bounds how long a silent wait may last.
func (b *bpsWebSocketBody) frame() ([]byte, error) {
	if !b.reuse {
		_, raw, err := b.conn.Read(b.readContext())
		if err != nil {
			if errors.Is(err, io.EOF) || websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		return raw, nil
	}
	select {
	case raw := <-b.inbox:
		return raw, nil
	case <-b.dead:
		// The lifelong reader may queue the terminal and then observe peer
		// closure before this consumer runs. Socket death must not discard an
		// already received frame; no read, wait or generation replay is needed.
		select {
		case raw := <-b.inbox:
			return raw, nil
		default:
			return nil, io.ErrUnexpectedEOF
		}
	case <-b.requestDone:
		return nil, context.Canceled
	}
}

func (b *bpsWebSocketBody) Read(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	for len(b.pending) == 0 {
		if b.terminal.Load() {
			if b.pool != nil {
				// The consumer drained the terminal event; hand the healthy
				// socket back exactly once, at end-of-stream.
				b.dispose()
			}
			return 0, io.EOF
		}
		raw, err := b.frame()
		if err != nil {
			return 0, err
		}
		var event struct {
			Type        string `json:"type"`
			RequestID   string `json:"request_id"`
			ResumeToken string `json:"resume_token"`
		}
		if json.Unmarshal(raw, &event) != nil || event.Type == "" {
			return 0, errNativeWSProtocol
		}
		if event.Type == "basispoints.response.resume_token" {
			if event.RequestID == b.requestID && event.ResumeToken != "" {
				b.mu.Lock()
				b.resumeToken = event.ResumeToken
				b.mu.Unlock()
			}
			continue // Recovery credentials never enter public events or logs.
		}
		if strings.HasPrefix(event.Type, "basispoints.") && event.Type != "basispoints.response.cancelled" && event.Type != "basispoints.response.metadata" {
			continue
		}
		for _, line := range bytes.Split(raw, []byte{'\n'}) {
			b.pending = append(b.pending, "data: "...)
			b.pending = append(b.pending, line...)
			b.pending = append(b.pending, '\n')
		}
		b.pending = append(b.pending, '\n')
		switch event.Type {
		case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "basispoints.response.cancelled", "error":
			// The official client closes the socket after an explicit cancel
			// and rotates on a draining signal; other terminals leave a
			// healthy socket reusable for the next sequential generation.
			reusable := event.Type != "response.cancelled" && event.Type != "basispoints.response.cancelled" &&
				!strings.Contains(event.Type, "server_draining") && !bytes.Contains(raw, []byte("server_draining"))
			b.terminal.Store(true)
			b.reusable.Store(reusable)
			if b.pool == nil {
				b.dispose()
			}
		}
	}
	n := copy(dst, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

// readContext picks the read context: a pooled socket keeps its own dial-time
// lifetime (b.lifetime is nil because the request never dialed).
func (b *bpsWebSocketBody) readContext() context.Context {
	if b.lifetime != nil {
		return b.lifetime
	}
	return context.Background()
}

// dispose runs the single final disposition of the upstream exchange: an
// unacknowledged generation sends the official cancellation control first,
// and a pooled socket is returned only after a reusable terminal was fully
// delivered. Everything else closes the socket.
func (b *bpsWebSocketBody) dispose() {
	b.once.Do(func() {
		if b.requestDone != nil {
			close(b.requestDone)
		}
		b.mu.Lock()
		token := b.resumeToken
		pending := len(b.pending) > 0
		b.mu.Unlock()
		if !b.terminal.Load() {
			control, _ := json.Marshal(map[string]string{"type": "basispoints.response.cancel", "request_id": b.requestID, "resume_token": token})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = b.conn.Write(ctx, websocket.MessageText, control)
			cancel()
		}
		if b.pool != nil && b.socket != nil {
			if b.terminal.Load() && b.reusable.Load() && !pending {
				b.pool.release(b.socket)
			} else {
				b.pool.discard(b.socket)
			}
			return
		}
		if b.stopLifetime != nil {
			b.stopLifetime()
		}
		_ = b.conn.CloseNow()
	})
}

func (b *bpsWebSocketBody) Close() error {
	b.dispose()
	return nil
}
