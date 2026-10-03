package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"codex-agent/internal/bps"
)

const managedUpstreamBodyLimit = 4 << 20

type managedUpstreamError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type managedUpstreamSnapshot struct {
	ID                string                               `json:"id"`
	Version           int64                                `json:"version"`
	CredentialVersion int64                                `json:"credential_version"`
	Kind              string                               `json:"kind"`
	Model             string                               `json:"model,omitempty"`
	OK                bool                                 `json:"ok"`
	HTTPCode          int                                  `json:"http_code"`
	CheckedAt         int64                                `json:"checked_at"`
	LatencyMs         int64                                `json:"latency_ms"`
	Source            string                               `json:"source"`
	Stale             bool                                 `json:"stale"`
	Error             *managedUpstreamError                `json:"error,omitempty"`
	Data              any                                  `json:"data"`
	LocalWindowCosts  map[string]*managedAccountWindowCost `json:"local_window_costs,omitempty"`
}

// Management snapshots never affect scheduling, credential validity or
// business usage counters. They live in the independent management store.
var errManagedUpstreamSnapshotSuperseded = errors.New("management snapshot superseded")

func (p *Proxy) cacheManagedUpstream(ctx context.Context, snapshot managedUpstreamSnapshot) error {
	err := p.managementState().Update(ctx, "account-upstream", snapshot.ID, func(entry *managementStateEntry) error {
		rows := map[string]managedUpstreamSnapshot{}
		if len(entry.Payload) > 0 {
			if err := json.Unmarshal(entry.Payload, &rows); err != nil {
				return err
			}
			if rows == nil {
				return errInvalid
			}
		}
		// Compare inside the management-store lock/transaction. Token epochs
		// outrank wall time; metadata Version must not order credential checks.
		// CheckedAt has second precision: ties retain last-completion behavior
		// rather than claiming an unobserved ordering within the same second.
		if current, found := rows[snapshot.Kind]; found &&
			(current.CredentialVersion > snapshot.CredentialVersion ||
				current.CredentialVersion == snapshot.CredentialVersion && current.CheckedAt > snapshot.CheckedAt) {
			return errManagedUpstreamSnapshotSuperseded
		}
		rows[snapshot.Kind] = snapshot
		raw, err := json.Marshal(rows)
		entry.Payload = raw
		return err
	})
	if errors.Is(err, errManagedUpstreamSnapshotSuperseded) {
		// The API still returns this check's own result. Only its stale cache
		// write is discarded, without incrementing the management document.
		return nil
	}
	return err
}

func (p *Proxy) handleManagedUpstreamSnapshot(w http.ResponseWriter, r *http.Request) {
	a, err := p.Store.GetMetadata(r.PathValue("name"))
	if err != nil {
		p.storeError(w, err)
		return
	}
	if a.ID != r.PathValue("name") {
		p.storeError(w, errNotFound)
		return
	}
	version := a.CredentialVersion
	result := map[string]managedUpstreamSnapshot{}
	entry, err := p.managementState().Get(r.Context(), "account-upstream", a.ID)
	if err != nil && !errors.Is(err, errNotFound) {
		p.storeError(w, err)
		return
	}
	if err == nil {
		if err = json.Unmarshal(entry.Payload, &result); err != nil {
			p.storeError(w, err)
			return
		}
	}
	for kind, snapshot := range result {
		snapshot.Stale = snapshot.Stale || snapshot.CredentialVersion != version || time.Now().Unix()-snapshot.CheckedAt > 300
		p.enrichManagedWindowCosts(r.Context(), []*managedUpstreamSnapshot{&snapshot})
		result[kind] = snapshot
	}
	writeJSON(w, map[string]any{"id": a.ID, "snapshots": result, "persistent": true})
}

