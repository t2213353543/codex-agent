package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func runtimeIdentityFixture(t *testing.T) (gatewayRuntimeIdentity, string) {
	t.Helper()
	before := configuredGatewayRuntimeIdentity.Load()
	t.Cleanup(func() { configuredGatewayRuntimeIdentity.Store(before) })
	configuredGatewayRuntimeIdentity.Store(nil)
	dir := t.TempDir()
	cliPath := filepath.Join(dir, "synthetic-cli")
	// The fixture validates provenance plumbing, not an official CLI capture.
	raw := []byte("synthetic source executable; never executed")
	if err := os.WriteFile(cliPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	identity := gatewayRuntimeIdentity{
		Schema: "codex-runtime-identity-v1", CLIPath: cliPath, SourceCLISHA256: hex.EncodeToString(digest[:]),
		CLIVersion: clientVersion, Originator: clientOriginator,
		UserAgent:  clientOriginator + "/" + clientVersion + " (Synthetic OS 1.0; x86_64) unknown",
		PlatformOS: runtime.GOOS, PlatformArch: runtime.GOARCH,
	}
	path := filepath.Join(dir, "identity.json")
	writeRuntimeIdentityFixture(t, path, identity)
	return identity, path
}

func writeRuntimeIdentityFixture(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func configureRuntimeIdentityFixture(t *testing.T, path string) {
	t.Helper()
	if err := configureGatewayRuntimeIdentity(func(key string) string {
		if key != gatewayRuntimeIdentityEnv {
			t.Fatalf("unexpected environment read %s", key)
		}
		return path
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayRuntimeIdentityGeneratedRequestsAndNativePassThrough(t *testing.T) {
	identity, path := runtimeIdentityFixture(t)
	configureRuntimeIdentityFixture(t, path)
	settings := defaultSettings()
	// Dormant legacy profile selections cannot block a transparent request.
	settings.CodexFingerprintProfile = "obsolete-unused-profile"
	for _, originator := range []string{"codex_exec", "thread-override"} {
		native := http.Header{
			"User-Agent": {"codex_exec/0.155.0 (Windows 11; x86_64) WindowsTerminal (native suffix)"},
			"Originator": {originator}, "Version": {"0.155.0"}, "X-Oai-Attestation": {"native-proof"},
			"Session-Id": {"native-session"}, "Thread-Id": {"native-thread"},
			"X-Codex-Installation-Id": {"native-install"}, "X-Codex-Window-Id": {"native-thread:1"},
			"X-Codex-Turn-Metadata": {`{"turn_id":"native-turn"}`},
		}
		got := make(http.Header)
		if err := copyCodexRequestHeaders(got, native, settings); err != nil {
			t.Fatal(err)
		}
		for name := range native {
			if !reflect.DeepEqual(got.Values(name), native.Values(name)) {
				t.Errorf("runtime capture overwrote caller provenance %s", name)
			}
		}
	}
	fallback := make(http.Header)
	copyRequestHeaders(fallback, http.Header{"User-Agent": {"curl/8.0"}, "X-Oai-Attestation": {"old-proof"}})
	if fallback.Get("User-Agent") != identity.UserAgent || fallback.Get("Originator") != identity.Originator || fallback.Get("Version") != clientVersion || fallback.Get("X-Oai-Attestation") != "" {
		t.Fatal("gateway fallback lost configured identity or retained stale proof")
	}
	oauth := make(http.Header)
	if err := applyGatewayOAuthHeaders(oauth, settings, false); err != nil || oauth.Get("User-Agent") != identity.UserAgent || oauth.Get("Originator") != identity.Originator || oauth.Get("Version") != "" {
		t.Fatalf("refresh default identity mismatch: %v", err)
	}
	if err := applyGatewayOAuthHeaders(oauth, settings, true); err != nil || oauth.Get("User-Agent") != "" || oauth.Get("Originator") != "" || len(oauth["User-Agent"]) != 0 {
		t.Fatalf("raw auth exchange acquired runtime identity: %v", err)
	}
	p := newTestProxy(t, 1)
	managed := httptest.NewRequest(http.MethodGet, "http://example.test/models", nil)
	if err := p.managedUpstreamHeaders(managed, Account{AccessToken: "synthetic-at", AccountID: "synthetic-workspace"}); err != nil {
		t.Fatal(err)
	}
	if managed.UserAgent() != identity.UserAgent || managed.Header.Get("Originator") != identity.Originator || managed.Header.Get("Version") != clientVersion || managed.Header.Get("Authorization") != "Bearer synthetic-at" {
		t.Fatal("managed upstream lost runtime identity or endpoint contract")
	}
}

func TestGatewayRuntimeIdentityPreventsSyntheticEnabledProfile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("verified production identity is a Linux capture")
	}
	identity, path := runtimeIdentityFixture(t)
	configureRuntimeIdentityFixture(t, path)
	settings := defaultSettings()
	settings.CodexFingerprintEnabled = true
	settings.CodexFingerprintProfile = "linux"
	headers := make(http.Header)
	if err := applyCodexFingerprint(headers, settings); err != nil {
		t.Fatal(err)
	}
	if headers.Get("User-Agent") != identity.UserAgent || headers.Get("Originator") != identity.Originator || headers.Get("Version") != identity.CLIVersion {
		t.Fatalf("enabled profile used a synthetic identity: %+v", headers)
	}
	settings.CodexFingerprintProfile = "macos"
	if err := applyCodexFingerprint(make(http.Header), settings); err == nil || !strings.Contains(err.Error(), "fingerprint profile differs") {
		t.Fatal("cross-platform synthetic profile accepted with a verified Linux runtime")
	}
}

func TestGatewayRuntimeIdentityRejectsInvalidManifestBeforePublish(t *testing.T) {
	identity, path := runtimeIdentityFixture(t)
	configureRuntimeIdentityFixture(t, path)
	installed := configuredGatewayRuntimeIdentity.Load()
	base, _ := json.Marshal(identity)
	mutate := func(field string, value any) []byte {
		var fields map[string]any
		_ = json.Unmarshal(base, &fields)
		fields[field] = value
		raw, _ := json.Marshal(fields)
		return raw
	}
	tests := map[string][]byte{
		"duplicate":            append([]byte(`{"schema":"codex-runtime-identity-v1",`), base[1:]...),
		"unknown":              mutate("device_id", "not-a-CLI-field"),
		"case_variant":         mutate("CLI_VERSION", clientVersion),
		"schema":               mutate("schema", "other"),
		"version":              mutate("cli_version", "0.154.0"),
		"originator":           mutate("originator", "codex_exec"),
		"platform_os":          mutate("platform_os", "not-this-runtime"),
		"platform_arch":        mutate("platform_arch", "not-this-architecture"),
		"relative_cli":         mutate("cli_path", "relative-cli"),
		"invalid_hash":         mutate("source_cli_sha256", "invalid"),
		"uppercase_hash":       mutate("source_cli_sha256", strings.ToUpper(identity.SourceCLISHA256)),
		"different_hash":       mutate("source_cli_sha256", strings.Repeat("0", 64)),
		"missing_source":       mutate("cli_path", filepath.Join(t.TempDir(), "missing-cli")),
		"source_is_directory":  mutate("cli_path", t.TempDir()),
		"different_ua_version": mutate("user_agent", "codex_cli_rs/0.154.0 (Linux 1; x86_64) unknown"),
		"short_ua":             mutate("user_agent", clientOriginator+"/"+clientVersion),
		"missing_terminal":     mutate("user_agent", clientOriginator+"/"+clientVersion+" (Linux 1; x86_64) "),
		"crlf":                 mutate("user_agent", identity.UserAgent+"\r\nInjected: value"),
		"tab":                  mutate("user_agent", identity.UserAgent+"\tbad"),
		"long_ua":              mutate("user_agent", identity.UserAgent+strings.Repeat("x", 1024)),
		"wrong_type":           mutate("platform_arch", 1),
		"null":                 mutate("originator", nil),
		"trailing_object":      append(append([]byte(nil), base...), []byte(` {}`)...),
		"trailing_garbage":     append(append([]byte(nil), base...), []byte(` invalid`)...),
		"array":                []byte(`[]`), "empty": nil,
		"oversize": []byte(strings.Repeat(" ", gatewayRuntimeIdentityLimit+1)),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			err := configureGatewayRuntimeIdentity(func(string) string { return path })
			if err == nil || !strings.Contains(err.Error(), gatewayRuntimeIdentityEnv) {
				t.Fatal("invalid runtime capture accepted")
			}
			if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), identity.UserAgent) {
				t.Fatal("validation error exposed source path or identity")
			}
			if configuredGatewayRuntimeIdentity.Load() != installed {
				t.Fatal("invalid capture replaced the immutable snapshot")
			}
		})
	}
}

