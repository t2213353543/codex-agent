package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestCodexFingerprintCaseInsensitiveIdentityOnWire(t *testing.T) {
	for _, profileID := range []string{"linux", "macos"} {
		t.Run(profileID, func(t *testing.T) {
			profile, _ := codexFingerprintProfile(profileID)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for name, want := range map[string]string{
					"User-Agent": profile.UserAgent, "Originator": profile.Originator, "Version": profile.Version,
				} {
					if got := r.Header.Values(name); !reflect.DeepEqual(got, []string{want}) {
						t.Errorf("wire %s = %q, want exactly %q", name, got, want)
					}
				}
				if values := r.Header.Values("X-Oai-Attestation"); len(values) != 0 {
					t.Errorf("rewritten identity retained attestation: %q", values)
				}
				if got := r.Header.Get("X-Codex-Turn-State"); got != "opaque-state" {
					t.Errorf("turn state changed: %q", got)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			request, err := http.NewRequest(http.MethodGet, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header = http.Header{
				"User-Agent": {"canonical-client", "second-client"}, "user-agent": {"lower-client"}, "uSeR-aGeNt": {"mixed-client"},
				"Originator": {"canonical-origin"}, "originator": {"lower-origin"}, "ORIGINATOR": {"mixed-origin"},
				"Version": {"canonical-version"}, "version": {"lower-version"}, "VERSION": {"mixed-version"},
				"X-Oai-Attestation": {"canonical-proof"}, "x-oai-attestation": {"lower-proof"}, "X-OAI-ATTESTATION": {"mixed-proof"},
				"X-Codex-Turn-State": {"opaque-state"},
			}
			settings := defaultSettings()
			settings.CodexFingerprintEnabled, settings.CodexFingerprintProfile = true, profileID
			if err := applyCodexFingerprint(request.Header, settings); err != nil {
				t.Fatal(err)
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				t.Fatalf("wire request status = %d", response.StatusCode)
			}
		})
	}
}

func TestCodexFingerprintRejectsUnwritableHeaders(t *testing.T) {
	settings := defaultSettings()
	if err := applyCodexFingerprint(nil, settings); err != nil {
		t.Fatalf("disabled apply must remain a no-op: %v", err)
	}
	for _, enabled := range []bool{false, true} {
		settings.CodexFingerprintEnabled = enabled
		if err := copyCodexRequestHeaders(nil, make(http.Header), settings); !errors.Is(err, errInvalid) {
			t.Errorf("nil destination with enabled=%v: %v", enabled, err)
		}
	}
	if err := applyCodexFingerprint(nil, settings); !errors.Is(err, errInvalid) {
		t.Fatalf("enabled nil destination: %v", err)
	}
}

func TestCodexFingerprintInvalidProfileIsAtomic(t *testing.T) {
	for _, invalid := range []string{"", "LINUX", "windows", "linux\r\nInjected: value"} {
		t.Run(invalid, func(t *testing.T) {
			settings := defaultSettings()
			settings.CodexFingerprintEnabled, settings.CodexFingerprintProfile = true, invalid
			destination := http.Header{"X-Codex-Turn-State": {"original"}, "User-Agent": {"original-client"}}
			before := destination.Clone()
			if err := copyCodexRequestHeaders(destination, fingerprintTestHeaders(), settings); !errors.Is(err, errInvalid) {
				t.Fatalf("invalid profile accepted: %v", err)
			}
			if !reflect.DeepEqual(destination, before) {
				t.Fatalf("rejected copy mutated destination: %v", destination)
			}
			if err := applyCodexFingerprint(destination, settings); !errors.Is(err, errInvalid) {
				t.Fatalf("invalid apply accepted: %v", err)
			}
			if !reflect.DeepEqual(destination, before) {
				t.Fatal("rejected apply mutated destination")
			}
		})
	}
}

func TestCodexFingerprintDisabledPreservesIdentityAndProof(t *testing.T) {
	headers := http.Header{
		"user-agent": {"custom-origin/0.155.0 (Linux 6.8; x86_64) unknown (MCP suffix)"},
		"originator": {"custom-origin"}, "x-oai-attestation": {"opaque-proof"},
		"X-Codex-Beta-Features": {"feature-a,feature-b"}, "X-Openai-Internal-Codex-Residency": {"us"},
	}
	before := headers.Clone()
	settings := defaultSettings()
	settings.CodexFingerprintProfile = "unselected-obsolete-profile"
	if err := applyCodexFingerprint(headers, settings); err != nil || !reflect.DeepEqual(headers, before) {
		t.Fatalf("disabled fingerprint changed caller state: %v, %v", headers, err)
	}
}

func TestCodexFingerprintCopyOwnsHeaderValues(t *testing.T) {
	source := fingerprintTestHeaders()
	source["X-Codex-Beta-Features"] = []string{"feature-a", "feature-b"}
	before := source.Clone()
	settings := defaultSettings()
	settings.CodexFingerprintEnabled = true
	destination := make(http.Header)
	if err := copyCodexRequestHeaders(destination, source, settings); err != nil {
		t.Fatal(err)
	}
	destination["X-Codex-Beta-Features"][0] = "changed"
	if !reflect.DeepEqual(source, before) {
		t.Fatal("upstream header changes mutated ingress header values")
	}
}

func TestCodexFingerprintReplacesInvalidIdentityValues(t *testing.T) {
	settings := defaultSettings()
	settings.CodexFingerprintEnabled = true
	request := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	request.Header = http.Header{
		"User-Agent": {"client\r\nInjected: value"}, "originator": {"bad\x00origin"},
		"VERSION": {"version\nInjected: value"}, "x-oai-attestation": {"proof\r\nInjected: value"},
	}
	if err := applyCodexFingerprint(request.Header, settings); err != nil {
		t.Fatal(err)
	}
	if err := request.Write(io.Discard); err != nil {
		t.Fatalf("fixed profile still contains invalid wire header values: %v", err)
	}
	for name, values := range request.Header {
		for _, value := range values {
			if strings.ContainsAny(value, "\r\n\x00") {
				t.Errorf("invalid identity survived in %s", name)
			}
		}
	}
}
