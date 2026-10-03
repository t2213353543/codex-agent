package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSourceNativeCLITransport starts only explicitly selected fresh CLI
// processes and a loopback capture server. It never attaches to a live desktop
// session. Captures contain whitelisted protocol metadata, not request bodies,
// bearer values, IDs, filesystem paths, account data or TLS key material.
func TestSourceNativeCLITransport(t *testing.T) {
	bin := os.Getenv("CODEX_NATIVE_CAPTURE_BIN")
	if bin == "" {
		t.Skip("set CODEX_NATIVE_CAPTURE_BIN to opt into isolated official CLI transport capture")
	}
	report := nativeCaptureReport{
		SchemaVersion: 1, ControlledUpstream: true, LiveModelInference: false,
		HostOS: runtime.GOOS, HostArch: runtime.GOARCH, GoVersion: runtime.Version(),
		CompletedAt: "", Scenarios: []nativeCaptureScenario{},
		SourceFindings: []string{
			"0.154.0 http-client/src/custom_ca.rs:168: CODEX_CA_CERTIFICATE takes precedence over SSL_CERT_FILE.",
			"0.154.0 http-client/src/custom_ca.rs:307: a custom CA explicitly selects rustls; successful custom-CA captures are not the default Windows native TLS stack.",
			"0.154.0 http-client/src/tls_backend_fallback.rs:1: native TLS remains default; recognized negotiation failures may select rustls for one destination.",
		},
		Limits: []string{
			"Only new CLI exec processes were observed; no existing desktop conversation, process environment, command line or account was captured.",
			"Servers listen only on loopback. No public model endpoint, production gateway, geolocation service or proxy exit was used.",
			"LANG/LC_ALL=en_US.UTF-8 and TZ=America/New_York are child process variables only; they do not change Windows regional settings, IP location, native device identity or establish a US device.",
			"The default-trust handshake-only probe is terminated after its first ClientHello to avoid trust failures and retry delays. It measures offered metadata, not a completed native TLS connection or HTTP headers.",
			"Custom-CA successful TLS requests exercise the official client's rustls path. The custom API-key provider does not exercise ChatGPT OAuth, authenticated Zstd request compression, production routing or WebSocket transport.",
			"TLS extension order is one observed sample per connection and may be randomized; no claim of a stable device fingerprint or byte-for-byte production fingerprint is made.",
			"The target is a loopback IP rather than a production hostname; SNI and server negotiation can therefore differ. HTTP header names are canonicalized, and raw wire header order is not captured.",
			"Only the first ClientHello per TCP connection is decoded; later retries, HelloRetryRequest responses, HTTP/2 frame settings and TCP/IP packet fingerprints are not captured.",
			"Raw TLS bytes are transient parser input only. Raw HTTP bodies, authorization, cookies, account IDs, session IDs, prompts and filesystem paths are not persisted or printed.",
		},
	}
	clients := []struct{ origin, path string }{{"official_npm_release", bin}}
	if desktop := os.Getenv("CODEX_NATIVE_DESKTOP_BIN"); desktop != "" {
		clients = append(clients, struct{ origin, path string }{"desktop_bundled_binary_fresh_exec", desktop})
	}
	for _, client := range clients {
		absolute, err := filepath.Abs(client.path)
		if err != nil {
			t.Fatal("cannot resolve selected CLI executable")
		}
		identity := nativeCaptureIdentity(t, absolute, client.origin)
		for _, probe := range []struct {
			name       string
			customCA   bool
			usEnv      bool
			viaGateway bool
		}{
			{"default_trust_client_hello", false, false, false},
			{"plain_http_direct_us_env", false, true, false},
			{"custom_ca_direct_default_env", true, false, false},
			{"custom_ca_direct_us_env", true, true, false},
			{"custom_ca_via_go_gateway_us_env", true, true, true},
		} {
			// The gateway's Go stack is independent of which binary generated
			// the request; one release-binary forwarding comparison is enough.
			if client.origin != "official_npm_release" && probe.viaGateway {
				continue
			}
			t.Run(client.origin+"/"+probe.name, func(t *testing.T) {
				result := nativeCaptureRun(t, absolute, identity, probe.name, probe.customCA, probe.usEnv, probe.viaGateway)
				report.Scenarios = append(report.Scenarios, result)
				if !result.Passed {
					t.Errorf("isolated capture failed: %s (exit=%d, requests=%d, hellos=%d)", result.Failure, result.ExitCode, len(result.Requests), len(result.Hellos))
				}
			})
		}
	}
	report.Passed = !t.Failed()
	report.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	report.Comparisons = nativeCaptureCompare(report.Scenarios)
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal("cannot serialize sanitized capture report")
	}
	if reportFile := os.Getenv("CODEX_NATIVE_CAPTURE_REPORT"); reportFile != "" {
		if err := os.WriteFile(reportFile, append(encoded, '\n'), 0600); err != nil {
			t.Fatal("cannot write sanitized capture report")
		}
	}
	t.Logf("sanitized native capture: scenarios=%d passed=%t", len(report.Scenarios), report.Passed)
}

