package server

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestBPSStaticModelDefaultRoutesCoverKnownModelsAndAliases(t *testing.T) {
	rules := defaultBPSModelRoutes()
	if err := validateBPSModelRoutes(rules); err != nil {
		t.Fatal(err)
	}
	parsed, err := parseBPSStaticModelRoutes(rules)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 24 {
		t.Fatalf("rules=%d want six exact+17 aliases+fallback", len(parsed))
	}
	for _, model := range bpsStaticModelIDs() {
		route, err := chooseBPSStaticModelRoute(rules, model, "high")
		if err != nil || route.Changed || route.CatalogChecked || route.ResolvedModel != model || route.ResolvedEffort != "high" {
			t.Fatalf("exact static model changed: %+v %v", route, err)
		}
	}
	for _, alias := range []string{"gpt-reserve", "gpt-5.5", "gpt-5.4", "gpt-5.3-codex", "gpt-5.2", "gpt-5.2-codex", "gpt-5.1", "gpt-5.1-codex", "gpt-5", "gpt-5-codex", "gpt-5.1-codex-max"} {
		if parsed[alias] != "gpt-5.6-sol" {
			t.Fatalf("missing full alias %s", alias)
		}
	}
	for _, alias := range []string{"codex-auto", "codex-auto-review"} {
		route, err := chooseBPSStaticModelRoute(rules, alias, "")
		if err != nil || route.ResolvedModel != "gpt-6-astra" || route.ResolvedEffort != "xhigh" || route.CatalogChecked {
			t.Fatalf("auto default changed: %+v %v", route, err)
		}
	}
	for _, alias := range []string{"codex-mini-latest", "gpt-5.1-codex-mini", "gpt-5.3-codex-spark", "gpt-5.4-mini"} {
		if parsed[alias] != "gpt-5.6-luna" {
			t.Fatalf("missing mini alias %s", alias)
		}
	}
	for _, model := range []string{"future-missing-name", "gpt-5.5-codex", "codex-unknown"} {
		route, err := chooseBPSStaticModelRoute(rules, model, "")
		if err != nil || route.ResolvedModel != "gpt-6-astra" || route.ResolvedEffort != "" {
			t.Fatalf("unknown wildcard failed: %+v %v", route, err)
		}
	}
}

func TestBPSStaticModelRulesOverrideAndTargetsAreFinal(t *testing.T) {
	legacy, err := chooseBPSStaticModelRoute("", "gpt-5.5", "max")
	if err != nil || legacy.ResolvedModel != "gpt-5.6-sol" || legacy.ResolvedEffort != "xhigh" {
		t.Fatalf("legacy empty settings did not use static defaults: %+v %v", legacy, err)
	}
	rules := "# single-hop overrides\n gpt-6-sol = gpt-6-astra \n gpt-6-astra = gpt-6-sol \n codex-auto = gpt-5.6-luna\n *=gpt-5.6-terra\n"
	for _, tc := range []struct{ model, want, effort string }{{"gpt-6-sol", "gpt-6-astra", ""}, {"gpt-6-astra", "gpt-6-sol", ""}, {"gpt-5.6-luna", "gpt-5.6-terra", ""}, {"unknown", "gpt-5.6-terra", ""}, {"codex-auto", "gpt-5.6-luna", "xhigh"}} {
		route, err := chooseBPSStaticModelRoute(rules, tc.model, "")
		if err != nil || route.ResolvedModel != tc.want || route.ResolvedEffort != tc.effort || route.CatalogChecked {
			t.Fatalf("single final target failed: %+v %v", route, err)
		}
	}
}

