package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

const (
	nativeWSHandshake  = 65
	nativeWSMessage    = 66
	nativeWSClose      = 67
	nativeWSError      = 68
	nativeWSEnd        = 69
	nativeWSAck        = 70
	nativeWSStart      = 71
	nativeWSChunk      = 72
	nativeWSFinish     = 73
	nativeWSSend       = 82
	nativeWSSendStart  = 85
	nativeWSSendChunk  = 86
	nativeWSSendFinish = 87
	nativeWSChunkSize  = 64 << 10
)

var (
	errNativeWSCapability         = errors.New("native WebSocket helper capability unavailable")
	errNativeWSProtocol           = errors.New("invalid native WebSocket IPC protocol")
	errNativeWSHandshake          = errors.New("native WebSocket handshake rejected")
	errNativeWSLimit              = errors.New("native WebSocket message limit exceeded")
	errNativeWSQueue              = errors.New("native WebSocket receive queue capacity exhausted")
	errNativeWSNoCompressionReady = errors.New("native helper WebSocket compression profile capability unavailable; upgrade helper")
)

type gatewayNativeWSCompressionDisabledKey struct{}

// Realtime's official connector does not negotiate permessage-deflate. This
// trusted local profile is independent from endpoint and payload size policy.
func withGatewayNativeWebSocketCompressionDisabled(ctx context.Context) context.Context {
	return context.WithValue(ctx, gatewayNativeWSCompressionDisabledKey{}, true)
}

func gatewayNativeWebSocketCompressionDisabled(ctx context.Context) bool {
	disabled, _ := ctx.Value(gatewayNativeWSCompressionDisabledKey{}).(bool)
	return disabled
}

type nativeWSPacket struct {
	typ     websocket.MessageType
	payload []byte
}

// The sole read pump demultiplexes write acknowledgements from decoded
// messages. No RFC6455 framing or compression state exists on the Go side.
type gatewayNativeWS struct {
	conn       net.Conn
	release    func()
	stopCancel func() bool
	once       sync.Once
	done       chan struct{}
	pumpDone   chan struct{}
	messages   chan nativeWSPacket
	acks       chan uint64
	writers    chan struct{}
	readers    chan struct{}
	mu         sync.Mutex
	err        error
	sequence   uint64
	waitingAck atomic.Uint64
	readLimit  atomic.Int64
	queued     atomic.Int64
	budgetWake chan struct{}
	unbounded  bool
}

func (p *Proxy) dialNativeGatewayWebSocket(ctx context.Context, headers http.Header, proxy *ProxyEntry) (gatewayWSUpstream, *http.Response, error) {
	return p.dialNativeGatewayWebSocketLifetime(ctx, ctx, headers, proxy)
}

// Handshake authorization/deadlines belong to the current admitted request.
// A pooled socket can instead use a service lifetime after its handshake.
func (p *Proxy) dialNativeGatewayWebSocketLifetime(ctx, lifetime context.Context, headers http.Header, proxy *ProxyEntry) (gatewayWSUpstream, *http.Response, error) {
	return p.dialNativeGatewayWebSocketEndpointLifetime(ctx, lifetime, headers, proxy, "/responses")
}

func (p *Proxy) dialNativeGatewayWebSocketEndpointLifetime(ctx, lifetime context.Context, headers http.Header, proxy *ProxyEntry, endpoint string) (gatewayWSUpstream, *http.Response, error) {
	if selected, err := gatewayWSModelEndpoint(endpoint); err != nil || selected != endpoint {
		return nil, nil, errNativeWSProtocol
	}
	target, err := gatewayUpstreamURL(p.gatewayInferenceBaseURL(ctx), endpoint, nil)
	if err != nil {
		return nil, nil, errNativeWSProtocol
	}
	return p.dialNativeGatewayWebSocketTargetLifetime(ctx, lifetime, headers, proxy, target)
}

