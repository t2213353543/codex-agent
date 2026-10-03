package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"

	"github.com/coder/websocket"
)

type bpsWarmupKey struct{}
type bpsWebSocketKey struct{}
type bpsWSLane struct {
	cancel     context.CancelFunc
	responseID string
}

// Codex WebSocket framing terminates here. Every generated turn uses the same
// authenticated HTTP/BPS path; incremental history and warmups are local state,
// never sent as unsupported BPS fields. One writer provides bounded backpressure.
func (p *Proxy) gatewayBPSWebSocket(w http.ResponseWriter, r *http.Request, keyID, keyName string, settings Settings) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	unbounded := gatewayUnboundedPayload(r.Context()) || officialRequestUnbounded(r)
	if unbounded {
		conn.SetReadLimit(-1)
	} else {
		conn.SetReadLimit(maxBody)
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var writes sync.Mutex
	send := func(raw []byte) error {
		writes.Lock()
		defer writes.Unlock()
		sendCtx, stop := context.WithTimeout(ctx, gatewayWSWriteTimeout)
		defer stop()
		return conn.Write(sendCtx, websocket.MessageText, raw)
	}
	var lanesMu sync.Mutex
	lanes := map[string]*bpsWSLane{}
	var work sync.WaitGroup
	defer func() {
		cancel()
		lanesMu.Lock()
		for _, lane := range lanes {
			lane.cancel()
		}
		lanesMu.Unlock()
		work.Wait()
	}()
	sendError := func(streamID, code, message string) {
		event := map[string]any{"type": "error", "status": http.StatusBadGateway, "error": map[string]string{"type": "gateway_error", "code": code, "message": message}}
		if streamID != "" {
			event["stream_id"] = streamID
		}
		raw, _ := json.Marshal(event)
		_ = send(raw)
	}
	for {
		typ, raw, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			sendError("", "unsupported_frame", "Responses requires text JSON frames")
			continue
		}
		fields, err := gatewayWSParseFields(raw)
		if err != nil {
			sendError("", "invalid_json", "invalid Responses frame")
			continue
		}
		var kind, streamID, responseID string
		if gatewayWSDecodeFields(fields, map[string]any{"type": &kind, "stream_id": &streamID, "response_id": &responseID}) != nil {
			sendError("", "invalid_frame", "invalid Responses frame fields")
			continue
		}
		if streamID != "" && !gatewayWSValidStreamID(streamID, unbounded) {
			sendError("", "invalid_stream_id", "invalid stream_id")
			continue
		}
		if kind == "response.cancel" || kind == "response.cancelled" {
			lanesMu.Lock()
			var lane *bpsWSLane
			if streamID != "" || responseID == "" {
				lane = lanes[streamID]
			}
			if lane != nil && responseID != "" && lane.responseID != responseID {
				lane = nil
			}
			if lane == nil && responseID != "" && streamID == "" {
				for _, candidate := range lanes {
					if candidate.responseID == responseID {
						lane = candidate
						break
					}
				}
			}
			if lane != nil {
				lane.cancel()
			}
			lanesMu.Unlock()
			if lane == nil {
				sendError(streamID, "response_not_running", "no matching active response")
			}
			continue
		}
		p.observeDashboardRealtimeWSCreate(raw)
		if kind != "response.create" {
			sendError(streamID, "unsupported_event", "unsupported Responses event")
			continue
		}
		generate := true
		if value, ok := fields["generate"]; ok && (string(value) == "null" || json.Unmarshal(value, &generate) != nil) {
			sendError(streamID, "invalid_generate", "generate must be boolean")
			continue
		}
		delete(fields, "type")
		delete(fields, "stream_id")
		delete(fields, "generate")
		fields["stream"] = json.RawMessage("true")
		body, err := json.Marshal(fields)
		if err != nil {
			sendError(streamID, "invalid_frame", "cannot encode Responses request")
			continue
		}
		lanesMu.Lock()
		if _, exists := lanes[streamID]; exists {
			lanesMu.Unlock()
			sendError(streamID, "stream_busy", "this stream already has an active response")
			continue
		}
		if len(lanes) >= 64 && !unbounded {
			lanesMu.Unlock()
			sendError(streamID, "too_many_streams", "connection stream limit reached")
			continue
		}
		turnCtx, stop := context.WithCancel(ctx)
		turnCtx = context.WithValue(turnCtx, bpsWebSocketKey{}, true)
		lane := &bpsWSLane{cancel: stop}
		lanes[streamID] = lane
		lanesMu.Unlock()
		turnCtx = context.WithValue(turnCtx, bpsWarmupKey{}, !generate)
		work.Add(1)
		go func(streamID string, body []byte, lane *bpsWSLane, turnCtx context.Context) {
			defer work.Done()
			defer lane.cancel()
			finish := func() {
				lanesMu.Lock()
				if lanes[streamID] == lane {
					delete(lanes, streamID)
				}
				lanesMu.Unlock()
			}
			defer finish()
			writer := &bpsWSResponseWriter{header: make(http.Header), streamID: streamID, send: send, onResponseID: func(id string) { lanesMu.Lock(); lane.responseID = id; lanesMu.Unlock() }, onTerminal: finish, unbounded: unbounded}
			request := r.Clone(turnCtx)
			request.Method = http.MethodPost
			request.URL.Path = "/v1/responses"
			request.URL.RawQuery = ""
			request.Body = io.NopCloser(bytes.NewReader(body))
			request.ContentLength = int64(len(body))
			request.Header = request.Header.Clone()
			request.Header.Del("Upgrade")
			request.Header.Del("Connection")
			request.Header.Del("Sec-WebSocket-Key")
			request.Header.Del("Sec-WebSocket-Version")
			request.Header.Set("Content-Type", "application/json")
			request.Header.Del("Content-Encoding")
			defer func() {
				if recovered := recover(); recovered != nil {
					if turnCtx.Err() != nil {
						writer.canceled()
					} else {
						sendError(streamID, "upstream_stream_interrupted", "upstream stream ended before a valid terminal response")
					}
				}
			}()
			if p.RuntimeModes && requestInferenceMode(request.Context()) == inferenceAppServer {
				p.gatewayOfficial(writer, request, inferenceAppServer)
			} else {
				p.gatewayBPS(writer, request)
			}
			if turnCtx.Err() != nil && !writer.terminal {
				writer.canceled()
			} else if err := writer.finish(); err != nil {
				sendError(streamID, "upstream_stream_interrupted", err.Error())
			}
		}(streamID, body, lane, turnCtx)
	}
}

