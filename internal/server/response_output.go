package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"sort"
)

// Codex can send complete items through response.output_item.done and omit
// response.output from the terminal event. The official client consumes those
// item events independently (codex-api/src/sse/responses.rs). JSON callers need
// the same items assembled into their single response, including opaque items.
//
// A map avoids allocating an array proportional to an untrusted output_index.
// The legacy route bounds item count and aggregate bytes independently of SSE
// events. The lightweight official route opts out of these local ceilings.
const maxCollectedResponseItems = 8192

type responseOutputAccumulator struct {
	unbounded  bool
	items      map[int64]json.RawMessage
	byID       map[string]int64
	byCall     map[string]int64
	implicit   map[int64]bool
	bytes      int
	next       int64
	responseID string
}

func (a *responseOutputAccumulator) consume(payload []byte) (json.RawMessage, bool, error) {
	response, terminal, err := gatewayHTTPTerminalResponse(payload)
	if !terminal {
		if err != nil {
			return nil, false, err
		}
		return nil, false, a.observe(payload)
	}
	// A terminal cannot combine a top-level response_id with a different
	// nested response.id. Use the same raw-ID rule as scheduling/accounting,
	// before returning an object that loses its enclosing event metadata.
	var identity gatewayHTTPCooldownIdentity
	if a.responseID != "" {
		identity.digest, identity.known = sha256.Sum256([]byte(a.responseID)), true
	}
	if jsonObject(response) && !identity.accepts(payload) {
		return nil, true, errors.New("upstream stream contains conflicting response IDs")
	}
	if identityErr := a.acceptResponseID(responseItemID(response)); identityErr != nil {
		if err != nil {
			return response, true, err
		}
		return nil, true, identityErr
	}
	if len(a.items) == 0 || !jsonObject(response) {
		return response, true, err
	}
	merged, mergeErr := a.complete(response)
	if mergeErr != nil {
		// Preserve an upstream failure's diagnostic and measured usage even if
		// its partial output cannot be assembled within the JSON response limit.
		if err != nil {
			return response, true, err
		}
		return nil, true, mergeErr
	}
	var failure *upstreamResponseError
	if errors.As(err, &failure) {
		copy := *failure
		copy.Response = merged
		err = &copy
	}
	return merged, true, err
}

