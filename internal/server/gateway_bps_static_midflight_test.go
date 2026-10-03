package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestBPSStaticMidflightSettingsKeepSelectedModelAcrossAccountFailover(t *testing.T) {
	for _, status := range []int{403, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var posts, catalogs atomic.Int32
			var models, owners []string
			var saved atomic.Bool
			var p *Proxy
			p = bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					catalogs.Add(1)
					w.WriteHeader(503)
					return
				}
				var body map[string]json.RawMessage
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Error("invalid request")
					return
				}
				models = append(models, bpsGatewayString(body, "model"))
				if r.Header.Get("Authorization") == "Bearer test-upstream-token" {
					owners = append(owners, "first")
				} else {
					owners = append(owners, "second")
				}
				n := posts.Add(1)
				if n == 1 {
					settings, err := p.Settings.Get()
					if err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					settings.BPSModelRoutes = "source-x=gpt-6-astra\n*=gpt-6-astra"
					if err := p.Settings.Set(settings); err != nil {
						t.Error("midflight settings save failed: ", err)
						w.WriteHeader(500)
						return
					}
					observed, err := p.Settings.Get()
					if err != nil || observed.BPSModelRoutes != settings.BPSModelRoutes {
						t.Error("new rules were not persisted")
						w.WriteHeader(500)
						return
					}
					saved.Store(true)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					if status == 403 {
						_, _ = io.WriteString(w, bpsObservedPolicyEnvelope)
					} else {
						_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","code":"rateLimitExceeded","message":"synthetic rate limit"}}`)
					}
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_midflight_%d\",\"model\":%q,\"status\":\"completed\",\"output\":[]}}\n\n", n, bpsGatewayString(body, "model"))
			})
			settings, err := p.Settings.Get()
			if err != nil {
				t.Fatal(err)
			}
			settings.BPSModelRoutes = "source-x=gpt-6-sol\n*=gpt-6-astra"
			settings.RateLimitCooldownEnabled = false
			if err = p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			if err = p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; return nil }); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"access_token": "next-token", "expires_at": time.Now().Add(time.Hour).Unix()})
			if err = p.Store.ImportWith("next", raw, AccountDefaults{Concurrency: 10, Weight: 1}); err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
			request := func(session string) *httptest.ResponseRecorder {
				out := httptest.NewRecorder()
				r := proxyRequest("POST", "/v1/responses", `{"model":"source-x","stream":false,"input":[{"role":"user","content":"portable synthetic"}]}`)
				r.Header.Set("Session-Id", session)
				p.gatewayBPS(out, r)
				return out
			}
			first := request("midflight-first-logical")
			if !saved.Load() || first.Code != 200 || posts.Load() != 2 {
				t.Fatalf("fixture did not save and fail over successfully: saved=%t HTTP=%d POST=%d", saved.Load(), first.Code, posts.Load())
			}
			if models[0] != "gpt-6-sol" || models[1] != "gpt-6-sol" {
				t.Errorf("one logical request changed final model across account retry: models=%v owners=%v", models, owners)
			}
			second := request("midflight-new-logical")
			if second.Code != 200 || posts.Load() != 3 || models[2] != "gpt-6-astra" {
				t.Errorf("new independent request did not use new rules: HTTP=%d POST=%d models=%v", second.Code, posts.Load(), models)
			}
			latest, _ := p.Settings.Get()
			if latest.RateLimitCooldownEnabled || catalogs.Load() != 0 {
				t.Fatal("cooldown setting or zero-catalog contract changed")
			}
			t.Logf("saved_rules=true first_status=%d next_status=%d models=%v owners=%v catalogGET=%d", first.Code, second.Code, models, owners, catalogs.Load())
		})
	}
}
