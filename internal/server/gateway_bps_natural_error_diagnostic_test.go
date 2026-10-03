package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func resetNaturalDiagnosticCache(t *testing.T) {
	t.Helper()
	naturalFailureDiagnosticMu.Lock()
	clear(naturalFailureDiagnosticRecent)
	naturalFailureDiagnosticMu.Unlock()
	t.Cleanup(func() {
		naturalFailureDiagnosticMu.Lock()
		clear(naturalFailureDiagnosticRecent)
		naturalFailureDiagnosticMu.Unlock()
	})
}

func TestBPSNaturalDiagnosticOffSuccessAndExactError(t *testing.T) {
	resetNaturalDiagnosticCache(t)
	root := t.TempDir()
	p := &Proxy{Store: Store{Path: filepath.Join(root, "accounts.json")}}
	account := Account{ID: "private-row", Name: "private-label", AccountID: "private-workspace", AccessToken: "private-access", RefreshToken: "private-refresh"}
	failure := []byte(`{"type":"response.failed","response":{"id":"private-response","status":"failed","output":[{"text":"PRIVATE_OUTPUT"}],"input":"PRIVATE_INPUT","headers":{"Authorization":"PRIVATE_HEADER"},"error":{"code":"request_rejected","type":"future_error","param":"input[0]","status":400,"message":"PowerPoint private-access private-refresh private-workspace private-row private-label rotated-access client-key"}}}`)
	t.Setenv("CODEX_NATURAL_FAILURE_DIAGNOSTICS", "")
	p.captureBPSNaturalFailure(account, failure)
	if _, err := os.Stat(filepath.Join(root, "diagnostics")); !os.IsNotExist(err) {
		t.Fatal("disabled diagnostic touched disk")
	}
	t.Setenv("CODEX_NATURAL_FAILURE_DIAGNOSTICS", "1")
	p.captureBPSNaturalFailure(account, []byte(`{"type":"response.completed","response":{"status":"completed"}}`))
	if _, err := os.Stat(filepath.Join(root, "diagnostics")); !os.IsNotExist(err) {
		t.Fatal("successful terminal touched disk")
	}
	p.captureBPSNaturalFailure(account, failure, "rotated-access", "client-key")
	files, err := filepath.Glob(filepath.Join(root, "diagnostics", "natural-failure-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("failed diagnostic absent: %v %v", files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{account.ID, account.Name, account.AccountID, account.AccessToken, account.RefreshToken, "private-response", "rotated-access", "client-key", "PRIVATE_INPUT", "PRIVATE_OUTPUT", "PRIVATE_HEADER", "Authorization", "PowerPoint"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("private or unselected field retained: %s", private)
		}
	}
	var record naturalFailureDiagnostic
	if json.Unmarshal(raw, &record) != nil || record.ErrorCode != "request_rejected" || record.ErrorType != "future_error" || record.ErrorParam != "input[0]" || record.ErrorStatus != "400" || record.EventType != "response.failed" {
		t.Fatal("exact error metadata not retained")
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(files[0])
		dir, _ := os.Stat(filepath.Dir(files[0]))
		if info.Mode().Perm() != 0600 || dir.Mode().Perm() != 0700 {
			t.Fatal("diagnostic mode not private")
		}
	}
	// Sampling is not a second classifier: camelCase or future recognized codes
	// remain visible and cannot disappear due to differing allowlists.
	known, ok := projectNaturalFailureDiagnostic(account, []byte(`{"type":"error","code":"rateLimitExceeded","message":"rate limit"}`), time.Now())
	if !ok || known.ErrorCode != "rateLimitExceeded" {
		t.Fatal("camelCase diagnostic was hidden")
	}
}

