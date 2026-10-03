package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

type nativeCompressionSample struct {
	ContentEncoding       string                     `json:"content_encoding"`
	WireBytes             int                        `json:"wire_bytes"`
	DecodedBytes          int                        `json:"decoded_bytes"`
	WireSHA256            string                     `json:"wire_sha256"`
	DecodedSHA256         string                     `json:"decoded_sha256"`
	SemanticSHA256        string                     `json:"semantic_json_sha256"`
	SchemaKeys            []string                   `json:"schema_keys"`
	InputTypes            map[string]int             `json:"input_type_counts"`
	ToolTypes             map[string]int             `json:"tool_type_counts"`
	AuthorizationPresent  bool                       `json:"authorization_present"`
	AccountHeaderPresent  bool                       `json:"account_header_present"`
	InstructionsPresent   bool                       `json:"instructions_present"`
	PromptCacheKeyPresent bool                       `json:"prompt_cache_key_present"`
	NativeUserAgent       bool                       `json:"native_user_agent"`
	JSONShape             nativeCompressionJSONShape `json:"json_shape"`
}

type nativeCompressionCase struct {
	Name                  string                     `json:"name"`
	ChatGPTAuth           bool                       `json:"synthetic_chatgpt_auth"`
	ProviderIsOpenAI      bool                       `json:"provider_name_is_openai"`
	FeatureEnabled        bool                       `json:"compression_feature_enabled"`
	ExpectedZstd          bool                       `json:"expected_zstd"`
	Passed                bool                       `json:"passed"`
	ExitCode              int                        `json:"cli_exit_code"`
	ElapsedSeconds        float64                    `json:"elapsed_seconds"`
	ToolOutput            bool                       `json:"tool_output_observed"`
	FinalReply            bool                       `json:"final_fixture_reply_observed"`
	Failure               string                     `json:"failure,omitempty"`
	Requests              []nativeCompressionSample  `json:"requests"`
	GatewayInbound        []nativeCompressionInbound `json:"gateway_inbound,omitempty"`
	GatewayInputPreserved *bool                      `json:"gateway_input_semantics_preserved,omitempty"`
	PreparationDeltas     []nativeCompressionDelta   `json:"preparation_deltas,omitempty"`
}

type nativeCompressionInbound struct {
	ContentEncoding string                     `json:"content_encoding"`
	WireBytes       int                        `json:"wire_bytes"`
	WireSHA256      string                     `json:"wire_sha256"`
	SemanticSHA256  string                     `json:"semantic_json_sha256"`
	JSONShape       nativeCompressionJSONShape `json:"json_shape"`
}

// Diagnostics for the serialization boundary. Only per-field digests and
// lexical counts persist, not official prompts, auth or arbitrary field values.
type nativeCompressionFieldShape struct {
	RawSHA256      string `json:"raw_sha256"`
	SemanticSHA256 string `json:"semantic_sha256"`
}
type nativeCompressionJSONShape struct {
	DecodedBytes          int                                    `json:"decoded_bytes"`
	ContentTreeSHA256     string                                 `json:"content_tree_sha256"`
	RawHTMLCharacters     int                                    `json:"raw_html_characters"`
	EscapedHTMLCharacters int                                    `json:"escaped_html_characters"`
	Fields                map[string]nativeCompressionFieldShape `json:"fields"`
}
type nativeCompressionDelta struct {
	DecodedByteDelta           int      `json:"decoded_byte_delta"`
	ContentTreePreserved       bool     `json:"content_tree_preserved"`
	SemanticFieldsChanged      []string `json:"semantic_fields_changed"`
	SerializationFieldsChanged []string `json:"serialization_fields_changed"`
	MissingFields              []string `json:"missing_fields"`
	ExtraFields                []string `json:"extra_fields"`
}

