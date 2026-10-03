package server

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Actual file-account gateway producer -> isolated PG -> fresh reader/exports.
// Does not import fabricated RequestRecords or rewrite observed transport codes.
func TestPGBPSStreamAuthAndLocalCompletionPersistence(t *testing.T) {
	for _, mode := range []string{"http200_typed_auth_sse", "structured_account_deleted"} {
		t.Run(mode, func(t *testing.T) {
			db := newMetricsTestDB(t)
			var p *Proxy
			calls := 0
			p = bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if mode == "structured_account_deleted" {
					reply := httptest.NewRecorder()
					bpsStructuredUpstreamReply(t, reply, r, "pg_deleted_completion", `{"number":9007199254740993}`)
					if err := p.Store.Delete("test"); err != nil {
						t.Error(err)
					}
					p.invalidateAccounts()
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write(reply.Body.Bytes())
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "data: "+`{"type":"response.created","response":{"id":"pg_stream_auth","status":"in_progress"}}`+"\n\n")
				_, _ = fmt.Fprint(w, "data: "+`{"type":"response.failed","response":{"id":"pg_stream_auth","status":"failed","model":"gpt-6-astra","output":[],"error":{"code":"invalid_api_key","type":"authentication_error","message":"synthetic expired upstream credential"},"usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":20}}}}`+"\n\n")
			})
			settings, err := p.Settings.Get()
			if err != nil {
				t.Fatal(err)
			}
			settings.Upstream401RecoveryEnabled = false
			settings.AutomaticTokenRefreshEnabled = false
			settings.RateLimitCooldownEnabled = false
			if err = p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			if mode == "http200_typed_auth_sse" {
				if err = p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; return nil }); err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(map[string]any{"access_token": "second-synthetic-token", "expires_at": time.Now().Add(time.Hour).Unix()})
				if err = p.Store.ImportWith("second", raw, AccountDefaults{Concurrency: 2, Priority: 1}); err != nil {
					t.Fatal(err)
				}
				p.invalidateAccounts()
			}
			// Real invalid client credentials remain401 and never reach upstream; keep
			// this control outside the later PG producer to isolate its single event.
			invalid := proxyRequest("POST", "/v1/responses", `{"model":"gpt-6-astra","input":"synthetic"}`)
			invalid.Header.Set("Authorization", "Bearer invalid-client-key")
			bad := httptest.NewRecorder()
			p.gatewayBPS(bad, invalid)
			if bad.Code != 401 || calls != 0 {
				t.Fatal("invalid client authentication remapped or dispatched")
			}
			p.Metrics.Close()
			p.Metrics = newTestPGMetrics(t, db)
			t.Cleanup(p.Metrics.Close)
			body := `{"model":"gpt-6-astra","stream":true,"input":"synthetic"}`
			if mode == "structured_account_deleted" {
				body = `{"model":"gpt-6-astra","stream":true,"input":"synthetic","text":` + bpsStructuredTestText + `}`
			}
			req := proxyRequest("POST", "/v1/responses", body)
			req.Header.Set("Session-Id", "pg-stream-auth-"+mode)
			out := httptest.NewRecorder()
			p.gatewayBPS(out, req)
			wantHTTP, wantCalls, wantCategory := 503, 2, "authentication"
			if mode == "structured_account_deleted" {
				wantHTTP, wantCalls, wantCategory = 200, 1, "unknown"
			}
			if out.Code != wantHTTP || calls != wantCalls {
				t.Fatalf("publicHTTP=%d attempts=%d want=%d/%d", out.Code, calls, wantHTTP, wantCalls)
			}
			if mode == "http200_typed_auth_sse" {
				var response struct{ Error struct{ Code, Type string } }
				if json.Unmarshal(out.Body.Bytes(), &response) != nil || response.Error.Code != "upstream_authentication_unavailable" || response.Error.Type != "server_error" || strings.Contains(out.Body.String(), "invalid_api_key") {
					t.Fatal("public stream auth translation incorrect")
				}
			} else {
				events := bpsGatewayEvents(t, out.Body.String())
				if len(events) != 1 || bpsGatewayString(events[0], "type") != "response.failed" {
					t.Fatal("local completion failure lacks one explicit terminal")
				}
				var response struct {
					Status string
					Output []json.RawMessage
					Error  struct{ Code, Type, Message string }
				}
				if json.Unmarshal(events[0]["response"], &response) != nil || response.Status != "failed" || len(response.Output) != 0 || response.Error.Code != "response_access_changed" || response.Error.Type != "proxy_error" || strings.Contains(strings.ToLower(response.Error.Message), "auth") {
					t.Fatal("local access change emitted auth semantics or output")
				}
				if _, err := p.bpsLoadHistory(withGatewayUnboundedPayload(t.Context(), true), "admin", "pg_deleted_completion"); err == nil {
					t.Fatal("failed local checkpoint saved successful history")
				}
			}
			if err = p.Metrics.Save(); err != nil || p.Metrics.HealthError() != nil {
				t.Fatal("actual gateway producer could not persist")
			}
			fresh := newTestPGMetrics(t, db)
			t.Cleanup(fresh.Close)
			query := managedQuery(t, "exact_total=true&http_code="+strconv.Itoa(wantHTTP)+"&error_category="+wantCategory)
			page, err := fresh.managedLogs(t.Context(), query)
			if err != nil || page.Total != 1 || len(page.Items) != 1 {
				t.Fatalf("fresh filtered log query count=%d/%d err=%v", page.Total, len(page.Items), err)
			}
			row := page.Items[0]
			assertPGStreamAuthCompletionRecord(t, row.RequestRecord, mode)
			if row.ErrorCategory != wantCategory {
				t.Fatal("fresh category changed")
			}
			detail, err := fresh.managedLogDetail(t.Context(), row.EventID)
			if err != nil {
				t.Fatal(err)
			}
			assertPGStreamAuthCompletionRecord(t, detail, mode)
			var stored []byte
			if err = db.Pool.QueryRow(t.Context(), "SELECT record FROM request_logs WHERE event_id=$1", row.EventID).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			var durable RequestRecord
			if json.Unmarshal(stored, &durable) != nil {
				t.Fatal("durable JSON malformed")
			}
			assertPGStreamAuthCompletionRecord(t, durable, mode)
			var count int
			var status string
			if err = db.Pool.QueryRow(t.Context(), "SELECT count(*),min(status) FROM usage_events WHERE event_id=$1", row.EventID).Scan(&count, &status); err != nil || count != 1 || status != "error" {
				t.Fatal("terminal failure persisted as success or duplicate usage event")
			}
			stats, err := fresh.managedLogStats(t.Context(), query)
			if err != nil || stats.Requests != 1 || stats.Errors != 1 || stats.Success != 0 || stats.UsageKnownRequests != 1 || stats.CacheReadSamples != 1 || stats.CacheWriteSamples != 1 || stats.TTFTSamples != 0 {
				t.Fatal("failure accounting/category/known-cache changed")
			}
			var jsonl bytes.Buffer
			if n, err := fresh.exportManagedLogs(t.Context(), query, "jsonl", &jsonl); err != nil || n != 1 {
				t.Fatal("JSONL export failed")
			}
			var exported RequestRecord
			if json.Unmarshal(bytes.TrimSpace(jsonl.Bytes()), &exported) != nil {
				t.Fatal("JSONL invalid")
			}
			assertPGStreamAuthCompletionRecord(t, exported, mode)
			var raw map[string]json.RawMessage
			_ = json.Unmarshal(bytes.TrimSpace(jsonl.Bytes()), &raw)
			if string(raw["error_category"]) != strconv.Quote(wantCategory) {
				t.Fatal("JSONL category changed")
			}
			var csvData bytes.Buffer
			if n, err := fresh.exportManagedLogs(t.Context(), query, "csv", &csvData); err != nil || n != 1 {
				t.Fatal("CSV export failed")
			}
			rows, err := csv.NewReader(&csvData).ReadAll()
			if err != nil || len(rows) != 2 || len(rows[0]) != 77 || len(rows[1]) != 77 {
				t.Fatal("CSV v4/77-column schema changed")
			}
			fields := map[string]string{}
			for i, key := range rows[0] {
				fields[key] = rows[1][i]
			}
			if fields["http_code"] != strconv.Itoa(wantHTTP) || fields["status"] != "error" || fields["error_category"] != wantCategory || fields["error_source"] != detail.ErrorSource || fields["error_phase"] != detail.ErrorPhase || fields["final_upstream_error_code"] != detail.FinalUpstreamErrorCode || fields["final_upstream_error_source"] != detail.FinalUpstreamErrorSource || fields["input_tokens"] != "100" || fields["cached_tokens"] != "80" || fields["ttft_ms"] != "" {
				t.Fatal("CSV changed public status/internal cause/known usage")
			}
			var attempts []GatewayAttemptSummary
			if json.Unmarshal([]byte(fields["attempts"]), &attempts) != nil || !reflect.DeepEqual(attempts, detail.Attempts) {
				t.Fatal("CSV changed logical vs observed transport ledger")
			}
			t.Logf("mode=%s public=%d persisted=error category=%s logicalattempt=%d observed200 attempts=%d usagepreserved=true exports77=true", mode, wantHTTP, wantCategory, *detail.Attempts[0].StatusCode, len(detail.Attempts))
		})
	}
}

