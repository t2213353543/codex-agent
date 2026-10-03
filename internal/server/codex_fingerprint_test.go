package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const fingerprintIncomingUA = "codex_cli_rs/0.149.0 (Windows 11; x86_64) WindowsTerminal"

func fingerprintTestHeaders() http.Header {
	return http.Header{
		"Authorization": {"Bearer test-client-key"},
		"User-Agent":    {fingerprintIncomingUA}, "Originator": {"codex_vscode"}, "Version": {"0.149.0"},
		"Session-Id": {"fingerprint-session"}, "Thread-Id": {"fingerprint-thread"},
		"X-Codex-Turn-State": {"opaque-turn-state"}, "X-Codex-Routing-Hint": {"opaque-routing-hint"},
		"Openai-Beta":        {"responses_websockets=2026-02-06"},
		"X-Oai-Attestation":  {"bound-to-original-client"},
		"Chatgpt-Account-Id": {"incoming-account-must-not-leak"},
		"Cookie":             {"private-client-cookie"}, "Proxy-Authorization": {"private-client-proxy"},
	}
}

func setFingerprintTestSettings(t *testing.T, p *Proxy, enabled bool, profile string) {
	t.Helper()
	s, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	s.CodexFingerprintEnabled, s.CodexFingerprintProfile = enabled, profile
	if err := p.Settings.Set(s); err != nil {
		t.Fatal(err)
	}
}

