package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func assertCustomizationDefaults(t *testing.T, settings Settings) {
	t.Helper()
	if !settings.HTTPStreamEnabled || !settings.HTTPJSONEnabled || !settings.CompactEnabled || !settings.ModelsEnabled || !settings.CompatibilityNormalizationEnabled || !settings.AutomaticTokenRefreshEnabled || !settings.Upstream401RecoveryEnabled {
		t.Fatalf("legacy document disabled an existing gateway feature: %+v", settings)
	}
	if settings.BuiltinPromptMode != "prepend" || settings.UpstreamStreamIdleTimeoutSeconds != 300 || settings.UpstreamResponseHeaderTimeoutSeconds != 90 || settings.UpstreamCompactTimeoutSeconds != 1200 {
		t.Fatalf("legacy document changed existing prompt or timeout behavior: %+v", settings)
	}
}

func TestSettingsCustomizationLegacyDefaultsAndExplicitFalse(t *testing.T) {
	for _, legacy := range []string{"", `{"version":3,"admin_api":true}`, `{"version":7,"admin_api":false,"builtin_prompt":"","builtin_prompt_enabled":false,"prompt_cache_key_enabled":false}`} {
		t.Run(fmt.Sprintf("legacy_%d", len(legacy)), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			if legacy != "" {
				if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
					t.Fatal(err)
				}
			}
			store := NewSettingsStore(path)
			settings, err := store.Get()
			if err != nil {
				t.Fatal(err)
			}
			assertCustomizationDefaults(t, settings)
			settings.HTTPStreamEnabled, settings.HTTPJSONEnabled = false, false
			settings.CompactEnabled, settings.ModelsEnabled = false, false
			settings.CompatibilityNormalizationEnabled, settings.AutomaticTokenRefreshEnabled, settings.Upstream401RecoveryEnabled = false, false, false
			settings.BuiltinPromptMode = "if_empty"
			settings.UpstreamStreamIdleTimeoutSeconds = 47
			settings.UpstreamResponseHeaderTimeoutSeconds = 13
			settings.UpstreamCompactTimeoutSeconds = 195
			if err := store.Set(settings); err != nil {
				t.Fatal(err)
			}
			settings.Version++
			got, err := NewSettingsStore(path).Get()
			if err != nil || got != settings {
				t.Fatalf("explicit values were lost after reopening store: got %+v, expected %+v, err %v", got, settings, err)
			}
		})
	}
}

func TestSettingsCustomizationLegacyInProcessDefaults(t *testing.T) {
	store := NewSettingsStore(filepath.Join(t.TempDir(), "settings.json"))
	if err := store.Set(Settings{AdminAPI: true}); err != nil {
		t.Fatal("legacy caller must not fail new timeout validation:", err)
	}
	got, err := store.Get()
	if err != nil || got.BuiltinPromptMode != "prepend" || got.UpstreamStreamIdleTimeoutSeconds != 300 || got.UpstreamResponseHeaderTimeoutSeconds != 90 || got.UpstreamCompactTimeoutSeconds != 1200 {
		t.Fatalf("legacy in-process numeric defaults: %+v %v", got, err)
	}
}

func TestSettingsCustomizationAdminRoundTrip(t *testing.T) {
	p, handler := webTestSetup(t)
	verifyCustomizationAdminRoundTrip(t, p, handler)
}

