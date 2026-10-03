package bps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// StructuredValidationError identifies a failure before any public replay:
// schema/projection validation or the caller's authorization/history commit.
// Transport, cancellation, observer and downstream I/O errors deliberately do
// not use this wrapper: callers must not attempt a second write after those.
type StructuredValidationError struct{ Err error }

func (e *StructuredValidationError) Error() string { return e.Err.Error() }
func (e *StructuredValidationError) Unwrap() error { return e.Err }

func structuredValidationError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &StructuredValidationError{Err: err}
}

// StreamStructured validates the entire structured-output turn before exposing
// any model text. This intentionally delays first output until the upstream
// terminal and retains the normalized events for this request in memory. It
// adds no payload ceiling, retries, tool execution or background reader. Calls
// without a format retain Stream's original incremental/backpressure behavior.
func StreamStructured(ctx context.Context, dst io.Writer, src io.Reader, options StreamOptions, format *StructuredFormat) (StreamResult, error) {
	if format == nil {
		return Stream(ctx, dst, src, options)
	}
	var buffered []json.RawMessage
	upstreamOptions := options
	upstreamOptions.Flush = nil
	upstreamOptions.OnComplete = nil
	upstreamOptions.ClientUsageTransform = nil
	upstreamOptions.OnEvent = func(event []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		buffered = append(buffered, bytes.Clone(event))
		return nil
	}
	result, upstreamErr := Stream(ctx, io.Discard, src, upstreamOptions)
	if err := ctx.Err(); err != nil {
		result.Completed = false
		return structuredPrivateResult(result, format), err
	}
	var terminal *TerminalError
	if upstreamErr != nil && !errors.As(upstreamErr, &terminal) {
		result.Completed = false
		return structuredPrivateResult(result, format), upstreamErr
	}
	public, events, err := projectStructuredTurnContext(ctx, result, buffered, format, upstreamErr == nil)
	if err != nil {
		public.Completed = false
		if canceled := ctx.Err(); canceled != nil {
			return public, canceled
		}
		return public, structuredValidationError(err)
	}
	if err := ctx.Err(); err != nil {
		public.Completed = false
		return public, err
	}
	if upstreamErr == nil && options.OnComplete != nil {
		if err := options.OnComplete(public); err != nil {
			public.Completed = false
			if canceled := ctx.Err(); canceled != nil {
				return public, canceled
			}
			return public, structuredValidationError(err)
		}
	}
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			public.Completed = false
			return public, err
		}
		if options.OnEvent != nil {
			if err := options.OnEvent(event); err != nil {
				public.Completed = false
				return public, err
			}
		}
		visible := event
		if options.ClientUsageTransform != nil {
			visible = options.ClientUsageTransform(event)
		}
		frame := append(append([]byte("data: "), visible...), '\n', '\n')
		if err := streamWrite(ctx, dst, frame, options.Flush); err != nil {
			public.Completed = false
			return public, err
		}
	}
	if terminal != nil {
		copy := *terminal
		if len(events) != 0 {
			copy.Payload = bytes.Clone(events[len(events)-1])
		}
		return public, &copy
	}
	return public, nil
}

func structuredInternalItem(item map[string]json.RawMessage, format *StructuredFormat) bool {
	if streamString(item, "type") != "function_call" || streamString(item, "name") != format.ToolName {
		return false
	}
	namespace := bytes.TrimSpace(item["namespace"])
	return len(namespace) == 0 || bytes.Equal(namespace, []byte(`""`)) || bytes.Equal(namespace, []byte("null"))
}

func structuredInternalTool(tool map[string]json.RawMessage, format *StructuredFormat) bool {
	if streamString(tool, "type") != "function" || streamString(tool, "name") != format.ToolName {
		return false
	}
	namespace := bytes.TrimSpace(tool["namespace"])
	return len(namespace) == 0 || bytes.Equal(namespace, []byte(`""`)) || bytes.Equal(namespace, []byte("null"))
}