func assertPGStreamAuthCompletionRecord(t *testing.T, r RequestRecord, mode string) {
	t.Helper()
	wantHTTP, wantAttempts, wantLogical, wantOut, wantWrite := 503, 2, 401, 7, 20
	if mode == "structured_account_deleted" {
		wantHTTP, wantAttempts, wantLogical, wantOut, wantWrite = 200, 1, 200, 10, 0
	}
	if r.HTTPCode != wantHTTP || r.Status != "error" || r.TerminalStatus != "failed" || r.ObservationSchemaVersion != 2 || r.InputTokens != 100 || r.OutputTokens != int64(wantOut) || r.CachedTokens != 80 || r.CacheWriteTokens == nil || *r.CacheWriteTokens != int64(wantWrite) || r.UsageKnown == nil || !*r.UsageKnown || r.CachedTokensKnown == nil || !*r.CachedTokensKnown || r.TTFTMs != nil || r.ReturnedModel != "gpt-6-astra" {
		t.Fatalf("terminal/accounting changed HTTP%d status%s terminal%s input%d output%d", r.HTTPCode, r.Status, r.TerminalStatus, r.InputTokens, r.OutputTokens)
	}
	if len(r.Attempts) != wantAttempts {
		t.Fatal("actual generation attempt count changed")
	}
	for i, a := range r.Attempts {
		if a.Attempt != i+1 || a.Phase != "request" || a.Endpoint != "/responses" || a.StatusCode == nil || *a.StatusCode != wantLogical || a.ObservedHTTPStatusCode == nil || *a.ObservedHTTPStatusCode != 200 {
			t.Fatalf("logical/actual status collapsed: %+v", a)
		}
	}
	if mode == "http200_typed_auth_sse" {
		if r.ErrorSource != "upstream" || r.ErrorPhase != "response_event" || r.FinalUpstreamErrorSource != "upstream" || r.FinalUpstreamErrorCode != "unauthorized" || !strings.Contains(r.Error, "(code=invalid_api_key)") || !strings.Contains(r.Error, "(error_type=authentication_error)") || strings.Contains(r.Error, "synthetic expired upstream credential") {
			t.Fatalf("upstream cause changed: %s/%s final=%s/%s", r.ErrorSource, r.ErrorPhase, r.FinalUpstreamErrorSource, r.FinalUpstreamErrorCode)
		}
	} else {
		if r.ErrorSource != "gateway" || r.ErrorPhase != "authorization" || !strings.Contains(r.Error, "after authentication recovery") || r.FinalUpstreamErrorCode == "invalid_api_key" || r.FinalUpstreamErrorCode == "unauthorized" {
			t.Fatalf("local completion cause became upstream auth: %s/%s final=%s/%s", r.ErrorSource, r.ErrorPhase, r.FinalUpstreamErrorSource, r.FinalUpstreamErrorCode)
		}
	}
}