func validManagedModel(model string) bool {
	return model != "" && len(model) <= 256 && strings.IndexFunc(model, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

// Select the account's first enabled configured proxy without changing the
// gateway's admission or round-robin state. Never fall back to direct egress
// when the account explicitly binds proxies but all are unavailable.
func (p *Proxy) managedAccountClient(account Account) (*http.Client, error) {
	var selected *ProxyEntry
	for _, id := range account.Proxies {
		entry, ok := p.Proxies.Get(id)
		if !ok {
			continue
		}
		if entry.Enabled {
			selected = entry
			break
		}
	}
	if selected == nil && len(account.Proxies) > 0 {
		return nil, errors.New("no enabled account proxy")
	}
	base := p.clientFor(selected)
	client := *base
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Timeout = 60 * time.Second
	return &client, nil
}

func (p *Proxy) managedUpstreamURL(kind string) (string, error) {
	base := strings.TrimRight(p.BaseURL, "/")
	useBPS := p.BPS != nil
	if p.RuntimeModes {
		settings, err := p.settingsValue()
		if err != nil {
			return "", err
		}
		useBPS = settings.InferenceMode == inferencePowerPoint
		if !useBPS {
			base = codexURL
		}
	}
	if p.BPS != nil && (kind == "usage" || kind == "reset_credits") {
		base = codexURL
	}
	if base == "" {
		base = codexURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errInvalid
	}
	switch kind {
	case "models":
		if useBPS {
			u.Path = strings.TrimRight(u.Path, "/") + "/responses/access"
			u.RawQuery = "include_models=true"
			return u.String(), nil
		}
		u.Path = strings.TrimRight(u.Path, "/") + "/models"
		q := url.Values{"client_version": {clientVersion}}
		u.RawQuery = q.Encode()
	case "usage":
		u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/codex") + "/wham/usage"
	case "reset_credits":
		u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/codex") + "/wham/rate-limit-reset-credits"
	case "stream":
		u.Path = strings.TrimRight(u.Path, "/") + "/responses"
	case "compact":
		u.Path = strings.TrimRight(u.Path, "/") + "/responses/compact"
	default:
		return "", errInvalid
	}
	return u.String(), nil
}

func (p *Proxy) managedUpstreamHeaders(request *http.Request, account Account) error {
	if p.isBPSUpstreamRequest(request) {
		applyBPSHeaders(request.Header, account)
		request.Header.Set("Accept", "application/json")
		return nil
	}
	*request = *request.WithContext(withGatewayNativeAccountScope(request.Context(), account))
	request.Header.Set("Authorization", "Bearer "+account.AccessToken)
	request.Header.Set("Accept", "application/json")
	userAgent, originator := gatewayDefaultApplicationIdentity()
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Originator", originator)
	request.Header.Set("Version", clientVersion)
	if account.AccountID != "" {
		request.Header.Set("ChatGPT-Account-ID", account.AccountID)
	}
	settings, err := p.settingsValue()
	if err != nil {
		return err
	}
	return applyCodexFingerprint(request.Header, settings)
}

func (p *Proxy) handleManagedUpstreamCheck(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Version          int64  `json:"version"`
		Kind             string `json:"kind"`
		Model            string `json:"model"`
		Prompt           string `json:"prompt"`
		ConfirmInference bool   `json:"confirm_inference"`
	}
	if err := readJSONLimit(w, r, &input, 16<<10); err != nil {
		adminError(w, err)
		return
	}
	if input.Version < 1 || !accountHas([]string{"models", "usage", "stream", "compact", "reset_credits"}, input.Kind) {
		adminError(w, errors.New("positive version and a supported check kind are required"))
		return
	}
	if (input.Kind == "stream" || input.Kind == "compact") && (!input.ConfirmInference || !validManagedModel(input.Model)) {
		adminError(w, errors.New("inference tests require a model and confirm_inference=true"))
		return
	}
	if len(input.Prompt) > 4096 || input.Prompt != "" && input.Kind != "stream" && input.Kind != "compact" {
		adminError(w, errors.New("prompt is limited to 4096 bytes and inference tests"))
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
	if a.Version != input.Version {
		p.storeError(w, errVersionConflict)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	snapshot := p.checkManagedUpstreamOptions(ctx, a, input.Kind, input.Model, input.Prompt, nil, nil)
	if err := p.cacheManagedUpstream(r.Context(), snapshot); err != nil {
		p.storeError(w, err)
		return
	}
	if p.Store.DB != nil {
		result := "ok"
		if !snapshot.OK {
			result = "failed"
		}
		if _, err := p.Store.DB.Pool.Exec(r.Context(), "INSERT INTO audit_logs(action,entity_id,result) VALUES($1,$2,$3)", "account.check."+input.Kind, a.ID, result); err != nil {
			p.storeError(w, err)
			return
		}
	}
	p.enrichManagedWindowCosts(r.Context(), []*managedUpstreamSnapshot{&snapshot})
	writeJSON(w, snapshot)
}

func (p *Proxy) checkManagedUpstream(ctx context.Context, a Account, kind, model string) (snapshot managedUpstreamSnapshot) {
	return p.checkManagedUpstreamOptions(ctx, a, kind, model, "", nil, nil)
}

func (p *Proxy) checkManagedUpstreamOptions(ctx context.Context, a Account, kind, model, prompt string, phase func(string) error, delta func(string) error) (snapshot managedUpstreamSnapshot) {
	if kind == "stream" || kind == "compact" {
		admission, admitted, err := p.acquireGatewayAccountContext(ctx, a)
		if err == nil && !admitted {
			err = &gatewayAccountBusyError{}
		}
		if err != nil {
			status, _, _ := gatewaySelectionFailure(err)
			return managedUpstreamSnapshot{ID: a.ID, Version: a.Version, CredentialVersion: a.CredentialVersion, Kind: kind, Model: model,
				CheckedAt: time.Now().Unix(), Source: "codex_" + kind, HTTPCode: status, Error: &managedUpstreamError{"account_admission", err.Error()}}
		}
		defer admission.Release()
		ctx = admission.Context
	}
	useBPS := p.BPS != nil
	if p.RuntimeModes {
		settings, err := p.settingsValue()
		if err != nil {
			return managedUpstreamSnapshot{ID: a.ID, Kind: kind, Error: &managedUpstreamError{"configuration_error", "cannot read inference mode"}}
		}
		ctx = withInferenceMode(ctx, settings.InferenceMode)
		useBPS = settings.InferenceMode == inferencePowerPoint
		if settings.InferenceMode == inferenceAppServer && (kind == "models" || kind == "stream" || kind == "compact") {
			return p.checkOfficialManaged(ctx, a, kind, model, prompt, phase, delta)
		}
	}
	start := time.Now()
	snapshot = managedUpstreamSnapshot{ID: a.ID, Version: a.Version, CredentialVersion: a.CredentialVersion, Kind: kind, CheckedAt: start.Unix(), Source: "codex_" + kind, Data: nil}
	if useBPS {
		snapshot.Source = "powerpoint_" + kind
		if kind == "usage" || kind == "reset_credits" {
			snapshot.Source = "chatgpt_account_" + kind
		}
	}
	if kind == "stream" || kind == "compact" {
		snapshot.Model = model
	}
	defer func() { snapshot.LatencyMs = time.Since(start).Milliseconds() }()
	fail := func(code, message string) managedUpstreamSnapshot {
		snapshot.Error = &managedUpstreamError{code, message}
		return snapshot
	}
	endpoint, err := p.managedUpstreamURL(kind)
	if err != nil {
		return fail("configuration_error", "invalid management upstream configuration")
	}
	client, err := p.managedAccountClient(a)
	if !useBPS && requestInferenceMode(ctx) == inferenceCodexCLI && p.CodexTransport != nil {
		client, err = p.CodexTransport.managedAccountClient(a)
	}
	if err != nil {
		return fail("proxy_unavailable", "no available configured account proxy")
	}
	method := http.MethodGet
	var body io.Reader
	if kind == "stream" || kind == "compact" {
		method = http.MethodPost
		if prompt == "" {
			prompt = "Reply with exactly OK."
		}
		payload := map[string]any{"model": model, "instructions": "You are an assistant performing a connectivity test. Reply briefly.", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": prompt}}}}}
		if kind == "stream" {
			payload["stream"] = true
			payload["store"] = false
		}
		raw, _ := json.Marshal(payload)
		if useBPS {
			prepared, prepareErr := bps.Prepare(raw, bps.Options{TaskID: bpsUUID(), TurnID: bpsUUID(), ToolsVersion: p.BPS.ToolsVersion, Compact: kind == "compact", SearchDisabled: p.BPS.SearchDisabled})
			if prepareErr != nil {
				return fail("configuration_error", "cannot prepare upstream test request")
			}
			raw = prepared.Body
		}
		body = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fail("configuration_error", "cannot create management request")
	}
	if err = p.managedUpstreamHeaders(request, a); err != nil {
		return fail("configuration_error", "cannot load management request settings")
	}
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	if kind == "stream" {
		request.Header.Set("Accept", "text/event-stream")
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return fail("canceled", "management check was canceled or timed out")
		}
		return fail("connection_failed", "upstream connection failed")
	}
	defer response.Body.Close()
	snapshot.HTTPCode = response.StatusCode
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fail("upstream_http_error", "upstream rejected the management check; inspect HTTP status")
	}
	var data any
	if kind == "stream" {
		if phase != nil && phase("streaming") != nil {
			return fail("canceled", "management check client disconnected")
		}
		data, err = readManagedTestStreamEvents(io.LimitReader(response.Body, managedUpstreamBodyLimit+1), delta)
	} else {
		var raw []byte
		raw, err = io.ReadAll(io.LimitReader(response.Body, managedUpstreamBodyLimit+1))
		if len(raw) > managedUpstreamBodyLimit {
			return fail("response_too_large", "upstream response exceeds 4 MiB")
		}
		if err == nil {
			switch kind {
			case "models":
				if useBPS {
					raw, err = bps.ModelsFromAccess(raw)
					if err != nil {
						break
					}
				}
				data, err = parseManagedModels(raw)
			case "usage":
				data, err = parseManagedUsage(raw)
			case "compact":
				data, err = parseManagedCompact(raw)
			case "reset_credits":
				data, err = parseManagedResetCredits(raw)
			}
		}
	}
	if err != nil {
		return fail("invalid_upstream_response", "upstream did not return a successful complete "+kind+" result")
	}
	if ctx.Err() != nil {
		return fail("canceled", "management check was canceled or timed out")
	}
	snapshot.OK = true
	snapshot.Data = data
	return snapshot
}

type managedModel struct {
	ID            string `json:"id"`
	DisplayName   string `json:"display_name"`
	ContextWindow *int64 `json:"context_window,omitempty"`
}

func parseManagedModels(raw []byte) (any, error) {
	var payload struct {
		Models []struct {
			Slug          string `json:"slug"`
			ID            string `json:"id"`
			DisplayName   string `json:"display_name"`
			ContextWindow *int64 `json:"context_window"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Models == nil {
		return nil, errors.New("missing model catalog")
	}
	models := make([]managedModel, 0, len(payload.Models))
	seen := map[string]bool{}
	for _, m := range payload.Models {
		id := m.Slug
		if id == "" {
			id = m.ID
		}
		if !validManagedModel(id) || seen[id] {
			continue
		}
		seen[id] = true
		if m.DisplayName == "" {
			m.DisplayName = id
		}
		if len(m.DisplayName) > 500 {
			return nil, errInvalid
		}
		models = append(models, managedModel{id, m.DisplayName, m.ContextWindow})
	}
	return map[string]any{"models": models, "scope": "upstream_catalog", "inference_verified": false}, nil
}

type managedRateWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds *int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  *int64   `json:"reset_after_seconds"`
	ResetAt            *int64   `json:"reset_at"`
}
type managedRateLimit struct {
	Allowed         *bool              `json:"allowed"`
	LimitReached    *bool              `json:"limit_reached"`
	PrimaryWindow   *managedRateWindow `json:"primary_window"`
	SecondaryWindow *managedRateWindow `json:"secondary_window"`
}
type managedCredits struct {
	HasCredits          *bool   `json:"has_credits"`
	Unlimited           *bool   `json:"unlimited"`
	Balance             *string `json:"balance"`
	OverageLimitReached *bool   `json:"overage_limit_reached"`
}

type managedSpendLimit struct {
	Limit             *string  `json:"limit"`
	Used              *string  `json:"used"`
	Remaining         *string  `json:"remaining"`
	UsedPercent       *float64 `json:"used_percent"`
	RemainingPercent  *float64 `json:"remaining_percent"`
	ResetAfterSeconds *int64   `json:"reset_after_seconds"`
	ResetAt           *int64   `json:"reset_at"`
}

type managedSpendControl struct {
	Reached         *bool              `json:"reached"`
	IndividualLimit *managedSpendLimit `json:"individual_limit"`
}

type managedRateLimitReachedType struct {
	Type string `json:"type"`
}

type managedModelUsage struct {
	Available          *bool   `json:"available"`
	CreditsWouldEnable *bool   `json:"credits_would_enable"`
	AvailableAt        *string `json:"available_at"`
}

type managedUsageResetCredits struct {
	AvailableCount           *int64 `json:"available_count"`
	ApplicableAvailableCount *int64 `json:"applicable_available_count"`
}

type managedAdditionalRateLimit struct {
	LimitName       string            `json:"limit_name"`
	MeteredFeature  string            `json:"metered_feature"`
	NormalModelSlug *string           `json:"normal_model_slug"`
	RateLimit       *managedRateLimit `json:"rate_limit"`
}

type managedUsage struct {
	PlanType             string                       `json:"plan_type"`
	RateLimit            *managedRateLimit            `json:"rate_limit"`
	Credits              *managedCredits              `json:"credits"`
	SpendControl         *managedSpendControl         `json:"spend_control"`
	RateLimitReachedType *managedRateLimitReachedType `json:"rate_limit_reached_type"`
	ModelUsage           map[string]managedModelUsage `json:"model_usage"`
	ResetCredits         *managedUsageResetCredits    `json:"rate_limit_reset_credits"`
	Additional           []managedAdditionalRateLimit `json:"additional_rate_limits"`
}

func parseManagedUsage(raw []byte) (any, error) {
	var usage managedUsage
	type core managedUsage
	optional := struct {
		*core
		SpendControl json.RawMessage `json:"spend_control"`
		ReachedType  json.RawMessage `json:"rate_limit_reached_type"`
		ModelUsage   json.RawMessage `json:"model_usage"`
	}{core: (*core)(&usage)}
	if err := json.Unmarshal(raw, &optional); err != nil || usage.PlanType == "" || len(usage.PlanType) > 100 {
		return nil, errors.New("missing usage payload")
	}
	usage.RateLimitReachedType = managedOptionalUsageValue[managedRateLimitReachedType](optional.ReachedType)
	if fields := managedOptionalUsageValue[map[string]json.RawMessage](optional.SpendControl); fields != nil {
		usage.SpendControl = &managedSpendControl{Reached: managedOptionalUsageValue[bool]((*fields)["reached"])}
		if limit := managedOptionalUsageValue[map[string]json.RawMessage]((*fields)["individual_limit"]); limit != nil {
			usage.SpendControl.IndividualLimit = &managedSpendLimit{
				Limit: managedOptionalUsageValue[string]((*limit)["limit"]), Used: managedOptionalUsageValue[string]((*limit)["used"]), Remaining: managedOptionalUsageValue[string]((*limit)["remaining"]),
				UsedPercent: managedOptionalUsageValue[float64]((*limit)["used_percent"]), RemainingPercent: managedOptionalUsageValue[float64]((*limit)["remaining_percent"]),
				ResetAt: managedOptionalUsageValue[int64]((*limit)["reset_at"]), ResetAfterSeconds: managedOptionalUsageValue[int64]((*limit)["reset_after_seconds"]),
			}
		}
	}
	if models := managedOptionalUsageValue[map[string]json.RawMessage](optional.ModelUsage); models != nil {
		if len(*models) > 512 {
			return nil, errors.New("too many model usage entries")
		}
		usage.ModelUsage = make(map[string]managedModelUsage, len(*models))
		for name, rawModel := range *models {
			if fields := managedOptionalUsageValue[map[string]json.RawMessage](rawModel); fields != nil {
				usage.ModelUsage[name] = managedModelUsage{Available: managedOptionalUsageValue[bool]((*fields)["available"]), CreditsWouldEnable: managedOptionalUsageValue[bool]((*fields)["credits_would_enable"]), AvailableAt: managedOptionalUsageValue[string]((*fields)["available_at"])}
			}
		}
	}
	// Fixed official rate-limit reasons have user/workspace semantics. Unknown
	// future text is not an account flag and must not be surfaced as a reason.
	if reached := usage.RateLimitReachedType; reached != nil {
		switch reached.Type {
		case "rate_limit_reached", "workspace_owner_credits_depleted", "workspace_member_credits_depleted",
			"workspace_owner_usage_limit_reached", "workspace_member_usage_limit_reached", "unknown":
		default:
			reached.Type = "unknown"
		}
	}
	if len(usage.ModelUsage) > 512 {
		return nil, errors.New("too many model usage entries")
	}
	for model, availability := range usage.ModelUsage {
		if !managedUsageModelSlug(model) {
			delete(usage.ModelUsage, model)
			continue
		}
		if availability.AvailableAt != nil {
			if len(*availability.AvailableAt) > 64 {
				availability.AvailableAt = nil
			} else if _, err := time.Parse(time.RFC3339Nano, *availability.AvailableAt); err != nil {
				// Optional time formats may evolve. Do not fabricate an epoch or
				// lose explicit availability booleans because timing is unknown.
				availability.AvailableAt = nil
			}
			usage.ModelUsage[model] = availability
		}
	}
	for index := range usage.Additional {
		if slug := usage.Additional[index].NormalModelSlug; slug != nil && !managedUsageModelSlug(*slug) {
			usage.Additional[index].NormalModelSlug = nil
		}
	}
	if usage.SpendControl != nil && usage.SpendControl.IndividualLimit != nil {
		limit := usage.SpendControl.IndividualLimit
		for _, value := range []**string{&limit.Limit, &limit.Used, &limit.Remaining} {
			if *value != nil && !managedUsageAmount(**value) {
				// Amounts are backend-owned strings. Preserve exact numeric
				// spellings, including signed/exponent values. Unknown formats
				// remain unknown without discarding other observed quota fields.
				*value = nil
			}
		}
	}
	return usage, nil
}

// Only newly observed optional fields are tolerant. Existing primary quota,
// credits and reset-count schemas still reject invalid core types as before.
func managedOptionalUsageValue[T any](raw json.RawMessage) *T {
	var value *T
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return value
}

func (value *managedCredits) UnmarshalJSON(raw []byte) error {
	type core managedCredits
	input := struct {
		*core
		Overage json.RawMessage `json:"overage_limit_reached"`
	}{core: (*core)(value)}
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	value.OverageLimitReached = managedOptionalUsageValue[bool](input.Overage)
	return nil
}

func (value *managedUsageResetCredits) UnmarshalJSON(raw []byte) error {
	type core managedUsageResetCredits
	input := struct {
		*core
		Applicable json.RawMessage `json:"applicable_available_count"`
	}{core: (*core)(value)}
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	value.ApplicableAvailableCount = managedOptionalUsageValue[int64](input.Applicable)
	return nil
}

func (value *managedAdditionalRateLimit) UnmarshalJSON(raw []byte) error {
	type core managedAdditionalRateLimit
	input := struct {
		*core
		Normal json.RawMessage `json:"normal_model_slug"`
	}{core: (*core)(value)}
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	value.NormalModelSlug = managedOptionalUsageValue[string](input.Normal)
	return nil
}

func managedUsageModelSlug(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("._:/-", char)) {
			return false
		}
	}
	return true
}

func managedUsageAmount(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	return (value[0] == '-' || value[0] >= '0' && value[0] <= '9') && json.Valid([]byte(value))
}

func parseManagedCompact(raw []byte) (any, error) {
	var response struct {
		Output []struct {
			Type             string `json:"type"`
			EncryptedContent string `json:"encrypted_content"`
		} `json:"output"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	if len(response.Error) > 0 && string(response.Error) != "null" {
		return nil, errInvalid
	}
	for _, item := range response.Output {
		if item.Type == "compaction" && item.EncryptedContent != "" {
			return map[string]any{"compaction_found": true}, nil
		}
	}
	return nil, errors.New("missing compaction item")
}

func readManagedTestStream(reader io.Reader) (any, error) {
	return readManagedTestStreamEvents(reader, nil)
}
func readManagedTestStreamEvents(reader io.Reader, emit func(string) error) (any, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var data strings.Builder
	var output strings.Builder
	completed := false
	total := 0
	var usage *struct {
		InputTokens  *int64 `json:"input_tokens"`
		OutputTokens *int64 `json:"output_tokens"`
		TotalTokens  *int64 `json:"total_tokens"`
	}
	process := func() error {
		if data.Len() == 0 {
			return nil
		}
		raw := strings.TrimSpace(data.String())
		data.Reset()
		if raw == "[DONE]" {
			return nil
		}
		var event struct {
			Type     string `json:"type"`
			Delta    string `json:"delta"`
			Response struct {
				Status string          `json:"status"`
				Usage  json.RawMessage `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			return err
		}
		switch event.Type {
		case "error", "response.failed", "response.incomplete":
			return errors.New("failed test response")
		case "response.output_text.delta":
			if output.Len()+len(event.Delta) <= 4096 {
				output.WriteString(event.Delta)
				if emit != nil {
					if err := emit(event.Delta); err != nil {
						return err
					}
				}
			}
		case "response.completed":
			if event.Response.Status != "" && event.Response.Status != "completed" {
				return errInvalid
			}
			completed = true
			if len(event.Response.Usage) > 0 {
				if err := json.Unmarshal(event.Response.Usage, &usage); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		total += len(line) + 1
		if total > managedUpstreamBodyLimit {
			return nil, errors.New("test response too large")
		}
		if line == "" {
			if err := process(); err != nil {
				return nil, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := process(); err != nil {
		return nil, err
	}
	if !completed {
		return nil, errors.New("test stream incomplete")
	}
	return map[string]any{"completed": true, "text": output.String(), "usage": usage}, nil
}
