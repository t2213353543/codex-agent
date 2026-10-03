package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGatewaySessionAliasIdentity(t *testing.T) {
	aliases := []string{"X-Session-Affinity", "X-Session-Id", "X-Opencode-Session", "X-Conversation-Id"}
	for _, alias := range aliases {
		t.Run(alias, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/responses", nil)
			r.Header.Set(alias, " \talias-session\t ")
			body := []byte(`{"prompt_cache_key":"cache-key"}`)
			if got := gatewaySessionKey(r, body); got != "alias-session" {
				t.Fatalf("alias identity=%q", got)
			}
			if got := gatewaySessionKey(r, []byte(`{"input":"no cache key"}`)); got != "alias-session" {
				t.Fatalf("alias without cache key identity=%q", got)
			}
			for _, canonical := range []string{"session-id", "session_id", "thread-id", "conversation_id"} {
				r.Header.Set(canonical, "canonical-session")
				if got := gatewaySessionKey(r, body); got != "canonical-session" {
					t.Fatalf("%s displaced canonical %s: %q", alias, canonical, got)
				}
				r.Header.Del(canonical)
			}
			r.Header.Set(alias, "\u00a0alias-session\u00a0")
			if got := gatewaySessionKey(r, body); got != "\u00a0alias-session\u00a0" {
				t.Fatalf("Unicode identity changed: %q", got)
			}
		})
	}
	for _, test := range []struct {
		name    string
		headers http.Header
		want    string
	}{
		{"canonical_scalar_wins", http.Header{"X-Session-Id": {"first", "second"}, "x-session-id": {"shadow"}}, "first"},
		{"deterministic_noncanonical", http.Header{"X-SESSION-ID": {"upper"}, "x-session-id": {"lower"}}, "upper"},
		{"blank_first_scalar_uses_next_alias", http.Header{"X-Session-Affinity": {" \t", "shadow"}, "X-Session-Id": {"next"}}, "next"},
		{"alias_priority", http.Header{"X-Session-Affinity": {"first"}, "X-Session-Id": {"second"}, "X-Opencode-Session": {"third"}, "X-Conversation-Id": {"fourth"}}, "first"},
		{"hop_only_alias_uses_next", http.Header{"X-Session-Affinity": {"hop-only"}, "X-Session-Id": {"stable"}, "Connection": {"keep-alive, x-session-affinity"}}, "stable"},
		{"hop_only_alias_uses_cache", http.Header{"X-Session-Id": {"hop-only"}, "connection": {"X-SESSION-ID"}}, "cache-key"},
		{"hop_only_canonical_uses_alias", http.Header{"Session-Id": {"hop-only"}, "X-Session-Id": {"stable"}, "Connection": {"Session-ID"}}, "stable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/responses", nil)
			r.Header = test.headers
			if got := gatewaySessionKey(r, []byte(`{"prompt_cache_key":"cache-key"}`)); got != test.want {
				t.Fatalf("identity=%q want=%q", got, test.want)
			}
		})
	}
}

type gatewayAliasWire struct {
	account  string
	headers  http.Header
	metadata json.RawMessage
	cacheKey string
}

