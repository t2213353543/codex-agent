package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"codex-agent/internal/bps"
)

func TestBPSSharedCacheChangesOnlyFinalHintAndIsIdempotent(t *testing.T) {
	account := bpsDefaultCacheAccount("user", "workspace")
	settings := defaultSettings()
	settings.BPSPromptCacheScope = bpsPromptCacheScopeAPIKey
	original := []byte(" {\n" + `"model":"gpt-6-astra","prompt_cache_key":"client\u002dkey","input":[{"role":"user","content":"\u4e2d\n<>&"},{"type":"additional_tools","tools":[{"type":"function","name":"inspect","parameters":{"n":9007199254740993,"v":1.2500e+02}}]}],"reasoning_effort":"xhigh","metadata":{"task_id":"task-original","turn_id":"turn-original"},"session_id":"session-original","thread_id":"thread-original"` + " }\r\n")
	prepared := &bps.Request{Body: original, Model: "gpt-6-astra"}
	before := append([]byte(nil), original...)
	if err := applyBPSDefaultPromptCache(prepared, "tenant-one", account, settings); err != nil {
		t.Fatal(err)
	}
	key := bpsDefaultPromptCacheKey(prepared, "tenant-one", account, settings)
	encoded, _ := json.Marshal(key)
	want := bytes.Replace(before, []byte(`"client\u002dkey"`), encoded, 1)
	if !bytes.Equal(prepared.Body, want) || !bytes.Equal(original, before) || !bpsSharedPromptCacheApplied(prepared, "tenant-one", account, settings) {
		t.Fatal("shared scope changed bytes outside the hint, mutated caller history, or was not observed")
	}
	first := prepared.Body
	if err := applyBPSDefaultPromptCache(prepared, "tenant-one", account, settings); err != nil || !bytes.Equal(prepared.Body, first) || &prepared.Body[0] != &first[0] {
		t.Fatal("already shared key was not idempotent")
	}
	for _, body := range []string{
		`{"model":"gpt-6-astra","input":"other session","prompt_cache_key":"different-client-key"}`,
		`{"model":"gpt-6-astra","input":"no client key"}`,
		`{"model":"gpt-6-astra","prompt_cache_key":null,"input":"null key"}`,
	} {
		p := &bps.Request{Body: []byte(body), Model: "gpt-6-astra"}
		if err := applyBPSDefaultPromptCache(p, "tenant-one", account, settings); err != nil || gatewayPromptCacheKeyValue(p.Body) != key {
			t.Fatal("same tenant, owner and model did not reuse the hint")
		}
	}
	for _, test := range []struct{ key, model, user, workspace string }{
		{"tenant-two", "gpt-6-astra", "user", "workspace"},
		{"tenant-one", "gpt-5.6-sol", "user", "workspace"},
		{"tenant-one", "gpt-6-astra", "other-user", "workspace"},
		{"tenant-one", "gpt-6-astra", "user", "other-workspace"},
	} {
		p := &bps.Request{Body: append([]byte(nil), original...), Model: test.model}
		if err := applyBPSDefaultPromptCache(p, test.key, bpsDefaultCacheAccount(test.user, test.workspace), settings); err != nil || gatewayPromptCacheKeyValue(p.Body) == key {
			t.Fatal("shared key crossed an API key, owner, workspace or model boundary")
		}
	}
	account.AccessToken = "refreshed-synthetic-token"
	if bpsDefaultPromptCacheKey(prepared, "tenant-one", account, settings) != key {
		t.Fatal("credential refresh split a confirmed owner's cache scope")
	}
}