type nativeCaptureClient struct {
	Origin  string `json:"origin"`
	Version string `json:"version"`
	SHA256  string `json:"binary_sha256"`
}

type nativeCaptureReport struct {
	SchemaVersion      int                     `json:"schema_version"`
	Passed             bool                    `json:"passed"`
	ControlledUpstream bool                    `json:"controlled_upstream"`
	LiveModelInference bool                    `json:"live_model_inference"`
	HostOS             string                  `json:"host_os"`
	HostArch           string                  `json:"host_arch"`
	GoVersion          string                  `json:"go_version"`
	CompletedAt        string                  `json:"completed_at"`
	Scenarios          []nativeCaptureScenario `json:"scenarios"`
	Comparisons        map[string]any          `json:"comparisons"`
	SourceFindings     []string                `json:"source_findings"`
	Limits             []string                `json:"limits"`
}

type nativeCaptureScenario struct {
	Name                   string                 `json:"name"`
	Client                 nativeCaptureClient    `json:"client"`
	Passed                 bool                   `json:"passed"`
	ExitCode               int                    `json:"cli_exit_code"`
	Failure                string                 `json:"failure,omitempty"`
	ElapsedSeconds         float64                `json:"elapsed_seconds"`
	CustomCA               bool                   `json:"ssl_cert_file_in_child"`
	ChildLocaleOverrides   map[string]string      `json:"child_locale_overrides"`
	TransportObserved      string                 `json:"transport_observed"`
	HTTPClientToGateway    string                 `json:"cli_to_gateway_protocol,omitempty"`
	GatewayIdentityPolicy  string                 `json:"gateway_identity_policy,omitempty"`
	ToolOutputObserved     bool                   `json:"function_call_output_observed"`
	ProbeStoppedAfterHello bool                   `json:"probe_stopped_after_client_hello"`
	NativeApplicationReply bool                   `json:"cli_completed_fixture_reply"`
	Hellos                 []nativeCaptureHello   `json:"client_hellos"`
	Requests               []nativeCaptureRequest `json:"requests"`
}

type nativeCaptureHello struct {
	LegacyVersion         string   `json:"legacy_version"`
	CipherSuites          []string `json:"cipher_suites_in_wire_order"`
	Extensions            []int    `json:"extension_ids_in_wire_order"`
	SupportedVersions     []string `json:"supported_versions"`
	SupportedGroups       []int    `json:"supported_group_ids"`
	SignatureAlgorithms   []string `json:"signature_algorithm_ids"`
	ALPN                  []string `json:"alpn_offered"`
	ServerNamePresent     bool     `json:"server_name_present"`
	ClientHelloByteLength int      `json:"client_hello_bytes"`
	ParseError            string   `json:"parse_error,omitempty"`
}