func structuredClientCall(item map[string]json.RawMessage) bool {
	switch streamString(item, "type") {
	case "function_call", "custom_tool_call", "local_shell_call", "shell_call":
		return true
	case "tool_search_call":
		return streamString(item, "execution") == "client"
	}
	return false
}

func structuredAnyCall(item map[string]json.RawMessage) bool {
	if structuredClientCall(item) {
		return true
	}
	switch streamString(item, "type") {
	case "web_search_call", "tool_search_call", "computer_call", "code_interpreter_call", "image_generation_call", "file_search_call", "mcp_call", "apply_patch_call":
		return true
	}
	return false
}

// Completed hosted searches are work already performed within this response,
// not a competing final answer or a request for the caller to execute a tool.
// Keep the same native families supported by ToolCatalog; never infer completion
// from response.completed or from the presence of an internal formatter.
func structuredCompletedHostedCall(item map[string]json.RawMessage) bool {
	if streamString(item, "status") != "completed" || streamString(item, "id") == "" {
		return false
	}
	switch streamString(item, "type") {
	case "web_search_call":
		execution := streamString(item, "execution")
		return execution == "" || execution == "server"
	case "tool_search_call":
		return streamString(item, "execution") == "server"
	}
	return false
}

func structuredScrubTools(toolsRaw json.RawMessage, format *StructuredFormat) json.RawMessage {
	var tools []json.RawMessage
	if json.Unmarshal(toolsRaw, &tools) != nil {
		return toolsRaw
	}
	filtered := make([]json.RawMessage, 0, len(tools))
	for _, toolRaw := range tools {
		tool, _ := streamObject(toolRaw)
		if !structuredInternalTool(tool, format) {
			filtered = append(filtered, toolRaw)
		}
	}
	return raw(filtered)
}

func structuredScrubInput(inputRaw json.RawMessage, format *StructuredFormat) json.RawMessage {
	var items []json.RawMessage
	if json.Unmarshal(inputRaw, &items) != nil {
		return inputRaw
	}
	filtered := make([]json.RawMessage, 0, len(items))
	for _, itemRaw := range items {
		item, err := streamObject(itemRaw)
		if err != nil {
			filtered = append(filtered, itemRaw)
			continue
		}
		if structuredInternalItem(item, format) {
			continue
		}
		if streamString(item, "type") == "additional_tools" {
			item["tools"] = structuredScrubTools(item["tools"], format)
		}
		if role := streamString(item, "role"); role == "developer" || role == "system" {
			var content []json.RawMessage
			if json.Unmarshal(item["content"], &content) == nil {
				for index, partRaw := range content {
					part, e := streamObject(partRaw)
					if e == nil {
						if _, exists := part["text"]; exists {
							part["text"] = structuredScrubError(part["text"], format.ToolName)
						}
						content[index], _ = json.Marshal(part)
					}
				}
				item["content"], _ = json.Marshal(content)
			}
		}
		encoded, _ := json.Marshal(item)
		filtered = append(filtered, encoded)
	}
	return raw(filtered)
}

func structuredMessageText(item map[string]json.RawMessage) (string, bool) {
	if streamString(item, "type") != "message" || streamString(item, "role") != "assistant" {
		return "", false
	}
	var content []json.RawMessage
	_ = json.Unmarshal(item["content"], &content)
	var text strings.Builder
	refusal := false
	for _, partRaw := range content {
		part, err := streamObject(partRaw)
		if err != nil {
			continue
		}
		switch streamString(part, "type") {
		case "output_text":
			text.WriteString(streamString(part, "text"))
		case "refusal":
			refusal = true
		}
	}
	return text.String(), refusal
}

