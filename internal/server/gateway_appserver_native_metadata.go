package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// appServerMirrorNativeMetadata is an explicit App Server identity projection
// for already-built native Lite and delta requests. It changes only an existing
// canonical turn-metadata object and its header projection: no input, tools,
// defaults, instruction prefixes or conversation state are synthesized.
func appServerMirrorNativeMetadata(raw []byte, headers http.Header) (body []byte, header http.Header, changed bool, err error) {
	body, header = raw, headers.Clone()
	var fields map[string]json.RawMessage
	if len(bytes.TrimSpace(raw)) > 0 {
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			return nil, nil, false, errors.New("invalid native Responses JSON")
		}
	}
	var client map[string]json.RawMessage
	if value := fields["client_metadata"]; len(value) > 0 && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		if json.Unmarshal(value, &client) != nil || client == nil {
			return nil, nil, false, errors.New("invalid native client metadata")
		}
	}
	var canonicalText string
	bodyCanonical, hasBodyCanonical := client["x-codex-turn-metadata"]
	if hasBodyCanonical && json.Unmarshal(bodyCanonical, &canonicalText) != nil {
		return nil, nil, false, errors.New("invalid native canonical metadata string")
	}
	headerText := ""
	for key, values := range headers {
		if strings.EqualFold(key, "x-codex-turn-metadata") && len(values) > 0 {
			headerText = values[0]
			break
		}
	}
	if !hasBodyCanonical && headerText == "" {
		return body, header, false, nil
	}
	if !hasBodyCanonical {
		canonicalText = headerText
	}
	var canonical map[string]json.RawMessage
	if json.Unmarshal([]byte(canonicalText), &canonical) != nil || canonical == nil {
		return nil, nil, false, errors.New("invalid native canonical turn metadata")
	}
	_, removed := canonical["thread_source"]
	if removed {
		delete(canonical, "thread_source")
		encoded, encodeErr := marshalGatewayRequestJSON(canonical)
		if encodeErr != nil {
			return nil, nil, false, encodeErr
		}
		canonicalText = string(encoded)
		if hasBodyCanonical {
			client["x-codex-turn-metadata"], _ = marshalGatewayRequestJSON(canonicalText)
			fields["client_metadata"], _ = marshalGatewayRequestJSON(client)
			body, err = marshalGatewayRequestJSON(fields)
			if err != nil {
				return nil, nil, false, err
			}
		}
		changed = true
	}
	// Canonical body wins over stale/conflicting compatibility metadata. The
	// projection excludes tool inventory only in the header, never in the body.
	projected, err := appServerTurnMetadataHeader(json.RawMessage(canonicalText))
	if err != nil {
		return nil, nil, false, err
	}
	if projected != headerText {
		if header == nil {
			header = make(http.Header)
		}
		for key := range header {
			if strings.EqualFold(key, "x-codex-turn-metadata") {
				delete(header, key)
			}
		}
		header.Set("x-codex-turn-metadata", projected)
		changed = true
	}
	return body, header, changed, nil
}
