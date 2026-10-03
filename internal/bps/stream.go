package bps

import (
	"bufio"
	"bytes"
	"codex-agent/internal/publicerrors"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	DefaultMaxEventBytes   = 32 << 20
	DefaultMaxHistoryBytes = 32 << 20
	DefaultMaxHistoryItems = 4096
)

var (
	ErrIncompleteStream = errors.New("bps stream ended without a complete terminal event")
	ErrEventTooLarge    = errors.New("bps stream event exceeds the configured limit")
	ErrInvalidEvent     = errors.New("bps stream contains an invalid event")
	ErrResponseIdentity = errors.New("bps response identity changed during the stream")
)

// ItemNormalizer may change an item's public tool identity, while preserving
// fields it does not understand. It runs on added/done items and terminal output.
// An added item can be a stub without a name or arguments. Normalizers should
// defer checks needing those fields until a complete item is available.
type ItemNormalizer func(map[string]json.RawMessage) error

type StreamOptions struct {
	// Unbounded disables application payload/history ceilings. Protocol,
	// identity and cancellation checks remain active.
	Unbounded bool
	// Flush runs after each emitted SSE frame. It should flush the real HTTP
	// response or send the frame to the WebSocket before returning.
	Flush         func() error
	NormalizeItem ItemNormalizer
	// OnComplete runs before the successful terminal is emitted. Returning an
	// error withholds that terminal, e.g. when durable history storage fails.
	OnComplete func(StreamResult) error
	// OnEvent observes each normalized JSON event before its SSE write. It must
	// not write a second copy when a non-discard destination is supplied.
	OnEvent func([]byte) error
	// ClientUsageTransform applies only to emitted bytes. Observers, completion
	// callbacks and Result retain the original upstream accounting and history.
	ClientUsageTransform func([]byte) []byte
	// OnUpstreamTerminal observes a validated real upstream terminal before
	// completion callbacks or downstream delivery. It is not a public-output/
	// TTFT observer and must not emit client data; structured buffering preserves
	// this observation even when later projection or downstream writes fail.
	OnUpstreamTerminal func([]byte) error
	// OnTerminalMetadata observes validated terminal identity/model/usage only
	// when output normalization rejects a terminal before normal accounting.
	// It cannot emit output or mark completion; successful terminals use Result.
	OnTerminalMetadata func([]byte)
	MaxEventBytes      int
	MaxHistoryBytes    int
	MaxHistoryItems    int
	Input              []json.RawMessage
}

// StreamResult is safe to retain as bounded conversation state only when both
// Completed and HistoryComplete are true. It never contains authentication data
// from headers. Response content is nevertheless private conversation data.
type StreamResult struct {
	ResponseID      string
	TerminalType    string
	Status          string
	Completed       bool
	HistoryComplete bool
	Input           []json.RawMessage
	Output          []json.RawMessage
	Usage           json.RawMessage
	Response        json.RawMessage
	Events          int64
}

// TerminalError means a real failure/incomplete/cancelled event was forwarded.
// Callers must not append another API error after the response has been written.
// Payload is available for internal inspection; Error deliberately omits it.
type TerminalError struct {
	Type   string
	Status string
	// Code is a recognized protocol code, never arbitrary upstream text.
	Code string
	// ErrorType is independently observed from the same selected error object.
	// It is never promoted to Code or used to change transport/retry policy.
	ErrorType  string
	ResponseID string
	Payload    json.RawMessage
}

func (e *TerminalError) Error() string {
	status := e.Status
	if status == "" {
		status = "unreported"
	}
	detail := fmt.Sprintf("bps response terminated with %s (status %s)", streamDiagnosticLabel(e.Type), streamDiagnosticLabel(status))
	if code := recognizedTerminalErrorCode(e.Code); code != "" {
		detail += " (code=" + code + ")"
	}
	if errorType := recognizedTerminalErrorType(e.ErrorType); errorType != "" {
		detail += " (error_type=" + errorType + ")"
	}
	return publicerrors.Message(detail)
}

// Native error types are diagnostic classifications, separate from codes.
// Do not infer them from message text or a lower-priority error object.
func recognizedTerminalErrorType(value string) string {
	switch value {
	case "server_error", "invalid_request_error", "rate_limit_error", "authentication_error":
		return value
	}
	return ""
}

