package server

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode"

	"codex-agent/internal/bps"
)

const bpsDefaultStaticModelRoutes = `# source=target; * is the fallback for unlisted request models
gpt-5.6-luna=gpt-5.6-luna
gpt-6-luna=gpt-6-luna
gpt-5.6-terra=gpt-5.6-terra
gpt-5.6-sol=gpt-5.6-sol
gpt-6-sol=gpt-6-sol
gpt-6-astra=gpt-6-astra
gpt-reserve=gpt-5.6-sol
gpt-5.5=gpt-5.6-sol
gpt-5.4=gpt-5.6-sol
gpt-5.3-codex=gpt-5.6-sol
gpt-5.2=gpt-5.6-sol
gpt-5.2-codex=gpt-5.6-sol
gpt-5.1=gpt-5.6-sol
gpt-5.1-codex=gpt-5.6-sol
gpt-5=gpt-5.6-sol
gpt-5-codex=gpt-5.6-sol
gpt-5.1-codex-max=gpt-5.6-sol
codex-auto=gpt-6-astra
codex-auto-review=gpt-6-astra
codex-mini-latest=gpt-5.6-luna
gpt-5.1-codex-mini=gpt-5.6-luna
gpt-5.3-codex-spark=gpt-5.6-luna
gpt-5.4-mini=gpt-5.6-luna
*=gpt-6-astra`

func defaultBPSModelRoutes() string { return bpsDefaultStaticModelRoutes }

// This order is a gateway routing preference, not a measured quality ranking.
// Return a fresh slice so settings callers cannot mutate the built-in catalog.
func bpsStaticModelIDs() []string {
	return []string{"gpt-5.6-luna", "gpt-6-luna", "gpt-5.6-terra", "gpt-5.6-sol", "gpt-6-sol", "gpt-6-astra"}
}

func bpsStaticModel(id string) (bps.AccessModel, bool) {
	found := false
	for _, known := range bpsStaticModelIDs() {
		if known == id {
			found = true
			break
		}
	}
	if !found {
		return bps.AccessModel{}, false
	}
	model := bps.AccessModel{ID: id, DefaultEffort: "medium"}
	levels := []string{"none", "low", "medium", "high", "xhigh"}
	if id == "gpt-6-astra" {
		levels = levels[1:]
	}
	for _, level := range levels {
		model.Efforts = append(model.Efforts, struct {
			Value       string `json:"value"`
			Label       string `json:"label"`
			Description string `json:"description"`
		}{Value: level})
	}
	return model, true
}

func bpsStaticRouteToken(value string) bool {
	return value != "" && !strings.ContainsRune(value, '=') && strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

func parseBPSStaticModelRoutes(rules string) (map[string]string, error) {
	parsed := make(map[string]string)
	for index, raw := range strings.Split(rules, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		source, target, ok := strings.Cut(line, "=")
		source, target = strings.TrimSpace(source), strings.TrimSpace(target)
		if !ok || !bpsStaticRouteToken(source) || !bpsStaticRouteToken(target) {
			return nil, fmt.Errorf("model route line %d must contain one source=target pair without whitespace inside model names", index+1)
		}
		if _, found := bpsStaticModel(target); !found {
			return nil, fmt.Errorf("model route line %d must target a supported upstream model", index+1)
		}
		if _, duplicate := parsed[source]; duplicate {
			return nil, fmt.Errorf("model route line %d repeats an earlier source model", index+1)
		}
		parsed[source] = target
	}
	if len(parsed) == 0 {
		return nil, errors.New("model routing rules must not be empty")
	}
	if parsed["*"] == "" {
		return nil, errors.New("model routing rules must include a *=target fallback")
	}
	return parsed, nil
}

func validateBPSModelRoutes(rules string) error {
	_, err := parseBPSStaticModelRoutes(rules)
	return err
}

// Only the most recently used rule set is retained. Published maps are never
// mutated, so an in-flight request can safely finish after settings are changed.
var bpsStaticRouteCache struct {
	sync.Mutex
	rules  string
	parsed map[string]string
}

func cachedBPSStaticModelRoutes(rules string) (map[string]string, error) {
	bpsStaticRouteCache.Lock()
	defer bpsStaticRouteCache.Unlock()
	if bpsStaticRouteCache.parsed != nil && bpsStaticRouteCache.rules == rules {
		return bpsStaticRouteCache.parsed, nil
	}
	parsed, err := parseBPSStaticModelRoutes(rules)
	if err != nil {
		return nil, err
	}
	bpsStaticRouteCache.rules, bpsStaticRouteCache.parsed = rules, parsed
	return parsed, nil
}

// chooseBPSStaticModelRoute performs exactly one local rule lookup followed by
// reasoning compatibility. A target is final: rules are never recursively
// followed, and this function does not fetch catalogs or attempt generation.
func chooseBPSStaticModelRoute(rules, model, effort string) (bpsModelRoute, error) {
	route := bpsModelRoute{RequestedModel: model, ResolvedModel: model, RequestedEffort: effort, ResolvedEffort: effort}
	if model == "" || !bpsStaticRouteToken(model) {
		return route, &bpsModelRoutingError{StatusCode: 400, Param: "model", Code: "model_not_available", Message: "a nonempty model name without whitespace is required for upstream routing"}
	}
	// Legacy literal Settings values predate this field. Explicit settings
	// validation still rejects an empty configuration; runtime zero values use
	// the same built-in defaults as a newly initialized settings store.
	if rules == "" {
		rules = defaultBPSModelRoutes()
	}
	parsed, err := cachedBPSStaticModelRoutes(rules)
	if err != nil {
		return route, &bpsModelRoutingError{StatusCode: 503, Param: "model", Code: "model_routing_configuration_invalid", Message: "configured upstream model routing rules are invalid"}
	}
	target, exists := parsed[model]
	if !exists {
		target = parsed["*"]
	}
	selected, found := bpsStaticModel(target)
	if !found {
		return route, &bpsModelRoutingError{StatusCode: 503, Param: "model", Code: "model_routing_configuration_invalid", Message: "configured upstream model target is invalid"}
	}
	strongestDefault := model == "codex-auto" || model == "codex-auto-review"
	resolvedEffort, err := bpsCompatibleEffort(selected, effort, strongestDefault)
	if err != nil {
		return route, err
	}
	route.ResolvedModel, route.ResolvedEffort = target, resolvedEffort
	route.Changed = route.RequestedModel != route.ResolvedModel || route.RequestedEffort != route.ResolvedEffort
	return route, nil
}
