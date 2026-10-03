package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/klauspost/compress/zstd"
)

func TestGatewayDefaultPromptCachePreservesIndependentSessionCacheKeys(t *testing.T) {
	runGatewayScopedPromptCacheAcrossSessions(t, false)
}

func TestGatewayNativeDefaultPromptCachePreservesIndependentSessionCacheKeys(t *testing.T) {
	runGatewayScopedPromptCacheAcrossSessions(t, true)
}

func runGatewayScopedPromptCacheAcrossSessions(t *testing.T, native bool) {
	for _, mode := range []string{"json", "sse", "zstd", "compact", "websocket"} {
		t.Run(mode, func(t *testing.T) {
			type capture struct {
				body             []byte
				session, account string
			}
			captured := make(chan capture, 2)
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				var body []byte
				if mode == "websocket" {
					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.CloseNow()
					_, body, err = conn.Read(r.Context())
					if err != nil {
						t.Error(err)
						return
					}
					captured <- capture{body, r.Header.Get("Session-Id"), r.Header.Get("Authorization")}
					_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"cache-fixture","status":"completed","output":[]}}`))
					return
				}
				body, _ = io.ReadAll(r.Body)
				if mode == "zstd" {
					decoder, err := zstd.NewReader(nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer decoder.Close()
					body, err = decoder.DecodeAll(body, nil)
					if err != nil {
						t.Error(err)
						return
					}
				}
				captured <- capture{body, r.Header.Get("Session-Id"), r.Header.Get("Authorization")}
				if mode == "compact" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"object":"response.compaction","output":[]}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"cache-fixture\",\"status\":\"completed\",\"output\":[]}}\n\n")
			})
			enableGatewayWS(t, p, "")
			if native {
				realGatewayNativePool(t, p)
				p.NativeWebSocket = true
			}
			server := httptest.NewServer(p)
			defer server.Close()
			var first string
			for index := 0; index < 2; index++ {
				session := fmt.Sprintf("independent-root-%d", index)
				raw := fmt.Sprintf(`{"type":"response.create","model":"fixture","instructions":"Stable application instructions","prompt_cache_key":%q,"client_metadata":{"session_id":%q,"thread_id":%q,"opaque":{"n":9007199254740993}},"tools":[{"name":"z"},{"name":"a"}],"input":[{"role":"user","content":"different suffix %d"}],"stream":%t}`, session, session, session, index, mode != "json")
				if mode == "websocket" {
					conn, _ := gatewayWSDial(t, server.URL, session)
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					err := conn.Write(ctx, websocket.MessageText, []byte(raw))
					cancel()
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Contains(gatewayWSRead(t, conn), []byte(`"response.completed"`)) {
						t.Fatal("missing terminal event")
					}
					_ = conn.CloseNow()
					waitGatewayWSReleased(t, p)
				} else {
					path := "/responses"
					if mode == "compact" {
						path += "/compact"
					}
					body := []byte(raw)
					if mode == "zstd" {
						encoder, err := zstd.NewWriter(nil)
						if err != nil {
							t.Fatal(err)
						}
						body = encoder.EncodeAll(body, nil)
						encoder.Close()
					}
					r := proxyRequest(http.MethodPost, path, string(body))
					r.Header.Set("Session-Id", session)
					if mode == "zstd" {
						r.Header.Set("Content-Encoding", "zstd")
					}
					w := httptest.NewRecorder()
					p.ServeHTTP(w, r)
					if w.Code != http.StatusOK {
						t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
					}
				}
				got := <-captured
				if got.session != session || got.account != "Bearer test-upstream-token" {
					t.Fatal("cache mapping changed account routing")
				}
				var before, after map[string]json.RawMessage
				_ = json.Unmarshal([]byte(raw), &before)
				_ = json.Unmarshal(got.body, &after)
				for _, field := range []string{"input", "tools", "instructions"} {
					if !bytes.Equal(before[field], after[field]) {
						t.Fatalf("cache mapping changed %s", field)
					}
				}
				key := policyString(t, got.body, "prompt_cache_key")
				if len(key) != 36 || key == session {
					t.Errorf("default CLI session did not receive a scoped UUID: key length=%d", len(key))
				}
				if first != "" && first == key {
					t.Error("independent default sessions collapsed into one account cache bucket")
				}
				first = key
			}
		})
	}
}

func TestGatewayAccountPromptCachePreservesOpaqueBytesAndOverrides(t *testing.T) {
	settings := defaultSettings()
	account := Account{ID: "account", AccountID: "workspace", AccessToken: "secret", Identity: &managedAccountIdentity{UserID: "user"}}
	const raw = `{ "prompt_cache_key" : "default", "client_metadata" : {"session_id":"default","unknown": {"n":9007199254740993}}, "input" : [{"type":"reasoning","encrypted_content":"opaque+/="}], "tools":[{"name":"z","duplicate":1,"duplicate":2}], "prompt_cache_options":{"breakpoints":[2,0]}, "previous_response_id":"resp-original" }`
	prepared, err := prepareGatewayAccountPromptCache([]byte(raw), "api-key", account, settings)
	if err != nil {
		t.Fatal(err)
	}
	key := policyString(t, prepared, "prompt_cache_key")
	want := strings.ReplaceAll(raw, `"default"`, `"`+key+`"`)
	if string(prepared) != want || len(key) != 36 {
		t.Fatal("cache projection rewrote bytes outside selected identity values")
	}
	repeat, err := prepareGatewayAccountPromptCache([]byte(raw), "api-key", account, settings)
	if err != nil || !bytes.Equal(repeat, prepared) {
		t.Fatal("same original request did not produce stable mapping")
	}
	for _, item := range []struct{ name, raw string }{
		{"missing_key", `{"input":[]}`},
		{"wrong_case", `{"Prompt_Cache_Key":"default","Client_Metadata":{"session_id":"default"}}`},
		{"empty", `{"prompt_cache_key":"","client_metadata":{"session_id":""}}`},
		{"whitespace", `{"prompt_cache_key":"  ","client_metadata":{"session_id":" "}}`},
		{"wrong_types", `{"prompt_cache_key":42,"client_metadata":{"session_id":null,"thread_id":[]}}`},
		{"malformed", `{"prompt_cache_key":"default","client_metadata":{"session_id":"default"}} trailing`},
	} {
		t.Run(item.name, func(t *testing.T) {
			got, err := prepareGatewayAccountPromptCache([]byte(item.raw), "api-key", account, settings)
			if err != nil || string(got) != item.raw {
				t.Fatal("absent/invalid identity was changed")
			}
		})
	}
	for _, original := range []string{"chosen-cache", "review:parent-thread", " default "} {
		raw := []byte(fmt.Sprintf(`{"prompt_cache_key":%q,"client_metadata":{"session_id":"default"}}`, original))
		got, err := prepareGatewayAccountPromptCache(raw, "api-key", account, settings)
		if err != nil || policyString(t, got, "prompt_cache_key") != gatewayScopeCacheIdentityValue(account, "api-key", "prompt-cache", original) {
			t.Fatal("caller override did not retain its own scoped identity")
		}
	}
	settings.PromptCacheKeyEnabled = false
	got, err := prepareGatewayAccountPromptCache([]byte(raw), "api-key", account, settings)
	if err != nil || string(got) != raw {
		t.Fatal("disabled cache projection modified request")
	}
}

func TestGatewayAccountPromptCacheOwnerAndTenantIsolation(t *testing.T) {
	settings := defaultSettings()
	const raw = `{"prompt_cache_key":"default","client_metadata":{"session_id":"default"}}`
	account := Account{ID: "account", Name: "original", AccountID: "workspace", AccessToken: "original-access", RefreshToken: "original-refresh", OwnerRevision: "owner-one", Identity: &managedAccountIdentity{UserID: "user"}}
	keyFor := func(keyID string, a Account) string {
		t.Helper()
		got, err := prepareGatewayAccountPromptCache([]byte(raw), keyID, a, settings)
		if err != nil {
			t.Fatal(err)
		}
		return policyString(t, got, "prompt_cache_key")
	}
	first := keyFor("tenant-a", account)
	refreshed := account
	refreshed.AccessToken, refreshed.RefreshToken = "rotated-access", "rotated-refresh"
	refreshed.ID, refreshed.Name, refreshed.Version, refreshed.OwnerRevision = "another-row", "renamed", 100, "owner-two"
	if keyFor("tenant-a", refreshed) != first {
		t.Fatal("known-owner refresh or local management fields split cache key")
	}
	// gatewayAccountRef still enforces independent local conversation ownership;
	// sharing an upstream cache namespace does not relax a sticky owner binding.
	if gatewayAccountRef(refreshed) == gatewayAccountRef(account) {
		t.Fatal("fixture failed to retain independent local binding identity")
	}
	for _, item := range []struct {
		name   string
		mutate func(*Account)
		keyID  string
	}{
		{"tenant", func(*Account) {}, "tenant-b"},
		{"workspace", func(a *Account) { a.AccountID = "other-workspace" }, "tenant-a"},
		{"user", func(a *Account) { a.Identity = &managedAccountIdentity{UserID: "other-user"} }, "tenant-a"},
	} {
		t.Run(item.name, func(t *testing.T) {
			next := account
			item.mutate(&next)
			if keyFor(item.keyID, next) == first {
				t.Fatal("cache namespace crossed tenant/owner boundary")
			}
		})
	}
	account.Identity = nil
	unknownKey := keyFor("tenant-a", account)
	account.AccessToken = "unknown-new-access"
	if keyFor("tenant-a", account) == unknownKey {
		t.Fatal("unknown-owner credential rotation reused cache namespace")
	}
}

func TestGatewayAccountPromptCacheValidatedSpanBoundaries(t *testing.T) {
	settings := defaultSettings()
	account := Account{ID: "span-account", AccessToken: "synthetic"}
	session := gatewayScopeCacheIdentityValue(account, "tenant", "session", "session")
	prompt := gatewayScopeCacheIdentityValue(account, "tenant", "prompt-cache", "session")
	chosen := gatewayScopeCacheIdentityValue(account, "tenant", "prompt-cache", "chosen")
	for _, test := range []struct{ name, raw, want string }{
		{"escaped_field_names", `{"pr\u006fmpt_cache_key" : "session","cl\u0069ent_metadata":{"sess\u0069on_id":"session"}}`, `{"pr\u006fmpt_cache_key" : "SESSION","cl\u0069ent_metadata":{"sess\u0069on_id":"SESSION"}}`},
		{"escaped_value", `{"prompt_cache_key" : "sess\u0069on","client_metadata":{"session_id":"session"}}`, `{"prompt_cache_key" : "SESSION","client_metadata":{"session_id":"SESSION"}}`},
		{"nested_same_names", `{"unknown":{"prompt_cache_key":"override","client_metadata":{"session_id":"override"}},"prompt_cache_key" : "session","client_metadata":{"nested":{"session_id":"other"},"session_id":"session"}}`, `{"unknown":{"prompt_cache_key":"override","client_metadata":{"session_id":"override"}},"prompt_cache_key" : "SESSION","client_metadata":{"nested":{"session_id":"other"},"session_id":"SESSION"}}`},
		{"last_cache_wins", `{"prompt_cache_key":"override","client_metadata":{"session_id":"session"},"prompt_cache_key" : "session"}`, `{"prompt_cache_key":"override","client_metadata":{"session_id":"SESSION"},"prompt_cache_key" : "SESSION"}`},
		{"last_metadata_wins", `{"client_metadata":{"session_id":"wrong"},"prompt_cache_key" : "session","client_metadata":{"session_id":"session"}}`, `{"client_metadata":{"session_id":"wrong"},"prompt_cache_key" : "SESSION","client_metadata":{"session_id":"SESSION"}}`},
		{"last_session_wins", `{"prompt_cache_key" : "session","client_metadata":{"session_id":null,"session_id":"session"}}`, `{"prompt_cache_key" : "SESSION","client_metadata":{"session_id":null,"session_id":"SESSION"}}`},
		{"duplicate_same_value", `{"prompt_cache_key" : "session","client_metadata":{"session_id":"session"},"prompt_cache_key" : "session"}`, `{"prompt_cache_key" : "session","client_metadata":{"session_id":"SESSION"},"prompt_cache_key" : "SESSION"}`},
		{"nested_only", `{"input":[{"prompt_cache_key":"session","client_metadata":{"session_id":"session"}}]}`, ""},
		{"array_metadata", `{"prompt_cache_key":"session","client_metadata":[{"session_id":"session"}]}`, `{"prompt_cache_key":"PROMPT","client_metadata":[{"session_id":"session"}]}`},
		{"last_cache_override", `{"prompt_cache_key":"session","client_metadata":{"session_id":"session"},"prompt_cache_key":"chosen"}`, `{"prompt_cache_key":"session","client_metadata":{"session_id":"SESSION"},"prompt_cache_key":"CHOSEN"}`},
		{"last_cache_null", `{"prompt_cache_key":"session","client_metadata":{"session_id":"session"},"prompt_cache_key":null}`, `{"prompt_cache_key":"session","client_metadata":{"session_id":"SESSION"},"prompt_cache_key":null}`},
		{"last_metadata_null", `{"client_metadata":{"session_id":"session"},"prompt_cache_key":"session","client_metadata":null}`, `{"client_metadata":{"session_id":"session"},"prompt_cache_key":"PROMPT","client_metadata":null}`},
		{"last_session_wrong_type", `{"prompt_cache_key":"session","client_metadata":{"session_id":"session","session_id":[]}}`, `{"prompt_cache_key":"PROMPT","client_metadata":{"session_id":"session","session_id":[]}}`},
		{"metadata_without_key", `{"client_metadata":{"session_id":"session"}}`, `{"client_metadata":{"session_id":"SESSION"}}`},
		{"array_document", `[{"prompt_cache_key":"session","client_metadata":{"session_id":"session"}}]`, ""},
		{"primitive_document", ` true `, ""},
		{"trailing_document", `{"prompt_cache_key":"session"} {}`, ""},
		{"missing_brace", `{"prompt_cache_key":"session"`, ""},
		{"bad_escape", `{"prompt_cache_key":"session","text":"\q"}`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := prepareGatewayAccountPromptCache([]byte(test.raw), "tenant", account, settings)
			if err != nil {
				t.Fatal(err)
			}
			want := test.raw
			if test.want != "" {
				want = strings.NewReplacer("SESSION", session, "PROMPT", prompt, "CHOSEN", chosen).Replace(test.want)
			}
			if string(got) != want {
				t.Fatalf("span rewrite crossed JSON boundary:\ngot  %s\nwant %s", got, want)
			}
		})
	}
}

func TestGatewayAccountPromptCacheLargeUnchangedBodyDoesNotAllocateCopies(t *testing.T) {
	settings := defaultSettings()
	account := Account{ID: "allocation-account", AccessToken: "synthetic"}
	var baseline float64
	for _, size := range []int{64, 8 << 20} {
		body := []byte(`{"input":[{"content":"` + strings.Repeat("x", size) + `"}],"unrelated_metadata":{"session_id":"chosen"}}`)
		allocations := testing.AllocsPerRun(5, func() {
			got, err := prepareGatewayAccountPromptCache(body, "tenant", account, settings)
			if err != nil || len(got) != len(body) || &got[0] != &body[0] {
				t.Fatal("unchanged request was copied or rejected")
			}
		})
		if size == 64 {
			baseline = allocations
		}
		if allocations > 2 || allocations > baseline {
			t.Fatalf("unrecognized request size=%d allocated unnecessary owner/parser copies: %.0f allocations (small body %.0f)", size, allocations, baseline)
		}
	}
}
