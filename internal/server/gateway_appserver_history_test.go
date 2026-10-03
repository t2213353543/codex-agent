package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var appServerHistoryTestPaths = []string{
	"/alpha/history/v2/list_windows", "/alpha/history/v2/list_items", "/alpha/history/v2/read_item", "/alpha/history/v2/search_contents",
	"/alpha/notes/v2/list_files_by_prefix", "/alpha/notes/v2/read_file", "/alpha/notes/v2/search_contents", "/alpha/notes/v2/append_to_file",
	"/alpha/notes/v2/write_file", "/alpha/notes/v2/thread_hint",
}

func appServerHistoryTestRequest(path, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-client-key")
	r.Header.Set("X-Openai-Tool-Output-Truncation-Policy", `{"mode":"bytes","limit":1024}`)
	r.Header.Set("X-Openai-Encrypted-Tool-Arguments", "true")
	r.Header.Set("Cookie", "client-cookie")
	r.Header.Set("X-Openai-Actor-Authorization", "client-actor-credential")
	r.Header.Set("Proxy-Authorization", "client-proxy-credential")
	return r.WithContext(withGatewayUnboundedPayload(withInferenceMode(r.Context(), inferenceCodexCLI), true))
}

func TestAppServerHistoryExactRoutesContextAndHeaders(t *testing.T) {
	p := appServerAuxiliaryTestProxy(t)
	raw := `{ "context":{"session_id":"history-fixture","current_agent_name":"/root/worker"},"query":"opaque-query","text":"opaque-note","path":"memo.md","window_id":"window_opaque","item_id":"item_opaque","unknown":9007199254740993 }`
	result := `{ "encrypted_output":"opaque-output", "images":[{"data":"cG5n","mime_type":"image/png","detail":"original"}],"unknown":9007199254740993 }`
	calls := 0
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		got, _ := io.ReadAll(req.Body)
		if string(got) != raw || !appServerRequiresOwner(req.Context()) || !gatewayUnboundedPayload(req.Context()) {
			t.Error("history request context, raw argument bytes or policy changed")
		}
		if !appServerHistoryPath(strings.TrimPrefix(req.URL.Path, "/backend-api/codex")) || req.URL.Host != "chatgpt.com" {
			t.Error("non-history upstream reached")
		}
		if req.Header.Get("X-Openai-Encrypted-Tool-Arguments") != "true" || req.Header.Get("X-Openai-Tool-Output-Truncation-Policy") != `{"mode":"bytes","limit":1024}` {
			t.Error("official encrypted argument/truncation controls changed")
		}
		if req.Header.Get("Authorization") != "Bearer test-upstream-token" || req.Header.Get("ChatGPT-Account-Id") != "test-account" || req.Header.Get("Cookie") != "" || req.Header.Get("X-Openai-Actor-Authorization") != "" || req.Header.Get("Proxy-Authorization") != "" {
			t.Error("history owner authentication or client credential isolation changed")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"fixture-history"}}, Body: io.NopCloser(strings.NewReader(result))}, nil
	})}
	for _, path := range appServerHistoryTestPaths {
		for _, prefix := range []string{"", "/v1"} {
			w := httptest.NewRecorder()
			p.gatewayAppServerHistory(w, appServerHistoryTestRequest(prefix+path, raw))
			if w.Code != 200 || w.Body.String() != result || w.Header().Get("X-Request-Id") != "fixture-history" {
				t.Fatalf("opaque history response changed for %s: %d", path, w.Code)
			}
		}
	}
	if calls != 20 {
		t.Fatalf("wrong exact-route count: %d", calls)
	}
	last, ok := p.Metrics.LastFor("test")
	if !ok || last.UsageKnown != nil || last.InputTokens != 0 || last.OutputTokens != 0 || last.CachedTokens != 0 {
		t.Fatal("invented history inference usage")
	}
}

func TestAppServerHistoryRejectsUnknownPathsAndConflictingIdentity(t *testing.T) {
	p := appServerAuxiliaryTestProxy(t)
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid history request reached upstream")
		return nil, nil
	})}
	for _, path := range []string{"/alpha/notes/v2/delete_file", "/alpha/notes/v2/write_file/", "/alpha/history/v2/read_item?account=other", "/alpha/notes/v2/%77rite_file"} {
		w := httptest.NewRecorder()
		p.gatewayAppServerHistory(w, appServerHistoryTestRequest(path, `{}`))
		if w.Code != 404 {
			t.Fatalf("unknown route accepted: %s %d", path, w.Code)
		}
	}
	for _, body := range []string{`{}`, `[]`, `{"context":{"session_id":"owner"}}`, `{"Context":{"session_id":"owner","current_agent_name":"/root"}}`, `{"context":{"session_id":"body-owner","current_agent_name":"/root"}}`} {
		r := appServerHistoryTestRequest("/alpha/notes/v2/thread_hint", body)
		r.Header.Set("Session-Id", "header-owner")
		w := httptest.NewRecorder()
		p.gatewayAppServerHistory(w, r)
		if w.Code != 400 || strings.Contains(w.Body.String(), "body-owner") || strings.Contains(w.Body.String(), "header-owner") {
			t.Fatal("invalid/mismatched session was admitted or leaked")
		}
	}
	r := appServerHistoryTestRequest("/alpha/notes/v2/thread_hint", `{}`)
	r.Method = http.MethodGet
	w := httptest.NewRecorder()
	p.gatewayAppServerHistory(w, r)
	if w.Code != 405 {
		t.Fatal("history accepted non-POST")
	}
}

