package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestBPSOAuthActualAttemptOrder(t *testing.T) {
	for _, mode := range []string{"preexpiry_success", "same_owner_401_recovery", "outer_retry", "prior_error_then_refresh_exhausted", "truncate_four"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			var sent []string
			generation := 0
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				mu.Lock()
				defer mu.Unlock()
				if r.URL.Path == "/oauth/token" {
					status := 200
					if mode == "prior_error_then_refresh_exhausted" {
						status = 400
					}
					sent = append(sent, fmt.Sprintf("refresh:%d", status))
					w.WriteHeader(status)
					if status == 400 {
						_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
						return
					}
					_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"synthetic-rotated","refresh_token":"synthetic-next-refresh","expires_in":3600}`, "test-account", "fixture-user"))
					return
				}
				generation++
				status := 200
				if mode == "same_owner_401_recovery" && generation == 1 || mode == "outer_retry" && generation == 1 || mode == "prior_error_then_refresh_exhausted" || mode == "truncate_four" {
					status = 401
				}
				sent = append(sent, fmt.Sprintf("request:%d", status))
				w.WriteHeader(status)
				if status != 200 {
					_, _ = io.WriteString(w, `{"error":{"code":"unauthorized","message":"original upstream unauthorized"}}`)
					return
				}
				_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "oauth_order_done", "[]"))
			})
			settings, _ := p.Settings.Get()
			settings.AutomaticTokenRefreshEnabled = true
			settings.Upstream401RecoveryEnabled = mode == "same_owner_401_recovery"
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			if err := p.Store.UpdateAccount("test", func(a *Account) error {
				a.Priority = 100
				if mode == "preexpiry_success" {
					a.ExpiresAt = time.Now().Add(-time.Hour).Unix()
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			count := 0
			if mode == "outer_retry" || mode == "prior_error_then_refresh_exhausted" {
				count = 1
			}
			if mode == "truncate_four" {
				count = 4
			}
			for i := 0; i < count; i++ {
				expires := time.Now().Add(time.Hour).Unix()
				if mode == "prior_error_then_refresh_exhausted" {
					expires = time.Now().Add(-time.Hour).Unix()
				}
				credentials, _ := json.Marshal(map[string]any{"access_token": fmt.Sprintf("candidate-%d", i), "refresh_token": "synthetic-refresh", "expires_at": expires})
				if err := p.Store.ImportWith(fmt.Sprintf("second-%d", i), credentials, AccountDefaults{Concurrency: 1, Priority: 50 - i}); err != nil {
					t.Fatal(err)
				}
			}
			p.invalidateAccounts()
			r := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","stream":false,"input":"synthetic plaintext"}`)
			r.Header.Set("Session-Id", "oauth-order-"+mode)
			w := httptest.NewRecorder()
			p.gatewayBPS(w, r)
			p.Metrics.mu.Lock()
			if p.Metrics.recentAt != 1 {
				p.Metrics.mu.Unlock()
				t.Fatal("must retain one request record")
			}
			record := p.Metrics.recent[0]
			p.Metrics.mu.Unlock()
			mu.Lock()
			actual := append([]string(nil), sent...)
			mu.Unlock()
			var observed []string
			for _, a := range record.Attempts {
				status := 0
				if a.StatusCode != nil {
					status = *a.StatusCode
				}
				observed = append(observed, fmt.Sprintf("%s:%d", a.Phase, status))
				if a.Attempt != len(observed) || a.HeaderMs == nil || a.DurationMs == nil {
					t.Errorf("actual observed timing/order missing: %+v", a)
				}
				if a.ObservedHTTPStatusCode == nil || *a.ObservedHTTPStatusCode != status {
					t.Errorf("actual HTTP header status missing: %+v", a)
				}
			}
			want := map[string][]string{"preexpiry_success": {"refresh:200", "request:200"}, "same_owner_401_recovery": {"request:401", "refresh:200", "request:200"}, "outer_retry": {"request:401", "request:200"}, "prior_error_then_refresh_exhausted": {"request:401", "refresh:400"}, "truncate_four": {"request:401", "request:401", "request:401", "request:401", "request:401"}}[mode]
			if !reflect.DeepEqual(actual, want) {
				t.Fatalf("dispatch contract changed: actual=%v want=%v status=%d", actual, want, w.Code)
			}
			retained := want
			if len(retained) > 4 {
				retained = retained[:4]
			}
			if !reflect.DeepEqual(observed, retained) {
				t.Errorf("actual network order lost/doubled: actual=%v recorded=%v", actual, observed)
			}
			if len(want) > 4 && (record.AttemptsTruncated == nil || !*record.AttemptsTruncated) {
				t.Error("real truncated attempts not flagged")
			}
			if len(want) <= 4 && record.AttemptsTruncated != nil && *record.AttemptsTruncated {
				t.Error("unexpected truncation")
			}
			wantHTTP := 200
			if mode == "prior_error_then_refresh_exhausted" || mode == "truncate_four" {
				wantHTTP = http.StatusServiceUnavailable
			}
			if w.Code != wantHTTP {
				t.Errorf("prior/result response changed: status=%d want=%d body=%s", w.Code, wantHTTP, w.Body.String())
			}
			t.Logf("mode=%s actual=%v observed=%v http=%d records=1", mode, actual, observed, w.Code)
		})
	}
}