type nativeCaptureRequest struct {
	Method             string              `json:"method"`
	HTTPProtocol       string              `json:"http_protocol"`
	TLSVersion         string              `json:"negotiated_tls_version"`
	TLSCipher          string              `json:"negotiated_tls_cipher"`
	ALPN               string              `json:"negotiated_alpn"`
	Headers            map[string][]string `json:"whitelisted_application_headers"`
	IDHeaderPresence   map[string]bool     `json:"identity_header_presence_only"`
	TopLevelKeys       []string            `json:"request_schema_keys"`
	ClientMetadataKeys []string            `json:"client_metadata_keys_only"`
	InputTypes         map[string]int      `json:"input_type_counts"`
	ToolTypes          map[string]int      `json:"tool_type_counts"`
	Cache              map[string]any      `json:"cache_options_redacted"`
	Stream             bool                `json:"stream"`
	Store              bool                `json:"store"`
	TurnMetadataKeys   []string            `json:"turn_metadata_keys_only,omitempty"`
}

func nativeCaptureIdentity(t *testing.T, bin, origin string) nativeCaptureClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	version, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		t.Fatal("cannot read selected CLI version")
	}
	text := strings.TrimSpace(string(version))
	if !strings.HasPrefix(text, "codex-cli ") || len(text) > 100 || strings.ContainsAny(text, "\r\n/\\") {
		t.Fatal("selected executable did not identify as codex-cli")
	}
	f, err := os.Open(bin)
	if err != nil {
		t.Fatal("cannot hash selected CLI")
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		t.Fatal("cannot hash selected CLI")
	}
	return nativeCaptureClient{Origin: origin, Version: text, SHA256: hex.EncodeToString(sum.Sum(nil))}
}

