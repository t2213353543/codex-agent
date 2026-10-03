package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Fields and enum values are the fixed official 0.155 backend-client/OpenAPI
// contract, not guessed account-risk flags. This tests real GET -> parser ->
// management persistence -> admin JSON, independently of inference scheduling.
func TestResearchEntitlementsUsageSnapshotRetainsOfficialWorkspaceLimits(t *testing.T) {
	p, handler := webTestSetup(t)
	account := managedTestAccount(t, p, "entitlement-usage", nil)
	reset := time.Now().Add(time.Hour).Unix()
	const kind = "workspace_member_usage_limit_reached"
	payload := fmt.Sprintf(`{"plan_type":"self_serve_business_prolite","rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":42,"limit_window_seconds":18000,"reset_after_seconds":3600,"reset_at":%d}},"credits":{"has_credits":true,"unlimited":false,"balance":"9.99"},"spend_control":{"reached":true,"individual_limit":{"limit":"25000","used":"25000","remaining":"0","used_percent":100,"remaining_percent":0,"reset_after_seconds":3600,"reset_at":%d}},"rate_limit_reached_type":{"type":%q},"additional_rate_limits":[{"limit_name":"fixture-reserve","metered_feature":"codex_other","normal_model_slug":"gpt-5.6-luna","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%d}}}],"account_id":"private-upstream-account","user_id":"private-upstream-user"}`, reset, reset, kind, reset)
	var gets atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/backend-api/wham/usage" || r.Header.Get("X-OpenAI-Codex-Luna-Reserve") != "" {
			t.Error("passive usage check changed route or opted into Reserve")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, payload)
	}))
	defer upstream.Close()
	p.BaseURL = upstream.URL + "/backend-api/codex"
	managedTestRequest(t, handler, "POST", "/api/accounts/"+account.ID+"/upstream", map[string]any{"version": account.Version, "kind": "usage"}, 200)
	response := managedTestRequest(t, handler, "GET", "/api/accounts/"+account.ID+"/upstream", nil, 200)
	var saved struct {
		Snapshots map[string]struct {
			OK   bool                       `json:"ok"`
			Data map[string]json.RawMessage `json:"data"`
		} `json:"snapshots"`
	}
	if json.Unmarshal(response.Body.Bytes(), &saved) != nil || !saved.Snapshots["usage"].OK || gets.Load() != 1 {
		t.Fatal("fixture failed to persist one official usage read")
	}
	data := saved.Snapshots["usage"].Data
	for _, field := range []string{"spend_control", "rate_limit_reached_type"} {
		if len(data[field]) == 0 || string(data[field]) == "null" {
			t.Errorf("official workspace state disappeared from management snapshot: %s", field)
		}
	}
	if !strings.Contains(string(data["additional_rate_limits"]), `"normal_model_slug":"gpt-5.6-luna"`) {
		t.Error("additional quota lost official normal-model metadata")
	}
	if !strings.Contains(string(data["rate_limit"]), `"allowed":false`) || !strings.Contains(string(data["rate_limit"]), `"limit_reached":true`) || !strings.Contains(string(data["credits"]), `"balance":"9.99"`) {
		t.Error("existing ordinary limits or credits changed")
	}
	if strings.Contains(response.Body.String(), "private-upstream-") {
		t.Error("upstream account/user IDs escaped the management allowlist")
	}
	if p.cachedManagedUsage(t.Context(), account, true) == nil {
		t.Error("fresh official workspace snapshot did not round-trip through cache")
	}
	if p.checkGatewayCooldown(withGatewayCooldownModel(t.Context(), "gpt-5.6-sol"), account) != nil || p.inFlight(account.ID) != 0 {
		t.Error("management entitlement observation changed inference admission/cooldown")
	}
}

