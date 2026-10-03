package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestOfficialFailoverRPCFixture(t *testing.T) {
	active := false
	for _, arg := range os.Args {
		if arg == "--failover-rpc-fixture" {
			active = true
		}
	}
	if !active {
		t.Skip("subprocess fixture")
	}
	encoder := json.NewEncoder(os.Stdout)
	bad := false
	partial, authFirst := false, false
	quotaCode := ""
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		var request struct {
			ID     json.RawMessage
			Method string
			Params map[string]json.RawMessage
		}
		if json.Unmarshal(scan.Bytes(), &request) != nil {
			os.Exit(2)
		}
		if len(request.ID) == 0 {
			continue
		}
		reply := func(value any) { _ = encoder.Encode(map[string]any{"id": request.ID, "result": value}) }
		notify := func(method string, value any) { _ = encoder.Encode(map[string]any{"method": method, "params": value}) }
		switch request.Method {
		case "initialize":
			reply(map[string]any{})
		case "account/login/start":
			bad = string(request.Params["accessToken"]) == `"bad-catalog"`
			partial = string(request.Params["accessToken"]) == `"partial-auth"`
			authFirst = string(request.Params["accessToken"]) == `"auth-before-output"`
			quotaCode = ""
			for _, code := range []string{"usageLimitExceeded", "rateLimitExceeded"} {
				if string(request.Params["accessToken"]) == `"`+code+`"` {
					quotaCode = code
				}
			}
			reply(map[string]any{})
		case "model/list":
			model := "model-a"
			if bad {
				model = "other-model"
			}
			raw, _ := json.Marshal(map[string]any{"models": []any{map[string]any{"slug": model, "tool_mode": "direct"}}})
			_ = os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "models_cache.json"), raw, 0600)
			reply(map[string]any{"data": []any{map[string]any{"model": model}}})
		case "thread/start":
			reply(map[string]any{"thread": map[string]any{"id": "thread-one", "path": filepath.Join(os.Getenv("CODEX_HOME"), "rollout.jsonl")}, "model": "model-a"})
		case "thread/inject_items":
			reply(map[string]any{})
		case "turn/start":
			reply(map[string]any{"turn": map[string]any{"id": "turn-one"}})
			if quotaCode != "" {
				notify("error", map[string]any{"threadId": "thread-one", "turnId": "turn-one", "error": map[string]any{"codexErrorInfo": quotaCode, "message": "synthetic account quota unavailable"}})
				continue
			}
			if authFirst {
				notify("error", map[string]any{"threadId": "thread-one", "turnId": "turn-one", "error": map[string]any{"codexErrorInfo": "unauthorized", "message": "synthetic account expired"}})
				continue
			}
			notify("item/agentMessage/delta", map[string]any{"threadId": "thread-one", "turnId": "turn-one", "itemId": "msg-one", "delta": "OK"})
			if partial {
				notify("error", map[string]any{"threadId": "thread-one", "turnId": "turn-one", "error": map[string]any{"codexErrorInfo": "unauthorized", "message": "synthetic account expired after output"}})
				continue
			}
			notify("rawResponseItem/completed", map[string]any{"threadId": "thread-one", "turnId": "turn-one", "item": map[string]any{"id": "msg-one", "type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "OK"}}}})
			notify("rawResponse/completed", map[string]any{"threadId": "thread-one", "turnId": "turn-one", "responseId": "native-one", "usage": map[string]int{"inputTokens": 23, "outputTokens": 2, "totalTokens": 25, "cachedInputTokens": 16}})
			notify("turn/completed", map[string]any{"threadId": "thread-one", "turn": map[string]any{"id": "turn-one", "status": "completed"}})
		case "thread/compact/start":
			reply(map[string]any{})
			_ = os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "rollout.jsonl"), []byte(`{"type":"compacted","payload":{"compaction_response_id":"cmp-one","replacement_history":[{"type":"compaction","encrypted_content":"unchanged-cipher"}]}}`+"\n"), 0600)
			notify("turn/started", map[string]any{"threadId": "thread-one", "turn": map[string]any{"id": "compact-one"}})
			notify("rawResponse/completed", map[string]any{"threadId": "thread-one", "turnId": "compact-one", "responseId": "cmp-one"})
			notify("turn/completed", map[string]any{"threadId": "thread-one", "turn": map[string]any{"id": "compact-one", "status": "completed"}})
		default:
			os.Exit(3)
		}
	}
	os.Exit(0)
}

