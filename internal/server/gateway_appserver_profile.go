package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// appServerProfileDefaults holds locally allocated identities, not invented
// client state. Allocate them once outside the pure resolver (UUIDs in the
// official runtime); reuse the snapshot on retries. Existing client values win.
type appServerProfileDefaults struct {
	InstallationID, SessionID, ThreadID, TurnID, ContextWindowID string
	WindowNumber                                                 *uint64
	TurnStartedAtUnixMS                                          *int64
	CreateTime                                                   json.Number
}

type appServerProfileInput struct {
	Request     appServerRequestInput
	RequestKind string
	Defaults    appServerProfileDefaults
	// CompleteHistory is an assertion by the caller, never guessed from the
	// presence of an input array. Only then can compaction count define a window.
	CompleteHistory bool
	// NewInputStart marks new user input. Do not annotate retained history or
	// encrypted/server-produced items. NewItemIDs is keyed by original index.
	NewInputStart   *int
	NewItemIDs      map[int]string
	MirrorAppServer bool
}

type appServerProfileResult struct {
	Request     appServerRequestInput
	Sources     map[string]string
	Missing     []string
	Environment *appServerProfileEnvironment
}

// appServerPrewarmProfile projects a generation snapshot onto the official
// startup-prewarm phase. INITIAL_SUBMIT_ID is the empty string, not an omitted
// turn_id. The startup tool snapshot is already built before metadata is read,
// so tool_namespaces_info remains in the body when present (and is omitted only
// from the compatibility header). This never edits the generation snapshot.
func appServerPrewarmProfile(in appServerRequestInput) (appServerRequestInput, error) {
	out := in
	out.ClientMetadata = make(map[string]string, len(in.ClientMetadata)+1)
	for key, value := range in.ClientMetadata {
		out.ClientMetadata[key] = value
	}
	out.ExtraHeaders = make(http.Header)
	for key, values := range in.ExtraHeaders {
		for _, value := range values {
			out.ExtraHeaders.Add(key, value)
		}
	}
	canonical := map[string]json.RawMessage{}
	raw := in.ClientMetadata["x-codex-turn-metadata"]
	if raw == "" {
		raw = out.ExtraHeaders.Get("x-codex-turn-metadata")
	}
	if raw != "" && (json.Unmarshal([]byte(raw), &canonical) != nil || canonical == nil) {
		return appServerRequestInput{}, errors.New("invalid App Server prewarm metadata")
	}
	canonical["request_kind"] = json.RawMessage(`"prewarm"`)
	canonical["turn_id"] = json.RawMessage(`""`)
	delete(canonical, "root_turn_id")
	delete(canonical, "turn_started_at_unix_ms")
	out.ClientMetadata["turn_id"] = ""
	delete(out.ClientMetadata, "root_turn_id")
	delete(out.ClientMetadata, "turn_started_at_unix_ms")
	encoded, err := marshalGatewayRequestJSON(canonical)
	if err != nil {
		return appServerRequestInput{}, err
	}
	out.ClientMetadata["x-codex-turn-metadata"] = string(encoded)
	header, err := appServerTurnMetadataHeader(encoded)
	if err != nil {
		return appServerRequestInput{}, err
	}
	out.ExtraHeaders.Set("x-codex-turn-metadata", header)
	return out, nil
}

