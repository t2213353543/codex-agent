package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// Content identity is separate from an upstream prompt-cache identity. It is
// persisted through sessionAccount's existing tenant namespace and owner checks;
// only the digest, never the message or image/audio payload, reaches the store.
const gatewayContentSessionPrefix = "compat_cs_"
const maxGatewayContentSessionAnchor = 1 << 20

func isGatewayContentSessionKey(session string) bool {
	if len(session) != len(gatewayContentSessionPrefix)+sha256.Size*2 || !strings.HasPrefix(session, gatewayContentSessionPrefix) {
		return false
	}
	encoded := strings.TrimPrefix(session, gatewayContentSessionPrefix)
	decoded, err := hex.DecodeString(encoded)
	return err == nil && len(decoded) == sha256.Size && encoded == hex.EncodeToString(decoded)
}

// gatewayContentSessionKey follows Sub2API's anchored content session seed:
// model, tools/functions, instructions, system/developer content and first user.
// Chat includes the leading system prefix; Responses includes every system item.
// A real first user anchor is required so a shared application prefix or model
// cannot become a hard binding for otherwise unrelated requests.
// Selected fields have a shared 1 MiB budget before/after canonicalization;
// oversized anchors fall back to the existing bounded scheduling preference.
func gatewayContentSessionKey(body []byte, unbounded ...bool) string {
	if len(body) == 0 || len(body) > maxBody && !gatewayPayloadUnboundedOption(unbounded) || !json.Valid(body) {
		return ""
	}
	config := gatewayValidatedJSONFieldSpans(body, "model", "tools")
	legacy := gatewayValidatedJSONFieldSpans(body, "functions", "instructions")
	conversation := gatewayValidatedJSONFieldSpans(body, "messages", "input")
	field := func(span gatewayJSONFieldSpan) []byte { return body[span.start:span.end] }
	hash := sha256.New()
	_, _ = hash.Write([]byte("codex-agent-content-session-v1\x00"))
	used, valid := 0, true
	bounded := func(raw []byte) bool {
		if len(raw) > maxGatewayContentSessionAnchor-used && !gatewayPayloadUnboundedOption(unbounded) {
			valid = false
			return false
		}
		return true
	}
	appendField := func(label string, raw []byte) {
		if !valid || len(raw) == 0 || !bounded(raw) {
			return
		}
		canonical, err := canonicalGatewayCacheJSON(raw)
		if err != nil || !bounded(canonical) {
			valid = false
			return
		}
		used += len(canonical)
		_, _ = hash.Write([]byte(label))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(canonical)
		_, _ = hash.Write([]byte{0})
	}
	for _, item := range []struct {
		label string
		raw   []byte
	}{
		{"model", field(config[0])},
		{"instructions", field(legacy[1])},
	} {
		if !bounded(item.raw) {
			return ""
		}
		var value string
		if json.Unmarshal(item.raw, &value) == nil && value != "" {
			appendField(item.label, item.raw)
		}
	}
	if !valid {
		return ""
	}
	for _, item := range []struct {
		label string
		raw   []byte
	}{
		{"tools", field(config[1])},
		{"functions", field(legacy[0])},
	} {
		raw := bytes.TrimSpace(item.raw)
		if len(raw) > 2 && raw[0] == '[' && len(bytes.TrimSpace(raw[1:len(raw)-1])) > 0 {
			appendField(item.label, raw)
		}
	}
	if !valid {
		return ""
	}
	items := bytes.TrimSpace(field(conversation[0]))
	chat := len(items) > 0 && items[0] == '['
	if !chat {
		items = bytes.TrimSpace(field(conversation[1]))
		if len(items) > 0 && items[0] == '"' {
			if !bounded(items) || !gatewayContentSessionMeaningful(items) {
				return ""
			}
			appendField("input", items)
			if !valid {
				return ""
			}
			return gatewayContentSessionPrefix + hex.EncodeToString(hash.Sum(nil))
		}
	}
	if len(items) == 0 || items[0] != '[' {
		return ""
	}
	firstUser, anchored, leading := false, false, true
	for position := gatewayJSONSkipSpace(items, 1); items[position] != ']'; {
		end := gatewayJSONValueEnd(items, position)
		item := items[position:end]
		fields := gatewayValidatedJSONFieldSpans(item, "role", "content")
		var role string
		_ = json.Unmarshal(item[fields[0].start:fields[0].end], &role)
		content := item[fields[1].start:fields[1].end]
		switch role {
		case "system", "developer":
			if !chat || leading {
				appendField("system", content)
			}
		case "user":
			leading = false
			if !firstUser {
				firstUser, anchored = true, bounded(content) && gatewayContentSessionMeaningful(content)
				appendField("first_user", content)
			}
		default:
			leading = false
		}
		if !chat && !firstUser {
			fields = gatewayValidatedJSONFieldSpans(item, "type", "text")
			var kind string
			_ = json.Unmarshal(item[fields[0].start:fields[0].end], &kind)
			if kind == "input_text" {
				content = item[fields[1].start:fields[1].end]
				firstUser, anchored = true, bounded(content) && gatewayContentSessionMeaningful(content)
				appendField("first_user", content)
			}
		}
		if !valid {
			return ""
		}
		if chat && firstUser {
			break
		}
		position = gatewayJSONSkipSpace(items, end)
		if items[position] == ',' {
			position = gatewayJSONSkipSpace(items, position+1)
		}
	}
	if !anchored {
		return ""
	}
	return gatewayContentSessionPrefix + hex.EncodeToString(hash.Sum(nil))
}

// Text, image, audio and file anchors all retain their complete canonical JSON.
// Empty text parts do not count just because they carry a nonempty type name.
func gatewayContentSessionMeaningful(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false
	}
	if raw[0] == '"' {
		var value string
		return json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value) != ""
	}
	if raw[0] == '[' {
		for position := gatewayJSONSkipSpace(raw, 1); raw[position] != ']'; {
			end := gatewayJSONValueEnd(raw, position)
			item := raw[position:end]
			text := gatewayValidatedJSONFieldSpans(item, "text", "")[0]
			if text.end > 0 {
				if gatewayContentSessionMeaningful(item[text.start:text.end]) {
					return true
				}
			} else if gatewayContentSessionMeaningful(item) {
				return true
			}
			position = gatewayJSONSkipSpace(raw, end)
			if raw[position] == ',' {
				position = gatewayJSONSkipSpace(raw, position+1)
			}
		}
		return false
	}
	return raw[0] != '{' || len(bytes.TrimSpace(raw[1:len(raw)-1])) > 0
}