type bpsWSResponseWriter struct {
	header       http.Header
	status       int
	streamID     string
	send         func([]byte) error
	onResponseID func(string)
	onTerminal   func()
	buffer       []byte
	eventBuffer  []byte
	streamJSON   []byte
	terminal     bool
	responseID   string
	unbounded    bool
}

// A rare large event must not pin its peak allocation for the rest of every
// active lane. Pending input is still allowed up to maxBody; this only limits
// capacity retained for reuse after synchronous delivery has completed.
const bpsWSBufferReuseLimit = 256 << 10

func (w *bpsWSResponseWriter) Header() http.Header { return w.header }
func (w *bpsWSResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *bpsWSResponseWriter) Flush() {
	if w.status == 0 {
		w.status = 200
	}
}
func (w *bpsWSResponseWriter) FlushError() error { w.Flush(); return nil }
func (w *bpsWSResponseWriter) Write(raw []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	if len(w.buffer)+len(raw) > maxBody && !w.unbounded {
		return 0, errors.New("WebSocket response event exceeds limit")
	}
	w.buffer = append(w.buffer, raw...)
	if w.status >= 300 {
		return len(raw), nil
	}
	consumed := 0
	for {
		index := bytes.Index(w.buffer[consumed:], []byte("\n\n"))
		if index < 0 {
			break
		}
		frame := w.buffer[consumed : consumed+index]
		consumed += index + 2
		var payload []byte
		parts := 0
		for len(frame) > 0 {
			line, rest, _ := bytes.Cut(frame, []byte("\n"))
			frame = rest
			if bytes.HasPrefix(line, []byte("data:")) {
				part := bytes.TrimPrefix(line[len("data:"):], []byte(" "))
				if parts == 0 {
					payload = part
				} else {
					if parts == 1 {
						// Single-line data events borrow the writer buffer. Only
						// multiline SSE needs a separately assembled JSON value.
						payload = bytes.Clone(payload)
					}
					payload = append(payload, '\n')
					payload = append(payload, part...)
				}
				parts++
			}
		}
		if parts == 0 {
			continue
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if err := w.event(payload); err != nil {
			w.buffer = w.buffer[consumed:]
			return 0, err
		}
	}
	// send is synchronous and applies the existing bounded socket backpressure;
	// its input is no longer in use when it returns, so consumed capacity is reusable.
	if consumed > 0 {
		remaining := w.buffer[consumed:]
		if cap(w.buffer) > bpsWSBufferReuseLimit && len(remaining) <= bpsWSBufferReuseLimit {
			// Preserve a following partial frame without letting its small tail
			// keep the completed large frame's backing allocation alive.
			w.buffer = bytes.Clone(remaining)
		} else {
			w.buffer = w.buffer[:copy(w.buffer, remaining)]
		}
	}
	return len(raw), nil
}
func (w *bpsWSResponseWriter) event(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if !json.Valid(raw) || len(raw) == 0 || raw[0] != '{' {
		return errors.New("invalid SSE JSON event")
	}
	raw = bpsWebSocketFailurePayload(raw)
	fields := gatewayValidatedJSONFieldSpans(raw, "type", "response")
	kind := gatewayJSONSpanString(raw, fields[0])
	if fields[1].end > 0 {
		response := raw[fields[1].start:fields[1].end]
		id := gatewayJSONSpanString(response, gatewayValidatedJSONFieldSpans(response, "id", "")[0])
		if id != "" {
			w.responseID = id
			if w.onResponseID != nil {
				w.onResponseID(id)
			}
		}
	}
	switch kind {
	case "response.completed", "response.done", "response.failed", "response.cancelled", "basispoints.response.cancelled", "response.incomplete", "error":
		w.terminal = true
		if w.onTerminal != nil {
			w.onTerminal()
		}
	}
	if w.streamID == "" {
		return w.send(raw)
	}
	if gatewayValidatedJSONFieldSpans(raw, "stream_id", "")[0].end > 0 {
		// Unusual upstream stream IDs must be replaced, including duplicate
		// or escaped member names; do not create an ambiguous second lane ID.
		var obj map[string]json.RawMessage
		_ = json.Unmarshal(raw, &obj)
		obj["stream_id"], _ = json.Marshal(w.streamID)
		payload, _ := json.Marshal(obj)
		return w.send(payload)
	}
	if len(w.streamJSON) == 0 {
		w.streamJSON, _ = json.Marshal(w.streamID)
	}
	w.eventBuffer = append(w.eventBuffer[:0], raw[:len(raw)-1]...)
	if len(bytes.TrimSpace(raw[1:len(raw)-1])) != 0 {
		w.eventBuffer = append(w.eventBuffer, ',')
	}
	w.eventBuffer = append(w.eventBuffer, `"stream_id":`...)
	w.eventBuffer = append(w.eventBuffer, w.streamJSON...)
	w.eventBuffer = append(w.eventBuffer, '}')
	err := w.send(w.eventBuffer)
	if cap(w.eventBuffer) > bpsWSBufferReuseLimit {
		w.eventBuffer = nil
	}
	return err
}

// The official WS client maps a wrapped HTTP 429 to a terminal rate-limit
// error. The same code in response.failed is retryable and triggers a second
// HTTP transport attempt. Preserve the provider's explicit rate-limit meaning
// without dropping other lanes or inventing a successful response.
func bpsWebSocketFailurePayload(raw []byte) []byte {
	spans := gatewayValidatedJSONFieldSpans(raw, "type", "response")
	if gatewayJSONSpanString(raw, spans[0]) == "error" {
		fields, err := gatewayWSParseFields(raw)
		if err != nil {
			return raw
		}
		detail, err := gatewayWSParseFields(fields["error"])
		if err != nil {
			detail = fields
		}
		var code string
		_ = json.Unmarshal(detail["code"], &code)
		if code != "basispoints_route_rate_limit_exceeded" {
			return raw
		}
		if len(fields["status"]) > 0 || len(fields["status_code"]) > 0 {
			return raw // Never overwrite an explicit provider status.
		}
		fields["status"] = json.RawMessage(`429`)
		encoded, err := json.Marshal(fields)
		if err != nil {
			return raw
		}
		return encoded
	}
	if gatewayJSONSpanString(raw, spans[0]) != "response.failed" || spans[1].end == 0 {
		return raw
	}
	var event, response, detail map[string]json.RawMessage
	if json.Unmarshal(raw, &event) != nil || json.Unmarshal(event["response"], &response) != nil || json.Unmarshal(response["error"], &detail) != nil {
		return raw
	}
	var code string
	_ = json.Unmarshal(detail["code"], &code)
	switch code {
	case "rate_limit_exceeded", "rate_limit_error", "insufficient_quota", "billing_hard_limit_reached", "basispoints_route_rate_limit_exceeded":
	default:
		return raw
	}
	if code == "basispoints_route_rate_limit_exceeded" && len(event["status"]) == 0 && len(event["status_code"]) > 0 {
		return raw
	}
	status := http.StatusTooManyRequests
	if original := event["status"]; len(original) > 0 {
		var observed int
		if json.Unmarshal(original, &observed) != nil || observed < 400 || observed > 599 {
			return raw
		}
		status = observed
	}
	event["type"] = json.RawMessage(`"error"`)
	event["status"], _ = json.Marshal(status)
	event["error"] = response["error"]
	encoded, err := json.Marshal(event)
	if err != nil {
		return raw
	}
	return encoded
}
func (w *bpsWSResponseWriter) finish() error {
	if w.status >= 300 {
		var upstream map[string]json.RawMessage
		_ = json.Unmarshal(w.buffer, &upstream)
		message := upstream["error"]
		if len(message) == 0 {
			message = json.RawMessage(`{"type":"gateway_error","message":"upstream request rejected"}`)
		}
		event := map[string]any{"type": "error", "status": w.status, "error": message}
		if w.streamID != "" {
			event["stream_id"] = w.streamID
		}
		raw, _ := json.Marshal(event)
		w.terminal = true
		if w.onTerminal != nil {
			w.onTerminal()
		}
		return w.send(raw)
	}
	if !w.terminal {
		return errors.New("upstream stream has no terminal response")
	}
	return nil
}
func (w *bpsWSResponseWriter) canceled() {
	if w.terminal {
		return
	}
	w.terminal = true
	if w.onTerminal != nil {
		w.onTerminal()
	}
	response := map[string]any{"id": w.responseID, "status": "cancelled", "output": []any{}}
	event := map[string]any{"type": "response.cancelled", "response": response}
	if w.streamID != "" {
		event["stream_id"] = w.streamID
	}
	raw, _ := json.Marshal(event)
	_ = w.send(raw)
}
