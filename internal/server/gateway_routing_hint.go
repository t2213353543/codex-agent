package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

const gatewayRoutingHintHeader = "X-Codex-Routing-Hint"

var errGatewayRoutingHint = errors.New("x-codex-routing-hint must match the prepared response model and service_tier")

// This compatibility addition is limited to the first-party subscription
// destination. A matching path on a custom provider is not evidence of its
// authentication mode or support for Codex routing headers.
func gatewayIsChatGPTCodexBackend(base string) bool {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Hostname(), "chatgpt.com") ||
		(u.Port() != "" && u.Port() != "443") || u.User != nil || u.Opaque != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(base, "#") {
		return false
	}
	return strings.TrimRight(u.EscapedPath(), "/") == "/backend-api/codex"
}

// Routing-hint values use semicolon-separated key=value components. Refuse
// ambiguous delimiters and whitespace rather than introducing a second model
// or tier when a caller supplies an unusual model identifier.
func gatewayRoutingHintValue(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for i := range len(value) {
		if value[i] <= ' ' || value[i] >= 127 || value[i] == ';' || value[i] == '=' {
			return false
		}
	}
	return true
}

func gatewayPreparedRoutingHint(prepared []byte) (model, tier string, err error) {
	// prepareBody and CLI request fields use exact JSON names. Struct decoding
	// would treat unknown Model/Service_Tier extensions as aliases, potentially
	// disagreeing with the sent body or suppressing its hint on a type error.
	// Validate the full document, then borrow just the two control values.
	// Copying unknown input/tool/encrypted fields into a map needlessly retains
	// another complete request while constructing this small diagnostic header.
	if !json.Valid(prepared) {
		return "", "", errGatewayRoutingHint
	}
	fields := gatewayValidatedJSONFieldSpans(prepared, "model", "service_tier")
	modelRaw := prepared[fields[0].start:fields[0].end]
	if json.Unmarshal(modelRaw, &model) != nil || !gatewayRoutingHintValue(model, 512) {
		return "", "", errGatewayRoutingHint
	}
	if span := fields[1]; span.end != 0 {
		var value *string
		if json.Unmarshal(prepared[span.start:span.end], &value) != nil {
			return "", "", errGatewayRoutingHint
		}
		if value == nil {
			return model, "", nil
		}
		if !gatewayRoutingHintValue(*value, 128) {
			return "", "", errGatewayRoutingHint
		}
		tier = *value
	}
	return model, tier, nil
}

func gatewayValidateRoutingHint(value, model, tier string) error {
	foundModel, foundTier := false, false
	for _, part := range strings.Split(value, ";") {
		key, field, ok := strings.Cut(strings.TrimSpace(part), "=")
		switch key {
		case "model":
			if !ok || foundModel || field != model {
				return errGatewayRoutingHint
			}
			foundModel = true
		case "tier":
			if !ok || foundTier || tier == "" || field != tier {
				return errGatewayRoutingHint
			}
			foundTier = true
		}
	}
	if !foundModel || foundTier != (tier != "") {
		return errGatewayRoutingHint
	}
	return nil
}

// Match CLI 0.155 build_routing_hint_header from the actual prepared request.
// Never change body controls or an explicit matching hint's future extensions.
// Nil prepared represents CLI-style WebSocket preconnect, before any model is
// known. Its handshake remains untouched. The finite result is diagnostic only:
// a conflicting explicit hint is not evidence that a caller's request is invalid.
func applyGatewayRoutingHint(headers http.Header, base, endpoint string, account Account, prepared []byte) string {
	if endpoint != "/responses" || !gatewayIsChatGPTCodexBackend(base) ||
		account.AccessToken == "" || strings.TrimSpace(account.AccountID) == "" {
		return "not_applicable"
	}
	if len(prepared) == 0 {
		return "deferred"
	}
	model, tier, err := gatewayPreparedRoutingHint(prepared)
	if err != nil {
		return "unrepresentable_controls"
	}
	if headers == nil {
		return "unwritable_headers"
	}
	// Callers normally supply the already-normalized copyCodexRequestHeaders
	// result. Handle direct/internal noncanonical maps without adding duplicates.
	var values []string
	for key, entries := range headers {
		if strings.EqualFold(key, gatewayRoutingHintHeader) {
			values = append(values, entries...)
		}
	}
	if len(values) > 0 {
		if len(values) != 1 || gatewayValidateRoutingHint(values[0], model, tier) != nil {
			return "preserved_unmatched"
		}
		return "preserved_match"
	}
	value := "model=" + model
	if tier != "" {
		value += ";tier=" + tier
	}
	headers.Set(gatewayRoutingHintHeader, value)
	return "generated"
}