func (a *responseOutputAccumulator) observe(payload []byte) error {
	var event struct {
		Type       string          `json:"type"`
		Index      json.RawMessage `json:"output_index"`
		Item       json.RawMessage `json:"item"`
		Response   json.RawMessage `json:"response"`
		ResponseID string          `json:"response_id"`
	}
	if gatewayWSDecodeExact(payload, map[string]any{
		"type": &event.Type, "output_index": &event.Index, "item": &event.Item,
		"response": &event.Response, "response_id": &event.ResponseID,
	}) != nil {
		return nil
	}
	if event.Type == "response.created" || event.Type == "response.in_progress" {
		return a.acceptResponseID(responseItemID(event.Response))
	}
	if event.Type != "response.output_item.done" {
		return nil
	}
	if err := a.acceptResponseID(event.ResponseID); err != nil {
		return err
	}
	if !jsonObject(event.Item) {
		return errors.New("upstream completed output item is missing or invalid")
	}
	var identity responseOutputItemIdentity
	identity.decode(event.Item)
	callKey := identity.callKey()
	index := a.next
	if len(event.Index) > 0 {
		if bytes.Equal(bytes.TrimSpace(event.Index), []byte("null")) || json.Unmarshal(event.Index, &index) != nil || index < 0 {
			return errors.New("upstream output_index must be a nonnegative integer")
		}
	} else {
		// Older compatible sources omit output_index. Retain arrival order,
		// but a repeated completion for the same item ID is an update, not a
		// second item (and must not replay a tool call twice to JSON clients).
		known, exists := a.byID[identity.ID]
		if identity.ID != "" && exists {
			index = known
		} else if known, exists := a.byCall[callKey]; exists && callKey != "" {
			index = known
		} else {
			for a.items[index] != nil {
				if index == math.MaxInt64 {
					return errors.New("upstream output index exceeds integer capacity")
				}
				index++
			}
			a.next = index
			if index < math.MaxInt64 {
				a.next++
			}
		}
	}
	// One completed item cannot occupy two output slots. In particular, an
	// unindexed legacy item must not become a second executable tool call when
	// a later event assigns the same ID a different explicit index.
	if known, exists := a.byID[identity.ID]; identity.ID != "" && exists && known != index {
		return errors.New("upstream output item ID conflicts with an earlier output_index")
	}
	if known, exists := a.byCall[callKey]; callKey != "" && exists && known != index {
		return errors.New("upstream tool call ID conflicts with an earlier output_index")
	}
	previous, exists := a.items[index]
	var previousIdentity responseOutputItemIdentity
	var previousCallKey string
	if exists {
		previousIdentity.decode(previous)
		previousCallKey = previousIdentity.callKey()
		if callKey != "" && previousCallKey != "" && callKey != previousCallKey {
			return errors.New("upstream output_index conflicts with an earlier tool call ID")
		}
	}
	if exists && !a.implicit[index] && identity.ID != "" {
		if previousIdentity.ID != "" && previousIdentity.ID != identity.ID {
			return errors.New("upstream output_index conflicts with an earlier output item ID")
		}
	}
	if exists && len(event.Index) > 0 && a.implicit[index] {
		// An arrival-order slot is not an upstream output_index. Only the
		// same identified item can safely claim it; otherwise fail explicitly
		// rather than overwriting a distinct message or opaque reasoning item.
		sameID := identity.ID != "" && previousIdentity.ID == identity.ID
		sameCall := callKey != "" && previousCallKey == callKey
		if !sameID && !sameCall {
			return errors.New("upstream output_index conflicts with an unindexed output item")
		}
	}
	if !a.unbounded && !exists && len(a.items) >= maxCollectedResponseItems {
		return errors.New("upstream response has too many output items")
	}
	// Include the array separator for each item in the accounting. Repeated
	// terminal item events replace their own index rather than duplicating it.
	size, sizeErr := responseCollectionSize(a.bytes-len(previous), len(event.Item))
	if sizeErr != nil {
		return sizeErr
	}
	if !exists {
		size, sizeErr = responseCollectionSize(size, 1)
		if sizeErr != nil {
			return sizeErr
		}
	}
	if !a.unbounded && size > maxBody-2 {
		return errors.New("upstream collected response exceeds 32 MiB")
	}
	if a.items == nil {
		a.items = make(map[int64]json.RawMessage)
	}
	if !exists && len(event.Index) == 0 {
		if a.implicit == nil {
			a.implicit = make(map[int64]bool)
		}
		a.implicit[index] = true
	} else if len(event.Index) > 0 {
		delete(a.implicit, index)
	}
	if exists {
		if oldIndex, known := a.byID[previousIdentity.ID]; known && oldIndex == index {
			delete(a.byID, previousIdentity.ID)
		}
		if oldIndex, known := a.byCall[previousCallKey]; known && oldIndex == index {
			delete(a.byCall, previousCallKey)
		}
	}
	a.items[index] = event.Item
	if identity.ID != "" {
		if a.byID == nil {
			a.byID = make(map[string]int64)
		}
		a.byID[identity.ID] = index
	}
	if callKey != "" {
		if a.byCall == nil {
			a.byCall = make(map[string]int64)
		}
		a.byCall[callKey] = index
	}
	a.bytes = size
	return nil
}

type responseOutputItemIdentity struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	CallID string `json:"call_id"`
}

// Only canonical protocol names identify or deduplicate executable items.
// Differently-cased extension fields remain opaque in their original item.
func (i *responseOutputItemIdentity) decode(raw json.RawMessage) {
	fields, err := gatewayWSParseFields(raw)
	if err != nil {
		return
	}
	// A malformed optional identity must not randomly suppress other valid
	// members as a map-iteration early return would. Preserve the previous
	// struct decoder's partial-field behavior, without its folded aliases.
	_ = json.Unmarshal(fields["id"], &i.ID)
	_ = json.Unmarshal(fields["type"], &i.Type)
	_ = json.Unmarshal(fields["call_id"], &i.CallID)
}

