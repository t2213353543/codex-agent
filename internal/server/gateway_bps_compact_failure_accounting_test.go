package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const compactFailureReviewUsage = `"usage":{"input_tokens":1000,"output_tokens":11,"total_tokens":1011,"input_tokens_details":{"cached_tokens":750,"cache_write_tokens":250}}`
const compactFailureReviewState = `[{"type":"compaction","encrypted_content":"PRIVATE_SYNTHETIC_COMPACT_STATE"}]`

func TestBPSCompactFailureAccountingReview(t *testing.T) {
	report := map[string]any{}
	defer func() {
		if folder := os.Getenv("COMPACT_FAILURE_REVIEW_OUTPUT"); folder != "" {
			raw, _ := json.MarshalIndent(report, "", "  ")
			if err := os.WriteFile(filepath.Join(folder, "gateway-records.json"), raw, 0600); err != nil {
				t.Error(err)
			}
		}
	}()
	for _, tc := range []struct {
		name, raw, terminal string
		meter, success      bool
	}{
		{"failed_state", `{"id":"resp_failed","model":"gpt-6-astra","status":"failed","error":{"message":"synthetic generation failure"},"output":` + compactFailureReviewState + `,` + compactFailureReviewUsage + `}`, "failed", true, false},
		{"failed_no_output", `{"id":"resp_failed_absent","model":"gpt-6-astra","status":"failed","error":{"message":"synthetic generation failure"},` + compactFailureReviewUsage + `}`, "failed", true, false},
		{"nested_response_cannot_replace_root", `{"id":"resp_root","model":"gpt-6-astra","status":"failed","output":[],` + compactFailureReviewUsage + `,"response":{"id":"resp_nested","model":"PRIVATE_WRONG_MODEL","status":"completed","usage":{"input_tokens":9999,"output_tokens":99}}}`, "failed", true, false},
		{"completed_without_state", `{"id":"resp_no_state","model":"gpt-6-astra","status":"completed","output":[],` + compactFailureReviewUsage + `}`, "completed", true, false},
		{"completed_with_error", `{"id":"resp_error","model":"gpt-6-astra","status":"completed","error":{"message":"synthetic contradictory error"},"output":` + compactFailureReviewState + `,` + compactFailureReviewUsage + `}`, "completed", true, false},
		{"incomplete_state", `{"id":"resp_incomplete","model":"gpt-6-astra","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":` + compactFailureReviewState + `,` + compactFailureReviewUsage + `}`, "incomplete", true, false},
		{"cancelled_state", `{"id":"resp_cancelled","model":"gpt-6-astra","status":"cancelled","output":` + compactFailureReviewState + `,` + compactFailureReviewUsage + `}`, "cancelled", true, false},
		{"legacy_success", `{"id":"resp_legacy","object":"response.compaction","model":"gpt-6-astra","output":` + compactFailureReviewState + `,` + compactFailureReviewUsage + `}`, "", true, true},
		{"invalid_json", `{"id":"resp_bad","model":"gpt-6-astra",` + compactFailureReviewUsage + `,`, "", false, false},
		{"missing_id_rejected", `{"model":"gpt-6-astra","status":"failed","output":[],` + compactFailureReviewUsage + `}`, "", false, false},
		{"missing_id_with_state", `{"model":"gpt-6-astra","status":"completed","output":` + compactFailureReviewState + `,` + compactFailureReviewUsage + `}`, "", false, false},
		{"id_wrong_type", `{"id":7,"model":"gpt-6-astra","status":"failed","output":[],` + compactFailureReviewUsage + `}`, "", false, false},
		{"output_object", `{"id":"resp_shape","model":"gpt-6-astra","status":"failed","output":{},` + compactFailureReviewUsage + `}`, "", false, false},
		{"output_null", `{"id":"resp_null","model":"gpt-6-astra","status":"failed","output":null,` + compactFailureReviewUsage + `}`, "", false, false},
		{"output_item_scalar", `{"id":"resp_scalar","model":"gpt-6-astra","status":"failed","output":[17],` + compactFailureReviewUsage + `}`, "", false, false},
		{"output_item_no_type", `{"id":"resp_untyped","model":"gpt-6-astra","status":"failed","output":[{}],` + compactFailureReviewUsage + `}`, "", false, false},
		{"status_unknown", `{"id":"resp_running","model":"gpt-6-astra","status":"in_progress","output":[],` + compactFailureReviewUsage + `}`, "", false, false},
		{"status_wrong_type", `{"id":"resp_status","model":"gpt-6-astra","status":7,"output":[],` + compactFailureReviewUsage + `}`, "", false, false},
		{"status_null", `{"id":"resp_status_null","model":"gpt-6-astra","status":null,"output":[],` + compactFailureReviewUsage + `}`, "", false, false},
		{"status_null_with_state", `{"id":"resp_status_null_state","model":"gpt-6-astra","status":null,"output":` + compactFailureReviewState + `,` + compactFailureReviewUsage + `}`, "", false, false},
		{"status_empty_with_state", `{"id":"resp_status_empty_state","model":"gpt-6-astra","status":"","output":` + compactFailureReviewState + `,` + compactFailureReviewUsage + `}`, "", false, false},
		{"error_wrong_shape", `{"id":"resp_error_shape","model":"gpt-6-astra","status":"failed","error":"PRIVATE_INVALID_ERROR","output":[],` + compactFailureReviewUsage + `}`, "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts := 0
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				posts++
				if r.Method != "POST" || r.URL.Path != "/responses/compact" {
					t.Error("unexpected request")
				}
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.raw)
			})
			w := httptest.NewRecorder()
			p.gatewayBPS(w, proxyRequest(http.MethodPost, "/v1/responses/compact", `{"model":"gpt-6-astra","input":[]}`))
			rec, ok := p.Metrics.LastFor("test")
			if !ok || posts != 1 {
				t.Fatal("not exactly one actual gateway request/record")
			}
			row := map[string]any{"public_http": w.Code, "record_status": rec.Status, "record_http": rec.HTTPCode, "input": rec.InputTokens, "output": rec.OutputTokens, "read": rec.CachedTokens, "write": rec.CacheWriteTokens, "usage_known": rec.UsageKnown, "cached_known": rec.CachedTokensKnown, "returned_model": rec.ReturnedModel, "terminal_status": rec.TerminalStatus, "error_source": rec.ErrorSource, "error_phase": rec.ErrorPhase, "error": rec.Error, "response_id": rec.ResponseID, "network_posts": posts, "attempts": rec.Attempts, "final_code": rec.FinalUpstreamErrorCode}
			report[tc.name] = row
			t.Logf("case=%s HTTP=%d status=%s in=%d out=%d read=%d write=%v model=%q terminal=%q source=%s/%s", tc.name, w.Code, rec.Status, rec.InputTokens, rec.OutputTokens, rec.CachedTokens, rec.CacheWriteTokens, rec.ReturnedModel, rec.TerminalStatus, rec.ErrorSource, rec.ErrorPhase)
			if tc.success {
				if w.Code != 200 || rec.Status != "ok" {
					t.Error("valid legacy compaction regressed")
				}
			} else {
				if w.Code != 502 || rec.Status != "error" || strings.Contains(w.Body.String(), "PRIVATE_SYNTHETIC_COMPACT_STATE") {
					t.Error("invalid compaction became success or leaked state")
				}
				if rec.TTFTMs != nil {
					t.Error("rejected compaction acquired TTFT")
				}
				var source struct{ ID string }
				_ = json.Unmarshal([]byte(tc.raw), &source)
				if source.ID != "" {
					if _, err := p.bpsLoadHistory(withGatewayUnboundedPayload(t.Context(), true), "admin", source.ID); err == nil {
						t.Error("rejected compaction became resumable history")
					}
				}
			}
			if tc.meter {
				if rec.UsageKnown == nil || !*rec.UsageKnown || rec.CachedTokensKnown == nil || !*rec.CachedTokensKnown || rec.InputTokens != 1000 || rec.OutputTokens != 11 || rec.CachedTokens != 750 || rec.CacheWriteTokens == nil || *rec.CacheWriteTokens != 250 || rec.ReturnedModel != "gpt-6-astra" || rec.TerminalStatus != tc.terminal {
					t.Error("identified complete JSON lost real usage/model/terminal metadata")
				}
				if !tc.success && rec.ErrorSource != "upstream" {
					t.Error("rejected complete upstream JSON lacks upstream provenance")
				}
				if tc.name == "nested_response_cannot_replace_root" && rec.ResponseID != "resp_root" {
					t.Error("unrelated nested response replaced validated root identity")
				}
			} else if rec.InputTokens != 0 || rec.OutputTokens != 0 || rec.CachedTokens != 0 || rec.CacheWriteTokens != nil || rec.ReturnedModel != "" || rec.UsageKnown != nil && *rec.UsageKnown {
				t.Error("invalid envelope acquired trusted usage/model")
			}
		})
	}
}