func recognizedTerminalErrorCode(code string) string {
	// Event errors can contain full prompts, URLs and credentials. Only known
	// machine codes enter Error()/request logs; the original sanitized event
	// remains available in Payload for internal inspection and client delivery.
	switch code {
	case "serverOverloaded", "server_overloaded", "model_overloaded", "overloaded_error",
		"rateLimitExceeded", "usageLimitExceeded", "basispoints_route_rate_limit_exceeded",
		"rate_limit_exceeded", "rate_limit_error", "usage_limit_reached", "insufficient_quota", "billing_hard_limit_reached",
		"invalid_request", "invalid_request_error", "invalid_encrypted_content", "invalid_prompt",
		"invalid_tool_call", "invalid_tool_output", "tool_call_limit_exceeded", "unsupported_parameter",
		"model_not_found", "model_not_available", "context_length_exceeded", "max_output_tokens",
		"content_filter", "access_denied", "authentication_error", "invalid_api_key", "permission_denied",
		"account_deactivated", "account_expired", "server_error", "internal_error", "upstream_error",
		"api_error", "bad_request", "timeout", "request_timeout", "safety_violation", "usage_policy":
		return code
	}
	return ""
}

// Failed envelopes can carry free-text diagnostics outside the error object.
// Successful content and machine identifiers are not diagnostic prose.
func sanitizeTerminalFreeText(kind string, fields map[string]json.RawMessage) {
	switch kind {
	case "response.failed", "response.incomplete", "response.cancelled", "basispoints.response.cancelled":
	default:
		return
	}
	for _, field := range []string{"message", "detail"} {
		var value string
		if json.Unmarshal(fields[field], &value) == nil {
			if clean := publicerrors.Message(value); clean != value {
				fields[field], _ = json.Marshal(clean)
			}
		}
	}
}

func newTerminalError(kind, status, responseID string, fields, response map[string]json.RawMessage, payload []byte) *TerminalError {
	code, errorType := "", ""
	foundErrorObject := false
	// Match the Responses error precedence: event.error, response.error, then
	// the documented flattened error event. Do not inspect message text.
	for _, value := range []json.RawMessage{fields["error"], response["error"]} {
		if object, err := streamObject(value); err == nil {
			foundErrorObject = true
			code = recognizedTerminalErrorCode(streamString(object, "code"))
			errorType = recognizedTerminalErrorType(streamString(object, "type"))
			break
		}
	}
	if !foundErrorObject && kind == "error" {
		code = recognizedTerminalErrorCode(streamString(fields, "code"))
	}
	return &TerminalError{Type: kind, Status: status, Code: code, ErrorType: errorType, ResponseID: responseID, Payload: bytes.Clone(payload)}
}

// Transformer normalizes independent events while retaining only bounded tool
// identity metadata and the final response history. NewTransformer creates one
// instance per upstream generation; do not reuse it for another response.
type Transformer struct {
	options           StreamOptions
	result            StreamResult
	identities        map[string]map[string]json.RawMessage
	identityBytes     int
	historyBytes      int
	historyItems      int
	doneItems         []streamDoneItem
	doneMetadataBytes int
	doneCipherBytes   int
	doneActionBytes   int
	originalEventType string
	eventType         string
}

type streamDoneItem struct {
	id      string
	index   int
	indexed bool
	digest  [32]byte
	kind    string
	fields  map[string][32]byte
	// Only a completed reasoning cipher is retained, never its whole item. BPS
	// can re-envelope the same reasoning state in response.completed; Codex has
	// already received the done item, so that exact upstream cipher is canonical.
	reasoningCipher json.RawMessage
	withoutCipher   [32]byte
	// Completed native search actions can acquire different source metadata in
	// the terminal. Preserve the upstream object already delivered to Codex.
	searchAction  json.RawMessage
	withoutAction [32]byte
}

func streamDiagnosticLabel(value string) string {
	if value == "" {
		return "<none>"
	}
	if len(value) > 128 {
		return "<redacted>"
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c == ':') {
			return "<redacted>"
		}
	}
	return value
}

// Upstream SSE and its final response can serialize nested objects in different
// key orders. Canonicalize recursively for integrity comparison only, retaining
// json.Number lexical values and leaving the actual forwarded raw JSON intact.
func streamIntegrityDigest(raw json.RawMessage) [32]byte {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return sha256.Sum256(raw)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return sha256.Sum256(raw)
	}
	return sha256.Sum256(canonical)
}

func streamDiagnosticFields(fields map[string]json.RawMessage) map[string][32]byte {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make(map[string][32]byte, min(len(keys), 64))
	for _, key := range keys[:min(len(keys), 64)] {
		result[streamDiagnosticLabel(key)] = streamIntegrityDigest(fields[key])
	}
	return result
}