func TestResearchEntitlementsObservedAvailabilityFieldsRemainTypedAndPrivate(t *testing.T) {
	const payload = `{"plan_type":"self_serve_business_prolite","credits":{"has_credits":false,"unlimited":false,"overage_limit_reached":false,"balance":null,"private_extra":"private-secret"},"spend_control":{"reached":false,"individual_limit":null,"private_extra":"private-secret"},"rate_limit_reached_type":{"type":"workspace_member_credits_depleted","details":{"email":"private-email"}},"rate_limit_reset_credits":{"available_count":0,"applicable_available_count":0,"private_extra":"private-secret"},"model_usage":{"gpt-6-astra":{"available":false,"credits_would_enable":true,"available_at":"2030-01-02T03:04:05.123Z","private_extra":"private-secret"},"private@email":{"available":true}},"account_id":"private-id","user_id":"private-user","email":"private-email","rate_limit_upsell":{"title":"private-secret"}}`
	value, err := parseManagedUsage([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"overage_limit_reached":false`, `"reached":false`, `"applicable_available_count":0`, `"available":false`, `"credits_would_enable":true`, `"available_at":"2030-01-02T03:04:05.123Z"`, `"type":"workspace_member_credits_depleted"`} {
		if !strings.Contains(string(raw), expected) {
			t.Errorf("observed state lost: %s", expected)
		}
	}
	for _, private := range []string{"private-secret", "private-id", "private-user", "private-email", "private@email", `"details"`, `"rate_limit_upsell"`} {
		if strings.Contains(string(raw), private) {
			t.Errorf("unknown private field retained: %s", private)
		}
	}
	for _, payload := range []string{
		`{"plan_type":"team"}`,
		`{"plan_type":"team","spend_control":null,"rate_limit_reached_type":null,"model_usage":null}`,
		`{"plan_type":"team","spend_control":{"individual_limit":{}},"model_usage":{"gpt-6-astra":{}}}`,
	} {
		parsed, err := parseManagedUsage([]byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		usage := parsed.(managedUsage)
		if usage.SpendControl != nil && usage.SpendControl.Reached != nil {
			t.Fatal("missing reached became false")
		}
		if value, exists := usage.ModelUsage["gpt-6-astra"]; exists && (value.Available != nil || value.CreditsWouldEnable != nil || value.AvailableAt != nil) {
			t.Fatal("missing model permission became a decision")
		}
	}
	parsed, err := parseManagedUsage([]byte(`{"plan_type":"team","rate_limit_reached_type":{"type":"private_future_reason"}}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(parsed)
	if strings.Contains(string(raw), "private_future_reason") || !strings.Contains(string(raw), `"type":"unknown"`) {
		t.Fatal("unknown reason was treated as a known entitlement")
	}
	parsed, err = parseManagedUsage([]byte(`{"plan_type":"team","model_usage":{"gpt-6-astra":{"available":false,"credits_would_enable":true,"available_at":"private-future-time-format"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	availability := parsed.(managedUsage).ModelUsage["gpt-6-astra"]
	if availability.AvailableAt != nil || availability.Available == nil || *availability.Available || availability.CreditsWouldEnable == nil || !*availability.CreditsWouldEnable {
		t.Fatal("unknown optional time fabricated availability or discarded observed bool")
	}
}

func TestResearchEntitlementsCacheExpiresAtKnownNewStateBoundaries(t *testing.T) {
	p := newTestProxy(t, 1)
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for _, scenario := range []struct {
		name, extra string
		cached      bool
	}{
		{"spend_expired_at", fmt.Sprintf(`,"spend_control":{"reached":true,"individual_limit":{"reset_at":%d}}`, now-1), false},
		{"spend_expired_relative", `,"spend_control":{"reached":true,"individual_limit":{"reset_after_seconds":0}}`, false},
		{"spend_future", fmt.Sprintf(`,"spend_control":{"reached":true,"individual_limit":{"reset_at":%d}}`, now+3600), true},
		{"spend_unknown", `,"spend_control":{"reached":true,"individual_limit":{}}`, true},
		{"model_expired", fmt.Sprintf(`,"model_usage":{"gpt-6-astra":{"available":false,"available_at":%q}}`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)), false},
		{"model_future", fmt.Sprintf(`,"model_usage":{"gpt-6-astra":{"available":false,"available_at":%q}}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)), true},
		{"model_unknown", `,"model_usage":{"gpt-6-astra":{"available":false,"available_at":null}}`, true},
		{"model_unparsed_time", `,"model_usage":{"gpt-6-astra":{"available":false,"available_at":"unknown"}}`, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			parsed, err := parseManagedUsage([]byte(`{"plan_type":"team","rate_limit":{"primary_window":{"reset_at":` + fmt.Sprint(now+3600) + `}}` + scenario.extra + `}`))
			if err != nil {
				t.Fatal(err)
			}
			snapshot := managedUpstreamSnapshot{ID: account.ID, Version: account.Version, CredentialVersion: account.CredentialVersion, Kind: "usage", Source: "codex_usage", CheckedAt: now, OK: true, Data: parsed}
			if err := p.cacheManagedUpstream(t.Context(), snapshot); err != nil {
				t.Fatal(err)
			}
			if got := p.cachedManagedUsage(t.Context(), account, true) != nil; got != scenario.cached {
				t.Errorf("new entitlement boundary cache=%t want=%t", got, scenario.cached)
			}
		})
	}
}