// Only trusted internal endpoint builders may select this target. Their route
// allowlists remain at the protocol handler; no client URL enters this helper.
// The native pool key includes its origin, isolating each official service.
func (p *Proxy) dialNativeGatewayWebSocketTargetLifetime(ctx, lifetime context.Context, headers http.Header, proxy *ProxyEntry, target string) (gatewayWSUpstream, *http.Response, error) {
	pool := p.nativeHTTPFor(ctx)
	if pool == nil {
		return nil, nil, errNativeWSCapability
	}
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return nil, nil, errNativeWSProtocol
	}
	pairs, err := gatewayNativeRequestHeaders(headers)
	if err != nil {
		return nil, nil, err
	}
	key := gatewayNativeKey{accountScope: gatewayNativeAccountScope(ctx, headers), origin: u.Scheme + "://" + u.Host, timeout: 90 * time.Second}
	if proxy != nil {
		key.proxyID, key.proxyVersion, key.fingerprint = proxy.ID, proxy.Version, gatewayProxyFingerprint(*proxy)
	}
	checkProxy := func() error {
		if proxy != nil && proxy.ID != "" && p.Proxies != nil {
			current, ok := p.Proxies.Get(proxy.ID)
			if !ok || gatewayProxyFingerprint(*current) != key.fingerprint {
				return errGatewayProxyUnavailable
			}
		}
		return nil
	}
	if err := checkProxy(); err != nil {
		return nil, nil, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	child, err := pool.acquire(dialCtx, key, proxy)
	if err != nil {
		return nil, nil, err
	}
	released := false
	defer func() {
		if !released {
			pool.release(child)
		}
	}()
	unbounded := gatewayUnboundedPayload(ctx)
	disableCompression := gatewayNativeWebSocketCompressionDisabled(ctx)
	if disableCompression && !child.capabilities["websocket_no_compression_v1"] {
		return nil, nil, errNativeWSNoCompressionReady
	}
	if unbounded && (!child.capabilities["unbounded_payload_v1"] || !child.capabilities["websocket_message_v2"]) {
		return nil, nil, errGatewayNativeUnboundedReady
	}
	if !unbounded && !child.capabilities["websocket_message_v1"] {
		return nil, nil, errNativeWSCapability
	}
	if err := checkProxy(); err != nil {
		return nil, nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(dialCtx, pool.options.network, child.address)
	if err != nil {
		return nil, nil, nativeWSContextError(dialCtx, errGatewayNativeUnavailable)
	}
	keep := false
	defer func() {
		if !keep {
			_ = conn.Close()
		}
	}()
	stopDial := context.AfterFunc(dialCtx, func() { _ = conn.Close() })
	defer stopDial()
	if deadline, ok := dialCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	head := map[string]any{"protocol": 1, "ipc_token": child.token, "operation": "websocket_message_v1", "method": "GET",
		"path": u.RequestURI(), "headers": pairs, "body_length": 0, "max_message_bytes": maxBody, "write_timeout_ms": 60000}
	if disableCompression {
		head["disable_websocket_compression"] = true
	}
	if unbounded {
		head["operation"] = "websocket_message_v2"
		head["unbounded_payload"] = true
		delete(head, "max_message_bytes")
	}
	if err := checkGatewayNativeDispatch(dialCtx); err != nil {
		return nil, nil, err
	}
	if err := checkProxy(); err != nil {
		return nil, nil, err
	}
	if err := gatewayNativeWriteJSON(conn, head); err != nil {
		return nil, nil, nativeWSContextError(dialCtx, err)
	}
	kind, raw, err := readNativeWSFrame(conn, gatewayNativeBodyLimit)
	if err != nil {
		return nil, nil, nativeWSContextError(dialCtx, err)
	}
	if kind == 4 {
		return nil, nil, gatewayNativeRemoteError(raw)
	}
	if kind == nativeWSError {
		return nil, nil, nativeWSRemoteError(raw, false)
	}
	if kind != nativeWSHandshake {
		return nil, nil, errNativeWSProtocol
	}
	response, err := decodeNativeWSHandshake(raw)
	if err != nil {
		return nil, nil, err
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		return nil, response, errNativeWSHandshake
	}
	if !stopDial() || dialCtx.Err() != nil || ctx.Err() != nil || lifetime.Err() != nil {
		return nil, nil, nativeWSContextError(dialCtx, errGatewayNativeUnavailable)
	}
	_ = conn.SetDeadline(time.Time{})
	ws := newGatewayNativeWSWithPolicy(lifetime, conn, func() { pool.release(child) }, unbounded)
	keep, released = true, true
	return ws, response, nil
}

func decodeNativeWSHandshake(raw []byte) (*http.Response, error) {
	var head struct {
		Status        int             `json:"status"`
		Headers       [][]string      `json:"headers"`
		BodyScope     string          `json:"body_scope"`
		BodyComplete  json.RawMessage `json:"body_complete"`
		BodyBase64    string          `json:"body_base64"`
		TailTruncated bool            `json:"tail_truncated"`
	}
	if nativeWSDecodeMetadata(raw, &head) != nil || head.Status < 100 || head.Status > 599 || len(head.Headers) > 512 ||
		(head.BodyScope != "handshake_tail" && head.BodyScope != "none") || !bytes.Equal(bytes.TrimSpace(head.BodyComplete), []byte("null")) {
		return nil, errNativeWSProtocol
	}
	body, err := base64.StdEncoding.DecodeString(head.BodyBase64)
	if err != nil || len(body) > 16<<10 || ((head.Status == 101 || head.BodyScope == "none") && (len(body) != 0 || head.TailTruncated)) {
		return nil, errNativeWSProtocol
	}
	headers := make(http.Header)
	for _, pair := range head.Headers {
		if len(pair) != 2 {
			return nil, errNativeWSProtocol
		}
		value, err := base64.StdEncoding.DecodeString(pair[1])
		if err != nil || !gatewayNativeHeaderName(pair[0]) || !gatewayNativeHeaderValue(string(value)) {
			return nil, errNativeWSProtocol
		}
		headers.Add(pair[0], string(value))
	}
	return &http.Response{StatusCode: head.Status, Status: strconv.Itoa(head.Status) + " " + http.StatusText(head.Status),
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: headers, ContentLength: -1,
		Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func readNativeWSFrame(reader io.Reader, limit int64) (byte, []byte, error) {
	kind, size, err := readNativeWSFrameHeader(reader, limit)
	if err != nil {
		return 0, nil, err
	}
	raw := make([]byte, size)
	if err := nativeWSReadFull(reader, raw); err != nil {
		return 0, nil, err
	}
	return kind, raw, nil
}

func readNativeWSFrameHeader(reader io.Reader, limit int64) (byte, int64, error) {
	var prefix [5]byte
	if err := nativeWSReadFull(reader, prefix[:]); err != nil {
		return 0, 0, err
	}
	kind, size := prefix[0], int64(binary.BigEndian.Uint32(prefix[1:]))
	allowed := int64(gatewayNativeHeadLimit)
	if kind == nativeWSMessage {
		allowed = gatewayNativeBodyLimit + 9
	}
	if (kind != 4 && (kind < nativeWSHandshake || kind > nativeWSAck)) || size > allowed ||
		(kind == nativeWSEnd && size != 0) || (kind == nativeWSAck && size != 8) || (kind == nativeWSMessage && size < 9) {
		return 0, 0, errNativeWSProtocol
	}
	if kind == nativeWSMessage && size-9 > limit {
		return 0, 0, errNativeWSLimit
	}
	return kind, size, nil
}

func nativeWSReadFull(reader io.Reader, raw []byte) error {
	_, err := io.ReadFull(reader, raw)
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// encoding/json otherwise replaces invalid UTF-8 and unpaired UTF-16 escapes
// with U+FFFD. IPC metadata must retain the helper's exact Unicode strings.
func nativeWSDecodeMetadata(raw []byte, value any) error {
	if len(raw) > gatewayNativeHeadLimit || !utf8.Valid(raw) {
		return errNativeWSProtocol
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) || raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return errNativeWSProtocol
		}
		code, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil || code >= 0xdc00 && code <= 0xdfff {
			return errNativeWSProtocol
		}
		i += 4
		if code < 0xd800 || code > 0xdbff {
			continue
		}
		if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return errNativeWSProtocol
		}
		low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return errNativeWSProtocol
		}
		i += 6
	}
	if json.Unmarshal(raw, value) != nil {
		return errNativeWSProtocol
	}
	return nil
}

