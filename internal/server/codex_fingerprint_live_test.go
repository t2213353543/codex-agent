package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// configureCodexCLIFingerprintTest is opt-in: baseline CLI acceptance keeps its
// existing settings and transport, including when an evidence path is supplied.
func configureCodexCLIFingerprintTest(t *testing.T, p *Proxy) {
	t.Helper()
	profileID := os.Getenv("CODEX_CLI_FINGERPRINT_PROFILE")
	if profileID == "" {
		return
	}
	profile, ok := codexFingerprintProfile(profileID)
	if !ok {
		t.Fatal("CODEX_CLI_FINGERPRINT_PROFILE must be linux or macos")
	}
	proxies, err := p.Proxies.List()
	if err != nil || len(proxies) != 0 {
		t.Fatal("fingerprint audit requires an empty temporary proxy store to cover every upstream client")
	}
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal("cannot read temporary fingerprint settings")
	}
	settings.CodexFingerprintEnabled = true
	settings.CodexFingerprintProfile = profileID
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal("cannot configure temporary fingerprint settings")
	}
	base := p.Client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	audit := &codexFingerprintAuditTransport{base: base, expected: profile}
	p.Client.Transport = audit
	evidenceFile := os.Getenv("CODEX_CLI_FINGERPRINT_EVIDENCE_FILE")
	model := os.Getenv("CODEX_CLI_MODEL")
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`).MatchString(model) {
		model = ""
	}
	linkedEvidence := filepath.Base(os.Getenv("CODEX_CLI_EVIDENCE_FILE"))
	if linkedEvidence == "." || !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`).MatchString(linkedEvidence) {
		linkedEvidence = ""
	}
	t.Cleanup(func() {
		report := audit.snapshot(t.Failed())
		report.Model, report.LinkedEvidence = model, linkedEvidence
		if !report.HeaderVerified {
			t.Errorf("fingerprint audit failed: requests=%d matched=%d mismatched=%d written=%d written_mismatched=%d", report.Requests, report.Matched, report.Mismatched, report.WrittenRequests, report.WrittenMismatched)
		}
		// Header verification alone cannot turn an upstream rejection or network
		// error into a successful live-model acceptance result.
		if report.NetworkErrors != 0 || report.UpstreamFailures != 0 {
			t.Errorf("fingerprint audit saw upstream failures: HTTP=%d network=%d", report.UpstreamFailures, report.NetworkErrors)
		}
		report.AllTestPassed = report.AllTestPassed && !t.Failed()
		if evidenceFile == "" {
			return
		}
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Error("cannot encode fingerprint evidence")
			return
		}
		if err := os.WriteFile(evidenceFile, append(data, '\n'), 0600); err != nil {
			t.Error("cannot write fingerprint evidence")
		}
	})
}

// This wrapper retains only aggregate counts and the public expected identity.
// It neither saves nor changes request headers, URLs, credentials or bodies.
type codexFingerprintAuditTransport struct {
	base     http.RoundTripper
	expected CodexFingerprintProfile
	mu       sync.Mutex
	requests int
	matched  int
	mismatch int
	errors   int
	paths    map[string]int
	statuses map[int]int
	written  int
	writeOK  int
	writeBad int
}

type codexFingerprintAuditEvidence struct {
	Timestamp         string         `json:"timestamp"`
	Profile           string         `json:"profile"`
	Model             string         `json:"model,omitempty"`
	Transport         string         `json:"transport"`
	LinkedEvidence    string         `json:"linked_integration_evidence,omitempty"`
	ExpectedUA        string         `json:"expected_user_agent"`
	ExpectedOrigin    string         `json:"expected_originator"`
	ExpectedVersion   string         `json:"expected_version"`
	Requests          int            `json:"request_count"`
	Matched           int            `json:"matched_count"`
	Mismatched        int            `json:"mismatched_count"`
	WrittenRequests   int            `json:"written_request_count"`
	WrittenMatched    int            `json:"written_matched_count"`
	WrittenMismatched int            `json:"written_mismatched_count"`
	NetworkErrors     int            `json:"network_error_count"`
	UpstreamFailures  int            `json:"upstream_failure_count"`
	EndpointCounts    map[string]int `json:"endpoint_counts"`
	HTTPStatusCounts  map[int]int    `json:"upstream_http_status_counts"`
	HeaderVerified    bool           `json:"header_verified"`
	AllTestPassed     bool           `json:"all_test_passed"`
	IdentityScope     string         `json:"identity_scope"`
	NoTLSClaim        bool           `json:"no_tls_claim"`
}

