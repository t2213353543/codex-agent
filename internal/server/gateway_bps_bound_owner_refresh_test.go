package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBPSBoundOwnerPlaintextMigrationMatrix(t *testing.T) {
	for _, tc := range []struct {
		name, ownerChange, payload string
		stale, denyKey, denyNew    bool
		want                       int
	}{
		{"disabled_plaintext", "disabled", "plain", false, false, false, 200},
		{"deleted_plaintext", "deleted", "plain", false, false, false, 200},
		{"group_changed_plaintext", "group", "plain", false, false, false, 200},
		{"disabled_stale_snapshot_plaintext", "disabled", "plain", true, false, false, 200},
		{"deleted_stale_snapshot_plaintext", "deleted", "plain", true, false, false, 200},
		{"group_stale_snapshot_plaintext", "group", "plain", true, false, false, 200},
		{"disabled_opaque", "disabled", "opaque", false, false, false, 503},
		{"deleted_compaction", "deleted", "compact", false, false, false, 503},
		{"disabled_fileid", "disabled", "file", false, false, false, 503},
		{"disabled_incremental_plaintext", "disabled", "previous", false, false, false, 503},
		{"deleted_client_key_plaintext", "disabled", "plain", false, true, false, 401},
		{"no_authorized_new_owner_plaintext", "disabled", "plain", false, false, true, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var captured []byte
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "Bearer candidate-bound-owner" {
					t.Error("request used unavailable owner")
				}
				captured, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "bound-owner-result", "[]"))
			})
			if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Groups = []string{"allowed"}; a.Priority = 100; return nil }); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"access_token": "candidate-bound-owner", "expires_at": time.Now().Add(time.Hour).Unix()})
			if err := p.Store.ImportWith("replacement", raw, AccountDefaults{Concurrency: 1, Groups: []string{"allowed"}}); err != nil {
				t.Fatal(err)
			}
			key, err := p.Keys.Create("bound-owner-key", []string{"allowed"})
			if err != nil {
				t.Fatal(err)
			}
			old, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			owner := gatewayAccountRef(old)
			const session = "bound-owner-review"
			if got, err := p.sessionAccount(t.Context(), key.ID, session, owner); err != nil || got != owner {
				t.Fatal("initial binding")
			}
			plain := json.RawMessage(`[{"role":"user","content":"FIRST_SYNTHETIC"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"PRIOR_SYNTHETIC"}]},{"role":"user","content":"NEXT_SYNTHETIC"}]`)
			if tc.payload == "previous" {
				saved, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": plain})
				err := p.bpsSaveHistory(withGatewayUnboundedPayload(t.Context(), true), key.ID, "prior-bound", bpsHistoryRecord{Owner: owner, Session: session, Request: saved})
				if err != nil {
					t.Fatal(err)
				}
			}
			if tc.stale {
				if _, err := p.accountsSnapshot(); err != nil {
					t.Fatal(err)
				}
			}
			switch tc.ownerChange {
			case "disabled":
				err = p.Store.UpdateAccount("test", func(a *Account) error { a.Enabled = false; return nil })
			case "deleted":
				err = p.Store.Delete("test")
			case "group":
				err = p.Store.UpdateAccount("test", func(a *Account) error { a.Groups = []string{"forbidden"}; return nil })
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.denyKey {
				if err := p.Keys.Delete(key.ID); err != nil {
					t.Fatal(err)
				}
			}
			if tc.denyNew {
				if err := p.Store.UpdateAccount("replacement", func(a *Account) error { a.Groups = []string{"forbidden"}; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if !tc.stale {
				p.invalidateAccounts()
			}
			body := map[string]any{"model": "gpt-6-astra", "stream": false, "input": plain}
			switch tc.payload {
			case "opaque":
				body["input"] = json.RawMessage(`[{"type":"reasoning","encrypted_content":"synthetic-opaque"},{"role":"user","content":"NEXT_SYNTHETIC"}]`)
			case "compact":
				body["input"] = json.RawMessage(`[{"type":"compaction","encrypted_content":"synthetic-opaque"},{"role":"user","content":"NEXT_SYNTHETIC"}]`)
			case "file":
				body["input"] = json.RawMessage(`[{"role":"user","content":[{"type":"input_image","file_id":"file_synthetic"}]}]`)
			case "previous":
				body["input"] = json.RawMessage(`[{"role":"user","content":"NEXT_SYNTHETIC"}]`)
				body["previous_response_id"] = "prior-bound"
			}
			encoded, _ := json.Marshal(body)
			r := proxyRequest(http.MethodPost, "/v1/responses", string(encoded))
			r.Header.Set("Authorization", "Bearer "+key.Key)
			r.Header.Set("Session-Id", session)
			w := httptest.NewRecorder()
			p.gatewayBPS(w, r)
			gotOwner, err := p.sessionAccount(t.Context(), key.ID, session, "")
			if err != nil {
				t.Fatal(err)
			}
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d calls=%d", w.Code, tc.want, calls)
			}
			if tc.want == 200 {
				replacement, _ := p.Store.Get("replacement")
				if calls != 1 || gotOwner != gatewayAccountRef(replacement) || !bytes.Contains(captured, []byte("FIRST_SYNTHETIC")) || !bytes.Contains(captured, []byte("PRIOR_SYNTHETIC")) || !bytes.Contains(captured, []byte("NEXT_SYNTHETIC")) {
					t.Fatal("plaintext history/authenticated replacement/CAS lost")
				}
			} else if calls != 0 || gotOwner != owner {
				t.Fatalf("unsafe/unauthorized request dispatched or migrated: calls=%d changed=%t", calls, gotOwner != owner)
			}
			if p.inFlight("replacement") != 0 {
				t.Fatal("slot leak")
			}
			t.Logf("case=%s http=%d upstream_calls=%d migrated=%t original_plaintext_retained=%t", tc.name, w.Code, calls, gotOwner != owner, bytes.Contains(captured, []byte("PRIOR_SYNTHETIC")))
		})
	}
}