func TestBPSSharedCacheRespectsSwitchMissingIdentityAndCompaction(t *testing.T) {
	for _, test := range []struct {
		name, scope, key, model string
		enabled, compact        bool
		account                 Account
	}{
		{"default_client", "", "tenant", "gpt-6-astra", true, false, bpsDefaultCacheAccount("user", "workspace")},
		{"client", bpsPromptCacheScopeClient, "tenant", "gpt-6-astra", true, false, bpsDefaultCacheAccount("user", "workspace")},
		{"disabled", bpsPromptCacheScopeAPIKey, "tenant", "gpt-6-astra", false, false, bpsDefaultCacheAccount("user", "workspace")},
		{"compact", bpsPromptCacheScopeAPIKey, "tenant", "gpt-6-astra", true, true, bpsDefaultCacheAccount("user", "workspace")},
		{"missing_key", bpsPromptCacheScopeAPIKey, "", "gpt-6-astra", true, false, bpsDefaultCacheAccount("user", "workspace")},
		{"blank_key", bpsPromptCacheScopeAPIKey, " ", "gpt-6-astra", true, false, bpsDefaultCacheAccount("user", "workspace")},
		{"missing_model", bpsPromptCacheScopeAPIKey, "tenant", "", true, false, bpsDefaultCacheAccount("user", "workspace")},
		{"blank_model", bpsPromptCacheScopeAPIKey, "tenant", " ", true, false, bpsDefaultCacheAccount("user", "workspace")},
		{"missing_user", bpsPromptCacheScopeAPIKey, "tenant", "gpt-6-astra", true, false, bpsDefaultCacheAccount("", "workspace")},
		{"missing_workspace", bpsPromptCacheScopeAPIKey, "tenant", "gpt-6-astra", true, false, bpsDefaultCacheAccount("user", "")},
		{"missing_owner", bpsPromptCacheScopeAPIKey, "tenant", "gpt-6-astra", true, false, Account{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := []byte(`{ "model":"gpt-6-astra", "prompt_cache_key":"client\u002dkey", "input":"original" }`)
			p := &bps.Request{Body: original, Model: test.model, Compact: test.compact}
			settings := Settings{PromptCacheKeyEnabled: test.enabled, BPSPromptCacheScope: test.scope}
			if err := applyBPSDefaultPromptCache(p, test.key, test.account, settings); err != nil || !bytes.Equal(p.Body, original) || bpsSharedPromptCacheApplied(p, test.key, test.account, settings) {
				t.Fatal("ineligible scope changed the client key or reported sharing")
			}
		})
	}
	if err := applyBPSDefaultPromptCache(nil, "tenant", Account{}, defaultSettings()); err != nil {
		t.Fatal(err)
	}
}

func TestBPSSharedCacheGatewayPreservesConversationHistoryAndObservesSentHint(t *testing.T) {
	var sentKeys, tasks []string
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body struct {
			Key      string `json:"prompt_cache_key"`
			Metadata struct {
				Task string `json:"task_id"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(data, &body); err != nil {
			t.Error(err)
		}
		sentKeys, tasks = append(sentKeys, body.Key), append(tasks, body.Metadata.Task)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", fmt.Sprintf("shared-cache-%d", len(sentKeys)), "[]"))
	})
	p.PromptCacheObserver = promptCacheObserverForTest(t)
	settings, _ := p.Settings.Get()
	settings.PromptCacheKeyEnabled, settings.BPSPromptCacheScope = true, bpsPromptCacheScopeAPIKey
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	for i, session := range []string{"session-one", "session-two", "session-two"} {
		clientKey := "client-key-" + session
		previous := ""
		if i == 2 {
			previous = `,"previous_response_id":"shared-cache-2"`
		}
		body := fmt.Sprintf(`{"model":"gpt-6-astra","stream":false,"prompt_cache_key":%q,"input":[{"role":"user","content":"same prefix"}]%s}`, clientKey, previous)
		r := proxyRequest(http.MethodPost, "/v1/responses", body)
		r.Header.Set("Session-Id", session)
		w := httptest.NewRecorder()
		p.gatewayBPS(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("request failed: %d %s", w.Code, w.Body)
		}
		history, err := p.bpsLoadHistory(withGatewayUnboundedPayload(t.Context(), true), "admin", fmt.Sprintf("shared-cache-%d", i+1))
		if err != nil || gatewayPromptCacheKeyValue(history.Request) != clientKey || history.Session != session {
			t.Fatalf("shared upstream hint changed stored client key or session: %v", err)
		}
		record, ok := p.Metrics.LastFor("test")
		if !ok || record.PromptCache == nil || record.PromptCache.KeyMode != "shared_api_key" || !record.PromptCache.CacheKeyPresent {
			t.Fatalf("actual sent sharing not observed: %+v", record.PromptCache)
		}
		fields := gatewayPromptCacheObservationFields([]byte(fmt.Sprintf(`{"prompt_cache_key":%q}`, sentKeys[i])))
		if record.PromptCache.CacheKeyHash != gatewayPromptCacheHashString(p.PromptCacheObserver.fieldDigest(fields, "prompt_cache_key")) {
			t.Fatal("observation was not based on the final sent key")
		}
	}
	if len(sentKeys) != 3 || sentKeys[0] == "" || sentKeys[0] != sentKeys[1] || sentKeys[1] != sentKeys[2] {
		t.Fatalf("explicit session keys were not consolidated: %v", sentKeys)
	}
	if tasks[0] == tasks[1] || tasks[1] != tasks[2] {
		t.Fatal("sharing hint changed task isolation or continuation binding")
	}
}
