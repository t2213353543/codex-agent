package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGatewayAttemptReviewHTTPRejectionsRemainUpstreamFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
	}{
		{"websocket_rejection", errNativeWSHandshake, 401},
		{"websocket_rate_limit", errors.New("synthetic upgrade rejection"), 429},
		{"oauth_rejection", &gatewayOAuthRefreshError{Kind: "http", StatusCode: 400}, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, observer := withGatewayAttemptObservation(t.Context())
			handle := observer.Begin("request", "/responses", time.Now())
			finishGatewayObservedAttempt(handle, &http.Response{StatusCode: test.status}, test.err)
			attempt := observer.Snapshot().Attempts[0]
			if attempt.StatusCode == nil || *attempt.StatusCode != test.status || attempt.ErrorSource != "upstream" || attempt.ErrorCode != "http_status" {
				t.Fatalf("actual HTTP rejection mislabeled: status=%v source=%q code=%q", attempt.StatusCode, attempt.ErrorSource, attempt.ErrorCode)
			}
		})
	}
}

func TestGatewayAttemptReviewValidJSONSchemaFallbackTruncation(t *testing.T) {
	for _, raw := range []string{
		`"` + strings.Repeat("x", 220) + `"`,
		`["` + strings.Repeat("x", 220) + `"]`,
		`{"error":"` + strings.Repeat("x", 220) + `"}`,
		`{"message":{"unexpected":"` + strings.Repeat("x", 220) + `"}}`,
	} {
		t.Run(fmt.Sprint(len(raw)), func(t *testing.T) {
			if !json.Valid([]byte(raw)) || !strings.HasSuffix(errorFromPayload([]byte(raw)), string(rune(0x2026))) {
				t.Fatal("fixture did not reach actual structured-parser fallback truncation")
			}
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400); _, _ = io.WriteString(w, raw) })
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest("POST", "/responses", `{"model":"fixture","stream":true}`))
			rec, ok := p.Metrics.LastFor("test")
			if !ok || w.Code != 400 || w.Body.String() != raw {
				t.Fatal("observation changed error bytes or record")
			}
			if rec.ErrorTruncated == nil || !*rec.ErrorTruncated {
				t.Fatal("actual parser fallback truncation was declared untruncated")
			}
		})
	}
}

func TestGatewayAttemptReviewPreExpiryFailureAndCachedRefresh(t *testing.T) {
	var oauth, inference atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			oauth.Add(1)
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		inference.Add(1)
		w.WriteHeader(500)
	})
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.ExpiresAt = time.Now().Add(-time.Hour).Unix(); return nil }); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	for request := range 2 {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, proxyRequest("POST", "/responses", `{"model":"inbound","stream":true}`))
		if w.Code != 503 {
			t.Fatal("pre-expiry failure changed selection status")
		}
		logs, err := p.Metrics.Logs(LogQuery{Size: 10})
		if err != nil || len(logs.Items) != request+1 {
			t.Fatal("selection failure usage cardinality changed")
		}
		rec := logs.Items[0]
		if rec.Model != "inbound" || rec.SentModel != "" || rec.UpstreamPath != "" {
			t.Fatal("failed pre-expiry selection invented inference send")
		}
		if request == 0 {
			if len(rec.Attempts) != 1 || rec.Attempts[0].Phase != "refresh" || rec.Attempts[0].StatusCode == nil || *rec.Attempts[0].StatusCode != 400 {
				t.Fatal("actual failed pre-expiry refresh missing")
			}
		} else if len(rec.Attempts) != 0 || rec.AttemptsTruncated != nil {
			t.Fatal("cached failed refresh invented another network attempt")
		}
	}
	if oauth.Load() != 1 || inference.Load() != 0 {
		t.Fatal("passive observations changed refresh cooldown or inference admission")
	}
}

func TestGatewayAttemptReviewWebSocket401RecoveryFirstTurnOnly(t *testing.T) {
	for _, headersIdentity := range []bool{false, true} {
		t.Run(fmt.Sprint(headersIdentity), func(t *testing.T) {
			var upgrades, refreshes, creates atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/token" {
					refreshes.Add(1)
					_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"synthetic-new","refresh_token":"synthetic-rt"}`, "test-account", "fixture-user"))
					return
				}
				if upgrades.Add(1) == 1 {
					w.WriteHeader(401)
					return
				}
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				for turn := range 2 {
					if _, _, err := conn.Read(r.Context()); err != nil {
						return
					}
					creates.Add(1)
					_ = conn.Write(r.Context(), websocket.MessageText, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"attempt-review-%d","status":"completed","model":"returned","output":[]}}`, turn)))
				}
				_, _, _ = conn.Read(r.Context())
			})
			enableGatewayWS(t, p, "")
			server := httptest.NewServer(p)
			defer server.Close()
			session := ""
			if headersIdentity {
				session = "review-session"
			}
			conn, _ := gatewayWSDial(t, server.URL, session)
			defer conn.CloseNow()
			for range 2 {
				if err := conn.Write(t.Context(), websocket.MessageText, []byte(`{"type":"response.create","model":"sent","prompt_cache_key":"review-session","input":[]}`)); err != nil {
					t.Fatal(err)
				}
				gatewayWSRead(t, conn)
				waitGatewayWSReleased(t, p)
			}
			rows := p.Metrics.RecentForAccount("test", 10)
			if len(rows) != 2 || upgrades.Load() != 2 || refreshes.Load() != 1 || creates.Load() != 2 {
				t.Fatal("WS lifecycle or accounting changed")
			}
			var first, second *RequestRecord
			for i := range rows {
				if rows[i].ResponseID == "attempt-review-0" {
					first = &rows[i]
				}
				if rows[i].ResponseID == "attempt-review-1" {
					second = &rows[i]
				}
			}
			if first == nil || second == nil || len(first.Attempts) != 3 || len(second.Attempts) != 0 || second.AttemptsTruncated != nil || second.UpstreamHeaderMs != nil {
				t.Fatal("handshake/refresh evidence missing or copied to later WS turn")
			}
			for i, want := range []struct {
				phase  string
				status int
			}{{"websocket", 401}, {"refresh", 200}, {"websocket", 101}} {
				got := first.Attempts[i]
				if got.Attempt != i+1 || got.Phase != want.phase || got.StatusCode == nil || *got.StatusCode != want.status || got.HeaderMs == nil || got.DurationMs == nil {
					t.Fatal("WS initial recovery trace not sourced from actual sends")
				}
			}
			if first.Attempts[0].ErrorSource != "upstream" || first.Attempts[0].ErrorCode != "http_status" {
				t.Fatal("rejected WS handshake mislabeled transport")
			}
		})
	}
}

