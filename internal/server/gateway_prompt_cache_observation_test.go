package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func promptCacheObserverForTest(t *testing.T) *gatewayPromptCacheObserver {
	t.Helper()
	o, err := newGatewayPromptCacheObserver()
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func promptCacheObservationAccount() Account {
	return Account{ID: "private-account", Name: "private-name", AccountID: "private-workspace", AccessToken: "private-token", Identity: &managedAccountIdentity{UserID: "private-user"}}
}

func TestPromptCacheObservationComparesCanonicalPrefixAndHistory(t *testing.T) {
	o := promptCacheObserverForTest(t)
	r := httptest.NewRequest("POST", "/responses", nil)
	r.Header.Set("Session-Id", "private-session")
	r.Header.Set("Thread-Id", "private-thread")
	first := []byte(`{"model":"fixture","instructions":"private-中文","tools":[{"name":"a","parameters":{"n":9007199254740993,"type":"integer"}}],"prompt_cache_key":"private-cache","reasoning":{"effort":"high"},"input":[{"role":"user","content":"private-question"}]}`)
	second := []byte(`{"input":[{"content":"private-question","role":"user"},{"role":"assistant","content":"private-response"}],"reasoning":{"effort":"high"},"prompt_cache_key":"private-cache","tools":[{"parameters":{"type":"integer","n":9007199254740993},"name":"a"}],"instructions":"private-\u4e2d\u6587","model":"fixture"}`)
	before := bytes.Clone(second)
	at := time.Unix(1000, 0)
	a := o.observeAt(r, first, "private-tenant", promptCacheObservationAccount(), "explicit", at)
	b := o.observeAt(r, second, "private-tenant", promptCacheObservationAccount(), "explicit", at.Add(1200*time.Millisecond))
	if a.Comparison != "first_observed" || b.Comparison != "compared" || b.PreviousAgeMS == nil || *b.PreviousAgeMS != 1200 || a.ScopeHash != b.ScopeHash || a.PrefixHash != b.PrefixHash || b.InputHistory != "extended" || b.SharedInputItems != 1 || b.ModelChanged || b.InstructionsChanged || b.ToolsChanged || b.ControlsChanged || b.CacheKeyChanged || b.AccountChanged {
		t.Fatalf("incorrect canonical comparison: %+v %+v", a, b)
	}
	if !bytes.Equal(second, before) {
		t.Fatal("observation mutated request")
	}
	record := RequestRecord{InputTokens: 2000, OutputTokens: 70, CachedTokens: 1024, PromptCache: b}
	serialized, err := json.Marshal(record)
	if err != nil || bytes.Contains(serialized, []byte("private-")) || record.InputTokens != 2000 || record.CachedTokens != 1024 || record.OutputTokens != 70 {
		t.Fatal("observation retained private content or changed usage")
	}
	var decoded RequestRecord
	if json.Unmarshal(serialized, &decoded) != nil || decoded.PromptCache == nil || decoded.PromptCache.ScopeHash != b.ScopeHash || decoded.InputTokens != 2000 {
		t.Fatal("request record cannot retain passive cache observation")
	}
}

func TestPromptCacheObservationAccountAndCredentialChangesHaveSeparateMeaning(t *testing.T) {
	o := promptCacheObserverForTest(t)
	r := httptest.NewRequest("POST", "/responses", nil)
	r.Header.Set("Session-Id", "root")
	r.Header.Set("Thread-Id", "thread")
	r.Header.Set("Authorization", "Bearer private-inbound-one")
	body := []byte(`{"model":"fixture","prompt_cache_key":"cache-one","input":[]}`)
	account := promptCacheObservationAccount()
	first := o.Observe(r, body, "tenant", account, "explicit")
	r.Header.Set("Authorization", "Bearer private-inbound-two")
	account.AccessToken, account.RefreshToken = "private-refreshed-token", "private-refreshed-refresh"
	refreshed := o.Observe(r, body, "tenant", account, "explicit")
	if refreshed.Comparison != "compared" || refreshed.AccountChanged || first.ScopeHash != refreshed.ScopeHash || first.AccountHash != refreshed.AccountHash {
		t.Fatal("credential refresh split known-owner observation")
	}
	account.ID, account.AccountID = "new-account", "new-workspace"
	switched := o.Observe(r, bytes.ReplaceAll(body, []byte("cache-one"), []byte("cache-two")), "tenant", account, "explicit")
	if switched.Comparison != "compared" || !switched.AccountChanged || !switched.CacheKeyChanged || first.ScopeHash != switched.ScopeHash {
		t.Fatal("account or key switch hidden by the comparison scope")
	}
	otherTenant := o.Observe(r, body, "another-tenant", account, "explicit")
	r.Header.Set("Thread-Id", "sibling")
	sibling := o.Observe(r, body, "tenant", account, "explicit")
	if otherTenant.Comparison != "first_observed" || sibling.Comparison != "first_observed" || otherTenant.ScopeHash == first.ScopeHash || sibling.ScopeHash == first.ScopeHash {
		t.Fatal("different tenant or sibling histories were compared")
	}
}

func TestPromptCacheObservationDetectsEachChangedBoundary(t *testing.T) {
	for _, test := range []struct {
		name, body string
		check      func(*GatewayPromptCacheObservation) bool
	}{
		{"model", `{"model":"other","instructions":"same","tools":[{"n":9007199254740993}],"reasoning":{"effort":"high"},"prompt_cache_key":"same","input":[{"content":"old"}]}`, func(o *GatewayPromptCacheObservation) bool { return o.ModelChanged }},
		{"instructions", `{"model":"fixture","instructions":"different","tools":[{"n":9007199254740993}],"reasoning":{"effort":"high"},"prompt_cache_key":"same","input":[{"content":"old"}]}`, func(o *GatewayPromptCacheObservation) bool { return o.InstructionsChanged }},
		{"tools exact integer", `{"model":"fixture","instructions":"same","tools":[{"n":9007199254740992}],"reasoning":{"effort":"high"},"prompt_cache_key":"same","input":[{"content":"old"}]}`, func(o *GatewayPromptCacheObservation) bool { return o.ToolsChanged }},
		{"controls", `{"model":"fixture","instructions":"same","tools":[{"n":9007199254740993}],"reasoning":{"effort":"low"},"prompt_cache_key":"same","input":[{"content":"old"}]}`, func(o *GatewayPromptCacheObservation) bool { return o.ControlsChanged }},
		{"cache key", `{"model":"fixture","instructions":"same","tools":[{"n":9007199254740993}],"reasoning":{"effort":"high"},"prompt_cache_key":"different","input":[{"content":"old"}]}`, func(o *GatewayPromptCacheObservation) bool { return o.CacheKeyChanged }},
		{"history", `{"model":"fixture","instructions":"same","tools":[{"n":9007199254740993}],"reasoning":{"effort":"high"},"prompt_cache_key":"same","input":[{"content":"new"}]}`, func(o *GatewayPromptCacheObservation) bool {
			return o.InputHistory == "changed" && o.SharedInputItems == 0
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := promptCacheObserverForTest(t)
			r := httptest.NewRequest("POST", "/responses", nil)
			r.Header.Set("Thread-Id", "thread")
			before := []byte(`{"model":"fixture","instructions":"same","tools":[{"n":9007199254740993}],"reasoning":{"effort":"high"},"prompt_cache_key":"same","input":[{"content":"old"}]}`)
			o.Observe(r, before, "tenant", promptCacheObservationAccount(), "explicit")
			after := o.Observe(r, []byte(test.body), "tenant", promptCacheObservationAccount(), "explicit")
			if after.Comparison != "compared" || !test.check(after) {
				t.Fatalf("change lost: %+v", after)
			}
		})
	}
}

func TestPromptCacheObservationLabelsWeakScopesAndProcessSalt(t *testing.T) {
	o, other := promptCacheObserverForTest(t), promptCacheObserverForTest(t)
	body := []byte(`{"prompt_cache_key":"private-shared","input":[]}`)
	a := o.Observe(nil, body, "tenant", promptCacheObservationAccount(), "explicit")
	b := other.Observe(nil, body, "tenant", promptCacheObservationAccount(), "explicit")
	c := o.Observe(nil, []byte(`{"model":"fixture","input":[]}`), "tenant", promptCacheObservationAccount(), "absent")
	metadata := o.Observe(nil, []byte(`{"client_metadata":{"session_id":"s","thread_id":"t"},"input":[]}`), "tenant", promptCacheObservationAccount(), "absent")
	if a.ScopeKind != "cache_key" || c.ScopeKind != "prefix" || metadata.ScopeKind != "session_thread" || a.ScopeHash == b.ScopeHash || a.CacheKeyHash == b.CacheKeyHash || a.AccountHash == b.AccountHash || a.Instance == b.Instance {
		t.Fatal("weak scope classification or per-process privacy salt lost")
	}
}

func TestPromptCacheObservationBodyCoverageBreaksComparisonChain(t *testing.T) {
	o := promptCacheObserverForTest(t)
	r := httptest.NewRequest("POST", "/responses", nil)
	r.Header.Set("Thread-Id", "thread")
	valid := []byte(`{"input":[]}`)
	o.Observe(r, valid, "tenant", promptCacheObservationAccount(), "explicit")
	large := []byte(`{"input":"` + strings.Repeat("p", gatewayPromptCacheObservationBodyLimit) + `"}`)
	limited := o.Observe(r, large, "tenant", promptCacheObservationAccount(), "private-untrusted-mode")
	if limited.Coverage != "body_limit" || limited.Comparison != "skipped" || limited.KeyMode != "unobserved" || limited.PrefixHash != "" || limited.ScopeHash == "" {
		t.Fatalf("body limit not distinguished: %+v", limited)
	}
	next := o.Observe(r, valid, "tenant", promptCacheObservationAccount(), "explicit")
	if next.Comparison != "previous_unobserved" || next.InputHistory != "" {
		t.Fatal("skipped request falsely became a comparable predecessor")
	}
	for _, raw := range []string{"", "null", "[]", "{", `{"x":NaN}`} {
		bad := o.Observe(r, []byte(raw), "tenant", promptCacheObservationAccount(), "explicit")
		if bad.Coverage != "invalid_body" || bad.Comparison != "skipped" {
			t.Fatalf("invalid JSON accepted: %q", raw)
		}
	}
}

func TestPromptCacheObservationBoundsItemsAndScopes(t *testing.T) {
	o := promptCacheObserverForTest(t)
	r := httptest.NewRequest("POST", "/responses", nil)
	r.Header.Set("Thread-Id", "long")
	items := strings.Repeat(`{"content":"private-item"},`, gatewayPromptCacheObservationItems)
	body := []byte(`{"input":[` + items + `{"content":"last"}]}`)
	a := o.Observe(r, body, "tenant", promptCacheObservationAccount(), "explicit")
	b := o.Observe(r, body, "tenant", promptCacheObservationAccount(), "explicit")
	if a.InputItems != 129 || a.InputComparedItems != 128 || !a.InputLimited || b.InputHistory != "comparison_limited" || b.SharedInputItems != 128 {
		t.Fatal("bounded item scan claimed full equality")
	}
	for index := 0; index < gatewayPromptCacheObservationScopes; index++ {
		r.Header.Set("Thread-Id", fmt.Sprint(index))
		o.Observe(r, []byte(`{"input":[]}`), "tenant", promptCacheObservationAccount(), "explicit")
	}
	if len(o.latest) != gatewayPromptCacheObservationScopes || o.order.Len() != gatewayPromptCacheObservationScopes {
		t.Fatal("scope state was not bounded")
	}
	r.Header.Set("Thread-Id", "long")
	if again := o.Observe(r, body, "tenant", promptCacheObservationAccount(), "explicit"); again.Comparison != "first_observed" {
		t.Fatal("evicted history was retained or described as a cold upstream cache")
	}
	for _, element := range o.latest {
		if snapshot := element.Value.(gatewayPromptCacheSnapshot); len(snapshot.input) > gatewayPromptCacheObservationItems {
			t.Fatal("stored item hashes exceed bound")
		}
	}
}

func TestPromptCacheObservationStatefulDeltaIsNotFullHistory(t *testing.T) {
	o := promptCacheObserverForTest(t)
	r := httptest.NewRequest("POST", "/responses", nil)
	r.Header.Set("Thread-Id", "thread")
	o.Observe(r, []byte(`{"input":[{"content":"first"}]}`), "tenant", promptCacheObservationAccount(), "explicit")
	delta := o.Observe(r, []byte(`{"previous_response_id":"private-response","input":[]}`), "tenant", promptCacheObservationAccount(), "explicit")
	if !delta.StatefulInput || delta.Comparison != "compared" || delta.InputHistory != "not_comparable" {
		t.Fatal("delta input misclassified as lost full history")
	}
}

func TestPromptCacheObservationKeyModeUsesOriginalExactFields(t *testing.T) {
	settings := defaultSettings()
	for _, test := range []struct{ body, session, want string }{
		{`{"prompt_cache_key":"s","client_metadata":{"session_id":"s"}}`, "", "codex_default"},
		{`{"prompt_cache_key":"custom","client_metadata":{"session_id":"s"}}`, "", "explicit_override"},
		{`{"prompt_cache_key":"s"}`, "s", "explicit_session_match"},
		{`{"Prompt_Cache_Key":"ignored","input":[]}`, "", "absent"},
		{`{"input":[]}`, "s", "absent"},
		{`{"input":[]}`, "", "absent"},
		{`{"prompt_cache_key":"","input":[]}`, "s", "absent"},
		{`{"prompt_cache_key":null,"input":[]}`, "s", "absent"},
		{`{"prompt_cache_key":"old","prompt_cache_key":null,"input":[]}`, "s", "absent"},
		{`{"prompt_cache_key":"s","prompt_cache_key":"other","client_metadata":{"session_id":"s"}}`, "", "explicit_override"},
		{`{"prompt_cache_key":"s","client_metadata":{"session_id":"other","session_id":"s"}}`, "", "codex_default"},
	} {
		r := httptest.NewRequest("POST", "/responses", nil)
		r.Header.Set("Session-Id", test.session)
		if got := gatewayPromptCacheObservationKeyMode(r, []byte(test.body), settings); got != test.want {
			t.Fatalf("mode=%s want=%s for %s", got, test.want, test.body)
		}
	}
	settings.PromptCacheKeyEnabled = false
	if gatewayPromptCacheObservationKeyMode(nil, []byte(`{"prompt_cache_key":"custom"}`), settings) != "disabled" {
		t.Fatal("disabled policy became generated key")
	}
}

func TestPromptCacheObservationAbsentKeyStaysAbsentAfterPolicyAndAccountScoping(t *testing.T) {
	settings := defaultSettings()
	settings.BuiltinPromptEnabled = false
	for _, test := range []struct {
		name, body, header string
	}{
		{"content", `{"model":"fixture","input":"first user"}`, ""},
		{"header", `{"model":"fixture","input":"first user"}`, "Session-Id"},
		{"alias", `{"model":"fixture","input":"first user"}`, "X-Session-Affinity"},
		{"body_session", `{"model":"fixture","input":"first user","client_metadata":{"session_id":"s","thread_id":"t"}}`, ""},
		{"null", `{"model":"fixture","input":"first user","prompt_cache_key":null}`, "Session-Id"},
		{"empty", `{"model":"fixture","input":"first user","prompt_cache_key":""}`, "Session-Id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/responses", nil)
			if test.header != "" {
				r.Header.Set(test.header, "original-session")
			}
			inbound := []byte(test.body)
			mode := gatewayPromptCacheObservationKeyMode(r, inbound, settings)
			prepared, session, err := applyGatewayPolicy(r, inbound, "tenant", settings)
			if err != nil || session == "" {
				t.Fatalf("local identity missing: %q %v", session, err)
			}
			prepared, err = prepareGatewayAccountPromptCache(prepared, "tenant", promptCacheObservationAccount(), settings)
			if err != nil {
				t.Fatal(err)
			}
			observation := promptCacheObserverForTest(t).Observe(r, prepared, "tenant", promptCacheObservationAccount(), mode)
			if mode != "absent" || observation.KeyMode != "absent" || observation.CacheKeyPresent || observation.Coverage != "complete" {
				t.Fatalf("local identity was reported as a generated cache key: %+v", observation)
			}
			var before, after map[string]json.RawMessage
			_ = json.Unmarshal(inbound, &before)
			_ = json.Unmarshal(prepared, &after)
			if !bytes.Equal(before["prompt_cache_key"], after["prompt_cache_key"]) {
				t.Fatal("absent, null or empty cache field changed at the wire boundary")
			}
		})
	}
}

