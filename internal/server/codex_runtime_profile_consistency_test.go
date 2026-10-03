package server

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestEffectiveCodexFingerprintProfilesPreserveLegacyWithoutRuntime(t *testing.T) {
	previous := configuredGatewayRuntimeIdentity.Load()
	t.Cleanup(func() { configuredGatewayRuntimeIdentity.Store(previous) })
	configuredGatewayRuntimeIdentity.Store(nil)

	legacy := codexFingerprintProfiles()
	effective := effectiveCodexFingerprintProfiles()
	if !reflect.DeepEqual(effective, legacy) {
		t.Fatalf("unconfigured effective profiles changed legacy behavior: got %+v want %+v", effective, legacy)
	}
	for _, id := range []string{"linux", "macos"} {
		got, err := effectiveCodexFingerprintProfile(id)
		if err != nil {
			t.Fatalf("legacy profile %q: %v", id, err)
		}
		want, ok := codexFingerprintProfile(id)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Fatalf("legacy profile %q: got %+v want %+v", id, got, want)
		}
	}
}

func TestEffectiveCodexFingerprintProfilesFollowDarwinRuntimeSnapshot(t *testing.T) {
	previous := configuredGatewayRuntimeIdentity.Load()
	t.Cleanup(func() { configuredGatewayRuntimeIdentity.Store(previous) })
	identity := &gatewayRuntimeIdentity{
		CLIVersion: clientVersion, Originator: clientOriginator,
		UserAgent:  clientOriginator + "/" + clientVersion + " (Darwin 24.0; arm64) unknown",
		PlatformOS: "darwin", PlatformArch: "arm64",
	}
	configuredGatewayRuntimeIdentity.Store(identity)

	profiles := effectiveCodexFingerprintProfiles()
	if len(profiles) != 1 || profiles[0].ID != "macos" {
		t.Fatalf("darwin runtime profiles = %+v, want one macos profile", profiles)
	}
	if profiles[0].UserAgent != identity.UserAgent || profiles[0].Originator != identity.Originator || profiles[0].Version != identity.CLIVersion {
		t.Fatalf("darwin effective profile lost runtime identity: %+v", profiles[0])
	}
	got, err := effectiveCodexFingerprintProfile("macos")
	if err != nil || !reflect.DeepEqual(got, profiles[0]) {
		t.Fatalf("darwin macos lookup: %+v, %v", got, err)
	}
	if _, err := effectiveCodexFingerprintProfile("linux"); err == nil || !strings.Contains(err.Error(), "runtime platform") {
		t.Fatalf("linux profile accepted for darwin runtime: %v", err)
	}
}

func TestCopyCodexRequestHeadersRejectsRuntimeProfileBeforeMutation(t *testing.T) {
	previous := configuredGatewayRuntimeIdentity.Load()
	t.Cleanup(func() { configuredGatewayRuntimeIdentity.Store(previous) })
	configuredGatewayRuntimeIdentity.Store(&gatewayRuntimeIdentity{
		CLIVersion: clientVersion, Originator: clientOriginator,
		UserAgent:  clientOriginator + "/" + clientVersion + " (Darwin 24.0; arm64) unknown",
		PlatformOS: "darwin", PlatformArch: "arm64",
	})
	settings := defaultSettings()
	settings.CodexFingerprintEnabled = true
	settings.CodexFingerprintProfile = "linux"
	destination := http.Header{"X-Codex-Turn-State": {"original"}, "User-Agent": {"original-client"}}
	before := destination.Clone()
	if err := copyCodexRequestHeaders(destination, fingerprintTestHeaders(), settings); err == nil || !errors.Is(err, errInvalid) {
		t.Fatalf("runtime-mismatched profile accepted: %v", err)
	}
	if !reflect.DeepEqual(destination, before) {
		t.Fatalf("runtime mismatch mutated destination: got %v want %v", destination, before)
	}
}
