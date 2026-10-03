package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf16"
)

// appServerRequestMetadata contains caller-owned identities and the canonical
// CodexTurnMetadataPayload. It projects that one snapshot onto HTTP headers and
// client_metadata without inventing an installation, window, turn or sandbox.
type appServerRequestMetadata struct {
	InstallationID, SessionID, ThreadID, WindowID              string
	TurnID, ParentThreadID, ParentTurnID, RootTurnID, Subagent string
	TurnMetadata                                               json.RawMessage
}

func projectAppServerRequestMetadata(in appServerRequestMetadata) (map[string]string, http.Header, error) {
	if in.InstallationID == "" || in.SessionID == "" || in.ThreadID == "" || in.WindowID == "" {
		return nil, nil, errors.New("App Server metadata requires explicit installation/session/thread/window identities")
	}
	metadata := map[string]string{"x-codex-installation-id": in.InstallationID, "session_id": in.SessionID, "thread_id": in.ThreadID, "x-codex-window-id": in.WindowID}
	headers := make(http.Header)
	headers.Set("x-codex-window-id", in.WindowID)
	for key, value := range map[string]string{"turn_id": in.TurnID, "x-codex-parent-thread-id": in.ParentThreadID, "parent_turn_id": in.ParentTurnID, "root_turn_id": in.RootTurnID, "x-openai-subagent": in.Subagent} {
		if value != "" {
			metadata[key] = value
		}
	}
	if in.ParentThreadID != "" {
		headers.Set("x-codex-parent-thread-id", in.ParentThreadID)
	}
	if in.Subagent != "" {
		headers.Set("x-openai-subagent", in.Subagent)
	}
	if len(in.TurnMetadata) > 0 {
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(in.TurnMetadata, &payload); err != nil || payload == nil {
			return nil, nil, errors.New("invalid App Server turn metadata")
		}
		// Preserve the canonical payload bytes in the model body. The header is
		// a bounded projection that excludes the tool inventory (0.155.0
		// core/src/responses_metadata.rs compatibility_headers).
		metadata["x-codex-turn-metadata"] = string(in.TurnMetadata)
		value, err := appServerTurnMetadataHeader(in.TurnMetadata)
		if err != nil {
			return nil, nil, err
		}
		headers.Set("x-codex-turn-metadata", value)
	}
	for name, values := range headers {
		for _, value := range values {
			if !gatewayNativeHeaderValue(value) {
				return nil, nil, fmt.Errorf("invalid App Server metadata header %s", name)
			}
		}
	}
	return metadata, headers, nil
}

// Compatibility projection keeps all known and future metadata except the
// unbounded tool inventory. The full canonical object remains in the body.
func appServerTurnMetadataHeader(raw json.RawMessage) (string, error) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(raw, &payload) != nil || payload == nil {
		return "", errors.New("invalid App Server turn metadata")
	}
	delete(payload, "tool_namespaces_info")
	encoded, err := marshalGatewayRequestJSON(payload)
	if err != nil {
		return "", err
	}
	var ascii strings.Builder
	for _, r := range string(encoded) {
		if r <= 0x7f {
			ascii.WriteRune(r)
		} else if r <= 0xffff {
			fmt.Fprintf(&ascii, "\\u%04x", r)
		} else {
			high, low := utf16.EncodeRune(r)
			fmt.Fprintf(&ascii, "\\u%04x\\u%04x", high, low)
		}
	}
	return ascii.String(), nil
}