func TestPromptCacheObservationSafeKeyModeRetainsHistoricalLabels(t *testing.T) {
	for _, mode := range []string{"absent", "generated_session", "generated_prefix"} {
		observation := promptCacheObserverForTest(t).Observe(nil, []byte(`{"input":[]}`), "tenant", promptCacheObservationAccount(), mode)
		if observation.KeyMode != mode {
			t.Fatalf("recognized observation label lost: %q", mode)
		}
	}
	if got := gatewayPromptCacheSafeKeyMode("private-untrusted-mode"); got != "unobserved" {
		t.Fatal("untrusted diagnostic mode escaped the fixed vocabulary")
	}
}

func TestPromptCacheObservationWirePrefixPreservesLargeMessageSuffixEvidence(t *testing.T) {
	o := promptCacheObserverForTest(t)
	r := httptest.NewRequest("POST", "/responses", nil)
	r.Header.Set("Session-Id", "session")
	r.Header.Set("Thread-Id", "thread")
	message := strings.Repeat("private-large-prefix ", 60000)
	first := []byte(`{"input":[{"role":"user","content":"` + message + `first suffix"}]}`)
	second := []byte(`{"input":[{"role":"user","content":"` + message + `different suffix"}]}`)
	a := o.Observe(r, first, "tenant", promptCacheObservationAccount(), "explicit")
	b := o.Observe(r, second, "tenant", promptCacheObservationAccount(), "explicit")
	if a.Coverage != "complete" || !a.HeaderSessionPresent || !a.HeaderThreadPresent || b.SharedInputItems != 0 || b.InputHistory != "changed" || b.SharedInputWirePrefixBytes != 1<<20 || !b.InputWirePrefixLimited {
		t.Fatalf("large single item suffix concealed its wire prefix: %+v", b)
	}
	for _, element := range o.latest {
		if snapshot := element.Value.(gatewayPromptCacheSnapshot); len(snapshot.inputWire) != gatewayPromptCacheWireBlocks {
			t.Fatal("wire prefix hash count exceeded bound")
		}
	}
	serialized, _ := json.Marshal(b)
	if bytes.Contains(serialized, []byte("private-large-prefix")) {
		t.Fatal("wire prefix text retained in record")
	}
}