func nativeCaptureRun(t *testing.T, bin string, identity nativeCaptureClient, name string, customCA, usEnv, viaGateway bool, configureGateway ...func(*Proxy, string)) nativeCaptureScenario {
	t.Helper()
	plainHTTP := name == "plain_http_direct_us_env"
	handshakeOnly := !customCA && !plainHTTP
	result := nativeCaptureScenario{
		Name: name, Client: identity, CustomCA: customCA, ExitCode: -1,
		ChildLocaleOverrides: map[string]string{}, Hellos: []nativeCaptureHello{}, Requests: []nativeCaptureRequest{},
		TransportObserved: "official CLI default trust; handshake only; intentionally terminated after first ClientHello",
	}
	if customCA {
		result.TransportObserved = "official CLI custom-CA rustls transport to local TLS server"
	}
	if plainHTTP {
		result.TransportObserved = "official CLI default configuration HTTP transport to loopback; no custom CA and no TLS"
	}
	if usEnv {
		result.ChildLocaleOverrides = map[string]string{"LANG": "en_US.UTF-8", "LC_ALL": "en_US.UTF-8", "TZ": "America/New_York"}
	}
	var mu sync.Mutex
	helloObserved := make(chan struct{})
	var helloOnce sync.Once
	cert, roots, caFile := nativeCaptureCertificate(t)
	const finalReply = "CONTROLLED_TRANSPORT_CAPTURE_COMPLETE"
	var requestCount int
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil || len(body) > maxBody {
			http.Error(w, "fixture body limit", http.StatusBadRequest)
			return
		}
		var payload map[string]any
		if json.Unmarshal(body, &payload) != nil {
			http.Error(w, "fixture invalid JSON", http.StatusBadRequest)
			return
		}
		// Do not save the raw request, even on failure. The fixture requires
		// authentication, but the credential is never included in the report.
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "fixture authentication required", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		result.Requests = append(result.Requests, nativeCaptureSanitizeRequest(r, payload))
		requestCount++
		n := requestCount
		for _, value := range realCLIArray(payload["input"]) {
			item, _ := value.(map[string]any)
			if item["type"] == "function_call_output" && item["call_id"] == "capture_plan_call" {
				result.ToolOutputObserved = true
			}
		}
		mu.Unlock()
		if n == 1 {
			ns, found := nativeCaptureFindTool(realCLIArray(payload["tools"]), "", "update_plan")
			toolName := "update_plan"
			arguments := `{"plan":[{"step":"Observe local transport metadata","status":"completed"}]}`
			if !found {
				toolName, ns = realCLIShellTool(realCLIArray(payload["tools"]), "")
				switch toolName {
				case "exec_command":
					arguments = `{"cmd":"echo CONTROLLED_CAPTURE_TOOL_OK","login":false,"max_output_tokens":100}`
				case "shell_command":
					arguments = `{"command":"echo CONTROLLED_CAPTURE_TOOL_OK","login":false}`
				default:
					mu.Lock()
					result.Failure = "fixture_supported_tool_unavailable"
					mu.Unlock()
					http.Error(w, "fixture tool unavailable", http.StatusBadRequest)
					return
				}
			}
			item := map[string]any{"type": "function_call", "id": "capture_plan_item", "call_id": "capture_plan_call", "name": toolName, "arguments": arguments}
			if ns != "" {
				item["namespace"] = ns
			}
			realCLIWriteSSE(w, "capture_response_1", item, 20)
			return
		}
		realCLIWriteSSE(w, "capture_response_2", realCLIMessage("capture_final", finalReply), 30)
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0) // TLS errors can include addresses.
	if plainHTTP {
		server.Start()
	} else {
		server.EnableHTTP2 = true
		server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		server.Listener = &nativeCaptureListener{Listener: server.Listener, onHello: func(hello nativeCaptureHello) {
			mu.Lock()
			result.Hellos = append(result.Hellos, hello)
			mu.Unlock()
			helloOnce.Do(func() { close(helloObserved) })
		}}
		server.StartTLS()
	}
	t.Cleanup(server.Close)
	providerURL := server.URL + "/v1"
	if viaGateway {
		p := newTestProxy(t, 3)
		p.BaseURL = server.URL
		// Preserve the current gateway Go transport defaults, changing only
		// its in-memory CA roots so this local self-signed fixture is trusted.
		transport := p.Client.Transport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{RootCAs: roots}
		p.Client.Transport = transport
		for _, configure := range configureGateway {
			configure(p, caFile)
		}
		gateway := httptest.NewServer(p)
		t.Cleanup(gateway.Close)
		providerURL = gateway.URL + "/v1"
		result.TransportObserved = "gateway default Go net/http TLS transport to local TLS server; in-memory fixture CA only"
		result.HTTPClientToGateway = "HTTP/1.1 over loopback without TLS"
		result.GatewayIdentityPolicy = "fixture default settings; optional fingerprint profiles disabled"
	}
	cliHome, workdir := t.TempDir(), t.TempDir()
	config := fmt.Sprintf(`model = "gpt-5.4"
model_provider = "capture_fixture"
approval_policy = "never"
sandbox_mode = "read-only"
web_search = "disabled"
[model_providers.capture_fixture]
name = "Local transport observation"
base_url = %q
env_key = "CODEX_CAPTURE_FIXTURE_KEY"
wire_api = "responses"
supports_websockets = false
request_max_retries = 0
stream_max_retries = 0
stream_idle_timeout_ms = 15000
[windows]
sandbox = "unelevated"
`, providerURL)
	if err := os.WriteFile(filepath.Join(cliHome, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal("cannot write isolated capture config")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 75*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "exec", "--skip-git-repo-check", "--ignore-rules", "--json", "Complete the local transport observation using the requested plan tool.")
	cmd.Dir = workdir
	for _, key := range []string{"PATH", "PATHEXT", "SYSTEMROOT", "WINDIR", "COMSPEC", "TEMP", "TMP", "USERPROFILE", "HOME", "APPDATA", "LOCALAPPDATA", "SHELL"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.Env = append(cmd.Env, "CODEX_HOME="+cliHome, "CODEX_CAPTURE_FIXTURE_KEY=test-client-key", "NO_PROXY=*", "no_proxy=*", "NO_COLOR=1")
	if customCA {
		cmd.Env = append(cmd.Env, "SSL_CERT_FILE="+caFile)
	}
	for key, value := range result.ChildLocaleOverrides {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	started := time.Now()
	runErr := cmd.Start()
	if runErr == nil {
		finished := make(chan error, 1)
		go func() { finished <- cmd.Wait() }()
		if handshakeOnly {
			select {
			case <-helloObserved:
				result.ProbeStoppedAfterHello = true
				cancel()
			case <-ctx.Done():
			case runErr = <-finished:
				finished = nil
			}
		}
		if finished != nil {
			runErr = <-finished
		}
	}
	result.ElapsedSeconds = time.Since(started).Seconds()
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	// Output is never printed or persisted; inspect only a fixed fixture token.
	result.NativeApplicationReply = strings.Contains(stdout.String(), finalReply)
	server.Close()
	mu.Lock()
	defer mu.Unlock()
	if handshakeOnly {
		if len(result.Hellos) > 0 && len(result.Requests) == 0 && result.ProbeStoppedAfterHello {
			result.Passed = true
		} else {
			result.Failure = "expected_stopped_handshake_without_http_request"
		}
	} else if ctx.Err() != nil {
		result.Failure = "cli_timeout"
	} else if runErr != nil || !result.NativeApplicationReply || !result.ToolOutputObserved || len(result.Requests) != 2 {
		if result.Failure == "" {
			result.Failure = "controlled_cli_tool_roundtrip_failed"
		}
	} else {
		result.Passed = true
	}
	for _, hello := range result.Hellos {
		if hello.ParseError != "" {
			result.Passed, result.Failure = false, "client_hello_parse_failed"
		}
	}
	t.Logf("%s %s: pass=%t exit=%d hellos=%d requests=%d", identity.Version, name, result.Passed, result.ExitCode, len(result.Hellos), len(result.Requests))
	return result
}

func nativeCaptureSanitizeRequest(r *http.Request, payload map[string]any) nativeCaptureRequest {
	out := nativeCaptureRequest{
		Method: r.Method, HTTPProtocol: r.Proto,
		Headers: map[string][]string{}, IDHeaderPresence: map[string]bool{},
		TopLevelKeys: nativeCaptureKeys(payload), InputTypes: map[string]int{}, ToolTypes: map[string]int{}, Cache: map[string]any{},
	}
	if r.TLS != nil {
		out.TLSVersion = tls.VersionName(r.TLS.Version)
		out.TLSCipher = tls.CipherSuiteName(r.TLS.CipherSuite)
		out.ALPN = r.TLS.NegotiatedProtocol
	}
	for _, key := range []string{"User-Agent", "Originator", "Version", "OpenAI-Beta", "Content-Type", "Accept", "Accept-Encoding", "Content-Encoding", "Accept-Language", "Content-Language", "X-Language", "X-Locale", "X-Codex-Locale", "X-Codex-Beta-Features"} {
		if values := r.Header.Values(key); len(values) > 0 {
			out.Headers[strings.ToLower(key)] = append([]string(nil), values...)
		}
	}
	for _, key := range []string{"Session-Id", "Thread-Id", "Session_id", "Conversation_id", "X-Codex-Turn-Metadata", "X-Codex-Turn-State", "X-Client-Request-Id", "X-Request-Id", "Traceparent"} {
		out.IDHeaderPresence[strings.ToLower(key)] = r.Header.Get(key) != ""
	}
	var turnMetadata map[string]any
	if json.Unmarshal([]byte(r.Header.Get("X-Codex-Turn-Metadata")), &turnMetadata) == nil {
		out.TurnMetadataKeys = nativeCaptureKeys(turnMetadata)
	}
	metadata, _ := payload["client_metadata"].(map[string]any)
	out.ClientMetadataKeys = nativeCaptureKeys(metadata)
	out.Stream, _ = payload["stream"].(bool)
	out.Store, _ = payload["store"].(bool)
	for _, value := range realCLIArray(payload["input"]) {
		item, _ := value.(map[string]any)
		kind, _ := item["type"].(string)
		if kind == "" {
			kind = "message_without_explicit_type"
		}
		out.InputTypes[kind]++
	}
	var countTools func([]any)
	countTools = func(tools []any) {
		for _, value := range tools {
			tool, _ := value.(map[string]any)
			kind, _ := tool["type"].(string)
			out.ToolTypes[kind]++
			countTools(realCLIArray(tool["tools"]))
		}
	}
	countTools(realCLIArray(payload["tools"]))
	cacheKey, _ := payload["prompt_cache_key"].(string)
	out.Cache["prompt_cache_key_present"] = cacheKey != ""
	out.Cache["prompt_cache_key_length"] = len(cacheKey)
	_, retentionPresent := payload["prompt_cache_retention"]
	out.Cache["prompt_cache_retention_present"] = retentionPresent
	if retention, _ := payload["prompt_cache_retention"].(string); retention == "in-memory" || retention == "24h" {
		out.Cache["prompt_cache_retention"] = retention
	}
	return out
}

func nativeCaptureKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func nativeCaptureFindTool(tools []any, namespace, wanted string) (string, bool) {
	for _, value := range tools {
		tool, _ := value.(map[string]any)
		name, _ := tool["name"].(string)
		if tool["type"] == "namespace" {
			if ns, ok := nativeCaptureFindTool(realCLIArray(tool["tools"]), name, wanted); ok {
				return ns, true
			}
		}
		if name == wanted {
			return namespace, true
		}
	}
	return "", false
}

func nativeCaptureCertificate(t *testing.T) (tls.Certificate, *x509.CertPool, string) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal("cannot generate local CA")
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Ephemeral local CLI capture CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal("cannot create local CA")
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal("cannot generate local TLS key")
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal("cannot create local TLS certificate")
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal("cannot encode local TLS key")
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal("cannot load local TLS certificate")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("cannot build local CA pool")
	}
	file := filepath.Join(t.TempDir(), "capture-ca.pem")
	if err := os.WriteFile(file, caPEM, 0600); err != nil {
		t.Fatal("cannot write temporary CA bundle")
	}
	return cert, pool, file
}

