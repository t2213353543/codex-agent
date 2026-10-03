package server

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestGatewaySub2CacheNamespacePreservesOriginalKeys(t *testing.T) {
	a := Account{ID: "one", AccountID: "workspace", AccessToken: "at", Identity: &managedAccountIdentity{UserID: "user"}}
	settings := defaultSettings()
	key := func(a Account, raw string) string {
		t.Helper()
		body, err := prepareGatewayAccountPromptCache([]byte(raw), "tenant", a, settings)
		if err != nil {
			t.Fatal(err)
		}
		return policyString(t, body, "prompt_cache_key")
	}
	first := key(a, `{"prompt_cache_key":"session-a","client_metadata":{"session_id":"session-a"}}`)
	second := key(a, `{"prompt_cache_key":"session-b","client_metadata":{"session_id":"session-b"}}`)
	if len(first) != 36 || first == second {
		t.Fatal("different original sessions must retain different UUID cache identities")
	}
	if explicit := key(a, `{"prompt_cache_key":"caller-override"}`); explicit == "caller-override" || len(explicit) != 36 {
		t.Fatal("explicit keys must receive the same tenant/credential namespace")
	}
	a.ID, a.Name, a.OwnerRevision, a.AccessToken = "another-local-row", "renamed", "revision-two", "refreshed"
	if key(a, `{"prompt_cache_key":"session-a","client_metadata":{"session_id":"session-a"}}`) != first {
		t.Fatal("same credential owner must share namespace after refresh, reimport, or local row change")
	}
}

func TestGatewaySub2CacheNamespacePreservesNonidentityBytes(t *testing.T) {
	a := Account{ID: "one", AccountID: "workspace", Identity: &managedAccountIdentity{UserID: "user"}}
	raw := []byte(`{ "prompt_cache_key" : "s", "client_metadata" : {"session_id":"s","thread_id":"s", "installation_id":"device", "turn_id":"turn", "window_id":"window", "x-client-request-id":"request", "opaque":{"n":9007199254740993}, "x-codex-turn-metadata":"{ \"session_id\" : \"s\", \"thread_id\":\"t\", \"n\":9007199254740993, \"escaped\":\"\u0061\", \"device_id\":\"device\" }"}, "input":[{"encrypted_content":"opaque+/="}], "tools":[{"name":"z","duplicate":1,"duplicate":2}] }`)
	next, err := prepareGatewayAccountPromptCache(raw, "tenant", a, defaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Key      string `json:"prompt_cache_key"`
		Metadata struct {
			Session string `json:"session_id"`
			Thread  string `json:"thread_id"`
			Nested  string `json:"x-codex-turn-metadata"`
		} `json:"client_metadata"`
	}
	if json.Unmarshal(next, &body) != nil || len(body.Key) != 36 || body.Metadata.Session != body.Key || body.Metadata.Thread == body.Metadata.Session {
		t.Fatal("default key/session alignment or per-kind identity failed")
	}
	var nested struct {
		Session string `json:"session_id"`
		Thread  string `json:"thread_id"`
	}
	if json.Unmarshal([]byte(body.Metadata.Nested), &nested) != nil || nested.Session != body.Metadata.Session || len(nested.Thread) != 36 {
		t.Fatal("embedded metadata did not receive consistent scope")
	}
	restored := bytes.ReplaceAll(next, []byte(body.Key), []byte("s"))
	restored = bytes.ReplaceAll(restored, []byte(body.Metadata.Thread), []byte("s"))
	restored = bytes.ReplaceAll(restored, []byte(nested.Thread), []byte("t"))
	if !bytes.Equal(restored, raw) {
		t.Fatalf("nonidentity bytes changed\ngot  %s\nwant %s", restored, raw)
	}
}