func TestBPSCompactFailureCacheUnknownZeroReview(t *testing.T) {
	for _, tc := range []struct {
		name, details string
		known         bool
	}{{"unknown", "", false}, {"zero", `,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0}`, true}} {
		t.Run(tc.name, func(t *testing.T) {
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				_, _ = io.WriteString(w, `{"id":"resp_cache_`+tc.name+`","model":"gpt-6-astra","status":"failed","output":[],"usage":{"input_tokens":1000,"output_tokens":11`+tc.details+`}}`)
			})
			w := httptest.NewRecorder()
			p.gatewayBPS(w, proxyRequest("POST", "/v1/responses/compact", `{"model":"gpt-6-astra","input":[]}`))
			rec, ok := p.Metrics.LastFor("test")
			if !ok || w.Code != 502 || rec.Status != "error" || rec.UsageKnown == nil || !*rec.UsageKnown || rec.CachedTokensKnown == nil || *rec.CachedTokensKnown != tc.known {
				t.Fatal("failed usage or cache known state lost")
			}
			if tc.known {
				if rec.CacheWriteTokens == nil || *rec.CacheWriteTokens != 0 {
					t.Fatal("measured zero write lost")
				}
			} else if rec.CacheWriteTokens != nil {
				t.Fatal("unknown write invented")
			}
		})
	}
}

