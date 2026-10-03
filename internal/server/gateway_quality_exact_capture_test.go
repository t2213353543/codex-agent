package server

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

const exactCaptureHeadSchema = "codex-quality-captured-request-v1"

// This peer has no outbound transport. It receives the actual gateway native
// IPC writer output, retains one private no-auth request, and returns a visibly
// synthetic failure. It must never be counted as an inference/quality result.
func TestQualityExactCaptureHelperProcess(t *testing.T) {
	if os.Getenv("CODEX_EXACT_CAPTURE_PEER") != "1" {
		return
	}
	readJSON := func(reader io.Reader, value any) error {
		var size uint32
		if err := binary.Read(reader, binary.BigEndian, &size); err != nil {
			return err
		}
		if size > gatewayNativeHeadLimit {
			return errors.New("head limit")
		}
		raw := make([]byte, size)
		if _, err := io.ReadFull(reader, raw); err != nil {
			return err
		}
		return json.Unmarshal(raw, value)
	}
	var config struct {
		Token      string `json:"ipc_token"`
		Lifecycle  string `json:"http_client_lifecycle"`
		UnixSocket string `json:"unix_socket"`
	}
	if readJSON(os.Stdin, &config) != nil {
		os.Exit(2)
	}
	network, address := "tcp", "127.0.0.1:0"
	if config.UnixSocket != "" {
		network, address = "unix", config.UnixSocket
	}
	listener, err := net.Listen(network, address)
	if err != nil {
		os.Exit(2)
	}
	if network == "unix" {
		if os.Chmod(address, 0600) != nil {
			os.Exit(2)
		}
	}
	ready := map[string]any{"protocol": 1, "transport": network, "max_body": maxBody, "max_active": 0, "http_client_lifecycle": config.Lifecycle, "capabilities": []string{"request_zstd_v1"}}
	if network == "unix" {
		ready["unix_socket"] = address
	} else {
		ready["listen"] = listener.Addr().String()
	}
	_ = json.NewEncoder(os.Stdout).Encode(ready)
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); os.Exit(0) }()
	for {
		conn, err := listener.Accept()
		if err != nil {
			os.Exit(0)
		}
		func() {
			defer conn.Close()
			var head struct {
				Token       string      `json:"ipc_token"`
				Method      string      `json:"method"`
				Path        string      `json:"path"`
				Headers     [][2]string `json:"headers"`
				BodyLength  int         `json:"body_length"`
				Compression string      `json:"request_compression"`
			}
			if readJSON(conn, &head) != nil || head.Token != config.Token || head.Method != "POST" || head.Path != "/backend-api/codex/responses" || head.BodyLength < 0 || head.BodyLength > maxBody {
				return
			}
			body := make([]byte, head.BodyLength)
			if _, err := io.ReadFull(conn, body); err != nil {
				return
			}
			output := os.Getenv("CODEX_EXACT_CAPTURE_OUTPUT")
			headers := make([][2]string, 0, len(head.Headers))
			for _, pair := range head.Headers {
				switch strings.ToLower(pair[0]) {
				case "authorization", "chatgpt-account-id":
					continue
				}
				headers = append(headers, pair)
			}
			compression := head.Compression
			if compression == "" {
				compression = "none"
			}
			captured := map[string]any{"schema": exactCaptureHeadSchema, "method": head.Method, "path": head.Path, "headers": headers, "request_compression": compression, "body_sha256": qualityDigest(body), "body_bytes": len(body)}
			raw, _ := json.Marshal(captured)
			writeExclusive := func(name string, data []byte) error {
				f, e := os.OpenFile(filepath.Join(output, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
				if e != nil {
					return e
				}
				_, e = f.Write(data)
				ce := f.Close()
				if e != nil {
					return e
				}
				return ce
			}
			if writeExclusive("request.body", body) != nil || writeExclusive("request-head.json", raw) != nil {
				return
			}
			frame := func(kind byte, data []byte) {
				_, _ = conn.Write([]byte{kind})
				_ = binary.Write(conn, binary.BigEndian, uint32(len(data)))
				_ = gatewayNativeWriteBytes(conn, data)
			}
			response, _ := json.Marshal(map[string]any{"status": 200, "version": "HTTP/1.1", "headers": [][2]string{{"Content-Type", base64.StdEncoding.EncodeToString([]byte("text/event-stream"))}}})
			frame(1, response)
			frame(2, []byte("data: {\"type\":\"response.failed\",\"response\":{\"id\":\"synthetic_capture_only\",\"status\":\"failed\",\"error\":{\"code\":\"synthetic_capture_only\",\"message\":\"Request captured locally; no model POST was sent.\"}}}\n\n"))
			frame(3, nil)
		}()
	}
}

func exactCaptureProxy(t *testing.T, output string) *Proxy {
	t.Helper()
	p := newTestProxy(t, 4)
	p.BaseURL, p.TokenURL = codexURL, "http://127.0.0.1:9/disabled-refresh"
	settings := defaultSettings()
	settings.BuiltinPromptEnabled, settings.CodexFingerprintEnabled, settings.CompatibilityNormalizationEnabled, settings.PromptCacheKeyEnabled = false, false, false, false
	settings.AutomaticTokenRefreshEnabled, settings.Upstream401RecoveryEnabled = false, false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	p.NativeHTTP = nativeHTTPFixturePool(t, func(options *gatewayNativeOptions) {
		options.command = func(path string) *exec.Cmd {
			cmd := exec.Command(path, "-test.run=^TestQualityExactCaptureHelperProcess$")
			cmd.Env = append(gatewayNativeEnvironment(), "CODEX_EXACT_CAPTURE_PEER=1", "CODEX_EXACT_CAPTURE_OUTPUT="+output)
			return cmd
		}
		if runtime.GOOS == "linux" {
			options.network = "unix"
		}
	})
	return p
}

func exactCaptureHandler(catalog, capture *Proxy, posts, refused *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			catalog.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			http.Error(w, "capture-only route unavailable", 404)
			return
		}
		if !posts.CompareAndSwap(0, 1) {
			refused.Add(1)
			http.Error(w, "single capture exhausted", 409)
			return
		}
		capture.ServeHTTP(w, r)
	})
}

