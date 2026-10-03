package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestInferenceModeLegacySettingsDefaultAndRejectUnknown(t *testing.T) {
	var legacy settingsState
	if err := json.Unmarshal([]byte(`{"version":4,"admin_api":true}`), &legacy); err != nil {
		t.Fatal(err)
	}
	settings, err := legacy.validatedSettings()
	if err != nil || settings.InferenceMode != inferencePowerPoint {
		t.Fatalf("legacy mode: %q %v", settings.InferenceMode, err)
	}
	invalid := "unrecognized"
	legacy.InferenceMode = &invalid
	if _, err := legacy.validatedSettings(); err == nil {
		t.Fatal("invalid persisted mode was accepted")
	}
	store := NewSettingsStore(filepath.Join(t.TempDir(), "settings.json"))
	for _, mode := range []string{inferenceCodexCLI, inferenceAppServer, inferencePowerPoint} {
		current, err := store.Get()
		if err != nil {
			t.Fatal(err)
		}
		current.InferenceMode = mode
		if err := store.Set(current); err != nil {
			t.Fatal(err)
		}
		reopened, err := NewSettingsStore(store.store.path).Get()
		if err != nil || reopened.InferenceMode != mode {
			t.Fatalf("mode did not persist: %q %v", reopened.InferenceMode, err)
		}
	}
}

func TestInferenceModeRequestContextDoesNotMutatePowerPointRuntime(t *testing.T) {
	p := &Proxy{BPS: &bpsRuntime{}, BaseURL: "https://bps.example.test/api"}
	bps := httptest.NewRequest(http.MethodPost, p.BaseURL+"/responses", strings.NewReader(`{}`))
	if !p.isBPSUpstreamRequest(bps) {
		t.Fatal("BPS request not recognized")
	}
	cli := bps.WithContext(withInferenceMode(context.Background(), inferenceCodexCLI))
	if p.isBPSUpstreamRequest(cli) || p.gatewayInferenceBaseURL(cli.Context()) != codexURL {
		t.Fatal("official request retained BPS route")
	}
	if !p.isBPSUpstreamRequest(bps) || p.BaseURL != "https://bps.example.test/api" {
		t.Fatal("request selection changed shared runtime")
	}
	usage := httptest.NewRequest(http.MethodGet, codexURL+"/../wham/usage", nil)
	if p.isBPSUpstreamRequest(usage) {
		t.Fatal("account usage request gained BPS identity")
	}
}

func TestInferenceModeManagementCASAndSessionOwnership(t *testing.T) {
	p, handler := webTestSetup(t)
	p.BPS = &bpsRuntime{ToolsVersion: "tools-powerpoint-fixture"}
	p.BaseURL = "https://bps.example.test/api"
	p.RuntimeModes = true
	read := func() map[string]json.RawMessage {
		out := webTestRequest(handler, "GET", "/api/settings", "", "Bearer "+webTestKey)
		if out.Code != 200 {
			t.Fatal(out.Code)
		}
		var value map[string]json.RawMessage
		_ = json.Unmarshal(out.Body.Bytes(), &value)
		return value
	}
	initial := read()
	version := string(initial["settings_version"])
	removed := webTestRequest(handler, "POST", "/api/settings", `{"settings_version":`+version+`,"inference_mode":"official_app_server"}`, "Bearer "+webTestKey)
	if removed.Code != 400 || strings.Contains(string(initial["inference_modes"]), "official_app_server") {
		t.Fatal("removed runtime still offered")
	}
	update := webTestRequest(handler, "POST", "/api/settings", `{"settings_version":`+version+`,"inference_mode":"official_codex_cli"}`, "Bearer "+webTestKey)
	if update.Code != 200 {
		t.Fatalf("mode save: %d %s", update.Code, update.Body.String())
	}
	current := read()
	if string(current["inference_mode"]) != `"official_codex_cli"` || !strings.Contains(string(current["inference"]), `"backend":"official_codex_cli"`) {
		t.Fatal("saved mode not effective")
	}
	stale := webTestRequest(handler, "POST", "/api/settings", `{"settings_version":`+version+`,"inference_mode":"official_codex_cli"}`, "Bearer "+webTestKey)
	if stale.Code != 409 {
		t.Fatal("stale mode write accepted")
	}
	invalid := webTestRequest(handler, "POST", "/api/settings", `{"settings_version":`+string(current["settings_version"])+`,"inference_mode":"unknown"}`, "Bearer "+webTestKey)
	if invalid.Code != 400 {
		t.Fatal("unknown backend accepted")
	}
	ctx := withInferenceMode(context.Background(), inferencePowerPoint)
	if err := p.bindInferenceSession(ctx, "key", "thread"); err != nil {
		t.Fatal(err)
	}
	if err := p.bindInferenceSession(withInferenceMode(ctx, inferenceAppServer), "key", "thread"); err != errInferenceSessionChanged {
		t.Fatal("provider changed for bound conversation")
	}
	if err := p.bindInferenceSession(withInferenceMode(ctx, inferenceAppServer), "other-key", "thread"); err != nil {
		t.Fatal("independent key inherited another binding")
	}
}
