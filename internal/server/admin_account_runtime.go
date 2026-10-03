package server

// Management-only observations. Reading this file's endpoints never acquires
// admission, expires/clears a cooldown, refreshes credentials or calls upstream.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type managedRuntimeCooldown struct {
	Kind     string `json:"kind"`
	Scope    string `json:"scope"`
	Source   string `json:"source"`
	Until    int64  `json:"until"`
	HTTPCode int    `json:"http_code,omitempty"`
}

type managedRuntimeCooldowns struct {
	Coverage    string                   `json:"coverage"`
	Model       string                   `json:"model,omitempty"`
	SharedState string                   `json:"shared_state"`
	Entries     []managedRuntimeCooldown `json:"entries"`
}

func (p *Proxy) managedRuntimeCooldowns(ctx context.Context, account Account, model string, now time.Time) managedRuntimeCooldowns {
	out := managedRuntimeCooldowns{Coverage: "connection_models_and_requested_model", Model: model, SharedState: "not_configured", Entries: []managedRuntimeCooldown{}}
	scopes := []string{gatewayCooldownConnection, "models"}
	if model != "" {
		scopes = append(scopes, "model:"+model)
	}
	cooldownAccount := account
	if p.Store.DB == nil {
		cooldownAccount.CredentialVersion = 0
	}
	keys := make([]string, len(scopes))
	for i, scope := range scopes {
		keys[i] = gatewayCooldownKey(cooldownAccount, scope)
	}
	appendUntil := func(kind, scope, source string, until time.Time, code int) {
		if until.After(now) {
			out.Entries = append(out.Entries, managedRuntimeCooldown{Kind: kind, Scope: scope, Source: source, Until: until.UnixMilli(), HTTPCode: code})
		}
	}
	// Do not use localGatewayCooldown/get: their reads prune entries or touch LRU.
	p.cooldownMu.Lock()
	for i, key := range keys {
		appendUntil("rate_limit", scopes[i], "process", p.cooldowns[key], 0)
	}
	appendUntil("capacity_guard", "all", "process", p.cooldownOverflowUntil, 0)
	p.cooldownMu.Unlock()
	refreshKey := gatewayRefreshFailureKey(account, p.TokenURL)
	p.refreshCooldown.mu.Lock()
	refresh, exists := p.refreshCooldown.entries[refreshKey]
	p.refreshCooldown.mu.Unlock()
	if exists && refresh.valid(now) {
		appendUntil("oauth_refresh_"+refresh.Kind, "token_refresh", "process", time.UnixMilli(refresh.Until), refresh.StatusCode)
	}
	if db := p.Store.DB; db != nil && db.Redis != nil {
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		pipe := db.Redis.Pipeline()
		clock := pipe.Time(readCtx)
		scores := make([]*redis.FloatCmd, len(keys))
		for i, key := range keys {
			scores[i] = pipe.ZScore(readCtx, db.Namespace+"gateway:cooldowns", key)
		}
		capacity := pipe.ZScore(readCtx, db.Namespace+"gateway:cooldowns", "!capacity")
		failure := pipe.Get(readCtx, db.Namespace+"gateway:oauth-failure:"+refreshKey)
		_, err := pipe.Exec(readCtx)
		out.SharedState = "ok"
		if err != nil && !errors.Is(err, redis.Nil) {
			out.SharedState = "unavailable"
		}
		serverNow, clockErr := clock.Result()
		if clockErr != nil {
			out.SharedState = "unavailable"
		} else {
			addScore := func(cmd *redis.FloatCmd, kind, scope string) {
				score, readErr := cmd.Result()
				if readErr != nil {
					if !errors.Is(readErr, redis.Nil) {
						out.SharedState = "unavailable"
					}
					return
				}
				remaining := time.UnixMilli(int64(score)).Sub(serverNow)
				if remaining > 0 {
					appendUntil(kind, scope, "shared_redis", now.Add(remaining), 0)
				}
			}
			for i, cmd := range scores {
				addScore(cmd, "rate_limit", scopes[i])
			}
			addScore(capacity, "capacity_guard", "all")
		}
		if raw, readErr := failure.Bytes(); readErr == nil {
			var shared gatewayRefreshFailure
			if json.Unmarshal(raw, &shared) != nil {
				out.SharedState = "unavailable"
			} else if shared.valid(now) {
				appendUntil("oauth_refresh_"+shared.Kind, "token_refresh", "shared_redis", time.UnixMilli(shared.Until), shared.StatusCode)
			}
		} else if !errors.Is(readErr, redis.Nil) {
			out.SharedState = "unavailable"
		}
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		if out.Entries[i].Until != out.Entries[j].Until {
			return out.Entries[i].Until > out.Entries[j].Until
		}
		return out.Entries[i].Scope+out.Entries[i].Source < out.Entries[j].Scope+out.Entries[j].Source
	})
	return out
}

type managedAccountLastUsed struct {
	ID string `json:"id"`
	At *int64 `json:"at"`
}

