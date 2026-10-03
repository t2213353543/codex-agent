package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func contentSessionForTest(body string) string {
	return gatewaySessionKey(httptest.NewRequest(http.MethodPost, "/responses", nil), []byte(body))
}

func TestGatewayContentSessionStableHistoryAndSourceFields(t *testing.T) {
	base := `{"model":"fixture","instructions":"stable","tools":[{"type":"function","name":"tool","parameters":{"maximum":9007199254740993,"type":"number"}}],"functions":[{"name":"legacy"}],"input":[{"role":"developer","content":"policy"},{"role":"user","content":[{"type":"input_text","text":"inspect project"},{"type":"input_image","image_url":"data:image/png;base64,AAA"}]}]}`
	seed := contentSessionForTest(base)
	if seed == "" {
		t.Fatal("a headerless anchored conversation has no persistent session")
	}
	for _, changed := range []string{
		strings.TrimSuffix(base, `]}`) + `,{"role":"assistant","content":"reply"},{"type":"function_call_output","call_id":"new","output":"result"},{"role":"user","content":"next"}]}`,
		strings.Replace(base, `"maximum":9007199254740993,"type":"number"`, `"type":"number","maximum":9007199254740993`, 1),
		strings.TrimSuffix(base, `}`) + `,"reasoning":{"effort":"high"},"client_metadata":{"turn_id":"new"}}`,
	} {
		if got := contentSessionForTest(changed); got != seed {
			t.Fatal("appended conversation history or equivalent encoding changed the session")
		}
	}
	for _, changed := range []string{
		strings.Replace(base, `"fixture"`, `"different"`, 1),
		strings.Replace(base, `"stable"`, `"different"`, 1),
		strings.Replace(base, `"policy"`, `"different"`, 1),
		strings.Replace(base, `"inspect project"`, `"different"`, 1),
		strings.Replace(base, `base64,AAA`, `base64,BBB`, 1),
		strings.Replace(base, `9007199254740993`, `9007199254740992`, 1),
		strings.Replace(base, `"legacy"`, `"different"`, 1),
		strings.TrimSuffix(base, `]}`) + `,{"role":"system","content":"later response policy"}]}`,
	} {
		if got := contentSessionForTest(changed); got == "" || got == seed {
			t.Fatal("changed model, system, tools/functions or first multimodal user did not split the session")
		}
	}
}

func TestGatewayContentSessionNeedsMeaningfulFirstUser(t *testing.T) {
	for _, body := range []string{
		`{}`, `{"model":"fixture"}`, `{"model":"fixture","instructions":"shared","tools":[{"type":"function","name":"shared"}],"input":[]}`,
		`{"model":"fixture","input":[{"role":"system","content":"shared"},{"role":"assistant","content":"reply"}]}`,
		`{"model":"fixture","input":[{"type":"reasoning","encrypted_content":"opaque"},{"type":"function_call_output","call_id":"a","output":"result"}]}`,
		`{"model":"fixture","input":[{"role":"user","content":"  "},{"role":"user","content":"later nonempty"}]}`,
		`{"model":"fixture","input":[{"role":"user","content":[{"type":"input_text","text":" "}]}]}`,
		`{"model":"fixture","input":[{"role":"user","content":{}}]}`,
		`{"model":"fixture","input":"anchor"} trailing`,
	} {
		if got := contentSessionForTest(body); got != "" {
			t.Fatalf("unanchored or invalid request created a hard shared session: %s", body)
		}
	}
	for _, body := range []string{
		`{"model":"fixture","input":"first prompt"}`,
		`{"model":"fixture","input":[{"type":"input_text","text":"first prompt"}]}`,
		`{"model":"fixture","input":[{"role":"user","content":[{"type":"input_image","image_url":"image-a"}]}]}`,
		`{"model":"fixture","input":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"audio-a","format":"wav"}}]}]}`,
		`{"model":"fixture","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-a"}]}]}`,
	} {
		if got := contentSessionForTest(body); got == "" {
			t.Fatalf("meaningful text/multimodal anchor lost its session: %s", body)
		}
	}
}

