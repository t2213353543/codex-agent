package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type appServerConnectionResultKey struct{}
type appServerConnectionResult struct {
	mu        sync.Mutex
	confirmed bool
	success   bool
	release   func(bool)
	ctx       context.Context
}

func newAppServerConnectionResult() *appServerConnectionResult { return &appServerConnectionResult{} }
func withAppServerConnectionResult(ctx context.Context, result *appServerConnectionResult) context.Context {
	result.mu.Lock()
	result.ctx = ctx
	result.mu.Unlock()
	return context.WithValue(ctx, appServerConnectionResultKey{}, result)
}

// The outer handler confirms only after downstream delivery and owner CAS.
func (r *appServerConnectionResult) Confirm(success bool) {
	r.mu.Lock()
	if r.confirmed {
		r.mu.Unlock()
		return
	}
	r.confirmed, r.success = true, success
	release := r.release
	r.release = nil
	r.mu.Unlock()
	if release != nil {
		release(success)
	}
}
func (r *appServerConnectionResult) bind(release func(bool)) {
	r.mu.Lock()
	if r.confirmed {
		success := r.success
		r.mu.Unlock()
		release(success)
		return
	}
	previous := r.release
	r.release = release
	r.mu.Unlock()
	if previous != nil {
		previous(false)
	}
}

type appServerConnectionPool struct {
	mu     sync.Mutex
	owner  *Proxy
	dial   func(context.Context, context.Context, http.Header, *ProxyEntry) (gatewayWSUpstream, *http.Response, error)
	ctx    context.Context
	cancel context.CancelFunc
	closed bool
	idle   time.Duration
	slots  map[[32]byte]*appServerConnectionSlot
}
type appServerConnectionSlot struct {
	key       [32]byte
	busy      bool
	ready     chan struct{}
	conn      gatewayWSUpstream
	cancel    context.CancelFunc
	timer     *time.Timer
	baseline  appServerConnectionBaseline
	turnID    string
	turnState string
}

// Fixed-size digests retain no prompts, tools, output text or account secrets.
type appServerConnectionBaseline struct {
	stable     [32]byte
	chain      [32]byte
	count      int
	responseID string
}

func newAppServerConnectionPool(p *Proxy) *appServerConnectionPool {
	ctx, cancel := context.WithCancel(context.Background())
	return &appServerConnectionPool{owner: p, dial: p.dialNativeGatewayWebSocketLifetime, ctx: ctx, cancel: cancel, idle: 10 * time.Minute, slots: map[[32]byte]*appServerConnectionSlot{}}
}
func (p *appServerConnectionPool) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.cancel()
	slots := p.slots
	p.slots = map[[32]byte]*appServerConnectionSlot{}
	for _, slot := range slots {
		if slot.timer != nil {
			slot.timer.Stop()
		}
		if slot.busy {
			close(slot.ready)
		}
	}
	p.mu.Unlock()
	for _, slot := range slots {
		if slot.cancel != nil {
			slot.cancel()
		}
		if slot.conn != nil {
			_ = slot.conn.CloseNow()
		}
	}
}
func (p *appServerConnectionPool) acquire(ctx context.Context, key [32]byte) (*appServerConnectionSlot, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errAppServerTransportClosed
		}
		slot := p.slots[key]
		if slot == nil {
			slot = &appServerConnectionSlot{key: key, busy: true, ready: make(chan struct{})}
			p.slots[key] = slot
			p.mu.Unlock()
			return slot, nil
		}
		if !slot.busy {
			if !appServerConnectionUsable(slot.conn) {
				delete(p.slots, key)
				if slot.timer != nil {
					slot.timer.Stop()
				}
				p.mu.Unlock()
				if slot.cancel != nil {
					slot.cancel()
				}
				if slot.conn != nil {
					_ = slot.conn.CloseNow()
				}
				continue
			}
			if slot.timer != nil {
				slot.timer.Stop()
				slot.timer = nil
			}
			slot.busy = true
			slot.ready = make(chan struct{})
			p.mu.Unlock()
			return slot, nil
		}
		ready := slot.ready
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.ctx.Done():
			return nil, errAppServerTransportClosed
		case <-ready:
		}
	}
}

