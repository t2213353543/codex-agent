package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// This is a synthetic ownership oracle, not a claim to implement or crack
// provider encryption. The fixture returns opaque A bytes on account A and
// rejects those exact bytes only when the gateway sends them to account B.
func TestBPSOpaqueKnownOutputMustNotChangeOwner(t *testing.T) {
	for _, mode := range []string{
		"explicit_alive", "content_stable", "explicit_tools_changed",
		"expired_explicit", "expired_content", "content_tools_changed",
		"content_instructions_changed", "cache_key_changed_no_header",
		"expired_previous_response",
	} {
		t.Run(mode, func(t *testing.T) {
			var owners []string
			var received [][]byte
			const cipher = "synthetic-owner-A-opaque-state"
			firstOutput := json.RawMessage(`[{"type":"reasoning","id":"rs_owned_A","summary":[],"encrypted_content":"` + cipher + `"},{"type":"message","id":"msg_owned_A","role":"assistant","content":[{"type":"output_text","text":"FIRST_OK"}]}]`)
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				received = append(received, bytes.Clone(body))
				owner := r.Header.Get("Authorization")
				owners = append(owners, owner)
				w.Header().Set("Content-Type", "text/event-stream")
				if len(owners) == 1 {
					if owner != "Bearer test-upstream-token" {
						t.Error("first output must be produced by owner A")
					}
					_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "opaque_route_first", string(firstOutput)))
					return
				}
				if !bytes.Contains(body, []byte(cipher)) {
					t.Error("opaque history was dropped or replaced")
				}
				if owner == "Bearer owner-B-token" {
					_, _ = io.WriteString(w, "data: "+`{"type":"response.failed","response":{"id":"opaque_wrong_owner","status":"failed","output":[],"error":{"code":"invalid_encrypted_content","type":"invalid_request_error","message":"Synthetic owner oracle rejected account A state on B"}}}`+"\n\n")
					return
				}
				_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "opaque_route_second", "[]"))
			})
			settings, err := p.Settings.Get()
			if err != nil {
				t.Fatal(err)
			}
			settings.BuiltinPromptEnabled = false
			settings.StickySessionsEnabled = true
			settings.PromptCacheKeyEnabled = true
			settings.BPSPromptCacheScope = bpsPromptCacheScopeAPIKey
			settings.Upstream401RecoveryEnabled = false
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; return nil }); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"access_token": "owner-B-token", "account_id": "workspace-B", "expires_at": time.Now().Add(time.Hour).Unix()})
			if err := p.Store.ImportWith("owner-B", raw, AccountDefaults{Concurrency: 2, Priority: 0, Weight: 1}); err != nil {
				t.Fatal(err)
			}
			if err := p.Store.UpdateAccount("owner-B", func(a *Account) error {
				a.Identity = &managedAccountIdentity{WorkspaceID: a.AccountID, UserID: "user-B"}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
			tools := json.RawMessage(`[{"type":"function","name":"synthetic_tool","description":"v1","parameters":{"type":"object","properties":{},"additionalProperties":false}}]`)
			body := map[string]any{"model": "gpt-6-astra", "stream": false, "input": []any{map[string]string{"role": "user", "content": "FIRST_SYNTHETIC_USER"}}, "instructions": "unchanged instructions", "tools": tools, "tool_choice": "none"}
			explicit := strings.HasPrefix(mode, "explicit_") || mode == "expired_explicit" || mode == "expired_previous_response"
			if mode == "cache_key_changed_no_header" {
				body["prompt_cache_key"] = "client-cache-A"
			}
			send := func(body map[string]any) *httptest.ResponseRecorder {
				encoded, _ := json.Marshal(body)
				r := proxyRequest(http.MethodPost, "/v1/responses", string(encoded))
				if explicit {
					r.Header.Set("Session-Id", "opaque-explicit-session")
				}
				w := httptest.NewRecorder()
				p.gatewayBPS(w, r)
				return w
			}
			first := send(body)
			if first.Code != 200 || len(owners) != 1 {
				t.Fatalf("initial generation failed HTTP%d", first.Code)
			}
			history, err := p.bpsLoadHistory(withGatewayUnboundedPayload(t.Context(), true), "admin", "opaque_route_first")
			if err != nil || history.Owner == "" || history.Session == "" || history.Expires <= time.Now().Unix() {
				t.Fatal("first actual output history/owner was not retained with bounded expiry")
			}
			var returned struct{ Output []json.RawMessage }
			if json.Unmarshal(first.Body.Bytes(), &returned) != nil || len(returned.Output) != 2 {
				t.Fatal("actual returned output unavailable")
			}
			input := []json.RawMessage{json.RawMessage(`{"role":"user","content":"FIRST_SYNTHETIC_USER"}`)}
			input = append(input, returned.Output...)
			input = append(input, json.RawMessage(`{"role":"user","content":"SECOND_SYNTHETIC_USER"}`))
			body["input"] = input
			if mode == "expired_previous_response" {
				body["input"] = []json.RawMessage{json.RawMessage(`{"role":"user","content":"SECOND_SYNTHETIC_USER"}`)}
				body["previous_response_id"] = "opaque_route_first"
			}
			if strings.HasPrefix(mode, "expired_") {
				key := auditSessionMapKey("admin", history.Session)
				p.sessionMu.Lock()
				route, ok := p.sessionRoutes[key]
				if ok {
					route.Expires = time.Now().Add(-time.Second)
					p.sessionRoutes[key] = route
				}
				p.sessionMu.Unlock()
				if !ok {
					t.Fatal("fixture cannot target the actual established binding")
				}
			}
			switch mode {
			case "content_tools_changed", "explicit_tools_changed":
				body["tools"] = bytes.ReplaceAll(tools, []byte("v1"), []byte("v2"))
				body["tools"] = json.RawMessage(body["tools"].([]byte))
			case "content_instructions_changed":
				body["instructions"] = "instructions with a harmless additional formatting preference"
			case "cache_key_changed_no_header":
				body["prompt_cache_key"] = "client-cache-B"
			}
			if err := p.Store.UpdateAccount("owner-B", func(a *Account) error { a.Priority = 200; return nil }); err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
			second := send(body)
			crossed := len(owners) == 2 && owners[1] == "Bearer owner-B-token"
			recordName := "test"
			if crossed {
				recordName = "owner-B"
			}
			record, _ := p.Metrics.LastFor(recordName)
			hasCipher := len(received) == 2 && bytes.Contains(received[1], []byte(cipher))
			// Compare every actual prior response item, not only the cipher leaf.
			var sent struct{ Input []json.RawMessage }
			if len(received) == 2 && json.Unmarshal(received[1], &sent) == nil {
				for _, prior := range returned.Output {
					matched := false
					for _, item := range sent.Input {
						matched = matched || bytes.Equal(item, prior)
					}
					if !matched {
						t.Fatal("actual completed output item was rewritten in the continuation")
					}
				}
			}
			// Retained history is still readable even after binding expiry.
			stillSaved, saveErr := p.bpsLoadHistory(withGatewayUnboundedPayload(t.Context(), true), "admin", "opaque_route_first")
			t.Logf("mode=%s HTTP=%d actual_calls=%d changed_owner=%t ciphertext_preserved=%t saved_history_alive=%t terminal_error=%t", mode, second.Code, len(owners), crossed, hasCipher, saveErr == nil && stillSaved.Owner == history.Owner, strings.Contains(record.Error, "invalid_encrypted_content"))
			if crossed {
				t.Fatalf("known account A output reached account B: mode=%s; routing-only synthetic reproduction, no real provider", mode)
			}
			if second.Code != 200 || len(owners) != 2 || owners[1] != "Bearer test-upstream-token" || !hasCipher {
				t.Fatalf("same-owner continuation failed: HTTP%d calls=%v", second.Code, owners)
			}
		})
	}
}