// resolveAppServerRequestProfile fills metadata that can be determined from
// the incoming request, actual catalog and explicit allocation snapshot. It
// does not infer a sandbox implementation, agent tree or execution capability.
func resolveAppServerRequestProfile(options appServerProfileInput) (appServerProfileResult, error) {
	out := appServerProfileResult{Request: options.Request, Sources: map[string]string{}}
	out.Request.ClientMetadata = map[string]string{}
	for key, value := range options.Request.ClientMetadata {
		out.Request.ClientMetadata[key] = value
	}
	out.Request.ExtraHeaders = make(http.Header)
	for key, values := range options.Request.ExtraHeaders {
		for _, value := range values {
			out.Request.ExtraHeaders.Add(key, value)
		}
	}
	canonical := map[string]json.RawMessage{}
	if raw := out.Request.ClientMetadata["x-codex-turn-metadata"]; raw != "" {
		if json.Unmarshal([]byte(raw), &canonical) != nil || canonical == nil {
			return out, errors.New("invalid client turn metadata")
		}
		for key := range canonical {
			out.Sources[key] = "client_metadata.turn_metadata"
		}
	}
	headerCanonical := map[string]json.RawMessage{}
	if raw := out.Request.ExtraHeaders.Get("x-codex-turn-metadata"); raw != "" {
		if json.Unmarshal([]byte(raw), &headerCanonical) != nil || headerCanonical == nil {
			return out, errors.New("invalid turn metadata header")
		}
		for key, value := range headerCanonical {
			if _, exists := canonical[key]; !exists {
				canonical[key] = value
				out.Sources[key] = "header.turn_metadata"
			}
		}
	}
	set := func(key string, value any, source string) {
		if _, exists := canonical[key]; !exists {
			raw, _ := marshalGatewayRequestJSON(value)
			canonical[key] = raw
			out.Sources[key] = source
		}
	}
	get := func(key string) string { var value string; _ = json.Unmarshal(canonical[key], &value); return value }
	// The canonical body and client flat projections are one logical snapshot.
	// Reject conflicting supplied identities instead of silently rewriting one.
	identityFields := [][3]string{{"installation_id", "x-codex-installation-id", "x-codex-installation-id"}, {"session_id", "session_id", "session-id"}, {"thread_id", "thread_id", "thread-id"}, {"window_id", "x-codex-window-id", "x-codex-window-id"}, {"turn_id", "turn_id", ""}, {"parent_thread_id", "x-codex-parent-thread-id", "x-codex-parent-thread-id"}, {"parent_turn_id", "parent_turn_id", ""}, {"root_turn_id", "root_turn_id", ""}}
	for _, fields := range identityFields {
		key, flat, header := fields[0], fields[1], fields[2]
		if value, exists := out.Request.ClientMetadata[flat]; exists {
			if existing, has := canonical[key]; has {
				var text string
				if json.Unmarshal(existing, &text) != nil || text != value {
					return out, fmt.Errorf("conflicting client %s metadata", key)
				}
			}
			set(key, value, "client_metadata."+flat)
		} else if get(key) == "" && header != "" {
			if value := out.Request.ExtraHeaders.Get(header); value != "" {
				set(key, value, "header."+header)
			}
		}
		if header != "" {
			if value := out.Request.ExtraHeaders.Get(header); value != "" && get(key) != "" && get(key) != value {
				return out, fmt.Errorf("conflicting client %s header", key)
			}
		}
	}
	for _, field := range []struct{ key, value string }{{"installation_id", options.Defaults.InstallationID}, {"session_id", options.Request.SessionID}, {"thread_id", options.Request.ThreadID}} {
		if field.value != "" {
			set(field.key, field.value, "request_identity")
		}
	}
	for _, field := range []struct{ key, value string }{{"session_id", options.Defaults.SessionID}, {"thread_id", options.Defaults.ThreadID}} {
		if field.value != "" {
			set(field.key, field.value, "allocated_identity")
		}
	}
	if get("thread_id") == "" && get("session_id") != "" {
		set("thread_id", get("session_id"), "session_id")
	}
	if get("session_id") == "" && get("thread_id") != "" {
		set("session_id", get("thread_id"), "thread_id")
	}
	out.Request.SessionID, out.Request.ThreadID = get("session_id"), get("thread_id")
	if options.NewInputStart != nil {
		if *options.NewInputStart < 0 || *options.NewInputStart > len(options.Request.Input) {
			return out, errors.New("invalid new-input boundary")
		}
		for _, raw := range options.Request.Input[*options.NewInputStart:] {
			var item struct {
				Metadata struct {
					TurnID string `json:"turn_id"`
				} `json:"internal_chat_message_metadata_passthrough"`
			}
			if json.Unmarshal(raw, &item) == nil && item.Metadata.TurnID != "" {
				set("turn_id", item.Metadata.TurnID, "new_input.metadata")
				break
			}
		}
	}
	if get("turn_id") == "" && options.Defaults.TurnID != "" {
		set("turn_id", options.Defaults.TurnID, "allocated_request_turn")
	}
	kind := get("request_kind")
	if kind == "" {
		kind = options.RequestKind
	}
	if kind == "" {
		kind = "turn"
		for _, raw := range options.Request.Input {
			var item struct{ Type string }
			if json.Unmarshal(raw, &item) == nil && item.Type == "compaction_trigger" {
				kind = "compaction"
				break
			}
		}
	}
	if _, provided := canonical["request_kind"]; !provided && kind != "turn" && kind != "prewarm" && kind != "compaction" && kind != "memory" {
		return out, errors.New("invalid App Server request kind")
	}
	set("request_kind", kind, "request_operation")
	if _, exists := canonical["window_number"]; !exists {
		if window := get("window_id"); window != "" && strings.HasPrefix(window, get("thread_id")+":") {
			if number, err := strconv.ParseUint(strings.TrimPrefix(window, get("thread_id")+":"), 10, 64); err == nil {
				set("window_number", number, "window_id")
			}
		}
	}
	if _, exists := canonical["window_number"]; !exists && options.Defaults.WindowNumber != nil {
		set("window_number", *options.Defaults.WindowNumber, "explicit_window")
	}
	if _, exists := canonical["window_number"]; !exists && options.CompleteHistory {
		var count uint64
		for _, raw := range options.Request.Input {
			var item struct{ Type string }
			if json.Unmarshal(raw, &item) == nil && item.Type == "compaction" {
				count++
			}
		}
		set("window_number", count, "complete_history.compaction_count")
	}
	if raw, exists := canonical["window_number"]; exists && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && get("window_id") == "" && get("thread_id") != "" {
		var number uint64
		if json.Unmarshal(raw, &number) != nil {
			return out, errors.New("invalid window_number")
		}
		set("window_id", fmt.Sprintf("%s:%d", get("thread_id"), number), "thread_id:window_number")
	}
	if get("context_window_id") == "" && get("window_id") != "" && options.Defaults.ContextWindowID != "" {
		set("context_window_id", options.Defaults.ContextWindowID, "allocated_context_window")
	}
	if get("parent_thread_id") == "" && get("parent_turn_id") == "" && get("turn_id") != "" && kind == "turn" {
		set("root_turn_id", get("turn_id"), "root_request_turn")
	}
	if kind != "prewarm" && options.Defaults.TurnStartedAtUnixMS != nil {
		set("turn_started_at_unix_ms", *options.Defaults.TurnStartedAtUnixMS, "request_start_clock")
	}
	var model map[string]json.RawMessage
	if json.Unmarshal(options.Request.ModelMetadataRaw, &model) != nil || model == nil {
		return out, errors.New("invalid profile model metadata")
	}
	for _, key := range []string{"node_repl_auto_review_required", "node_repl_disabled"} {
		if raw, exists := model[key]; exists {
			var value bool
			if json.Unmarshal(raw, &value) != nil {
				return out, fmt.Errorf("invalid model %s", key)
			}
			set(key, value, "model_catalog")
		} else {
			// ModelInfo marks both fields serde(default), i.e. false in the
			// fixed 0.155.0 source. This is a model default, not host policy.
			set(key, false, "model_catalog.serde_default")
		}
	}
	var err error
	out.Environment, err = appServerEnvironmentFromInput(options.Request.Input)
	if err != nil {
		// Environment text is caller-owned model context, not a mandatory API
		// control. An unparseable quoted/example XML block cannot reject an
		// otherwise valid model request or justify inventing host metadata.
		out.Environment = nil
		out.Missing = append(out.Missing, "client_environment.parseable_xml")
	}
	if out.Environment != nil && out.Environment.SandboxMode != "" {
		set("sandbox_mode", out.Environment.SandboxMode, "client_environment.filesystem")
	}
	if options.MirrorAppServer {
		delete(canonical, "thread_source")
		out.Sources["thread_source"] = "omitted_by_explicit_app_server_mirror"
	}
	// App Server does not make an arbitrary client a root multi-agent runtime.
	// Agent name, sandbox engine and review state require caller evidence.
	for _, key := range []string{"installation_id", "session_id", "thread_id", "turn_id", "window_id", "window_number", "context_window_id", "agent_name", "sandbox", "sandbox_mode", "auto_review_enabled"} {
		if _, exists := canonical[key]; !exists {
			out.Missing = append(out.Missing, key)
		}
	}
	for _, fields := range identityFields {
		if raw, exists := canonical[fields[0]]; exists {
			var value string
			if json.Unmarshal(raw, &value) != nil {
				return out, fmt.Errorf("invalid %s identity", fields[0])
			}
			out.Request.ClientMetadata[fields[1]] = value
		}
	}
	canonicalRaw, err := marshalGatewayRequestJSON(canonical)
	if err != nil {
		return out, err
	}
	out.Request.ClientMetadata["x-codex-turn-metadata"] = string(canonicalRaw)
	header, err := appServerTurnMetadataHeader(canonicalRaw)
	if err != nil {
		return out, err
	}
	out.Request.ExtraHeaders.Set("x-codex-turn-metadata", header)
	for _, pair := range [][2]string{{"window_id", "x-codex-window-id"}, {"parent_thread_id", "x-codex-parent-thread-id"}} {
		if value := get(pair[0]); value != "" {
			out.Request.ExtraHeaders.Set(pair[1], value)
		}
	}
	if value := out.Request.ClientMetadata["x-openai-subagent"]; value != "" {
		out.Request.ExtraHeaders.Set("x-openai-subagent", value)
	} else if value = out.Request.ExtraHeaders.Get("x-openai-subagent"); value != "" {
		out.Request.ClientMetadata["x-openai-subagent"] = value
	}
	out.Request.Input, err = appServerProfileInputItems(options, get("turn_id"))
	if err != nil {
		return out, err
	}
	missingItemID, missingClassifications := false, false
	for _, raw := range out.Request.Input {
		var item struct {
			Role, Type string
			ID         *string
			Metadata   struct {
				Kinds json.RawMessage `json:"content_item_kinds"`
			} `json:"internal_chat_message_metadata_passthrough"`
		}
		if json.Unmarshal(raw, &item) != nil || item.Role != "user" || item.Type != "" && item.Type != "message" {
			continue
		}
		if item.ID == nil {
			missingItemID = true
		}
		if len(item.Metadata.Kinds) == 0 {
			missingClassifications = true
		} else {
			out.Request.ContentItemKinds = true
		}
	}
	if missingItemID {
		out.Missing = append(out.Missing, "input.item_id")
	}
	if missingClassifications {
		out.Missing = append(out.Missing, "input.content_item_kinds")
	}
	sort.Strings(out.Missing)
	return out, nil
}