func streamDiagnosticDiff(previous map[string][32]byte, current map[string]json.RawMessage) string {
	next := streamDiagnosticFields(current)
	var added, removed, changed []string
	for name, sum := range previous {
		if value, exists := next[name]; !exists {
			removed = append(removed, name)
		} else if value != sum {
			changed = append(changed, name)
		}
	}
	for name := range next {
		if _, exists := previous[name]; !exists {
			added = append(added, name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	return fmt.Sprintf("added_fields=%v removed_fields=%v changed_fields=%v", added, removed, changed)
}

func streamDoneMismatch(evidence streamDoneItem, index int, current json.RawMessage, message string) error {
	detail := "terminal_item_missing=true"
	if len(current) != 0 {
		item, _ := streamObject(current)
		detail = "terminal_type=" + streamDiagnosticLabel(streamString(item, "type")) + " terminal_id=" + streamDiagnosticLabel(streamString(item, "id")) + " " + streamDiagnosticDiff(evidence.fields, item)
	}
	return fmt.Errorf("%w: %s; item_type=%s item_id=%s output_index=%d %s", ErrInvalidEvent, message,
		streamDiagnosticLabel(evidence.kind), streamDiagnosticLabel(evidence.id), index, detail)
}

func (t *Transformer) rememberDone(fields map[string]json.RawMessage) error {
	item, err := streamObject(fields["item"])
	if err != nil {
		return err
	}
	evidence := streamDoneItem{id: streamString(item, "id"), kind: streamString(item, "type"), index: len(t.doneItems), digest: streamIntegrityDigest(fields["item"]), fields: streamDiagnosticFields(item)}
	if evidence.kind == "reasoning" && evidence.id != "" && streamString(item, "encrypted_content") != "" {
		evidence.reasoningCipher = bytes.Clone(item["encrypted_content"])
		delete(item, "encrypted_content")
		raw, _ := json.Marshal(item)
		evidence.withoutCipher = streamIntegrityDigest(raw)
	}
	if evidence.kind == "web_search_call" && evidence.id != "" {
		if _, err := streamObject(item["action"]); err == nil {
			evidence.searchAction = bytes.Clone(item["action"])
			delete(item, "action")
			raw, _ := json.Marshal(item)
			evidence.withoutAction = streamIntegrityDigest(raw)
		}
	}
	if raw, present := fields["output_index"]; present {
		if json.Unmarshal(raw, &evidence.index) != nil || evidence.index < 0 {
			return fmt.Errorf("%w: invalid output index", ErrInvalidEvent)
		}
		evidence.indexed = true
	}
	for _, previous := range t.doneItems {
		if evidence.indexed && previous.indexed && previous.index == evidence.index || evidence.id != "" && previous.id == evidence.id {
			if previous.digest != evidence.digest {
				return streamDoneMismatch(previous, evidence.index, fields["item"], "completed output item changed")
			}
			return nil
		}
	}
	if !t.options.Unbounded && len(t.doneItems) >= t.options.MaxHistoryItems {
		return fmt.Errorf("%w: too many completed output items", ErrEventTooLarge)
	}
	t.doneMetadataBytes += len(evidence.id) + len(evidence.kind) + 128
	for name := range evidence.fields {
		t.doneMetadataBytes += len(name) + 32
	}
	t.doneCipherBytes += len(evidence.reasoningCipher)
	t.doneActionBytes += len(evidence.searchAction)
	if !t.options.Unbounded && t.doneMetadataBytes+t.doneCipherBytes+t.doneActionBytes+t.historyBytes > t.options.MaxHistoryBytes {
		return fmt.Errorf("%w: completed-item integrity metadata", ErrEventTooLarge)
	}
	t.doneItems = append(t.doneItems, evidence)
	return nil
}

func (t *Transformer) validateDoneItems(output []json.RawMessage) (bool, error) {
	changed := false
	byID := make(map[string]int, len(output))
	for index, raw := range output {
		item, _ := streamObject(raw)
		if id := streamString(item, "id"); id != "" {
			if _, duplicate := byID[id]; duplicate {
				return false, fmt.Errorf("%w: duplicate terminal output identity", ErrInvalidEvent)
			}
			byID[id] = index
		}
	}
	for _, evidence := range t.doneItems {
		index := evidence.index
		if !evidence.indexed && evidence.id != "" {
			var found bool
			index, found = byID[evidence.id]
			if !found {
				return false, streamDoneMismatch(evidence, index, nil, "terminal output omitted a completed item")
			}
		}
		if index >= len(output) {
			return false, streamDoneMismatch(evidence, index, nil, "terminal output omitted a completed item")
		}
		if streamIntegrityDigest(output[index]) != evidence.digest {
			// Restore only a verified alternative envelope of the same completed
			// reasoning item. Every other field must match and both ciphers must
			// be real nonempty upstream strings. Compaction is intentionally not
			// covered: no observed protocol evidence permits that substitution.
			item, _ := streamObject(output[index])
			if len(evidence.reasoningCipher) > 0 && streamString(item, "type") == "reasoning" &&
				streamString(item, "id") == evidence.id && streamString(item, "encrypted_content") != "" {
				delete(item, "encrypted_content")
				withoutCipher, _ := json.Marshal(item)
				if streamIntegrityDigest(withoutCipher) == evidence.withoutCipher {
					item["encrypted_content"] = evidence.reasoningCipher
					output[index], _ = json.Marshal(item)
					changed = true
					continue
				}
			}
			if len(evidence.searchAction) > 0 && streamString(item, "type") == "web_search_call" && streamString(item, "id") == evidence.id {
				if _, err := streamObject(item["action"]); err == nil {
					delete(item, "action")
					withoutAction, _ := json.Marshal(item)
					if streamIntegrityDigest(withoutAction) == evidence.withoutAction {
						item["action"] = evidence.searchAction
						output[index], _ = json.Marshal(item)
						changed = true
						continue
					}
				}
			}
			return false, streamDoneMismatch(evidence, index, output[index], "terminal output disagrees with completed items")
		}
	}
	// Preserved done fields are now present in final output or unchanged there;
	// release temporary copies before retaining bounded conversation history.
	for i := range t.doneItems {
		t.doneItems[i].reasoningCipher = nil
		t.doneItems[i].searchAction = nil
	}
	t.doneCipherBytes = 0
	t.doneActionBytes = 0
	return changed, nil
}

func NewTransformer(options StreamOptions) *Transformer {
	if options.MaxEventBytes <= 0 || options.MaxEventBytes > DefaultMaxEventBytes {
		options.MaxEventBytes = DefaultMaxEventBytes
	}
	if options.MaxHistoryBytes <= 0 {
		options.MaxHistoryBytes = DefaultMaxHistoryBytes
	}
	if options.MaxHistoryItems <= 0 {
		options.MaxHistoryItems = DefaultMaxHistoryItems
	}
	t := &Transformer{options: options, result: StreamResult{HistoryComplete: true}, identities: make(map[string]map[string]json.RawMessage)}
	t.result.Input = t.retainHistory(options.Input)
	return t
}

func (t *Transformer) Result() StreamResult { return t.result }

func (t *Transformer) retainHistory(items []json.RawMessage) []json.RawMessage {
	if !t.result.HistoryComplete {
		return nil
	}
	count, size := t.historyItems+len(items), t.historyBytes
	for _, item := range items {
		size += len(item)
		if !t.options.Unbounded && (size > t.options.MaxHistoryBytes || count > t.options.MaxHistoryItems) {
			t.result.HistoryComplete = false
			t.result.Input, t.result.Output = nil, nil
			return nil
		}
	}
	t.historyItems, t.historyBytes = count, size
	retained := make([]json.RawMessage, len(items))
	for i := range items {
		retained[i] = bytes.Clone(items[i])
	}
	return retained
}

func streamString(fields map[string]json.RawMessage, key string) string {
	return stringField(fields, key)
}

func streamObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, ErrInvalidEvent
	}
	return fields, nil
}

func (t *Transformer) observeIdentity(fields map[string]json.RawMessage) error {
	id := streamString(fields, "id")
	if id == "" {
		return nil
	}
	if t.result.ResponseID != "" && id != t.result.ResponseID {
		return ErrResponseIdentity
	}
	t.result.ResponseID = id
	return nil
}

func (t *Transformer) normalizeItem(raw json.RawMessage) (json.RawMessage, error) {
	item, err := streamObject(raw)
	if err != nil {
		return nil, err
	}
	id := streamString(item, "id")
	if prior := t.identities[id]; id != "" && prior != nil {
		for _, key := range []string{"type", "name", "namespace", "call_id"} {
			if _, present := item[key]; !present {
				if value, known := prior[key]; known {
					item[key] = value
				}
			}
		}
		if previous, current := streamString(prior, "call_id"), streamString(item, "call_id"); previous != "" && current != "" && previous != current {
			return nil, fmt.Errorf("%w: tool call identity changed", ErrResponseIdentity)
		}
	}
	if t.options.NormalizeItem != nil {
		if err := t.options.NormalizeItem(item); err != nil {
			return nil, err
		}
	}
	id = streamString(item, "id")
	itemType := streamString(item, "type")
	if id != "" && (itemType == "function_call" || itemType == "custom_tool_call" || t.identities[id] != nil) {
		identity := t.identities[id]
		if identity == nil {
			if !t.options.Unbounded && len(t.identities) >= t.options.MaxHistoryItems {
				return nil, fmt.Errorf("%w: too many tool identities", ErrEventTooLarge)
			}
			t.identityBytes += len(id)
			if !t.options.Unbounded && t.identityBytes > t.options.MaxHistoryBytes {
				return nil, fmt.Errorf("%w: tool identity metadata", ErrEventTooLarge)
			}
			identity = make(map[string]json.RawMessage)
			t.identities[id] = identity
		}
		for _, key := range []string{"type", "name", "namespace", "call_id"} {
			if value, ok := item[key]; ok {
				t.identityBytes += len(value) - len(identity[key])
				if !t.options.Unbounded && t.identityBytes > t.options.MaxHistoryBytes {
					return nil, fmt.Errorf("%w: tool identity metadata", ErrEventTooLarge)
				}
				identity[key] = bytes.Clone(value)
			} else if value, ok := identity[key]; ok {
				// Some done items omit identities already supplied on added.
				item[key] = value
			}
		}
	}
	return json.Marshal(item)
}

func (t *Transformer) normalizeArgumentEvent(fields map[string]json.RawMessage, kind string) (string, error) {
	id := streamString(fields, "item_id")
	identity := t.identities[id]
	// Some upstreams only supply the name on the final arguments event. Learn
	// that identity without buffering or replaying already forwarded deltas.
	if id != "" && streamString(fields, "name") != "" {
		item := make(map[string]json.RawMessage)
		for key, value := range identity {
			item[key] = value
		}
		item["id"] = fields["item_id"]
		if item["type"] == nil {
			item["type"] = json.RawMessage(`"function_call"`)
			if strings.HasPrefix(kind, "response.custom_tool_call_input.") {
				item["type"] = json.RawMessage(`"custom_tool_call"`)
			}
		}
		for _, key := range []string{"name", "namespace", "call_id"} {
			if value, ok := fields[key]; ok {
				item[key] = value
			}
		}
		raw, _ := json.Marshal(item)
		if _, err := t.normalizeItem(raw); err != nil {
			return kind, err
		}
		identity = t.identities[id]
	}
	if identity == nil {
		return kind, nil
	}
	if previous, current := streamString(identity, "call_id"), streamString(fields, "call_id"); previous != "" && current != "" && previous != current {
		return kind, fmt.Errorf("%w: tool call identity changed", ErrResponseIdentity)
	}
	for _, key := range []string{"name", "namespace", "call_id"} {
		if value, ok := identity[key]; ok {
			fields[key] = value
		}
	}
	if streamString(identity, "type") == "custom_tool_call" && strings.HasPrefix(kind, "response.function_call_arguments.") {
		kind = strings.Replace(kind, "response.function_call_arguments.", "response.custom_tool_call_input.", 1)
		fields["type"], _ = json.Marshal(kind)
		if arguments, ok := fields["arguments"]; ok {
			fields["input"] = arguments
			delete(fields, "arguments")
		}
	}
	return kind, nil
}

// Transform processes exactly one JSON event, not an SSE envelope. On a valid
// failure terminal it returns both the transformed payload and TerminalError so
// the caller can forward the actual event before handling the failure.
func (t *Transformer) Transform(payload []byte) ([]byte, bool, error) {
	if t.result.TerminalType != "" {
		return nil, true, fmt.Errorf("%w: event after terminal", ErrInvalidEvent)
	}
	if !t.options.Unbounded && len(payload) > t.options.MaxEventBytes {
		return nil, false, ErrEventTooLarge
	}
	if !utf8.Valid(payload) {
		return nil, false, fmt.Errorf("%w: invalid UTF-8", ErrInvalidEvent)
	}
	fields, err := streamObject(payload)
	if err != nil {
		return nil, false, err
	}
	if raw, ok := fields["error"]; ok {
		fields["error"] = publicerrors.JSON(raw, true)
	}
	kind := streamString(fields, "type")
	if kind == "" {
		return nil, false, fmt.Errorf("%w: missing event type", ErrInvalidEvent)
	}
	if kind == "error" {
		clean := publicerrors.JSON(payload, true)
		fields, err = streamObject(clean)
		if err != nil {
			return nil, false, err
		}
	}
	t.originalEventType = kind
	if kind == "error" {
		// BPS uses an error event which the official HTTP Responses parser
		// ignores. Its WebSocket parser also ignores it without an HTTP status,
		// leaving the client waiting until its idle timeout on a reusable socket.
		// Express the same failure using the shared Responses terminal contract.
		failed, e := streamObject(fields["response"])
		if e != nil {
			failed = make(map[string]json.RawMessage)
		}
		detail := fields["error"]
		if _, e = streamObject(detail); e != nil {
			detail = failed["error"]
		}
		if _, e = streamObject(detail); e != nil {
			flat := make(map[string]json.RawMessage)
			for _, name := range []string{"code", "message", "param"} {
				if value, ok := fields[name]; ok {
					flat[name] = value
				}
			}
			if len(flat) == 0 {
				flat["message"] = raw("upstream response failed")
			}
			detail, _ = json.Marshal(flat)
		}
		failed["error"], failed["status"] = detail, raw("failed")
		if streamString(failed, "id") == "" {
			id := streamString(fields, "response_id")
			if id == "" {
				id = t.result.ResponseID
			}
			if id != "" {
				failed["id"] = raw(id)
			}
		}
		fields["response"], _ = json.Marshal(failed)
		kind = "response.failed"
		fields["type"] = raw(kind)
	}
	sanitizeTerminalFreeText(kind, fields)
	t.eventType = kind
	if item, ok := fields["item"]; ok && (kind == "response.output_item.added" || kind == "response.output_item.done") {
		fields["item"], err = t.normalizeItem(item)
		if err != nil {
			return nil, false, err
		}
		if kind == "response.output_item.done" {
			if err = t.rememberDone(fields); err != nil {
				return nil, false, err
			}
		}
	}
	if strings.HasPrefix(kind, "response.function_call_arguments.") || strings.HasPrefix(kind, "response.custom_tool_call_input.") {
		kind, err = t.normalizeArgumentEvent(fields, kind)
		if err != nil {
			return nil, false, err
		}
		t.eventType = kind
	}
	var response map[string]json.RawMessage
	var outputItems []json.RawMessage
	if raw, ok := fields["response"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		response, err = streamObject(raw)
		if err != nil {
			return nil, false, err
		}
		if err = t.observeIdentity(response); err != nil {
			return nil, false, err
		}
		if raw, ok := response["error"]; ok {
			response["error"] = publicerrors.JSON(raw, true)
		}
		sanitizeTerminalFreeText(kind, response)
		if output, ok := response["output"]; ok {
			if json.Unmarshal(output, &outputItems) != nil || bytes.Equal(bytes.TrimSpace(output), []byte("null")) {
				return nil, false, fmt.Errorf("%w: response output is not an array", ErrInvalidEvent)
			}
			for i := range outputItems {
				outputItems[i], err = t.normalizeItem(outputItems[i])
				if err != nil {
					if t.options.OnTerminalMetadata != nil && !errors.Is(err, ErrResponseIdentity) {
						if metadata := terminalObservationMetadata(kind, fields, response, t.result.ResponseID); metadata != nil {
							t.options.OnTerminalMetadata(metadata)
						}
					}
					return nil, false, err
				}
			}
			response["output"], _ = json.Marshal(outputItems)
		}
		fields["response"], _ = json.Marshal(response)
	}
	if responseID := streamString(fields, "response_id"); responseID != "" {
		if t.result.ResponseID != "" && t.result.ResponseID != responseID {
			return nil, false, ErrResponseIdentity
		}
		t.result.ResponseID = responseID
	}
	terminal := false
	switch kind {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "basispoints.response.cancelled", "error":
		terminal = true
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, false, err
	}
	if !t.options.Unbounded && len(encoded) > t.options.MaxEventBytes {
		return nil, false, ErrEventTooLarge
	}
	t.result.Events++
	if !terminal {
		if t.options.OnEvent != nil {
			if err := t.options.OnEvent(encoded); err != nil {
				return nil, false, err
			}
		}
		return encoded, false, nil
	}
	t.result.TerminalType, t.result.Status = kind, streamString(response, "status")
	t.result.Usage = bytes.Clone(response["usage"])
	t.result.Response = bytes.Clone(fields["response"])
	if kind == "response.completed" || kind == "response.done" {
		if response == nil || streamString(response, "id") == "" || t.result.Status != "completed" || response["output"] == nil {
			// A malformed completion must never be forwarded as successful.
			return nil, true, fmt.Errorf("%w: malformed completion", ErrInvalidEvent)
		}
		for _, value := range []json.RawMessage{fields["error"], response["error"]} {
			if len(value) != 0 && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return nil, true, fmt.Errorf("%w: completion also contains an error", ErrInvalidEvent)
			}
		}
		changed, err := t.validateDoneItems(outputItems)
		if err != nil {
			return nil, true, err
		}
		// Most terminals already agree with their done items. Only an actual
		// verified cipher/action reconciliation needs another full JSON encode.
		if changed {
			response["output"], _ = json.Marshal(outputItems)
			fields["response"], _ = json.Marshal(response)
			encoded, _ = json.Marshal(fields)
			if !t.options.Unbounded && len(encoded) > t.options.MaxEventBytes {
				return nil, true, ErrEventTooLarge
			}
			t.result.Response = bytes.Clone(fields["response"])
		}
		t.result.Completed = true
	}
	if response != nil && response["output"] != nil {
		t.result.Output = t.retainHistory(outputItems)
	}
	if t.options.OnUpstreamTerminal != nil {
		if err := t.options.OnUpstreamTerminal(encoded); err != nil {
			t.result.Completed = false
			return nil, true, err
		}
	}
	if !t.result.Completed {
		if t.options.OnEvent != nil {
			if err := t.options.OnEvent(encoded); err != nil {
				return nil, true, err
			}
		}
		return encoded, true, newTerminalError(kind, t.result.Status, t.result.ResponseID, fields, response, encoded)
	}
	if t.options.OnComplete != nil {
		if err := t.options.OnComplete(t.Result()); err != nil {
			t.result.Completed = false
			return nil, true, err
		}
	}
	if t.options.OnEvent != nil {
		if err := t.options.OnEvent(encoded); err != nil {
			t.result.Completed = false
			return nil, true, err
		}
	}
	return encoded, true, nil
}