func structuredScrubResponse(rawResponse json.RawMessage, output []json.RawMessage, format *StructuredFormat) json.RawMessage {
	response, err := streamObject(rawResponse)
	if err != nil {
		return rawResponse
	}
	if output != nil {
		response["output"], _ = json.Marshal(output)
	}
	if _, exists := response["text"]; exists {
		// The actual final content follows the caller's validated format, even
		// though the internal function transport reports its native text mode.
		// Replace only an existing echo; never invent absent response metadata.
		if configured := format.PublicText(); len(configured) != 0 {
			response["text"] = configured
		}
	}
	if toolsRaw, exists := response["tools"]; exists {
		response["tools"] = structuredScrubTools(toolsRaw, format)
	}
	if instructions, exists := response["instructions"]; exists {
		response["instructions"] = structuredScrubError(instructions, format.ToolName)
	}
	if input, exists := response["input"]; exists {
		response["input"] = structuredScrubInput(input, format)
	}
	if choice, err := streamObject(response["tool_choice"]); err == nil && structuredInternalTool(choice, format) {
		delete(response, "tool_choice")
	}
	if _, exists := response["output_text"]; exists && output != nil {
		var text strings.Builder
		for _, rawItem := range output {
			item, _ := streamObject(rawItem)
			value, _ := structuredMessageText(item)
			text.WriteString(value)
		}
		response["output_text"] = raw(text.String())
	}
	if response["error"] != nil {
		response["error"] = structuredScrubError(response["error"], format.ToolName)
	}
	encoded, _ := json.Marshal(response)
	return encoded
}

