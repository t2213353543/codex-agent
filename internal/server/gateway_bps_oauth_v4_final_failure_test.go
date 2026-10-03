package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestBPSOAuthSelectedGenerationFailure(t *testing.T) {
	rows := map[string]RequestRecord{}
	for _, mode := range []string{"http429", "sse429", "prior429_refresh400", "refresh_only", "retry_success", "truncated_final429"} {
		t.Run(mode, func(t *testing.T) {
			gets, refreshes, generations := 0, 0, 0
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.URL.Path == "/responses/access" {
					gets++
					t.Error("unexpected catalog")
				}
				if r.URL.Path == "/oauth/token" {
					refreshes++
					w.WriteHeader(400)
					_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
					return
				}
				generations++
				if mode == "sse429" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+`{"type":"response.failed","response":{"id":"sse429","status":"failed","output":[],"error":{"code":"rate_limit_exceeded","message":"synthetic quota"},"usage":{"input_tokens":37,"output_tokens":0}}}`+"\n\n")
					return
				}
				if mode == "retry_success" && generations == 2 {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "success", "[]"))
					return
				}
				status := 429
				if mode == "truncated_final429" && generations < 5 {
					status = 401
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"message":"synthetic upstream rejection"}}`)
			})
			settings, _ := p.Settings.Get()
			settings.Upstream401RecoveryEnabled = false
			settings.AutomaticTokenRefreshEnabled = true
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			if err := p.Store.UpdateAccount("test", func(a *Account) error {
				a.Priority = 100
				if mode == "refresh_only" {
					a.ExpiresAt = time.Now().Add(-time.Hour).Unix()
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			additional := 0
			if mode == "prior429_refresh400" || mode == "retry_success" {
				additional = 1
			}
			if mode == "truncated_final429" {
				additional = 4
			}
			for i := 0; i < additional; i++ {
				expires := time.Now().Add(time.Hour).Unix()
				if mode == "prior429_refresh400" {
					expires = time.Now().Add(-time.Hour).Unix()
				}
				credentials, _ := json.Marshal(map[string]any{"access_token": fmt.Sprintf("synthetic-%d", i), "refresh_token": "synthetic-refresh", "expires_at": expires})
				if err := p.Store.ImportWith(fmt.Sprintf("next-%d", i), credentials, AccountDefaults{Concurrency: 1, Priority: 50 - i}); err != nil {
					t.Fatal(err)
				}
			}
			p.invalidateAccounts()
			body := `{"model":"gpt-6-astra","stream":true,"input":"synthetic plaintext"}`
			if mode == "sse429" || mode == "http429" || mode == "refresh_only" {
				account, _ := p.Store.Get("test")
				if _, err := p.sessionAccount(t.Context(), "admin", "fixed-"+mode, gatewayAccountRef(account)); err != nil {
					t.Fatal(err)
				}
				body = `{"model":"gpt-6-astra","stream":true,"input":[{"type":"reasoning","encrypted_content":"synthetic-cipher"}]}`
			}
			r := proxyRequest(http.MethodPost, "/v1/responses", body)
			r.Header.Set("Session-Id", "fixed-"+mode)
			w := httptest.NewRecorder()
			p.gatewayBPS(w, r)
			p.Metrics.mu.Lock()
			count := p.Metrics.recentAt
			record := p.Metrics.recent[count-1]
			p.Metrics.mu.Unlock()
			if count != 1 || record.ObservationSchemaVersion != 2 {
				t.Fatalf("wrong record/schema: records=%d schema=%d", count, record.ObservationSchemaVersion)
			}
			wantCode := "rateLimitExceeded"
			wantHTTP, wantGeneration, wantRefresh, wantEntries := 429, 1, 0, 1
			switch mode {
			case "sse429":
				wantHTTP = 200
			case "prior429_refresh400":
				wantRefresh, wantEntries = 1, 2
			case "refresh_only":
				wantCode = ""
				wantHTTP, wantGeneration, wantRefresh = 503, 0, 1
			case "retry_success":
				wantCode = ""
				wantHTTP, wantGeneration, wantEntries = 200, 2, 2
			case "truncated_final429":
				wantGeneration, wantEntries = 5, 4
			}
			if w.Code != wantHTTP || generations != wantGeneration || refreshes != wantRefresh || len(record.Attempts) != wantEntries || gets != 0 {
				t.Fatalf("dispatch changed: http=%d generations=%d refresh=%d entries=%d", w.Code, generations, refreshes, len(record.Attempts))
			}
			if record.FinalUpstreamErrorCode != wantCode || (wantCode != "" && record.FinalUpstreamErrorSource != "upstream") || (wantCode == "" && record.FinalUpstreamErrorSource != "") {
				t.Fatalf("selected final generation error wrong: code=%q source=%q", record.FinalUpstreamErrorCode, record.FinalUpstreamErrorSource)
			}
			if mode == "prior429_refresh400" {
				if record.Attempts[0].Phase != "request" || record.Attempts[0].ErrorCode != "rateLimitExceeded" || record.Attempts[1].Phase != "refresh" || *record.Attempts[1].ObservedHTTPStatusCode != 400 {
					t.Fatal("prior response dropped refresh or lost generation")
				}
			}
			if mode == "truncated_final429" && (record.AttemptsTruncated == nil || !*record.AttemptsTruncated || *record.Attempts[3].StatusCode != 401) {
				t.Fatal("final outcome depended on retained last item")
			}
			if mode == "refresh_only" && (record.Attempts[0].Phase != "refresh" || *record.Attempts[0].StatusCode != 400) {
				t.Fatal("refresh relabeled as generation")
			}
			rows[mode] = record
			t.Logf("mode=%s http=%d generations=%d refresh=%d network_entries=%d final_generation_code=%q", mode, w.Code, generations, refreshes, len(record.Attempts), record.FinalUpstreamErrorCode)
		})
	}
	if path := os.Getenv("CODEX_OAUTH_V4_RECORDS"); path != "" {
		data, _ := json.MarshalIndent(rows, "", "  ")
		if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBPSOAuthFinalFailureRecognizedOnly(t *testing.T) {
	for _, tc := range []struct {
		name                string
		failure             error
		model, path, status string
		want                string
	}{
		{"typed_quota", &officialProtocolError{status: 429, code: "rateLimitExceeded", source: "upstream"}, "m", "/responses", "error", "rateLimitExceeded"},
		{"unknown_code", &officialProtocolError{status: 429, code: "PRIVATE_UNKNOWN", source: "upstream"}, "m", "/responses", "error", ""},
		{"text_only", errors.New("code=rateLimitExceeded"), "m", "/responses", "error", ""},
		{"oauth_only", &gatewayOAuthRefreshError{Kind: "http", StatusCode: 429}, "", "", "error", ""},
		{"catalog", &officialProtocolError{status: 429, code: "rateLimitExceeded", source: "upstream"}, "m", "/responses/access", "error", ""},
		{"gateway_source", &officialProtocolError{status: 429, code: "rateLimitExceeded", source: "gateway"}, "m", "/responses", "error", ""},
		{"success", &officialProtocolError{status: 429, code: "rateLimitExceeded", source: "upstream"}, "m", "/responses", "ok", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := RequestRecord{Status: tc.status, SentModel: tc.model, UpstreamPath: tc.path, HTTPCode: 503}
			s := officialFailoverState{failure: tc.failure}
			s.captureFinalGenerationFailure(&r)
			if r.FinalUpstreamErrorCode != tc.want {
				t.Fatalf("got=%q want=%q", r.FinalUpstreamErrorCode, tc.want)
			}
		})
	}
}