func TestGatewayAttemptReviewRefreshBodyTimingAndCancellation(t *testing.T) {
	ctx, observer := withGatewayAttemptObservation(t.Context())
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		// Cancellation after the exchange starts must not discard rotated tokens.
		// Cancellation before dispatch is covered by the refresh preflight tests.
		cancel()
		time.Sleep(60 * time.Millisecond)
		_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"synthetic-new","refresh_token":"synthetic-rotated"}`, "test-account", "fixture-user"))
	})
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := refreshAccountWithSettings(ctx, p.Client, p.TokenURL, account, defaultSettings())
	if err != nil || updated.RefreshToken != "synthetic-rotated" {
		t.Fatal("passive observation changed rotating refresh's detached lifetime")
	}
	snapshot := observer.Snapshot()
	if len(snapshot.Attempts) != 1 {
		t.Fatal("actual refresh not recorded")
	}
	a := snapshot.Attempts[0]
	if a.StatusCode == nil || *a.StatusCode != 200 || a.HeaderMs == nil || a.DurationMs == nil || *a.DurationMs-*a.HeaderMs < 50 || a.ErrorCode != "" {
		t.Fatal("refresh headers delayed to body completion or cancellation fabricated failure")
	}
}

func TestGatewayAttemptReviewPersistedErrorTruncationAfterRedaction(t *testing.T) {
	for _, test := range []struct {
		name, message string
		truncated     bool
	}{
		{"long_plain_message", strings.Repeat("x", 3000), true},
		{"redaction_removes_long_secret", "Bearer " + strings.Repeat("x", 3000), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"error": map[string]string{"message": test.message}})
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400); _, _ = w.Write(raw) })
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest("POST", "/responses", `{"model":"fixture","stream":true}`))
			rec, ok := p.Metrics.LastFor("test")
			if !ok || w.Code != 400 || w.Body.String() != string(raw) {
				t.Fatal("passive metadata changed upstream error bytes")
			}
			if rec.ErrorTruncated == nil || *rec.ErrorTruncated != test.truncated {
				t.Fatal("stored diagnostic truncation flag does not describe redacted persistence boundary")
			}
			if rec.Error != redactMetricError(test.message) {
				t.Fatal("observer changed existing redaction/truncation result")
			}
		})
	}
}

func TestGatewayAttemptReviewOAuthInvalidBodyIsUpstreamProtocol(t *testing.T) {
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `invalid-json`) })
	a, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, observer := withGatewayAttemptObservation(t.Context())
	if _, err := refreshAccountWithSettings(ctx, p.Client, p.TokenURL, a, defaultSettings()); err == nil {
		t.Fatal("invalid OAuth body accepted")
	}
	attempts := observer.Snapshot().Attempts
	if len(attempts) != 1 || attempts[0].StatusCode == nil || *attempts[0].StatusCode != 200 || attempts[0].ErrorSource != "upstream" || attempts[0].ErrorCode != "protocol" {
		t.Fatal("received invalid OAuth body was labeled transport fault")
	}
}

func TestGatewayAttemptReviewModel304IsNotAttemptError(t *testing.T) {
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"fixture"`)
		w.WriteHeader(http.StatusNotModified)
	})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest(http.MethodGet, "/models?client_version=0.155.0", ""))
	rec, ok := p.Metrics.LastFor("test")
	if !ok || w.Code != 304 || w.Body.Len() != 0 || rec.Status != "ok" || len(rec.Attempts) != 1 || rec.Attempts[0].StatusCode == nil || *rec.Attempts[0].StatusCode != 304 {
		t.Fatal("models304 fixture behavior changed")
	}
	if rec.Attempts[0].ErrorSource != "" || rec.Attempts[0].ErrorCode != "" {
		t.Fatal("successful model validator response mislabeled failed attempt")
	}
}
