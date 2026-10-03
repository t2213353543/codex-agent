package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// appServerTransport ports the network exchange only. An admitted request with
// an outer completion marker can lease a credential/route/session-bound WSS
// connection. Calls without that marker retain the private-connection behavior.
// Neither path retains conversation bodies or executes client tools.
type appServerTransport struct {
	dial        func(context.Context, http.Header, *ProxyEntry) (gatewayWSUpstream, *http.Response, error)
	now         func() time.Time
	mu          sync.Mutex
	closed      bool
	active      map[*appServerTransportExchange]struct{}
	connections *appServerConnectionPool
}

// Callers may consider HTTP fallback only when GenerationSent is false.
// A failed generation write is ambiguous and must never trigger replay.
type appServerTransportError struct {
	Stage          string
	GenerationSent bool
	Err            error
}

func (e *appServerTransportError) Error() string {
	return "App Server transport " + e.Stage + " failed"
}
func (e *appServerTransportError) Unwrap() error { return e.Err }

var errAppServerTransportClosed = errors.New("App Server transport closed")
var errAppServerTransportProtocol = errors.New("invalid App Server transport response")
var errAppServerTransportTerminal = errors.New("App Server upstream returned terminal error")
var errAppServerWSHandshakeUnavailable = errors.New("App Server WebSocket handshake unavailable before generation")

type appServerTransportExchange struct {
	owner  *appServerTransport
	conn   gatewayWSUpstream
	cancel context.CancelFunc
	once   sync.Once
}

func (e *appServerTransportExchange) close() {
	e.once.Do(func() {
		e.cancel()
		_ = e.conn.CloseNow()
		e.owner.mu.Lock()
		delete(e.owner.active, e)
		e.owner.mu.Unlock()
	})
}
func newAppServerTransport(p *Proxy) *appServerTransport {
	return &appServerTransport{dial: p.dialNativeGatewayWebSocket, now: time.Now, active: make(map[*appServerTransportExchange]struct{}),
		connections: newAppServerConnectionPool(p)}
}
func (t *appServerTransport) Close() error {
	if t.connections != nil {
		t.connections.close()
	}
	t.mu.Lock()
	t.closed = true
	pending := make([]*appServerTransportExchange, 0, len(t.active))
	for exchange := range t.active {
		pending = append(pending, exchange)
	}
	t.mu.Unlock()
	for _, exchange := range pending {
		exchange.close()
	}
	return nil
}

type appServerTransportBody struct {
	*io.PipeReader
	exchange *appServerTransportExchange
}

func (b *appServerTransportBody) Close() error {
	err := b.PipeReader.Close()
	b.exchange.close()
	return err
}