func nativeWSRemoteError(raw []byte, accepted bool) error {
	var event struct {
		Phase    string `json:"phase"`
		Code     string `json:"code"`
		Accepted *bool  `json:"accepted"`
	}
	phase := "handshake"
	if accepted {
		phase = "message"
	}
	if nativeWSDecodeMetadata(raw, &event) != nil || event.Accepted == nil || *event.Accepted != accepted || event.Phase != phase {
		return errNativeWSProtocol
	}
	switch event.Code {
	case "ws_handshake_timeout", "ws_write_timeout", "ws_ipc_write_timeout", "ws_ipc_read_timeout", "ws_close_timeout", "total_timeout":
		return gatewayNativeError(event.Code)
	case "ws_message_limit", "ws_frame_limit":
		return errNativeWSLimit
	case "ws_transport", "ws_connection_closed", "ws_unexpected_eof", "ws_ipc_read", "ws_ipc_write", "ws_output_incomplete":
		return errGatewayNativeUnavailable
	default:
		return errNativeWSProtocol
	}
}

func nativeWSContextError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func newGatewayNativeWS(ctx context.Context, conn net.Conn, release func()) *gatewayNativeWS {
	return newGatewayNativeWSWithPolicy(ctx, conn, release, false)
}

func newGatewayNativeWSWithPolicy(ctx context.Context, conn net.Conn, release func(), unbounded bool) *gatewayNativeWS {
	ws := &gatewayNativeWS{conn: conn, release: release, done: make(chan struct{}), pumpDone: make(chan struct{}),
		messages: make(chan nativeWSPacket, 16), acks: make(chan uint64, 1), writers: make(chan struct{}, 1), readers: make(chan struct{}, 1), budgetWake: make(chan struct{}, 1), unbounded: unbounded}
	ws.readLimit.Store(gatewayNativeBodyLimit)
	if unbounded {
		ws.readLimit.Store(-1)
	}
	ws.stopCancel = context.AfterFunc(ctx, func() { ws.fail(ctx.Err()) })
	go ws.pump()
	return ws
}

