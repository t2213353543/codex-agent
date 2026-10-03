package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
)

const maxGatewayCacheAffinityAnchor = 1 << 20

type gatewayCacheAffinityContextKey struct{}
type gatewayCacheAffinity struct{ seed [sha256.Size]byte }

// Content affinity is a scheduling preference, never a conversation identity.
// It creates no Redis/session binding and writes no request field. Explicit
// sessions retain their existing strict owner binding and take precedence.
func withGatewayCacheAffinity(ctx context.Context, keyID, session string, prepared []byte, settings Settings) context.Context {
	if session != "" || keyID == "" || !settings.StickySessionsEnabled || !settings.PromptCacheKeyEnabled {
		return ctx
	}
	seed, ok := gatewayCacheAffinitySeed(keyID, prepared, gatewayUnboundedPayload(ctx))
	if !ok {
		return ctx
	}
	return context.WithValue(ctx, gatewayCacheAffinityContextKey{}, gatewayCacheAffinity{seed})
}

// Hash only bounded reusable fields and the first textual user anchor. Appended
// assistant/tool results are scanned without allocating copies and do not split
// a growing full-history request. No content is retained in the context.
func gatewayCacheAffinitySeed(keyID string, prepared []byte, unbounded ...bool) ([sha256.Size]byte, bool) {
	var empty [sha256.Size]byte
	if len(prepared) == 0 || len(prepared) > maxBody && !gatewayPayloadUnboundedOption(unbounded) || !json.Valid(prepared) {
		return empty, false
	}
	config := gatewayValidatedJSONFieldSpans(prepared, "model", "instructions")
	content := gatewayValidatedJSONFieldSpans(prepared, "tools", "input")
	field := func(span gatewayJSONFieldSpan) []byte { return prepared[span.start:span.end] }
	var model string
	if json.Unmarshal(field(config[0]), &model) != nil || strings.TrimSpace(model) == "" || len(model) > 512 && !gatewayPayloadUnboundedOption(unbounded) {
		return empty, false
	}
	hash := sha256.New()
	used := 0
	appendField := func(label string, raw []byte) bool {
		if len(raw) == 0 || len(raw) > maxGatewayCacheAffinityAnchor-used && !gatewayPayloadUnboundedOption(unbounded) {
			return false
		}
		canonical, err := canonicalGatewayCacheJSON(raw)
		if err != nil || len(canonical) > maxGatewayCacheAffinityAnchor-used && !gatewayPayloadUnboundedOption(unbounded) {
			return false
		}
		used += len(canonical)
		_, _ = hash.Write([]byte(label))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(canonical)
		_, _ = hash.Write([]byte{0})
		return true
	}
	namespace, _ := json.Marshal([]string{"codex-agent-content-affinity-v1", keyID})
	_, _ = hash.Write(namespace)
	if !appendField("model", field(config[0])) {
		return empty, false
	}
	meaningful := false
	if raw := field(config[1]); len(raw) > 0 {
		var instructions string
		if (len(raw) <= maxGatewayCacheAffinityAnchor || gatewayPayloadUnboundedOption(unbounded)) && json.Unmarshal(raw, &instructions) == nil && strings.TrimSpace(instructions) != "" {
			meaningful = appendField("instructions", raw) || meaningful
		}
	}
	if raw := bytes.TrimSpace(field(content[0])); len(raw) > 2 && raw[0] == '[' {
		// An empty tools array has no reusable prompt prefix.
		if len(bytes.TrimSpace(raw[1:len(raw)-1])) > 0 {
			meaningful = appendField("tools", raw) || meaningful
		}
	}
	input := bytes.TrimSpace(field(content[1]))
	if len(input) > 0 && input[0] == '"' && gatewayCacheAffinityText(input, unbounded...) {
		meaningful = appendField("first_user", input) || meaningful
	} else if len(input) > 0 && input[0] == '[' {
		leading := true
		for position := gatewayJSONSkipSpace(input, 1); input[position] != ']'; {
			end := gatewayJSONValueEnd(input, position)
			item := input[position:end]
			if len(item) > 0 && item[0] == '{' {
				kind := gatewayValidatedJSONFieldSpans(item, "type", "")
				var itemType string
				_ = json.Unmarshal(item[kind[0].start:kind[0].end], &itemType)
				fields := gatewayValidatedJSONFieldSpans(item, "role", "content")
				var role string
				_ = json.Unmarshal(item[fields[0].start:fields[0].end], &role)
				if itemType != "" && itemType != "message" {
					role = ""
				}
				message := item[fields[1].start:fields[1].end]
				if role == "user" {
					if gatewayCacheAffinityText(message, unbounded...) {
						meaningful = appendField("first_user", message) || meaningful
					}
					break
				}
				if leading && (role == "system" || role == "developer") {
					if gatewayCacheAffinityText(message, unbounded...) {
						meaningful = appendField(role, message) || meaningful
					}
				} else {
					leading = false
				}
				if role == "" {
					fields := gatewayValidatedJSONFieldSpans(item, "type", "text")
					if itemType == "input_text" {
						text := item[fields[1].start:fields[1].end]
						if gatewayCacheAffinityText(text, unbounded...) {
							meaningful = appendField("first_user", text) || meaningful
						}
						break
					}
				}
			} else {
				leading = false
			}
			position = gatewayJSONSkipSpace(input, end)
			if input[position] == ',' {
				position = gatewayJSONSkipSpace(input, position+1)
			}
		}
	}
	if !meaningful {
		return empty, false
	}
	copy(empty[:], hash.Sum(nil))
	return empty, true
}