// req carries an admitted credential/route snapshot, never the incoming client
// bearer. Non-101 handshake responses are returned intact with nil error so the
// existing account boundary observes actual 401/429/5xx. Prewarm fails before
// publishing a body; once generation Write is attempted, replay is prohibited.
func (t *appServerTransport) RoundTripPrepared(req *http.Request, proxy *ProxyEntry, in appServerRequestInput) (*http.Response, error) {
	if req == nil {
		return nil, &appServerTransportError{Stage: "request", Err: errAppServerTransportProtocol}
	}
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return nil, &appServerTransportError{Stage: "closed", Err: errAppServerTransportClosed}
	}
	if result, _ := req.Context().Value(appServerConnectionResultKey{}).(*appServerConnectionResult); result != nil && t.connections != nil &&
		t.connections.owner.nativeHTTPFor(req.Context()) != nil {
		return t.connections.roundTrip(req, proxy, in, result)
	}
	full, err := buildAppServerRequest(in)
	if err != nil {
		return nil, &appServerTransportError{Stage: "request", Err: err}
	}
	// Startup warmup includes only writer-generated tools/base instructions.
	warmInput, err := appServerPrewarmProfile(in)
	if err != nil {
		return nil, &appServerTransportError{Stage: "prewarm_prepare", Err: err}
	}
	warmInput.Input = nil
	// Startup prewarm uses official auto tool choice. A caller's required or
	// named choice applies only to generation; the stable-property comparison
	// below then correctly selects a full request instead of a continuation.
	if _, explicit := in.ClientControls["tool_choice"]; explicit {
		warmInput.ClientControls = make(map[string]json.RawMessage, len(in.ClientControls))
		for key, value := range in.ClientControls {
			if key != "tool_choice" {
				warmInput.ClientControls[key] = value
			}
		}
	}
	warm, err := buildAppServerRequest(warmInput)
	if err != nil {
		return nil, &appServerTransportError{Stage: "request", Err: err}
	}
	state := appServerWebSocketRequestState{Warmup: true, StreamRequestStartMS: t.now().UnixMilli(),
		RoutingHint: req.Header.Get("x-codex-routing-hint"), Traceparent: req.Header.Get("traceparent"), Tracestate: req.Header.Get("tracestate")}
	if state.RoutingHint == "" {
		var fields struct {
			Model       string `json:"model"`
			ServiceTier string `json:"service_tier"`
		}
		if err = json.Unmarshal(full.Body, &fields); err != nil {
			return nil, &appServerTransportError{Stage: "request", Err: err}
		}
		state.RoutingHint = "model=" + fields.Model
		if fields.ServiceTier != "" {
			state.RoutingHint += ";tier=" + fields.ServiceTier
		}
	}
	warmWS, err := buildAppServerWebSocketRequest(warmInput, state)
	if err != nil {
		return nil, &appServerTransportError{Stage: "request", Err: err}
	}
	headers := req.Header.Clone()
	for name, values := range warmWS.Headers {
		headers[name] = append([]string(nil), values...)
	}
	// Writer ExtraHeaders may never replace admitted account credentials.
	for _, name := range []string{"Authorization", "ChatGPT-Account-Id"} {
		headers.Del(name)
		for _, value := range req.Header.Values(name) {
			headers.Add(name, value)
		}
	}
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Encoding", "Accept-Encoding", "Accept",
		"Connection", "Upgrade", "Host", "Transfer-Encoding", "Sec-WebSocket-Key", "Sec-WebSocket-Version",
		"Sec-WebSocket-Accept", "Sec-WebSocket-Extensions", "x-openai-internal-codex-responses-lite", "x-codex-turn-state"} {
		headers.Del(name)
	}
	ctx, cancel := context.WithCancel(req.Context())
	conn, handshake, err := t.dial(ctx, headers, proxy)
	if err != nil || conn == nil {
		cancel()
		if conn != nil {
			_ = conn.CloseNow()
		}
		if handshake != nil && handshake.StatusCode != http.StatusSwitchingProtocols {
			handshake.Request = req
			if handshake.StatusCode == http.StatusForbidden || handshake.StatusCode == http.StatusUpgradeRequired {
				return handshake, &appServerTransportError{Stage: "handshake", Err: errAppServerWSHandshakeUnavailable}
			}
			return handshake, nil
		}
		if err == nil {
			err = errAppServerTransportProtocol
		}
		return nil, &appServerTransportError{Stage: "handshake", Err: err}
	}
	if gatewayUnboundedPayload(req.Context()) {
		conn.SetReadLimit(-1)
	} else {
		conn.SetReadLimit(maxBody)
	}
	exchange := &appServerTransportExchange{owner: t, conn: conn, cancel: cancel}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		exchange.close()
		return nil, &appServerTransportError{Stage: "closed", Err: errAppServerTransportClosed}
	}
	t.active[exchange] = struct{}{}
	t.mu.Unlock()
	keep := false
	defer func() {
		if !keep {
			exchange.close()
		}
	}()
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.CloseNow() })
	defer func() {
		if !keep {
			stopCancel()
		}
	}()
	if handshake != nil && handshake.Body != nil {
		_ = handshake.Body.Close()
	}
	turnState := in.TurnState
	if turnState == "" && handshake != nil {
		turnState = handshake.Header.Get("x-codex-turn-state")
	}
	// Official OnceLock semantics keep the first turn-state token.
	warmInput.TurnState = turnState
	warmWS, err = buildAppServerWebSocketRequest(warmInput, state)
	if err != nil {
		return nil, &appServerTransportError{Stage: "prewarm_prepare", Err: err}
	}
	if err = gatewayWSWrite(ctx, conn, websocket.MessageText, warmWS.Body); err != nil {
		return nil, &appServerTransportError{Stage: "prewarm_write", Err: err}
	}
	prior, err := appServerReadWarmup(ctx, conn, &turnState)
	if err != nil {
		return nil, &appServerTransportError{Stage: "prewarm_read", Err: err}
	}
	in.TurnState = turnState
	state.Warmup = false
	state.StreamRequestStartMS = t.now().UnixMilli()
	if delta, ok := appServerInputDelta(warm.Body, full.Body, prior.outputHashes); ok && prior.responseID != "" {
		state.PreviousResponseID, state.InputDelta = &prior.responseID, &delta
	}
	request, err := buildAppServerWebSocketRequest(in, state)
	if err != nil {
		return nil, &appServerTransportError{Stage: "generation_prepare", Err: err}
	}
	if err = gatewayWSWrite(ctx, conn, websocket.MessageText, request.Body); err != nil {
		return nil, &appServerTransportError{Stage: "generation_write", GenerationSent: true, Err: err}
	}
	reader, writer := io.Pipe()
	body := &appServerTransportBody{PipeReader: reader, exchange: exchange}
	responseHeaders := make(http.Header)
	if handshake != nil {
		copyResponseHeaders(responseHeaders, handshake.Header)
	}
	for name := range responseHeaders {
		if strings.HasPrefix(strings.ToLower(name), "sec-websocket-") {
			responseHeaders.Del(name)
		}
	}
	responseHeaders.Del("Content-Length")
	responseHeaders.Del("Content-Encoding")
	responseHeaders.Set("Content-Type", "text/event-stream")
	responseHeaders.Set("Cache-Control", "no-cache")
	if turnState != "" {
		responseHeaders.Set("x-codex-turn-state", turnState)
	}
	stopPipe := context.AfterFunc(ctx, func() { _ = writer.CloseWithError(ctx.Err()) })
	go func() {
		err := appServerPumpSSE(ctx, conn, writer)
		stopPipe()
		stopCancel()
		if err != nil {
			_ = writer.CloseWithError(&appServerTransportError{Stage: "generation_read", GenerationSent: true, Err: err})
		} else {
			_ = writer.Close()
		}
		exchange.close()
	}()
	keep = true
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: responseHeaders, Body: body, ContentLength: -1, Request: req}, nil
}