func (ws *gatewayNativeWS) fail(err error) {
	ws.once.Do(func() {
		ws.mu.Lock()
		ws.err = err
		ws.mu.Unlock()
		_ = ws.conn.Close()
		ws.release()
		close(ws.done)
	})
}

func (ws *gatewayNativeWS) failure() error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.err == nil {
		return net.ErrClosed
	}
	return ws.err
}

func (ws *gatewayNativeWS) pump() {
	defer close(ws.pumpDone)
	defer close(ws.messages)
	defer ws.stopCancel()
	var sequence uint64
	for {
		kind, raw, err := ws.readIncomingFrame()
		if err != nil {
			ws.fail(err)
			return
		}
		switch kind {
		case nativeWSMessage:
			sequence++
			typ := websocket.MessageType(raw[8])
			if sequence == 0 || binary.BigEndian.Uint64(raw[:8]) != sequence || (typ != websocket.MessageText && typ != websocket.MessageBinary) ||
				(typ == websocket.MessageText && !utf8.Valid(raw[9:])) {
				ws.fail(errNativeWSProtocol)
				ws.releaseMessageBudget(int64(len(raw) - 9))
				return
			}
			if limit := ws.readLimit.Load(); limit >= 0 && int64(len(raw)-9) > limit {
				ws.fail(errNativeWSLimit)
				ws.releaseMessageBudget(int64(len(raw) - 9))
				return
			}
			if err := ws.enqueueMessage(nativeWSPacket{typ, raw[9:]}); err != nil {
				ws.releaseMessageBudget(int64(len(raw) - 9))
				ws.fail(err)
				return
			}
		case nativeWSAck:
			id := binary.BigEndian.Uint64(raw)
			if id == 0 || !ws.waitingAck.CompareAndSwap(id, 0) {
				ws.fail(errNativeWSProtocol)
				return
			}
			select {
			case ws.acks <- id:
			default:
				ws.fail(errNativeWSProtocol)
				return
			}
		case nativeWSClose:
			var closeFrame struct {
				Code   *int    `json:"code"`
				Reason *string `json:"reason"`
			}
			if nativeWSDecodeMetadata(raw, &closeFrame) != nil || closeFrame.Reason == nil || len(*closeFrame.Reason) > 123 {
				ws.fail(errNativeWSProtocol)
				return
			}
			code := websocket.StatusNoStatusRcvd
			if closeFrame.Code != nil {
				if !nativeWSAllowedCloseCode(*closeFrame.Code) {
					ws.fail(errNativeWSProtocol)
					return
				}
				code = websocket.StatusCode(*closeFrame.Code)
			} else if *closeFrame.Reason != "" {
				ws.fail(errNativeWSProtocol)
				return
			}
			ws.fail(websocket.CloseError{Code: code, Reason: *closeFrame.Reason})
			return
		case nativeWSError:
			ws.fail(nativeWSRemoteError(raw, true))
			return
		default:
			ws.fail(errNativeWSProtocol)
			return
		}
	}
}

