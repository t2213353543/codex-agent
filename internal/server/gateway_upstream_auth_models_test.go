package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestBPSModelsUpstreamAuthenticationIsNotClientAuthentication(t *testing.T) {
	for _, invalidClient := range []bool{false, true} {
		name := "valid_client"
		if invalidClient {
			name = "invalid_client"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.Path != "/responses/access" {
					t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","code":"unauthorized","message":"SYNTHETIC_OAUTH_REVOKED"}}`)
			})
			settings, _ := p.Settings.Get()
			settings.Upstream401RecoveryEnabled = false
			settings.AutomaticTokenRefreshEnabled = false
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; return nil }); err != nil {
				t.Fatal(err)
			}
			credential, _ := json.Marshal(map[string]any{"access_token": "synthetic-second-token", "expires_at": time.Now().Add(time.Hour).Unix()})
			if err := p.Store.ImportWith("second", credential, AccountDefaults{Concurrency: 2, Priority: 1, Weight: 1}); err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
			r := proxyRequest(http.MethodGet, "/v1/models", "")
			if invalidClient {
				r.Header.Set("Authorization", "Bearer synthetic-invalid-client")
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			rec, recorded := p.Metrics.LastFor("test")
			t.Logf("MODEL_STATUS invalid_client=%t public_http=%d upstream_gets=%d recorded=%t internal_http=%d source=%s phase=%s attempts=%d", invalidClient, w.Code, calls.Load(), recorded, rec.HTTPCode, rec.ErrorSource, rec.ErrorPhase, len(rec.Attempts))
			if invalidClient {
				if w.Code != 401 || calls.Load() != 0 {
					t.Fatal("real invalid client key semantics changed")
				}
				return
			}
			if calls.Load() != 1 || !recorded || rec.ErrorSource != "upstream" || rec.HTTPCode != 503 {
				t.Fatal("fixture did not reach catalog upstream-auth boundary")
			}
			if len(rec.Attempts) != 1 || rec.Attempts[0].StatusCode == nil || *rec.Attempts[0].StatusCode != 401 || rec.Attempts[0].ObservedHTTPStatusCode == nil || *rec.Attempts[0].ObservedHTTPStatusCode != 401 {
				t.Fatal("catalog actual HTTP401 lost")
			}
			if w.Header().Get("WWW-Authenticate") != "" {
				t.Fatal("catalog forwarded upstream auth challenge")
			}
			if w.Code != 503 {
				t.Errorf("RED: valid client key received upstream catalog HTTP %d instead of dependency failure503", w.Code)
			}
		})
	}
}