func TestGatewayRuntimeIdentityStartupBeforeDatabaseAndSourceChanges(t *testing.T) {
	identity, path := runtimeIdentityFixture(t)
	t.Setenv(gatewayRuntimeIdentityEnv, path)
	t.Setenv("DATABASE_URL", "")
	if err := os.WriteFile(identity.CLIPath, []byte("changed source"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(nil); err == nil || !strings.Contains(err.Error(), gatewayRuntimeIdentityEnv) || strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("invalid capture was not rejected before DB startup: %v", err)
	}
	for _, badPath := range []string{"relative.json", t.TempDir(), filepath.Join(t.TempDir(), "missing.json")} {
		if err := configureGatewayRuntimeIdentity(func(string) string { return badPath }); err == nil {
			t.Fatalf("invalid configuration path accepted: %s", badPath)
		}
	}
}

func TestGatewayRuntimeIdentitySnapshotDoesNotReadPerRequest(t *testing.T) {
	identity, path := runtimeIdentityFixture(t)
	configureRuntimeIdentityFixture(t, path)
	// Removing both source files after startup must not trigger per-request IO,
	// execute a CLI, or silently change an established identity.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(identity.CLIPath); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != identity.UserAgent || r.Header.Get("Originator") != identity.Originator {
			t.Error("captured identity differs on the wire")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	copyRequestHeaders(request.Header, nil)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if err := configureGatewayRuntimeIdentity(func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	userAgent, originator := gatewayDefaultApplicationIdentity()
	if userAgent != clientOriginator+"/"+clientVersion || originator != clientOriginator {
		t.Fatal("unconfigured runtime changed the legacy fallback")
	}
}