func TestBPSNaturalDiagnosticStateDirectoryAndMessageBound(t *testing.T) {
	resetNaturalDiagnosticCache(t)
	root := t.TempDir()
	t.Setenv("STATE_DIRECTORY", root)
	t.Setenv("CODEX_NATURAL_FAILURE_DIAGNOSTICS", "1")
	p := &Proxy{Store: Store{DB: &Database{}}}
	event, _ := json.Marshal(map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed", "error": map[string]string{"code": "unknown_code", "message": strings.Repeat("测", 800)}}})
	p.captureBPSNaturalFailure(Account{ID: "state-account"}, event)
	raw, err := os.ReadFile(filepath.Join(root, "diagnostics", "natural-failure-00.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record naturalFailureDiagnostic
	if json.Unmarshal(raw, &record) != nil || len([]rune(record.ErrorMessage)) != 400 {
		t.Fatal("message not bounded to400characters")
	}
}

func TestBPSNaturalDiagnosticRateRotationAndFailureBudget(t *testing.T) {
	resetNaturalDiagnosticCache(t)
	dir := filepath.Join(t.TempDir(), "diagnostics")
	stamp := time.Now().Unix()
	first := naturalFailureDiagnostic{Schema: 1, CapturedAt: stamp, AccountSHA256: "a", ErrorCode: "first"}
	if err := storeNaturalFailureDiagnostic(dir, first); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 20 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			copy := first
			copy.CapturedAt++
			copy.ErrorCode = "duplicate"
			_ = storeNaturalFailureDiagnostic(dir, copy)
		}()
	}
	workers.Wait()
	files, _ := filepath.Glob(filepath.Join(dir, "natural-failure-*.json"))
	if len(files) != 1 {
		t.Fatal("same-account concurrent samples exceeded budget")
	}
	for i := 1; i < 40; i++ {
		copy := first
		copy.AccountSHA256 = fmt.Sprint(i)
		if err := storeNaturalFailureDiagnostic(dir, copy); err != nil {
			t.Fatal(err)
		}
	}
	files, _ = filepath.Glob(filepath.Join(dir, "natural-failure-*.json"))
	if len(files) != 32 || len(naturalFailureDiagnosticRecent) > 32 {
		t.Fatal("global retention/cache exceeded32")
	}
	// Simulate a new process: persisted samples still block the same account.
	naturalFailureDiagnosticMu.Lock()
	clear(naturalFailureDiagnosticRecent)
	naturalFailureDiagnosticMu.Unlock()
	copy := first
	copy.CapturedAt = stamp + 2
	copy.ErrorCode = "same-minute-restart"
	if err := storeNaturalFailureDiagnostic(dir, copy); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "natural-failure-00.json"))
	if strings.Contains(string(raw), "same-minute") {
		t.Fatal("restart bypassed sampling budget")
	}
	copy.AccountSHA256 = "new-minute"
	copy.CapturedAt = stamp + 61
	copy.ErrorCode = "rotated"
	if err := storeNaturalFailureDiagnostic(dir, copy); err != nil {
		t.Fatal(err)
	}
	files, _ = filepath.Glob(filepath.Join(dir, "natural-failure-*.json"))
	if len(files) != 32 {
		t.Fatal("rotation grew retention")
	}
	bad := filepath.Join(t.TempDir(), "not-directory")
	if err := os.WriteFile(bad, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	copy.AccountSHA256 = "storage-failure"
	if err := storeNaturalFailureDiagnostic(bad, copy); err == nil {
		t.Fatal("fixture did not fail storage")
	}
	if err := storeNaturalFailureDiagnostic(bad, copy); err != nil {
		t.Fatal("storage failure was not suppressed during60s window")
	}
}

func TestBPSNaturalDiagnosticHookPreservesFailureDelivery(t *testing.T) {
	resetNaturalDiagnosticCache(t)
	t.Setenv("CODEX_NATURAL_FAILURE_DIAGNOSTICS", "1")
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+`{"type":"response.failed","response":{"id":"diag-failure","status":"failed","output":[],"error":{"code":"future_failure","message":"synthetic failure"}}}`+"\n\n")
	})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", `{"model":"gpt-6-astra","input":[],"stream":false}`))
	if w.Code != 502 || !strings.Contains(w.Body.String(), "future_failure") {
		t.Fatal("diagnostic changed public failure")
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(p.Store.Path), "diagnostics", "natural-failure-*.json"))
	if len(files) != 1 {
		t.Fatal("OnUpstreamTerminal diagnostic hook not called")
	}
}