func (ws *gatewayNativeWS) enqueueMessage(message nativeWSPacket) error {
	select {
	case ws.messages <- message:
		return nil
	default:
	}
	// Most messages can be handed off immediately. Allocate a timer only when
	// the bounded queue actually applies backpressure.
	timer := time.NewTimer(gatewayWSWriteTimeout)
	defer timer.Stop()
	select {
	case ws.messages <- message:
		return nil
	case <-ws.done:
		return ws.failure()
	case <-timer.C:
		return errNativeWSQueue
	}
}

func (ws *gatewayNativeWS) notifyMessageBudget() {
	select {
	case ws.budgetWake <- struct{}{}:
	default:
	}
}

func (ws *gatewayNativeWS) releaseMessageBudget(size int64) {
	ws.queued.Add(-size)
	ws.notifyMessageBudget()
}

// Reserve payload capacity from its validated frame prefix before allocating
// or reading it. A legal large message waits for the reader, not an overflow.
func (ws *gatewayNativeWS) reserveMessageBudget(size int64) error {
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		if limit := ws.readLimit.Load(); limit >= 0 && size > limit {
			return errNativeWSLimit
		}
		select {
		case <-ws.done:
			return ws.failure()
		default:
		}
		current := ws.queued.Load()
		if ws.unbounded {
			// Queue backpressure is by complete-message slots, not a 32 MiB
			// aggregate byte ceiling. Still reject arithmetic overflow.
			if size > int64(^uint64(0)>>1)-current {
				return errNativeWSProtocol
			}
			if ws.queued.CompareAndSwap(current, current+size) {
				return nil
			}
			continue
		}
		if size <= gatewayNativeBodyLimit-current && ws.queued.CompareAndSwap(current, current+size) {
			return nil
		}
		if timer == nil {
			timer = time.NewTimer(gatewayWSWriteTimeout)
		}
		select {
		case <-ws.budgetWake:
		case <-ws.done:
			return ws.failure()
		case <-timer.C:
			return errNativeWSQueue
		}
	}
}

func (ws *gatewayNativeWS) readIncomingFrame() (byte, []byte, error) {
	if ws.unbounded {
		return ws.readIncomingV2Frame()
	}
	kind, size, err := readNativeWSFrameHeader(ws.conn, ws.readLimit.Load())
	if err != nil {
		return 0, nil, err
	}
	if kind == nativeWSMessage {
		if err := ws.reserveMessageBudget(size - 9); err != nil {
			return 0, nil, err
		}
	}
	raw := make([]byte, size)
	if err := nativeWSReadFull(ws.conn, raw); err != nil {
		if kind == nativeWSMessage {
			ws.releaseMessageBudget(size - 9)
		}
		return 0, nil, err
	}
	return kind, raw, nil
}

func readNativeWSV2Header(reader io.Reader) (byte, int, error) {
	var prefix [5]byte
	if err := nativeWSReadFull(reader, prefix[:]); err != nil {
		return 0, 0, err
	}
	kind, size := prefix[0], binary.BigEndian.Uint32(prefix[1:])
	valid := false
	switch kind {
	case nativeWSStart:
		valid = size == 17
	case nativeWSChunk:
		valid = size > 0 && size <= nativeWSChunkSize
	case nativeWSFinish, nativeWSAck:
		valid = size == 8
	case nativeWSClose, nativeWSError:
		valid = size <= gatewayNativeHeadLimit
	case nativeWSEnd:
		valid = size == 0
	}
	if !valid {
		return 0, 0, errNativeWSProtocol
	}
	return kind, int(size), nil
}

