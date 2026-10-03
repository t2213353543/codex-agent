package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// These are deterministic application-header reference profiles, constructed
// from Codex 0.155.0 login/src/auth/default_client.rs. The OS versions describe
// the reference profile, not the gateway host or a captured physical device.
// Transport identity is selected separately: these headers do not configure
// either the Go transport or the native Rust HTTP/WebSocket transport.
type CodexFingerprintProfile struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	UserAgent  string `json:"user_agent"`
	Originator string `json:"originator"`
	Version    string `json:"version"`
}

const defaultCodexFingerprintProfile = "linux"

// codexFingerprintProfileIDForRuntime maps the runtime identity vocabulary to
// the user-facing profile IDs.  The persisted/runtime snapshot uses GOOS
// names (darwin), while the Codex settings API intentionally uses macos.
func codexFingerprintProfileIDForRuntime(platformOS string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(platformOS)) {
	case "linux":
		return "linux", true
	case "darwin":
		return "macos", true
	default:
		return "", false
	}
}

func codexFingerprintProfiles() []CodexFingerprintProfile {
	return []CodexFingerprintProfile{
		{ID: "linux", Label: "Linux CLI（源码参考配置）",
			UserAgent:  clientOriginator + "/" + clientVersion + " (Linux 6.8.0; x86_64) unknown",
			Originator: clientOriginator, Version: clientVersion},
		{ID: "macos", Label: "macOS CLI（源码参考配置）",
			UserAgent:  clientOriginator + "/" + clientVersion + " (Mac OS 15.0.0; arm64) unknown",
			Originator: clientOriginator, Version: clientVersion},
	}
}

func codexFingerprintProfile(id string) (CodexFingerprintProfile, bool) {
	for _, profile := range codexFingerprintProfiles() {
		if profile.ID == id {
			return profile, true
		}
	}
	return CodexFingerprintProfile{}, false
}

// effectiveCodexFingerprintProfiles derives the selectable profiles from one
// immutable runtime snapshot.  Without a snapshot this deliberately preserves
// the legacy static profiles; runtime identity is opt-in and must not silently
// change an existing unconfigured deployment.
func effectiveCodexFingerprintProfiles() []CodexFingerprintProfile {
	identity := configuredGatewayRuntimeIdentity.Load()
	return effectiveCodexFingerprintProfilesForIdentity(identity)
}

func effectiveCodexFingerprintProfilesForIdentity(identity *gatewayRuntimeIdentity) []CodexFingerprintProfile {
	if identity == nil {
		return codexFingerprintProfiles()
	}
	id, ok := codexFingerprintProfileIDForRuntime(identity.PlatformOS)
	if !ok {
		return nil
	}
	legacy, _ := codexFingerprintProfile(id)
	return []CodexFingerprintProfile{{
		ID: id, Label: legacy.Label,
		UserAgent: identity.UserAgent, Originator: identity.Originator, Version: identity.CLIVersion,
	}}
}

// effectiveCodexFingerprintProfile performs the same runtime snapshot lookup
// used for the settings response.  Callers should resolve before mutating any
// destination headers so a rejected profile is atomic.
func effectiveCodexFingerprintProfile(id string) (CodexFingerprintProfile, error) {
	identity := configuredGatewayRuntimeIdentity.Load()
	for _, profile := range effectiveCodexFingerprintProfilesForIdentity(identity) {
		if profile.ID == id {
			return profile, nil
		}
	}
	if identity != nil {
		return CodexFingerprintProfile{}, fmt.Errorf("%w: fingerprint profile differs from runtime platform", errInvalid)
	}
	return CodexFingerprintProfile{}, fmt.Errorf("%w: codex_fingerprint_profile must be linux or macos", errInvalid)
}

func applyCodexFingerprint(headers http.Header, settings Settings) error {
	if !settings.CodexFingerprintEnabled {
		return nil
	}
	profile, err := effectiveCodexFingerprintProfile(settings.CodexFingerprintProfile)
	if err != nil {
		return err
	}
	if headers == nil {
		return fmt.Errorf("%w: Codex fingerprint requires writable request headers", errInvalid)
	}
	// Header.Set/Del only handle canonical map keys. Headers built by transports
	// or internal callers can also contain lower-case or mixed-case keys, which
	// would otherwise survive rewriting and produce a second wire identity.
	for name := range headers {
		if strings.EqualFold(name, "User-Agent") || strings.EqualFold(name, "Originator") ||
			strings.EqualFold(name, "Version") || strings.EqualFold(name, "X-Oai-Attestation") {
			delete(headers, name)
		}
	}
	headers.Set("User-Agent", profile.UserAgent)
	headers.Set("Originator", profile.Originator)
	headers.Set("Version", profile.Version)
	// Do not replay a caller's opaque attestation across an identity rewrite.
	// Preserve session IDs, turn state, beta capabilities and tracing headers.
	return nil
}

func copyCodexRequestHeaders(dst, src http.Header, settings Settings) error {
	if dst == nil {
		return fmt.Errorf("%w: Codex request requires writable request headers", errInvalid)
	}
	if !settings.CodexFingerprintEnabled {
		copyRequestHeaders(dst, src)
		return nil
	}
	// Build and validate a private candidate first. This both keeps the runtime
	// identity snapshot consistent with applyCodexFingerprint and guarantees a
	// rejected profile cannot partially overwrite the caller's destination.
	candidate := make(http.Header)
	copyRequestHeaders(candidate, src)
	if err := applyCodexFingerprint(candidate, settings); err != nil {
		return err
	}
	for name := range dst {
		delete(dst, name)
	}
	for name, values := range candidate {
		dst[name] = append([]string(nil), values...)
	}
	return nil
}

// Both explicit admin refresh and automatic refresh read the same persisted
// profile. OAuth client_id still belongs to the account that issued the token.
func (p *Proxy) refreshAccountWithFingerprint(ctx context.Context, account Account) (Account, error) {
	settings, err := p.settingsValue()
	if err != nil {
		return account, err
	}
	proxy, err := p.gatewayRequestProxy(ctx, account)
	if err != nil {
		return account, err
	}
	return p.refreshAccountWithCooldown(ctx, p.gatewayRequestClient(ctx, proxy, gatewayClientHTTP), account, settings)
}