func TestBPSNaturalHTTPDiagnosticPreservesResponseAndRedacts(t *testing.T) {
	resetNaturalDiagnosticCache(t)
	t.Setenv("CODEX_NATURAL_FAILURE_DIAGNOSTICS", "1")
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.Header().Set("Set-Cookie", "PRIVATE_COOKIE")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"rateLimitExceeded","type":"quota_error","message":"test-upstream-token PRIVATE_DIAGNOSTIC"},"input":"DO_NOT_CAPTURE"}`)
	})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", `{"model":"gpt-6-astra","input":[],"stream":false}`))
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "rateLimitExceeded") || w.Header().Get("Retry-After") != "17" {
		t.Fatalf("diagnostic changed HTTP response: %d %s", w.Code, w.Body.String())
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(p.Store.Path), "diagnostics", "natural-failure-*.json"))
	if len(files) != 1 {
		t.Fatalf("HTTP failure sample count: %d", len(files))
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var got naturalFailureDiagnostic
	if json.Unmarshal(raw, &got) != nil || got.HTTPStatus != 429 || got.ErrorCode != "rateLimitExceeded" || got.RetryAfter == nil || *got.RetryAfter != 17 || got.EventType != "http.error" {
		t.Fatalf("HTTP diagnostic fields missing: %s", raw)
	}
	for _, secret := range []string{"test-upstream-token", "PRIVATE_COOKIE", "DO_NOT_CAPTURE", "Set-Cookie"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("private field captured: %s", secret)
		}
	}
}

func TestBPSNaturalHTTPDiagnosticRetryMetadataAndUnstructuredBody(t *testing.T) {
	for _, tc := range []struct{ name, retry, body, code string }{
		{"date", "Wed, 21 Oct 2015 07:28:00 GMT", `{"detail":{"code":"billing_not_active","message":"fixture"}}`, "billing_not_active"},
		{"unstructured", "private-invalid-header", "<html>PRIVATE_HTML_TOKEN</html>", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetNaturalDiagnosticCache(t)
			t.Setenv("CODEX_NATURAL_FAILURE_DIAGNOSTICS", "1")
			dir := t.TempDir()
			p := &Proxy{Store: Store{Path: filepath.Join(dir, "accounts.json")}}
			account := Account{ID: "private-http-row", AccessToken: "private-old-token"}
			ctx := context.WithValue(t.Context(), gatewayCooldownAccountContextKey{}, account)
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://fixture.invalid/models", nil)
			req.Header.Set("Authorization", "Bearer private-current-token")
			response := &http.Response{StatusCode: 429, Proto: "HTTP/2.0", Request: req, Header: http.Header{"Retry-After": []string{tc.retry}}}
			p.captureBPSNaturalHTTPFailure(response, []byte(tc.body))
			raw, err := os.ReadFile(filepath.Join(dir, "diagnostics", "natural-failure-00.json"))
			if err != nil {
				t.Fatal(err)
			}
			var got naturalFailureDiagnostic
			if json.Unmarshal(raw, &got) != nil || got.ErrorCode != tc.code || got.HTTPProtocol != "HTTP/2.0" {
				t.Fatal(string(raw))
			}
			if tc.name == "date" && got.RetryAt != "2015-10-21T07:28:00Z" {
				t.Fatal("HTTP date not preserved as parsed timestamp")
			}
			if tc.name == "unstructured" && (got.RetryAt != "" || got.RetryAfter != nil || got.ErrorMessage != "") {
				t.Fatal("unstructured content retained")
			}
			if strings.Contains(string(raw), "PRIVATE_HTML_TOKEN") || strings.Contains(string(raw), "private-invalid-header") {
				t.Fatal("unselected text captured")
			}
		})
	}
}
