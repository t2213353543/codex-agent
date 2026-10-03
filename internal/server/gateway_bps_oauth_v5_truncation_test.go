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

// A real single handler can overflow the observer before its final request:
// failed preflight refreshes for two candidates, successful refresh for a third,
// 401 response, another refresh and the successful retry. The fourth retained
// entry is not the final generation and must not inherit the final 200.
func TestBPSOAuthTruncatedSnapshotDoesNotRelabelEarlierGeneration(t *testing.T) {
	var mu sync.Mutex
	var actual []string
	goodRefresh, generation := 0, 0
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/oauth/token" {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["refresh_token"] != "good-refresh" && body["refresh_token"] != "good-next-refresh" {
				actual = append(actual, "refresh:400")
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
				return
			}
			goodRefresh++
			actual = append(actual, "refresh:200")
			payload := fmt.Sprintf(`{"access_token":"good-access-%d","refresh_token":"good-next-refresh","expires_in":3600}`, goodRefresh)
			_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(payload, "good-workspace", "good-user"))
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		generation++
		if generation == 1 {
			actual = append(actual, "request:401")
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":{"code":"unauthorized"}}`)
			return
		}
		actual = append(actual, "request:200")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "real-final-success", "[]"))
	})
	settings, _ := p.Settings.Get()
	settings.AutomaticTokenRefreshEnabled = true
	settings.Upstream401RecoveryEnabled = true
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 300; a.ExpiresAt = time.Now().Add(-time.Hour).Unix(); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		name, token, refresh, workspace, user string
		priority                              int
	}{
		{"second-bad", "bad-second", "bad-second-refresh", "bad-workspace", "bad-user", 200},
		{"good", "good-initial", "good-refresh", "good-workspace", "good-user", 100},
	} {
		payload, _ := json.Marshal(map[string]any{"access_token": entry.token, "refresh_token": entry.refresh, "chatgpt_account_id": entry.workspace, "expires_at": time.Now().Add(-time.Hour).Unix()})
		if err := p.Store.ImportWith(entry.name, payload, AccountDefaults{Concurrency: 1, Priority: entry.priority}); err != nil {
			t.Fatal(err)
		}
		if err := p.Store.UpdateAccount(entry.name, func(a *Account) error {
			a.Identity = &managedAccountIdentity{UserID: entry.user, WorkspaceID: entry.workspace}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	p.invalidateAccounts()
	w := httptest.NewRecorder()
	r := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","stream":false,"input":"synthetic bounded truncation"}`)
	r.Header.Set("Session-Id", "independent-v4-truncation")
	p.gatewayBPS(w, r)
	mu.Lock()
	want := []string{"refresh:400", "refresh:400", "refresh:200", "request:401", "refresh:200", "request:200"}
	observed := append([]string(nil), actual...)
	mu.Unlock()
	if w.Code != 200 || !reflect.DeepEqual(observed, want) {
		t.Fatalf("fixture did not reach intended real path: status=%d actual=%v", w.Code, observed)
	}
	record, ok := p.Metrics.LastFor("good")
	if !ok || record.Status != "ok" || len(record.Attempts) != 4 || record.AttemptsTruncated == nil || !*record.AttemptsTruncated {
		t.Fatalf("final record/truncation missing: %+v", record)
	}
	if record.FinalUpstreamErrorCode != "" || record.FinalUpstreamErrorSource != "" {
		t.Fatal("successful retry inherited final failure")
	}
	last := record.Attempts[3]
	if last.Phase != "request" || last.ObservedHTTPStatusCode == nil || *last.ObservedHTTPStatusCode != 401 {
		t.Fatalf("fourth actual response lost: %+v", last)
	}
	if last.StatusCode == nil || *last.StatusCode != 401 || last.ErrorCode != "http_status" || last.ErrorSource != "upstream" {
		var logical any
		if last.StatusCode != nil {
			logical = *last.StatusCode
		}
		t.Fatalf("truncated fourth entry incorrectly inherited hidden final generation: actual=%v logical=%v observed_http=%d code=%q source=%q", observed, logical, *last.ObservedHTTPStatusCode, last.ErrorCode, last.ErrorSource)
	}
}