func TestOfficialFailoverDifferentAccountSuccessPreservesSingleRecordAndCompressedInput(t *testing.T) {
	for _, mode := range []string{"stream", "json", "zstd", "compact", "auth-before-output", "partial-auth", "five-accounts", "usageLimitExceeded", "rateLimitExceeded"} {
		t.Run(mode, func(t *testing.T) {
			p := newTestProxy(t, 1)
			p.RuntimeModes = true
			p.BPS = &bpsRuntime{}
			rt, err := newOfficialRuntime(func(k string) string {
				if k == "CODEX_RUNTIME_DIR" {
					return t.TempDir()
				}
				return ""
			})
			if err != nil {
				t.Fatal(err)
			}
			p.Official = rt
			t.Cleanup(func() { _ = rt.Close() })
			rt.command = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				bin, _ := os.Executable()
				return exec.CommandContext(ctx, bin, "-test.run=^TestOfficialFailoverRPCFixture$", "--", "--failover-rpc-fixture")
			}
			if err := p.Store.UpdateAccount("test", func(a *Account) error {
				a.Priority = 100
				a.AccessToken = "bad-catalog"
				if mode == "partial-auth" || mode == "auth-before-output" || mode == "usageLimitExceeded" || mode == "rateLimitExceeded" {
					a.AccessToken = mode
				}
				a.RefreshToken = ""
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"access_token": "good-catalog", "expires_at": time.Now().Add(time.Hour).Unix()})
			if err := p.Store.ImportWith("good", raw, AccountDefaults{Concurrency: 1, Priority: 0}); err != nil {
				t.Fatal(err)
			}
			if mode == "five-accounts" {
				for n := 1; n <= 3; n++ {
					raw, _ := json.Marshal(map[string]any{"access_token": "bad-catalog", "expires_at": time.Now().Add(time.Hour).Unix()})
					if err := p.Store.ImportWith(fmt.Sprintf("bad-%d", n), raw, AccountDefaults{Concurrency: 1, Priority: 100 - n}); err != nil {
						t.Fatal(err)
					}
				}
			}
			p.invalidateAccounts()
			s, _ := p.Settings.Get()
			s.InferenceMode = inferenceAppServer
			s.BuiltinPromptEnabled = false
			s.AutomaticTokenRefreshEnabled = false
			s.Upstream401RecoveryEnabled = false
			_ = p.Settings.Set(s)
			path := "/v1/responses"
			if mode == "compact" {
				path += "/compact"
			}
			stream := mode == "stream" || mode == "partial-auth" || mode == "auth-before-output" || mode == "usageLimitExceeded" || mode == "rateLimitExceeded"
			body := fmt.Sprintf(`{"model":"model-a","stream":%t,"input":"Reply OK"}`, stream)
			req := httptest.NewRequest("POST", path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer test-client-key")
			req.Header.Set("Session-Id", "same-session")
			if mode == "zstd" {
				encoder, _ := zstd.NewWriter(nil)
				compressed := encoder.EncodeAll([]byte(body), nil)
				encoder.Close()
				req = httptest.NewRequest("POST", path, strings.NewReader(string(compressed)))
				req.Header.Set("Authorization", "Bearer test-client-key")
				req.Header.Set("Session-Id", "same-session")
				req.Header.Set("Content-Encoding", "zstd")
			}
			out := httptest.NewRecorder()
			p.ServeHTTP(out, req)
			if out.Code != 200 || mode != "partial-auth" && strings.Contains(out.Body.String(), `"error"`) {
				t.Fatalf("failover failed: %d %s", out.Code, out.Body.String())
			}
			p.Metrics.mu.Lock()
			records := append([]RequestRecord(nil), p.Metrics.recent[:p.Metrics.recentAt]...)
			p.Metrics.mu.Unlock()
			if mode == "partial-auth" {
				if len(records) != 1 || len(records[0].Attempts) != 1 || records[0].Account != "test" || records[0].Status != "error" || !strings.Contains(out.Body.String(), "unauthorized") {
					t.Fatalf("visible partial output incorrectly replayed: %+v %s", records, out.Body.String())
				}
				return
			}
			wantAttempts := 2
			if mode == "five-accounts" {
				wantAttempts = 4
			}
			if len(records) != 1 || len(records[0].Attempts) != wantAttempts || records[0].Account != "good" || records[0].Status != "ok" {
				t.Fatalf("incorrect final accounting: %+v", records)
			}
			if mode == "five-accounts" && (records[0].AttemptsTruncated == nil || !*records[0].AttemptsTruncated) {
				t.Fatal("fifth account success must retain bounded diagnostics and mark truncated")
			}
			if mode != "compact" && (records[0].InputTokens != 23 || records[0].CachedTokens != 16 || records[0].UsageKnown == nil || !*records[0].UsageKnown) {
				t.Fatalf("usage lost: %+v", records[0])
			}
			bound, err := p.sessionAccount(t.Context(), "admin", "same-session", "")
			if err != nil {
				t.Fatal(err)
			}
			accounts, _, _, _ := p.candidatesForContext(t.Context(), "test-client-key")
			for _, a := range accounts {
				if a.Name == "good" && bound != gatewayAccountRef(a) {
					t.Fatal("successful owner was not migrated")
				}
			}
			if stream && strings.Count(out.Body.String(), `"type":"response.created"`) != 1 {
				t.Fatal("failed attempt metadata leaked")
			}
		})
	}
}

