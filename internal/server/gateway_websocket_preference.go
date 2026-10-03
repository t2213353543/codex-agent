package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

var errPreferredWebSocketUnavailable = errors.New("preferred WebSocket unavailable before generation")

// Call only at the handshake boundary, before any generation write. Refused
// credentials/policy/quota and malformed protocol replies do not authorize an
// alternate transport; an unsupported endpoint or failed connection does.
func gatewayWebSocketUnavailableBeforeGeneration(ctx context.Context, response *http.Response, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if response != nil {
		switch response.StatusCode {
		case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUpgradeRequired, http.StatusNotImplemented:
			return true
		default:
			return false
		}
	}
	var authorization *gatewayRecoveryAuthorizationError
	var owner *gatewayOwnerChangedError
	if errors.As(err, &authorization) || errors.As(err, &owner) || errors.Is(err, errGatewayProxyUnavailable) || errors.Is(err, context.Canceled) {
		return false
	}
	var network net.Error
	return errors.As(err, &network) || errors.Is(err, errGatewayNativeUnavailable) || errors.Is(err, errNativeWSCapability)
}

// Caller-owned/native Responses bodies already contain the intended profile.
// Prefer WS without adding a second model-context/tool prefix or prewarm turn.
func (p *Proxy) preferredResponsesWebSocket(req *http.Request, proxy *ProxyEntry, prepared []byte, settings Settings) (*http.Response, error) {
	route := gatewayWSAccountFrom(req.Context())
	availabilityKey := gatewayWSAvailabilityKey(route.ID, proxy, gatewayObservedRoute(req.URL.Path))
	if p.wsAvailability.blocked(availabilityKey) || p.wsAvailability.dialLimited(availabilityKey) {
		// Known-wasted dial for this account and egress; keep the shared
		// HTTP fallback.
		return nil, errPreferredWebSocketUnavailable
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(prepared, &body) != nil || body == nil {
		return nil, errNativeWSProtocol
	}
	// Codex 0.155 ResponseCreateWsRequest retains stream. HTTP-only handshake
	// metadata, however, rides the per-frame client_metadata object on WS.
	var metadata map[string]json.RawMessage
	if raw := body["client_metadata"]; len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if json.Unmarshal(raw, &metadata) != nil {
			return nil, errNativeWSProtocol
		}
	}
	if metadata == nil {
		metadata = make(map[string]json.RawMessage)
	}
	for header, field := range map[string]string{"x-openai-internal-codex-responses-lite": "ws_request_header_x_openai_internal_codex_responses_lite", "x-codex-turn-state": "x-codex-turn-state"} {
		if value := req.Header.Get(header); value != "" {
			metadata[field], _ = json.Marshal(value)
		}
	}
	if len(metadata) > 0 {
		body["client_metadata"], _ = marshalGatewayRequestJSON(metadata)
	}
	body["type"] = json.RawMessage(`"response.create"`)
	wire, err := marshalGatewayRequestJSON(body)
	if err != nil {
		return nil, err
	}
	headers := req.Header.Clone()
	for _, name := range []string{"Accept", "Content-Type", "Content-Length", "Content-Encoding", "Connection", "Upgrade", "x-openai-internal-codex-responses-lite", "x-codex-turn-state"} {
		headers.Del(name)
	}
	headers.Set("OpenAI-Beta", "responses_websockets=2026-02-06")
	dialCtx, cancelDial := context.WithTimeout(req.Context(), time.Duration(settings.UpstreamResponseHeaderTimeoutSeconds)*time.Second)
	defer cancelDial()
	var conn gatewayWSUpstream
	var handshake *http.Response
	useNative := p.nativeWebSocketFor(req.Context())
	if useNative {
		conn, handshake, err = p.dialNativeGatewayWebSocketTargetLifetime(dialCtx, req.Context(), headers, proxy, req.URL.String())
	}
	if !useNative || errors.Is(err, errGatewayNativeUnboundedReady) {
		if err = checkGatewayNativeDispatch(dialCtx); err == nil {
			var concrete *websocket.Conn
			concrete, handshake, err = websocket.Dial(dialCtx, req.URL.String(), &websocket.DialOptions{
				HTTPClient: p.gatewayRequestClient(req.Context(), proxy, gatewayClientWebSocket), HTTPHeader: headers,
				CompressionMode: websocket.CompressionContextTakeover})
			if concrete != nil {
				conn = concrete
			}
		}
	}
	observePreferredWebSocketHandshake(req.Context(), handshake)
		if err != nil || conn == nil || handshake == nil || handshake.StatusCode != http.StatusSwitchingProtocols {
			if conn != nil {
				_ = conn.CloseNow()
			}
			if gatewayWebSocketUnavailableBeforeGeneration(req.Context(), handshake, err) {
				p.wsAvailability.observeUnavailable(availabilityKey)
				return handshake, errors.Join(errPreferredWebSocketUnavailable, err)
			}
			if handshake != nil && handshake.StatusCode == http.StatusTooManyRequests {
				// Capacity refusal at the connection level; pause this pairing's
				// dials briefly while the request itself keeps its HTTP recovery.
				p.wsAvailability.observeHandshakeLimited(availabilityKey)
			}
			if handshake != nil && handshake.StatusCode >= 300 {
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
	if gatewayUnboundedPayload(req.Context()) {
		conn.SetReadLimit(-1)
	} else {
		conn.SetReadLimit(maxBody)
	}
	if err := checkGatewayNativeDispatch(req.Context()); err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	if err := gatewayWSWrite(req.Context(), conn, websocket.MessageText, wire); err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	responseHeaders := gatewayHTTPResponseView(handshake).Header
	for name := range responseHeaders {
		if strings.HasPrefix(strings.ToLower(name), "sec-websocket-") {
			responseHeaders.Del(name)
		}
	}
	responseHeaders.Del("Content-Length")
	responseHeaders.Del("Content-Encoding")
	responseHeaders.Set("Content-Type", "text/event-stream")
	stream := &gatewayPreferredWebSocketBody{conn: conn, ctx: req.Context()}
	stop := context.AfterFunc(req.Context(), func() { _ = stream.Close() })
	p.wsAvailability.observeEstablished(availabilityKey)
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: responseHeaders, ContentLength: -1, Request: req,
		Body: &idleStreamBody{body: stream, cancel: func() { stop() }, idle: time.Duration(settings.UpstreamStreamIdleTimeoutSeconds) * time.Second, sse: true}}, nil
}

type gatewayPreferredWebSocketBody struct {
	conn    gatewayWSUpstream
	ctx     context.Context
	once    sync.Once
	pending []byte
	done    bool
}

func (b *gatewayPreferredWebSocketBody) Read(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	if len(b.pending) == 0 {
		if b.done {
			return 0, io.EOF
		}
		typ, raw, err := b.conn.Read(b.ctx)
		if err != nil {
			if errors.Is(err, io.EOF) || websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		var kind string
		if typ != websocket.MessageText || gatewayWSDecodeExact(raw, map[string]any{"type": &kind}) != nil || kind == "" {
			return 0, errNativeWSProtocol
		}
		for _, line := range bytes.Split(raw, []byte{'\n'}) {
			b.pending = append(b.pending, "data: "...)
			b.pending = append(b.pending, line...)
			b.pending = append(b.pending, '\n')
		}
		b.pending = append(b.pending, '\n')
		switch kind {
		case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "error":
			b.done = true
			_ = b.Close()
		}
	}
	n := copy(dst, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func observePreferredWebSocketHandshake(ctx context.Context, response *http.Response) {
	attempt, _ := ctx.Value(gatewayTransportAttemptKey{}).(*gatewayAttemptHandle)
	if attempt == nil || attempt.collector == nil {
		return
	}
	collector := attempt.collector
	collector.mu.Lock()
	valid := attempt.index >= 0 && attempt.index < collector.count && attempt.index < len(collector.attempts)
	if valid && !collector.attempts[attempt.index].finished {
		collector.attempts[attempt.index].summary.Phase = "websocket"
	}
	collector.mu.Unlock()
	if valid && response != nil {
		attempt.Headers(response.StatusCode, time.Now())
	}
}

func (b *gatewayPreferredWebSocketBody) Close() error {
	b.once.Do(func() { _ = b.conn.CloseNow() })
	return nil
}