func appServerConnectionUsable(conn gatewayWSUpstream) bool {
	if conn == nil {
		return false
	}
	if native, ok := conn.(*gatewayNativeWS); ok {
		select {
		case <-native.done:
			return false
		default:
		}
	}
	return true
}
func (p *appServerConnectionPool) release(slot *appServerConnectionSlot, healthy bool) {
	p.mu.Lock()
	if p.slots[slot.key] != slot {
		p.mu.Unlock()
		return
	}
	if !healthy || p.closed {
		delete(p.slots, slot.key)
		if slot.timer != nil {
			slot.timer.Stop()
		}
		if slot.busy {
			close(slot.ready)
		}
		slot.busy = false
		p.mu.Unlock()
		if slot.cancel != nil {
			slot.cancel()
		}
		if slot.conn != nil {
			_ = slot.conn.CloseNow()
		}
		return
	}
	slot.busy = false
	close(slot.ready)
	slot.timer = time.AfterFunc(p.idle, func() {
		p.mu.Lock()
		if p.slots[slot.key] != slot || slot.busy {
			p.mu.Unlock()
			return
		}
		delete(p.slots, slot.key)
		p.mu.Unlock()
		if slot.cancel != nil {
			slot.cancel()
		}
		if slot.conn != nil {
			_ = slot.conn.CloseNow()
		}
	})
	p.mu.Unlock()
}

func appServerConnectionKey(req *http.Request, proxy *ProxyEntry, in appServerRequestInput, headers http.Header) ([32]byte, error) {
	route, _ := req.Context().Value(gatewayRequestRouteKey{}).(*gatewayRequestRoute)
	if route == nil || route.keyID == "" || in.SessionID == "" || in.ThreadID == "" {
		return [32]byte{}, errors.New("connection reuse requires admitted key/session/thread")
	}
	credential, _ := req.Context().Value(gatewayRequestCredentialsKey{}).(gatewayRequestCredentials)
	proxyHash := [32]byte{}
	if proxy != nil {
		proxyHash = gatewayProxyFingerprint(*proxy)
	}
	// Request trace ids/turn metadata change per call and ride message metadata.
	// All remaining handshake identities are compared, including UA/originator,
	// OpenAI-Beta, account headers, TLS route/model hint and unknown extensions.
	names := []string{}
	for name := range headers {
		switch strings.ToLower(name) {
		case "x-client-request-id", "traceparent", "tracestate", "x-codex-turn-metadata":
			continue
		}
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })
	pairs := make([]any, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, []any{strings.ToLower(name), headers.Values(name)})
	}
	value := []any{req.URL.Scheme, req.URL.Host, req.URL.Path, route.keyID, in.SessionID, in.ThreadID,
		gatewayNativeAccountScope(req.Context(), req.Header), credential.id, credential.version, proxyHash, pairs, gatewayUnboundedPayload(req.Context())}
	raw, err := json.Marshal(value)
	return sha256.Sum256(raw), err
}
func appServerConnectionBody(raw []byte) (appServerConnectionBaseline, []json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return appServerConnectionBaseline{}, nil, errAppServerTransportProtocol
	}
	var input []json.RawMessage
	if json.Unmarshal(fields["input"], &input) != nil {
		return appServerConnectionBaseline{}, nil, errAppServerTransportProtocol
	}
	// Unknown writer extensions participate conservatively. The known fields
	// excluded here match official request-properties comparison.
	for _, name := range []string{"input", "client_metadata", "stream_options", "access_programs", "type", "generate", "previous_response_id"} {
		delete(fields, name)
	}
	for name, value := range fields {
		var decoded any
		d := json.NewDecoder(bytes.NewReader(value))
		d.UseNumber()
		if d.Decode(&decoded) != nil {
			return appServerConnectionBaseline{}, nil, errAppServerTransportProtocol
		}
		fields[name], _ = json.Marshal(decoded)
	}
	canonical, err := json.Marshal(fields)
	if err != nil {
		return appServerConnectionBaseline{}, nil, err
	}
	baseline := appServerConnectionBaseline{stable: sha256.Sum256(canonical)}
	for _, item := range input {
		hash, err := appServerItemHash(item)
		if err != nil {
			return baseline, nil, err
		}
		baseline.append(hash)
	}
	return baseline, input, nil
}
func (b *appServerConnectionBaseline) append(item [32]byte) {
	var joined [64]byte
	copy(joined[:32], b.chain[:])
	copy(joined[32:], item[:])
	b.chain = sha256.Sum256(joined[:])
	b.count++
}
func (b appServerConnectionBaseline) delta(full appServerConnectionBaseline, input []json.RawMessage) ([]json.RawMessage, bool) {
	if b.responseID == "" || b.stable != full.stable || len(input) < b.count {
		return nil, false
	}
	prefix := appServerConnectionBaseline{}
	for _, item := range input[:b.count] {
		hash, err := appServerItemHash(item)
		if err != nil {
			return nil, false
		}
		prefix.append(hash)
	}
	if prefix.chain != b.chain {
		return nil, false
	}
	return input[b.count:], true
}