func (i responseOutputItemIdentity) callKey() string {
	if i.CallID == "" {
		return ""
	}
	// Call outputs deliberately share call_id with their invocation. Restrict
	// deduplication to executable calls, and keep different tool types distinct.
	switch i.Type {
	case "function_call", "custom_tool_call", "local_shell_call", "tool_search_call":
		return i.Type + "\x00" + i.CallID
	}
	return ""
}

func responseItemID(raw json.RawMessage) string {
	var identity struct {
		ID string `json:"id"`
	}
	if gatewayWSDecodeExact(raw, map[string]any{"id": &identity.ID}) != nil {
		return ""
	}
	return identity.ID
}

func (a *responseOutputAccumulator) acceptResponseID(id string) error {
	if id == "" {
		return nil
	}
	if a.responseID != "" && a.responseID != id {
		return errors.New("upstream stream contains conflicting response IDs")
	}
	a.responseID = id
	return nil
}

func (a *responseOutputAccumulator) complete(response json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(response, &fields) != nil || fields == nil {
		return nil, errors.New("upstream response missing or invalid")
	}
	var terminalItems map[int64]json.RawMessage
	var terminalOrder []int64
	terminalFieldsRestored := false
	if output, exists := fields["output"]; exists {
		var items []json.RawMessage
		if json.Unmarshal(output, &items) != nil {
			return response, nil
		}
		if len(items) > 0 {
			// Some sources put only the final message in terminal output. The
			// CLI already consumed every output_item.done independently. Restore
			// omitted items only when every terminal ID matches a collected item;
			// anonymous/new IDs cannot establish a safe merge, so remain opaque.
			terminalItems = make(map[int64]json.RawMessage, len(items))
			for _, item := range items {
				id := responseItemID(item)
				index, known := a.byID[id]
				if id == "" || !known {
					return response, nil
				}
				if _, duplicate := terminalItems[index]; duplicate {
					return nil, errors.New("upstream terminal output contains duplicate item IDs")
				}
				terminalItems[index] = item
				terminalOrder = append(terminalOrder, index)
			}
			for index, item := range terminalItems {
				merged, restored, err := completeResponseOutputItem(a.items[index], item, a.unbounded)
				if err != nil {
					return nil, err
				}
				terminalItems[index] = merged
				terminalFieldsRestored = terminalFieldsRestored || restored
			}
			if len(terminalItems) == len(a.items) && !terminalFieldsRestored {
				return response, nil
			}
		}
	}
	indices := make([]int64, 0, len(a.items))
	for index := range a.items {
		indices = append(indices, index)
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
	if len(terminalItems) == len(a.items) {
		// A complete terminal array already defines its order. Filling a field
		// absent from one item must not reorder otherwise authoritative output.
		indices = terminalOrder
	}
	size, err := responseCollectionSize(a.bytes, 1)
	if err != nil {
		return nil, err
	}
	for index, item := range terminalItems {
		size, err = responseCollectionSize(size-len(a.items[index]), len(item))
		if err != nil {
			return nil, err
		}
	}
	if !a.unbounded && size > maxBody {
		return nil, errors.New("upstream collected response exceeds 32 MiB")
	}
	output := make([]byte, 0, size)
	output = append(output, '[')
	for i, index := range indices {
		if i > 0 {
			output = append(output, ',')
		}
		item := a.items[index]
		if terminal, exists := terminalItems[index]; exists {
			item = terminal
		}
		output = append(output, item...)
	}
	output = append(output, ']')
	// Check before marshaling as well as afterwards: JSON escaping and metadata
	// must not turn a bounded item collection into an oversized response.
	assembledSize, err := responseCollectionSize(len(response)-len(fields["output"]), len(output))
	if err != nil {
		return nil, err
	}
	if _, exists := fields["output"]; !exists {
		extra := len(`"output":`)
		if len(fields) > 0 {
			extra++
		}
		assembledSize, err = responseCollectionSize(assembledSize, extra)
		if err != nil {
			return nil, err
		}
	}
	if !a.unbounded && assembledSize > maxBody {
		return nil, errors.New("upstream collected response exceeds 32 MiB")
	}
	fields["output"] = output
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	// This is JSON, not embedded HTML. Escaping every '<', '>' or '&' can
	// inflate a valid large message sixfold and exceed the collection budget.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(fields); err != nil {
		return nil, errors.New("cannot assemble upstream response")
	}
	merged := bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'})
	if !a.unbounded && len(merged) > maxBody {
		return nil, errors.New("upstream collected response exceeds 32 MiB")
	}
	return merged, nil
}

