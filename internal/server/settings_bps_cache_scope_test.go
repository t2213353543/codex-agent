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

func TestBPSCacheScopeSettingsLegacyAndExportRoundTrip(t *testing.T) {
	for _, raw := range []string{`{"version":2,"admin_api":true}`, `{"version":2,"admin_api":true,"bps_prompt_cache_scope":"client"}`, `{"version":2,"admin_api":true,"bps_prompt_cache_scope":"api_key"}`} {
		dir := t.TempDir()
		path := filepath.Join(dir, "settings.json")
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		settings, err := NewSettingsStore(path).Get()
		if err != nil || settings.BPSPromptCacheScope == "" {
			t.Fatalf("missing legacy default: %+v %v", settings, err)
		}
		bundle, err := readLegacyBundle(dir)
		if err != nil {
			t.Fatal(err)
		}
		imported, err := bundle.Settings.validatedSettings()
		if err != nil || imported != settings {
			t.Fatalf("legacy import changed scope: %+v %v", imported, err)
		}
		// exportLegacyBundle serializes settings.state() into settings.json.
		exported, err := json.Marshal(imported.state())
		if err != nil {
			t.Fatal(err)
		}
		var state settingsState
		if err := json.Unmarshal(exported, &state); err != nil {
			t.Fatal(err)
		}
		roundTrip, err := state.validatedSettings()
		if err != nil || roundTrip != settings || state.BPSPromptCacheScope == nil {
			t.Fatalf("exported scope was omitted or changed: %+v %v", roundTrip, err)
		}
	}
	store := NewSettingsStore(filepath.Join(t.TempDir(), "settings.json"))
	if err := store.Set(Settings{AdminAPI: true}); err != nil {
		t.Fatal("legacy in-process settings rejected:", err)
	}
	if settings, err := store.Get(); err != nil || settings.BPSPromptCacheScope != bpsPromptCacheScopeClient {
		t.Fatalf("legacy in-process cache scope changed: %+v %v", settings, err)
	}
}

func TestBPSCacheScopeSettingsPartialUpdatePersistenceAndValidation(t *testing.T) {
	p, handler := webTestSetup(t)
	verifyBPSCacheScopeSettingsUpdate(t, p, handler)
}

func TestPGBPSCacheScopeSettingsPartialUpdatePersistenceAndValidation(t *testing.T) {
	p, handler := adminPGTestProxy(t)
	verifyBPSCacheScopeSettingsUpdate(t, p, handler)
}

func verifyBPSCacheScopeSettingsUpdate(t *testing.T, p *Proxy, handler http.Handler) {
	t.Helper()
	reload := func() (Settings, error) {
		if p.Store.DB != nil {
			return NewPGSettingsStore(p.Store.DB).Get()
		}
		return NewSettingsStore(p.Settings.store.path).Get()
	}
	initial, _ := p.Settings.Get()
	if initial.BPSPromptCacheScope != bpsPromptCacheScopeClient {
		t.Fatal("default scope changed")
	}
	patch := fmt.Sprintf(`{"settings_version":%d,"bps_prompt_cache_scope":"api_key"}`, initial.Version)
	w := webTestRequest(handler, http.MethodPost, "/api/settings", patch, "Bearer "+webTestKey)
	if w.Code != http.StatusOK {
		t.Fatalf("scope-only update failed: %d %s", w.Code, w.Body)
	}
	expected := initial
	expected.Version++
	expected.BPSPromptCacheScope = bpsPromptCacheScopeAPIKey
	stored, err := reload()
	if err != nil || stored != expected {
		t.Fatalf("scope-only update lost unrelated settings or persistence: %+v %v", stored, err)
	}
	w = webTestRequest(handler, http.MethodGet, "/api/settings", "", "Bearer "+webTestKey)
	var got Settings
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got != expected {
		t.Fatalf("settings GET omitted scope: %+v", got)
	}
	for _, test := range []struct {
		body string
		want int
	}{
		{patch, http.StatusConflict},
		{`{"bps_prompt_cache_scope":""}`, http.StatusBadRequest},
		{`{"bps_prompt_cache_scope":"global"}`, http.StatusBadRequest},
		{`{"bps_prompt_cache_scope":"API_KEY"}`, http.StatusBadRequest},
		{`{"bps_prompt_cache_scope":true}`, http.StatusBadRequest},
		{`{"bps_prompt_cache_scope":"api_key","regenerate":true}`, http.StatusBadRequest},
	} {
		w = webTestRequest(handler, http.MethodPost, "/api/settings", test.body, "Bearer "+webTestKey)
		if w.Code != test.want {
			t.Fatalf("invalid/stale scope update status=%d want=%d body=%s", w.Code, test.want, w.Body)
		}
	}
	if unchanged, err := p.Settings.Get(); err != nil || unchanged != expected {
		t.Fatal("rejected update changed current scope")
	}
	w = webTestRequest(handler, http.MethodPost, "/api/settings", `{"websocket_enabled":true}`, "Bearer "+webTestKey)
	got, err = p.Settings.Get()
	if w.Code != http.StatusOK || err != nil || got.BPSPromptCacheScope != bpsPromptCacheScopeAPIKey {
		t.Fatal("legacy partial client update reset shared cache scope")
	}
	w = webTestRequest(handler, http.MethodPost, "/api/settings", `{"bps_prompt_cache_scope":"client"}`, "Bearer "+webTestKey)
	got, err = reload()
	if w.Code != http.StatusOK || err != nil || got.BPSPromptCacheScope != bpsPromptCacheScopeClient {
		t.Fatal("cache scope could not be restored")
	}
}

func TestBPSCacheScopeRejectsInvalidPersistedPolicy(t *testing.T) {
	for _, value := range []string{"", "global", "API_KEY", " api_key "} {
		path := filepath.Join(t.TempDir(), "settings.json")
		raw, _ := json.Marshal(map[string]any{"version": 2, "bps_prompt_cache_scope": value})
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		store := NewSettingsStore(path)
		if _, err := store.Get(); !errors.Is(err, errInvalid) || store.loaded {
			t.Fatalf("invalid stored scope accepted or cached: %q %v", value, err)
		}
	}
}