func TestBPSBoundOwnerAuthorizationChangedDuringRefresh(t *testing.T) {
	for _, mode := range []string{"plaintext_new_group_allowed", "opaque_new_group_allowed", "key_revoked"} {
		t.Run(mode, func(t *testing.T) {
			var p *Proxy
			var key APIKey
			refreshes, generations := 0, 0
			p = bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/token" {
					refreshes++
					if mode == "key_revoked" {
						if err := p.Keys.Delete(key.ID); err != nil {
							t.Error(err)
						}
					} else {
						if _, err := p.Keys.Update(key.ID, func(k *APIKey) error { k.Groups = []string{"new-group"}; return nil }); err != nil {
							t.Error(err)
						}
					}
					_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"refreshed-old-owner","refresh_token":"rotated-old-owner","expires_in":3600}`, "test-account", "fixture-user"))
					return
				}
				generations++
				if r.Header.Get("Authorization") != "Bearer fresh-group-owner" {
					t.Error("unauthorized old account dispatched")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "refresh-group-result", "[]"))
			})
			if err := p.Store.UpdateAccount("test", func(a *Account) error { a.ExpiresAt = 1; a.Groups = []string{"old-group"}; return nil }); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"access_token": "fresh-group-owner", "expires_at": time.Now().Add(time.Hour).Unix()})
			if err := p.Store.ImportWith("new-owner", raw, AccountDefaults{Concurrency: 1, Groups: []string{"new-group"}}); err != nil {
				t.Fatal(err)
			}
			var err error
			key, err = p.Keys.Create("group-change-key", []string{"old-group"})
			if err != nil {
				t.Fatal(err)
			}
			old, _ := p.Store.Get("test")
			owner := gatewayAccountRef(old)
			const session = "refresh-group-change"
			if _, err := p.sessionAccount(t.Context(), key.ID, session, owner); err != nil {
				t.Fatal(err)
			}
			input := `[{"role":"user","content":"Full plain synthetic history"}]`
			if mode == "opaque_new_group_allowed" {
				input = `[{"type":"reasoning","encrypted_content":"opaque-synthetic"},{"role":"user","content":"Full plain synthetic history"}]`
			}
			r := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","stream":false,"input":`+input+`}`)
			r.Header.Set("Authorization", "Bearer "+key.Key)
			r.Header.Set("Session-Id", session)
			w := httptest.NewRecorder()
			p.gatewayBPS(w, r)
			migrated, _ := p.sessionAccount(t.Context(), key.ID, session, "")
			want := 503
			wantGenerations := 0
			if mode == "plaintext_new_group_allowed" {
				want = 200
				wantGenerations = 1
			}
			t.Logf("mode=%s http=%d refreshes=%d generations=%d migrated=%t", mode, w.Code, refreshes, generations, migrated != owner)
			if w.Code != want || refreshes != 1 || generations != wantGenerations {
				t.Fatalf("post-refresh currently authorized replacement boundary status=%d want=%d generations=%d want=%d", w.Code, want, generations, wantGenerations)
			}
			if mode != "plaintext_new_group_allowed" && migrated != owner {
				t.Fatal("opaque or revoked key was migrated")
			}
			if p.inFlight("test") != 0 || p.inFlight("new-owner") != 0 {
				t.Fatal("admission slot leak")
			}
		})
	}
}