func TestAppServerHistoryOwnerSurvivesDisabledStickyAndPriorityChange(t *testing.T) {
	p := appServerAuxiliaryTestProxy(t)
	settings, _ := p.Settings.Get()
	settings.StickySessionsEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; return nil }); err != nil {
		t.Fatal(err)
	}
	other, _ := json.Marshal(map[string]any{"access_token": "other", "expires_at": time.Now().Add(time.Hour).Unix()})
	if err := p.Store.ImportWith("other", other, AccountDefaults{Concurrency: 1, Priority: 0}); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	var calls []string
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.Header.Get("Authorization"))
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"text":"fixture"}`))}, nil
	})}
	body := `{"context":{"session_id":"owner-session","current_agent_name":"/root"}}`
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		p.gatewayAppServerHistory(w, appServerHistoryTestRequest("/alpha/notes/v2/thread_hint", body))
		if w.Code != 200 {
			t.Fatalf("bound history request failed: %d %s", w.Code, w.Body.String())
		}
		if err := p.Store.UpdateAccount("other", func(a *Account) error { a.Priority = 1000; return nil }); err != nil {
			t.Fatal(err)
		}
		p.invalidateAccounts()
	}
	if len(calls) != 2 || calls[0] != "Bearer test-upstream-token" || calls[1] != calls[0] {
		t.Fatalf("history moved owners after priority change: %v", calls)
	}
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	w := httptest.NewRecorder()
	p.gatewayAppServerHistory(w, appServerHistoryTestRequest("/alpha/notes/v2/read_file", body))
	if w.Code < 400 || len(calls) != 2 {
		t.Fatal("unavailable history owner silently switched to another account")
	}
}

func TestAppServerHistoryWriteDoesNotReplayAmbiguousOrRejectedOperation(t *testing.T) {
	for _, failure := range []string{"transport", "read-after-accepted", "unauthorized", "server-error"} {
		t.Run(failure, func(t *testing.T) {
			p := appServerAuxiliaryTestProxy(t)
			calls := 0
			p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				_, _ = io.Copy(io.Discard, r.Body)
				if failure == "transport" {
					return nil, errors.New("synthetic ambiguous write")
				}
				status := 200
				var body io.ReadCloser = io.NopCloser(strings.NewReader(`{"error":{"code":"unauthorized"}}`))
				if failure == "read-after-accepted" {
					body = io.NopCloser(errorReader{})
				}
				if failure == "unauthorized" {
					status = 401
				}
				if failure == "server-error" {
					status = 503
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}, nil
			})}
			w := httptest.NewRecorder()
			p.gatewayAppServerHistory(w, appServerHistoryTestRequest("/alpha/notes/v2/append_to_file", `{"context":{"session_id":"owner-write","current_agent_name":"/root"},"path":"memo.md","text":"opaque"}`))
			if calls != 1 || w.Code < 400 {
				t.Fatalf("write was replayed or failure hidden: calls=%d status=%d", calls, w.Code)
			}
		})
	}
}

func TestAppServerHistorySameOwner401RefreshAndAccountConcurrency(t *testing.T) {
	t.Run("refresh", func(t *testing.T) {
		p := appServerAuxiliaryTestProxy(t)
		settings, _ := p.Settings.Get()
		settings.Upstream401RecoveryEnabled = true
		settings.AutomaticTokenRefreshEnabled = true
		if err := p.Settings.Set(settings); err != nil {
			t.Fatal(err)
		}
		var tokens []string
		refreshes := 0
		body := `{"context":{"session_id":"refresh-owner","current_agent_name":"/root"},"path":"memo.md","text":"opaque"}`
		p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
			status, result := 200, `{"text":"ok"}`
			if r.URL.Path == "/oauth/token" {
				refreshes++
				result = gatewayKnownOwnerRefreshFixture(`{"access_token":"refreshed","expires_in":3600}`, "test-account", "fixture-user")
			} else {
				tokens = append(tokens, r.Header.Get("Authorization"))
				raw, _ := io.ReadAll(r.Body)
				if string(raw) != body {
					t.Error("refresh replay changed write bytes")
				}
				if len(tokens) == 1 {
					status, result = 401, `{"error":{"code":"unauthorized"}}`
				}
			}
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(result))}, nil
		})}
		w := httptest.NewRecorder()
		p.gatewayAppServerHistory(w, appServerHistoryTestRequest("/alpha/notes/v2/write_file", body))
		if w.Code != 200 || refreshes != 1 || len(tokens) != 2 || tokens[1] != "Bearer refreshed" {
			t.Fatalf("same-owner refresh failed: status=%d refreshes=%d tokens=%v", w.Code, refreshes, tokens)
		}
	})
	t.Run("busy", func(t *testing.T) {
		p := appServerAuxiliaryTestProxy(t)
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			close(entered)
			<-release
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		})}
		body := `{"context":{"session_id":"busy-owner","current_agent_name":"/root"}}`
		done := make(chan struct{})
		go func() {
			defer close(done)
			p.gatewayAppServerHistory(httptest.NewRecorder(), appServerHistoryTestRequest("/alpha/notes/v2/thread_hint", body))
		}()
		<-entered
		w := httptest.NewRecorder()
		p.gatewayAppServerHistory(w, appServerHistoryTestRequest("/alpha/notes/v2/thread_hint", body))
		close(release)
		<-done
		if w.Code != 429 || calls.Load() != 1 || p.inFlight("test") != 0 {
			t.Fatal("history bypassed account concurrency or leaked lease")
		}
	})
}

func TestAppServerHistoryConcurrentOwnerChangeCannotExposeResult(t *testing.T) {
	p := appServerAuxiliaryTestProxy(t)
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		owner, err := p.sessionAccount(r.Context(), "admin", "history-owner-change", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = p.switchSessionAccount(r.Context(), "admin", "history-owner-change", owner, "concurrent-owner"); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"encrypted_output":"must-not-deliver"}`))}, nil
	})}
	w := httptest.NewRecorder()
	p.gatewayAppServerHistory(w, appServerHistoryTestRequest("/alpha/history/v2/read_item", `{"context":{"session_id":"history-owner-change","current_agent_name":"/root"},"window_id":"opaque-window","item_id":"opaque-item"}`))
	if w.Code != 409 || strings.Contains(w.Body.String(), "must-not-deliver") {
		t.Fatal("concurrent account binding change exposed history result")
	}
}