func TestBPSStaticModelRoutesRejectInvalidSettings(t *testing.T) {
	for name, rules := range map[string]string{"empty": "", "comments": "# only\n\n", "no fallback": "gpt-6-sol=gpt-6-sol", "empty source": "=gpt-6-sol\n*=gpt-6-astra", "empty target": "fixture=\n*=gpt-6-astra", "unknown target": "*=gpt-7-future", "duplicate": "*=gpt-6-astra\n*=gpt-6-sol", "source whitespace": "two names=gpt-6-sol\n*=gpt-6-astra", "unicode whitespace": "two\u2003names=gpt-6-sol\n*=gpt-6-astra", "control": "bad\x00name=gpt-6-sol\n*=gpt-6-astra", "extra equals": "name=gpt-6-sol=other\n*=gpt-6-astra", "inline comments": "*=gpt-6-astra # comment"} {
		t.Run(name, func(t *testing.T) {
			if err := validateBPSModelRoutes(rules); err == nil {
				t.Fatal("invalid rules accepted")
			}
		})
	}
	if err := validateBPSModelRoutes("# comment\r\n*=gpt-6-astra\r\nname=gpt-6-sol\r\n"); err != nil {
		t.Fatal(err)
	}
}

func TestBPSStaticModelEffortCompatibility(t *testing.T) {
	for _, tc := range []struct{ model, effort, want string }{{"gpt-6-astra", "none", "low"}, {"gpt-6-astra", "minimal", "low"}, {"gpt-6-astra", "high", "high"}, {"gpt-6-sol", "none", "none"}, {"gpt-6-sol", "minimal", "low"}, {"gpt-6-sol", "max", "xhigh"}, {"gpt-5.6-luna", "ultra", "xhigh"}, {"gpt-6-astra", "", ""}} {
		route, err := chooseBPSStaticModelRoute(defaultBPSModelRoutes(), tc.model, tc.effort)
		if err != nil || route.ResolvedEffort != tc.want || route.RequestedEffort != tc.effort {
			t.Fatalf("effort failed %+v %v", route, err)
		}
	}
	for _, tc := range []struct{ model, effort, param string }{{"", "high", "model"}, {"bad name", "high", "model"}, {"gpt-6-astra", "future_unadvertised", "reasoning.effort"}} {
		_, err := chooseBPSStaticModelRoute(defaultBPSModelRoutes(), tc.model, tc.effort)
		var failure *bpsModelRoutingError
		if !errors.As(err, &failure) || failure.StatusCode != 400 || failure.Param != tc.param {
			t.Fatalf("invalid request classification: %v", err)
		}
	}
	_, err := chooseBPSStaticModelRoute("PRIVATE_INVALID_RULES", "gpt-6-astra", "")
	if err == nil || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("invalid config accepted or leaked")
	}
}

func TestBPSStaticModelCatalogAndCacheRemainImmutableAcrossCallers(t *testing.T) {
	ids := bpsStaticModelIDs()
	ids[0] = "corrupted"
	if bpsStaticModelIDs()[0] != "gpt-5.6-luna" {
		t.Fatal("model ids share mutable backing storage")
	}
	model, _ := bpsStaticModel("gpt-6-astra")
	model.Efforts[0].Value = "corrupted"
	again, _ := bpsStaticModel("gpt-6-astra")
	if again.Efforts[0].Value != "low" {
		t.Fatal("efforts share mutable backing storage")
	}
	rulesA, rulesB := "*=gpt-6-astra", "*=gpt-6-sol"
	first, err := cachedBPSStaticModelRoutes(rulesA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cachedBPSStaticModelRoutes(rulesB); err != nil {
		t.Fatal(err)
	}
	if first["*"] != "gpt-6-astra" {
		t.Fatal("new settings mutated in-flight parsed map")
	}
	var group sync.WaitGroup
	for i := 0; i < 32; i++ {
		i := i
		group.Go(func() {
			rules, want := rulesA, "gpt-6-astra"
			if i%2 == 1 {
				rules, want = rulesB, "gpt-6-sol"
			}
			for n := 0; n < 20; n++ {
				route, err := chooseBPSStaticModelRoute(rules, "same-request", "high")
				if err != nil || route.ResolvedModel != want || route.CatalogChecked {
					t.Errorf("concurrent static route leaked settings: %+v %v", route, err)
					return
				}
			}
		})
	}
	group.Wait()
	bpsStaticRouteCache.Lock()
	entries := len(bpsStaticRouteCache.parsed)
	latest := bpsStaticRouteCache.rules
	bpsStaticRouteCache.Unlock()
	if entries != 1 || (latest != rulesA && latest != rulesB) {
		t.Fatal("single-version cache retained unexpected state")
	}
}
