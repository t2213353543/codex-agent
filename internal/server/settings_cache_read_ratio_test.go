package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
)

func TestCacheReadRatioLegacyDefaultPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	store := NewSettingsStore(path)
	settings, err := store.Get()
	if err != nil || settings.CacheReadRatioEnabled || settings.CacheReadRatioPercent != 0 {
		t.Fatal("ratio projection must default to disabled and zero")
	}
	settings.CacheReadRatioEnabled, settings.CacheReadRatioPercent = true, 73
	if err := store.Set(settings); err != nil {
		t.Fatal(err)
	}
	got, err := NewSettingsStore(path).Get()
	if err != nil || !got.CacheReadRatioEnabled || got.CacheReadRatioPercent != 73 {
		t.Fatal("ratio lost on restart")
	}
	got.CacheReadRatioEnabled = false
	if err := store.Set(got); err != nil {
		t.Fatal(err)
	}
	got, err = NewSettingsStore(path).Get()
	if err != nil || got.CacheReadRatioEnabled || got.CacheReadRatioPercent != 73 {
		t.Fatal("disabling lost preset")
	}
}

func verifyCacheReadRatioSettings(t *testing.T, p *Proxy, handler http.Handler) {
	t.Helper()
	initial, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	post := func(body string, want int) {
		t.Helper()
		response := webTestRequest(handler, http.MethodPost, "/api/settings", body, "Bearer "+webTestKey)
		if response.Code != want {
			t.Fatalf("HTTP %d want %d: %s", response.Code, want, response.Body)
		}
	}
	patch := fmt.Sprintf(`{"settings_version":%d,"cache_read_ratio_enabled":true,"cache_read_ratio_percent":100}`, initial.Version)
	post(patch, 200)
	expected := initial
	expected.Version++
	expected.CacheReadRatioEnabled, expected.CacheReadRatioPercent = true, 100
	if got, err := p.Settings.Get(); err != nil || got != expected {
		t.Fatal("ratio update changed unrelated settings")
	}
	post(patch, 409)
	for _, value := range []string{"-1", "101", "1.5", `"50"`, "null", "true"} {
		post(`{"cache_read_ratio_percent":`+value+`}`, 400)
	}
	for _, value := range []string{"0", `"true"`, "null"} {
		post(`{"cache_read_ratio_enabled":`+value+`}`, 400)
	}
	post(`{"cache_read_ratio_enabled":true,"regenerate":true}`, 400)
	post(`{"cache_read_ratio_enabled":false}`, 200)
	expected.Version++
	expected.CacheReadRatioEnabled = false
	if got, err := p.Settings.Get(); err != nil || got != expected {
		t.Fatal("invalid update persisted or disable lost ratio")
	}
	response := webTestRequest(handler, http.MethodGet, "/api/settings", "", "Bearer "+webTestKey)
	var body map[string]json.RawMessage
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &body) != nil || string(body["cache_read_ratio_enabled"]) != "false" || string(body["cache_read_ratio_percent"]) != "100" {
		t.Fatal("GET omitted ratio configuration")
	}
	post(`{"cache_read_ratio_percent":0}`, 200)
	if got, _ := p.Settings.Get(); got.CacheReadRatioPercent != 0 {
		t.Fatal("zero percent did not persist")
	}
}

func TestCacheReadRatioSettingsAPI(t *testing.T) {
	p, h := webTestSetup(t)
	verifyCacheReadRatioSettings(t, p, h)
}
func TestPGCacheReadRatioSettingsAPI(t *testing.T) {
	p, h := adminPGTestProxy(t)
	verifyCacheReadRatioSettings(t, p, h)
}
