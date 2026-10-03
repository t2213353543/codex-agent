package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestBPSCacheCreationAsReadLegacyDefaultAndFalseRestart(t *testing.T) {
	if !defaultSettings().CacheCreationAsReadEnabled {
		t.Fatal("cache creation projection must default to enabled")
	}
	for _, tc := range []struct {
		name, document string
		enabled        bool
	}{
		{"new_store", "", true},
		{"legacy_absent", `{"version":4,"admin_api":true}`, true},
		{"explicit_false", `{"version":4,"admin_api":true,"cache_creation_as_read_enabled":false}`, false},
		{"explicit_true", `{"version":4,"admin_api":true,"cache_creation_as_read_enabled":true}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			if tc.document != "" {
				if err := os.WriteFile(path, []byte(tc.document), 0600); err != nil {
					t.Fatal(err)
				}
			}
			store := NewSettingsStore(path)
			got, err := store.Get()
			if err != nil || got.CacheCreationAsReadEnabled != tc.enabled {
				t.Fatalf("legacy/default value changed: %+v %v", got, err)
			}
			if err := store.Set(got); err != nil {
				t.Fatal(err)
			}
			reloaded, err := NewSettingsStore(path).Get()
			if err != nil || reloaded.CacheCreationAsReadEnabled != tc.enabled {
				t.Fatal("explicit value changed after durable restart")
			}
			wire, err := os.ReadFile(path)
			var state settingsState
			if err != nil || json.Unmarshal(wire, &state) != nil || state.CacheCreationAsReadEnabled == nil || *state.CacheCreationAsReadEnabled != tc.enabled {
				t.Fatal("persisted field was omitted or false collapsed into default")
			}
			var exported settingsState
			raw, _ := json.Marshal(reloaded.state())
			if json.Unmarshal(raw, &exported) != nil || exported.settings().CacheCreationAsReadEnabled != tc.enabled {
				t.Fatal("settings export roundtrip changed explicit value")
			}
		})
	}
}

func TestBPSCacheCreationAsReadSettingsPartialUpdateAndRuntime(t *testing.T) {
	p, handler := webTestSetup(t)
	verifyCacheCreationAsReadSettings(t, p, handler)
}

func TestPGBPSCacheCreationAsReadSettingsPartialUpdateAndRuntime(t *testing.T) {
	p, handler := adminPGTestProxy(t)
	verifyCacheCreationAsReadSettings(t, p, handler)
}

func verifyCacheCreationAsReadSettings(t *testing.T, p *Proxy, handler http.Handler) {
	t.Helper()
	newStore := func() *SettingsStore {
		if p.Store.DB != nil {
			return NewPGSettingsStore(p.Store.DB)
		}
		return NewSettingsStore(p.Settings.store.path)
	}
	// Persist a legacy document in the real file/PG store, rather than merely
	// unmarshalling a literal. The missing new field must become true on reload.
	if err := p.Settings.store.Modify(func(state *settingsState) error { state.CacheCreationAsReadEnabled = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	p.Settings = newStore()
	initial, err := p.Settings.Get()
	if err != nil || !initial.CacheCreationAsReadEnabled {
		t.Fatal("legacy persisted settings did not enable projection by default")
	}
	post := func(body string, want int) {
		t.Helper()
		response := webTestRequest(handler, http.MethodPost, "/api/settings", body, "Bearer "+webTestKey)
		if response.Code != want {
			t.Fatalf("settings update returned %d, want %d: %s", response.Code, want, response.Body)
		}
	}
	patch := fmt.Sprintf(`{"settings_version":%d,"cache_creation_as_read_enabled":false}`, initial.Version)
	post(patch, http.StatusOK)
	expected := initial
	expected.Version++
	expected.CacheCreationAsReadEnabled = false
	if got, err := newStore().Get(); err != nil || got != expected {
		t.Fatalf("setting-only update changed unrelated policy or lost false: %+v %v", got, err)
	}
	response := webTestRequest(handler, http.MethodGet, "/api/settings", "", "Bearer "+webTestKey)
	var observed map[string]json.RawMessage
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &observed) != nil || string(observed["cache_creation_as_read_enabled"]) != "false" {
		t.Fatal("settings GET must report measured false, not omit the field")
	}
	post(patch, http.StatusConflict)
	for _, value := range []string{`"false"`, `0`, `{}`, `[]`, `null`} {
		post(`{"cache_creation_as_read_enabled":`+value+`}`, http.StatusBadRequest)
	}
	post(`{"cache_creation_as_read_enabled":true,"regenerate":true}`, http.StatusBadRequest)
	if got, err := newStore().Get(); err != nil || got != expected {
		t.Fatal("rejected stale/invalid setting changed stored policy")
	}
	post(`{"websocket_enabled":true}`, http.StatusOK)
	expected.Version++
	expected.WebsocketEnabled = true
	if got, err := newStore().Get(); err != nil || got != expected {
		t.Fatal("unrelated partial update reset explicit false")
	}
	// An independent store models another process/restart, with no local cache.
	p.Settings = newStore()
	account := managedTestAccount(t, p, "cache-creation-policy", nil)
	checkRuntime := func(want bool) {
		t.Helper()
		response := managedTestRequest(t, handler, http.MethodGet, "/api/accounts/"+account.ID+"/runtime", nil, http.StatusOK)
		var result struct {
			Policy struct {
				Enabled *bool `json:"cache_creation_as_read_enabled"`
			} `json:"effective_policy"`
		}
		if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Policy.Enabled == nil || *result.Policy.Enabled != want {
			t.Fatal("account runtime omitted or changed effective cache policy")
		}
	}
	checkRuntime(false)
	post(fmt.Sprintf(`{"settings_version":%d,"cache_creation_as_read_enabled":true}`, expected.Version), http.StatusOK)
	expected.Version++
	expected.CacheCreationAsReadEnabled = true
	if got, err := newStore().Get(); err != nil || got != expected {
		t.Fatal("projection could not be re-enabled without changing other policy")
	}
	checkRuntime(true)
}