type appServerConnectionUse struct {
	mu                sync.Mutex
	pool              *appServerConnectionPool
	slot              *appServerConnectionSlot
	stopCancel        func() bool
	stopClient        func() bool
	terminalReady     bool
	terminal          bool
	eof               bool
	confirmed         bool
	approved          bool
	finished          bool
	state             appServerConnectionBaseline
	turnID, turnState string
}

func (u *appServerConnectionUse) update(action string, ok bool) {
	u.mu.Lock()
	if u.finished {
		u.mu.Unlock()
		return
	}
	switch action {
	case "terminal-ready":
		u.terminalReady = ok
	case "terminal":
		u.terminal = ok
	case "eof":
		u.eof = ok
	case "confirm":
		u.confirmed = true
		u.approved = ok
	}
	bad := !ok
	// Confirm(true) is the outer handler's proof of complete terminal
	// consumption/delivery and owner CAS; scanners need not perform extra EOF.
	good := u.terminal && u.confirmed && u.approved
	if !bad && !good {
		u.mu.Unlock()
		return
	}
	u.finished = true
	if u.stopCancel != nil {
		u.stopCancel()
	}
	if u.stopClient != nil {
		u.stopClient()
	}
	if good && !bad {
		u.slot.baseline = u.state
		u.slot.turnID, u.slot.turnState = u.turnID, u.turnState
	}
	u.mu.Unlock()
	u.pool.release(u.slot, good && !bad)
}

type appServerConnectionBodyReader struct {
	*io.PipeReader
	use *appServerConnectionUse
}

func (b *appServerConnectionBodyReader) Read(raw []byte) (int, error) {
	n, err := b.PipeReader.Read(raw)
	if err != nil {
		b.use.update("eof", errors.Is(err, io.EOF))
	}
	return n, err
}
func (b *appServerConnectionBodyReader) Close() error {
	b.use.update("close", true)
	return b.PipeReader.Close()
}