// Images, audio and opaque reasoning never become a text conversation anchor.
// A usable instructions/tools prefix may still provide a soft preference.
func gatewayCacheAffinityText(raw []byte, unbounded ...bool) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > maxGatewayCacheAffinityAnchor && !gatewayPayloadUnboundedOption(unbounded) {
		return false
	}
	if raw[0] == '"' {
		var value string
		return json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value) != ""
	}
	if raw[0] != '[' {
		return false
	}
	var pieces []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &pieces) != nil || len(pieces) == 0 {
		return false
	}
	meaningful := false
	for _, piece := range pieces {
		if piece.Type != "input_text" {
			return false
		}
		meaningful = meaningful || strings.TrimSpace(piece.Text) != ""
	}
	return meaningful
}

// Weighted rendezvous gives each independent content anchor its normal weight
// distribution while selecting the same order on subsequent turns. The caller
// still enforces priority, current authorization, cooldown and admission.
func gatewayCacheAffinityPick(ctx context.Context, accounts []Account) Account {
	affinity, ok := ctx.Value(gatewayCacheAffinityContextKey{}).(gatewayCacheAffinity)
	if !ok || len(accounts) == 0 {
		return weightedPick(accounts)
	}
	selected, best, bestRef := accounts[0], math.Inf(1), ""
	for _, account := range accounts {
		ref := gatewayAccountRef(account)
		hash := sha256.New()
		_, _ = hash.Write(affinity.seed[:])
		_, _ = hash.Write([]byte(ref))
		digest := hash.Sum(nil)
		// 52 random bits + 1 keep U strictly inside (0,1), including max hash.
		u := float64((binary.BigEndian.Uint64(digest[:8])>>12)+1) / float64((uint64(1)<<52)+1)
		score := -math.Log(u) / float64(account.effectiveWeight())
		if score < best || (score == best && ref < bestRef) {
			selected, best, bestRef = account, score, ref
		}
	}
	return selected
}

func gatewayCacheAffinityProxySeed(ctx context.Context) string {
	if affinity, ok := ctx.Value(gatewayCacheAffinityContextKey{}).(gatewayCacheAffinity); ok {
		return "content-affinity:" + hex.EncodeToString(affinity.seed[:])
	}
	return ""
}
