package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type nativeCLIFallbackGuard struct{ calls atomic.Int32 }

func (g *nativeCLIFallbackGuard) RoundTrip(r *http.Request) (*http.Response, error) {
	g.calls.Add(1)
	if r.Body != nil {
		_ = r.Body.Close()
	}
	return nil, errors.New("synthetic native CLI fixture forbids Go upstream fallback")
}

// This opt-in test uses an actual official CLI and Rust helper, but its model
// and tool-requesting response are deterministic loopback fixtures, not inference.
func TestGatewayNativeActualCLI(t *testing.T) {
	cli, helper := os.Getenv("CODEX_NATIVE_CAPTURE_BIN"), os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN")
	if cli == "" || helper == "" || runtime.GOOS != "linux" {
		t.Skip("set CODEX_NATIVE_CAPTURE_BIN and CODEX_NATIVE_HTTP_TEST_BIN on Linux for actual CLI through Rust UDS")
	}
	absolute, err := filepath.Abs(cli)
	if err != nil {
		t.Fatal("cannot resolve isolated official CLI executable")
	}
	identity := nativeCaptureIdentity(t, absolute, "official_npm_release")
	if identity.Version != "codex-cli "+clientVersion {
		t.Fatal("native CLI fixture requires the source-pinned official release")
	}
	helperBytes, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal("cannot hash selected native HTTP test helper")
	}
	helperSum := sha256.Sum256(helperBytes)
	var gateway *Proxy
	guard := &nativeCLIFallbackGuard{}
	result := nativeCaptureRun(t, absolute, identity, "custom_ca_via_native_http_uds_gateway_us_env", true, true, true, func(p *Proxy, caFile string) {
		t.Setenv("CODEX_CA_CERTIFICATE", caFile)
		realGatewayNativePool(t, p)
		gateway = p
		// This is deliberately not a working Go TLS transport. A successful
		// fixture must cross the Rust boundary, not silently use the old path.
		p.Client.Transport = guard
	})
	result.TransportObserved = "official CLI to Go gateway over loopback HTTP; native Go adapter to Rust helper over Unix-domain IPC; custom-CA rustls HTTP/2 to loopback TLS upstream"
	checks := map[string]bool{
		"native_helper_configured":      gateway != nil && gateway.NativeHTTP != nil,
		"controlled_cli_completed":      result.Passed && result.ExitCode == 0 && result.NativeApplicationReply,
		"actual_tool_output_returned":   result.ToolOutputObserved,
		"two_upstream_requests_only":    len(result.Requests) == 2,
		"tls_client_hello_observed":     len(result.Hellos) > 0,
		"no_go_upstream_fallback":       guard.calls.Load() == 0,
		"custom_ca_explicit":            result.CustomCA,
		"child_only_us_locale":          result.ChildLocaleOverrides["LANG"] == "en_US.UTF-8" && result.ChildLocaleOverrides["TZ"] == "America/New_York",
		"native_application_headers":    len(result.Requests) == 2,
		"native_request_schema":         len(result.Requests) == 2,
		"tls13_and_h2_negotiated":       len(result.Requests) == 2,
		"tool_result_in_followup_input": len(result.Requests) == 2 && result.Requests[1].InputTypes["function_call_output"] > 0,
		"rustls_h2_offered":             len(result.Hellos) > 0,
	}
	for _, request := range result.Requests {
		headers := request.Headers
		checks["native_application_headers"] = checks["native_application_headers"] &&
			len(headers["user-agent"]) == 1 && strings.HasPrefix(headers["user-agent"][0], "codex_exec/"+clientVersion+" ") &&
			slices.Equal(headers["originator"], []string{"codex_exec"}) && slices.Equal(headers["accept"], []string{"text/event-stream"}) &&
			slices.Equal(headers["content-type"], []string{"application/json"}) && len(headers["version"]) == 0 && len(headers["accept-encoding"]) == 0
		checks["native_request_schema"] = checks["native_request_schema"] && request.Method == "POST" && request.Stream && !request.Store &&
			request.Cache["prompt_cache_key_present"] == true && request.IDHeaderPresence["session-id"] && request.IDHeaderPresence["thread-id"] &&
			slices.Contains(request.TopLevelKeys, "tools") && slices.Contains(request.TopLevelKeys, "input") && slices.Contains(request.TopLevelKeys, "instructions")
		checks["tls13_and_h2_negotiated"] = checks["tls13_and_h2_negotiated"] && request.TLSVersion == "TLS 1.3" && request.HTTPProtocol == "HTTP/2.0" && request.ALPN == "h2"
	}
	for _, hello := range result.Hellos {
		checks["rustls_h2_offered"] = checks["rustls_h2_offered"] && hello.ParseError == "" && slices.Contains(hello.ALPN, "h2")
	}
	if gateway != nil {
		page, readErr := gateway.Metrics.Logs(LogQuery{Size: 10})
		checks["two_gateway_usage_records"] = readErr == nil && len(page.Items) == 2
		checks["gateway_admission_released"] = gateway.inFlight("test") == 0
		checks["gateway_usage_and_observation"] = len(page.Items) == 2
		var input, output int64
		for _, record := range page.Items {
			input += record.InputTokens
			output += record.OutputTokens
			checks["gateway_usage_and_observation"] = checks["gateway_usage_and_observation"] && record.Status == "ok" && record.RequestType == "http_sse" &&
				record.UpstreamHeaderMs != nil && record.TTFTMs != nil && record.UsageKnown != nil && *record.UsageKnown
		}
		checks["synthetic_usage_totals_unchanged"] = input == 50 && output == 2
	}
	for name, passed := range checks {
		if !passed {
			t.Errorf("native CLI loopback assertion failed: %s", name)
		}
	}
	report := map[string]any{
		"schema": "native-http-actual-cli-loopback-v1", "passed": !t.Failed(),
		"host_os": runtime.GOOS, "host_arch": runtime.GOARCH,
		"completed_at":   time.Now().UTC().Format(time.RFC3339),
		"execution_path": "official_cli -> go_gateway_native_http -> rust_helper_uds -> loopback_tls_upstream",
		"helper_sha256":  hex.EncodeToString(helperSum[:]), "scenario": result, "assertions": checks,
		"live_model_inference": false, "real_account_credentials": false,
		"limits": []string{
			"Official CLI really executes the requested available fixture tool and returns function_call_output; loopback SSE responses are synthetic, not model inference.",
			"Custom CA explicitly selects rustls; this does not verify default native certificate trust, production SNI, proxies, TLS resumption or all HTTP/2 settings.",
			"The inbound CLI hop is loopback HTTP. The TLS ClientHello and negotiated protocol here belong to the Rust helper's outbound connection, not the CLI itself.",
			"LANG/LC_ALL/TZ are isolated child variables, not evidence of a US device or upstream exit location.",
			"The custom API-key provider has no version header; builtin ChatGPT provider behavior is a separate source-backed case.",
			"No default Go outbound fallback is allowed by this fixture. WebSocket remains outside this HTTP-only acceptance.",
		},
	}
	if path := os.Getenv("CODEX_NATIVE_HTTP_CLI_REPORT"); path != "" {
		encoded, encodeErr := json.MarshalIndent(report, "", "  ")
		if encodeErr != nil || os.WriteFile(path, append(encoded, '\n'), 0600) != nil {
			t.Fatal("cannot write sanitized native CLI report")
		}
	}
	t.Logf("native CLI loopback: passed=%t requests=%d tool_output=%t go_fallback_attempts=%d", !t.Failed(), len(result.Requests), result.ToolOutputObserved, guard.calls.Load())
}
