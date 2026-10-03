package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestOfficialNativeSessionReconnectIsolationAndShutdown(t *testing.T) {
	p, account := officialManagedFixture(t, false)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	first, err := p.Official.nativeSession(ctx, "key-a", "session-a", account, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Official.nativeSession(ctx, "key-a", "session-a", account, nil); err == nil {
		t.Fatal("simultaneous native websocket acquired same process")
	}
	first.mu.Unlock()
	reconnected, err := p.Official.nativeSession(ctx, "key-a", "session-a", account, nil)
	if err != nil || reconnected.rpc != first.rpc {
		t.Fatal("native reconnect lost process")
	}
	reconnected.mu.Unlock()
	other, err := p.Official.nativeSession(ctx, "key-b", "session-a", account, nil)
	if err != nil {
		t.Fatal(err)
	}
	if other.rpc == first.rpc {
		t.Fatal("different key shared native process")
	}
	other.mu.Unlock()
	rootA, rootB := first.rpc.root, other.rpc.root
	if err := p.Official.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{rootA, rootB} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("runtime shutdown retained RPC home")
		}
	}
}

func TestOfficialNativeReplayRestoresFailedWriteBeforeNewerEvents(t *testing.T) {
	rpc := &officialRPC{wake: make(chan struct{}, 1), done: make(chan struct{}), cancel: func() {}}
	first, second := json.RawMessage(`{"id":"first","result":{}}`), json.RawMessage(`{"id":"second","result":{}}`)
	rpc.queue = []json.RawMessage{first, second}
	rpc.queued = len(first) + len(second)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	raw, err := rpc.receive(ctx)
	if err != nil || string(raw) != string(first) {
		t.Fatal("first event unavailable")
	}
	rpc.requeue(raw)
	for _, want := range []json.RawMessage{first, second} {
		raw, err := rpc.receive(ctx)
		if err != nil || string(raw) != string(want) {
			t.Fatal("native reconnect reordered or lost RPC response")
		}
	}
	if rpc.queued != 0 {
		t.Fatal("native replay byte accounting drifted")
	}
}

func TestOfficialNativeWebSocketDisabledBeforeProcessSpawn(t *testing.T) {
	p, _ := webTestSetup(t)
	p.RuntimeModes = true
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.InferenceMode = inferenceAppServer
	settings.WebsocketEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	p.Official = officialTaskFixtureRuntime(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/codex/app-server", nil)
	r.Header.Set("Authorization", "Bearer "+webTestKey)
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Connection", "Upgrade")
	p.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("disabled native WS got %d: %s", w.Code, w.Body.String())
	}
	p.Official.mu.Lock()
	defer p.Official.mu.Unlock()
	if len(p.Official.processes) != 0 || len(p.Official.native) != 0 {
		t.Fatal("disabled native websocket spawned official runtime")
	}
}

func TestOfficialNativeReconnectRefreshesCredentialsWithoutLosingProcess(t *testing.T) {
	p, account := officialManagedFixture(t, false)
	account.Identity = &managedAccountIdentity{UserID: "same-user"}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	first, err := p.Official.nativeSession(ctx, "key", "session", account, nil)
	if err != nil {
		t.Fatal(err)
	}
	first.mu.Unlock()
	account.AccessToken = "SENSITIVE_FIXTURE_TOKEN_REFRESHED"
	account.CredentialVersion++
	for i := 0; i < 2; i++ {
		next, err := p.Official.nativeSession(ctx, "key", "session", account, nil)
		if err != nil {
			t.Fatal(err)
		}
		if next.rpc != first.rpc {
			t.Fatal("credential refresh discarded native session")
		}
		raw, err := next.rpc.call(ctx, "fixture/login_count", map[string]any{})
		next.mu.Unlock()
		var counts struct {
			Count int `json:"count"`
		}
		if err != nil || json.Unmarshal(raw, &counts) != nil || counts.Count != 2 {
			t.Fatalf("unexpected credential login count: %s %v", raw, err)
		}
	}
	account.AccessToken = "SENSITIVE_FIXTURE_TOKEN_RESTART"
	account.CredentialVersion++
	replacement, err := p.Official.nativeSession(ctx, "key", "session", account, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.mu.Unlock()
	if replacement.rpc == first.rpc || first.rpc.alive() {
		t.Fatal("rejected credential update retained old official process")
	}
	if _, err := os.Stat(first.rpc.root); !os.IsNotExist(err) {
		t.Fatal("rejected credential update retained old RPC home")
	}
}
