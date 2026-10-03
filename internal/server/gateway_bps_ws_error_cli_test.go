package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The official CLI connects only to loopback fixtures using fake credentials.
// No ChatGPT account or inference request is involved.
func TestBPSGatewayOfficialCLIStatuslessWSErrorTerminates(t *testing.T) {
	binary := os.Getenv("BPS_SYNTHETIC_CODEX_BINARY")
	if binary == "" {
		t.Skip("set BPS_SYNTHETIC_CODEX_BINARY for the offline official CLI fixture")
	}
	for _, tc := range []struct {
		name, event             string
		websocket, nativeStatus bool
	}{
		{"ws-flat", `{"type":"error","code":"rate_limit_exceeded","message":"Synthetic rate limit fixture"}`, true, false},
		{"ws-nested", `{"type":"error","error":{"code":"rate_limit_exceeded","message":"Synthetic rate limit fixture"}}`, true, false},
		{"http-flat", `{"type":"error","code":"rate_limit_exceeded","message":"Synthetic rate limit fixture"}`, false, false},
		{"http-nested", `{"type":"error","error":{"code":"rate_limit_exceeded","message":"Synthetic rate limit fixture"}}`, false, false},
		{"ws-upstream-http429", `{"type":"error","status":429,"error":{"code":"rate_limit_exceeded","message":"Synthetic rate limit fixture"}}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if tc.nativeStatus {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = fmt.Fprint(w, `{"error":{"code":"rate_limit_exceeded","message":"Synthetic rate limit fixture"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "data: "+tc.event+"\n\n")
			}))
			defer upstream.Close()
			var evidence []map[string]any
			workRoot, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			h := newBPSLiveHarness(t, binary, workRoot, bpsLiveAccount{AccessToken: "synthetic-upstream-token", AccountID: "synthetic-account"}, nil, &evidence)
			h.recorder.p.BaseURL = upstream.URL
			h.recorder.requireWS = tc.websocket
			configPath := filepath.Join(h.home, "config.toml")
			config, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if tc.websocket {
				config = bytes.Replace(config, []byte("supports_websockets=false"), []byte("supports_websockets=true"), 1)
			}
			if err := os.WriteFile(configPath, config, 0600); err != nil {
				t.Fatal(err)
			}
			h.recorder.begin(tc.name)
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "exec", "--json", "--skip-git-repo-check", "--ignore-rules", "--color", "never", "--sandbox", "workspace-write", "-")
			cmd.Dir, cmd.Env = h.work, h.env
			cmd.Stdin = strings.NewReader("Reply OK without tools.")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			started := time.Now()
			err = cmd.Run()
			elapsed := time.Since(started)
			failed := bytes.Contains(stdout.Bytes(), []byte(`"type":"turn.failed"`))
			fallback := bytes.Contains(stdout.Bytes(), []byte("live_ws_fallback_forbidden"))
			// The official CLI formats a WS HTTP error as its status instead of
			// the provider's message; the writer contract test checks that the
			// original message remains on the wire. HTTP SSE retains that text.
			correctError := (bytes.Contains(stdout.Bytes(), []byte("Synthetic rate limit fixture")) || tc.websocket && bytes.Contains(stdout.Bytes(), []byte("429"))) && !bytes.Contains(stdout.Bytes(), []byte("stream closed before response.completed"))
			t.Logf("official_cli_error_fixture elapsed_ms=%d upstream_calls=%d timed_out=%t turn_failed=%t http_fallback=%t correct_upstream_error=%t stdout_bytes=%d stderr_bytes=%d", elapsed.Milliseconds(), calls.Load(), ctx.Err() != nil, failed, fallback, correctError, stdout.Len(), stderr.Len())
			if err == nil || ctx.Err() != nil || !failed || fallback || !correctError || calls.Load() != 1 || elapsed >= 7*time.Second {
				t.Fatal("official CLI did not terminate promptly after the upstream error frame")
			}
		})
	}
}