type nativeCaptureListener struct {
	net.Listener
	onHello func(nativeCaptureHello)
}

func (l *nativeCaptureListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &nativeCaptureConn{Conn: conn, onHello: l.onHello}, nil
}

// The first TLS handshake is bounded and buffered only until ClientHello is
// parsed. Randoms, session IDs, key shares, SNI and raw bytes are then discarded.
type nativeCaptureConn struct {
	net.Conn
	onHello   func(nativeCaptureHello)
	records   []byte
	handshake []byte
	done      bool
}

func (c *nativeCaptureConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if c.done || n == 0 {
		return n, err
	}
	if len(c.records)+len(c.handshake)+n > 128<<10 {
		c.complete(nativeCaptureHello{ParseError: "client_hello_exceeded_capture_limit"})
		return n, err
	}
	c.records = append(c.records, p[:n]...)
	for len(c.records) >= 5 {
		length := int(binary.BigEndian.Uint16(c.records[3:5]))
		if len(c.records) < length+5 {
			break
		}
		if c.records[0] != 22 {
			c.complete(nativeCaptureHello{ParseError: "expected_tls_handshake_record"})
			break
		}
		c.handshake = append(c.handshake, c.records[5:5+length]...)
		c.records = c.records[5+length:]
		if len(c.handshake) < 4 {
			continue
		}
		size := int(c.handshake[1])<<16 | int(c.handshake[2])<<8 | int(c.handshake[3])
		if c.handshake[0] != 1 {
			c.complete(nativeCaptureHello{ParseError: "expected_client_hello"})
			break
		}
		if len(c.handshake) >= 4+size {
			c.complete(nativeCaptureParseHello(c.handshake[4 : 4+size]))
			break
		}
	}
	return n, err
}