func TestBPSCompactFailureUsageAliasesReview(t *testing.T) {
	for i, usage := range []string{
		`{"input_tokens":null,"prompt_tokens":1000,"output_tokens":11,"cached_tokens":750}`,
		`{"input_tokens":1000,"output_tokens":11,"input_tokens_details":{"cached_tokens":null},"cached_tokens":750}`,
		`{"input_tokens":1000,"output_tokens":11,"input_tokens_details":"PRIVATE_INVALID_DETAILS","cached_tokens":750}`,
		`{"input_tokens":9007199254740993,"output_tokens":11,"input_tokens_details":{"cached_tokens":9007199254740992,"cache_write_tokens":0}}`,
	} {
		raw := []byte(`{"id":"resp_alias","model":"gpt-6-astra","status":"failed","output":[],"usage":` + usage + `}`)
		var expected RequestRecord
		recordBPSTerminalUsage(&expected, raw)
		p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body); _, _ = w.Write(raw) })
		w := httptest.NewRecorder()
		p.gatewayBPS(w, proxyRequest("POST", "/v1/responses/compact", `{"model":"gpt-6-astra","input":[]}`))
		rec, ok := p.Metrics.LastFor("test")
		if !ok || w.Code != 502 || rec.Status != "error" {
			t.Fatal("fixture did not fail")
		}
		if rec.InputTokens != expected.InputTokens || rec.OutputTokens != expected.OutputTokens || rec.CachedTokens != expected.CachedTokens || !reflect.DeepEqual(rec.UsageKnown, expected.UsageKnown) || !reflect.DeepEqual(rec.CachedTokensKnown, expected.CachedTokensKnown) || !reflect.DeepEqual(rec.CacheWriteTokens, expected.CacheWriteTokens) {
			t.Errorf("case %d changed usage priority/precision", i)
		}
	}
}