type appServerWarmupResponse struct {
	responseID   string
	outputHashes [][32]byte
}
type appServerTransportEvent struct {
	Type     string                     `json:"type"`
	Headers  map[string]json.RawMessage `json:"headers"`
	Item     json.RawMessage            `json:"item"`
	Response struct {
		ID     string            `json:"id"`
		Output []json.RawMessage `json:"output"`
	} `json:"response"`
}

func appServerReadWarmup(ctx context.Context, conn gatewayWSUpstream, turnState *string) (appServerWarmupResponse, error) {
	result := appServerWarmupResponse{}
	for {
		typ, raw, err := conn.Read(ctx)
		if err != nil {
			return result, err
		}
		if typ != websocket.MessageText || !json.Valid(raw) {
			return result, errAppServerTransportProtocol
		}
		var event appServerTransportEvent
		if json.Unmarshal(raw, &event) != nil || event.Type == "" {
			return result, errAppServerTransportProtocol
		}
		if event.Type == "response.metadata" && *turnState == "" {
			for name, value := range event.Headers {
				if strings.EqualFold(name, "x-codex-turn-state") {
					var token string
					if json.Unmarshal(value, &token) == nil && gatewayNativeHeaderValue(token) {
						*turnState = token
					}
				}
			}
		}
		switch event.Type {
		case "response.output_item.done":
			hash, err := appServerItemHash(event.Item)
			if err != nil {
				return result, err
			}
			result.outputHashes = append(result.outputHashes, hash)
		case "response.completed":
			result.responseID = event.Response.ID
			if len(result.outputHashes) == 0 {
				for _, item := range event.Response.Output {
					hash, err := appServerItemHash(item)
					if err != nil {
						return result, err
					}
					result.outputHashes = append(result.outputHashes, hash)
				}
			}
			return result, nil
		case "error", "response.failed", "response.incomplete":
			// Exact failure event stays available without echoing it into logs.
			return result, &appServerTransportUpstreamError{Event: append([]byte(nil), raw...)}
		}
	}
}