func (c *nativeCaptureConn) complete(hello nativeCaptureHello) {
	c.done = true
	c.records, c.handshake = nil, nil
	c.onHello(hello)
}

func nativeCaptureParseHello(raw []byte) nativeCaptureHello {
	result := nativeCaptureHello{CipherSuites: []string{}, Extensions: []int{}, SupportedVersions: []string{}, SupportedGroups: []int{}, SignatureAlgorithms: []string{}, ALPN: []string{}, ClientHelloByteLength: len(raw) + 4}
	reader := bytes.NewReader(raw)
	readBytes := func(size int) ([]byte, bool) {
		if size > reader.Len() {
			return nil, false
		}
		data := make([]byte, size)
		_, err := io.ReadFull(reader, data)
		return data, err == nil
	}
	readVector := func(wide bool) ([]byte, bool) {
		size, ok := readBytes(1)
		if !ok {
			return nil, false
		}
		n := int(size[0])
		if wide {
			more, ok := readBytes(1)
			if !ok {
				return nil, false
			}
			n = n<<8 | int(more[0])
		}
		return readBytes(n)
	}
	header, ok := readBytes(34)
	if !ok {
		result.ParseError = "short_hello_header"
		return result
	}
	result.LegacyVersion = fmt.Sprintf("0x%04x", binary.BigEndian.Uint16(header[:2]))
	if _, ok := readVector(false); !ok { // opaque session ID intentionally ignored
		result.ParseError = "short_session_vector"
		return result
	}
	ciphers, ok := readVector(true)
	if !ok || len(ciphers)%2 != 0 {
		result.ParseError = "invalid_cipher_vector"
		return result
	}
	for i := 0; i < len(ciphers); i += 2 {
		result.CipherSuites = append(result.CipherSuites, fmt.Sprintf("0x%04x", binary.BigEndian.Uint16(ciphers[i:i+2])))
	}
	if _, ok := readVector(false); !ok {
		result.ParseError = "invalid_compression_vector"
		return result
	}
	if reader.Len() == 0 {
		return result
	}
	extensions, ok := readVector(true)
	if !ok {
		result.ParseError = "invalid_extension_vector"
		return result
	}
	for len(extensions) > 0 {
		if len(extensions) < 4 {
			result.ParseError = "short_extension_header"
			break
		}
		id, length := int(binary.BigEndian.Uint16(extensions[:2])), int(binary.BigEndian.Uint16(extensions[2:4]))
		if len(extensions) < 4+length {
			result.ParseError = "short_extension_data"
			break
		}
		data := extensions[4 : 4+length]
		result.Extensions = append(result.Extensions, id)
		switch id {
		case 0:
			result.ServerNamePresent = true
		case 10, 13:
			if len(data) >= 2 && int(binary.BigEndian.Uint16(data[:2])) == len(data)-2 && len(data)%2 == 0 {
				for i := 2; i < len(data); i += 2 {
					v := binary.BigEndian.Uint16(data[i : i+2])
					if id == 10 {
						result.SupportedGroups = append(result.SupportedGroups, int(v))
					} else {
						result.SignatureAlgorithms = append(result.SignatureAlgorithms, fmt.Sprintf("0x%04x", v))
					}
				}
			}
		case 43:
			if len(data) > 0 && int(data[0]) == len(data)-1 && len(data)%2 == 1 {
				for i := 1; i < len(data); i += 2 {
					result.SupportedVersions = append(result.SupportedVersions, fmt.Sprintf("0x%04x", binary.BigEndian.Uint16(data[i:i+2])))
				}
			}
		case 16:
			if len(data) < 2 || int(binary.BigEndian.Uint16(data[:2])) != len(data)-2 {
				result.ParseError = "invalid_alpn_vector"
				break
			}
			for protocols := data[2:]; len(protocols) > 0; {
				size := int(protocols[0])
				if len(protocols) < 1+size {
					result.ParseError = "short_alpn_protocol"
					break
				}
				protocol := string(protocols[1 : 1+size])
				if protocol == "h2" || protocol == "http/1.1" || protocol == "h3" {
					result.ALPN = append(result.ALPN, protocol)
				} else {
					result.ALPN = append(result.ALPN, "other")
				}
				protocols = protocols[1+size:]
			}
		}
		extensions = extensions[4+length:]
	}
	return result
}