func TestQualityExactRequestCaptureOnly(t *testing.T) {
	path := os.Getenv("CODEX_QUALITY_EXACT_CAPTURE_CONFIG")
	if path == "" {
		t.Skip("explicit capture-only fixture configuration required")
	}
	if runtime.GOOS != "linux" || os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN") == "" {
		t.Fatal("Linux native helper required for GET models")
	}
	var config struct {
		CredentialFile string   `json:"credential_file"`
		Output         string   `json:"output"`
		Settings       Settings `json:"settings"`
	}
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &config) != nil || config.Output == "" || validateSettings(config.Settings) != nil {
		t.Fatal("invalid capture config")
	}
	var secret struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
		Exp         int64  `json:"exp"`
	}
	raw, err = os.ReadFile(config.CredentialFile)
	if err != nil || json.Unmarshal(raw, &secret) != nil || secret.AccessToken == "" || secret.AccountID == "" || secret.Exp < time.Now().Add(20*time.Minute).Unix() {
		t.Fatal("invalid capture credential")
	}
	if err := os.Mkdir(config.Output, 0700); err != nil {
		t.Fatal("capture output must be new")
	}
	capture := exactCaptureProxy(t, config.Output)
	catalog := newTestProxy(t, 4)
	catalog.BaseURL, catalog.TokenURL = codexURL, "http://127.0.0.1:9/disabled-refresh"
	for _, p := range []*Proxy{capture, catalog} {
		p.AdminKey = secret.AccessToken
		if err := p.Store.UpdateAccount("test", func(a *Account) error {
			a.AccessToken, a.AccountID, a.ExpiresAt = secret.AccessToken, secret.AccountID, secret.Exp
			a.RefreshToken, a.Identity = "", nil
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		settings := config.Settings
		settings.Version = 0
		settings.BuiltinPromptEnabled, settings.CodexFingerprintEnabled, settings.CompatibilityNormalizationEnabled, settings.PromptCacheKeyEnabled = false, false, false, false
		settings.AutomaticTokenRefreshEnabled, settings.Upstream401RecoveryEnabled = false, false
		if err := p.Settings.Set(settings); err != nil {
			t.Fatal(err)
		}
	}
	realGatewayNativePool(t, catalog)
	var posts, refused atomic.Int32
	server := httptest.NewServer(exactCaptureHandler(catalog, capture, &posts, &refused))
	defer server.Close()
	ready, _ := json.Marshal(map[string]any{"base_url": server.URL + "/v1", "model_post_forwarded": false, "capture_request_limit": 1})
	if os.WriteFile(filepath.Join(config.Output, "ready.json"), ready, 0600) != nil {
		t.Fatal("write ready")
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
wait:
	for {
		select {
		case <-t.Context().Done():
			break wait
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(config.Output, "stop")); err == nil {
				break wait
			}
		}
	}
	server.Close()
	capture.closeGatewayClients()
	catalog.closeGatewayClients()
	body, bodyErr := os.ReadFile(filepath.Join(config.Output, "request.body"))
	head, headErr := os.ReadFile(filepath.Join(config.Output, "request-head.json"))
	report, _ := json.Marshal(map[string]any{"schema": "capture-only-report-v1", "capture_posts": posts.Load(), "extra_posts_refused": refused.Load(), "model_inference_requests": 0, "synthetic_result": true, "capture_complete": bodyErr == nil && headErr == nil, "body_sha256": qualityDigest(body), "head_sha256": qualityDigest(head), "capture_boundary": "actual_gateway_native_IPC_before_Rust_compression_no_upstream_POST"})
	if os.WriteFile(filepath.Join(config.Output, "capture-report.json"), report, 0600) != nil {
		t.Fatal("write capture report")
	}
}

func TestQualityExactCaptureOfflineWriterContract(t *testing.T) {
	output := t.TempDir()
	p := exactCaptureProxy(t, output)
	var catalogRequests, posts, refused atomic.Int32
	catalog := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		catalogRequests.Add(1)
		if r.Method != "GET" {
			t.Error("catalog route leaked POST")
		}
		_, _ = io.WriteString(w, `{"models":[]}`)
	})
	handler := exactCaptureHandler(catalog, p, &posts, &refused)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, proxyRequest("GET", "/v1/models?client_version=0.155.0", ""))
	if w.Code != 200 || catalogRequests.Load() != 1 {
		t.Fatal("GET model catalog did not reach its isolated route")
	}
	const body = `{"model":"fixture","instructions":"private body <>&","input":[],"store":false,"stream":true,"reasoning":{"effort":"medium"},"client_metadata":{"turn_id":"fixed-turn"}}`
	r := proxyRequest("POST", "/v1/responses", body)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Session-Id", "fixed-session")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "synthetic_capture_only") {
		t.Fatalf("synthetic capture failed: %d %s", w.Code, w.Body.String())
	}
	raw, err := os.ReadFile(filepath.Join(output, "request.body"))
	if err != nil {
		t.Fatal(err)
	}
	prepared, _, err := prepareBody([]byte(body), false)
	if err != nil || string(raw) != string(prepared) {
		t.Fatal("capture did not preserve exact prepared native writer bytes")
	}
	head, err := os.ReadFile(filepath.Join(output, "request-head.json"))
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		Headers     [][2]string `json:"headers"`
		BodySHA     string      `json:"body_sha256"`
		Compression string      `json:"request_compression"`
	}
	if json.Unmarshal(head, &value) != nil || value.BodySHA != qualityDigest(raw) || value.Compression != "none" {
		t.Fatal("captured head contract")
	}
	for _, pair := range value.Headers {
		if strings.EqualFold(pair[0], "authorization") || strings.EqualFold(pair[0], "chatgpt-account-id") {
			t.Fatal("raw credential persisted in captured head")
		}
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, proxyRequest("POST", "/v1/responses", body))
	if w.Code != 409 || posts.Load() != 1 || refused.Load() != 1 || catalogRequests.Load() != 1 {
		t.Fatal("capture fixture retried or leaked model POST")
	}
}

