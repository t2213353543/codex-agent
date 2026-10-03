package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"codex-agent/internal/bps"
)

const (
	bpsPromptCacheScopeClient = "client"
	bpsPromptCacheScopeAPIKey = "api_key"
)

// By default, supply a stable upstream hint only when the client omits it.
// The opt-in api_key scope also consolidates explicit hints across sessions of
// the same API key, confirmed account owner and model. Neither mode changes
// scheduling, conversation ownership or stored client history.
func applyBPSDefaultPromptCache(prepared *bps.Request, keyID string, account Account, settings Settings) error {
	if !settings.PromptCacheKeyEnabled || prepared == nil || prepared.Compact || keyID == "" {
		return nil
	}
	if !json.Valid(prepared.Body) {
		return errors.New("invalid prepared request for cache identity")
	}
	existing := gatewayPromptCacheKeyValue(prepared.Body)
	if settings.BPSPromptCacheScope != bpsPromptCacheScopeAPIKey && existing != "" {
		// Explicit client identities have already been account-scoped by the
		// existing request preparation path. Do not replace or scope them again.
		return nil
	}
	key := bpsDefaultPromptCacheKey(prepared, keyID, account, settings)
	if key == "" || existing == key {
		return nil
	}
	encoded, _ := json.Marshal(key)
	span := gatewayValidatedJSONFieldSpans(prepared.Body, "prompt_cache_key", "")[0]
	if span.end != 0 {
		edits := []gatewayJSONEdit{{gatewayJSONFieldSpan: span, replacement: encoded}}
		prepared.Body = gatewayApplyJSONEdits(prepared.Body, edits, gatewayJSONEditedLength(prepared.Body, edits))
		return nil
	}
	end := len(bytes.TrimRight(prepared.Body, " \t\r\n")) - 1
	if end < 1 || prepared.Body[end] != '}' {
		return errors.New("invalid prepared object for cache identity")
	}
	// Insert one field without reserializing any model-visible input/tool data.
	field := append([]byte(`,"prompt_cache_key":`), encoded...)
	body := make([]byte, 0, len(prepared.Body)+len(field))
	body = append(body, prepared.Body[:end]...)
	body = append(body, field...)
	body = append(body, prepared.Body[end:]...)
	prepared.Body = body
	return nil
}

func bpsDefaultPromptCacheKey(prepared *bps.Request, keyID string, account Account, settings Settings) string {
	if !settings.PromptCacheKeyEnabled || prepared == nil || prepared.Compact || strings.TrimSpace(keyID) == "" || strings.TrimSpace(prepared.Model) == "" {
		return ""
	}
	// Credential-only fallbacks keep their existing client-mode behavior, but
	// cannot prove the owner needed to merge explicit cross-session hints.
	if settings.BPSPromptCacheScope == bpsPromptCacheScopeAPIKey && !gatewayCompleteOwner(account) {
		return ""
	}
	namespace := gatewayCacheOwnerNamespace(account)
	if namespace == "" {
		return ""
	}
	return gatewayScopeCacheIdentityInNamespace(namespace, keyID, "bps-default-prefix-v1", prepared.Model)
}

func bpsSharedPromptCacheApplied(prepared *bps.Request, keyID string, account Account, settings Settings) bool {
	if settings.BPSPromptCacheScope != bpsPromptCacheScopeAPIKey {
		return false
	}
	key := bpsDefaultPromptCacheKey(prepared, keyID, account, settings)
	return key != "" && gatewayPromptCacheKeyValue(prepared.Body) == key
}
