package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

type researchSessionWire struct {
	owner, session string
	body           map[string]json.RawMessage
}

func researchSessionFixture(t *testing.T, native bool) (*Proxy, string, <-chan researchSessionWire) {
	t.Helper()
	observed := make(chan researchSessionWire, 8)
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "invalid synthetic request", 400)
			return
		}
		owner := "original"
		if r.Header.Get("Authorization") == "Bearer synthetic-session-alternate" {
			owner = "alternate"
		}
		observed <- researchSessionWire{owner: owner, session: r.Header.Get("Session-Id"), body: body}
		// A synthetic encrypted-history ownership boundary, not real crypto or
		// a model: only the issuing account can accept this continuation marker.
		if len(body["previous_response_id"]) > 0 && owner != "original" {
			http.Error(w, `{"error":{"code":"synthetic_history_owner_mismatch"}}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"session-research-response\",\"status\":\"completed\",\"output\":[]}}\n\n")
	})
	if err := p.Store.ImportWith("alternate", []byte(`{"access_token":"synthetic-session-alternate","chatgpt_account_id":"synthetic-session-workspace-b","expires_at":4102444800}`), AccountDefaults{Concurrency: 2, Priority: -10}); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; return nil }); err != nil {
		t.Fatal(err)
	}
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.BuiltinPromptEnabled = false
	settings.StickySessionsEnabled = true
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	if native {
		realGatewayNativePool(t, p)
	}
	gateway := httptest.NewServer(p)
	t.Cleanup(gateway.Close)
	return p, gateway.URL, observed
}

func researchSessionPreferAlternate(t *testing.T, p *Proxy) {
	t.Helper()
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = -10; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("alternate", func(a *Account) error { a.Priority = 100; return nil }); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
}

func researchSessionSend(t *testing.T, target, body, session string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target+"/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer test-client-key")
	if session != "" {
		request.Header.Set("Session-Id", session)
	}
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	return response.StatusCode
}

func TestResearchSessionCanonicalCacheKeySurvivesUnknownCaseFields(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) {
			if native && (runtime.GOOS != "linux" || os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN") == "") {
				t.Skip("Linux and explicit real Rust helper required")
			}
			for _, extension := range []string{
				`,"Prompt_Cache_Key":"foreign-thread"`,
				`,"PROMPT_CACHE_KEY":"foreign-thread"`,
				`,"Prompt_Cache_Key":"first-shadow","PROMPT_CACHE_KEY":"last-shadow"`,
				`,"Prompt_Cache_Key":{}`, // Unknown extension type must not erase a valid key.
			} {
				t.Run(extension, func(t *testing.T) {
					p, target, observed := researchSessionFixture(t, native)
					const canonical = "explicit-canonical-thread"
					first := `{"model":"fixture","stream":true,"prompt_cache_key":"` + canonical + `","input":[]}`
					if status := researchSessionSend(t, target, first, ""); status != 200 {
						t.Fatal("first canonical request failed")
					}
					if got := <-observed; got.owner != "original" {
						t.Fatal("priority fixture did not establish original account")
					}
					researchSessionPreferAlternate(t, p)
					second := `{"model":"fixture","stream":true,"prompt_cache_key":"` + canonical + `","previous_response_id":"session-research-response","input":[{"type":"reasoning","encrypted_content":"synthetic-original-owner-history"}]` + extension + `}`
					status := researchSessionSend(t, target, second, "")
					got := <-observed
					if string(got.body["prompt_cache_key"]) != `"`+gatewayFixtureScopedValue("prompt-cache", canonical)+`"` || !strings.Contains(string(got.body["input"]), "synthetic-original-owner-history") {
						t.Fatal("fixture changed canonical upstream cache key or opaque history")
					}
					if status != 200 || got.owner != "original" {
						t.Errorf("unknown case field rerouted the canonical continuation: status=%d upstream_owner=%s", status, got.owner)
					}
					original, err := p.Store.Get("test")
					if err != nil {
						t.Fatal(err)
					}
					gatewaySessionAuthorizationAssertBinding(t, p, "admin", canonical, original)
				})
			}
		})
	}
}

func TestResearchSessionHeaderWhitespaceMatchesActualWireIdentity(t *testing.T) {
	for _, scenario := range []struct {
		name, second, wantWire, wantOwner string
	}{
		{"http_optional_whitespace", " \tcanonical-header\t ", "canonical-header", "original"},
		{"nonbreaking_space_is_header_data", "\u00a0canonical-header\u00a0", "\u00a0canonical-header\u00a0", "alternate"},
		{"ideographic_space_is_header_data", "\u3000canonical-header\u3000", "\u3000canonical-header\u3000", "alternate"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			p, target, observed := researchSessionFixture(t, false)
			const body = `{"model":"fixture","stream":true,"input":[]}`
			if status := researchSessionSend(t, target, body, "canonical-header"); status != 200 {
				t.Fatal("first header request failed")
			}
			if first := <-observed; first.owner != "original" || first.session != "canonical-header" {
				t.Fatal("first explicit header did not reach original account unchanged")
			}
			researchSessionPreferAlternate(t, p)
			if status := researchSessionSend(t, target, body, scenario.second); status != 200 {
				t.Fatal("second header request failed")
			}
			got := <-observed
			if got.session != scenario.wantWire {
				t.Fatalf("wire premise differs: upstream_session=%q want=%q", got.session, scenario.wantWire)
			}
			if got.owner != scenario.wantOwner {
				t.Errorf("local session normalization merged distinct wire identities: wire=%q upstream_owner=%s want=%s", got.session, got.owner, scenario.wantOwner)
			}
		})
	}
}

func TestResearchSessionBodyIdentityUsesOnlyCanonicalMember(t *testing.T) {
	for _, scenario := range []struct{ raw, want string }{
		{`{"prompt_cache_key":"canonical","Prompt_Cache_Key":"extension"}`, "canonical"},
		{`{"Prompt_Cache_Key":"extension","prompt_cache_key":"canonical"}`, "canonical"},
		{`{"Prompt_Cache_Key":"extension"}`, ""},
		{`{"PROMPT_CACHE_KEY":"extension"}`, ""},
		{`{"prompt_cache_key":null,"Prompt_Cache_Key":"extension"}`, ""},
		{`{"prompt_cache_key":13,"Prompt_Cache_Key":"extension"}`, ""},
		{`{"prompt_cache_key":"first","prompt_cache_key":"last"}`, "last"},
		{`{"prompt_cache_key":"first","prompt_cache_key":{},"Prompt_Cache_Key":"extension"}`, ""},
		{`{"prompt_cache_key":"first"} trailing`, ""},
	} {
		t.Run(scenario.raw, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/responses", nil)
			if got := gatewaySessionKey(r, []byte(scenario.raw)); got != scenario.want {
				t.Fatalf("noncanonical/invalid cache-key member affected routing: got=%q want=%q", got, scenario.want)
			}
			r.Header.Set("Session-Id", "explicit-header")
			if got := gatewaySessionKey(r, []byte(scenario.raw)); got != "explicit-header" {
				t.Fatal("body fallback displaced the established explicit-header precedence")
			}
		})
	}
}

func TestResearchSessionGeneratedPrefixAndUnknownExtensionDoNotCreateBinding(t *testing.T) {
	p, target, observed := researchSessionFixture(t, false)
	const body = `{"model":"fixture","stream":true,"Prompt_Cache_Key":"unknown-extension","instructions":"shared application prefix","input":[]}`
	if status := researchSessionSend(t, target, body, ""); status != 200 {
		t.Fatal("unidentified first request failed")
	}
	first := <-observed
	researchSessionPreferAlternate(t, p)
	if status := researchSessionSend(t, target, body, ""); status != 200 {
		t.Fatal("unidentified second request failed")
	}
	second := <-observed
	if first.owner != "original" || second.owner != "alternate" {
		t.Errorf("unknown extension or generated shared prefix incorrectly established affinity: first=%s second=%s", first.owner, second.owner)
	}
	if len(first.body["prompt_cache_key"]) != 0 || len(second.body["prompt_cache_key"]) != 0 {
		t.Fatal("ordinary Responses manufactured a prompt cache key for a shared prefix")
	}
	if string(first.body["Prompt_Cache_Key"]) != `"unknown-extension"` || string(second.body["Prompt_Cache_Key"]) != `"unknown-extension"` {
		t.Fatal("unknown extension bytes were rewritten instead of excluded from routing")
	}
	p.sessionMu.Lock()
	bindings := len(p.sessionRoutes)
	p.sessionMu.Unlock()
	if bindings != 0 {
		t.Fatalf("generated/shared prefix or unknown key created %d thread bindings", bindings)
	}
}
