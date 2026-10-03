package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

// A real loopback handler receives growing full histories without any client
// session signal. Routing these turns at random discards account-local warmth.
func TestGatewayContentAffinityHeaderlessHistoryStaysOnAccount(t *testing.T) {
	p, target, observed := researchSessionFixture(t, false)
	for _, name := range []string{"test", "alternate"} {
		if err := p.Store.UpdateAccount(name, func(a *Account) error { a.Priority = 0; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	p.invalidateAccounts()
	instructions, _ := json.Marshal(strings.Repeat("Stable reusable application instructions. ", 256))
	owners := make(map[string]int)
	for turn := 0; turn < 48; turn++ {
		input := `[{"role":"user","content":"Inspect the same project"}`
		for prior := 0; prior < turn; prior++ {
			input += fmt.Sprintf(`,{"type":"function_call_output","call_id":"call-%d","output":"tool result %d"}`, prior, prior)
		}
		input += `]`
		body := fmt.Sprintf(`{"model":"fixture","stream":true,"instructions":%s,"input":%s}`, instructions, input)
		if status := researchSessionSend(t, target, body, ""); status != 200 {
			t.Fatalf("turn %d failed: %d", turn, status)
		}
		wire := <-observed
		owners[wire.owner]++
		if wire.session != "" || string(wire.body["input"]) != input {
			t.Fatal("soft affinity changed the client's session or history")
		}
	}
	if len(owners) != 1 {
		t.Fatalf("same headerless history drifted across accounts: %v", owners)
	}
	p.sessionMu.Lock()
	bindings := len(p.sessionRoutes)
	p.sessionMu.Unlock()
	if bindings != 1 {
		t.Fatalf("anchored content must retain one conversation binding, got %d", bindings)
	}
}

func TestGatewaySoftAffinitySeedUsesBoundedStableContent(t *testing.T) {
	base := `{"model":"fixture","instructions":"stable","tools":[{"type":"function","name":"tool","parameters":{"maximum":9007199254740993,"type":"number"}}],"input":[{"role":"developer","content":"project policy"},{"role":"user","content":[{"type":"input_text","text":"work on project"}]}]}`
	seed, ok := gatewayCacheAffinitySeed("tenant", []byte(base))
	if !ok {
		t.Fatal("text history has no affinity")
	}
	for _, changed := range []string{
		strings.TrimSuffix(base, `]}`) + `,{"type":"function_call_output","call_id":"new","output":"tool output"},{"role":"assistant","content":"reply"},{"role":"user","content":"next"},{"role":"developer","content":"later changing context"}]}`,
		strings.Replace(base, `"maximum":9007199254740993,"type":"number"`, `"type":"number","maximum":9007199254740993`, 1),
		strings.TrimSuffix(base, `}`) + `,"reasoning":{"effort":"high"},"prompt_cache_key":"generated","client_metadata":{"turn_id":"new"}}`,
	} {
		if got, ok := gatewayCacheAffinitySeed("tenant", []byte(changed)); !ok || got != seed {
			t.Fatal("appended history, control or equivalent encoding split the content anchor")
		}
	}
	for _, changed := range []string{
		strings.Replace(base, `"fixture"`, `"different"`, 1),
		strings.Replace(base, `"stable"`, `"different"`, 1),
		strings.Replace(base, `"project policy"`, `"different"`, 1),
		strings.Replace(base, `"work on project"`, `"different"`, 1),
		strings.Replace(base, `9007199254740993`, `9007199254740992`, 1),
	} {
		if got, ok := gatewayCacheAffinitySeed("tenant", []byte(changed)); !ok || got == seed {
			t.Fatal("model, prefix or first-user change did not split the content anchor")
		}
	}
	if got, ok := gatewayCacheAffinitySeed("other-tenant", []byte(base)); !ok || got == seed {
		t.Fatal("content affinity is not tenant isolated")
	}
	for _, body := range []string{
		`{}`, `{"model":"fixture"}`, `{"model":"fixture","input":[]}`, `{"model":"fixture","tools":[ ]}`,
		`{"model":"fixture","input":[{"role":"user","content":[{"type":"input_image","image_url":"opaque"}]}]}`,
		`{"model":"fixture","input":[{"type":"reasoning","role":"user","content":"opaque"}]}`,
		`{"model":"fixture","input":"` + strings.Repeat("x", maxGatewayCacheAffinityAnchor+1) + `"}`,
		`{"model":"fixture","input":[]} trailing`,
	} {
		if _, ok := gatewayCacheAffinitySeed("tenant", []byte(body)); ok {
			t.Fatal("missing/opaque/oversize/invalid input created content affinity")
		}
	}
	for _, body := range []string{
		`{"model":"fixture","input":"first prompt"}`,
		`{"model":"fixture","input":[{"type":"input_text","text":"first prompt"}]}`,
		`{"model":"fixture","instructions":"stable prefix","input":[]}`,
		`{"model":"fixture","input":[{"role":"system","content":"stable prefix"}]}`,
		`{"model":"fixture","tools":[{"type":"function","name":"tool"}]}`,
	} {
		if _, ok := gatewayCacheAffinitySeed("tenant", []byte(body)); !ok {
			t.Fatalf("supported text/prefix has no affinity: %s", body)
		}
	}
	textItem := `{"model":"fixture","input":[{"type":"input_text","text":"first prompt"}]}`
	textSeed, _ := gatewayCacheAffinitySeed("tenant", []byte(textItem))
	textContinuation := strings.TrimSuffix(textItem, `]}`) + `,{"type":"function_call_output","call_id":"tool","output":"result"}]}`
	if got, ok := gatewayCacheAffinitySeed("tenant", []byte(textContinuation)); !ok || got != textSeed {
		t.Fatal("direct input_text anchor changed when tool history was appended")
	}
}

func TestGatewaySoftAffinityHonorsSwitchesAndExplicitIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, tenant, session string
		sticky, cache         bool
		want                  bool
	}{
		{"enabled", "tenant", "", true, true, true},
		{"explicit_session", "tenant", "original-session", true, true, false},
		{"sticky_off", "tenant", "", false, true, false},
		{"cache_off", "tenant", "", true, false, false},
		{"no_tenant", "", "", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := Settings{StickySessionsEnabled: tc.sticky, PromptCacheKeyEnabled: tc.cache}
			body := []byte(`{"model":"fixture","input":"first prompt"}`)
			ctx := withGatewayCacheAffinity(t.Context(), tc.tenant, tc.session, body, settings)
			_, exists := ctx.Value(gatewayCacheAffinityContextKey{}).(gatewayCacheAffinity)
			if exists != tc.want {
				t.Fatalf("affinity=%v want=%v", exists, tc.want)
			}
			if string(body) != `{"model":"fixture","input":"first prompt"}` {
				t.Fatal("affinity changed the body")
			}
		})
	}
}