func (p *Proxy) managedAccountLastUsed(ctx context.Context, ids []string) ([]managedAccountLastUsed, string, error) {
	items := make([]managedAccountLastUsed, len(ids))
	positions := make(map[string]int, len(ids))
	for i, id := range ids {
		items[i].ID = id
		positions[id] = i
	}
	scope := "retained_request_details"
	if p.Metrics == nil {
		return nil, scope, errors.New("request metrics unavailable")
	}
	set := func(id string, at int64) {
		if i, found := positions[id]; found && at > 0 && (items[i].At == nil || at > *items[i].At) {
			value := at
			items[i].At = &value
		}
	}
	if db := p.Metrics.db; db != nil {
		scope = "durable_usage_events"
		// The writer advances this durable snapshot in the same transaction as
		// the event. Unlike usage_totals.last_at it never invents a legacy bucket
		// timestamp, and it avoids scanning every event for these stable IDs.
		rows, err := db.Pool.Query(ctx, "SELECT account_ref,extract(epoch FROM last_at)::bigint FROM account_request_state WHERE account_ref=ANY($1::text[])", ids)
		if err != nil {
			return nil, scope, err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var at int64
			if err = rows.Scan(&id, &at); err != nil {
				return nil, scope, err
			}
			set(id, at)
		}
		return items, scope, rows.Err()
	}
	_, err := p.Metrics.walkManagedFile(ctx, 0, func(record RequestRecord) error { set(record.AccountID, record.Time); return nil })
	return items, scope, err
}

func (p *Proxy) handleManagedAccountLastUsed(w http.ResponseWriter, r *http.Request) {
	var input struct {
		IDs []string `json:"ids"`
	}
	if err := readJSONLimit(w, r, &input, 32<<10); err != nil {
		adminError(w, err)
		return
	}
	if len(input.IDs) < 1 || len(input.IDs) > 100 {
		adminError(w, fmt.Errorf("%w: ids must contain 1 to 100 unique account IDs", errInvalid))
		return
	}
	seen := map[string]bool{}
	for _, id := range input.IDs {
		if strings.TrimSpace(id) == "" || len(id) > 200 || seen[id] {
			adminError(w, fmt.Errorf("%w: account IDs must be unique and non-empty", errInvalid))
			return
		}
		seen[id] = true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	items, scope, err := p.managedAccountLastUsed(ctx, input.IDs)
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"items": items, "scope": scope, "source": "gateway_request_records", "checked_at": time.Now().Unix()})
}

func (p *Proxy) handleManagedAccountRuntime(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for key, values := range q {
		if key != "model" || len(values) != 1 {
			adminError(w, fmt.Errorf("%w: only one model query is supported", errInvalid))
			return
		}
	}
	model := strings.TrimSpace(q.Get("model"))
	if len(model) > 200 || strings.ContainsAny(model, "\r\n\x00") {
		adminError(w, fmt.Errorf("%w: invalid model", errInvalid))
		return
	}
	a, err := p.Store.Get(r.PathValue("name"))
	if err != nil {
		p.storeError(w, err)
		return
	}
	if a.ID != r.PathValue("name") {
		p.storeError(w, errNotFound)
		return
	}
	settings, err := p.managementSettings()
	if err != nil {
		p.storeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	last, scope, err := p.managedAccountLastUsed(ctx, []string{a.ID})
	if err != nil {
		p.storeError(w, err)
		return
	}
	now := time.Now()
	fingerprint := map[string]any{"enabled": settings.CodexFingerprintEnabled, "configured_profile": settings.CodexFingerprintProfile, "profile": nil, "scope": "application_headers_only"}
	if settings.CodexFingerprintEnabled {
		if profile, err := effectiveCodexFingerprintProfile(settings.CodexFingerprintProfile); err == nil {
			fingerprint["profile"] = profile
		} else {
			fingerprint["error"] = "runtime_profile_mismatch"
		}
	}
	flight := p.inFlight(gatewayAdmissionKey(a))
	writeJSON(w, map[string]any{
		"id": a.ID, "version": a.Version, "checked_at": now.UnixMilli(),
		"admission": map[string]any{"scope": "process", "in_flight": flight, "limit": a.effectiveConcurrency(), "saturated": flight >= int64(a.effectiveConcurrency())},
		"cooldowns": p.managedRuntimeCooldowns(ctx, a, model, now),
		"effective_policy": map[string]any{
			"inference": p.inferenceInfo(),
			"source":    "global_settings", "settings_version": settings.Version, "per_account_override": false, "fingerprint": fingerprint,
			"websocket_enabled": settings.WebsocketEnabled, "http_stream_enabled": settings.HTTPStreamEnabled, "http_json_enabled": settings.HTTPJSONEnabled,
			"compact_enabled": settings.CompactEnabled, "models_enabled": settings.ModelsEnabled, "compatibility_normalization_enabled": settings.CompatibilityNormalizationEnabled,
			"automatic_token_refresh_enabled": settings.AutomaticTokenRefreshEnabled, "upstream_401_recovery_enabled": settings.Upstream401RecoveryEnabled,
			"sticky_sessions_enabled": settings.StickySessionsEnabled, "prompt_cache_key_enabled": settings.PromptCacheKeyEnabled,
			"bps_prompt_cache_scope":         settings.BPSPromptCacheScope,
			"cache_creation_as_read_enabled": settings.CacheCreationAsReadEnabled,
			"cache_read_ratio_enabled":       settings.CacheReadRatioEnabled,
			"cache_read_ratio_percent":       settings.CacheReadRatioPercent,
			"bps_model_routes":               settings.BPSModelRoutes,
			"bps_model_route_targets":        bpsStaticModelIDs(),
			"rate_limit_cooldown_enabled":    settings.RateLimitCooldownEnabled,
			"builtin_prompt_enabled":         settings.BuiltinPromptEnabled, "builtin_prompt_mode": settings.BuiltinPromptMode,
			"account": map[string]any{"enabled": a.Enabled, "concurrency": a.effectiveConcurrency(), "has_refresh_token": a.HasRefreshToken, "expires_at": a.ExpiresAt},
		},
		"last_used": map[string]any{"at": last[0].At, "scope": scope, "source": "gateway_request_records"},
	})
}
