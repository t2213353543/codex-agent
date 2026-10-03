package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// The real official CLI uses only a freshly generated synthetic ChatGPT auth
// file. Network isolation is mandatory even though every target is loopback.
func TestGatewayNativeActualOAuthCompressedCLI(t *testing.T) {
	requireNativeCompressionNetworkNone(t)
	helper := os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN")
	if helper == "" {
		t.Skip("set the explicit Rust helper for native compression chain")
	}
	bin := os.Getenv("CODEX_NATIVE_CAPTURE_BIN")
	identity := nativeCaptureIdentity(t, bin, "official_npm_release")
	if identity.Version != "codex-cli 0.155.0" {
		t.Fatal("compression chain requires source-pinned CLI 0.155.0")
	}
	helperRaw, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal("cannot hash selected compression helper")
	}
	helperHash := sha256.Sum256(helperRaw)
	guard := &nativeCLIFallbackGuard{}
	var gateway *Proxy
	result := runNativeCompressionFixture(t, bin, "chatgpt_openai_enabled_via_native_gateway", true, true, true, func(p *Proxy) {
		settings, err := p.Settings.Get()
		if err != nil {
			t.Fatal(err)
		}
		settings.BuiltinPromptEnabled = false
		settings.CompatibilityNormalizationEnabled = false
		if err := p.Settings.Set(settings); err != nil {
			t.Fatal(err)
		}
		realGatewayNativePool(t, p)
		p.Client.Transport = guard
		gateway = p
	})
	checks := map[string]bool{
		"official_cli_completed":            result.Passed && result.ExitCode == 0 && result.FinalReply,
		"actual_tool_output_returned":       result.ToolOutput,
		"two_inbound_and_outbound_requests": len(result.GatewayInbound) == 2 && len(result.Requests) == 2,
		"original_json_semantics_preserved": result.GatewayInputPreserved != nil && *result.GatewayInputPreserved,
		"native_helper_configured":          gateway != nil && gateway.NativeHTTP != nil,
		"no_go_upstream_fallback":           guard.calls.Load() == 0,
		"inbound_official_zstd":             len(result.GatewayInbound) == 2,
		"outbound_native_zstd":              len(result.Requests) == 2,
	}
	for _, inbound := range result.GatewayInbound {
		checks["inbound_official_zstd"] = checks["inbound_official_zstd"] && inbound.ContentEncoding == "zstd"
	}
	for _, outbound := range result.Requests {
		checks["outbound_native_zstd"] = checks["outbound_native_zstd"] && outbound.ContentEncoding == "zstd" && outbound.WireBytes < outbound.DecodedBytes
	}
	if gateway != nil {
		rows, err := gateway.Metrics.Logs(LogQuery{Size: 10})
		var inputs, outputs int64
		responses := 0
		checks["gateway_completed_and_admission_released"] = gateway.inFlight("test") == 0
		for _, row := range rows.Items {
			// ChatGPT-mode CLI may also refresh its model catalog. That is a
			// separate GET, not an additional model turn or duplicated inference.
			if row.Endpoint != "responses" {
				continue
			}
			responses++
			inputs += row.InputTokens
			outputs += row.OutputTokens
			checks["gateway_completed_and_admission_released"] = checks["gateway_completed_and_admission_released"] && row.Status == "ok" && row.RequestType == "http_sse" && row.UsageKnown != nil && *row.UsageKnown
		}
		checks["two_gateway_response_usage_records"] = err == nil && responses == 2
		checks["usage_unchanged"] = inputs == 50 && outputs == 2
	}
	for name, pass := range checks {
		if !pass {
			t.Errorf("synthetic compressed native CLI assertion failed: %s", name)
		}
	}
	report := map[string]any{"schema": "native-compressed-oauth-cli-loopback-v1", "passed": !t.Failed(), "client": identity, "helper_sha256": hex.EncodeToString(helperHash[:]),
		"execution_path": "official_cli_synthetic_chatgpt -> go_gateway -> rust_uds_helper -> loopback_http_upstream",
		"network":        "none", "live_accounts": false, "live_model_inference": false, "desktop_auth_read": false, "completed_at": time.Now().UTC().Format(time.RFC3339), "scenario": result, "assertions": checks,
		"limits": []string{"Synthetic auth triggers the official compression code path; this is not remote authentication or model inference.", "Loopback upstream emits a deterministic plan/tool request and verifies actual official CLI tool-result follow-up.", "Optional prompt injection and compatibility normalization are disabled only in this fixture to compare full structured JSON semantics.", "Compressed byte identity is not required after gateway preparation; structured JSON including instructions/input/tools/cache metadata must remain equal.", "This chain uses loopback HTTP on both external hops and UDS internally; it is not additional TLS, proxy or real-account evidence."}}
	if path := os.Getenv("CODEX_NATIVE_COMPRESSION_CHAIN_REPORT"); path != "" {
		raw, err := json.MarshalIndent(report, "", "  ")
		if err != nil || os.WriteFile(path, append(raw, '\n'), 0600) != nil {
			t.Fatal("cannot write sanitized native compression chain report")
		}
	}
}