type appServerTransportUpstreamError struct{ Event []byte }

func (e *appServerTransportUpstreamError) Error() string {
	return errAppServerTransportTerminal.Error()
}
func (e *appServerTransportUpstreamError) Unwrap() error { return errAppServerTransportTerminal }

// Mirrors 0.155.0 responses_request_properties_match and incremental prefix
// comparison. Ignore item-internal metadata, not message content or tool output.
func appServerInputDelta(previous, current []byte, output [][32]byte) ([]json.RawMessage, bool) {
	var old, next map[string]json.RawMessage
	if json.Unmarshal(previous, &old) != nil || json.Unmarshal(current, &next) != nil {
		return nil, false
	}
	for _, key := range []string{"model", "instructions", "tools", "tool_choice", "parallel_tool_calls", "reasoning",
		"store", "stream", "include", "service_tier", "prompt_cache_key", "text"} {
		if !officialJSONEqual(appServerValueOrNull(old[key]), appServerValueOrNull(next[key])) {
			return nil, false
		}
	}
	var before, after []json.RawMessage
	if json.Unmarshal(old["input"], &before) != nil || json.Unmarshal(next["input"], &after) != nil ||
		len(after) < len(before)+len(output) {
		return nil, false
	}
	for index, item := range before {
		oldHash, err1 := appServerItemHash(item)
		newHash, err2 := appServerItemHash(after[index])
		if err1 != nil || err2 != nil || oldHash != newHash {
			return nil, false
		}
	}
	for index, want := range output {
		got, err := appServerItemHash(after[len(before)+index])
		if err != nil || got != want {
			return nil, false
		}
	}
	// Empty delta is permitted by official prepare_websocket_request.
	return after[len(before)+len(output):], true
}
func appServerValueOrNull(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}
func appServerItemHash(raw json.RawMessage) ([32]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var item map[string]any
	if decoder.Decode(&item) != nil || item == nil {
		return [32]byte{}, errAppServerTransportProtocol
	}
	delete(item, "internal_chat_message_metadata_passthrough")
	encoded, err := json.Marshal(item)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}
func appServerPumpSSE(ctx context.Context, conn gatewayWSUpstream, writer io.Writer) error {
	for {
		typ, raw, err := conn.Read(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		if typ != websocket.MessageText || !json.Valid(raw) {
			return errAppServerTransportProtocol
		}
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &event) != nil || event.Type == "" {
			return errAppServerTransportProtocol
		}
		// Preserve exact event JSON, including tools, reasoning and usage.
		for _, line := range bytes.Split(raw, []byte{'\n'}) {
			if _, err = writer.Write(append(append([]byte("data: "), line...), '\n')); err != nil {
				return err
			}
		}
		if _, err = writer.Write([]byte{'\n'}); err != nil {
			return err
		}
		switch event.Type {
		case "response.completed":
			return nil
		case "error", "response.failed", "response.incomplete":
			// A complete terminal failure is still a complete protocol exchange.
			// Let SSE observers classify its exact payload and account failover;
			// treating this as a broken pipe aborts buffered errors before the
			// client can see them. A socket close before a terminal remains an
			// error above and must never be replayed as an accepted generation.
			return nil
		}
	}
}