func TestGatewaySub2CacheNamespacePreflightIncludesMetadataGrowth(t *testing.T) {
	a := Account{ID: "one", AccountID: "workspace", Identity: &managedAccountIdentity{UserID: "user"}}
	base := `{"prompt_cache_key":"s","client_metadata":{"session_id":"s","thread_id":"t","session-id":"s","thread-id":"t","x-codex-turn-metadata":"{\"session_id\":\"s\",\"thread_id\":\"t\"}"},"padding":""}`
	small, err := prepareGatewayAccountPromptCache([]byte(base), "tenant", a, defaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	growth := len(small) - len(base)
	if growth < 100 {
		t.Fatal("fixture did not rewrite metadata identities")
	}
	for _, spare := range []int{growth - 1, growth, growth + 1} {
		raw := []byte(strings.Replace(base, `"padding":""`, `"padding":"`+strings.Repeat("x", maxBody-len(base)-spare)+`"`, 1))
		preflight := validateGatewayAccountPromptCacheSize(raw, "tenant", defaultSettings())
		prepared, err := prepareGatewayAccountPromptCache(raw, "tenant", a, defaultSettings())
		if (preflight != nil) != (spare < growth) || (err != nil) != (preflight != nil) {
			t.Fatalf("preflight and actual size diverged with %d spare bytes", spare)
		}
		if err == nil && len(prepared) > maxBody {
			t.Fatal("oversized prepared body")
		}
	}
}

func TestGatewaySub2CacheNamespaceIndependentVectorsAndFrozenKind(t *testing.T) {
	a := Account{AccountID: "workspace", Identity: &managedAccountIdentity{UserID: "user"}}
	// Derived independently with Python hashlib/json/uuid using the documented
	// tuple and RFC UUID version/variant bits, rather than another Go code path.
	const session = "469cf823-3c7a-4a8b-b5ed-cea41c5d3282"
	if got := gatewayScopeCacheIdentityValue(a, "tenant", "session", " s "); got != session {
		t.Fatalf("UUID derivation changed: %q", got)
	}
	if got := gatewayUpstreamCacheSession("tenant", a, " s "); got != "333490f69f06df29" {
		t.Fatalf("upstream session derivation changed: %q", got)
	}
	settings := defaultSettings()
	raw := []byte(`{"prompt_cache_key":"s","previous_response_id":"response-one"}`)
	next, err := prepareGatewayAccountPromptCacheWithKind(raw, "tenant", a, settings, "session")
	if err != nil || gatewayPromptCacheKeyValue(next) != session {
		t.Fatal("partial continuation lost original session-kind projection")
	}
	unfrozen, err := prepareGatewayAccountPromptCache(raw, "tenant", a, settings)
	if err != nil || gatewayPromptCacheKeyValue(unfrozen) == session {
		t.Fatal("explicit cache key must use a separate kind by default")
	}
	defaultBody := []byte(`{"prompt_cache_key":"s","client_metadata":{"session_id":"s"}}`)
	frozenExplicit, err := prepareGatewayAccountPromptCacheWithKind(defaultBody, "tenant", a, settings, "prompt-cache")
	if err != nil || gatewayPromptCacheKeyValue(frozenExplicit) != gatewayPromptCacheKeyValue(unfrozen) || !bytes.Contains(frozenExplicit, []byte(`"session_id":"`+session+`"`)) {
		t.Fatal("frozen explicit-key kind changed metadata session projection")
	}
	for _, body := range []string{`{}`, `{"prompt_cache_key":null}`, `{"prompt_cache_key":" "}`, `{"Prompt_Cache_Key":"s"}`, `{"prompt_cache_key":"s"} trailing`} {
		if gatewayPromptCacheKeyValue([]byte(body)) != "" {
			t.Fatalf("invalid key was recognized in %s", body)
		}
	}
}

func TestGatewaySub2CacheNamespaceEmbeddedUnicodeOffsets(t *testing.T) {
	a := Account{AccountID: "workspace", Identity: &managedAccountIdentity{UserID: "user"}}
	const target = `\"session_id\":\"s\"`
	const session = "469cf823-3c7a-4a8b-b5ed-cea41c5d3282"
	for _, prefix := range []string{
		`\"plain\":\"abc\",`,
		`\"unicode\":\"你好😀\",`,
		`\"escaped\":\"\u4f60\u597d\",`,
		`\"pair\":\"\uD83D\uDE00\",`,
		`\"lone_high\":\"\uD800\",`,
		`\"lone_low\":\"\uDC00\",`,
		`\"bad_pair\":\"\uD800\u0041\",`,
		`\"slashes\":\"\\n\\t\\\\\\\"\",`,
		`\"invalid_utf8\":\"` + string([]byte{0xff}) + `\",`,
	} {
		raw := []byte(`{"client_metadata":{"x-codex-turn-metadata":"{` + prefix + target + `,\"big\":9007199254740993}"}}`)
		if !json.Valid(raw) {
			t.Fatalf("invalid fixture %q", prefix)
		}
		next, err := prepareGatewayAccountPromptCache(raw, "tenant", a, defaultSettings())
		if err != nil {
			t.Fatal(err)
		}
		want := bytes.Replace(raw, []byte(target), []byte(`\"session_id\":\"`+session+`\"`), 1)
		if !bytes.Equal(next, want) {
			t.Fatalf("embedded offset changed unknown bytes: prefix=%q\ngot %s\nwant %s", prefix, next, want)
		}
	}
}

func TestGatewaySub2CacheNamespaceMetadataOnlyPreflight(t *testing.T) {
	a := Account{AccountID: "workspace", Identity: &managedAccountIdentity{UserID: "user"}}
	for _, base := range []string{
		`{"client_metadata":{"session_id":"s","thread_id":"t"},"padding":""}`,
		`{"prompt_cache_key":"s","client_metadata":{"session_id":"s","session-id":"s","thread_id":"t","thread-id":"t","x-codex-turn-metadata":"{\"session_id\":\"s\",\"session-id\":\"s\",\"thread_id\":\"t\",\"thread-id\":\"t\"}"},"padding":""}`,
	} {
		small, err := prepareGatewayAccountPromptCache([]byte(base), "tenant", a, defaultSettings())
		if err != nil {
			t.Fatal(err)
		}
		growth := len(small) - len(base)
		for _, delta := range []int{0, 1} {
			raw := []byte(strings.Replace(base, `"padding":""`, `"padding":"`+strings.Repeat("x", maxBody-len(base)-growth+delta)+`"`, 1))
			preflight := validateGatewayAccountPromptCacheSize(raw, "tenant", defaultSettings())
			mapped, err := prepareGatewayAccountPromptCache(raw, "tenant", a, defaultSettings())
			if (preflight != nil) != (delta == 1) || (err != nil) != (preflight != nil) || err == nil && len(mapped) != maxBody {
				t.Fatalf("metadata-only or maximum alias expansion escaped preflight: delta=%d growth=%d", delta, growth)
			}
		}
	}
}