// CLI history consumes output_item.done independently of terminal metadata. A
// slim terminal item with the same ID must not erase opaque reasoning, tool
// arguments, or message phase that the client already received. Only missing
// top-level fields are restored: explicit nulls, empty values and nested objects
// remain authoritative, so previously returned tool content is not revived.
func completeResponseOutputItem(completed, terminal json.RawMessage, unbounded ...bool) (json.RawMessage, bool, error) {
	if bytes.Equal(bytes.TrimSpace(completed), bytes.TrimSpace(terminal)) {
		return terminal, false, nil
	}
	var previous, current map[string]json.RawMessage
	if json.Unmarshal(completed, &previous) != nil || json.Unmarshal(terminal, &current) != nil || previous == nil || current == nil {
		return nil, false, errors.New("upstream completed output item is invalid")
	}
	// Sharing an item ID cannot turn a completed tool into a different tool or
	// message. Do not splice fields from conflicting identities into one item.
	for _, field := range []string{"type", "call_id", "name", "namespace", "role"} {
		oldValue, oldExists := previous[field]
		newValue, newExists := current[field]
		if !oldExists || !newExists {
			continue
		}
		if field == "namespace" && bytes.Equal(bytes.TrimSpace(newValue), []byte("null")) {
			continue // Clearing an optional namespace is explicit, not missing.
		}
		var oldIdentity, newIdentity string
		if json.Unmarshal(oldValue, &oldIdentity) != nil || json.Unmarshal(newValue, &newIdentity) != nil || oldIdentity != newIdentity {
			return nil, false, errors.New("upstream terminal output item conflicts with completed item identity")
		}
	}
	if encrypted, hadEncrypted := previous["encrypted_function_args"]; hadEncrypted {
		_, hasEncrypted := current["encrypted_function_args"]
		arguments, hasArguments := current["arguments"]
		var parts []string
		if !hasEncrypted && hasArguments && json.Unmarshal(encrypted, &parts) == nil && len(parts) > 0 {
			var oldArguments, newArguments string
			if json.Unmarshal(previous["arguments"], &oldArguments) != nil || json.Unmarshal(arguments, &newArguments) != nil || oldArguments != newArguments {
				return nil, false, errors.New("upstream terminal tool arguments conflict with completed encrypted arguments")
			}
		}
	}
	changed := false
	for field, value := range previous {
		if _, exists := current[field]; !exists {
			current[field] = value
			changed = true
		}
	}
	if !changed {
		return terminal, false, nil
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(current); err != nil {
		return nil, false, errors.New("cannot assemble upstream completed output item")
	}
	merged := bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'})
	if !(len(unbounded) > 0 && unbounded[0]) && len(merged) > maxBody {
		return nil, false, errors.New("upstream collected response exceeds 32 MiB")
	}
	return merged, true, nil
}

// There is no configured byte budget in the lightweight route. Arithmetic
// must still be representable before allocating or assembling a JSON array.
func responseCollectionSize(base, added int) (int, error) {
	if base < 0 || added < 0 || added > math.MaxInt-base {
		return 0, errors.New("upstream collected response exceeds integer capacity")
	}
	return base + added, nil
}
