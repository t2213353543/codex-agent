package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"codex-agent/internal/bps"
)

const bpsModelCatalogTTL = time.Minute

type bpsModelRoute struct {
	RequestedModel  string `json:"requested_model"`
	ResolvedModel   string `json:"resolved_model"`
	RequestedEffort string `json:"requested_effort,omitempty"`
	ResolvedEffort  string `json:"resolved_effort,omitempty"`
	Changed         bool   `json:"changed"`
	CatalogChecked  bool   `json:"catalog_checked"`
}

type bpsModelRoutingError struct {
	StatusCode int
	Param      string
	Code       string
	Message    string
}

func (e *bpsModelRoutingError) Error() string { return e.Message }

type bpsModelCatalogEntry struct {
	catalog *bps.AccessCatalog
	expires time.Time
}

type bpsModelCatalogFlight struct {
	done    chan struct{}
	catalog *bps.AccessCatalog
	err     error
}

type bpsModelCatalogCache struct {
	mu      sync.Mutex
	entries map[string]bpsModelCatalogEntry
	pending map[string]*bpsModelCatalogFlight
}

func bpsModelCatalogCacheKey(account Account, proxy *ProxyEntry, keyID, baseURL string) string {
	proxyHash := "direct"
	if proxy != nil {
		digest := gatewayProxyFingerprint(*proxy)
		proxyHash = proxy.ID + ":" + hex.EncodeToString(digest[:])
	}
	raw, _ := json.Marshal([]any{"bps-model-catalog-v1", keyID, gatewayAccountRef(account), account.CredentialVersion, strings.TrimRight(baseURL, "/"), proxyHash})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func (c *bpsModelCatalogCache) pruneLocked(now time.Time) {
	for key, entry := range c.entries {
		if !entry.expires.After(now) {
			delete(c.entries, key)
		}
	}
}

func (p *Proxy) cachedBPSModelCatalog(account Account, proxy *ProxyEntry, keyID string) *bps.AccessCatalog {
	if p.BPS == nil {
		return nil
	}
	c := &p.BPS.modelCatalog
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(time.Now())
	return c.entries[bpsModelCatalogCacheKey(account, proxy, keyID, p.BaseURL)].catalog
}

// A successful management /models read can seed the same immutable catalog
// used by subsequent generation requests for this owner/credential/route.
func (p *Proxy) rememberBPSModelCatalog(account Account, proxy *ProxyEntry, keyID string, catalog *bps.AccessCatalog) {
	if p.BPS == nil || catalog == nil || !catalog.Allowed || len(catalog.ModelCatalog.Models) == 0 {
		return
	}
	c := &p.BPS.modelCatalog
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(time.Now())
	if c.entries == nil {
		c.entries = make(map[string]bpsModelCatalogEntry)
	}
	c.entries[bpsModelCatalogCacheKey(account, proxy, keyID, p.BaseURL)] = bpsModelCatalogEntry{catalog: catalog, expires: time.Now().Add(bpsModelCatalogTTL)}
}

func (p *Proxy) loadBPSModelCatalog(ctx context.Context, r *http.Request, account Account, proxy *ProxyEntry, keyID string, settings Settings) (*bps.AccessCatalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.BPS == nil {
		return nil, errors.New("upstream model catalog is unavailable")
	}
	if catalog := p.cachedBPSModelCatalog(account, proxy, keyID); catalog != nil {
		return catalog, nil
	}
	key := bpsModelCatalogCacheKey(account, proxy, keyID, p.BaseURL)
	c := &p.BPS.modelCatalog
	c.mu.Lock()
	if entry := c.entries[key]; entry.catalog != nil && entry.expires.After(time.Now()) {
		c.mu.Unlock()
		return entry.catalog, nil
	}
	if flight := c.pending[key]; flight != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-flight.done:
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return flight.catalog, flight.err
		}
	}
	if c.pending == nil {
		c.pending = make(map[string]*bpsModelCatalogFlight)
	}
	flight := &bpsModelCatalogFlight{done: make(chan struct{})}
	c.pending[key] = flight
	c.mu.Unlock()
	catalog, err := p.fetchBPSModelCatalog(ctx, r, account, proxy, keyID, settings)
	c.mu.Lock()
	if err == nil {
		if c.entries == nil {
			c.entries = make(map[string]bpsModelCatalogEntry)
		}
		c.entries[key] = bpsModelCatalogEntry{catalog: catalog, expires: time.Now().Add(bpsModelCatalogTTL)}
	}
	flight.catalog, flight.err = catalog, err
	delete(c.pending, key)
	close(flight.done)
	c.mu.Unlock()
	return catalog, err
}