type streamLineSplitter struct{ scanned int }

func (s *streamLineSplitter) split(data []byte, eof bool) (int, []byte, error) {
	for i := min(s.scanned, len(data)); i < len(data); i++ {
		if data[i] == '\n' || data[i] == '\r' {
			end := i + 1
			if data[i] == '\r' {
				if end == len(data) && !eof {
					s.scanned = i
					return 0, nil, nil
				}
				if end < len(data) && data[end] == '\n' {
					end++
				}
			}
			s.scanned = 0
			return end, data[:i], nil
		}
	}
	s.scanned = len(data)
	if eof && len(data) > 0 {
		s.scanned = 0
		return len(data), data, nil
	}
	return 0, nil, nil
}

// streamCancellation closes real HTTP bodies/io.Pipes on cancellation so a
// blocked Read is interrupted. For readers without Close and blocked writers,
// the caller must arrange deadlines or cancellation; arbitrary io interfaces
// do not have a safe interruption primitive. No pump goroutine buffers data.
func streamCancellation(ctx context.Context, src io.Reader) func() {
	closer, ok := src.(io.Closer)
	if !ok || ctx.Done() == nil {
		return func() {}
	}
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		select {
		case <-ctx.Done():
			_ = closer.Close()
		case <-done:
		}
	}()
	return func() { close(done); <-exited }
}