func appServerProfileInputItems(options appServerProfileInput, turnID string) ([]json.RawMessage, error) {
	items := append([]json.RawMessage(nil), options.Request.Input...)
	if options.NewInputStart == nil {
		return items, nil
	}
	for index := *options.NewInputStart; index < len(items); index++ {
		var item map[string]json.RawMessage
		if json.Unmarshal(items[index], &item) != nil || item == nil {
			return nil, errors.New("invalid profile input item")
		}
		if string(item["role"]) != `"user"` || len(item["type"]) > 0 && string(item["type"]) != `"message"` {
			continue
		}
		var content []struct{ Type, Text string }
		if json.Unmarshal(item["content"], &content) != nil {
			continue
		}
		var metadata map[string]json.RawMessage
		if raw := item["internal_chat_message_metadata_passthrough"]; len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			if json.Unmarshal(raw, &metadata) != nil {
				return nil, errors.New("invalid input metadata")
			}
		}
		if metadata == nil {
			metadata = map[string]json.RawMessage{}
		}
		if len(item["id"]) == 0 && options.NewItemIDs[index] != "" {
			id := options.NewItemIDs[index]
			if !strings.HasPrefix(id, "msg_") || len(id) <= 4 {
				return nil, errors.New("new user item identity must have a msg_ prefix")
			}
			item["id"], _ = marshalGatewayRequestJSON(id)
		}
		if len(metadata["turn_id"]) == 0 && turnID != "" {
			metadata["turn_id"], _ = marshalGatewayRequestJSON(turnID)
		}
		if len(metadata["create_time"]) == 0 && options.Defaults.CreateTime != "" {
			raw, err := marshalGatewayRequestJSON(options.Defaults.CreateTime)
			if err != nil {
				return nil, err
			}
			metadata["create_time"] = raw
		}
		if options.Request.ContentItemKinds && len(metadata["content_item_kinds"]) == 0 {
			kinds := make([]string, 0, len(content))
			known := true
			for _, part := range content {
				switch part.Type {
				case "input_text":
					if strings.HasPrefix(part.Text, "<environment_context>") && strings.HasSuffix(part.Text, "</environment_context>") {
						kinds = append(kinds, "environments.environment_context")
					} else {
						kinds = append(kinds, "user.text")
					}
				case "input_image":
					kinds = append(kinds, "user.image")
				case "input_audio":
					kinds = append(kinds, "user.audio")
				default:
					known = false
				}
			}
			if known {
				metadata["content_item_kinds"], _ = marshalGatewayRequestJSON(kinds)
			}
		}
		if len(metadata) > 0 {
			item["internal_chat_message_metadata_passthrough"], _ = marshalGatewayRequestJSON(metadata)
		}
		encoded, err := marshalGatewayRequestJSON(item)
		if err != nil {
			return nil, err
		}
		items[index] = encoded
	}
	return items, nil
}