func (p *Proxy) fetchBPSModelCatalog(ctx context.Context, r *http.Request, account Account, proxy *ProxyEntry, keyID string, settings Settings) (*bps.AccessCatalog, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Catalog transport timing must not replace the inference body/attempt or
	// cache diagnostics. This is an account-bound metadata GET, never generation.
	ctx = context.WithValue(ctx, gatewayAttemptObservationContextKey{}, (*gatewayAttemptObservation)(nil))
	current, err := p.gatewayCurrentBoundAccountContext(ctx, account, gatewayAccountRef(account))
	if err != nil {
		return nil, err
	}
	var rec RequestRecord
	res, err := p.bpsRequest(ctx, r, current, proxy, keyID, "/responses/access", nil, settings, &rec)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil, &bpsModelRoutingError{StatusCode: res.StatusCode, Param: "model", Code: "model_catalog_unavailable", Message: "upstream model catalog is unavailable for the selected account"}
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, (8<<20)+1))
	if err != nil || len(raw) > 8<<20 {
		return nil, &bpsModelRoutingError{StatusCode: 502, Param: "model", Code: "model_catalog_unavailable", Message: "upstream model catalog could not be read"}
	}
	var access struct {
		Allowed *bool `json:"allowed"`
	}
	if json.Unmarshal(raw, &access) == nil && access.Allowed != nil && !*access.Allowed {
		return nil, &bpsModelRoutingError{StatusCode: 403, Param: "model", Code: "model_access_denied", Message: "upstream model access is unavailable for the selected account"}
	}
	catalog, err := bps.ParseAccessCatalog(raw)
	if err != nil {
		return nil, &bpsModelRoutingError{StatusCode: 502, Param: "model", Code: "model_catalog_unavailable", Message: "upstream returned an invalid or unavailable model catalog"}
	}
	if _, err = p.gatewayCurrentBoundAccountContext(ctx, current, gatewayAccountRef(account)); err != nil {
		return nil, err
	}
	return catalog, nil
}

// bpsModelPreference is this gateway's configured compatibility preference,
// not an assertion about measured model quality. Exact advertised models win.
var bpsModelPreference = []string{
	"gpt-5.6-luna", "gpt-6-luna", "gpt-5.6-terra", "gpt-5.6-sol", "gpt-6-sol", "gpt-6-astra",
}

func bpsModelCandidates(model string) ([]string, bool) {
	target, strongestDefault := len(bpsModelPreference)-1, false
	for index, known := range bpsModelPreference {
		if known == model {
			target = index
			break
		}
	}
	switch model {
	case "gpt-reserve", "gpt-5.5", "gpt-5.4", "gpt-5.3-codex", "gpt-5.2", "gpt-5.2-codex", "gpt-5.1", "gpt-5.1-codex", "gpt-5", "gpt-5-codex", "gpt-5.1-codex-max":
		target = 3 // existing full-size aliases prefer gpt-5.6-sol
	case "codex-auto", "codex-auto-review":
		target, strongestDefault = len(bpsModelPreference)-1, true
	case "codex-mini-latest", "gpt-5.1-codex-mini", "gpt-5.3-codex-spark", "gpt-5.4-mini":
		target = 0
	}
	ordered := make([]string, 0, len(bpsModelPreference))
	for index := target; index < len(bpsModelPreference); index++ {
		ordered = append(ordered, bpsModelPreference[index])
	}
	for index := target - 1; index >= 0; index-- {
		ordered = append(ordered, bpsModelPreference[index])
	}
	return ordered, strongestDefault
}