func streamWrite(ctx context.Context, dst io.Writer, data []byte, flush func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for len(data) > 0 {
		n, err := dst.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if flush != nil {
		return flush()
	}
	return nil
}

func streamEnvelope(envelope []byte, before, after string) []byte {
	if before == after || len(envelope) == 0 {
		return envelope
	}
	lines := bytes.Split(envelope, []byte{'\n'})
	for i, line := range lines {
		field, value, found := bytes.Cut(line, []byte{':'})
		if found && string(field) == "event" && string(bytes.TrimPrefix(value, []byte{' '})) == before {
			lines[i] = []byte("event: " + after)
		}
	}
	return bytes.Join(lines, []byte{'\n'})
}

// Stream transforms and forwards one SSE frame at a time. It returns immediately
// after the terminal frame is flushed; there is no whole-response buffering,
// retry, replay, or fallback to a different upstream. EOF and [DONE] alone never
// establish success. src should be an io.ReadCloser for prompt cancellation.
func Stream(ctx context.Context, dst io.Writer, src io.Reader, options StreamOptions) (StreamResult, error) {
	t := NewTransformer(options)
	if err := ctx.Err(); err != nil {
		return t.Result(), err
	}
	stop := streamCancellation(ctx, src)
	defer stop()
	scanner := newStreamLines(src, t.options)
	var data, envelope bytes.Buffer
	frameBytes, dataLines, lineNumber := 0, 0, 0
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return t.Result(), err
		}
		line := scanner.Bytes()
		if lineNumber == 0 {
			line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
		}
		lineNumber++
		if !utf8.Valid(line) {
			return t.Result(), fmt.Errorf("%w: invalid UTF-8", ErrInvalidEvent)
		}
		frameBytes += len(line) + 1
		if !t.options.Unbounded && frameBytes > t.options.MaxEventBytes {
			return t.Result(), ErrEventTooLarge
		}
		if len(line) != 0 {
			field, value, found := bytes.Cut(line, []byte(":"))
			if found {
				value = bytes.TrimPrefix(value, []byte(" "))
			}
			if string(field) == "data" {
				if dataLines > 0 {
					data.WriteByte('\n')
				}
				data.Write(value)
				dataLines++
			} else {
				envelope.Write(line)
				envelope.WriteByte('\n')
			}
			continue
		}
		if dataLines == 0 {
			if envelope.Len() > 0 {
				envelope.WriteByte('\n')
				if err := streamWrite(ctx, dst, envelope.Bytes(), options.Flush); err != nil {
					return t.Result(), err
				}
			}
			envelope.Reset()
			frameBytes = 0
			continue
		}
		if bytes.Equal(bytes.TrimSpace(data.Bytes()), []byte("[DONE]")) {
			return t.Result(), ErrIncompleteStream
		}
		payload, terminal, transformErr := t.Transform(data.Bytes())
		if len(payload) > 0 {
			if options.ClientUsageTransform != nil {
				payload = options.ClientUsageTransform(payload)
			}
			fixedEnvelope := streamEnvelope(envelope.Bytes(), t.originalEventType, t.eventType)
			if !bytes.Equal(fixedEnvelope, envelope.Bytes()) {
				envelope.Reset()
				envelope.Write(fixedEnvelope)
			}
			envelope.WriteString("data: ")
			envelope.Write(payload)
			envelope.WriteString("\n\n")
			if err := streamWrite(ctx, dst, envelope.Bytes(), options.Flush); err != nil {
				result := t.Result()
				result.Completed = false
				return result, err
			}
		}
		if transformErr != nil || terminal {
			return t.Result(), transformErr
		}
		data.Reset()
		envelope.Reset()
		frameBytes, dataLines = 0, 0
	}
	if err := ctx.Err(); err != nil {
		return t.Result(), err
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return t.Result(), ErrEventTooLarge
		}
		return t.Result(), err
	}
	return t.Result(), ErrIncompleteStream
}