func verifyCustomizationAdminRoundTrip(t *testing.T, p *Proxy, handler http.Handler) {
	t.Helper()
	before, err := p.Settings.Latest()
	if err != nil {
		t.Fatal(err)
	}
	assertCustomizationDefaults(t, before)
	changed := before
	changed.HTTPStreamEnabled, changed.HTTPJSONEnabled = false, false
	changed.CompactEnabled, changed.ModelsEnabled = false, false
	changed.CompatibilityNormalizationEnabled, changed.AutomaticTokenRefreshEnabled, changed.Upstream401RecoveryEnabled = false, false, false
	changed.BuiltinPromptMode = "append"
	changed.UpstreamStreamIdleTimeoutSeconds = 42
	changed.UpstreamResponseHeaderTimeoutSeconds = 21
	changed.UpstreamCompactTimeoutSeconds = 125
	body, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	w := webTestRequest(handler, http.MethodPost, "/api/settings", string(body), "Bearer "+webTestKey)
	if w.Code != http.StatusOK {
		t.Fatalf("complete settings update: %d %s", w.Code, w.Body.String())
	}
	changed.Version++
	w = webTestRequest(handler, http.MethodGet, "/api/settings", "", "Bearer "+webTestKey)
	var got Settings
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK || got != changed {
		t.Fatalf("GET did not round-trip all settings: %+v, expected %+v, status %d, err %v", got, changed, w.Code, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	var expectedFields map[string]json.RawMessage
	if err := json.Unmarshal(body, &expectedFields); err != nil {
		t.Fatal(err)
	}
	for field := range expectedFields {
		if _, present := fields[field]; !present {
			t.Errorf("settings GET omitted field %q, including disabled switches", field)
		}
	}
	if string(fields["admin_key"]) != `""` || fields["admin_key_masked"] == nil || fields["codex_fingerprint_profiles"] == nil {
		t.Fatal("settings extension changed masked admin key or fingerprint profile discovery")
	}
	// Updating one switch must preserve every other value, including false.
	partial := fmt.Sprintf(`{"settings_version":%d,"models_enabled":true}`, changed.Version)
	w = webTestRequest(handler, http.MethodPost, "/api/settings", partial, "Bearer "+webTestKey)
	if w.Code != http.StatusOK {
		t.Fatalf("partial update: %d %s", w.Code, w.Body.String())
	}
	changed.ModelsEnabled = true
	changed.Version++
	got, err = p.Settings.Latest()
	if err != nil || got != changed {
		t.Fatalf("partial update changed omitted settings: %+v %v", got, err)
	}
	w = webTestRequest(handler, http.MethodPost, "/api/settings", partial, "Bearer "+webTestKey)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale settings update overwrote newer settings: %d %s", w.Code, w.Body.String())
	}
	if got, err = p.Settings.Latest(); err != nil || got != changed {
		t.Fatalf("conflicting request mutated settings: %+v %v", got, err)
	}
}

func TestSettingsCustomizationAdminValidation(t *testing.T) {
	p, handler := webTestSetup(t)
	before, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	invalid := []string{
		`{"builtin_prompt_mode":""}`, `{"builtin_prompt_mode":"replace"}`, `{"builtin_prompt_mode":false}`,
		`{"upstream_stream_idle_timeout_seconds":0}`, `{"upstream_stream_idle_timeout_seconds":9}`, `{"upstream_stream_idle_timeout_seconds":3601}`,
		`{"upstream_response_header_timeout_seconds":0}`, `{"upstream_response_header_timeout_seconds":4}`, `{"upstream_response_header_timeout_seconds":601}`,
		`{"upstream_compact_timeout_seconds":0}`, `{"upstream_compact_timeout_seconds":29}`, `{"upstream_compact_timeout_seconds":3601}`,
		`{"upstream_compact_timeout_seconds":30.5}`, `{"upstream_stream_idle_timeout_seconds":"300"}`,
		`{"http_stream_enabled":"true"}`, `{"http_json_enabled":1}`, `{"compact_enabled":"false"}`, `{"models_enabled":[]}`,
		`{"compatibility_normalization_enabled":{}}`, `{"automatic_token_refresh_enabled":1}`, `{"upstream_401_recovery_enabled":"yes"}`,
		`{"regenerate":true,"http_stream_enabled":false}`, `{"regenerate_key":true,"builtin_prompt_mode":"append"}`,
	}
	for _, body := range invalid {
		w := webTestRequest(handler, http.MethodPost, "/api/settings", body, "Bearer "+webTestKey)
		if w.Code != http.StatusBadRequest {
			t.Errorf("invalid setting %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if got, err := p.Settings.Get(); err != nil || got != before {
		t.Fatalf("invalid request mutated settings: %+v %v", got, err)
	}
	for _, body := range []string{
		`{"builtin_prompt_mode":"prepend","upstream_stream_idle_timeout_seconds":10,"upstream_response_header_timeout_seconds":5,"upstream_compact_timeout_seconds":30}`,
		`{"builtin_prompt_mode":"if_empty","upstream_stream_idle_timeout_seconds":3600,"upstream_response_header_timeout_seconds":600,"upstream_compact_timeout_seconds":3600}`,
	} {
		w := webTestRequest(handler, http.MethodPost, "/api/settings", body, "Bearer "+webTestKey)
		if w.Code != http.StatusOK {
			t.Fatalf("valid boundary rejected: %s: %d %s", body, w.Code, w.Body.String())
		}
	}
}

func TestPGSettingsCustomizationPersistenceAndAdminAPI(t *testing.T) {
	p, handler := adminPGTestProxy(t)
	verifyCustomizationAdminRoundTrip(t, p, handler)
	expected, err := p.Settings.Latest()
	if err != nil {
		t.Fatal(err)
	}
	independent := NewPGSettingsStore(p.Store.DB)
	got, err := independent.Get()
	if err != nil || got != expected {
		t.Fatalf("PostgreSQL omitted settings on independent reload: %+v %v", got, err)
	}
	stale := got
	got.HTTPJSONEnabled = true
	got.BuiltinPromptMode = "if_empty"
	if err := independent.Set(got); err != nil {
		t.Fatal(err)
	}
	if err := p.Settings.Set(stale); !errors.Is(err, errVersionConflict) {
		t.Fatalf("PostgreSQL lost settings version conflict: %v", err)
	}
	got.Version++
	if latest, err := p.Settings.Latest(); err != nil || latest != got {
		t.Fatalf("PostgreSQL management read retained stale settings: %+v %v", latest, err)
	}
}