func TestGatewayContentSessionChatLeadingPrefixAndInputPrecedence(t *testing.T) {
	base := `{"model":"fixture","messages":[{"role":"system","content":"policy"},{"role":"user","content":"first"}]}`
	seed := contentSessionForTest(base)
	if seed == "" {
		t.Fatal("chat user anchor missing")
	}
	for _, changed := range []string{
		strings.TrimSuffix(base, `]}`) + `,{"role":"system","content":"later policy"},{"role":"assistant","content":"reply"},{"role":"user","content":"next"}]}`,
		strings.TrimSuffix(base, `}`) + `,"input":"unrelated response input"}`,
	} {
		if got := contentSessionForTest(changed); got != seed {
			t.Fatal("chat fallback changed on later system content or non-authoritative input")
		}
	}
	if got := contentSessionForTest(strings.Replace(base, `"policy"`, `"different"`, 1)); got == seed {
		t.Fatal("leading chat system prompt did not change session")
	}
}

func TestGatewayContentSessionBoundsSelectedFieldsBeforeCanonicalization(t *testing.T) {
	for name, body := range map[string]string{
		"oversize_user":     `{"model":"fixture","input":"` + strings.Repeat("x", maxGatewayContentSessionAnchor+1) + `"}`,
		"combined_budget":   `{"model":"fixture","instructions":"` + strings.Repeat("x", 600000) + `","input":"` + strings.Repeat("y", 600000) + `"}`,
		"escaped_expansion": `{"model":"fixture","input":"` + strings.Repeat("<", 200000) + `"}`,
		"large_image":       `{"model":"fixture","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,` + strings.Repeat("A", 10<<20) + `"}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := contentSessionForTest(body); got != "" {
				t.Fatal("oversize content created a partial or unbounded hard session")
			}
		})
	}
	base := `{"model":"fixture","input":[{"role":"user","content":"first prompt"}]}`
	largeHistory := strings.TrimSuffix(base, `]}`) + `,{"type":"function_call_output","call_id":"a","output":"` + strings.Repeat("x", 2<<20) + `"}]}`
	if got := contentSessionForTest(largeHistory); got == "" || got != contentSessionForTest(base) {
		t.Fatal("large appended tool result consumed the selected anchor budget")
	}
}

func TestGatewayContentSessionNilRequestAndExplicitPrecedence(t *testing.T) {
	body := []byte(`{"model":"fixture","input":"first prompt"}`)
	if got := gatewaySessionKey(nil, body); got == "" {
		t.Fatal("nil request lost body content fallback")
	}
	if got := gatewaySessionKey(nil, nil); got != "" {
		t.Fatal("nil request/body created an identity")
	}
	r := httptest.NewRequest(http.MethodPost, "/responses", nil)
	r.Header.Set("Session-Id", "native")
	r.Header.Set("X-Conversation-Id", "alias")
	if got := gatewaySessionKey(r, body); got != "native" {
		t.Fatal("content displaced native session")
	}
	r.Header.Del("Session-Id")
	if got := gatewaySessionKey(r, body); got != "alias" {
		t.Fatal("content displaced compatible session alias")
	}
	if got := gatewaySessionKey(nil, []byte(`{"model":"fixture","input":"first prompt","prompt_cache_key":"custom"}`)); got != "custom" {
		t.Fatal("content displaced explicit cache key")
	}
}

func TestGatewayContentSessionPolicySeparatesSharedCache(t *testing.T) {
	settings := defaultSettings()
	settings.BuiltinPromptEnabled = false
	first, firstSession, err := applyGatewayPolicy(nil, []byte(`{"model":"fixture","instructions":"shared policy","input":"project A"}`), "tenant", settings)
	if err != nil || firstSession == "" {
		t.Fatalf("first content session: %q %v", firstSession, err)
	}
	second, secondSession, err := applyGatewayPolicy(nil, []byte(`{"model":"fixture","instructions":"shared policy","input":"project B"}`), "tenant", settings)
	if err != nil || secondSession == "" || secondSession == firstSession {
		t.Fatalf("independent prompts merged hard bindings: %q %v", secondSession, err)
	}
	if policyString(t, first, "prompt_cache_key") != "" || policyString(t, second, "prompt_cache_key") != "" {
		t.Fatal("local content session manufactured an upstream prompt cache key")
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(first, &fields)
	if _, exists := fields["session_id"]; exists {
		t.Fatal("local content fallback was injected into the upstream body")
	}
	settings.PromptCacheKeyEnabled = false
	_, sessionWithoutCache, err := applyGatewayPolicy(nil, []byte(`{"model":"fixture","instructions":"shared policy","input":"project A"}`), "tenant", settings)
	if err != nil || sessionWithoutCache != firstSession {
		t.Fatal("prompt-cache switch disabled or changed conversation ownership")
	}
	settings.StickySessionsEnabled = false
	if _, session, err := applyGatewayPolicy(nil, []byte(`{"model":"fixture","input":"project A"}`), "tenant", settings); err != nil || session != "" {
		t.Fatal("disabled sticky sessions still created a content binding")
	}
}

func TestGatewayContentSessionTTLDoesNotShortenExplicitSessions(t *testing.T) {
	for _, tc := range []struct {
		name, session string
		want          time.Duration
	}{
		{"inferred", contentSessionForTest(`{"model":"fixture","input":"first prompt"}`), time.Hour},
		{"explicit", "native-session", gatewaySessionTTL},
		{"explicit_similar_prefix", "compat_cs_native-session", gatewaySessionTTL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestProxy(t, 1)
			if _, err := p.sessionAccount(t.Context(), "tenant", tc.session, "original"); err != nil {
				t.Fatal(err)
			}
			p.sessionMu.Lock()
			defer p.sessionMu.Unlock()
			if len(p.sessionRoutes) != 1 {
				t.Fatal("session was not persisted")
			}
			for _, route := range p.sessionRoutes {
				if remaining := time.Until(route.Expires); remaining < tc.want-5*time.Second || remaining > tc.want {
					t.Fatalf("session TTL=%v want=%v", remaining, tc.want)
				}
			}
		})
	}
}

func TestGatewayContentSessionPersistsAcrossPriorityChangesAndRejectsOwnerReplacement(t *testing.T) {
	p, target, observed := researchSessionFixture(t, false)
	const body = `{"model":"fixture","stream":true,"instructions":"shared","input":[{"role":"user","content":"same project"}]}`
	if status := researchSessionSend(t, target, body, ""); status != 200 {
		t.Fatal(status)
	}
	if first := <-observed; first.owner != "original" || first.session != "" {
		t.Fatal("initial priority/forwarded session premise changed")
	}
	researchSessionPreferAlternate(t, p)
	continuation := strings.TrimSuffix(body, `]}`) + `,{"role":"assistant","content":"reply"},{"type":"reasoning","encrypted_content":"original-owner-history"},{"role":"user","content":"next"}]}`
	if status := researchSessionSend(t, target, continuation, ""); status != 200 {
		t.Fatal(status)
	}
	if second := <-observed; second.owner != "original" || second.session != "" {
		t.Fatal("content conversation drifted after scheduler priorities changed")
	}
	if err := p.Store.UpdateAccount("test", func(account *Account) error {
		account.AccountID = "replacement-owner"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if status := researchSessionSend(t, target, continuation, ""); status == 200 {
		t.Fatal("old content history reached a replacement owner")
	}
	select {
	case <-observed:
		t.Fatal("owner replacement failure still dispatched the old content")
	default:
	}
}

func TestGatewayContentSessionUsesTenantNamespaceAndConfiguredTTL(t *testing.T) {
	p := newTestProxy(t, 1)
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.StickySessionTTLSeconds = 120
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	session := contentSessionForTest(`{"model":"fixture","input":"same project"}`)
	if session == "" {
		t.Fatal("missing content session")
	}
	for tenant, account := range map[string]string{"tenant-a": "account-a", "tenant-b": "account-b"} {
		if got, err := p.sessionAccount(t.Context(), tenant, session, account); err != nil || got != account {
			t.Fatalf("tenant content binding: %q %v", got, err)
		}
		if got, err := p.sessionAccount(t.Context(), tenant, session, ""); err != nil || got != account {
			t.Fatalf("tenant content lookup: %q %v", got, err)
		}
	}
	p.sessionMu.Lock()
	defer p.sessionMu.Unlock()
	if len(p.sessionRoutes) != 2 {
		t.Fatal("identical content merged two tenant namespaces")
	}
	for _, route := range p.sessionRoutes {
		remaining := time.Until(route.Expires)
		if remaining < 115*time.Second || remaining > 120*time.Second {
			t.Fatalf("configured session TTL not preserved: %v", remaining)
		}
	}
}