func TestResearchEntitlementsOptionalAmountsDoNotEraseKnownLimits(t *testing.T) {
	for _, amount := range []string{"-1.5", "1e+9", "9007199254740993", "0.0001"} {
		raw := fmt.Sprintf(`{"plan_type":"team","rate_limit":{"allowed":false},"spend_control":{"reached":true,"individual_limit":{"remaining":%q}}}`, amount)
		parsed, err := parseManagedUsage([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		usage := parsed.(managedUsage)
		if usage.SpendControl.IndividualLimit.Remaining == nil || *usage.SpendControl.IndividualLimit.Remaining != amount {
			t.Error("official numeric string spelling not retained")
		}
	}
	for _, unknown := range []string{"unlimited_future_format", "private-email@example.test", "[]", " 5 ", strings.Repeat("9", 129)} {
		raw := fmt.Sprintf(`{"plan_type":"team","rate_limit":{"allowed":false,"limit_reached":true},"spend_control":{"reached":true,"individual_limit":{"limit":"100","remaining":%q,"used":"101"}}}`, unknown)
		parsed, err := parseManagedUsage([]byte(raw))
		if err != nil {
			t.Fatal("unknown optional amount invalidated otherwise-known official quota")
		}
		usage := parsed.(managedUsage)
		if usage.SpendControl.IndividualLimit.Remaining != nil || usage.SpendControl.IndividualLimit.Limit == nil || usage.RateLimit.Allowed == nil || *usage.RateLimit.Allowed {
			t.Error("unknown amount fabricated availability or lost known limit")
		}
	}
}

func TestResearchEntitlementsNewOptionalTypesDoNotInvalidateCoreQuota(t *testing.T) {
	for _, extra := range []string{
		`,"spend_control":{"reached":true,"individual_limit":{"remaining":123,"used":"101"}}`,
		`,"spend_control":{"reached":"future","individual_limit":[]}`,
		`,"spend_control":"future"`,
		`,"rate_limit_reached_type":{"type":123}`,
		`,"model_usage":[]`,
		`,"model_usage":{"gpt-6-astra":{"available":false,"credits_would_enable":true,"available_at":{}}}`,
		`,"credits":{"has_credits":false,"unlimited":false,"balance":"9.99","overage_limit_reached":"future"}`,
		`,"rate_limit_reset_credits":{"available_count":0,"applicable_available_count":[]}`,
		`,"additional_rate_limits":[{"limit_name":"fixture","metered_feature":"fixture","rate_limit":{"allowed":false},"normal_model_slug":{}}]`,
	} {
		parsed, err := parseManagedUsage([]byte(`{"plan_type":"team","rate_limit":{"allowed":false,"limit_reached":true}` + extra + `}`))
		if err != nil {
			t.Errorf("new optional type discarded valid core usage: %s: %v", extra, err)
			continue
		}
		usage := parsed.(managedUsage)
		if usage.RateLimit == nil || usage.RateLimit.Allowed == nil || *usage.RateLimit.Allowed || usage.RateLimit.LimitReached == nil || !*usage.RateLimit.LimitReached {
			t.Error("core usage changed")
		}
	}
	for _, raw := range []string{
		`{"plan_type":"team","rate_limit":{"allowed":"bad"}}`,
		`{"plan_type":"team","credits":{"has_credits":"bad"}}`,
		`{"plan_type":"team","rate_limit_reset_credits":{"available_count":"bad"}}`,
		`{"plan_type":"team","additional_rate_limits":[{"rate_limit":{"allowed":"bad"}}]}`,
	} {
		if _, err := parseManagedUsage([]byte(raw)); err == nil {
			t.Error("existing core quota type validation was relaxed")
		}
	}
}