func codexFingerprintAuditEndpoint(request *http.Request) string {
	if request.URL == nil {
		return "other"
	}
	for _, suffix := range []string{"/responses/compact", "/responses", "/models", "/oauth/token"} {
		if strings.HasSuffix(request.URL.Path, suffix) {
			return suffix
		}
	}
	return "other"
}

func (a *codexFingerprintAuditTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	matched := len(request.Header.Values("X-Oai-Attestation")) == 0
	for key, expected := range map[string]string{
		"User-Agent": a.expected.UserAgent, "Originator": a.expected.Originator, "Version": a.expected.Version,
	} {
		values := request.Header.Values(key)
		if len(values) != 1 || values[0] != expected {
			matched = false
		}
	}
	endpoint := codexFingerprintAuditEndpoint(request)
	a.mu.Lock()
	a.requests++
	if matched {
		a.matched++
	} else {
		a.mismatch++
	}
	if a.paths == nil {
		a.paths = make(map[string]int)
	}
	a.paths[endpoint]++
	a.mu.Unlock()

	// Trace the original transport's actual header writes as well as its input.
	// Only a bitset and booleans survive each callback; no header values are saved.
	var writeMu sync.Mutex
	seen, writeMismatch := uint8(0), false
	trace := &httptrace.ClientTrace{
		WroteHeaderField: func(key string, values []string) {
			writeMu.Lock()
			defer writeMu.Unlock()
			var bit uint8
			var expected string
			switch strings.ToLower(key) {
			case "user-agent":
				bit, expected = 1, a.expected.UserAgent
			case "originator":
				bit, expected = 2, a.expected.Originator
			case "version":
				bit, expected = 4, a.expected.Version
			case "x-oai-attestation":
				writeMismatch = true
				return
			default:
				return
			}
			if seen&bit != 0 || len(values) != 1 || values[0] != expected {
				writeMismatch = true
			}
			seen |= bit
		},
		WroteHeaders: func() {
			writeMu.Lock()
			defer writeMu.Unlock()
			a.mu.Lock()
			a.written++
			if seen == 7 && !writeMismatch {
				a.writeOK++
			} else {
				a.writeBad++
			}
			a.mu.Unlock()
			seen, writeMismatch = 0, false
		},
	}
	forward := request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	response, err := a.base.RoundTrip(forward)
	a.mu.Lock()
	if err != nil {
		a.errors++
	}
	if response != nil {
		if a.statuses == nil {
			a.statuses = make(map[int]int)
		}
		a.statuses[response.StatusCode]++
	}
	a.mu.Unlock()
	return response, err
}

func (a *codexFingerprintAuditTransport) snapshot(testFailed bool) codexFingerprintAuditEvidence {
	a.mu.Lock()
	defer a.mu.Unlock()
	report := codexFingerprintAuditEvidence{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Profile: a.expected.ID,
		ExpectedUA: a.expected.UserAgent, ExpectedOrigin: a.expected.Originator, ExpectedVersion: a.expected.Version,
		Requests: a.requests, Matched: a.matched, Mismatched: a.mismatch, NetworkErrors: a.errors,
		WrittenRequests: a.written, WrittenMatched: a.writeOK, WrittenMismatched: a.writeBad,
		EndpointCounts: make(map[string]int), HTTPStatusCounts: make(map[int]int),
		HeaderVerified: a.requests > 0 && a.mismatch == 0 && a.matched == a.requests && a.written >= a.requests && a.writeBad == 0 && a.writeOK == a.written,
		IdentityScope:  "application_headers_only", NoTLSClaim: true, Transport: "http_sse",
	}
	completed := a.errors
	for key, count := range a.paths {
		report.EndpointCounts[key] = count
	}
	for status, count := range a.statuses {
		report.HTTPStatusCounts[status] = count
		completed += count
		if status >= 400 || status < 100 {
			report.UpstreamFailures += count
		}
	}
	report.AllTestPassed = report.HeaderVerified && !testFailed && a.errors == 0 &&
		report.UpstreamFailures == 0 && completed == a.requests
	return report
}

type codexFingerprintAuditRoundTripFunc func(*http.Request) (*http.Response, error)

type codexFingerprintAuditUntracedRoundTripFunc func(*http.Request) (*http.Response, error)

func (f codexFingerprintAuditUntracedRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func (f codexFingerprintAuditRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	if trace := httptrace.ContextClientTrace(r.Context()); trace != nil {
		for name, values := range r.Header {
			if trace.WroteHeaderField != nil {
				trace.WroteHeaderField(name, values)
			}
		}
		if trace.WroteHeaders != nil {
			trace.WroteHeaders()
		}
	}
	return f(r)
}

func codexFingerprintAuditRequest(t *testing.T, profile CodexFingerprintProfile, endpoint string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, "https://example.invalid/private-account"+endpoint+"?private-query=hidden", strings.NewReader("private-request-body"))
	if err != nil {
		t.Fatal("cannot construct audit unit-test request")
	}
	r.Header.Set("User-Agent", profile.UserAgent)
	r.Header.Set("Originator", profile.Originator)
	r.Header.Set("Version", profile.Version)
	r.Header.Set("Authorization", "Bearer fake-audit-token")
	return r
}

func TestCodexFingerprintAudit(t *testing.T) {
	profile, _ := codexFingerprintProfile("linux")
	t.Run("disabled is inert", func(t *testing.T) {
		t.Setenv("CODEX_CLI_FINGERPRINT_PROFILE", "")
		evidence := filepath.Join(t.TempDir(), "must-not-exist.json")
		t.Setenv("CODEX_CLI_FINGERPRINT_EVIDENCE_FILE", evidence)
		configureCodexCLIFingerprintTest(t, nil)
		if _, err := os.Stat(evidence); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("disabled audit wrote evidence")
		}
	})
	t.Run("forwarding and redaction", func(t *testing.T) {
		request := codexFingerprintAuditRequest(t, profile, "/responses")
		headers, body, target := request.Header.Clone(), request.Body, request.URL
		response := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("private-response-body"))}
		audit := &codexFingerprintAuditTransport{expected: profile, base: codexFingerprintAuditRoundTripFunc(func(got *http.Request) (*http.Response, error) {
			if got.Body != body || got.URL != target || got.Method != request.Method || !reflect.DeepEqual(got.Header, headers) {
				t.Error("audit changed the forwarded request")
			}
			data, err := io.ReadAll(got.Body)
			if err != nil || string(data) != "private-request-body" {
				t.Error("audit consumed or changed the request body")
			}
			return response, nil
		})}
		got, err := audit.RoundTrip(request)
		if err != nil || got != response {
			t.Fatal("audit changed the forwarded response")
		}
		report := audit.snapshot(false)
		if !report.HeaderVerified || !report.AllTestPassed || report.Requests != 1 || report.EndpointCounts["/responses"] != 1 || report.HTTPStatusCounts[200] != 1 {
			t.Fatal("successful request audit was not recorded")
		}
		encoded, err := json.Marshal(report)
		if err != nil {
			t.Fatal("cannot encode audit report")
		}
		for _, secret := range []string{"fake-audit-token", "private-account", "private-query", "private-request-body", "private-response-body", "example.invalid", "Authorization"} {
			if strings.Contains(string(encoded), secret) {
				t.Error("audit evidence contains request or response data")
			}
		}
		if audit.snapshot(true).AllTestPassed {
			t.Error("audit ignored an existing test failure")
		}
	})
	t.Run("rejections and failures stay failures", func(t *testing.T) {
		for _, scenario := range []string{"no requests", "HTTP rejection", "network error", "identity mismatch", "duplicate identity"} {
			t.Run(scenario, func(t *testing.T) {
				networkError := errors.New("private-network-details")
				audit := &codexFingerprintAuditTransport{expected: profile, base: codexFingerprintAuditRoundTripFunc(func(*http.Request) (*http.Response, error) {
					if scenario == "network error" {
						return nil, networkError
					}
					status := http.StatusOK
					if scenario == "HTTP rejection" {
						status = http.StatusBadRequest
					}
					return &http.Response{StatusCode: status, Body: http.NoBody}, nil
				})}
				if scenario != "no requests" {
					request := codexFingerprintAuditRequest(t, profile, "/models")
					if scenario == "identity mismatch" {
						request.Header.Set("Version", "incorrect")
					}
					if scenario == "duplicate identity" {
						request.Header.Add("Originator", "unexpected")
					}
					_, err := audit.RoundTrip(request)
					if scenario == "network error" && err != networkError {
						t.Error("audit replaced the upstream error")
					}
				}
				report := audit.snapshot(false)
				if report.AllTestPassed {
					t.Fatal("incomplete or unsuccessful audit reported a pass")
				}
				if scenario == "HTTP rejection" && (report.HTTPStatusCounts[400] != 1 || report.UpstreamFailures != 1 || !report.HeaderVerified) {
					t.Error("audit lost the actual rejection status or valid header evidence")
				}
				if scenario == "network error" && (report.NetworkErrors != 1 || len(report.HTTPStatusCounts) != 0) {
					t.Error("audit lost the network error or fabricated an HTTP status")
				}
				if (scenario == "identity mismatch" || scenario == "duplicate identity") && (report.HeaderVerified || report.Mismatched != 1) {
					t.Error("audit accepted a mismatched identity")
				}
			})
		}
	})
	t.Run("concurrent requests", func(t *testing.T) {
		audit := &codexFingerprintAuditTransport{expected: profile, base: codexFingerprintAuditRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
		})}
		var workers sync.WaitGroup
		for i := 0; i < 24; i++ {
			request := codexFingerprintAuditRequest(t, profile, "/responses")
			workers.Go(func() { _, _ = audit.RoundTrip(request) })
		}
		workers.Wait()
		report := audit.snapshot(false)
		if !report.AllTestPassed || report.Requests != 24 || report.Matched != 24 || report.HTTPStatusCounts[200] != 24 {
			t.Error("concurrent audit lost request counts")
		}
	})
	t.Run("actual transport writes required", func(t *testing.T) {
		for _, scenario := range []string{"no trace", "changed written identity", "written attestation"} {
			t.Run(scenario, func(t *testing.T) {
				audit := &codexFingerprintAuditTransport{expected: profile, base: codexFingerprintAuditUntracedRoundTripFunc(func(request *http.Request) (*http.Response, error) {
					if scenario != "no trace" {
						trace := httptrace.ContextClientTrace(request.Context())
						ua := profile.UserAgent
						if scenario == "changed written identity" {
							ua = "different-on-wire"
						}
						trace.WroteHeaderField("User-Agent", []string{ua})
						trace.WroteHeaderField("Originator", []string{profile.Originator})
						trace.WroteHeaderField("Version", []string{profile.Version})
						if scenario == "written attestation" {
							trace.WroteHeaderField("X-Oai-Attestation", []string{"fake-attestation"})
						}
						trace.WroteHeaders()
					}
					return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
				})}
				_, _ = audit.RoundTrip(codexFingerprintAuditRequest(t, profile, "/responses"))
				report := audit.snapshot(false)
				if report.Matched != 1 || report.HeaderVerified || report.AllTestPassed {
					t.Error("request input alone was accepted as actual written-header evidence")
				}
				if scenario != "no trace" && (report.WrittenRequests != 1 || report.WrittenMismatched != 1) {
					t.Error("actual written-header mismatch was not counted")
				}
			})
		}
	})
	t.Run("configured cleanup evidence", func(t *testing.T) {
		for _, profileID := range []string{"linux", "macos"} {
			t.Run(profileID, func(t *testing.T) {
				evidence := filepath.Join(t.TempDir(), "evidence.json")
				t.Setenv("CODEX_CLI_FINGERPRINT_PROFILE", profileID)
				t.Setenv("CODEX_CLI_FINGERPRINT_EVIDENCE_FILE", evidence)
				t.Run("configured request", func(t *testing.T) {
					p := newTestProxy(t, 1)
					p.Client.Transport = codexFingerprintAuditRoundTripFunc(func(*http.Request) (*http.Response, error) {
						return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
					})
					configureCodexCLIFingerprintTest(t, p)
					settings, err := p.Settings.Get()
					if err != nil || !settings.CodexFingerprintEnabled || settings.CodexFingerprintProfile != profileID {
						t.Fatal("opt-in audit settings not applied")
					}
					expected, _ := codexFingerprintProfile(profileID)
					if _, err := p.Client.Transport.RoundTrip(codexFingerprintAuditRequest(t, expected, "/responses")); err != nil {
						t.Fatal("fake upstream request failed")
					}
				})
				data, err := os.ReadFile(evidence)
				var report codexFingerprintAuditEvidence
				if err != nil || json.Unmarshal(data, &report) != nil || !report.AllTestPassed || report.Profile != profileID || report.IdentityScope != "application_headers_only" || !report.NoTLSClaim {
					t.Error("cleanup did not write valid application-header evidence")
				}
			})
		}
	})
}