func nativeCaptureCompare(scenarios []nativeCaptureScenario) map[string]any {
	out := map[string]any{}
	for _, origin := range []string{"official_npm_release", "desktop_bundled_binary_fresh_exec"} {
		byName := map[string]nativeCaptureScenario{}
		for _, scenario := range scenarios {
			if scenario.Client.Origin == origin {
				byName[scenario.Name] = scenario
			}
		}
		base, us := byName["custom_ca_direct_default_env"], byName["custom_ca_direct_us_env"]
		plain := byName["plain_http_direct_us_env"]
		if len(plain.Requests) > 0 && len(us.Requests) > 0 {
			out[origin+"_plain_http_vs_custom_ca_tls"] = map[string]any{
				"same_whitelisted_application_headers": reflect.DeepEqual(plain.Requests[0].Headers, us.Requests[0].Headers),
				"same_request_schema_keys":             reflect.DeepEqual(plain.Requests[0].TopLevelKeys, us.Requests[0].TopLevelKeys),
				"plain_http_protocol":                  plain.Requests[0].HTTPProtocol,
				"custom_ca_tls_http_protocol":          us.Requests[0].HTTPProtocol,
			}
		}
		if len(base.Requests) > 0 && len(us.Requests) > 0 {
			out[origin+"_locale_env"] = map[string]any{
				"same_whitelisted_application_headers": reflect.DeepEqual(base.Requests[0].Headers, us.Requests[0].Headers),
				"same_request_schema_keys":             reflect.DeepEqual(base.Requests[0].TopLevelKeys, us.Requests[0].TopLevelKeys),
				"conclusion_scope":                     "comparison of one fresh exec per environment using the same binary and controlled TLS server",
			}
		}
		gateway := byName["custom_ca_via_go_gateway_us_env"]
		if len(gateway.Requests) > 0 && len(us.Requests) > 0 {
			out[origin+"_gateway"] = map[string]any{
				"same_user_agent":                      reflect.DeepEqual(gateway.Requests[0].Headers["user-agent"], us.Requests[0].Headers["user-agent"]),
				"same_whitelisted_application_headers": reflect.DeepEqual(gateway.Requests[0].Headers, us.Requests[0].Headers),
				"same_request_schema_keys":             reflect.DeepEqual(gateway.Requests[0].TopLevelKeys, us.Requests[0].TopLevelKeys),
				"direct_http_protocol":                 us.Requests[0].HTTPProtocol,
				"gateway_http_protocol":                gateway.Requests[0].HTTPProtocol,
			}
		}
	}
	return out
}

