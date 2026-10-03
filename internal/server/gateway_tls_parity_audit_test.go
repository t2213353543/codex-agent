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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type auditWSTLSHandshake struct {
	HTTPProtocol       string `json:"http_protocol"`
	TLSVersion         string `json:"tls_version"`
	TLSCipher          string `json:"tls_cipher"`
	ALPN               string `json:"negotiated_alpn"`
	ServerName         string `json:"server_name"`
	DidResume          bool   `json:"tls_session_resumed"`
	DeflateOffered     string `json:"deflate_offered"`
	DeflateNegotiated  string `json:"deflate_negotiated"`
	Messages           int    `json:"messages_on_connection"`
	Warmups            int    `json:"warmups_on_connection"`
	Generated          int    `json:"generated_on_connection"`
	ExactPreviousLinks int    `json:"exact_previous_response_links"`
}

type auditWSTLSScenario struct {
	Name             string                `json:"name"`
	Hellos           []nativeCaptureHello  `json:"client_hellos"`
	Handshakes       []auditWSTLSHandshake `json:"websocket_handshakes"`
	CLICompleted     int                   `json:"completed_cli_processes"`
	CLIFailed        int                   `json:"failed_cli_processes"`
	CLITimedOut      int                   `json:"timed_out_cli_processes"`
	CLIStopped       int                   `json:"cli_stopped_after_certificate_rejection"`
	TLSCertAlerts    int                   `json:"client_certificate_rejection_alerts"`
	HTTPFallbacks    int                   `json:"http_fallback_requests"`
	GoFallbacks      int32                 `json:"go_outbound_fallback_attempts"`
	RejectedNativeWS bool                  `json:"native_ws_rejected"`
	openConnections  int
}

// This is an opt-in same-platform comparison of the unmodified CLI and the
// gateway's real Rust WSS adapter against the SAME loopback TLS listener. It
// records only protocol metadata, never bearer values, request bodies or keys.
func TestGatewayNativeActualWebSocketTLSParity(t *testing.T) {
	cli, helper := os.Getenv("CODEX_NATIVE_CAPTURE_BIN"), os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN")
	if cli == "" || helper == "" || runtime.GOOS != "linux" {
		t.Skip("set CODEX_NATIVE_CAPTURE_BIN and CODEX_NATIVE_HTTP_TEST_BIN on Linux")
	}
	cli, err := filepath.Abs(cli)
	if err != nil {
		t.Fatal(err)
	}
	identity := nativeCaptureIdentity(t, cli, "official_release")
	if identity.Version != "codex-cli "+clientVersion {
		t.Fatal("WSS parity requires the source-pinned official CLI")
	}
	helperBytes, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal("cannot hash native helper")
	}
	helperSum := sha256.Sum256(helperBytes)
	scenarios := []map[string]any{}
	for _, mode := range []string{"default_rustls_native_roots", "codex_custom_ca", "untrusted_ca", "wrong_dns_name"} {
		t.Run(mode, func(t *testing.T) {
			scenarios = append(scenarios, auditWSTLSRun(t, cli, mode))
		})
	}
	report := map[string]any{
		"schema": "official-cli-native-wss-tls-parity-v1", "passed": !t.Failed(),
		"host_os": runtime.GOOS, "host_arch": runtime.GOARCH, "cli": identity,
		"helper_sha256": hex.EncodeToString(helperSum[:]), "scenarios": scenarios,
		"controlled_loopback_only": true, "live_model_inference": false,
		"limits": []string{
			"Both clients use the same localhost server and certificate in each scenario; no production host, proxy or public model endpoint is contacted.",
			"Default-root success sets only SSL_CERT_DIR to an ephemeral PEM directory; this exercises the unmodified ExplicitCodexTls native-root loader, not installed system trust or HTTP native TLS.",
			"Compared parsed ClientHello metadata after sorting only randomized TLS extension IDs; random bytes, key shares, TCP/IP fingerprint and raw header ordering are not compared.",
			"Three fresh CLI processes and one persistent gateway helper perform three WSS connections per successful mode; warmup and generated turns reuse each socket. TLS resumption is observed, not inferred from equal dependencies.",
			"The server negotiates permessage-deflate and transmits repeated text through the real implementations. RSV1 bit patterns and exact compressed byte equality are outside this capture.",
			"Negative official CLI probes are explicitly stopped only after the server observes a peer certificate-rejection TLS alert; this is not a normal CLI process completion. Gateway rejection is tested directly through its native WSS adapter. Neither client may reach the HTTP/WS application handler.",
		},
	}
	if name := os.Getenv("CODEX_NATIVE_WS_TLS_REPORT"); name != "" {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil || os.WriteFile(name, append(encoded, '\n'), 0600) != nil {
			t.Fatal("cannot write sanitized WSS TLS report")
		}
	}
}

