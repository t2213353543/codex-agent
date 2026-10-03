package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Opt in explicitly: this consumes actual model tokens using one supplied
// account export, with no refresh rotation and no existing deployment writes.
func TestCodexCLIWorkload(t *testing.T) {
	if os.Getenv("CODEX_CLI_WORKLOAD") != "1" {
		t.Skip("set CODEX_CLI_WORKLOAD=1, CODEX_CLI_MODEL and explicit account export or upstream URL/key")
	}
	accountFile, model := os.Getenv("CODEX_CLI_ACCOUNT_FILE"), os.Getenv("CODEX_CLI_MODEL")
	upstreamURL, upstreamKey := os.Getenv("CODEX_CLI_UPSTREAM_URL"), os.Getenv("CODEX_CLI_UPSTREAM_KEY")
	if model == "" || (accountFile == "" && (upstreamURL == "" || upstreamKey == "")) {
		t.Fatal("explicit account export or upstream URL/key and a model are required")
	}
	if accountFile != "" && (upstreamURL != "" || upstreamKey != "") {
		t.Fatal("choose one credential source")
	}
	if accountFile != "" {
		upstreamURL = codexURL
	}
	parsed, err := url.Parse(upstreamURL)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.HasSuffix(parsed.Path, "/responses") ||
		strings.ContainsAny(upstreamKey, "\r\n") {
		t.Fatal("invalid upstream base URL or credential")
	}
	p := newTestProxy(t, 10)
	t.Cleanup(p.closeGatewayClients)
	if accountFile != "" {
		codexCLIImportTestAccount(t, p, accountFile)
	} else if err := p.Store.UpdateAccount("test", func(account *Account) error {
		account.AccessToken, account.RefreshToken, account.AccountID = upstreamKey, "", ""
		account.ExpiresAt = time.Now().Add(time.Hour).Unix()
		return nil
	}); err != nil {
		t.Fatal("cannot configure isolated upstream gateway credential")
	}
	p.invalidateAccounts()
	p.BaseURL = strings.TrimRight(upstreamURL, "/")
	diagnostics, err := newGatewayCacheDiagnostics()
	if err != nil {
		t.Fatal(err)
	}
	transport := p.Client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	observer := &codexWorkloadTransport{base: transport, cache: diagnostics, statuses: make(map[int]int)}
	// Egress is observed through the same Go client/proxy path as inference. Do
	// not export the address, proxy configuration or Cloudflare request headers.
	egress := codexWorkloadEgress(p.Client)
	p.Client.Transport = observer
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	python, err := exec.LookPath("python")
	if err != nil {
		t.Fatal("python is required")
	}
	script, err := filepath.Abs(filepath.Join(projectRoot(t), "tools", "codex_cli_workload.py"))
	if err != nil {
		t.Fatal("cannot resolve workload script")
	}
	args := []string{script, "--base-url", server.URL + "/v1", "--model", model,
		"--api-key-env", "CODEX_CLI_LOCAL_KEY", "--timeout", "300"}
	if executable := os.Getenv("CODEX_CLI_BIN"); executable != "" {
		args = append(args, "--codex", executable)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 16*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, python, args...)
	command.Env = codexCLIIntegrationEnvironment()
	command.WaitDelay = 5 * time.Second
	if runtime.GOOS == "windows" {
		command.Cancel = func() error {
			killCtx, killCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer killCancel()
			if exec.CommandContext(killCtx, "taskkill", "/PID", strconv.Itoa(command.Process.Pid), "/T", "/F").Run() != nil {
				return command.Process.Kill()
			}
			return nil
		}
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	runErr := command.Run()
	server.Close()
	var workload struct {
		Passed bool `json:"passed"`
	}
	validReport := json.Unmarshal(stdout.Bytes(), &workload) == nil
	cache := diagnostics.Snapshot()
	observer.mu.Lock()
	evidence := map[string]any{
		"schema_version": 1, "scope": "real_cli_code_task_and_native_request_cache",
		"date": time.Now().UTC().Format(time.RFC3339), "model": model,
		"passed": runErr == nil && validReport && workload.Passed && cache.SuccessfulResponses > 0 &&
			cache.FailedResponses == 0 && cache.PendingRequests == 0 && observer.errors == 0 && observer.diagnosticErrors == 0,
		"timed_out": ctx.Err() != nil, "egress_observation": egress,
		"http_status_counts": observer.statuses, "transport_errors": observer.errors,
		"diagnostic_errors": observer.diagnosticErrors, "connections_observed": observer.connections,
		"connections_reused": observer.reused, "tls_handshakes": observer.handshakes,
		"cache": cache, "transport": "Go net/http; application identity is preserved, native TLS parity is not claimed",
	}
	observer.mu.Unlock()
	if validReport {
		evidence["workload"] = json.RawMessage(stdout.Bytes())
	} else {
		evidence["report_error"] = "invalid_workload_report"
	}
	encoded, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Fatal("cannot encode workload evidence")
	}
	if output := os.Getenv("CODEX_CLI_WORKLOAD_EVIDENCE"); output != "" {
		if os.WriteFile(output, append(encoded, '\n'), 0600) != nil {
			t.Error("cannot write workload evidence")
		}
	}
	t.Logf("Content-free workload evidence: %s", encoded)
	if evidence["passed"] != true {
		t.Fatal("real CLI workload failed; inspect the content-free evidence")
	}
}