func TestOfficialFailoverWriterWaitsForRealOutputAndRejectsCASConflict(t *testing.T) {
	for _, event := range []string{`{"type":"response.output_text.delta","delta":"hello"}`, `{"type":"response.output_item.done","item":{"type":"tool_search_call","arguments":{"query":"tool"}}}`, `{"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"cipher"}}`} {
		out := httptest.NewRecorder()
		commits := 0
		state := &officialFailoverState{commit: func() error { commits++; return nil }}
		writer := &officialFailoverWriter{target: out, header: make(http.Header), state: state}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"first\"}}\n\n"))
		writer.Flush()
		if out.Body.Len() != 0 || commits != 0 {
			t.Fatal("metadata escaped before actual output")
		}
		if _, err := writer.Write([]byte("data: " + event + "\n\n")); err != nil {
			t.Fatal(err)
		}
		if !writer.delivered || commits != 1 || out.Body.Len() == 0 {
			t.Fatal("real output did not commit")
		}
	}
	out := httptest.NewRecorder()
	state := &officialFailoverState{commit: func() error { return errors.New("CAS conflict") }}
	w := &officialFailoverWriter{target: out, header: make(http.Header), state: state}
	w.Header().Set("Content-Type", "text/event-stream")
	if _, err := w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"PRIVATE_OUTPUT\"}\n\n")); err == nil {
		t.Fatal("CAS failure ignored")
	}
	_ = w.deliver()
	if out.Code != 409 || strings.Contains(out.Body.String(), "PRIVATE_OUTPUT") {
		t.Fatal("unbound output leaked")
	}
}

func TestOfficialFailoverOnlyStructuredAccountFailures(t *testing.T) {
	for _, err := range []error{errors.New("401 arbitrary text"), &officialProtocolError{status: 403, code: "policy", source: "upstream"}, &officialProtocolError{status: 429, code: "unknown", source: "upstream"}, &officialProtocolError{status: 429, code: "usageLimitExceeded", source: "gateway"}, &officialProtocolError{status: 401, code: "authentication_unavailable", cause: context.Canceled}, context.DeadlineExceeded} {
		if officialFailoverEligible(err) {
			t.Fatalf("unsafe replay eligible: %v", err)
		}
	}
	if !officialFailoverEligible(&officialProtocolError{status: 401, code: "authentication_unavailable", source: "upstream"}) {
		t.Fatal("structured account auth failure not retried")
	}
}