func (ws *gatewayNativeWS) readIncomingV2Frame() (byte, []byte, error) {
	kind, size, err := readNativeWSV2Header(ws.conn)
	if err != nil {
		return 0, nil, err
	}
	raw := make([]byte, size)
	if err := nativeWSReadFull(ws.conn, raw); err != nil {
		return 0, nil, err
	}
	if kind != nativeWSStart {
		if kind == nativeWSChunk || kind == nativeWSFinish {
			return 0, nil, errNativeWSProtocol
		}
		return kind, raw, nil
	}
	total := binary.BigEndian.Uint64(raw[9:])
	// No allocation follows a declared total. Validate platform arithmetic and
	// reserve accounting, then grow the buffer only as chunks actually arrive.
	if total > uint64(int(^uint(0)>>1)-9) || (raw[8] != byte(websocket.MessageText) && raw[8] != byte(websocket.MessageBinary)) {
		return 0, nil, errNativeWSProtocol
	}
	if err := ws.reserveMessageBudget(int64(total)); err != nil {
		return 0, nil, err
	}
	succeeded := false
	defer func() {
		if !succeeded {
			ws.releaseMessageBudget(int64(total))
		}
	}()
	sequence := binary.BigEndian.Uint64(raw[:8])
	message := append([]byte(nil), raw[:9]...)
	var chunk [nativeWSChunkSize]byte
	for {
		kind, size, err = readNativeWSV2Header(ws.conn)
		if err != nil {
			return 0, nil, err
		}
		if kind == nativeWSChunk {
			if uint64(size) > total-uint64(len(message)-9) {
				return 0, nil, errNativeWSProtocol
			}
			if err := nativeWSReadFull(ws.conn, chunk[:size]); err != nil {
				return 0, nil, err
			}
			message = append(message, chunk[:size]...)
			continue
		}
		if kind != nativeWSFinish {
			return 0, nil, errNativeWSProtocol
		}
		if err := nativeWSReadFull(ws.conn, chunk[:8]); err != nil {
			return 0, nil, err
		}
		if uint64(len(message)-9) != total || binary.BigEndian.Uint64(chunk[:8]) != sequence {
			return 0, nil, errNativeWSProtocol
		}
		succeeded = true
		return nativeWSMessage, message, nil
	}
}

func nativeWSWritePacket(writer io.Writer, kind byte, payload []byte) error {
	if len(payload) > nativeWSChunkSize {
		return errNativeWSProtocol
	}
	var prefix [5]byte
	prefix[0] = kind
	binary.BigEndian.PutUint32(prefix[1:], uint32(len(payload)))
	if err := nativeWSWriteAll(writer, prefix[:]); err != nil {
		return err
	}
	return nativeWSWriteAll(writer, payload)
}

func (ws *gatewayNativeWS) writeV2Message(typ websocket.MessageType, payload []byte) error {
	var start [17]byte
	binary.BigEndian.PutUint64(start[:8], ws.sequence)
	start[8] = byte(typ)
	binary.BigEndian.PutUint64(start[9:], uint64(len(payload)))
	if err := nativeWSWritePacket(ws.conn, nativeWSSendStart, start[:]); err != nil {
		return err
	}
	for len(payload) > 0 {
		size := min(len(payload), nativeWSChunkSize)
		if err := nativeWSWritePacket(ws.conn, nativeWSSendChunk, payload[:size]); err != nil {
			return err
		}
		payload = payload[size:]
	}
	return nativeWSWritePacket(ws.conn, nativeWSSendFinish, start[:8])
}

func nativeWSAllowedCloseCode(code int) bool {
	if code >= 3000 && code <= 4999 {
		return true
	}
	switch code {
	case 1000, 1001, 1002, 1003, 1007, 1008, 1009, 1010, 1011, 1012, 1013:
		return true
	}
	return false
}

