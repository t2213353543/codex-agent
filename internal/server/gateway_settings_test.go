package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGatewaySettingsMigrationAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"version":4,"admin_api":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	s := NewSettingsStore(path)
	initial, err := s.Get()
	if err != nil {
		t.Fatal(err)
	}
	if initial.AdminAPI || initial.Version != 4 || initial.WebsocketEnabled || !initial.BuiltinPromptEnabled || initial.BuiltinPrompt == "" || !initial.StickySessionsEnabled || !initial.PromptCacheKeyEnabled || initial.StickySessionTTLSeconds != 604800 {
		t.Fatalf("migration defaults: %+v", initial)
	}
	initial.BuiltinPromptEnabled, initial.StickySessionsEnabled, initial.PromptCacheKeyEnabled = false, false, false
	initial.BuiltinPrompt = ""
	initial.WebsocketEnabled = true
	initial.StickySessionTTLSeconds = 120
	if err := s.Set(initial); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewSettingsStore(path).Get()
	if err != nil {
		t.Fatal(err)
	}
	initial.Version++
	if reloaded != initial {
		t.Fatalf("explicit false/empty values lost: %+v", reloaded)
	}
}

func TestGatewaySettingsPartialUpdateValidationAndConflict(t *testing.T) {
	p, handler := webTestSetup(t)
	initial, _ := p.Settings.Get()
	post := func(body string, want int) {
		t.Helper()
		w := webTestRequest(handler, http.MethodPost, "/api/settings", body, "Bearer "+webTestKey)
		if w.Code != want {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	body := fmt.Sprintf(`{"settings_version":%d,"websocket_enabled":true,"builtin_prompt":"稳定提示词","sticky_session_ttl_seconds":120}`, initial.Version)
	post(body, 200)
	updated, _ := p.Settings.Get()
	if !updated.AdminAPI || !updated.WebsocketEnabled || !updated.BuiltinPromptEnabled || !updated.StickySessionsEnabled || updated.BuiltinPrompt != "稳定提示词" || updated.StickySessionTTLSeconds != 120 {
		t.Fatalf("partial update replaced omitted fields: %+v", updated)
	}
	post(body, 409)
	for _, invalid := range []string{`{}`, `{"sticky_session_ttl_seconds":0}`, `{"sticky_session_ttl_seconds":2592001}`, `{"websocket_enabled":"yes"}`, `{"settings_version":-1,"websocket_enabled":false}`, `{"regenerate":true,"websocket_enabled":true}`} {
		post(invalid, 400)
	}
	oversized, _ := json.Marshal(map[string]any{"builtin_prompt": strings.Repeat("中", maxBuiltinPromptBytes/3+1)})
	post(string(oversized), 400)
	unchanged, _ := p.Settings.Get()
	if unchanged != updated {
		t.Fatal("failed update changed persisted settings")
	}
	w := webTestRequest(handler, http.MethodGet, "/api/settings", "", "Bearer "+webTestKey)
	var response Settings
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response != updated {
		t.Fatalf("GET omitted policy settings: %+v", response)
	}
}

func TestPGGatewaySettingsRefreshAcrossInstances(t *testing.T) {
	store := accountPGTestStore(t)
	first, second := NewPGSettingsStore(store.DB), NewPGSettingsStore(store.DB)
	if err := first.Set(defaultSettings()); err != nil {
		t.Fatal(err)
	}
	before, err := second.Get()
	if err != nil {
		t.Fatal(err)
	}
	changed, _ := first.Get()
	changed.WebsocketEnabled = true
	changed.BuiltinPrompt = "updated on another gateway"
	if err := first.Set(changed); err != nil {
		t.Fatal(err)
	}
	// Expire the read cache without making the test sleep.
	second.mu.Lock()
	second.loadedAt = time.Now().Add(-settingsCacheTTL)
	second.mu.Unlock()
	after, err := second.Get()
	if err != nil || !after.WebsocketEnabled || after.Version != before.Version+1 || after.BuiltinPrompt != changed.BuiltinPrompt {
		t.Fatalf("stale cross-process settings: %+v %v", after, err)
	}
	if err := second.Set(before); err == nil {
		t.Fatal("stale process overwrote settings")
	}
}
