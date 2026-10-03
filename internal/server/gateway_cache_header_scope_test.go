package server

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestGatewaySub2CacheHeaderScopeHTTPAndWebSocket(t *testing.T) {
	account := Account{AccountID: "workspace", Identity: &managedAccountIdentity{UserID: "user"}}
	const embedded = `{ "session_id":"root", "thread_id":"child", "session-id":"root", "thread-id":"child", "turn_id":"turn", "device_id":"device", "n":9007199254740993, "escaped":"\u0061" }`
	for _, websocket := range []bool{false, true} {
		t.Run(map[bool]string{false: "HTTP", true: "WebSocket"}[websocket], func(t *testing.T) {
			headers := make(http.Header)
			for name, value := range map[string]string{"Session-Id": "root", "Thread-Id": "child", "session_id": "older-session", "conversation_id": "conversation", "X-Codex-Turn-Metadata": embedded, "X-Codex-Turn-State": "opaque-state", "X-Codex-Installation-Id": "installation", "X-Codex-Window-Id": "window", "X-Client-Request-Id": "request"} {
				headers.Set(name, value)
			}
			applyGatewayCacheHeaders(headers, "https://chatgpt.com/backend-api/codex", "/responses", "tenant", account, "cache", defaultSettings(), websocket)
			session := gatewayScopeCacheIdentityValue(account, "tenant", "session", "root")
			thread := gatewayScopeCacheIdentityValue(account, "tenant", "thread", "child")
			wantEmbedded := strings.ReplaceAll(strings.ReplaceAll(embedded, `"root"`, `"`+session+`"`), `"child"`, `"`+thread+`"`)
			if headers.Get("X-Codex-Turn-Metadata") != wantEmbedded || headers.Get("Session-Id") != session || headers.Get("Thread-Id") != thread {
				t.Fatal("header session/thread scope differs between scalar and embedded metadata")
			}
			for name, want := range map[string]string{"X-Codex-Turn-State": "opaque-state", "X-Codex-Installation-Id": "installation", "X-Codex-Window-Id": "window", "X-Client-Request-Id": "request"} {
				if headers.Get(name) != want {
					t.Fatalf("cache changed unrelated identity %s", name)
				}
			}
			wantSession, wantConversation := "cache", "cache"
			if websocket {
				wantSession, wantConversation = "root", "conversation"
			}
			if headers.Get("session_id") != gatewayUpstreamCacheSession("tenant", account, wantSession) || headers.Get("conversation_id") != gatewayUpstreamCacheSession("tenant", account, wantConversation) {
				t.Fatal("underscore session/conversation projection double-scoped or ignored raw source")
			}
		})
	}
}

func TestGatewaySub2CacheHeaderScopeGatesAndAbsentKeys(t *testing.T) {
	account := Account{AccountID: "workspace", Identity: &managedAccountIdentity{UserID: "user"}}
	for _, test := range []struct {
		name, base, endpoint, keyID string
		enabled                     bool
	}{
		{"disabled", "https://chatgpt.com/backend-api/codex", "/responses", "tenant", false},
		{"third_party", "https://example.test/backend-api/codex", "/responses", "tenant", true},
		{"lookalike", "https://chatgpt.com.example.test/backend-api/codex", "/responses", "tenant", true},
		{"other_endpoint", "https://chatgpt.com/backend-api/codex", "/models", "tenant", true},
		{"no_tenant", "https://chatgpt.com/backend-api/codex", "/responses", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			headers := http.Header{"Session-Id": {"raw"}, "X-Codex-Turn-Metadata": {`{"session_id":"raw"}`}}
			before := headers.Clone()
			settings := defaultSettings()
			settings.PromptCacheKeyEnabled = test.enabled
			applyGatewayCacheHeaders(headers, test.base, test.endpoint, test.keyID, account, "cache", settings, false)
			if !reflect.DeepEqual(headers, before) {
				t.Fatal("scope gate changed headers")
			}
		})
	}
	for _, raw := range []string{`not-json`, `{"session_id":42}`, `["raw"]`, `{"turn_id":"raw"}`} {
		headers := http.Header{"X-Codex-Turn-Metadata": {raw}}
		applyGatewayCacheHeaders(headers, "https://chatgpt.com/backend-api/codex", "/responses", "tenant", account, "", defaultSettings(), false)
		if headers.Get("X-Codex-Turn-Metadata") != raw || headers.Get("session_id") != "" || headers.Get("conversation_id") != "" {
			t.Fatal("invalid metadata or absent key generated identity")
		}
	}
}

func TestGatewaySub2CacheHeadersDoNotRestoreConnectionNominatedIdentity(t *testing.T) {
	account := Account{AccountID: "workspace", Identity: &managedAccountIdentity{UserID: "user"}}
	inbound := make(http.Header)
	inbound.Set("Connection", "Session-Id, X-Codex-Turn-Metadata")
	inbound.Set("Session-Id", "private-session")
	inbound.Set("X-Codex-Turn-Metadata", `{"session_id":"private-session"}`)
	headers := make(http.Header)
	if err := copyCodexRequestHeaders(headers, inbound, defaultSettings()); err != nil {
		t.Fatal(err)
	}
	applyGatewayCacheHeaders(headers, "https://chatgpt.com/backend-api/codex", "/responses", "tenant", account, "", defaultSettings(), true)
	if headers.Get("Session-Id") != "" || headers.Get("session_id") != "" || headers.Get("X-Codex-Turn-Metadata") != "" {
		t.Fatal("scoping resurrected Connection-nominated identity")
	}
}
