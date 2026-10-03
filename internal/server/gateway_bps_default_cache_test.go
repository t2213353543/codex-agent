package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-agent/internal/bps"
)

func bpsDefaultCacheAccount(user, workspace string) Account {
	return Account{ID: "local-row", Name: "fixture", AccountID: workspace, AccessToken: "not-a-real-token", Identity: &managedAccountIdentity{UserID: user, WorkspaceID: workspace}}
}

func TestBPSDefaultCacheIdentityIndependentOfSessionAndInput(t *testing.T) {
	a := bpsDefaultCacheAccount("user-one", "workspace")
	settings := Settings{PromptCacheKeyEnabled: true}
	makeBody := func(model, input, task string) *bps.Request {
		body, _ := json.Marshal(map[string]any{"model": model, "tools": []any{map[string]any{"type": "function", "name": "inspect", "parameters": map[string]any{"type": "object"}}}, "input": input})
		prepared, err := bps.Prepare(body, bps.Options{TaskID: task, TurnID: "turn-" + task})
		if err != nil {
			t.Fatal(err)
		}
		return prepared
	}
	first := makeBody("gpt-6-astra", "conversation one", "session-one")
	before := append([]byte(nil), first.Body...)
	if err := applyBPSDefaultPromptCache(first, "tenant-one", a, settings); err != nil {
		t.Fatal(err)
	}
	key := gatewayPromptCacheKeyValue(first.Body)
	if key == "" || !bytes.HasPrefix(first.Body, before[:len(before)-1]) {
		t.Fatal("key missing or existing payload bytes changed")
	}
	second := makeBody("gpt-6-astra", "different conversation two", "session-two")
	if err := applyBPSDefaultPromptCache(second, "tenant-one", a, settings); err != nil || gatewayPromptCacheKeyValue(second.Body) != key {
		t.Fatal("session/task/content split default cache identity")
	}
	for _, test := range []struct{ tenant, model, user, workspace string }{
		{"tenant-two", "gpt-6-astra", "user-one", "workspace"},
		{"tenant-one", "gpt-5.6-sol", "user-one", "workspace"},
		{"tenant-one", "gpt-6-astra", "user-two", "workspace"},
		{"tenant-one", "gpt-6-astra", "user-one", "other-workspace"},
	} {
		prepared := makeBody(test.model, "conversation", "session")
		if err := applyBPSDefaultPromptCache(prepared, test.tenant, bpsDefaultCacheAccount(test.user, test.workspace), settings); err != nil || gatewayPromptCacheKeyValue(prepared.Body) == key {
			t.Fatal("tenant/account/model namespaces collided")
		}
	}
	rotated := a
	rotated.AccessToken = "rotated-synthetic-token"
	third := makeBody("gpt-6-astra", "conversation", "session-three")
	if err := applyBPSDefaultPromptCache(third, "tenant-one", rotated, settings); err != nil || gatewayPromptCacheKeyValue(third.Body) != key {
		t.Fatal("same owner credential refresh changed cache namespace")
	}
}

func TestBPSDefaultCachePreservesExplicitKeysExactPayloadAndSwitch(t *testing.T) {
	a := bpsDefaultCacheAccount("user", "workspace")
	for _, value := range []string{`"already-account-scoped-client-key"`, `null`, `""`, `"   "`} {
		body := []byte(`{ "model":"m", "input":[{"type":"additional_tools","id":"at_client_owned","role":"developer","tools":[{"type":"function","name":"inspect","parameters":{"z":9007199254740993,"a":1.2500e+02}}]},{"role":"user","content":"<>&exact"}], "prompt_cache_key":` + value + ` }`)
		original := append([]byte(nil), body...)
		p := &bps.Request{Body: body, Model: "m"}
		if err := applyBPSDefaultPromptCache(p, "key", a, Settings{PromptCacheKeyEnabled: false}); err != nil || !bytes.Equal(p.Body, original) {
			t.Fatal("disabled cache setting mutated request")
		}
		if err := applyBPSDefaultPromptCache(p, "key", a, Settings{PromptCacheKeyEnabled: true}); err != nil {
			t.Fatal(err)
		}
		if value == `"already-account-scoped-client-key"` {
			if !bytes.Equal(p.Body, original) {
				t.Fatal("explicit client key was changed")
			}
		} else if gatewayPromptCacheKeyValue(p.Body) == "" {
			t.Fatal("absent effective key was not filled")
		}
		before := gatewayValidatedJSONFieldSpans(original, "input", "")[0]
		after := gatewayValidatedJSONFieldSpans(p.Body, "input", "")[0]
		if !bytes.Equal(original[before.start:before.end], p.Body[after.start:after.end]) {
			t.Fatal("original input/tool schema bytes changed")
		}
		if !bytes.Equal(body, original) {
			t.Fatal("caller-owned request buffer mutated")
		}
	}
}

func TestBPSDefaultCacheGatewayAcrossSessionsAndHistory(t *testing.T) {
	var keys, tasks []string
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body struct {
			Key      string `json:"prompt_cache_key"`
			Metadata struct {
				Task string `json:"task_id"`
			} `json:"metadata"`
		}
		_ = json.Unmarshal(data, &body)
		keys = append(keys, body.Key)
		tasks = append(tasks, body.Metadata.Task)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", fmt.Sprintf("default-cache-%d", len(keys)), "[]"))
	})
	settings, _ := p.Settings.Get()
	settings.PromptCacheKeyEnabled = true
	_ = p.Settings.Set(settings)
	const original = `{"model":"gpt-6-astra","stream":false,"tools":[{"type":"function","name":"inspect","parameters":{"type":"object"}}],"input":[{"role":"user","content":"same prefix"}]}`
	for i, body := range []string{original, original, `{"model":"gpt-6-astra","stream":false,"previous_response_id":"default-cache-2","input":[{"role":"user","content":"follow up"}]}`} {
		r := proxyRequest("POST", "/v1/responses", body)
		session := "session-one"
		if i > 0 {
			session = "session-two"
		}
		r.Header.Set("Session-Id", session)
		w := httptest.NewRecorder()
		p.gatewayBPS(w, r)
		if w.Code != 200 {
			t.Fatalf("request%d failed %d %s", i, w.Code, w.Body)
		}
		history, err := p.bpsLoadHistory(withGatewayUnboundedPayload(t.Context(), true), "admin", fmt.Sprintf("default-cache-%d", i+1))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(history.Request), "prompt_cache_key") {
			t.Fatal("generated upstream key polluted stored client history")
		}
		if strings.Contains(string(history.Request), "additional_tools") {
			t.Fatal("generated tool declaration polluted stored history")
		}
	}
	if len(keys) != 3 || keys[0] == "" || keys[1] != keys[0] || keys[2] != keys[0] {
		t.Fatalf("default key not reusable: %v", keys)
	}
	if tasks[0] == tasks[1] || tasks[1] != tasks[2] {
		t.Fatal("upstream cache identity changed conversation task binding")
	}
	settings, _ = p.Settings.Get()
	settings.PromptCacheKeyEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := proxyRequest("POST", "/v1/responses", original)
	r.Header.Set("Session-Id", "disabled-session")
	p.gatewayBPS(w, r)
	if w.Code != 200 || keys[3] != "" {
		t.Fatal("disabled setting still generated upstream default key")
	}
}