func TestPromptCacheObservationConcurrentAndNilArePassive(t *testing.T) {
	var absent *gatewayPromptCacheObserver
	if absent.Observe(nil, []byte(`{}`), "tenant", Account{}, "explicit") != nil {
		t.Fatal("disabled observer created metadata")
	}
	o := promptCacheObserverForTest(t)
	var wg sync.WaitGroup
	for index := 0; index < 32; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			r := httptest.NewRequest("POST", "/responses", nil)
			r.Header.Set("Thread-Id", fmt.Sprint(index))
			for range 16 {
				if got := o.Observe(r, []byte(`{"input":[]}`), "tenant", promptCacheObservationAccount(), "explicit"); got.Coverage != "complete" {
					t.Error("concurrent observer lost coverage")
				}
			}
		}(index)
	}
	wg.Wait()
	if len(o.latest) != 32 {
		t.Fatal("concurrent scope tracking lost entries")
	}
}

func TestPromptCacheObservationLocalAliasesPreserveScopeWithoutInventingNativeHeaders(t *testing.T) {
	for _, header := range []string{"x-session-affinity", "x-session-id", "x-opencode-session", "x-conversation-id"} {
		t.Run(header, func(t *testing.T) {
			o := promptCacheObserverForTest(t)
			r := httptest.NewRequest("POST", "/responses", nil)
			r.Header[header] = []string{"private-alias"}
			body := []byte(`{"input":[]}`)
			first := o.Observe(r, body, "tenant", promptCacheObservationAccount(), "absent")
			next := o.Observe(r, body, "tenant", promptCacheObservationAccount(), "absent")
			if first.ScopeKind != "alias_session" || !first.HeaderAliasPresent || first.HeaderSessionPresent || first.HeaderThreadPresent || next.Comparison != "compared" || r.Header.Get("Session-Id") != "" {
				t.Fatalf("local alias became weak/native identity: %+v", next)
			}
			if mode := gatewayPromptCacheObservationKeyMode(r, body, defaultSettings()); mode != "absent" {
				t.Fatal("local alias was reported as a generated cache key")
			}
			r.Header.Set("Connection", header)
			stripped := o.Observe(r, body, "tenant", promptCacheObservationAccount(), "absent")
			if stripped.ScopeKind != "prefix" || stripped.HeaderAliasPresent {
				t.Fatal("Connection-excluded alias changed observation identity")
			}
			r.Header.Del("Connection")
			r.Header.Set("Thread-Id", "native-thread")
			native := o.Observe(r, body, "tenant", promptCacheObservationAccount(), "absent")
			if native.ScopeKind != "thread" || !native.HeaderThreadPresent {
				t.Fatal("local alias replaced a native identity")
			}
		})
	}
}