func assertFingerprintUpstreamHeaders(t *testing.T, headers http.Header, enabled bool, profile string) {
	t.Helper()
	wantUA, wantOriginator, wantVersion := fingerprintIncomingUA, "codex_vscode", "0.149.0"
	wantAttestation := "bound-to-original-client"
	if enabled {
		platform := "Linux 6.8.0; x86_64"
		if profile == "macos" {
			platform = "Mac OS 15.0.0; arm64"
		}
		wantUA = "codex_cli_rs/" + clientVersion + " (" + platform + ") unknown"
		wantOriginator, wantVersion, wantAttestation = "codex_cli_rs", clientVersion, ""
	}
	for key, want := range map[string]string{
		"User-Agent": wantUA, "Originator": wantOriginator, "Version": wantVersion,
		"X-Oai-Attestation": wantAttestation,
		"Session-Id":        "fingerprint-session", "Thread-Id": "fingerprint-thread",
		"X-Codex-Turn-State": "opaque-turn-state", "X-Codex-Routing-Hint": "opaque-routing-hint",
		"OpenAI-Beta":   "responses_websockets=2026-02-06",
		"Authorization": "Bearer test-upstream-token", "ChatGPT-Account-Id": "test-account",
		"Cookie": "", "Proxy-Authorization": "",
	} {
		if got := headers.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestCodexFingerprintSettingsAPIAndPersistence(t *testing.T) {
	p, handler := webTestSetup(t)
	initial, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	if initial.CodexFingerprintEnabled || initial.CodexFingerprintProfile != "linux" {
		t.Fatalf("default fingerprint settings: %+v", initial)
	}
	post := func(body string, want int) {
		t.Helper()
		response := webTestRequest(handler, http.MethodPost, "/api/settings", body, "Bearer "+webTestKey)
		if response.Code != want {
			t.Fatalf("POST %s: HTTP %d: %s", body, response.Code, response.Body.String())
		}
	}
	body := fmt.Sprintf(`{"settings_version":%d,"codex_fingerprint_enabled":true,"codex_fingerprint_profile":"macos"}`, initial.Version)
	post(body, http.StatusOK)
	post(body, http.StatusConflict)
	updated, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	want := initial
	want.Version++
	want.CodexFingerprintEnabled, want.CodexFingerprintProfile = true, "macos"
	if updated != want {
		t.Fatalf("fingerprint update changed unrelated settings: got %+v want %+v", updated, want)
	}
	for _, invalid := range []string{
		`{"codex_fingerprint_enabled":"true"}`, `{"codex_fingerprint_profile":true}`,
		`{"codex_fingerprint_profile":""}`, `{"codex_fingerprint_profile":"ios"}`,
		`{"codex_fingerprint_profile":"windows"}`, `{"codex_fingerprint_profile":"MACOS"}`,
		`{"codex_fingerprint_enabled":false,"codex_fingerprint_profile":"invalid"}`,
		`{"regenerate":true,"codex_fingerprint_enabled":false}`,
	} {
		post(invalid, http.StatusBadRequest)
	}
	unchanged, err := p.Settings.Get()
	if err != nil || unchanged != updated {
		t.Fatalf("invalid update persisted: %+v, %v", unchanged, err)
	}
	response := webTestRequest(handler, http.MethodGet, "/api/settings", "", "Bearer "+webTestKey)
	var actual struct {
		Settings
		Profiles []struct {
			ID         string `json:"id"`
			Label      string `json:"label"`
			UserAgent  string `json:"user_agent"`
			Originator string `json:"originator"`
			Version    string `json:"version"`
		} `json:"codex_fingerprint_profiles"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &actual) != nil || actual.Settings != updated {
		t.Fatalf("GET fingerprint settings: HTTP %d: %s", response.Code, response.Body.String())
	}
	if len(actual.Profiles) != 2 {
		t.Fatalf("GET profiles = %+v", actual.Profiles)
	}
	seen := make(map[string]bool)
	for _, profile := range actual.Profiles {
		configured, exists := codexFingerprintProfile(profile.ID)
		if !exists || seen[profile.ID] || profile.Label == "" || profile.UserAgent != configured.UserAgent || profile.Originator != "codex_cli_rs" || profile.Version != clientVersion {
			t.Errorf("invalid or duplicate profile in GET: %+v", profile)
		}
		seen[profile.ID] = true
	}
	if !seen["linux"] || !seen["macos"] {
		t.Errorf("missing supported profiles: %+v", seen)
	}
	reloaded, err := NewSettingsStore(p.Settings.store.path).Get()
	if err != nil || reloaded != updated {
		t.Fatalf("restart lost settings: %+v, %v", reloaded, err)
	}
	post(`{"codex_fingerprint_enabled":false}`, http.StatusOK)
	disabled, err := NewSettingsStore(p.Settings.store.path).Get()
	if err != nil || disabled.CodexFingerprintEnabled || disabled.CodexFingerprintProfile != "macos" {
		t.Fatalf("partial disable was not saved: %+v, %v", disabled, err)
	}
}

func TestCodexFingerprintLegacySettingsMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"version":4,"admin_api":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	settings, err := NewSettingsStore(path).Get()
	if err != nil || settings.CodexFingerprintEnabled || settings.CodexFingerprintProfile != "linux" || settings.AdminAPI || settings.Version != 4 {
		t.Fatalf("legacy defaults changed: %+v, %v", settings, err)
	}
}

func TestCodexFingerprintHTTPRoutesAndDisable(t *testing.T) {
	for _, profile := range []string{"linux", "macos"} {
		t.Run(profile, func(t *testing.T) {
			for _, enabled := range []bool{true, false} {
				t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
					for _, route := range []struct{ name, method, path, body string }{
						{"sse", http.MethodPost, "/v1/responses", `{"model":"test","stream":true,"input":[],"prompt_cache_key":"client-cache-key"}`},
						{"json", http.MethodPost, "/v1/responses", `{"model":"test","stream":false,"input":[],"prompt_cache_key":"client-cache-key"}`},
						{"compact", http.MethodPost, "/v1/responses/compact", `{"model":"test","input":[],"prompt_cache_key":"client-cache-key"}`},
						{"models", http.MethodGet, "/v1/models?client_version=0.149.0&client_key=never-forward", ""},
					} {
						t.Run(route.name, func(t *testing.T) {
							p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
								assertFingerprintUpstreamHeaders(t, r.Header, enabled, profile)
								if route.name == "models" {
									wantVersion := "0.149.0"
									if enabled {
										wantVersion = clientVersion
									}
									if r.URL.Path != "/models" || r.URL.Query().Get("client_version") != wantVersion || len(r.URL.Query()) != 1 {
										t.Errorf("models query did not follow identity: %s", r.URL)
									}
									w.Header().Set("Content-Type", "application/json")
									_, _ = io.WriteString(w, `{"models":[{"slug":"test"}]}`)
									return
								}
								var body map[string]json.RawMessage
								if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
									t.Error(err)
								}
								if string(body["prompt_cache_key"]) != `"`+gatewayFixtureScopedValue("prompt-cache", "client-cache-key")+`"` {
									t.Errorf("client cache key lost its owner namespace: %s", body["prompt_cache_key"])
								}
								if route.name == "compact" {
									w.Header().Set("Content-Type", "application/json")
									_, _ = io.WriteString(w, `{"id":"compact_fingerprint","object":"response.compaction","output":[]}`)
									return
								}
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fingerprint\",\"status\":\"completed\",\"output\":[]}}\n\n")
							})
							// Exercise an actual on -> off transition for the disabled request.
							setFingerprintTestSettings(t, p, true, profile)
							if !enabled {
								setFingerprintTestSettings(t, p, false, profile)
							}
							r := proxyRequest(route.method, route.path, route.body)
							r.Header = fingerprintTestHeaders()
							w := httptest.NewRecorder()
							p.ServeHTTP(w, r)
							if w.Code != http.StatusOK {
								t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
							}
						})
					}
				})
			}
		})
	}
}

func TestCodexFingerprintWebSocketHandshake(t *testing.T) {
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		assertFingerprintUpstreamHeaders(t, r.Header, true, "macos")
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		_, raw, err := conn.Read(r.Context())
		if err != nil {
			t.Error(err)
			return
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Error(err)
		}
		if string(payload["prompt_cache_key"]) != `"`+gatewayFixtureScopedValue("prompt-cache", "client-cache-key")+`"` {
			t.Errorf("WS cache key lost its owner namespace: %s", raw)
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_fingerprint_ws","status":"completed","output":[]}}`))
		_, _, _ = conn.Read(r.Context())
	})
	enableGatewayWS(t, p, "")
	setFingerprintTestSettings(t, p, true, "macos")
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, server.URL+"/v1/responses", &websocket.DialOptions{HTTPHeader: fingerprintTestHeaders()})
	if err != nil {
		t.Fatalf("WS dial: %v (%v)", err, response)
	}
	defer conn.CloseNow()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"test","input":[],"prompt_cache_key":"client-cache-key"}`)); err != nil {
		t.Fatal(err)
	}
	if raw := gatewayWSRead(t, conn); !strings.Contains(string(raw), "resp_fingerprint_ws") {
		t.Fatalf("WS response changed: %s", raw)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
	waitGatewayWSReleased(t, p)
}

func TestCodexFingerprintOAuthRefresh(t *testing.T) {
	for _, profile := range []string{"linux", "macos"} {
		t.Run(profile, func(t *testing.T) {
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				configured, ok := codexFingerprintProfile(profile)
				if !ok {
					t.Errorf("missing profile %s", profile)
				}
				if r.URL.Path != "/oauth/token" || r.Header.Get("User-Agent") != configured.UserAgent || r.Header.Get("Originator") != "codex_cli_rs" || r.Header.Get("Version") != "" {
					t.Errorf("OAuth identity mismatch: %s %v", r.URL, r.Header)
				}
				var payload map[string]string
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if r.Header.Get("Content-Type") != "application/json" || payload["refresh_token"] != "test-refresh-token" || payload["grant_type"] != "refresh_token" {
					t.Error("invalid refresh JSON")
				}
				if payload["client_id"] != "fingerprint-issued-client" {
					t.Error("fingerprint profile replaced the OAuth token issuer client_id")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`, "test-account", "fixture-user"))
			})
			setFingerprintTestSettings(t, p, true, profile)
			account, err := p.Store.Active()
			if err != nil {
				t.Fatal(err)
			}
			account.ClientID = "fingerprint-issued-client"
			updated, err := p.refreshAccountWithFingerprint(context.Background(), account)
			if err != nil || updated.AccessToken != "rotated-access" || updated.RefreshToken != "rotated-refresh" || updated.AccountID != account.AccountID {
				t.Fatalf("refresh result: %+v, %v", updated, err)
			}
		})
	}
}

