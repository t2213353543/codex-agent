package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"
)

// These snapshots exercise both supported runtime platforms even on Windows.
// They are synthetic, immutable, and restored before another test can run.
func adminRuntimeProfileFixture(t *testing.T, platform string) (string, string, *gatewayRuntimeIdentity) {
	t.Helper()
	previous := configuredGatewayRuntimeIdentity.Load()
	t.Cleanup(func() { configuredGatewayRuntimeIdentity.Store(previous) })
	profile, mismatch, arch := "linux", "macos", "amd64"
	if platform == "darwin" {
		profile, mismatch, arch = "macos", "linux", "arm64"
	}
	identity := &gatewayRuntimeIdentity{
		CLIVersion: clientVersion, Originator: clientOriginator,
		UserAgent:  "codex_cli_rs/" + clientVersion + " (Synthetic " + platform + "; " + arch + ") test-runtime",
		PlatformOS: platform, PlatformArch: arch,
	}
	configuredGatewayRuntimeIdentity.Store(identity)
	return profile, mismatch, identity
}

func TestAdminSettingsRuntimeProfileRejection(t *testing.T) {
	runAdminSettingsRuntimeProfileRejection(t, webTestSetup)
}

func TestPGAdminSettingsRuntimeProfileRejection(t *testing.T) {
	runAdminSettingsRuntimeProfileRejection(t, adminPGTestProxy)
}

func runAdminSettingsRuntimeProfileRejection(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			profile, mismatch, _ := adminRuntimeProfileFixture(t, platform)
			for _, scenario := range []string{"current_version", "stale_version", "no_version", "enable_saved_mismatch", "change_enabled_profile"} {
				t.Run(scenario, func(t *testing.T) {
					p, handler := setup(t)
					initial := defaultSettings()
					initial.CodexFingerprintProfile = profile
					if scenario == "enable_saved_mismatch" {
						initial.CodexFingerprintProfile = mismatch
					}
					initial.CodexFingerprintEnabled = scenario == "change_enabled_profile"
					if err := p.Settings.Set(initial); err != nil {
						t.Fatal(err)
					}
					before, err := p.Settings.Get()
					if err != nil {
						t.Fatal(err)
					}
					durableBefore, err := p.Settings.store.Read()
					if err != nil {
						t.Fatal(err)
					}
					update := map[string]any{
						"codex_fingerprint_enabled": true, "codex_fingerprint_profile": mismatch,
						"builtin_prompt": "must not persist with an incompatible profile",
					}
					switch scenario {
					case "current_version":
						update["settings_version"] = before.Version
					case "stale_version":
						update["settings_version"] = before.Version - 1
					case "enable_saved_mismatch":
						delete(update, "codex_fingerprint_profile")
					case "change_enabled_profile":
						delete(update, "codex_fingerprint_enabled")
					}
					body, err := json.Marshal(update)
					if err != nil {
						t.Fatal(err)
					}
					response := webTestRequest(handler, http.MethodPost, "/api/settings", string(body), "Bearer "+webTestKey)
					var failure struct {
						Error struct{ Code string } `json:"error"`
					}
					if response.Code != http.StatusBadRequest || json.Unmarshal(response.Body.Bytes(), &failure) != nil || failure.Error.Code != "invalid_request" {
						t.Errorf("incompatible profile POST returned %d, want 400 invalid_request: %s", response.Code, response.Body.String())
					}
					durableAfter, err := p.Settings.store.Read()
					if err != nil || !reflect.DeepEqual(durableBefore, durableAfter) {
						t.Errorf("rejected update changed durable settings: %+v, %v", durableAfter, err)
					}
					after, err := p.Settings.Get()
					if err != nil || after != before {
						t.Errorf("rejected update changed cached settings: %+v, %v", after, err)
					}
				})
			}
		})
	}
}

func TestAdminSettingsRuntimeProfileRepair(t *testing.T) {
	runAdminSettingsRuntimeProfileRepair(t, webTestSetup)
}

func TestPGAdminSettingsRuntimeProfileRepair(t *testing.T) {
	runAdminSettingsRuntimeProfileRepair(t, adminPGTestProxy)
}