func TestGatewaySoftAffinityWeightedOrderAndMinimalMovement(t *testing.T) {
	accounts := []Account{{ID: "a", Weight: 1}, {ID: "b", Weight: 3}, {ID: "c", Weight: 1}}
	counts := make(map[string]int)
	for i := 0; i < 6000; i++ {
		seed, _ := gatewayCacheAffinitySeed("tenant", []byte(fmt.Sprintf(`{"model":"m","input":"prompt %d"}`, i)))
		ctx := context.WithValue(t.Context(), gatewayCacheAffinityContextKey{}, gatewayCacheAffinity{seed})
		winner := gatewayCacheAffinityPick(ctx, accounts)
		counts[winner.ID]++
		reordered := []Account{accounts[2], accounts[0], accounts[1]}
		if gatewayCacheAffinityPick(ctx, reordered).ID != winner.ID {
			t.Fatal("candidate ordering changed deterministic selection")
		}
		subset := slices.DeleteFunc(slices.Clone(accounts), func(a Account) bool { return a.ID != winner.ID && a.ID != "b" })
		if gatewayCacheAffinityPick(ctx, subset).ID != winner.ID {
			t.Fatal("removing a losing account displaced the winner")
		}
	}
	if counts["b"] < 3300 || counts["b"] > 3900 || counts["a"] < 1000 || counts["c"] < 1000 {
		t.Fatalf("weighted content distribution was lost: %v", counts)
	}
	for _, weight := range []int{0, -1, math.MaxInt} {
		seed, _ := gatewayCacheAffinitySeed("tenant", []byte(`{"model":"m","input":"prompt"}`))
		ctx := context.WithValue(t.Context(), gatewayCacheAffinityContextKey{}, gatewayCacheAffinity{seed})
		if got := gatewayCacheAffinityPick(ctx, []Account{{ID: "a", Weight: weight}}); got.ID != "a" {
			t.Fatal("extreme/default weight made the sole account unselectable")
		}
	}
}