func nativeCompressionShape(raw []byte) nativeCompressionJSONShape {
	shape := nativeCompressionJSONShape{DecodedBytes: len(raw), Fields: make(map[string]nativeCompressionFieldShape)}
	if tree, err := researchBodyCanonical(raw); err == nil {
		shape.ContentTreeSHA256 = researchBodyHash(tree)
	}
	for _, char := range []byte{'<', '>', '&'} {
		shape.RawHTMLCharacters += bytes.Count(raw, []byte{char})
	}
	for _, escaped := range []string{`\u003c`, `\u003e`, `\u0026`} {
		shape.EscapedHTMLCharacters += bytes.Count(bytes.ToLower(raw), []byte(escaped))
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return shape
	}
	for name, value := range fields {
		hash := sha256.Sum256(value)
		tree, err := researchBodyCanonical(value)
		var digest string
		if err == nil {
			digest = researchBodyHash(tree)
		}
		shape.Fields[name] = nativeCompressionFieldShape{hex.EncodeToString(hash[:]), digest}
	}
	return shape
}

func nativeCompressionCompareShape(before, after nativeCompressionJSONShape) nativeCompressionDelta {
	delta := nativeCompressionDelta{DecodedByteDelta: after.DecodedBytes - before.DecodedBytes, SemanticFieldsChanged: []string{}, SerializationFieldsChanged: []string{}, MissingFields: []string{}, ExtraFields: []string{}}
	delta.ContentTreePreserved = before.ContentTreeSHA256 != "" && before.ContentTreeSHA256 == after.ContentTreeSHA256
	for name, old := range before.Fields {
		current, exists := after.Fields[name]
		if !exists {
			delta.MissingFields = append(delta.MissingFields, name)
			continue
		}
		if old.SemanticSHA256 == "" || old.SemanticSHA256 != current.SemanticSHA256 {
			delta.SemanticFieldsChanged = append(delta.SemanticFieldsChanged, name)
		} else if old.RawSHA256 != current.RawSHA256 {
			delta.SerializationFieldsChanged = append(delta.SerializationFieldsChanged, name)
		}
	}
	for name := range after.Fields {
		if _, exists := before.Fields[name]; !exists {
			delta.ExtraFields = append(delta.ExtraFields, name)
		}
	}
	sort.Strings(delta.SemanticFieldsChanged)
	sort.Strings(delta.SerializationFieldsChanged)
	sort.Strings(delta.MissingFields)
	sort.Strings(delta.ExtraFields)
	return delta
}

// Only the network-none runner may opt in. All auth files are generated inside
// this test's temporary home; no desktop/user Codex credentials are read.
func TestSourceNativeCLIRequestCompression(t *testing.T) {
	requireNativeCompressionNetworkNone(t)
	bin := os.Getenv("CODEX_NATIVE_CAPTURE_BIN")
	identity := nativeCaptureIdentity(t, bin, "official_npm_release")
	if identity.Version != "codex-cli 0.155.0" {
		t.Fatal("compression fixture is source-pinned to official CLI 0.155.0")
	}
	cases := []nativeCompressionCase{}
	for _, config := range []struct {
		name                     string
		chatgpt, openai, enabled bool
	}{
		{"chatgpt_openai_enabled", true, true, true},
		{"chatgpt_openai_disabled", true, true, false},
		{"apikey_openai_enabled", false, true, true},
		{"chatgpt_custom_name_enabled", true, false, true},
	} {
		t.Run(config.name, func(t *testing.T) {
			result := runNativeCompressionFixture(t, bin, config.name, config.chatgpt, config.openai, config.enabled)
			cases = append(cases, result)
			if !result.Passed {
				t.Errorf("isolated CLI compression fixture failed: %s exit=%d requests=%d", result.Failure, result.ExitCode, len(result.Requests))
			}
		})
	}
	report := map[string]any{"schema": "official-cli-synthetic-auth-compression-v1", "passed": !t.Failed(), "client": identity,
		"network": "none", "live_accounts": false, "live_model_inference": false, "desktop_auth_read": false, "cases": cases,
		"completed_at": time.Now().UTC().Format(time.RFC3339),
		"source":       []string{"core/src/client.rs:1450 requires feature + uses_codex_backend + provider.is_openai", "model-provider-info/src/lib.rs:526 is_openai compares provider name OpenAI", "features/src/lib.rs:1231 enable_request_compression defaults true", "http-client/src/request.rs zstd level 3 prepared body"},
		"limits":       []string{"Synthetic auth.json exercises the official local ChatGPT-auth code path, not successful remote OAuth authentication.", "All upstream responses are deterministic loopback fixtures; the official CLI actually executes a tool and returns its result.", "This initial capture is direct CLI to loopback HTTP, not TLS, native helper or production gateway acceptance.", "Only sizes, body digests, schema/type counts and header-presence booleans persist; no auth, prompt, tool arguments or output is retained."}}
	if path := os.Getenv("CODEX_NATIVE_COMPRESSION_REPORT"); path != "" {
		raw, err := json.MarshalIndent(report, "", "  ")
		if err != nil || os.WriteFile(path, append(raw, '\n'), 0600) != nil {
			t.Fatal("cannot write sanitized compression evidence")
		}
	}
}