func (p *appServerConnectionPool) roundTrip(req *http.Request, proxy *ProxyEntry, in appServerRequestInput, result *appServerConnectionResult) (*http.Response, error) {
	full, err := buildAppServerRequest(in)
	if err != nil {
		return nil, &appServerTransportError{Stage: "request", Err: err}
	}
	baseline, input, err := appServerConnectionBody(full.Body)
	if err != nil {
		return nil, &appServerTransportError{Stage: "request", Err: err}
	}
	warmInput, err := appServerPrewarmProfile(in)
	if err != nil {
		return nil, &appServerTransportError{Stage: "prewarm_prepare", Err: err}
	}
	warmInput.Input = nil
	if _, exists := in.ClientControls["tool_choice"]; exists {
		warmInput.ClientControls = map[string]json.RawMessage{}
		for k, v := range in.ClientControls {
			if k != "tool_choice" {
				warmInput.ClientControls[k] = v
			}
		}
	}
	state := appServerWebSocketRequestState{StreamRequestStartMS: time.Now().UnixMilli(), RoutingHint: req.Header.Get("x-codex-routing-hint"), Traceparent: req.Header.Get("traceparent"), Tracestate: req.Header.Get("tracestate")}
	if state.RoutingHint == "" {
		var f struct {
			Model       string
			ServiceTier string `json:"service_tier"`
		}
		_ = json.Unmarshal(full.Body, &f)
		state.RoutingHint = "model=" + f.Model
		if f.ServiceTier != "" {
			state.RoutingHint += ";tier=" + f.ServiceTier
		}
	}
	// The first handshake belongs to startup prewarm too. Reused sockets keep
	// their established headers; generation metadata rides each new frame.
	wire, err := buildAppServerWebSocketRequest(warmInput, state)
	if err != nil {
		return nil, &appServerTransportError{Stage: "request", Err: err}
	}
	headers := req.Header.Clone()
	for name, values := range wire.Headers {
		headers[name] = append([]string(nil), values...)
	}
	for _, name := range []string{"Authorization", "ChatGPT-Account-Id"} {
		headers.Del(name)
		for _, v := range req.Header.Values(name) {
			headers.Add(name, v)
		}
	}
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Encoding", "Accept-Encoding", "Accept", "Connection", "Upgrade", "Host", "Transfer-Encoding", "Sec-WebSocket-Key", "Sec-WebSocket-Version", "Sec-WebSocket-Accept", "Sec-WebSocket-Extensions", "x-openai-internal-codex-responses-lite", "x-codex-turn-state"} {
		headers.Del(name)
	}
	key, err := appServerConnectionKey(req, proxy, in, headers)
	if err != nil {
		return nil, &appServerTransportError{Stage: "request", Err: err}
	}
	slot, err := p.acquire(req.Context(), key)
	if err != nil {
		return nil, &appServerTransportError{Stage: "acquire", Err: err}
	}
	keep := false
	defer func() {
		if !keep {
			p.release(slot, false)
		}
	}()
	// Revalidate current key/account/proxy at every lease, not just handshake.
	if err = checkGatewayNativeDispatch(req.Context()); err != nil {
		return nil, &appServerTransportError{Stage: "authorization", Err: err}
	}
	if proxy != nil && proxy.ID != "" && p.owner.Proxies != nil {
		current, exists := p.owner.Proxies.Get(proxy.ID)
		if !exists || gatewayProxyFingerprint(*current) != gatewayProxyFingerprint(*proxy) {
			return nil, &appServerTransportError{Stage: "route", Err: errGatewayProxyUnavailable}
		}
	}
	fresh := slot.conn == nil
	var handshake *http.Response
	if fresh {
		lifetime, cancel := context.WithCancel(p.ctx)
		p.mu.Lock()
		if p.closed || p.slots[key] != slot {
			p.mu.Unlock()
			cancel()
			return nil, &appServerTransportError{Stage: "closed", Err: errAppServerTransportClosed}
		}
		slot.cancel = cancel
		p.mu.Unlock()
		var connected gatewayWSUpstream
		connected, handshake, err = p.dial(req.Context(), lifetime, headers, proxy)
		p.mu.Lock()
		if p.closed || p.slots[key] != slot {
			p.mu.Unlock()
			cancel()
			if connected != nil {
				_ = connected.CloseNow()
			}
			return nil, &appServerTransportError{Stage: "closed", Err: errAppServerTransportClosed}
		}
		slot.conn = connected
		p.mu.Unlock()
		if err != nil || connected == nil {
			if handshake != nil && handshake.StatusCode != 101 {
				handshake.Request = req
				if handshake.StatusCode == 403 || handshake.StatusCode == 426 {
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
			slot.conn.SetReadLimit(-1)
		} else {
			slot.conn.SetReadLimit(maxBody)
		}
		if handshake != nil && handshake.Body != nil {
			_ = handshake.Body.Close()
		}
	}
	turnID := in.ClientMetadata["turn_id"]
	turnState := in.TurnState
	if turnID != "" && turnID == slot.turnID {
		turnState = slot.turnState // OnceLock within one explicit turn.
	}
	if fresh && turnState == "" && handshake != nil {
		turnState = handshake.Header.Get("x-codex-turn-state")
	}
	use := &appServerConnectionUse{pool: p, slot: slot, state: baseline, turnID: turnID, turnState: turnState}
	result.mu.Lock()
	clientCtx := result.ctx
	result.mu.Unlock()
	if clientCtx == nil {
		clientCtx = req.Context()
	}
	stopActive := context.AfterFunc(req.Context(), func() {
		use.mu.Lock()
		ready := use.terminalReady
		use.mu.Unlock()
		if !ready || clientCtx.Err() != nil {
			use.update("cancel", false)
		}
	})
	stopClient := context.AfterFunc(clientCtx, func() { use.update("cancel", false) })
	use.mu.Lock()
	use.stopCancel = stopActive
	use.stopClient = stopClient
	use.mu.Unlock()
	defer func() {
		if !keep {
			stopActive()
			stopClient()
		}
	}()
	if fresh {
		warmInput.TurnState = turnState
		state.Warmup = true
		warm, buildErr := buildAppServerWebSocketRequest(warmInput, state)
		if buildErr != nil {
			return nil, &appServerTransportError{Stage: "prewarm_prepare", Err: buildErr}
		}
		if err = gatewayWSWrite(req.Context(), slot.conn, websocket.MessageText, warm.Body); err != nil {
			return nil, &appServerTransportError{Stage: "prewarm_write", Err: err}
		}
		prior, readErr := appServerReadWarmup(req.Context(), slot.conn, &turnState)
		if readErr != nil {
			return nil, &appServerTransportError{Stage: "prewarm_read", Err: readErr}
		}
		warmBase, _, parseErr := appServerConnectionBody(warm.Body)
		if parseErr != nil {
			return nil, &appServerTransportError{Stage: "prewarm_read", Err: parseErr}
		}
		for _, hash := range prior.outputHashes {
			warmBase.append(hash)
		}
		warmBase.responseID = prior.responseID
		slot.baseline = warmBase
	}
	in.TurnState = turnState
	state.Warmup = false
	state.StreamRequestStartMS = time.Now().UnixMilli()
	if delta, ok := slot.baseline.delta(baseline, input); ok {
		state.PreviousResponseID = &slot.baseline.responseID
		state.InputDelta = &delta
	}
	generation, err := buildAppServerWebSocketRequest(in, state)
	if err != nil {
		return nil, &appServerTransportError{Stage: "generation_prepare", Err: err}
	}
	// A reused connection cannot rely on the original handshake's old guard.
	if err = checkGatewayNativeDispatch(req.Context()); err != nil {
		return nil, &appServerTransportError{Stage: "authorization", Err: err}
	}
	if err = gatewayWSWrite(req.Context(), slot.conn, websocket.MessageText, generation.Body); err != nil {
		return nil, &appServerTransportError{Stage: "generation_write", GenerationSent: true, Err: err}
	}
	reader, writer := io.Pipe()
	result.bind(func(ok bool) { use.update("confirm", ok) })
	body := &appServerConnectionBodyReader{PipeReader: reader, use: use}
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
	keep = true
	go func() {
		pipeCancel := context.AfterFunc(clientCtx, func() { _ = writer.CloseWithError(clientCtx.Err()) })
		pipeShutdown := context.AfterFunc(p.ctx, func() { _ = writer.CloseWithError(errAppServerTransportClosed) })
		next, token, success, err := appServerConnectionPump(req.Context(), slot.conn, writer, baseline, turnState, func() { use.update("terminal-ready", true) })
		pipeCancel()
		pipeShutdown()
		use.mu.Lock()
		use.state = next
		use.turnState = token
		use.mu.Unlock()
		use.update("terminal", success && err == nil)
		if err != nil {
			_ = writer.CloseWithError(&appServerTransportError{Stage: "generation_read", GenerationSent: true, Err: err})
		} else {
			_ = writer.Close()
		}
	}()
	return &http.Response{StatusCode: 200, Status: "200 OK", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: responseHeaders, Body: body, ContentLength: -1, Request: req}, nil
}

func appServerConnectionPump(ctx context.Context, conn gatewayWSUpstream, writer io.Writer, baseline appServerConnectionBaseline, turnState string, terminalReady func()) (appServerConnectionBaseline, string, bool, error) {
	next := baseline
	outputCount := 0
	createdID := ""
	for {
		typ, raw, err := conn.Read(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return next, turnState, false, err
		}
		var event struct {
			Type        string                     `json:"type"`
			OutputIndex *int                       `json:"output_index"`
			Item        json.RawMessage            `json:"item"`
			Headers     map[string]json.RawMessage `json:"headers"`
			Response    struct {
				ID, Status string
				Output     []json.RawMessage
			} `json:"response"`
		}
		if typ != websocket.MessageText || !json.Valid(raw) || json.Unmarshal(raw, &event) != nil || event.Type == "" {
			return next, turnState, false, errAppServerTransportProtocol
		}
		if event.Type == "response.metadata" && turnState == "" {
			for name, value := range event.Headers {
				if strings.EqualFold(name, "x-codex-turn-state") {
					var token string
					if json.Unmarshal(value, &token) == nil && gatewayNativeHeaderValue(token) {
						turnState = token
					}
				}
			}
		}
		switch event.Type {
		case "response.created":
			if createdID != "" && event.Response.ID != createdID {
				return next, turnState, false, errAppServerTransportProtocol
			}
			createdID = event.Response.ID
		case "response.output_item.done":
			if event.OutputIndex != nil && *event.OutputIndex != outputCount {
				return next, turnState, false, errAppServerTransportProtocol
			}
			hash, err := appServerItemHash(event.Item)
			if err != nil {
				return next, turnState, false, err
			}
			next.append(hash)
			outputCount++
		case "response.completed":
			if event.Response.ID == "" || createdID != "" && createdID != event.Response.ID || event.Response.Status != "" && event.Response.Status != "completed" {
				return next, turnState, false, errAppServerTransportProtocol
			}
			if len(event.Response.Output) > 0 {
				final := baseline
				for _, item := range event.Response.Output {
					hash, err := appServerItemHash(item)
					if err != nil {
						return next, turnState, false, err
					}
					final.append(hash)
				}
				if outputCount > 0 && (final.count != next.count || final.chain != next.chain) {
					return next, turnState, false, errAppServerTransportProtocol
				}
				next = final
			}
			next.responseID = event.Response.ID
			terminalReady()
		}
		for _, line := range bytes.Split(raw, []byte{'\n'}) {
			if _, err = writer.Write(append(append([]byte("data: "), line...), '\n')); err != nil {
				return next, turnState, false, err
			}
		}
		if _, err = writer.Write([]byte{'\n'}); err != nil {
			return next, turnState, false, err
		}
		switch event.Type {
		case "response.completed":
			return next, turnState, true, nil
		case "response.failed", "response.incomplete", "error":
			return next, turnState, false, nil
		}
	}
}
