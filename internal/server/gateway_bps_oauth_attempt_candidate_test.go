package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestBPSOAuthRefreshAttemptSurvivesLocalSelectionFailure(t *testing.T) {
	var oauth, generation atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/oauth/token" {
			oauth.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		generation.Add(1)
		w.WriteHeader(500)
	})
	account := expiredCooldownAccount(t, p)
	const session = "synthetic-refresh-then-local-failure"
	if _, err := p.sessionAccount(t.Context(), "admin", session, gatewayAccountRef(account)); err != nil {
		t.Fatal(err)
	}
	for iteration := range 2 {
		r := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","stream":true,"input":[{"type":"reasoning","encrypted_content":"synthetic-cipher"}]}`)
		r.Header.Set("Session-Id", session)
		w := httptest.NewRecorder()
		p.gatewayBPS(w, r)
		p.Metrics.mu.Lock()
		record := p.Metrics.recent[p.Metrics.recentAt-1]
		p.Metrics.mu.Unlock()
		if w.Code != 503 || oauth.Load() != 1 || generation.Load() != 0 {
			t.Fatalf("unexpected boundary status=%d oauth=%d generation=%d", w.Code, oauth.Load(), generation.Load())
		}
		if iteration == 0 {
			if len(record.Attempts) != 1 || record.Attempts[0].Phase != "refresh" || record.Attempts[0].StatusCode == nil || *record.Attempts[0].StatusCode != 400 {
				t.Errorf("real OAuth attempt was lost/mislabeled: %+v", record.Attempts)
			}
			if len(record.Attempts) != 1 || record.Attempts[0].ObservedHTTPStatusCode == nil || *record.Attempts[0].ObservedHTTPStatusCode != 400 {
				t.Errorf("real OAuth HTTP400 header observation missing: %+v", record.Attempts)
			}
		} else if len(record.Attempts) != 0 {
			t.Errorf("cached OAuth failure invented new network attempt: %+v", record.Attempts)
		}
		t.Logf("iteration=%d total_oauth=%d generation=%d attempts=%+v record_source=%s", iteration, oauth.Load(), generation.Load(), record.Attempts, record.ErrorSource)
	}
}