func requireNativeCompressionNetworkNone(t *testing.T) {
	t.Helper()
	if os.Getenv("CODEX_NATIVE_CAPTURE_BIN") == "" || os.Getenv("CODEX_NATIVE_COMPRESSION_NETWORK_NONE") != "1" || runtime.GOOS != "linux" {
		t.Skip("run with explicit CLI binary inside the network-none compression fixture")
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal("cannot establish network isolation")
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagLoopback == 0 {
			t.Fatal("compression fixture requires loopback-only network namespace")
		}
	}
}

func nativeCompressionJSONDigest(raw []byte) string {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return ""
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}

func nativeCompressionDecode(raw []byte, encoding string) ([]byte, error) {
	if encoding == "" {
		return raw, nil
	}
	if encoding != "zstd" {
		return nil, fmt.Errorf("unsupported synthetic fixture encoding")
	}
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(2*maxBody))
	if err != nil {
		return nil, err
	}
	defer decoder.Close()
	decoded, err := decoder.DecodeAll(raw, nil)
	if err != nil || len(decoded) > maxBody {
		return nil, fmt.Errorf("invalid synthetic fixture zstd")
	}
	return decoded, nil
}

func nativeCompressionSyntheticAuth(t *testing.T, chatgpt bool) []byte {
	t.Helper()
	if !chatgpt {
		return []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"synthetic-local-api-key-not-real"}`)
	}
	claims, err := json.Marshal(map[string]any{"sub": "synthetic-local-user", "email": "fixture@example.invalid", "exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{"chatgpt_user_id": "synthetic-local-user", "chatgpt_account_id": "synthetic-local-workspace", "chatgpt_plan_type": "pro"}})
	if err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(claims) + ".synthetic-fixture-signature"
	auth, err := json.Marshal(map[string]any{"auth_mode": "chatgpt", "OPENAI_API_KEY": nil, "tokens": map[string]string{"id_token": token, "access_token": token, "refresh_token": "synthetic-local-refresh-not-real", "account_id": "synthetic-local-workspace"}, "last_refresh": time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	return auth
}

func runNativeCompressionFixture(t *testing.T, bin, name string, chatgpt, openai, enabled bool, configureGateway ...func(*Proxy)) nativeCompressionCase {
	t.Helper()
	result := nativeCompressionCase{Name: name, ChatGPTAuth: chatgpt, ProviderIsOpenAI: openai, FeatureEnabled: enabled, ExpectedZstd: chatgpt && openai && enabled, ExitCode: -1, Requests: []nativeCompressionSample{}}
	var mu sync.Mutex
	const final = "SYNTHETIC_COMPRESSION_TOOL_COMPLETE"
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"models":[]}`)
			return
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/responses") {
			http.NotFound(w, r)
			return
		}
		wire, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil || len(wire) > maxBody {
			http.Error(w, "fixture body limit", 400)
			return
		}
		encoding := r.Header.Get("Content-Encoding")
		decoded, err := nativeCompressionDecode(wire, encoding)
		if err != nil {
			http.Error(w, "fixture encoding", 400)
			return
		}
		var payload map[string]any
		if json.Unmarshal(decoded, &payload) != nil {
			http.Error(w, "fixture invalid JSON", 400)
			return
		}
		wireHash, decodedHash := sha256.Sum256(wire), sha256.Sum256(decoded)
		base := nativeCaptureSanitizeRequest(r, payload)
		_, instructions := payload["instructions"]
		cache, _ := payload["prompt_cache_key"].(string)
		sample := nativeCompressionSample{ContentEncoding: encoding, WireBytes: len(wire), DecodedBytes: len(decoded), WireSHA256: hex.EncodeToString(wireHash[:]), DecodedSHA256: hex.EncodeToString(decodedHash[:]), SchemaKeys: base.TopLevelKeys, InputTypes: base.InputTypes, ToolTypes: base.ToolTypes,
			SemanticSHA256:       nativeCompressionJSONDigest(decoded),
			AuthorizationPresent: r.Header.Get("Authorization") != "", AccountHeaderPresent: r.Header.Get("ChatGPT-Account-Id") != "", InstructionsPresent: instructions, PromptCacheKeyPresent: cache != "", NativeUserAgent: strings.HasPrefix(r.UserAgent(), "codex_exec/0.155.0 ")}
		sample.JSONShape = nativeCompressionShape(decoded)
		mu.Lock()
		result.Requests = append(result.Requests, sample)
		n := len(result.Requests)
		for _, raw := range realCLIArray(payload["input"]) {
			item, _ := raw.(map[string]any)
			if item["type"] == "function_call_output" && item["call_id"] == "compression_fixture_tool" {
				result.ToolOutput = true
			}
		}
		mu.Unlock()
		if n == 1 {
			namespace, found := nativeCaptureFindTool(realCLIArray(payload["tools"]), "", "update_plan")
			tool, args := "update_plan", `{"plan":[{"step":"Complete isolated compression fixture","status":"completed"}]}`
			if !found {
				tool, namespace = realCLIShellTool(realCLIArray(payload["tools"]), "")
				switch tool {
				case "exec_command":
					args = `{"cmd":"echo SYNTHETIC_COMPRESSION_TOOL_OK","login":false,"max_output_tokens":100}`
				case "shell_command":
					args = `{"command":"echo SYNTHETIC_COMPRESSION_TOOL_OK","login":false}`
				default:
					http.Error(w, "fixture tool unavailable", 400)
					return
				}
			}
			item := map[string]any{"type": "function_call", "id": "compression_fixture_item", "call_id": "compression_fixture_tool", "name": tool, "arguments": args}
			if namespace != "" {
				item["namespace"] = namespace
			}
			realCLIWriteSSE(w, "compression_fixture_1", item, 20)
			return
		}
		realCLIWriteSSE(w, "compression_fixture_2", realCLIMessage("compression_fixture_final", final), 30)
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.Start()
	defer server.Close()
	home, workspace := t.TempDir(), t.TempDir()
	auth := nativeCompressionSyntheticAuth(t, chatgpt)
	providerURL := server.URL + "/backend-api/codex/v1"
	if len(configureGateway) > 0 {
		p := newTestProxy(t, 3)
		p.BaseURL = server.URL
		var fixtureAuth struct {
			APIKey string `json:"OPENAI_API_KEY"`
			Tokens struct {
				AccessToken string `json:"access_token"`
			} `json:"tokens"`
		}
		if json.Unmarshal(auth, &fixtureAuth) != nil {
			t.Fatal("cannot decode generated fixture auth")
		}
		key := fixtureAuth.APIKey
		if chatgpt {
			key = fixtureAuth.Tokens.AccessToken
		}
		p.setAdminKey(key)
		for _, configure := range configureGateway {
			configure(p)
		}
		gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/responses") {
				wire, readErr := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
				_ = r.Body.Close()
				decoded, decodeErr := nativeCompressionDecode(wire, r.Header.Get("Content-Encoding"))
				if readErr != nil || decodeErr != nil || len(wire) > maxBody {
					http.Error(w, "fixture capture body", 400)
					return
				}
				hash := sha256.Sum256(wire)
				mu.Lock()
				result.GatewayInbound = append(result.GatewayInbound, nativeCompressionInbound{r.Header.Get("Content-Encoding"), len(wire), hex.EncodeToString(hash[:]), nativeCompressionJSONDigest(decoded), nativeCompressionShape(decoded)})
				mu.Unlock()
				r.Body = io.NopCloser(bytes.NewReader(wire))
			}
			p.ServeHTTP(w, r)
		}))
		defer gateway.Close()
		providerURL = gateway.URL + "/v1"
	}
	providerName := "Local Synthetic Compression"
	if openai {
		providerName = "OpenAI"
	}
	config := fmt.Sprintf(`model = "gpt-5.4"
model_provider = "synthetic_compression"
approval_policy = "never"
sandbox_mode = "read-only"
web_search = "disabled"
cli_auth_credentials_store = "file"
chatgpt_base_url = %q
[features]
enable_request_compression = %t
[model_providers.synthetic_compression]
name = %q
base_url = %q
wire_api = "responses"
requires_openai_auth = true
supports_websockets = false
supports_standalone_web_search = false
request_max_retries = 0
stream_max_retries = 0
stream_idle_timeout_ms = 15000
`, server.URL, enabled, providerName, providerURL)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal("cannot write synthetic fixture config")
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), auth, 0600); err != nil {
		t.Fatal("cannot write synthetic auth fixture")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "exec", "--skip-git-repo-check", "--ignore-rules", "--json", "Complete the isolated compression fixture using the requested plan tool.")
	cmd.Dir = workspace
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CODEX_HOME=" + home, "NO_PROXY=*", "no_proxy=*", "NO_COLOR=1", "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8", "TZ=America/New_York"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	started := time.Now()
	runErr := cmd.Run()
	result.ElapsedSeconds = time.Since(started).Seconds()
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	result.FinalReply = strings.Contains(stdout.String(), final)
	server.Close()
	mu.Lock()
	defer mu.Unlock()
	result.Passed = runErr == nil && result.FinalReply && result.ToolOutput && len(result.Requests) == 2
	for _, request := range result.Requests {
		if (request.ContentEncoding == "zstd") != result.ExpectedZstd || !request.AuthorizationPresent || !request.InstructionsPresent || !request.PromptCacheKeyPresent || !request.NativeUserAgent || request.DecodedBytes == 0 {
			result.Passed = false
		}
		if chatgpt && !request.AccountHeaderPresent {
			result.Passed = false
		}
	}
	if len(configureGateway) > 0 {
		preserved := len(result.GatewayInbound) == 2 && len(result.Requests) == 2
		if preserved {
			for index, inbound := range result.GatewayInbound {
				preserved = preserved && inbound.SemanticSHA256 != "" && inbound.SemanticSHA256 == result.Requests[index].SemanticSHA256
				delta := nativeCompressionCompareShape(inbound.JSONShape, result.Requests[index].JSONShape)
				result.PreparationDeltas = append(result.PreparationDeltas, delta)
				// This fixture disables additive policies and uses canonical CLI
				// JSON. Request preparation must not inflate its HTML/source text.
				preserved = preserved && delta.ContentTreePreserved && delta.DecodedByteDelta == 0 && len(delta.SemanticFieldsChanged) == 0 && len(delta.MissingFields) == 0 && len(delta.ExtraFields) == 0
			}
		}
		result.GatewayInputPreserved = &preserved
		result.Passed = result.Passed && preserved
	}
	if !result.Passed {
		result.Failure = "compression_or_tool_roundtrip_mismatch"
		if ctx.Err() != nil {
			result.Failure = "isolated_cli_timeout"
		}
	}
	t.Logf("synthetic compression: case=%s exit=%d requests=%d tool_output=%t passed=%t", name, result.ExitCode, len(result.Requests), result.ToolOutput, result.Passed)
	return result
}