// The access catalog has no explicit modality field in the current protocol.
// For fallback only, retain recognizable text families and exclude names that
// explicitly describe incompatible endpoints. Exact advertised IDs bypass this
// preference classification so a future upstream ID is not renamed on sight.
func bpsFallbackTextModel(id string) bool {
	name := strings.ToLower(strings.TrimSpace(id))
	if name == "" {
		return false
	}
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == '/' || r == ':' }) {
		switch part {
		case "image", "images", "audio", "speech", "tts", "whisper", "transcribe", "transcription", "realtime", "embedding", "embeddings", "moderation", "sora", "video", "dall":
			return false
		}
	}
	if strings.HasPrefix(name, "gpt-") || strings.HasPrefix(name, "codex-") || strings.HasPrefix(name, "text-") {
		return true
	}
	return len(name) > 1 && name[0] == 'o' && name[1] >= '0' && name[1] <= '9'
}

var bpsEffortRank = map[string]int{"none": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6, "ultra": 7}

func bpsCompatibleEffort(model bps.AccessModel, requested string, strongestDefault bool) (string, error) {
	if requested == "" && !strongestDefault {
		return "", nil
	}
	for _, effort := range model.Efforts {
		if requested != "" && effort.Value == requested {
			return requested, nil
		}
	}
	rank, known := bpsEffortRank[requested]
	if requested == "" && strongestDefault {
		rank, known = bpsEffortRank["ultra"], true
	}
	if !known {
		return "", &bpsModelRoutingError{StatusCode: 400, Param: "reasoning.effort", Code: "unsupported_parameter", Message: fmt.Sprintf("reasoning effort %q is not advertised for the selected upstream model", requested)}
	}
	below, above := "", ""
	belowRank, aboveRank := -1, int(^uint(0)>>1)
	for _, effort := range model.Efforts {
		candidateRank, ok := bpsEffortRank[effort.Value]
		if !ok {
			continue
		}
		if candidateRank <= rank && candidateRank > belowRank {
			below, belowRank = effort.Value, candidateRank
		}
		if candidateRank > rank && candidateRank < aboveRank {
			above, aboveRank = effort.Value, candidateRank
		}
	}
	// Upgrade to the nearest advertised effort before considering downgrade.
	if above != "" {
		return above, nil
	}
	if below != "" {
		return below, nil
	}
	return "", &bpsModelRoutingError{StatusCode: 400, Param: "reasoning.effort", Code: "unsupported_parameter", Message: "the selected upstream model advertises no compatible reasoning effort"}
}

func chooseBPSModelRoute(catalog *bps.AccessCatalog, model, effort string) (bpsModelRoute, error) {
	route := bpsModelRoute{RequestedModel: model, ResolvedModel: model, RequestedEffort: effort, ResolvedEffort: effort, CatalogChecked: true}
	if catalog == nil || !catalog.Allowed {
		return route, &bpsModelRoutingError{StatusCode: 403, Param: "model", Code: "model_access_denied", Message: "upstream model access is unavailable for the selected account"}
	}
	byID := make(map[string]bps.AccessModel, len(catalog.ModelCatalog.Models))
	restricted := make(map[string]bool, len(catalog.ModelCatalog.RestrictedModels))
	for _, raw := range catalog.ModelCatalog.RestrictedModels {
		var id string
		if json.Unmarshal(raw, &id) == nil {
			restricted[id] = true
		}
	}
	for _, entry := range catalog.ModelCatalog.Models {
		if !restricted[entry.ID] {
			byID[entry.ID] = entry
		}
	}
	resolve := func(selected bps.AccessModel, strongestDefault bool) (bpsModelRoute, error) {
		resolvedEffort, err := bpsCompatibleEffort(selected, effort, strongestDefault)
		if err != nil {
			return route, err
		}
		resolved := route
		resolved.ResolvedModel, resolved.ResolvedEffort = selected.ID, resolvedEffort
		resolved.Changed = resolved.RequestedModel != resolved.ResolvedModel || resolved.RequestedEffort != resolved.ResolvedEffort
		return resolved, nil
	}
	if exact, found := byID[model]; found {
		// An available exact model is retained. An unknown effort does not
		// authorize moving an otherwise valid request to a different model.
		return resolve(exact, false)
	}
	candidates, strongestDefault := bpsModelCandidates(model)
	visited := make(map[string]bool, len(candidates))
	var effortFailure error
	for _, candidate := range candidates {
		visited[candidate] = true
		if advertised, ok := byID[candidate]; ok {
			resolved, err := resolve(advertised, strongestDefault)
			if err == nil {
				return resolved, nil
			}
			if effortFailure == nil {
				effortFailure = err
			}
		}
	}
	// If no known preference can satisfy the full request, future text models
	// follow directory order. Effort support is still required, never invented.
	for _, entry := range catalog.ModelCatalog.Models {
		if !visited[entry.ID] && !restricted[entry.ID] && bpsFallbackTextModel(entry.ID) {
			resolved, err := resolve(entry, strongestDefault)
			if err == nil {
				return resolved, nil
			}
			if effortFailure == nil {
				effortFailure = err
			}
		}
	}
	if effortFailure != nil {
		return route, effortFailure
	}
	return route, &bpsModelRoutingError{StatusCode: 400, Param: "model", Code: "model_not_available", Message: fmt.Sprintf("model %q has no compatible model in the selected account's current catalog", model)}
}