func (ws *gatewayNativeWS) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	if err := ctx.Err(); err != nil {
		ws.fail(err)
		return 0, nil, err
	}
	select {
	case ws.readers <- struct{}{}:
		defer func() { <-ws.readers }()
	case <-ctx.Done():
		ws.fail(ctx.Err())
		return 0, nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		ws.fail(err)
		return 0, nil, err
	}
	select {
	case message, ok := <-ws.messages:
		if !ok {
			return 0, nil, ws.failure()
		}
		ws.releaseMessageBudget(int64(len(message.payload)))
		if limit := ws.readLimit.Load(); limit >= 0 && int64(len(message.payload)) > limit {
			ws.fail(errNativeWSLimit)
			return 0, nil, errNativeWSLimit
		}
		return message.typ, message.payload, nil
	case <-ctx.Done():
		ws.fail(ctx.Err())
		return 0, nil, ctx.Err()
	}
}

func (ws *gatewayNativeWS) Write(ctx context.Context, typ websocket.MessageType, payload []byte) error {
	if !ws.unbounded && len(payload) > gatewayNativeBodyLimit {
		return errNativeWSLimit
	}
	if (typ != websocket.MessageText && typ != websocket.MessageBinary) || (typ == websocket.MessageText && !utf8.Valid(payload)) {
		return errNativeWSProtocol
	}
	select {
	case ws.writers <- struct{}{}:
		defer func() { <-ws.writers }()
	case <-ctx.Done():
		ws.fail(ctx.Err())
		return ctx.Err()
	case <-ws.done:
		return ws.failure()
	}
	if err := ctx.Err(); err != nil {
		ws.fail(err)
		return err
	}
	select {
	case <-ws.done:
		return ws.failure()
	default:
	}
	ws.sequence++
	if ws.sequence == 0 {
		ws.fail(errNativeWSProtocol)
		return errNativeWSProtocol
	}
	ws.waitingAck.Store(ws.sequence)
	stop := context.AfterFunc(ctx, func() { ws.fail(ctx.Err()) })
	defer stop()
	if ws.unbounded {
		if err := ws.writeV2Message(typ, payload); err != nil {
			ws.fail(nativeWSContextError(ctx, err))
			return nativeWSContextError(ctx, ws.failure())
		}
	} else {
		var prefix [14]byte
		prefix[0] = nativeWSSend
		binary.BigEndian.PutUint32(prefix[1:5], uint32(len(payload)+9))
		binary.BigEndian.PutUint64(prefix[5:13], ws.sequence)
		prefix[13] = byte(typ)
		for _, part := range [][]byte{prefix[:], payload} {
			if err := nativeWSWriteAll(ws.conn, part); err != nil {
				ws.fail(nativeWSContextError(ctx, err))
				// The read pump may have closed IPC after receiving a typed helper
				// error while this write was blocked. Preserve that first failure.
				return nativeWSContextError(ctx, ws.failure())
			}
		}
	}
	select {
	case ack := <-ws.acks:
		if ack != ws.sequence {
			ws.fail(errNativeWSProtocol)
			return errNativeWSProtocol
		}
		return nil
	case <-ctx.Done():
		ws.fail(ctx.Err())
		return ctx.Err()
	case <-ws.done:
		select {
		case ack := <-ws.acks:
			if ack == ws.sequence {
				return nil
			}
		default:
		}
		return ws.failure()
	}
}

func nativeWSWriteAll(writer io.Writer, raw []byte) error {
	for len(raw) > 0 {
		n, err := writer.Write(raw)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(raw) {
			return io.ErrShortWrite
		}
		raw = raw[n:]
	}
	return nil
}

func (ws *gatewayNativeWS) CloseNow() error {
	ws.stopCancel()
	ws.fail(net.ErrClosed)
	<-ws.pumpDone
	// Explicit local close abandons unread output. Peer-close alone does not:
	// its preceding messages remain available until the caller closes or reads.
	for message := range ws.messages {
		ws.releaseMessageBudget(int64(len(message.payload)))
	}
	return nil
}

func (ws *gatewayNativeWS) SetReadLimit(limit int64) {
	if ws.unbounded && limit < 0 {
		limit = -1
	}
	if !ws.unbounded && (limit < 0 || limit > gatewayNativeBodyLimit) {
		limit = gatewayNativeBodyLimit
	}
	ws.readLimit.Store(limit)
	ws.notifyMessageBudget()
}