func TestSourceNativeCaptureExcludesRequestSecrets(t *testing.T) {
	const sentinel = "SENSITIVE_FIXTURE_VALUE_MUST_NOT_BE_REPORTED"
	r := httptest.NewRequest("POST", "https://localhost/responses", nil)
	r.Header.Set("Authorization", "Bearer "+sentinel)
	r.Header.Set("Cookie", sentinel)
	r.Header.Set("ChatGPT-Account-Id", sentinel)
	r.Header.Set("Session-Id", sentinel)
	r.Header.Set("Thread-Id", sentinel)
	r.Header.Set("X-Unknown-Private-Header", sentinel)
	r.Header.Set("X-Codex-Turn-Metadata", `{"turn_id":"`+sentinel+`","request_kind":"turn"}`)
	r.Header.Set("User-Agent", "codex_cli_rs/fixture")
	request := nativeCaptureSanitizeRequest(r, map[string]any{
		"instructions":     sentinel,
		"prompt_cache_key": sentinel,
		"client_metadata":  map[string]any{"session_id": sentinel},
		"input":            []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": sentinel}}}},
		"tools":            []any{map[string]any{"type": "function", "name": sentinel, "description": sentinel}},
	})
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(sentinel)) {
		t.Fatal("capture leaked a credential, identity or body value")
	}
	if request.Headers["user-agent"][0] != "codex_cli_rs/fixture" || !request.IDHeaderPresence["session-id"] || request.ToolTypes["function"] != 1 {
		t.Fatal("capture discarded required sanitized protocol metadata")
	}
}

func TestSourceNativeCaptureRejectsTruncatedHello(t *testing.T) {
	// ClientHello fixed header, empty session ID, one advertised cipher,
	// no compression and a supported_versions extension offering TLS 1.3.
	hello := append([]byte{3, 3}, make([]byte, 32)...)
	hello = append(hello, 0, 0, 2, 0x13, 0x01, 1, 0, 0, 7, 0, 43, 0, 3, 2, 3, 4)
	parsed := nativeCaptureParseHello(hello)
	if parsed.ParseError != "" || !reflect.DeepEqual(parsed.CipherSuites, []string{"0x1301"}) || !reflect.DeepEqual(parsed.SupportedVersions, []string{"0x0304"}) {
		t.Fatalf("valid controlled hello was not parsed: %+v", parsed)
	}
	for _, cut := range []int{0, 20, 34, 36, len(hello) - 1} {
		if got := nativeCaptureParseHello(hello[:cut]); got.ParseError == "" {
			t.Errorf("accepted truncated ClientHello at byte %d", cut)
		}
	}
}
