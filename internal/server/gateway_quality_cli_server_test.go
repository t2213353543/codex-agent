package server

// Explicit server-side live experiment support. This exposes one isolated
// account on loopback for an unmodified official CLI and accepts one model
// request; all other model calls are refused rather than retried by the probe.
import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestQualityOfficialCLILoopbackGateway(t *testing.T) {
	path := os.Getenv("CODEX_QUALITY_CLI_SERVER_CONFIG")
	if path == "" {
		t.Skip("explicit private live CLI server configuration required")
	}
	if runtime.GOOS != "linux" || os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN") == "" {
		t.Fatal("explicit Linux native helper required")
	}
	var config struct {
		CredentialFile string   `json:"credential_file"`
		Output         string   `json:"output"`
		Settings       Settings `json:"settings"`
	}
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &config) != nil || config.Output == "" || validateSettings(config.Settings) != nil {
		t.Fatal("invalid private server configuration")
	}
	var secret struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
		Exp         int64  `json:"exp"`
	}
	raw, err = os.ReadFile(config.CredentialFile)
	if err != nil || json.Unmarshal(raw, &secret) != nil || secret.AccessToken == "" || secret.AccountID == "" || secret.Exp < time.Now().Add(20*time.Minute).Unix() {
		t.Fatal("invalid credential snapshot")
	}
	// CLI can fetch models concurrently with its one inference request. Keep
	// catalog admission from fabricating a local 429 in this experiment.
	p := newTestProxy(t, 4)
	p.AdminKey = secret.AccessToken
	p.BaseURL, p.TokenURL = codexURL, "http://127.0.0.1:9/refresh-disabled"
	if err := p.Store.UpdateAccount("test", func(a *Account) error {
		a.AccessToken, a.AccountID, a.ExpiresAt = secret.AccessToken, secret.AccountID, secret.Exp
		a.RefreshToken, a.Identity = "", nil
		return nil
	}); err != nil {
		t.Fatal("cannot initialize account")
	}
	settings := config.Settings
	settings.Version = 0
	settings.BuiltinPromptEnabled, settings.CodexFingerprintEnabled, settings.CompatibilityNormalizationEnabled, settings.PromptCacheKeyEnabled = false, false, false, false
	settings.AutomaticTokenRefreshEnabled, settings.Upstream401RecoveryEnabled = false, false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal("cannot set transparent settings")
	}
	realGatewayNativePool(t, p)
	var mu sync.Mutex
	requests, refused := 0, 0
	var ingress []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			p.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			http.Error(w, "experiment route unavailable", http.StatusNotFound)
			return
		}
		mu.Lock()
		if requests != 0 {
			refused++
			mu.Unlock()
			http.Error(w, "single request experiment exhausted", http.StatusConflict)
			return
		}
		requests++
		mu.Unlock()
		// Preserve the original compressed request body, while separately
		// decoding a copy through the real bounded request reader for its SHA.
		wire, readErr := io.ReadAll(io.LimitReader(r.Body, maxCompressedBody+1))
		_ = r.Body.Close()
		if readErr != nil || len(wire) > maxCompressedBody {
			http.Error(w, "request capture failed", 400)
			return
		}
		copy := r.Clone(r.Context())
		copy.Body = io.NopCloser(bytes.NewReader(wire))
		decoded, decodeErr := readBody(httptest.NewRecorder(), copy)
		if decodeErr != nil {
			http.Error(w, "request decode failed", 400)
			return
		}
		mu.Lock()
		ingress = append(ingress, map[string]any{"wire_bytes": len(wire), "wire_sha256": qualityDigest(wire), "decoded_bytes": len(decoded), "decoded_sha256": qualityDigest(decoded), "content_encoding": gatewayRequestContentEncoding(r.Header), "authorization_token_sha256": qualityDigest([]byte(bearerToken(r))), "workspace_sha256": qualityDigest([]byte(r.Header.Get("ChatGPT-Account-Id"))), "request_id_sha256": qualityDigest([]byte(r.Header.Get("X-Client-Request-Id")))})
		mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(wire))
		p.ServeHTTP(w, r)
	}))
	defer server.Close()
	if err := os.MkdirAll(config.Output, 0700); err != nil {
		t.Fatal(err)
	}
	ready, _ := json.Marshal(map[string]any{"base_url": server.URL + "/v1", "model_request_limit": 1})
	if err := os.WriteFile(filepath.Join(config.Output, "ready.json"), ready, 0600); err != nil {
		t.Fatal(err)
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
	p.closeGatewayClients()
	mu.Lock()
	report := map[string]any{"inference_requests": requests, "extra_requests_refused": refused, "ingress": ingress}
	mu.Unlock()
	var outcomes []map[string]any
	for _, rec := range p.Metrics.RecentForAccount("test", 8) {
		outcomes = append(outcomes, map[string]any{"status": rec.Status, "http_code": rec.HTTPCode, "request_type": rec.RequestType, "sent_model": rec.SentModel, "returned_model": rec.ReturnedModel, "reasoning_effort": rec.ReasoningEffort, "service_tier": rec.ServiceTier, "terminal_status": rec.TerminalStatus, "attempts": len(rec.Attempts), "error_source": rec.ErrorSource, "error_phase": rec.ErrorPhase})
	}
	report["outcomes"] = outcomes
	encoded, _ := json.Marshal(report)
	if err := os.WriteFile(filepath.Join(config.Output, "server-report.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
}