func (p *Proxy) resolveBPSModelRoute(ctx context.Context, r *http.Request, account Account, proxy *ProxyEntry, keyID string, prepared *bps.Request, settings Settings) (bpsModelRoute, error) {
	if err := ctx.Err(); err != nil {
		return bpsModelRoute{}, err
	}
	if prepared == nil || !json.Valid(prepared.Body) {
		return bpsModelRoute{}, errors.New("invalid prepared request for model routing")
	}
	spans := gatewayValidatedJSONFieldSpans(prepared.Body, "model", "reasoning_effort")
	model, effort := gatewayJSONSpanString(prepared.Body, spans[0]), gatewayJSONSpanString(prepared.Body, spans[1])
	if model == "" {
		return bpsModelRoute{}, &bpsModelRoutingError{StatusCode: 400, Param: "model", Code: "model_not_available", Message: "a nonempty model is required for upstream routing"}
	}
	if prepared.Compact && effort == "" {
		effort = bpsAttachmentString(prepared.Policy.ClientControls["reasoning_effort"])
	}
	route := bpsModelRoute{RequestedModel: model, ResolvedModel: model, RequestedEffort: effort, ResolvedEffort: effort}
	route, err := chooseBPSStaticModelRoute(settings.BPSModelRoutes, model, effort)
	if err != nil || !route.Changed {
		return route, err
	}
	var edits []gatewayJSONEdit
	if route.ResolvedModel != model {
		raw, _ := json.Marshal(route.ResolvedModel)
		edits = append(edits, gatewayJSONEdit{gatewayJSONFieldSpan: spans[0], replacement: raw})
	}
	if !prepared.Compact && route.ResolvedEffort != effort && spans[1].end != 0 {
		raw, _ := json.Marshal(route.ResolvedEffort)
		edits = append(edits, gatewayJSONEdit{gatewayJSONFieldSpan: spans[1], replacement: raw})
	}
	out := gatewayApplyJSONEdits(prepared.Body, edits, gatewayJSONEditedLength(prepared.Body, edits))
	if !prepared.Compact && route.ResolvedEffort != effort && spans[1].end == 0 {
		end := len(bytes.TrimRight(out, " \t\r\n")) - 1
		raw, _ := json.Marshal(route.ResolvedEffort)
		field := append([]byte(`,"reasoning_effort":`), raw...)
		next := make([]byte, 0, len(out)+len(field))
		next = append(next, out[:end]...)
		next = append(next, field...)
		next = append(next, out[end:]...)
		out = next
	}
	prepared.Body, prepared.Model = out, route.ResolvedModel
	if prepared.Compact && route.ResolvedEffort != effort {
		if prepared.Policy.ClientControls == nil {
			prepared.Policy.ClientControls = make(map[string]json.RawMessage)
		}
		prepared.Policy.ClientControls["reasoning_effort"], _ = json.Marshal(route.ResolvedEffort)
	}
	prepared.Adaptations = append(prepared.Adaptations, "model/reasoning routed using configured static mapping")
	return route, nil
}