func auditWSTLSRun(t *testing.T, cli, mode string) map[string]any {
	t.Helper()
	negative := mode == "untrusted_ca" || mode == "wrong_dns_name"
	dnsName := "localhost"
	if mode == "wrong_dns_name" {
		dnsName = "wrong.invalid"
	}
	cert, caFile := auditWSTLSCertificate(t, dnsName)
	trustFile := caFile
	if mode == "untrusted_ca" {
		_, _, trustFile = nativeCaptureCertificate(t)
	}
	for _, key := range []string{"CODEX_CA_CERTIFICATE", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		// Truly remove absent variables: rustls-native-certs treats an empty
		// SSL_CERT_FILE as an explicit path, unlike Codex's custom-CA selection.
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "default_rustls_native_roots" {
		rootDir := t.TempDir()
		pemBytes, err := os.ReadFile(trustFile)
		if err != nil || os.WriteFile(filepath.Join(rootDir, "fixture-root.pem"), pemBytes, 0600) != nil {
			t.Fatal("cannot prepare temporary native root directory")
		}
		t.Setenv("SSL_CERT_DIR", rootDir)
	} else {
		t.Setenv("CODEX_CA_CERTIFICATE", trustFile)
	}
	// This comparison is specifically the default CLI lifecycle. The separate
	// optional pooled mode intentionally allows different TLS-cache behavior.
	t.Setenv("CODEX_NATIVE_HTTP_TEST_LIFECYCLE", "cli_request")
	var mu sync.Mutex
	official := &auditWSTLSScenario{Name: "official_direct_wss"}
	native := &auditWSTLSScenario{Name: "official_cli_via_gateway_native_wss"}
	active := official
	connectionOwners := make(map[string]*auditWSTLSScenario)
	certificateRejected := make(chan struct{})
	var rejectionOnce sync.Once
	const finalReply = "WSS_TLS_PARITY_COMPLETE"
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current := active
		if r.Method != http.MethodGet || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			current.HTTPFallbacks++
			mu.Unlock()
			http.Error(w, "WebSocket required", http.StatusUpgradeRequired)
			return
		}
		h := auditWSTLSHandshake{HTTPProtocol: r.Proto, TLSVersion: tls.VersionName(r.TLS.Version), TLSCipher: tls.CipherSuiteName(r.TLS.CipherSuite),
			ALPN: r.TLS.NegotiatedProtocol, ServerName: r.TLS.ServerName, DidResume: r.TLS.DidResume, DeflateOffered: r.Header.Get("Sec-WebSocket-Extensions")}
		mu.Unlock()
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover, CompressionThreshold: 1})
		if err != nil {
			t.Error("fixture could not accept WSS handshake")
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(maxBody)
		h.DeflateNegotiated = w.Header().Get("Sec-WebSocket-Extensions")
		mu.Lock()
		index := len(current.Handshakes)
		current.Handshakes = append(current.Handshakes, h)
		current.openConnections++
		mu.Unlock()
		defer func() { mu.Lock(); current.openConnections--; mu.Unlock() }()
		previous := ""
		for {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var body map[string]any
			if json.Unmarshal(raw, &body) != nil || body["type"] != "response.create" {
				t.Error("fixture received invalid WSS request")
				return
			}
			warmup := body["generate"] == false
			mu.Lock()
			h := &current.Handshakes[index]
			h.Messages++
			id := fmt.Sprintf("resp_wss_%d_%d", index, h.Messages)
			if warmup {
				h.Warmups++
			} else {
				h.Generated++
			}
			if previous != "" {
				if body["previous_response_id"] != previous {
					t.Error("WSS continuation did not reference this socket's latest response")
				} else {
					h.ExactPreviousLinks++
				}
			}
			mu.Unlock()
			output := []any{}
			events := []map[string]any{{"type": "response.created", "response": map[string]any{"id": id}}}
			if !warmup {
				item := realCLIMessage("wss_final", finalReply+" "+strings.Repeat("deflate-loopback ", 128))
				output = append(output, item)
				events = append(events, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
			}
			events = append(events, map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "status": "completed", "output": output,
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
			for _, event := range events {
				if lane, ok := body["stream_id"]; ok {
					event["stream_id"] = lane
				}
				if conn.Write(r.Context(), websocket.MessageText, []byte(realCLIJSON(event))) != nil {
					return
				}
			}
			previous = id
		}
	}))
	server.Config.ErrorLog = log.New(auditWSTLSLogWriter(func(message string) {
		// These messages come from Go's TLS parser after receiving the peer's
		// certificate alert. A ClientHello or EOF alone is insufficient proof.
		for _, alert := range []string{"remote error: tls: bad certificate", "remote error: tls: unknown certificate", "remote error: tls: unknown certificate authority"} {
			if strings.Contains(message, alert) {
				_, peerError, ok := strings.Cut(message, "TLS handshake error from ")
				peer, _, found := strings.Cut(peerError, ": remote error:")
				mu.Lock()
				owner := connectionOwners[peer]
				if ok && found && owner != nil {
					owner.TLSCertAlerts++
				}
				mu.Unlock()
				if ok && found && owner == official {
					rejectionOnce.Do(func() { close(certificateRejected) })
				}
				return
			}
		}
	}), "", 0)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}}
	server.Listener = &auditWSTLSListener{Listener: server.Listener, accepted: func(connection net.Conn) func(nativeCaptureHello) {
		mu.Lock()
		owner := active
		connectionOwners[connection.RemoteAddr().String()] = owner
		mu.Unlock()
		return func(hello nativeCaptureHello) {
			mu.Lock()
			owner.Hellos = append(owner.Hellos, hello)
			mu.Unlock()
		}
	}}
	server.StartTLS()
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	upstreamURL := "https://localhost:" + port
	waitClosed := func(current *auditWSTLSScenario) {
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			closed := current.openConnections == 0
			mu.Unlock()
			if closed {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("WSS fixture connection failed to close after CLI exit")
			}
			time.Sleep(time.Millisecond)
		}
	}
	rounds := 3
	if negative {
		rounds = 1
	}
	var stopAfterRejection <-chan struct{}
	if negative {
		stopAfterRejection = certificateRejected
	}
	for i := 0; i < rounds; i++ {
		completed, timedOut, stopped := auditWSTLSCLI(t, cli, upstreamURL+"/v1", finalReply, stopAfterRejection)
		if completed {
			official.CLICompleted++
		} else if stopped {
			official.CLIStopped++
		} else {
			official.CLIFailed++
		}
		if timedOut {
			official.CLITimedOut++
		}
		waitClosed(official)
	}
	mu.Lock()
	active = native
	mu.Unlock()
	p := newTestProxy(t, 1)
	p.BaseURL = upstreamURL
	enableGatewayWS(t, p, "")
	realGatewayNativePool(t, p)
	p.NativeWebSocket = true
	guard := &nativeCLIFallbackGuard{}
	p.Client.Transport = guard
	var routeProcesses []*gatewayNativeProcess
	if negative {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		connection, response, dialErr := p.dialNativeGatewayWebSocket(ctx, http.Header{"Authorization": {"Bearer synthetic-wss-key"}}, nil)
		cancel()
		native.RejectedNativeWS = dialErr != nil && connection == nil && response == nil
		if connection != nil {
			connection.CloseNow()
		}
	} else {
		gateway := httptest.NewServer(p)
		defer gateway.Close()
		for i := 0; i < rounds; i++ {
			completed, timedOut, _ := auditWSTLSCLI(t, cli, gateway.URL+"/v1", finalReply, nil)
			if completed {
				native.CLICompleted++
			} else {
				native.CLIFailed++
			}
			if timedOut {
				native.CLITimedOut++
			}
			waitClosed(native)
			waitGatewayWSReleased(t, p)
			p.NativeHTTP.mu.Lock()
			var live *gatewayNativeProcess
			for key, process := range p.NativeHTTP.entries {
				if key.origin == upstreamURL && !process.starting && !process.closing && process.command != nil && process.command.Process != nil {
					live = process
				}
			}
			routeProcesses = append(routeProcesses, live)
			p.NativeHTTP.mu.Unlock()
		}
	}
	native.GoFallbacks = guard.calls.Load()
	// Close the helper before reading capture arrays: failure may leave a final
	// TLS alert in flight, and httptest does not own hijacked WebSocket sockets.
	p.closeGatewayClients()
	server.Close()
	mu.Lock()
	defer mu.Unlock()
	checks := map[string]bool{"no_go_outbound_fallback": native.GoFallbacks == 0, "no_cli_timeout": official.CLITimedOut == 0 && native.CLITimedOut == 0}
	if negative {
		checks["official_rejected_invalid_certificate"] = official.CLIFailed+official.CLIStopped == 1 && official.CLICompleted == 0 && len(official.Hellos) > 0 && official.TLSCertAlerts > 0
		checks["native_rejected_invalid_certificate"] = native.RejectedNativeWS && len(native.Hellos) > 0 && native.TLSCertAlerts > 0
		checks["neither_client_reached_http_or_ws_application"] = len(official.Handshakes) == 0 && len(native.Handshakes) == 0 && official.HTTPFallbacks == 0 && native.HTTPFallbacks == 0
	} else {
		checks["three_connections_each"] = len(official.Handshakes) == 3 && len(native.Handshakes) == 3 && len(official.Hellos) == 3 && len(native.Hellos) == 3
		checks["six_cli_processes_completed"] = official.CLICompleted == 3 && native.CLICompleted == 3
		checks["same_gateway_helper_process_across_three_connections"] = len(routeProcesses) == 3 && routeProcesses[0] != nil && routeProcesses[0] == routeProcesses[1] && routeProcesses[1] == routeProcesses[2]
		checks["no_http_fallback"] = official.HTTPFallbacks == 0 && native.HTTPFallbacks == 0
		checks["normalized_hello_fields_match_all_connections"] = len(official.Hellos) == 3 && len(native.Hellos) == 3
		checks["negotiated_protocols_and_deflate_match"] = len(official.Handshakes) == 3 && len(native.Handshakes) == 3
		checks["explicit_rustls_ws_profile_and_sni"] = true
		checks["warmup_and_generated_reuse_same_connection"] = true
		checks["new_connection_does_not_resume_tls_session"] = true
		for _, sample := range []*auditWSTLSScenario{official, native} {
			for _, hello := range sample.Hellos {
				checks["explicit_rustls_ws_profile_and_sni"] = checks["explicit_rustls_ws_profile_and_sni"] && hello.ParseError == "" && hello.ServerNamePresent && len(hello.ALPN) == 0 && slices.Equal(hello.SupportedGroups, []int{4588, 29, 23, 24})
			}
			for _, h := range sample.Handshakes {
				checks["warmup_and_generated_reuse_same_connection"] = checks["warmup_and_generated_reuse_same_connection"] && h.Messages == 2 && h.Warmups == 1 && h.Generated == 1 && h.ExactPreviousLinks == 1
				checks["new_connection_does_not_resume_tls_session"] = checks["new_connection_does_not_resume_tls_session"] && !h.DidResume
				checks["explicit_rustls_ws_profile_and_sni"] = checks["explicit_rustls_ws_profile_and_sni"] && h.HTTPProtocol == "HTTP/1.1" && h.TLSVersion == "TLS 1.3" && h.ServerName == "localhost" && h.ALPN == ""
			}
		}
		for i := 0; i < min(len(official.Hellos), len(native.Hellos)); i++ {
			left, right := official.Hellos[i], native.Hellos[i]
			left.Extensions, right.Extensions = slices.Clone(left.Extensions), slices.Clone(right.Extensions)
			slices.Sort(left.Extensions)
			slices.Sort(right.Extensions)
			checks["normalized_hello_fields_match_all_connections"] = checks["normalized_hello_fields_match_all_connections"] && reflect.DeepEqual(left, right)
		}
		for i := 0; i < min(len(official.Handshakes), len(native.Handshakes)); i++ {
			left, right := official.Handshakes[i], native.Handshakes[i]
			checks["negotiated_protocols_and_deflate_match"] = checks["negotiated_protocols_and_deflate_match"] && left == right && strings.Contains(left.DeflateOffered, "permessage-deflate") && strings.Contains(left.DeflateNegotiated, "permessage-deflate")
		}
	}
	for name, passed := range checks {
		if !passed {
			t.Errorf("WSS TLS parity failed: %s", name)
		}
	}
	return map[string]any{"trust_mode": mode, "passed": !t.Failed(), "official": official, "gateway": native, "checks": checks}
}