func structuredScrubError(value json.RawMessage, internalName string) json.RawMessage {
	if internalName == "" {
		return value
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(value, &object) == nil && object != nil {
		for key, child := range object {
			object[key] = structuredScrubError(child, internalName)
		}
		encoded, _ := json.Marshal(object)
		return encoded
	}
	var array []json.RawMessage
	if json.Unmarshal(value, &array) == nil && array != nil {
		for index, child := range array {
			array[index] = structuredScrubError(child, internalName)
		}
		encoded, _ := json.Marshal(array)
		return encoded
	}
	var text string
	if json.Unmarshal(value, &text) == nil && strings.Contains(text, internalName) {
		return raw(strings.ReplaceAll(text, internalName, "structured output"))
	}
	return value
}

// Flat diagnostics can coexist with nested errors on a failed response. Apply
// the same private formatter-name replacement without changing other fields.
func structuredScrubFailureDiagnostics(fields map[string]json.RawMessage, format *StructuredFormat) {
	for _, field := range []string{"code", "message", "param", "detail"} {
		if value, exists := fields[field]; exists {
			fields[field] = structuredScrubError(value, format.ToolName)
		}
	}
}

func structuredPrivateResult(result StreamResult, format *StructuredFormat) StreamResult {
	public, _ := structuredPrivateResultContext(context.Background(), result, format)
	return public
}

func structuredPrivateResultContext(ctx context.Context, result StreamResult, format *StructuredFormat) (StreamResult, error) {
	public := result
	public.Response = nil
	public.Output = make([]json.RawMessage, 0, len(result.Output))
	for _, itemRaw := range result.Output {
		if err := ctx.Err(); err != nil {
			public.Output = nil
			public.Completed = false
			return public, err
		}
		item, _ := streamObject(itemRaw)
		if !structuredInternalItem(item, format) {
			public.Output = append(public.Output, itemRaw)
		}
	}
	if err := ctx.Err(); err != nil {
		public.Output = nil
		public.Completed = false
		return public, err
	}
	public.Response = structuredScrubResponse(result.Response, public.Output, format)
	return public, nil
}

type structuredProjection struct {
	ctx               context.Context
	format            *StructuredFormat
	internalIDs       map[string]bool
	internalIndices   map[int]bool
	removedIndices    []int
	identityNeedsLink bool
	indexMap          map[int]int
	message           json.RawMessage
	text, messageID   string
	internalIndex     int
	failed            bool
}

func (p *structuredProjection) checkContext() error {
	if p.ctx != nil {
		return p.ctx.Err()
	}
	return nil
}

func (p *structuredProjection) rememberArgumentIdentity(event map[string]json.RawMessage) {
	if !strings.HasPrefix(streamString(event, "type"), "response.function_call_arguments.") {
		return
	}
	item := map[string]json.RawMessage{"type": raw("function_call"), "name": event["name"], "namespace": event["namespace"], "id": event["item_id"]}
	if !structuredInternalItem(item, p.format) {
		return
	}
	if id := streamString(event, "item_id"); id != "" {
		p.identityNeedsLink = p.identityNeedsLink || !p.internalIDs[id]
		p.internalIDs[id] = true
	}
	if index, ok := structuredEventIndex(event); ok {
		p.identityNeedsLink = p.identityNeedsLink || !p.internalIndices[index]
		p.internalIndices[index] = true
	}
}

func structuredEventIndex(event map[string]json.RawMessage) (int, bool) {
	var index int
	if json.Unmarshal(event["output_index"], &index) != nil || index < 0 {
		return 0, false
	}
	return index, true
}

func (p *structuredProjection) rememberInternal(rawItem json.RawMessage, index int, indexed bool) {
	item, err := streamObject(rawItem)
	if err != nil || !structuredInternalItem(item, p.format) {
		return
	}
	if id := streamString(item, "id"); id != "" {
		p.internalIDs[id] = true
	}
	if indexed {
		p.internalIndices[index] = true
	}
}

func (p *structuredProjection) internalEvent(event map[string]json.RawMessage) bool {
	if item, err := streamObject(event["item"]); err == nil {
		if structuredInternalItem(item, p.format) || p.internalIDs[streamString(item, "id")] {
			return true
		}
	}
	if id := streamString(event, "item_id"); id != "" && p.internalIDs[id] {
		return true
	}
	if index, ok := structuredEventIndex(event); ok && p.internalIndices[index] {
		return true
	}
	if strings.HasPrefix(streamString(event, "type"), "response.function_call_arguments.") && streamString(event, "name") == p.format.ToolName {
		namespace := bytes.TrimSpace(event["namespace"])
		return len(namespace) == 0 || bytes.Equal(namespace, []byte(`""`)) || bytes.Equal(namespace, []byte("null"))
	}
	return false
}

func structuredValidateArgumentsEvents(events []json.RawMessage, p *structuredProjection, arguments string) error {
	var deltas strings.Builder
	seenDelta := false
	for _, rawEvent := range events {
		if err := p.checkContext(); err != nil {
			return err
		}
		event, _ := streamObject(rawEvent)
		if !p.internalEvent(event) {
			continue
		}
		switch streamString(event, "type") {
		case "response.function_call_arguments.delta":
			var delta string
			if json.Unmarshal(event["delta"], &delta) != nil {
				return fmt.Errorf("%w: structured argument delta is not text", ErrInvalidEvent)
			}
			deltas.WriteString(delta)
			seenDelta = true
		case "response.function_call_arguments.done":
			var value string
			if json.Unmarshal(event["arguments"], &value) != nil || value != arguments {
				return fmt.Errorf("%w: structured argument completion differs from terminal", ErrInvalidEvent)
			}
		}
	}
	if seenDelta && deltas.String() != arguments {
		return fmt.Errorf("%w: structured argument deltas differ from terminal", ErrInvalidEvent)
	}
	return nil
}

func structuredValidateInternalIdentity(events []json.RawMessage, p *structuredProjection, final map[string]json.RawMessage) error {
	id, call := streamString(final, "id"), streamString(final, "call_id")
	if id == "" || call == "" {
		return fmt.Errorf("%w: structured final call has no identity", ErrResponseIdentity)
	}
	if status := streamString(final, "status"); status != "" && status != "completed" {
		return fmt.Errorf("%w: structured final call is not completed", ErrInvalidEvent)
	}
	for observed := range p.internalIDs {
		if observed != id {
			return fmt.Errorf("%w: structured call identity changed", ErrResponseIdentity)
		}
	}
	for observed := range p.internalIndices {
		if observed != p.internalIndex {
			return fmt.Errorf("%w: structured output index changed", ErrResponseIdentity)
		}
	}
	for _, rawEvent := range events {
		if err := p.checkContext(); err != nil {
			return err
		}
		event, _ := streamObject(rawEvent)
		if !p.internalEvent(event) {
			continue
		}
		if eventID := streamString(event, "item_id"); eventID != "" && eventID != id {
			return fmt.Errorf("%w: structured event identity changed", ErrResponseIdentity)
		}
		if item, err := streamObject(event["item"]); err == nil {
			if itemID := streamString(item, "id"); itemID != "" && itemID != id {
				return fmt.Errorf("%w: structured item identity changed", ErrResponseIdentity)
			}
			if itemCall := streamString(item, "call_id"); itemCall != "" && itemCall != call {
				return fmt.Errorf("%w: structured call identity changed", ErrResponseIdentity)
			}
			if name := streamString(item, "name"); name != "" && !structuredInternalItem(item, p.format) {
				return fmt.Errorf("%w: structured item changed tool identity", ErrResponseIdentity)
			}
		}
		if eventCall := streamString(event, "call_id"); eventCall != "" && eventCall != call {
			return fmt.Errorf("%w: structured event call identity changed", ErrResponseIdentity)
		}
	}
	return nil
}

func structuredValidateTextEvents(ctx context.Context, events, output []json.RawMessage) error {
	type contentKey struct {
		item, part int
		kind       string
	}
	expected := map[contentKey]string{}
	ids := map[string]int{}
	for outputIndex, itemRaw := range output {
		if err := ctx.Err(); err != nil {
			return err
		}
		item, err := streamObject(itemRaw)
		if err != nil {
			return err
		}
		if id := streamString(item, "id"); id != "" {
			if _, exists := ids[id]; exists {
				return fmt.Errorf("%w: duplicate final item identity", ErrResponseIdentity)
			}
			ids[id] = outputIndex
		}
		if streamString(item, "type") != "message" || streamString(item, "role") != "assistant" {
			continue
		}
		var content []json.RawMessage
		_ = json.Unmarshal(item["content"], &content)
		for partIndex, partRaw := range content {
			if err := ctx.Err(); err != nil {
				return err
			}
			part, _ := streamObject(partRaw)
			kind := streamString(part, "type")
			if kind == "output_text" {
				expected[contentKey{outputIndex, partIndex, kind}] = streamString(part, "text")
			}
			if kind == "refusal" {
				expected[contentKey{outputIndex, partIndex, kind}] = streamString(part, "refusal")
			}
		}
	}
	deltas := map[contentKey]*strings.Builder{}
	for _, rawEvent := range events {
		if err := ctx.Err(); err != nil {
			return err
		}
		event, err := streamObject(rawEvent)
		if err != nil {
			return err
		}
		kind := streamString(event, "type")
		index, indexed := structuredEventIndex(event)
		if len(event["output_index"]) != 0 && !indexed {
			return fmt.Errorf("%w: invalid output index", ErrResponseIdentity)
		}
		itemID := streamString(event, "item_id")
		if item, e := streamObject(event["item"]); e == nil {
			if nestedID := streamString(item, "id"); nestedID != "" {
				if itemID != "" && nestedID != itemID {
					return fmt.Errorf("%w: conflicting event item identities", ErrResponseIdentity)
				}
				itemID = nestedID
			}
		}
		if itemID != "" {
			resolved, exists := ids[itemID]
			if !exists || indexed && index != resolved {
				return fmt.Errorf("%w: event item identity and output index differ", ErrResponseIdentity)
			}
			index, indexed = resolved, true
		}
		if indexed && (index < 0 || index >= len(output)) {
			return fmt.Errorf("%w: event output index has no final item", ErrResponseIdentity)
		}
		isText := kind == "response.output_text.delta" || kind == "response.output_text.done"
		isRefusal := kind == "response.refusal.delta" || kind == "response.refusal.done"
		if !isText && !isRefusal {
			continue
		}
		if !indexed {
			return fmt.Errorf("%w: text event has no item identity", ErrResponseIdentity)
		}
		var partIndex int
		if value := event["content_index"]; len(value) > 0 && (json.Unmarshal(value, &partIndex) != nil || partIndex < 0) {
			return ErrInvalidEvent
		}
		contentType := "output_text"
		if isRefusal {
			contentType = "refusal"
		}
		key := contentKey{index, partIndex, contentType}
		text, exists := expected[key]
		if !exists {
			return fmt.Errorf("%w: text event has no final content", ErrResponseIdentity)
		}
		isDone := strings.HasSuffix(kind, ".done")
		field := "delta"
		if isDone {
			field = "text"
			if isRefusal {
				field = "refusal"
			}
		}
		var value string
		if json.Unmarshal(event[field], &value) != nil {
			return ErrInvalidEvent
		}
		if isDone {
			if value != text {
				return fmt.Errorf("%w: text completion differs from terminal", ErrInvalidEvent)
			}
		} else {
			if deltas[key] == nil {
				deltas[key] = &strings.Builder{}
			}
			deltas[key].WriteString(value)
		}
	}
	for key, value := range deltas {
		if err := ctx.Err(); err != nil {
			return err
		}
		if value.String() != expected[key] {
			return fmt.Errorf("%w: text deltas differ from terminal", ErrInvalidEvent)
		}
	}
	return nil
}

func projectStructuredTurn(result StreamResult, events []json.RawMessage, format *StructuredFormat, success bool) (StreamResult, []json.RawMessage, error) {
	return projectStructuredTurnContext(context.Background(), result, events, format, success)
}

func projectStructuredTurnContext(ctx context.Context, result StreamResult, events []json.RawMessage, format *StructuredFormat, success bool) (StreamResult, []json.RawMessage, error) {
	public, err := structuredPrivateResultContext(ctx, result, format)
	if err != nil {
		return public, nil, err
	}
	p := &structuredProjection{ctx: ctx, format: format, internalIDs: map[string]bool{}, internalIndices: map[int]bool{}, indexMap: map[int]int{}, internalIndex: -1, failed: !success}
	var internal []map[string]json.RawMessage
	publicCalls, allPublicCalls, incompatibleCalls, refusal := 0, 0, 0, false
	var finalText strings.Builder
	for index, itemRaw := range result.Output {
		if err := ctx.Err(); err != nil {
			return public, nil, err
		}
		item, _ := streamObject(itemRaw)
		if structuredInternalItem(item, format) {
			internal = append(internal, item)
			p.rememberInternal(itemRaw, index, true)
			p.internalIndex = index
			continue
		}
		if structuredClientCall(item) {
			publicCalls++
		}
		if structuredAnyCall(item) {
			allPublicCalls++
			if !structuredCompletedHostedCall(item) {
				incompatibleCalls++
			}
		}
		text, denied := structuredMessageText(item)
		finalText.WriteString(text)
		refusal = refusal || denied
	}
	for _, eventRaw := range events {
		if err := ctx.Err(); err != nil {
			return public, nil, err
		}
		event, _ := streamObject(eventRaw)
		p.rememberArgumentIdentity(event)
		index, indexed := structuredEventIndex(event)
		p.rememberInternal(event["item"], index, indexed)
		if response, err := streamObject(event["response"]); err == nil {
			var output []json.RawMessage
			_ = json.Unmarshal(response["output"], &output)
			for i, item := range output {
				p.rememberInternal(item, i, true)
			}
		}
	}
	// Names may be learned only after the anonymous added item, and the later
	// arguments event may omit output_index. Revisit the buffered identities so
	// hidden items also contribute to correct public index remapping on failure.
	if p.identityNeedsLink {
		for _, eventRaw := range events {
			if err := ctx.Err(); err != nil {
				return public, nil, err
			}
			event, _ := streamObject(eventRaw)
			if !p.internalEvent(event) {
				continue
			}
			if index, ok := structuredEventIndex(event); ok {
				p.internalIndices[index] = true
			}
			if item, err := streamObject(event["item"]); err == nil {
				if id := streamString(item, "id"); id != "" {
					p.internalIDs[id] = true
				}
			}
		}
	}
	for index := range p.internalIndices {
		p.removedIndices = append(p.removedIndices, index)
	}
	sort.Ints(p.removedIndices)
	if success {
		if err := structuredValidateTextEvents(ctx, events, result.Output); err != nil {
			return public, nil, err
		}
		if len(internal) > 1 || len(internal) == 1 && (incompatibleCalls > 0 || finalText.Len() > 0 || refusal) {
			return public, nil, &structuredMixedError{observation: StructuredMixedObservation{
				FormatterCount: len(internal), ClientCallCount: publicCalls, OtherCallCount: allPublicCalls - publicCalls,
				HasText: finalText.Len() > 0, HasRefusal: refusal,
			}}
		}
		if len(internal) == 0 && (len(p.internalIDs) > 0 || len(p.internalIndices) > 0) {
			return public, nil, fmt.Errorf("%w: structured call missing from terminal", ErrResponseIdentity)
		}
		if len(internal) == 1 {
			if err := structuredValidateInternalIdentity(events, p, internal[0]); err != nil {
				return public, nil, err
			}
			var arguments string
			if json.Unmarshal(internal[0]["arguments"], &arguments) != nil {
				return public, nil, errors.New("structured output arguments must be text")
			}
			validated, err := format.ValidateArguments(arguments)
			if err != nil {
				return public, nil, err
			}
			if validated != arguments {
				return public, nil, errors.New("structured output validation changed original arguments")
			}
			if err = structuredValidateArgumentsEvents(events, p, arguments); err != nil {
				return public, nil, err
			}
			id := streamString(internal[0], "id")
			if id == "" {
				id = streamString(internal[0], "call_id")
			}
			if id == "" {
				id = result.ResponseID + ":" + strconv.Itoa(p.internalIndex)
			}
			digest := sha256.Sum256([]byte(id + "\x00" + format.ToolName))
			p.messageID = "msg_" + hex.EncodeToString(digest[:16])
			p.text = arguments
			p.message = raw(map[string]any{"id": p.messageID, "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": arguments, "annotations": []any{}}}})
		} else {
			finalMessages := 0
			for _, itemRaw := range result.Output {
				if err := ctx.Err(); err != nil {
					return public, nil, err
				}
				item, _ := streamObject(itemRaw)
				if streamString(item, "type") != "message" || streamString(item, "role") != "assistant" {
					continue
				}
				text, denied := structuredMessageText(item)
				if denied && text == "" {
					continue
				}
				if streamString(item, "phase") == "commentary" && publicCalls > 0 {
					continue
				}
				if _, err := format.ValidateArguments(text); err != nil {
					return public, nil, err
				}
				finalMessages++
			}
			if finalMessages == 0 && publicCalls == 0 && !refusal {
				return public, nil, errors.New("structured response has no final JSON object")
			}
		}
	}
	public.Output = make([]json.RawMessage, 0, len(result.Output))
	for oldIndex, itemRaw := range result.Output {
		if err := ctx.Err(); err != nil {
			return public, nil, err
		}
		item, _ := streamObject(itemRaw)
		if structuredInternalItem(item, format) {
			if !success {
				continue
			}
			itemRaw = p.message
		}
		p.indexMap[oldIndex] = len(public.Output)
		public.Output = append(public.Output, itemRaw)
	}
	public.Response = structuredScrubResponse(result.Response, public.Output, format)
	if !success {
		if response, err := streamObject(public.Response); err == nil {
			structuredScrubFailureDiagnostics(response, format)
			public.Response, _ = json.Marshal(response)
		}
	}
	var projected []json.RawMessage
	emittedInternal := false
	for _, eventRaw := range events {
		if err := ctx.Err(); err != nil {
			return public, nil, err
		}
		event, err := streamObject(eventRaw)
		if err != nil {
			return public, nil, err
		}
		kind := streamString(event, "type")
		if p.internalEvent(event) {
			switch kind {
			case "response.output_item.added", "response.output_item.done", "response.function_call_arguments.delta", "response.function_call_arguments.done":
				// These documented events have a precise public text projection.
			default:
				return public, nil, fmt.Errorf("%w: unsupported event references an internal structured item", ErrInvalidEvent)
			}
			if success && !emittedInternal {
				projected = append(projected, p.messageEvents()...)
				emittedInternal = true
			}
			continue
		}
		isTerminal := kind == result.TerminalType
		if success && isTerminal && len(internal) == 1 && !emittedInternal {
			projected = append(projected, p.messageEvents()...)
			emittedInternal = true
		}
		if index, ok := structuredEventIndex(event); ok {
			if updated, exists := p.indexMap[index]; exists {
				event["output_index"] = raw(updated)
			} else if p.failed {
				removed := sort.SearchInts(p.removedIndices, index)
				event["output_index"] = raw(index - removed)
			}
		}
		if responseRaw, exists := event["response"]; exists {
			if isTerminal {
				event["response"] = public.Response
			} else {
				response, _ := streamObject(responseRaw)
				var output []json.RawMessage
				_ = json.Unmarshal(response["output"], &output)
				filtered := make([]json.RawMessage, 0, len(output))
				for _, itemRaw := range output {
					if err := ctx.Err(); err != nil {
						return public, nil, err
					}
					item, _ := streamObject(itemRaw)
					if structuredInternalItem(item, format) {
						if success && len(p.message) != 0 {
							filtered = append(filtered, p.message)
						}
					} else {
						filtered = append(filtered, itemRaw)
					}
				}
				if response["output"] == nil {
					filtered = nil
				}
				event["response"] = structuredScrubResponse(responseRaw, filtered, format)
			}
		}
		if event["error"] != nil {
			event["error"] = structuredScrubError(event["error"], format.ToolName)
		}
		if !success && isTerminal {
			structuredScrubFailureDiagnostics(event, format)
		}
		encoded, _ := json.Marshal(event)
		projected = append(projected, encoded)
	}
	for index, eventRaw := range projected {
		if err := ctx.Err(); err != nil {
			return public, nil, err
		}
		event, _ := streamObject(eventRaw)
		event["sequence_number"] = raw(index)
		projected[index], _ = json.Marshal(event)
	}
	public.Events = int64(len(projected))
	return public, projected, nil
}

func (p *structuredProjection) messageEvents() []json.RawMessage {
	index := p.indexMap[p.internalIndex]
	part := map[string]any{"type": "output_text", "text": p.text, "annotations": []any{}}
	stub := map[string]any{"id": p.messageID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}
	return []json.RawMessage{
		raw(map[string]any{"type": "response.output_item.added", "output_index": index, "item": stub}),
		raw(map[string]any{"type": "response.content_part.added", "output_index": index, "item_id": p.messageID, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}),
		raw(map[string]any{"type": "response.output_text.delta", "output_index": index, "item_id": p.messageID, "content_index": 0, "delta": p.text}),
		raw(map[string]any{"type": "response.output_text.done", "output_index": index, "item_id": p.messageID, "content_index": 0, "text": p.text}),
		raw(map[string]any{"type": "response.content_part.done", "output_index": index, "item_id": p.messageID, "content_index": 0, "part": part}),
		raw(map[string]any{"type": "response.output_item.done", "output_index": index, "item": p.message}),
	}
}
