package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Observation probes, not a model simulator. The upstream always returns
// synthetic terminal events and records exactly which client payload arrived.
func TestCacheReviewWSTogglePreservesIncrementalWireKey(t *testing.T) {
	for _, initiallyEnabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("initially_enabled_%v", initiallyEnabled), func(t *testing.T) {
			type wire struct {
				Key, Previous string
				Input         json.RawMessage
			}
			captured := make(chan wire, 4)
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				for i := 0; i < 4; i++ {
					_, raw, err := conn.Read(r.Context())
					if err != nil {
						t.Error(err)
						return
					}
					var body map[string]json.RawMessage
					if err := json.Unmarshal(raw, &body); err != nil {
						t.Error(err)
						return
					}
					var got wire
					_ = json.Unmarshal(body["prompt_cache_key"], &got.Key)
					_ = json.Unmarshal(body["previous_response_id"], &got.Previous)
					got.Input = body["input"]
					captured <- got
					payload := fmt.Sprintf(`{"type":"response.completed","response":{"id":"review-response-%d","status":"completed","output":[]}}`, i+1)
					if err := conn.Write(r.Context(), websocket.MessageText, []byte(payload)); err != nil {
						t.Error(err)
						return
					}
				}
			})
			enableGatewayWS(t, p, "")
			settings, err := p.Settings.Get()
			if err != nil {
				t.Fatal(err)
			}
			settings.PromptCacheKeyEnabled = initiallyEnabled
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(p)
			defer server.Close()
			conn, _ := gatewayWSDial(t, server.URL, "review-session")
			defer conn.CloseNow()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			first := `{"type":"response.create","model":"fixture","instructions":"","prompt_cache_key":"review-session","client_metadata":{"session_id":"review-session"},"input":[{"role":"user","content":"original history"}]}`
			if err := conn.Write(ctx, websocket.MessageText, []byte(first)); err != nil {
				t.Fatal(err)
			}
			if got := gatewayWSRead(t, conn); !strings.Contains(string(got), `"response.completed"`) {
				t.Fatalf("first did not complete: %s", got)
			}
			one := <-captured
			settings, err = p.Settings.Get()
			if err != nil {
				t.Fatal(err)
			}
			settings.PromptCacheKeyEnabled = !initiallyEnabled
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			second := `{"type":"response.create","model":"fixture","instructions":"","prompt_cache_key":"review-session","client_metadata":{"session_id":"review-session"},"previous_response_id":"review-response-1","input":[]}`
			bad := strings.Replace(second, `"prompt_cache_key":"review-session"`, `"prompt_cache_key":"caller-changed-key"`, 1)
			if err := conn.Write(ctx, websocket.MessageText, []byte(bad)); err != nil {
				t.Fatal(err)
			}
			if reply := gatewayWSRead(t, conn); !strings.Contains(string(reply), "resend full input") {
				t.Fatal("changed key continuation was forwarded")
			}
			if err := conn.Write(ctx, websocket.MessageText, []byte(second)); err != nil {
				t.Fatal(err)
			}
			if got := gatewayWSRead(t, conn); !strings.Contains(string(got), `"response.completed"`) {
				t.Fatalf("second did not complete: %s", got)
			}
			two := <-captured
			if one.Key != two.Key {
				t.Fatal("cache policy change corrupted the incremental wire key")
			}
			if two.Previous != "review-response-1" || string(two.Input) != "[]" {
				t.Fatal("premise disproved: gateway rebuilt the full request")
			}
			if err := conn.Write(ctx, websocket.MessageText, []byte(first)); err != nil {
				t.Fatal(err)
			}
			if got := gatewayWSRead(t, conn); !strings.Contains(string(got), `"response.completed"`) {
				t.Fatal("new full chain did not complete")
			}
			three := <-captured
			if three.Key == one.Key || three.Previous != "" {
				t.Fatal("new full chain did not apply the latest cache policy")
			}
			if err := conn.Write(ctx, websocket.MessageText, []byte(second)); err != nil {
				t.Fatal(err)
			}
			if got := gatewayWSRead(t, conn); !strings.Contains(string(got), `"response.completed"`) {
				t.Fatal("old chain branch did not complete")
			}
			four := <-captured
			if four.Key != one.Key || four.Previous != "review-response-1" {
				t.Fatal("old response branch inherited the new independent chain policy")
			}
			t.Logf("observed_key_change=false previous_response_id_preserved=true empty_delta_preserved=true initially_enabled=%v first_key_length=%d second_key_length=%d", initiallyEnabled, len(one.Key), len(two.Key))
		})
	}
}