func TestCodexFingerprintWebSocketSettingsChange(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		before, after               bool
		beforeProfile, afterProfile string
		wantConflict                bool
	}{
		{"switch_profile", true, true, "linux", "macos", true},
		{"disable", true, false, "linux", "linux", true},
		{"enable", false, true, "linux", "linux", true},
		{"preselect_while_disabled", false, false, "linux", "macos", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var turns atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				for {
					if _, _, err := conn.Read(r.Context()); err != nil {
						return
					}
					turn := turns.Add(1)
					result := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_turn_%d","status":"completed","output":[]}}`, turn)
					if err := conn.Write(r.Context(), websocket.MessageText, []byte(result)); err != nil {
						return
					}
				}
			})
			enableGatewayWS(t, p, "")
			setFingerprintTestSettings(t, p, tc.before, tc.beforeProfile)
			server := httptest.NewServer(p)
			t.Cleanup(server.Close)
			conn, _ := gatewayWSDial(t, server.URL, "fingerprint-switch-session")
			defer conn.CloseNow()
			request := []byte(`{"type":"response.create","model":"test","input":[]}`)
			if err := gatewayWSWrite(t.Context(), conn, websocket.MessageText, request); err != nil {
				t.Fatal(err)
			}
			if raw := gatewayWSRead(t, conn); !strings.Contains(string(raw), "resp_turn_1") {
				t.Fatalf("first turn: %s", raw)
			}
			setFingerprintTestSettings(t, p, tc.after, tc.afterProfile)
			if err := gatewayWSWrite(t.Context(), conn, websocket.MessageText, request); err != nil {
				t.Fatal(err)
			}
			raw := gatewayWSRead(t, conn)
			if tc.wantConflict {
				var event struct {
					Type   string `json:"type"`
					Status int    `json:"status"`
				}
				if json.Unmarshal(raw, &event) != nil || event.Type != "error" || event.Status != http.StatusConflict || !strings.Contains(string(raw), "reconnect") {
					t.Fatalf("changed identity reused old connection: %s", raw)
				}
			} else if !strings.Contains(string(raw), "resp_turn_2") {
				t.Fatalf("disabled profile selection interrupted unchanged identity: %s", raw)
			}
			_ = conn.Close(websocket.StatusNormalClosure, "done")
			waitGatewayWSReleased(t, p)
			wantTurns := int32(2)
			if tc.wantConflict {
				wantTurns = 1
			}
			if got := turns.Load(); got != wantTurns {
				t.Fatalf("upstream received %d turns, want %d", got, wantTurns)
			}
		})
	}
}

func TestCodexFingerprintHeaderOverrideAndCompatibility(t *testing.T) {
	source := fingerprintTestHeaders()
	original := source.Clone()
	settings := defaultSettings()
	legacy := make(http.Header)
	copyRequestHeaders(legacy, source)
	disabled := make(http.Header)
	if err := copyCodexRequestHeaders(disabled, source, settings); err != nil || !reflect.DeepEqual(disabled, legacy) {
		t.Fatalf("disabled behavior differs from existing filter: %v, %v", disabled, err)
	}
	settings.CodexFingerprintEnabled, settings.CodexFingerprintProfile = true, "linux"
	override := make(http.Header)
	if err := copyCodexRequestHeaders(override, source, settings); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(source, original) {
		t.Fatal("rewriting mutated original client headers")
	}
	if override.Get("X-Oai-Attestation") != "" {
		t.Fatal("identity-bound attestation survived rewrite")
	}
	if override.Get("User-Agent") == fingerprintIncomingUA {
		t.Fatal("native Windows identity bypassed forced fingerprint")
	}
	settings.CodexFingerprintProfile = "ios"
	if err := applyCodexFingerprint(make(http.Header), settings); err == nil {
		t.Fatal("unsupported enabled profile was silently accepted")
	}
}