func auditWSTLSCLI(t *testing.T, bin, providerURL, finalReply string, stopAfterRejection <-chan struct{}) (completed, timedOut, stopped bool) {
	t.Helper()
	cliHome, workdir := t.TempDir(), t.TempDir()
	config := fmt.Sprintf(`model = "gpt-5.4"
model_provider = "wss_fixture"
approval_policy = "never"
sandbox_mode = "read-only"
web_search = "disabled"
[model_providers.wss_fixture]
name = "OpenAI"
base_url = %q
env_key = "CODEX_WSS_FIXTURE_KEY"
wire_api = "responses"
supports_websockets = true
request_max_retries = 0
stream_max_retries = 0
stream_idle_timeout_ms = 3000
`, providerURL)
	if os.WriteFile(filepath.Join(cliHome, "config.toml"), []byte(config), 0600) != nil {
		t.Fatal("cannot write isolated WSS CLI configuration")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "exec", "--skip-git-repo-check", "--ignore-rules", "--json", "Complete the local WebSocket TLS observation.")
	cmd.Dir = workdir
	for _, key := range []string{"PATH", "HOME", "USERPROFILE", "TMP", "TEMP", "SYSTEMROOT", "WINDIR", "SHELL", "SSL_CERT_DIR", "CODEX_CA_CERTIFICATE"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.Env = append(cmd.Env, "CODEX_HOME="+cliHome, "CODEX_WSS_FIXTURE_KEY=test-client-key", "NO_PROXY=*", "no_proxy=*", "NO_COLOR=1")
	var stdout bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal("cannot start isolated WSS CLI process")
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	select {
	case err := <-finished:
		return err == nil && strings.Contains(stdout.String(), finalReply), ctx.Err() == context.DeadlineExceeded, false
	case <-stopAfterRejection:
		cancel()
		<-finished
		return false, false, true
	case <-ctx.Done():
		<-finished
		return false, true, false
	}
}

type auditWSTLSLogWriter func(string)

func (writer auditWSTLSLogWriter) Write(message []byte) (int, error) {
	writer(string(message))
	return len(message), nil
}

type auditWSTLSListener struct {
	net.Listener
	accepted func(net.Conn) func(nativeCaptureHello)
}

func (listener *auditWSTLSListener) Accept() (net.Conn, error) {
	connection, err := listener.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &nativeCaptureConn{Conn: connection, onHello: listener.accepted(connection)}, nil
}

// DNS-only leaves let the negative test distinguish hostname verification from
// CA trust. No system trust store or DNS configuration is changed.
func auditWSTLSCertificate(t *testing.T, dnsName string) (tls.Certificate, string) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Temporary WSS audit CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{dnsName}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(t.TempDir(), "wss-ca.pem")
	if os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0600) != nil {
		t.Fatal("cannot write WSS fixture CA")
	}
	return cert, caFile
}