// CompactJSONToSSE bridges a nonstreaming compact response to Codex SSE. Only
// this JSON endpoint necessarily buffers its single bounded response document.
// Unknown items and compaction items without IDs remain opaque and intact.
func CompactJSONToSSE(ctx context.Context, dst io.Writer, src io.Reader, options StreamOptions) (StreamResult, error) {
	t := NewTransformer(options)
	if err := ctx.Err(); err != nil {
		return t.Result(), err
	}
	stop := streamCancellation(ctx, src)
	defer stop()
	var source io.Reader = src
	if !t.options.Unbounded {
		source = io.LimitReader(source, int64(t.options.MaxEventBytes)+1)
	}
	raw, err := io.ReadAll(source)
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return t.Result(), err
	}
	if err = ctx.Err(); err != nil {
		return t.Result(), err
	}
	if !t.options.Unbounded && len(raw) > t.options.MaxEventBytes {
		return t.Result(), ErrEventTooLarge
	}
	if !utf8.Valid(raw) {
		return t.Result(), ErrInvalidEvent
	}
	response, err := streamObject(raw)
	if err != nil {
		return t.Result(), err
	}
	// A complete response can report real usage even when its compacted state
	// cannot be used. This observation never commits output or history.
	defer func() {
		if len(t.result.Response) == 0 && t.options.OnTerminalMetadata != nil {
			if metadata := CompactResponseMetadata(raw); metadata != nil {
				t.options.OnTerminalMetadata(metadata)
			}
		}
	}()
	if _, ok := response["status"]; !ok {
		response["status"] = json.RawMessage(`"completed"`)
	}
	if streamString(response, "id") == "" || response["output"] == nil {
		return t.Result(), fmt.Errorf("%w: malformed compact response", ErrInvalidEvent)
	}
	var items []json.RawMessage
	if json.Unmarshal(response["output"], &items) != nil || bytes.Equal(bytes.TrimSpace(response["output"]), []byte("null")) {
		return t.Result(), ErrInvalidEvent
	}
	kind := "response.completed"
	if status := streamString(response, "status"); status != "completed" {
		if status != "failed" && status != "incomplete" && status != "cancelled" {
			return t.Result(), fmt.Errorf("%w: invalid compact status", ErrInvalidEvent)
		}
		kind = "response." + status
	} else if value := response["error"]; len(value) > 0 && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return t.Result(), fmt.Errorf("%w: completion also contains an error", ErrInvalidEvent)
	}
	if streamString(response, "status") == "completed" {
		hasState := false
		for _, value := range items {
			item, err := streamObject(value)
			if err == nil && streamString(item, "type") == "compaction" && streamString(item, "encrypted_content") != "" {
				hasState = true
				break
			}
		}
		if !hasState {
			return t.Result(), fmt.Errorf("%w: compact response did not include compacted state", ErrInvalidEvent)
		}
	}
	emit := func(event map[string]json.RawMessage) error {
		encoded, _ := json.Marshal(event)
		payload, _, transformErr := t.Transform(encoded)
		if len(payload) != 0 {
			if options.ClientUsageTransform != nil {
				payload = options.ClientUsageTransform(payload)
			}
			frame := append(append([]byte("data: "), payload...), '\n', '\n')
			if err := streamWrite(ctx, dst, frame, options.Flush); err != nil {
				t.result.Completed = false
				return err
			}
		}
		return transformErr
	}
	for i, item := range items {
		if err := emit(map[string]json.RawMessage{"type": json.RawMessage(`"response.output_item.done"`), "output_index": json.RawMessage(strconv.Itoa(i)), "item": item}); err != nil {
			return t.Result(), err
		}
	}
	responseRaw, _ := json.Marshal(response)
	kindRaw, _ := json.Marshal(kind)
	err = emit(map[string]json.RawMessage{"type": kindRaw, "response": responseRaw})
	return t.Result(), err
}