func gatewayAliasFixture(t *testing.T) (*Proxy, *httptest.Server, <-chan gatewayAliasWire) {
	t.Helper()
	wires := make(chan gatewayAliasWire, 8)
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Metadata json.RawMessage `json:"client_metadata"`
			CacheKey string          `json:"prompt_cache_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		wires <- gatewayAliasWire{r.Header.Get("Authorization"), r.Header.Clone(), body.Metadata, body.CacheKey}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"alias-fixture\",\"status\":\"completed\",\"output\":[]}}\n\n")
	})
	gateway := httptest.NewServer(p)
	t.Cleanup(gateway.Close)
	return p, gateway, wires
}

func gatewayAliasCall(t *testing.T, gateway *httptest.Server, token string, headers http.Header, turn int) (int, string) {
	t.Helper()
	body := fmt.Sprintf(`{"model":"test","stream":false,"prompt_cache_key":"caller-key-%d","input":[{"role":"user","content":"turn %d"}],"client_metadata":{"session_id":"body-session","thread_id":"body-thread"}}`, turn, turn)
	r, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header = headers.Clone()
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	response, err := gateway.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	result, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(result)
}

func gatewayAliasTakeWire(t *testing.T, wires <-chan gatewayAliasWire) gatewayAliasWire {
	t.Helper()
	select {
	case wire := <-wires:
		return wire
	case <-time.After(3 * time.Second):
		t.Fatal("missing upstream request")
		return gatewayAliasWire{}
	}
}

func gatewayAliasAddPreferredAccount(t *testing.T, p *Proxy, groups []string) {
	t.Helper()
	if err := p.Store.ImportWith("other", []byte(`{"access_token":"other-token"}`), AccountDefaults{Concurrency: 10, Groups: groups}); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("other", func(a *Account) error { a.Priority = 100; return nil }); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
}

func TestGatewaySessionAliasHTTPKeepsAccountAndWireIdentity(t *testing.T) {
	for _, alias := range []string{"X-Session-Affinity", "X-Session-Id", "X-Opencode-Session", "X-Conversation-Id"} {
		t.Run(alias, func(t *testing.T) {
			p, gateway, wires := gatewayAliasFixture(t)
			for turn := 1; turn <= 4; turn++ {
				headers := http.Header{alias: {"stable-session"}}
				if turn == 3 {
					// The same identity survives switching header conventions;
					// the canonical value wins over a conflicting alias.
					headers.Set("Session-Id", "stable-session")
					headers.Set(alias, "conflicting-alias")
				}
				if turn == 4 {
					headers.Set(alias, "new-session")
				}
				status, result := gatewayAliasCall(t, gateway, "test-client-key", headers, turn)
				if status != http.StatusOK {
					t.Fatalf("turn %d status=%d body=%s", turn, status, result)
				}
				wire := gatewayAliasTakeWire(t, wires)
				wantAccount := "Bearer test-upstream-token"
				if turn == 4 {
					wantAccount = "Bearer other-token"
				}
				if wire.account != wantAccount {
					t.Fatalf("turn %d account=%q want=%q", turn, wire.account, wantAccount)
				}
				for _, localOnly := range []string{"X-Session-Affinity", "X-Session-Id", "X-Opencode-Session", "X-Conversation-Id"} {
					if wire.headers.Get(localOnly) != "" {
						t.Fatalf("routing-only %s leaked upstream", localOnly)
					}
				}
				wantSession := ""
				if turn == 3 {
					wantSession = "stable-session"
				}
				if wire.headers.Get("Session-Id") != wantSession || wire.headers.Get("Session_id") != "" || wire.headers.Get("Thread-Id") != "" || wire.headers.Get("Conversation_id") != "" {
					t.Fatalf("routing alias rewrote upstream conversation identity: %v", wire.headers)
				}
				accountName := "test"
				if turn == 4 {
					accountName = "other"
				}
				account, err := p.Store.Get(accountName)
				if err != nil {
					t.Fatal(err)
				}
				wantMetadata, _ := json.Marshal(map[string]string{
					"session_id": gatewayScopeCacheIdentityValue(account, "admin", "session", "body-session"),
					"thread_id":  gatewayScopeCacheIdentityValue(account, "admin", "thread", "body-thread"),
				})
				assertJSONEqual(t, wire.metadata, wantMetadata)
				if wire.cacheKey != gatewayScopeCacheIdentityValue(account, "admin", "prompt-cache", fmt.Sprintf("caller-key-%d", turn)) {
					t.Fatalf("caller cache key lost the admitted owner namespace: %q", wire.cacheKey)
				}
				if turn == 1 {
					gatewayAliasAddPreferredAccount(t, p, nil)
				}
			}
		})
	}
}

func TestGatewaySessionAliasHTTPRechecksAccess(t *testing.T) {
	for _, revoke := range []string{"account-disabled", "group-revoked", "key-disabled"} {
		t.Run(revoke, func(t *testing.T) {
			p, gateway, wires := gatewayAliasFixture(t)
			if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Groups = []string{"original"}; return nil }); err != nil {
				t.Fatal(err)
			}
			key, err := p.Keys.Create("alias-client", []string{"original"})
			if err != nil {
				t.Fatal(err)
			}
			headers := http.Header{"X-Session-Id": {"access-session"}}
			if status, result := gatewayAliasCall(t, gateway, key.Key, headers, 1); status != http.StatusOK {
				t.Fatalf("initial status=%d body=%s", status, result)
			}
			if wire := gatewayAliasTakeWire(t, wires); wire.account != "Bearer test-upstream-token" {
				t.Fatalf("initial account=%q", wire.account)
			}
			gatewayAliasAddPreferredAccount(t, p, []string{"original", "different"})
			switch revoke {
			case "account-disabled":
				err = p.Store.UpdateAccount("test", func(a *Account) error { a.Enabled = false; return nil })
			case "group-revoked":
				_, err = p.Keys.Update(key.ID, func(k *APIKey) error { k.Groups = []string{"different"}; return nil })
			case "key-disabled":
				_, err = p.Keys.Update(key.ID, func(k *APIKey) error { k.Enabled = false; return nil })
			}
			if err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
			status, result := gatewayAliasCall(t, gateway, key.Key, headers, 2)
			if status < 400 || (revoke != "key-disabled" && !strings.Contains(result, "no longer enabled or allowed")) {
				t.Fatalf("revoked session did not fail closed: status=%d body=%s", status, result)
			}
			select {
			case wire := <-wires:
				t.Fatalf("revoked session reached upstream account=%q", wire.account)
			default:
			}
		})
	}
}

func TestGatewaySessionAliasHTTPUnicodeAndHopBoundaries(t *testing.T) {
	for _, scenario := range []string{"unicode", "connection"} {
		t.Run(scenario, func(t *testing.T) {
			p, gateway, wires := gatewayAliasFixture(t)
			identity := "local-session"
			if scenario == "unicode" {
				identity = "\u00a0local-session\u00a0"
			}
			for turn := 1; turn <= 3; turn++ {
				headers := http.Header{"X-Session-Id": {identity}}
				wantAccount := "Bearer test-upstream-token"
				if turn == 3 {
					wantAccount = "Bearer other-token"
					if scenario == "unicode" {
						headers.Set("X-Session-Id", "local-session")
					} else {
						headers.Set("Connection", "keep-alive, X-SESSION-ID")
					}
				}
				status, result := gatewayAliasCall(t, gateway, "test-client-key", headers, turn)
				if status != http.StatusOK {
					t.Fatalf("turn %d status=%d body=%s", turn, status, result)
				}
				if wire := gatewayAliasTakeWire(t, wires); wire.account != wantAccount {
					t.Fatalf("turn %d account=%q want=%q", turn, wire.account, wantAccount)
				}
				if turn == 1 {
					gatewayAliasAddPreferredAccount(t, p, nil)
				}
			}
		})
	}
}
