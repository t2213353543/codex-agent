package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBPSRuntimeLifecycleAuthenticatedDrainKeepsHandlers(t *testing.T) {
	token := strings.Repeat("private-control-", 4)
	t.Setenv("CODEX_LIFECYCLE_TOKEN", token)
	workerCtx, stop := context.WithCancel(t.Context())
	defer stop()
	done := make(chan struct{})
	p := &Proxy{lifecycle: &runtimeLifecycle{stopWorkers: stop, workersDone: done}}
	p.lifecycle.active.Store(2)
	call := func(method, path, peer, credential string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = peer
		r.Header.Set("X-Codex-Lifecycle-Token", credential)
		w := httptest.NewRecorder()
		if !runtimeLifecycleHandler(p, w, r) {
			t.Fatal("control not handled")
		}
		return w
	}
	for _, tc := range []struct{ peer, key string }{{"203.0.113.2:1234", token}, {"127.0.0.1:1234", "bad"}} {
		w := call("GET", "/_internal/lifecycle", tc.peer, tc.key)
		if w.Code != 404 {
			t.Fatal("untrusted control accepted")
		}
	}
	w := call("POST", "/_internal/drain", "127.0.0.1:1234", token)
	if w.Code != 200 || workerCtx.Err() == nil || p.lifecycle.active.Load() != 2 {
		t.Fatal("drain interrupted handlers or failed to stop workers")
	}
	close(done)
	p.lifecycle.active.Store(0)
	w = call("GET", "/_internal/lifecycle", "127.0.0.1:1234", token)
	var result struct {
		PID      int
		Draining bool
		Active   int    `json:"active_handlers"`
		Pending  int    `json:"pending_persistence"`
		Mode     string `json:"admission_mode"`
	}
	if json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.Draining || result.Active != 0 || result.Pending != 0 || result.PID <= 0 || result.Mode != "process_local" {
		t.Fatalf("incorrect drain state %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), token) {
		t.Fatal("private lifecycle token exposed")
	}
	if call(http.MethodDelete, "/_internal/drain", "127.0.0.1:1234", token).Code != 405 {
		t.Fatal("unexpected mutating control")
	}
}