func runAdminSettingsRuntimeProfileRepair(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			profile, mismatch, identity := adminRuntimeProfileFixture(t, platform)
			for _, warm := range []bool{false, true} {
				for _, repair := range []string{"disable", "correct_profile"} {
					t.Run(fmt.Sprintf("warm_%t/%s", warm, repair), func(t *testing.T) {
						p, handler := setup(t)
						account := managedTestAccount(t, p, "runtime-profile-repair", nil)
						// An older deployment can already have persisted this static-valid
						// but runtime-incompatible configuration. Reads must allow repair.
						initial := defaultSettings()
						initial.CodexFingerprintEnabled = true
						initial.CodexFingerprintProfile = mismatch
						initial.BuiltinPrompt = "preserve the old prompt during repair"
						if err := p.Settings.Set(initial); err != nil {
							t.Fatal(err)
						}
						before, err := p.Settings.Get()
						if err != nil {
							t.Fatal(err)
						}
						if !warm {
							p.Settings.mu.Lock()
							p.Settings.loaded = false
							p.Settings.mu.Unlock()
						}
						checkSettings := func(want Settings) {
							t.Helper()
							response := webTestRequest(handler, http.MethodGet, "/api/settings", "", "Bearer "+webTestKey)
							var got struct {
								Settings
								Profiles []CodexFingerprintProfile `json:"codex_fingerprint_profiles"`
							}
							if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &got) != nil || got.Settings != want {
								t.Fatalf("management GET must remain usable for repair: %d %s", response.Code, response.Body.String())
							}
							if len(got.Profiles) != 1 || got.Profiles[0].ID != profile || got.Profiles[0].UserAgent != identity.UserAgent || got.Profiles[0].Originator != identity.Originator || got.Profiles[0].Version != identity.CLIVersion {
								t.Fatalf("settings GET lost runtime identity: %+v", got.Profiles)
							}
						}
						checkRuntime := func(enabled bool, configured, wantError string) {
							t.Helper()
							response := webTestRequest(handler, http.MethodGet, "/api/accounts/"+account.ID+"/runtime", "", "Bearer "+webTestKey)
							var got struct {
								Policy struct {
									Fingerprint struct {
										Enabled    bool
										Configured string `json:"configured_profile"`
										Profile    *CodexFingerprintProfile
										Error      string
									}
								} `json:"effective_policy"`
							}
							if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &got) != nil {
								t.Fatalf("account runtime GET failed: %d %s", response.Code, response.Body.String())
							}
							fingerprint := got.Policy.Fingerprint
							if fingerprint.Enabled != enabled || fingerprint.Configured != configured || fingerprint.Error != wantError {
								t.Fatalf("account runtime misreported profile: %+v", fingerprint)
							}
							if enabled && wantError == "" {
								if fingerprint.Profile == nil || fingerprint.Profile.ID != profile || fingerprint.Profile.UserAgent != identity.UserAgent {
									t.Fatalf("account runtime lost effective identity: %+v", fingerprint)
								}
							} else if fingerprint.Profile != nil {
								t.Fatalf("inactive or mismatched runtime exposed a usable profile: %+v", fingerprint)
							}
						}
						checkSettings(before)
						checkRuntime(true, mismatch, "runtime_profile_mismatch")
						field := `"codex_fingerprint_enabled":false`
						want := before
						want.CodexFingerprintEnabled = false
						if repair == "correct_profile" {
							field = fmt.Sprintf(`"codex_fingerprint_profile":%q`, profile)
							want.CodexFingerprintEnabled = true
							want.CodexFingerprintProfile = profile
						}
						// A valid repair still obeys optimistic concurrency control.
						stale := fmt.Sprintf(`{"settings_version":%d,%s}`, before.Version-1, field)
						response := webTestRequest(handler, http.MethodPost, "/api/settings", stale, "Bearer "+webTestKey)
						if response.Code != http.StatusConflict {
							t.Fatalf("stale repair returned %d: %s", response.Code, response.Body.String())
						}
						checkSettings(before)
						body := fmt.Sprintf(`{"settings_version":%d,%s}`, before.Version, field)
						response = webTestRequest(handler, http.MethodPost, "/api/settings", body, "Bearer "+webTestKey)
						if response.Code != http.StatusOK {
							t.Fatalf("valid repair returned %d: %s", response.Code, response.Body.String())
						}
						want.Version++
						current, err := p.Settings.Get()
						if err != nil || current != want {
							t.Fatalf("repair lost settings: %+v, %v", current, err)
						}
						durable, err := p.Settings.store.Read()
						if err != nil || durable.settings() != want {
							t.Fatalf("repair not persisted: %+v, %v", durable, err)
						}
						checkSettings(want)
						checkRuntime(want.CodexFingerprintEnabled, want.CodexFingerprintProfile, "")
					})
				}
			}
		})
	}
}