func TestQualityExactCaptureOfflineCompressedWriter(t *testing.T) {
	output := t.TempDir()
	p := exactCaptureProxy(t, output)
	const original = `{"model":"fixture","instructions":"private <>&","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"exact source"}]}],"stream":true,"store":false}`
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	wire := encoder.EncodeAll([]byte(original), nil)
	encoder.Close()
	request := proxyRequest("POST", "/v1/responses", string(wire))
	request.Header.Set("Content-Encoding", "zstd")
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, request)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "synthetic_capture_only") {
		t.Fatal("compressed capture was not synthetic")
	}
	body, err := os.ReadFile(filepath.Join(output, "request.body"))
	if err != nil {
		t.Fatal(err)
	}
	prepared, _, err := prepareBody([]byte(original), false)
	if err != nil || string(body) != string(prepared) {
		t.Fatal("captured body was recompressed or changed")
	}
	raw, err := os.ReadFile(filepath.Join(output, "request-head.json"))
	if err != nil {
		t.Fatal(err)
	}
	var head struct {
		Compression string      `json:"request_compression"`
		Headers     [][2]string `json:"headers"`
	}
	if json.Unmarshal(raw, &head) != nil || head.Compression != "zstd" {
		t.Fatal("native compression intent lost")
	}
	for _, pair := range head.Headers {
		if strings.EqualFold(pair[0], "Content-Encoding") {
			t.Fatal("wire compression duplicated in captured headers")
		}
	}
}
