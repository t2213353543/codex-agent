package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBPSModelRoutesSettingsLegacyDefaultAndExport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"version":7,"admin_api":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	settings, err := NewSettingsStore(path).Get()
	if err != nil || settings.BPSModelRoutes == "" || settings.BPSModelRoutes != defaultBPSModelRoutes() || settings.RateLimitCooldownEnabled {
		t.Fatal("legacy default changed fixed routing or cooldown default")
	}
	state := settings.state()
	if state.BPSModelRoutes == nil || *state.BPSModelRoutes != settings.BPSModelRoutes {
		t.Fatal("export omitted exact routing configuration")
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var imported settingsState
	if json.Unmarshal(raw, &imported) != nil {
		t.Fatal("exported routing settings invalid")
	}
	if roundTrip, err := imported.validatedSettings(); err != nil || roundTrip != settings {
		t.Fatal("export/import changed routing or unrelated settings")
	}
	store := NewSettingsStore(filepath.Join(t.TempDir(), "settings.json"))
	if err := store.Set(Settings{AdminAPI: true}); err != nil {
		t.Fatal("legacy in-process caller rejected:", err)
	}
	if got, err := store.Get(); err != nil || got.BPSModelRoutes != defaultBPSModelRoutes() || got.RateLimitCooldownEnabled {
		t.Fatal("legacy in-process defaults changed")
	}
}

func TestBPSModelRoutesSettingsPartialUpdatePersistence(t *testing.T) {
	p, handler := webTestSetup(t)
	verifyBPSModelRoutesSettingsPersistence(t, p, handler)
}

func TestPGBPSModelRoutesSettingsPartialUpdatePersistence(t *testing.T) {
	p, handler := adminPGTestProxy(t)
	verifyBPSModelRoutesSettingsPersistence(t, p, handler)
}

func verifyBPSModelRoutesSettingsPersistence(t *testing.T, p *Proxy, handler http.Handler) {
	t.Helper()
	reload := func() (Settings, error) {
		if p.Store.DB != nil {
			return NewPGSettingsStore(p.Store.DB).Get()
		}
		return NewSettingsStore(p.Settings.store.path).Get()
	}
	initial, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	custom := "gpt-5.5=gpt-6-astra\ncodex-auto=gpt-5.6-sol\n*=gpt-6-astra"
	post := func(body any, want int) {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		response := webTestRequest(handler, http.MethodPost, "/api/settings", string(raw), "Bearer "+webTestKey)
		if response.Code != want {
			t.Fatalf("route settings status=%d want=%d body=%s", response.Code, want, response.Body)
		}
	}
	update := map[string]any{"settings_version": initial.Version, "bps_model_routes": custom}
	post(update, http.StatusOK)
	expected := initial
	expected.Version++
	expected.BPSModelRoutes = custom
	if got, err := reload(); err != nil || got != expected {
		t.Fatal("routing-only update changed unrelated settings or failed durable reload")
	}
	response := webTestRequest(handler, http.MethodGet, "/api/settings", "", "Bearer "+webTestKey)
	var got Settings
	var catalog struct {
		Targets []string `json:"bps_model_route_targets"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &got) != nil || json.Unmarshal(response.Body.Bytes(), &catalog) != nil || got != expected || !reflect.DeepEqual(catalog.Targets, bpsStaticModelIDs()) {
		t.Fatal("settings GET omitted exact routes or static target choices")
	}
	post(update, http.StatusConflict)
	for _, invalid := range []any{"", " \n\t", "source=unsupported-target", "same=gpt-6-astra\nsame=gpt-5.6-sol", true} {
		post(map[string]any{"bps_model_routes": invalid}, http.StatusBadRequest)
		if unchanged, err := reload(); err != nil || unchanged != expected {
			t.Fatal("invalid route update mutated saved settings")
		}
	}
	post(map[string]any{"bps_model_routes": custom, "regenerate": true}, http.StatusBadRequest)
	post(map[string]any{"websocket_enabled": !expected.WebsocketEnabled}, http.StatusOK)
	expected.Version++
	expected.WebsocketEnabled = !expected.WebsocketEnabled
	if got, err := reload(); err != nil || got != expected {
		t.Fatal("legacy partial update reset model routes")
	}
	post(map[string]any{"bps_model_routes": defaultBPSModelRoutes()}, http.StatusOK)
	expected.Version++
	expected.BPSModelRoutes = defaultBPSModelRoutes()
	if got, err := reload(); err != nil || got != expected || got.RateLimitCooldownEnabled != initial.RateLimitCooldownEnabled {
		t.Fatal("routing restore changed other policy")
	}
}

func TestBPSModelRoutesRejectsInvalidPersistedSettings(t *testing.T) {
	for _, value := range []string{"", " \n\t", "source=unsupported-target", "same=gpt-6-astra\nsame=gpt-5.6-sol"} {
		path := filepath.Join(t.TempDir(), "settings.json")
		raw, _ := json.Marshal(map[string]any{"version": 2, "bps_model_routes": value})
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		store := NewSettingsStore(path)
		if _, err := store.Get(); !errors.Is(err, errInvalid) || store.loaded {
			t.Fatal("invalid persisted routing defaulted silently or entered cache")
		}
	}
}

func TestBPSModelRoutesRuntimePolicyIncludesSettings(t *testing.T) {
	p, handler := webTestSetup(t)
	account := managedTestAccount(t, p, "model-route-runtime", nil)
	settings, _ := p.Settings.Get()
	settings.BPSModelRoutes = "codex-auto=gpt-5.6-sol\n*=gpt-6-astra"
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	response := managedTestRequest(t, handler, http.MethodGet, "/api/accounts/"+account.ID+"/runtime", nil, http.StatusOK)
	var got struct {
		Policy struct {
			Routes  string   `json:"bps_model_routes"`
			Targets []string `json:"bps_model_route_targets"`
		} `json:"effective_policy"`
	}
	if json.Unmarshal(response.Body.Bytes(), &got) != nil || got.Policy.Routes != settings.BPSModelRoutes || !reflect.DeepEqual(got.Policy.Targets, bpsStaticModelIDs()) {
		t.Fatal("account runtime omitted effective fixed model routes")
	}
}