func TestBPSOpaqueMissingOrReplacedAfterOwnerMutation(t *testing.T) {
	for _, mutation := range []string{"same_owner_token_rotation", "different_user", "identity_removed", "deleted_recreated_same_user"} {
		t.Run(mutation, func(t *testing.T) {
			calls := 0
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "mutation_response", "[]"))
			})
			before, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			const session = "mutation-opaque-session"
			ctx := withGatewayUnboundedPayload(t.Context(), true)
			output := []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"synthetic-opaque","summary":[]}`)}
			if err := p.bpsSaveHistoryWithOpaqueOwners(ctx, "admin", "mutation-prior", bpsHistoryRecord{Owner: gatewayAccountRef(before), Session: "mutation-opaque-session", Request: json.RawMessage(`{"input":[]}`), Output: output}, output); err != nil {
				t.Fatal(err)
			}
			if bound, err := p.sessionAccount(t.Context(), "admin", session, gatewayAccountRef(before)); err != nil || bound != gatewayAccountRef(before) {
				t.Fatal("cannot establish known owner")
			}
			switch mutation {
			case "same_owner_token_rotation":
				err = p.Store.UpdateAccount("test", func(a *Account) error {
					a.AccessToken = "new-same-owner-token"
					a.RefreshToken = "new-same-owner-refresh"
					return nil
				})
			case "different_user":
				err = p.Store.UpdateAccount("test", func(a *Account) error {
					a.Identity = &managedAccountIdentity{WorkspaceID: a.AccountID, UserID: "different-user"}
					return nil
				})
			case "identity_removed":
				err = p.Store.UpdateAccount("test", func(a *Account) error { a.Identity = nil; return nil })
			case "deleted_recreated_same_user":
				err = p.Store.Delete("test")
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(map[string]any{"access_token": "new-token", "account_id": before.AccountID, "expires_at": time.Now().Add(time.Hour).Unix()})
				err = p.Store.ImportWith("test", raw, AccountDefaults{Concurrency: 2})
				if err == nil {
					err = p.Store.UpdateAccount("test", func(a *Account) error {
						a.Identity = &managedAccountIdentity{WorkspaceID: before.AccountID, UserID: before.Identity.UserID}
						return nil
					})
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
			current, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			r := proxyRequest("POST", "/v1/responses", `{"model":"gpt-6-astra","stream":false,"input":[{"type":"reasoning","encrypted_content":"synthetic-opaque"},{"role":"user","content":"next"}]}`)
			r.Header.Set("Session-Id", session)
			out := httptest.NewRecorder()
			p.gatewayBPS(out, r)
			rec, _ := p.Metrics.LastFor("test")
			// Early bound failure has no chosen account, so read the logical record.
			if rec.BoundSelection == nil {
				p.Metrics.mu.Lock()
				if p.Metrics.recentAt > 0 {
					rec = p.Metrics.recent[p.Metrics.recentAt-1]
				}
				p.Metrics.mu.Unlock()
			}
			state, outcome := "unreported", "unreported"
			if rec.BoundSelection != nil {
				state, outcome = rec.BoundSelection.BoundSnapshotState, rec.BoundSelection.RecoveryOutcome
			}
			same := gatewayAccountRef(before) == gatewayAccountRef(current)
			t.Logf("mutation=%s same_owner_ref=%t HTTP=%d calls=%d bound_state=%s recovery=%s", mutation, same, out.Code, calls, state, outcome)
			if mutation == "same_owner_token_rotation" {
				if !same || out.Code != 200 || calls != 1 {
					t.Fatal("known same-owner token rotation broke continuity")
				}
			} else if same || calls != 0 || out.Code != 503 || state != "missing_or_replaced" || outcome != "blocked_input" {
				t.Fatal(fmt.Sprintf("mutation did not hit conservative missing-owner boundary: %s", mutation))
			}
		})
	}
}