func TestAppServerHistoryOfficialWriterOracleOverHTTP(t *testing.T) {
	raw, err := os.ReadFile("testdata/appserver_history_official_oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var oracle struct {
		OfficialBinarySHA string `json:"official_binary_sha256"`
		Loopback          bool   `json:"only_loopback_available"`
		Operations        []struct {
			Method, Path string
			Headers      map[string]string
			Body         string `json:"body_utf8"`
			SHA          string `json:"body_sha256"`
		} `json:"operations"`
	}
	if err = json.Unmarshal(raw, &oracle); err != nil {
		t.Fatal(err)
	}
	if !oracle.Loopback || oracle.OfficialBinarySHA != "660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501" || len(oracle.Operations) != 10 {
		t.Fatal("invalid official runtime oracle")
	}
	received := make(chan *http.Request, 1)
	bodies := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- r
		bodies <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"encrypted_output":"synthetic-returned-output"}`)
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	p := appServerAuxiliaryTestProxy(t)
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		clone.URL.Scheme, clone.URL.Host = upstreamURL.Scheme, upstreamURL.Host
		return http.DefaultTransport.RoundTrip(clone)
	})}
	gateway := httptest.NewServer(p)
	defer gateway.Close()
	for _, operation := range oracle.Operations {
		want := sha256.Sum256([]byte(operation.Body))
		if hex.EncodeToString(want[:]) != operation.SHA {
			t.Fatal("official captured body hash changed")
		}
		path := strings.TrimPrefix(operation.Path, "/backend-api/codex")
		req, err := http.NewRequestWithContext(t.Context(), operation.Method, gateway.URL+"/v1"+path, strings.NewReader(operation.Body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer test-client-key")
		for name, value := range operation.Headers {
			req.Header.Set(name, value)
		}
		res, err := gateway.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		result, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != 200 || !bytes.Contains(result, []byte("synthetic-returned-output")) {
			t.Fatalf("captured request rejected by gateway: path=%s status=%d response=%s", path, res.StatusCode, result)
		}
		forwarded, body := <-received, <-bodies
		if forwarded.URL.Path != operation.Path || sha256.Sum256(body) != want {
			t.Fatalf("captured request writer changed on %s", path)
		}
		for _, name := range []string{"x-openai-encrypted-tool-arguments", "x-openai-tool-output-truncation-policy"} {
			if forwarded.Header.Get(name) != operation.Headers[name] {
				t.Fatalf("captured control header changed on %s: %s", path, name)
			}
		}
	}
}