type codexWorkloadTransport struct {
	base                                          http.RoundTripper
	cache                                         *gatewayCacheDiagnostics
	mu                                            sync.Mutex
	statuses                                      map[int]int
	errors, diagnosticErrors, connections, reused int
	handshakes                                    []map[string]any
}

func (o *codexWorkloadTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var id uint64
	// This experiment measures Responses SSE, not JSON compact/OAuth calls.
	// Keep all transport counters below, but never mix those routes into the
	// per-thread prompt history or pass their JSON bodies to the SSE observer.
	if r.Method == http.MethodPost && r.URL != nil && gatewayObservedRoute(r.URL.RequestURI()) == "/responses" && r.GetBody != nil {
		body, err := r.GetBody()
		if err == nil {
			raw, readErr := io.ReadAll(io.LimitReader(body, maxBody+1))
			_ = body.Close()
			if readErr == nil {
				id, err = o.cache.ObserveRequest(r, raw)
			} else {
				err = readErr
			}
		}
		if err != nil {
			o.mu.Lock()
			o.diagnosticErrors++
			o.mu.Unlock()
		}
	}
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			o.mu.Lock()
			defer o.mu.Unlock()
			o.connections++
			if info.Reused {
				o.reused++
			}
		},
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			o.mu.Lock()
			defer o.mu.Unlock()
			o.handshakes = append(o.handshakes, map[string]any{
				"completed": err == nil, "version": tls.VersionName(state.Version),
				"cipher": tls.CipherSuiteName(state.CipherSuite), "alpn": state.NegotiatedProtocol})
		},
	}
	response, err := o.base.RoundTrip(r.WithContext(httptrace.WithClientTrace(r.Context(), trace)))
	o.mu.Lock()
	if err != nil {
		o.errors++
	} else {
		o.statuses[response.StatusCode]++
	}
	o.mu.Unlock()
	if id != 0 {
		if err != nil || response.StatusCode >= 300 {
			// HTTP rejections have no model terminal event. Account for them
			// above without mislabeling an expected JSON error as broken SSE.
			o.cache.abandon(id)
		} else {
			observer := o.cache.ResponseObserver(id)
			response.Body = &codexWorkloadBody{ReadCloser: response.Body, observer: observer, owner: o}
		}
	}
	return response, err
}

type codexWorkloadBody struct {
	io.ReadCloser
	observer *gatewayCacheResponseObserver
	owner    *codexWorkloadTransport
	once     sync.Once
}

func (b *codexWorkloadBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		_, _ = b.observer.Write(p[:n])
	}
	return n, err
}

func (b *codexWorkloadBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() {
		if b.observer.Finish() != nil {
			b.owner.mu.Lock()
			b.owner.diagnosticErrors++
			b.owner.mu.Unlock()
		}
	})
	return err
}

func codexWorkloadEgress(client *http.Client) map[string]string {
	result := map[string]string{"scope": "same Go client system-proxy route; upstream destination routing may differ"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.cloudflare.com/cdn-cgi/trace", nil)
	response, err := client.Do(r)
	if err != nil {
		result["status"] = "unavailable"
		return result
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 8192))
	if err != nil || response.StatusCode != http.StatusOK {
		result["status"] = "unavailable"
		return result
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && (key == "loc" || key == "colo" || key == "tls" || key == "http") {
			result[key] = value
		}
	}
	result["status"] = "observed"
	return result
}
