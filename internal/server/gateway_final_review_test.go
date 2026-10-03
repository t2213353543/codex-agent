package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"testing"
)

// Exercise the authenticated management handler, rather than calling Set
// directly: a runtime read must reject damaged policy while the repair path
// must merge against the current durable document without publishing it.
func auditFinalSettingsRepair(t *testing.T, p *Proxy, handler http.Handler, warm bool) {
	t.Helper()
	initial := defaultSettings()
	initial.AdminAPI = true
	initial.BuiltinPrompt = "old cached prompt"
	if err := p.Settings.Set(initial); err != nil {
		t.Fatal(err)
	}
	cached, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	badTimeout, badMode := -1, "invalid persisted mode"
	durablePrompt := "preserve the current durable prompt"
	if err := p.Settings.store.Modify(func(state *settingsState) error {
		state.Version++
		state.AdminAPI = false
		state.BuiltinPrompt = &durablePrompt
		state.BuiltinPromptMode = &badMode
		state.UpstreamResponseHeaderTimeoutSeconds = &badTimeout
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := p.Settings.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !warm {
		p.Settings.mu.Lock()
		p.Settings.loaded = false
		p.Settings.mu.Unlock()
		if _, err := p.Settings.Get(); !errors.Is(err, errInvalid) {
			t.Fatalf("runtime accepted corrupt policy: %v", err)
		}
	}
	post := func(body, auth string, want int) {
		t.Helper()
		response := webTestRequest(handler, http.MethodPost, "/api/settings", body, auth)
		if response.Code != want {
			t.Fatalf("repair %s returned %d, want %d: %s", body, response.Code, want, response.Body.String())
		}
	}
	unchanged := func() {
		t.Helper()
		after, err := p.Settings.store.Read()
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("rejected update modified durable settings: %v", err)
		}
		if p.Settings.cached != cached {
			t.Fatal("repair read published unvalidated settings into runtime cache")
		}
	}
	repair := fmt.Sprintf(`{"settings_version":%d,"builtin_prompt_mode":"append","upstream_response_header_timeout_seconds":37}`, before.Version)
	post(repair, "Bearer incorrect", http.StatusUnauthorized)
	unchanged()
	post(fmt.Sprintf(`{"settings_version":%d,"upstream_response_header_timeout_seconds":37}`, before.Version), "Bearer "+webTestKey, http.StatusBadRequest)
	unchanged()
	post(fmt.Sprintf(`{"settings_version":%d,"builtin_prompt_mode":"append","upstream_response_header_timeout_seconds":37}`, before.Version-1), "Bearer "+webTestKey, http.StatusConflict)
	unchanged()
	post(repair, "Bearer "+webTestKey, http.StatusOK)
	current, err := p.Settings.Get()
	if err != nil || current.AdminAPI || current.BuiltinPrompt != durablePrompt || current.BuiltinPromptMode != "append" || current.UpstreamResponseHeaderTimeoutSeconds != 37 || current.Version != before.Version+1 {
		t.Fatalf("corrected durable policy or untouched fields lost: %+v, %v", current, err)
	}
	response := webTestRequest(handler, http.MethodGet, "/api/settings", "", "Bearer "+webTestKey)
	var result struct {
		Version int64 `json:"settings_version"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Version != current.Version {
		t.Fatalf("management GET did not recover after repair: %d %s", response.Code, response.Body.String())
	}
}

func TestAuditFinalSettingsRepairThroughFileHandler(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(fmt.Sprintf("warm_cache_%t", warm), func(t *testing.T) {
			p, handler := webTestSetup(t)
			auditFinalSettingsRepair(t, p, handler, warm)
		})
	}
}

func TestPGGatewayAuditFinalSettingsRepairThroughHandler(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(fmt.Sprintf("warm_cache_%t", warm), func(t *testing.T) {
			p, handler := adminPGTestProxy(t)
			auditFinalSettingsRepair(t, p, handler, warm)
		})
	}
}

func TestAuditFinalSettingsRepairCannotResetInvalidVersion(t *testing.T) {
	for _, version := range []int64{-1, math.MaxInt64} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			p, handler := webTestSetup(t)
			if err := p.Settings.Set(defaultSettings()); err != nil {
				t.Fatal(err)
			}
			if err := p.Settings.store.Modify(func(state *settingsState) error {
				state.Version = version
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			response := webTestRequest(handler, http.MethodPost, "/api/settings", `{"builtin_prompt_mode":"append"}`, "Bearer "+webTestKey)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("invalid version repair returned %d: %s", response.Code, response.Body.String())
			}
			state, err := p.Settings.store.Read()
			if err != nil || state.Version != version {
				t.Fatal("invalid durable version was reset or incremented")
			}
		})
	}
}