func TestCacheReviewHTTPFinalRejectionDoesNotBindSession(t *testing.T) {
	var sent atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		sent.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.BuiltinPromptEnabled = false
	settings.PromptCacheKeyEnabled, settings.StickySessionsEnabled = true, true
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	const session = "oversize-review"
	before, err := p.sessionAccount(t.Context(), "admin", session, "")
	if err != nil || before != "" {
		t.Fatal("fixture session not empty")
	}
	const head = `{"model":"test","stream":true,"store":false,"instructions":"","input":[],"prompt_cache_key":"s","client_metadata":{"session_id":"s"},"padding":"`
	const tail = `"}`
	raw := head + strings.Repeat("x", maxBody-32-len(head)-len(tail)) + tail
	r := proxyRequest(http.MethodPost, "/responses", raw)
	r.Header.Set("Session-Id", session)
	policy, gotSession, err := applyGatewayPolicy(r, []byte(raw), "admin", settings)
	if err != nil || gotSession != session {
		t.Fatal("pre-policy premise differs")
	}
	prepared, _, err := prepareBody(policy, false)
	if err != nil || len(prepared) != maxBody-32 {
		t.Fatalf("pre-admission size premise differs: length=%d err=%v", len(prepared), err)
	}
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepareGatewayAccountPromptCache(prepared, "admin", account, settings); !errors.Is(err, errGatewayPreparedBodyLimit) {
		t.Fatal("final rewrite did not exceed body limit")
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	after, err := p.sessionAccount(t.Context(), "admin", session, "")
	if err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusRequestEntityTooLarge || sent.Load() != 0 {
		t.Fatalf("status=%d upstream_calls=%d", w.Code, sent.Load())
	}
	if after != "" {
		t.Fatal("locally rejected unsent request retained a new session binding")
	}
	if p.inFlight("test") != 0 {
		t.Fatal("admission release unexpectedly leaked")
	}
	t.Logf("http_status=%d upstream_calls=0 fresh_binding_retained=false admission_released=true inbound_bytes=%d cache_rewrite_growth_bytes=63", w.Code, len(raw))
}

func TestCacheReviewSettingDefaultsAndExplicitFalse(t *testing.T) {
	s := NewSettingsStore(filepath.Join(t.TempDir(), "settings.json"))
	initial, err := s.Get()
	if err != nil || !initial.PromptCacheKeyEnabled {
		t.Fatal("missing persisted setting did not default true")
	}
	initial.PromptCacheKeyEnabled = false
	if err := validateSettings(initial); err != nil {
		t.Fatal(err)
	}
	if initial.PromptCacheKeyEnabled {
		t.Fatal("validation modified explicit false")
	}
	if err := s.Set(initial); err != nil {
		t.Fatal(err)
	}
	reloaded := NewSettingsStore(s.store.path)
	got, err := reloaded.Get()
	if err != nil || got.PromptCacheKeyEnabled {
		t.Fatal("explicit false did not survive disk reload")
	}
	t.Log("default_true=true validator_force_true=false explicit_false_persists=true no_new_independent_switch=true")
}

func TestCacheReviewFailedFirstTurnDoesNotFreezePolicy(t *testing.T) {
	for _, initial := range []bool{false, true} {
		t.Run(fmt.Sprint(initial), func(t *testing.T) {
			var sent atomic.Int32
			captured := make(chan []byte, 1)
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				_, raw, err := conn.Read(r.Context())
				if err != nil {
					t.Error(err)
					return
				}
				sent.Add(1)
				captured <- raw
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"after-local-failure","status":"completed","output":[]}}`))
			})
			enableGatewayWS(t, p, "")
			s, _ := p.Settings.Get()
			s.PromptCacheKeyEnabled = initial
			if err := p.Settings.Set(s); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(p)
			defer server.Close()
			conn, _ := gatewayWSDial(t, server.URL, "failed-first")
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			invalid := `{"type":"response.create","model":17,"prompt_cache_key":"s","client_metadata":{"session_id":"s"},"input":[]}`
			if err := conn.Write(ctx, websocket.MessageText, []byte(invalid)); err != nil {
				t.Fatal(err)
			}
			if reply := gatewayWSRead(t, conn); !strings.Contains(string(reply), `"type":"error"`) {
				t.Fatal("invalid first turn was accepted")
			}
			if sent.Load() != 0 {
				t.Fatal("invalid first turn reached upstream")
			}
			s, _ = p.Settings.Get()
			s.PromptCacheKeyEnabled = !initial
			if err := p.Settings.Set(s); err != nil {
				t.Fatal(err)
			}
			valid := strings.Replace(invalid, `"model":17`, `"model":"fixture"`, 1)
			if err := conn.Write(ctx, websocket.MessageText, []byte(valid)); err != nil {
				t.Fatal(err)
			}
			if reply := gatewayWSRead(t, conn); !strings.Contains(string(reply), `"response.completed"`) {
				t.Fatal("corrected first turn failed")
			}
			key := policyString(t, <-captured, "prompt_cache_key")
			if (key == "s") != initial {
				t.Fatal("locally rejected first turn froze its obsolete policy")
			}
		})
	}
}

func TestCacheReviewUnknownPriorDefaultRequiresFullRequest(t *testing.T) {
	var sent atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) { sent.Add(1); w.WriteHeader(500) })
	enableGatewayWS(t, p, "")
	server := httptest.NewServer(p)
	defer server.Close()
	conn, _ := gatewayWSDial(t, server.URL, "")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	raw := `{"type":"response.create","model":"fixture","prompt_cache_key":"s","client_metadata":{"session_id":"s"},"previous_response_id":"response-from-another-connection","input":[]}`
	if err := conn.Write(ctx, websocket.MessageText, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	if reply := gatewayWSRead(t, conn); !strings.Contains(string(reply), "resend full input") {
		t.Fatal("unknown prior policy was guessed")
	}
	if sent.Load() != 0 {
		t.Fatal("unknown-policy delta reached upstream")
	}
}

func TestCacheReviewCompletedPolicyBoundedAndPending(t *testing.T) {
	turns := gatewayWSTurns{pending: map[string][]*gatewayWSTurn{"lane": {{responseID: "in-progress", cachePolicy: gatewayWSCachePolicy{enabled: true}}}}}
	if enabled, ok := turns.cachePolicyForResponse("in-progress"); !ok || !enabled.enabled {
		t.Fatal("response.created policy unavailable")
	}
	for i := 0; i < maxGatewayWSCompletedResponses+1; i++ {
		turns.completed.add(fmt.Sprintf("done-%d", i), gatewayWSCachePolicy{enabled: i%2 == 0})
	}
	if _, ok := turns.cachePolicyForResponse("done-0"); ok {
		t.Fatal("policy survived identity eviction")
	}
	if enabled, ok := turns.cachePolicyForResponse("done-1"); !ok || enabled.enabled {
		t.Fatal("explicit false was treated as unknown")
	}
	if enabled, ok := turns.cachePolicyForResponse("done-2"); !ok || !enabled.enabled {
		t.Fatal("true policy was lost")
	}
}

func TestCacheReviewNearLimitRejectedRequestPreservesExistingBinding(t *testing.T) {
	p := newTestProxy(t, 1)
	s, _ := p.Settings.Get()
	s.BuiltinPromptEnabled = false
	if err := p.Settings.Set(s); err != nil {
		t.Fatal(err)
	}
	account, _ := p.Store.Get("test")
	want := gatewayAccountRef(account)
	if _, err := p.sessionAccount(t.Context(), "admin", "already-bound", want); err != nil {
		t.Fatal(err)
	}
	const head = `{"model":"fixture","instructions":"","stream":true,"store":false,"input":[],"prompt_cache_key":"s","client_metadata":{"session_id":"s"},"padding":"`
	raw := head + strings.Repeat("x", maxBody-32-len(head)-2) + `"}`
	r := proxyRequest(http.MethodPost, "/responses", raw)
	r.Header.Set("Session-Id", "already-bound")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	got, err := p.sessionAccount(t.Context(), "admin", "already-bound", "")
	if w.Code != 413 || err != nil || got != want {
		t.Fatal("rejection removed or modified an existing valid binding")
	}
}

func TestCacheReviewDisabledUnknownPriorPassesAcrossConnection(t *testing.T) {
	captured := make(chan []byte, 2)
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		_, raw, err := conn.Read(r.Context())
		if err != nil {
			t.Error(err)
			return
		}
		captured <- raw
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"cross-connection-prior","status":"completed","output":[]}}`))
	})
	enableGatewayWS(t, p, "")
	s, _ := p.Settings.Get()
	s.PromptCacheKeyEnabled = false
	if err := p.Settings.Set(s); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(p)
	defer server.Close()
	for i := 0; i < 2; i++ {
		conn, _ := gatewayWSDial(t, server.URL, "")
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		previous := ""
		if i == 1 {
			previous = `,"previous_response_id":"cross-connection-prior"`
		}
		raw := `{"type":"response.create","model":"fixture","prompt_cache_key":"s","client_metadata":{"session_id":"s"},"input":[]` + previous + `}`
		if err := conn.Write(ctx, websocket.MessageText, []byte(raw)); err != nil {
			t.Fatal(err)
		}
		if reply := gatewayWSRead(t, conn); !strings.Contains(string(reply), `"response.completed"`) {
			t.Fatalf("disabled rewrite rejected transparent continuation: %s", reply)
		}
		if got := <-captured; string(got) != raw {
			t.Fatal("disabled cross-connection payload was rewritten")
		}
		cancel()
		_ = conn.CloseNow()
	}
}

func TestCacheReviewExplicitKeyIsTrackedAfterPolicy(t *testing.T) {
	captured := make(chan []byte, 2)
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for i := 0; i < 2; i++ {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				t.Error(err)
				return
			}
			captured <- raw
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"explicit-key-%d","status":"completed","output":[]}}`, i)))
		}
	})
	enableGatewayWS(t, p, "")
	s, _ := p.Settings.Get()
	r := proxyRequest(http.MethodPost, "/responses", "")
	r.Header.Set("Session-Id", "explicit-session")
	base := `{"type":"response.create","model":"fixture","prompt_cache_key":"client-partition","input":[]}`
	policy, _, err := applyGatewayPolicy(r, []byte(base), "admin", s)
	if err != nil {
		t.Fatal(err)
	}
	if key := policyString(t, policy, "prompt_cache_key"); key != "client-partition" {
		t.Fatal("policy changed the caller's cache partition before account admission")
	}
	raw := strings.TrimSuffix(base, "}") + `,"client_metadata":{"session_id":"separate-session"}}`
	server := httptest.NewServer(p)
	defer server.Close()
	conn, _ := gatewayWSDial(t, server.URL, "explicit-session")
	defer conn.CloseNow()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	if reply := gatewayWSRead(t, conn); !strings.Contains(string(reply), `"response.completed"`) {
		t.Fatal("first explicit cache request failed")
	}
	first := <-captured
	if key := policyString(t, first, "prompt_cache_key"); len(key) != 36 || key == "client-partition" {
		t.Fatal("explicit cache partition was not isolated after account admission")
	}
	bad := `{"type":"response.create","model":"fixture","prompt_cache_key":"new-explicit-key","previous_response_id":"explicit-key-0","input":[]}`
	if err := conn.Write(ctx, websocket.MessageText, []byte(bad)); err != nil {
		t.Fatal(err)
	}
	if reply := gatewayWSRead(t, conn); !strings.Contains(string(reply), "resend full input") {
		t.Fatal("explicit account-scoped cache chain was not tracked")
	}
	second := strings.TrimSuffix(raw, "}") + `,"previous_response_id":"explicit-key-0"}`
	if err := conn.Write(ctx, websocket.MessageText, []byte(second)); err != nil {
		t.Fatal(err)
	}
	if reply := gatewayWSRead(t, conn); !strings.Contains(string(reply), `"response.completed"`) {
		t.Fatal("unchanged explicit cache continuation failed")
	}
	if key := policyString(t, <-captured, "prompt_cache_key"); key != policyString(t, first, "prompt_cache_key") {
		t.Fatal("explicit cache continuation changed key")
	}
}