func TestGatewaySoftAffinityPreservesAdmissionAndFallback(t *testing.T) {
	for _, condition := range []string{"busy", "cooldown", "disabled", "unauthorized", "credentials", "priority"} {
		t.Run(condition, func(t *testing.T) {
			p := newTestProxy(t, 1)
			if err := p.Store.ImportWith("alternate", []byte(`{"access_token":"alternate-token"}`), AccountDefaults{Concurrency: 1}); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"test", "alternate"} {
				if err := p.Store.UpdateAccount(name, func(a *Account) error { a.Groups = []string{"allowed"}; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			key, err := p.Keys.Create("affinity-key", []string{"allowed"})
			if err != nil {
				t.Fatal(err)
			}
			settings, _ := p.Settings.Get()
			ctx := withGatewayCacheAffinity(withGatewayCooldownModel(t.Context(), "fixture"), key.ID, "", []byte(`{"model":"fixture","input":"same project"}`), settings)
			first, release, _, _, err := p.pickAccount(ctx, key.Key)
			if err != nil {
				t.Fatal(err)
			}
			other := "test"
			if first.Name == other {
				other = "alternate"
			}
			if condition == "busy" {
				defer release()
			} else {
				release()
			}
			switch condition {
			case "cooldown":
				enableRateLimitCooldownForTest(t, p)
				p.recordGatewayCooldown(ctx, first, "60")
			case "disabled", "unauthorized", "credentials":
				if err := p.Store.UpdateAccount(first.Name, func(a *Account) error {
					switch condition {
					case "disabled":
						a.Enabled = false
					case "unauthorized":
						a.Groups = []string{"forbidden"}
					case "credentials":
						a.ExpiresAt, a.RefreshToken = 1, ""
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			case "priority":
				if err := p.Store.UpdateAccount(other, func(a *Account) error { a.Priority = 100; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			// Retain the original ranking snapshot for access changes: the
			// existing authoritative row/key checks must reject it after ranking.
			if condition != "disabled" && condition != "unauthorized" {
				p.invalidateAccounts()
			}
			second, done, _, _, err := p.pickAccount(ctx, key.Key)
			if done != nil {
				done()
			}
			if err != nil || second.Name != other {
				t.Fatalf("soft preference bypassed %s: selected=%s err=%v", condition, second.Name, err)
			}
			if len(p.sessionRoutes) != 0 {
				t.Fatal("fallback created a hard session")
			}
		})
	}
}

func TestGatewaySoftAffinityStableProxyWithoutSession(t *testing.T) {
	p := newTestProxy(t, 1)
	account, _ := p.Store.Get("test")
	for i := range 3 {
		entry, err := p.Proxies.Create(ProxyEntry{Name: fmt.Sprintf("affinity-proxy-%d", i), Type: "http", Host: "localhost", Port: 8500 + i})
		if err != nil {
			t.Fatal(err)
		}
		account.Proxies = append(account.Proxies, entry.ID)
	}
	settings, _ := p.Settings.Get()
	want := ""
	for range 10 {
		ctx := withGatewayRequestRoute(t.Context(), "tenant", "")
		ctx = withGatewayCacheAffinity(ctx, "tenant", "", []byte(`{"model":"fixture","input":"same project"}`), settings)
		proxy, err := p.gatewayRequestProxy(ctx, account)
		if err != nil || proxy == nil {
			t.Fatalf("proxy selection failed: %v", err)
		}
		if want == "" {
			want = proxy.ID
		} else if proxy.ID != want {
			t.Fatal("headerless content affinity rotated the outbound proxy")
		}
		if ctx.Value(gatewayRequestRouteKey{}).(*gatewayRequestRoute).session != "" {
			t.Fatal("proxy affinity was promoted to a real session")
		}
	}
}

func TestGatewaySoftAffinityHeaderlessWebSocketConnections(t *testing.T) {
	observed := make(chan string, 1)
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		_, _, err = conn.Read(r.Context())
		if err != nil {
			t.Error(err)
			return
		}
		if r.Header.Get("Session-Id") != "" || r.Header.Get("session_id") != "" || r.Header.Get("conversation_id") != "" {
			t.Error("soft affinity generated upstream session headers")
		}
		observed <- r.Header.Get("Authorization")
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"affinity-response","status":"completed","output":[]}}`))
		_, _, _ = conn.Read(r.Context())
	})
	enableGatewayWS(t, p, "")
	if err := p.Store.ImportWith("alternate", []byte(`{"access_token":"alternate-token"}`), AccountDefaults{Concurrency: 1}); err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(p)
	defer gateway.Close()
	want := ""
	for i := 0; i < 16; i++ {
		conn, _ := gatewayWSDial(t, gateway.URL, "")
		if err := conn.Write(t.Context(), websocket.MessageText, []byte(`{"type":"response.create","model":"fixture","input":[{"role":"user","content":"same project"}]}`)); err != nil {
			t.Fatal(err)
		}
		_ = gatewayWSRead(t, conn)
		owner := <-observed
		if want == "" {
			want = owner
		} else if owner != want {
			t.Fatal("headerless WebSocket connection changed account")
		}
		_ = conn.CloseNow()
	}
	p.sessionMu.Lock()
	bindings := len(p.sessionRoutes)
	p.sessionMu.Unlock()
	if bindings != 1 {
		t.Fatalf("headerless WebSocket content did not retain one session: %d", bindings)
	}
}

func BenchmarkGatewaySoftAffinityLargeToolHistory(b *testing.B) {
	body := []byte(`{"model":"fixture","instructions":"stable","input":[{"role":"user","content":"project anchor"},{"type":"function_call_output","call_id":"tool","output":"` + strings.Repeat("x", 8<<20) + `"}]}`)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, ok := gatewayCacheAffinitySeed("tenant", body); !ok {
			b.Fatal("large history lost its bounded first-user anchor")
		}
	}
}
