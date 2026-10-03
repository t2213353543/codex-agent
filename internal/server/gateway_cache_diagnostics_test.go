package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func cacheDiagnosticsForTest(t *testing.T) *gatewayCacheDiagnostics {
	t.Helper()
	d, err := newGatewayCacheDiagnostics()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func observeCacheRequestForTest(t *testing.T, d *gatewayCacheDiagnostics, session, thread, body string) uint64 {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/responses", nil)
	r.Header.Set("Authorization", "Bearer secret-authorization-for-diagnostics-test")
	r.Header.Set("session-id", session)
	r.Header.Set("thread-id", thread)
	id, err := d.ObserveRequest(r, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestGatewayCacheDiagnosticsTracksThreadPrefixWithoutRewriting(t *testing.T) {
	d := cacheDiagnosticsForTest(t)
	first := `{"model":"native-model","instructions":"secret-instructions-中文","tools":[{"name":"z","parameters":{"maximum":9007199254740993,"type":"number"}},{"name":"a"}],"reasoning":{"effort":"high"},"prompt_cache_key":"secret-cache-key","prompt_cache_options":{"breakpoints":[1,3],"unknown_state":{"opaque":true}},"input":[{"type":"message","role":"user","content":"secret-user-text"},{"type":"reasoning","encrypted_content":"secret-encrypted-payload"}],"client_metadata":{"session_id":"secret-session","thread_id":"root"}}`
	var fields map[string]json.RawMessage
	_ = json.Unmarshal([]byte(first), &fields)
	fields["instructions"] = json.RawMessage(`"secret-instructions-\u4e2d\u6587"`)
	fields["tools"] = json.RawMessage(`[{"parameters":{"type":"number","maximum":9007199254740993},"name":"z"},{"name":"a"}]`)
	fields["input"] = json.RawMessage(`[{"content":"secret-user-text","role":"user","type":"message"},{"encrypted_content":"secret-encrypted-payload","type":"reasoning"},{"type":"function_call_output","call_id":"call-opaque","output":"secret-tool-result"}]`)
	second, _ := json.Marshal(fields)
	original := append([]byte(nil), second...)
	observeCacheRequestForTest(t, d, "secret-session", "root", first)
	observeCacheRequestForTest(t, d, "secret-session", "child-a", `{"model":"child-model","prompt_cache_key":"secret-cache-key","input":[{"type":"message","content":"different child"}]}`)
	observeCacheRequestForTest(t, d, "secret-session", "child-b", `{"model":"child-model","prompt_cache_key":"secret-cache-key","input":[]}`)
	if _, err := d.ObserveRequest(nil, second); err != nil {
		t.Fatal(err)
	}
	// Authorization scopes prevent metadata-only observations from being mixed
	// with requests made under an observed credential. Use the same headers here.
	observeCacheRequestForTest(t, d, "secret-session", "root", string(second))
	if !bytes.Equal(second, original) {
		t.Fatal("observer mutated the outbound payload")
	}
	summary := d.Snapshot()
	if summary.Requests != 5 || summary.ComparablePairs != 1 || summary.StableConfigurationPairs != 1 || summary.SharedInputItems != 2 || summary.PreviousInputItemsCompared != 2 || summary.InputHistoryExtensionPairs != 1 || summary.CacheKeyChangedPairs != 0 {
		t.Fatalf("unexpected scoped prefix summary: %+v", summary)
	}
	serialized, _ := json.Marshal(summary)
	for _, secret := range []string{"secret-", "native-model", "call-opaque", "maximum", "Bearer", "child-a"} {
		if bytes.Contains(serialized, []byte(secret)) {
			t.Fatalf("summary contains private request content: %s", secret)
		}
	}
}

func TestGatewayCacheDiagnosticsDetectsRealConfigurationChanges(t *testing.T) {
	d := cacheDiagnosticsForTest(t)
	requests := []string{
		`{"model":"a","instructions":"same","tools":[{"name":"z","n":9007199254740993},{"name":"a"}],"prompt_cache_key":"a","input":[{"content":"first"}]}`,
		`{"model":"a","instructions":"same","tools":[{"name":"z","n":9007199254740992},{"name":"a"}],"prompt_cache_key":"a","input":[{"content":"first"}]}`,
		`{"model":"a","instructions":"same","tools":[{"name":"a"},{"name":"z","n":9007199254740992}],"prompt_cache_key":"a","input":[{"content":"first"}]}`,
		`{"model":"a","instructions":"changed","tools":[{"name":"a"},{"name":"z","n":9007199254740992}],"prompt_cache_key":"b","text":null,"input":[{"content":"changed"}]}`,
		`{"model":"b","instructions":"changed","tools":[{"name":"a"},{"name":"z","n":9007199254740992}],"prompt_cache_key":"b","text":null,"input":[]}`,
	}
	for _, body := range requests {
		observeCacheRequestForTest(t, d, "session", "root", body)
	}
	s := d.Snapshot()
	if s.ComparablePairs != 4 || s.StableConfigurationPairs != 0 || s.ToolsChangedPairs != 2 || s.InstructionsChangedPairs != 1 || s.ControlsChangedPairs != 1 || s.ModelChangedPairs != 1 || s.CacheKeyChangedPairs != 1 || s.InputHistoryChangedPairs != 2 {
		t.Fatalf("configuration changes were lost: %+v", s)
	}
}

func TestGatewayCacheDiagnosticsDoesNotInventSessionFromSharedCacheKey(t *testing.T) {
	d := cacheDiagnosticsForTest(t)
	for range 2 {
		observeCacheRequestForTest(t, d, "", "", `{"prompt_cache_key":"shared-app-key","input":[]}`)
	}
	for range 2 {
		if _, err := d.ObserveRequest(nil, []byte(`{"client_metadata":{"session_id":"root","thread_id":"child"},"input":[]}`)); err != nil {
			t.Fatal(err)
		}
	}
	s := d.Snapshot()
	if s.UnscopedRequests != 2 || s.ComparablePairs != 1 || s.MissingCacheKeyRequests != 2 {
		t.Fatalf("incorrect scope inference: %+v", s)
	}
}

func TestGatewayCacheDiagnosticsUsesReportedWeightedCacheUsage(t *testing.T) {
	d := cacheDiagnosticsForTest(t)
	cases := []string{
		`{"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":80}}}}`,
		`{"status":"completed","usage":{"input_tokens":900,"output_tokens":20,"cached_tokens":90}}`,
		`{"status":"completed","usage":{"input_tokens":1000,"output_tokens":30}}`,
		`{"status":"completed","usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":0}}}`,
		`{"type":"response.failed","response":{"status":"unknown","usage":{"input_tokens":50,"output_tokens":1,"input_tokens_details":{"cached_tokens":20}}}}`,
		`{"type":"response.incomplete","response":{"usage":{"input_tokens":50,"output_tokens":2,"input_tokens_details":{"cached_tokens":10}}}}`,
		`{"type":"error","message":"secret-upstream-error"}`,
		`{"status":"cancelled","usage":null}`,
	}
	var lastID uint64
	for _, response := range cases {
		lastID = observeCacheRequestForTest(t, d, "s", "t", `{"input":[]}`)
		if err := d.ObserveResponse(lastID, []byte(response)); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.ObserveResponse(lastID, []byte(cases[len(cases)-1])); err == nil {
		t.Fatal("duplicate terminal event was counted again")
	}
	s := d.Snapshot()
	if s.Responses != 8 || s.SuccessfulResponses != 4 || s.FailedResponses != 3 || s.IncompleteResponses != 1 || s.UsageResponses != 6 || s.CacheUsageResponses != 5 || s.CacheHitResponses != 4 || s.InputTokens != 2200 || s.OutputTokens != 73 || s.CachedInputTokens != 200 || s.CacheUsageInputTokens != 1200 || s.CacheHitRatio == nil || math.Abs(*s.CacheHitRatio-1.0/6) > 1e-12 || s.PendingRequests != 0 {
		t.Fatalf("invalid usage aggregation: %+v", s)
	}
}

func TestGatewayCacheDiagnosticsRejectsInvalidUsageWithoutChangingOutcome(t *testing.T) {
	d := cacheDiagnosticsForTest(t)
	for _, usage := range []string{
		`{"input_tokens":10,"output_tokens":1,"input_tokens_details":{"cached_tokens":-1}}`,
		`{"input_tokens":10,"output_tokens":1,"input_tokens_details":{"cached_tokens":11}}`,
		`{"input_tokens":-10,"output_tokens":1,"cached_tokens":1}`,
		`{"input_tokens":10,"output_tokens":1,"cached_tokens":"secret-malformed"}`,
		`{"input_tokens":10,"cached_tokens":1}`,
		`[]`,
	} {
		id := observeCacheRequestForTest(t, d, "s", "t", `{}`)
		if err := d.ObserveResponse(id, []byte(`{"status":"failed","usage":`+usage+`}`)); err != nil {
			t.Fatal(err)
		}
	}
	s := d.Snapshot()
	if s.FailedResponses != 6 || s.InvalidUsageResponses != 6 || s.CacheUsageResponses != 0 || s.CacheHitRatio != nil || s.PendingRequests != 0 {
		t.Fatalf("invalid usage fabricated a cache hit or hid a failure: %+v", s)
	}
}

func TestGatewayCacheDiagnosticsRejectsNonterminalAndProtectsUsageTotals(t *testing.T) {
	d := cacheDiagnosticsForTest(t)
	id := observeCacheRequestForTest(t, d, "s", "t", `{}`)
	for _, response := range []string{`{}`, `{"type":"response.output_text.delta","status":"completed"}`, `{"type":"response.completed","response":null}`} {
		if err := d.ObserveResponse(id, []byte(response)); err == nil {
			t.Fatalf("accepted a nonterminal response: %s", response)
		}
	}
	if s := d.Snapshot(); s.Responses != 0 || s.PendingRequests != 1 {
		t.Fatalf("invalid response consumed pending request: %+v", s)
	}
	if err := d.ObserveResponse(id, []byte(`{"status":"completed","usage":{"input_tokens":9223372036854775807,"output_tokens":0,"cached_tokens":9223372036854775807}}`)); err != nil {
		t.Fatal(err)
	}
	id = observeCacheRequestForTest(t, d, "s", "t", `{}`)
	if err := d.ObserveResponse(id, []byte(`{"status":"completed","usage":{"input_tokens":1,"output_tokens":0,"cached_tokens":1}}`)); err != nil {
		t.Fatal(err)
	}
	s := d.Snapshot()
	if s.InputTokens != math.MaxInt64 || s.CachedInputTokens != math.MaxInt64 || s.InvalidUsageResponses != 1 || s.CacheHitRatio == nil || *s.CacheHitRatio != 1 || s.Responses != 2 {
		t.Fatalf("token total overflowed: %+v", s)
	}
}

func TestGatewayCacheDiagnosticsRetainsOnlyBoundedDigests(t *testing.T) {
	d := cacheDiagnosticsForTest(t)
	body := `{"input":[` + strings.Repeat(`{"type":"message","content":"private"},`, maxCacheDiagnosticItems) + `{"type":"message","content":"private-last"}]}`
	observeCacheRequestForTest(t, d, "s", "t", body)
	observeCacheRequestForTest(t, d, "s", "t", strings.TrimSuffix(body, "]}")+`,{"content":"later"}]}`)
	for _, value := range d.latest {
		if len(value.input) != maxCacheDiagnosticItems {
			t.Fatalf("unexpected digest retention %d", len(value.input))
		}
	}
	if s := d.Snapshot(); s.InputComparisonLimitedRequests != 2 || s.InputHistoryExtensionPairs != 0 {
		t.Fatalf("truncated input claimed a verified extension: %+v", s)
	}
	for i := 0; i < maxCacheDiagnosticScopes; i++ {
		id := observeCacheRequestForTest(t, d, "s", fmt.Sprint(i), `{}`)
		d.abandon(id)
	}
	if len(d.latest) != maxCacheDiagnosticScopes || d.Snapshot().ScopeEvictions != 1 {
		t.Fatal("scope history was not bounded")
	}
	for len(d.pending) < maxCacheDiagnosticPending {
		observeCacheRequestForTest(t, d, "", "", `{}`)
	}
	if _, err := d.ObserveRequest(nil, []byte(`{}`)); err == nil || d.Snapshot().PendingRequests != maxCacheDiagnosticPending {
		t.Fatal("pending request limit was not enforced")
	}
}

func TestGatewayCacheResponseObserverParsesSplitSSEAndFirstTerminal(t *testing.T) {
	for _, lineEnding := range []string{"\n", "\r", "\r\n"} {
		t.Run(fmt.Sprintf("%q", lineEnding), func(t *testing.T) {
			d := cacheDiagnosticsForTest(t)
			id := observeCacheRequestForTest(t, d, "s", "t", `{}`)
			o := d.ResponseObserver(id)
			stream := strings.Join([]string{
				`: comment`, `data: {"type":"response.output_text.delta","delta":"secret"}`, ``,
				`data: {"type":"response.failed",`, `data: "response":{"status":"failed","usage":{"input_tokens":20,"output_tokens":1,"input_tokens_details":{"cached_tokens":10}}}}`, ``,
				`data: {"type":"response.completed","response":{}}`, ``, ``,
			}, lineEnding)
			for i := range len(stream) {
				if n, err := o.Write([]byte{stream[i]}); n != 1 || err != nil {
					t.Fatalf("observer interrupted transport: n=%d err=%v", n, err)
				}
			}
			for range 2 {
				if err := o.Finish(); err != nil {
					t.Fatal(err)
				}
			}
			s := d.Snapshot()
			if s.Responses != 1 || s.FailedResponses != 1 || s.SuccessfulResponses != 0 || s.CachedInputTokens != 10 || s.PendingRequests != 0 || len(o.line) != 0 || len(o.eventData) != 0 {
				t.Fatalf("first terminal or retained data incorrect: %+v", s)
			}
		})
	}
}

func TestGatewayCacheResponseObserverRequiresDispatchedTerminal(t *testing.T) {
	for _, tail := range []string{
		`data: {"type":"response.completed","response":{}}`,
		"data: {\"type\":\"response.completed\",\"response\":{}}\n",
		"data: {\"type\":\"response.completed\"}\n\n",
		"data: [DONE]\n\n",
	} {
		d := cacheDiagnosticsForTest(t)
		id := observeCacheRequestForTest(t, d, "s", "t", `{}`)
		o := d.ResponseObserver(id)
		_, _ = o.Write([]byte(tail))
		if err := o.Finish(); err == nil {
			t.Fatalf("invalid terminal accepted: %q", tail)
		}
		_ = o.Finish()
		if s := d.Snapshot(); s.Responses != 0 || s.UnfinishedResponses != 1 || s.PendingRequests != 0 || s.CacheHitRatio != nil {
			t.Fatalf("unfinished stream became a cache miss: %+v", s)
		}
	}
}

func TestGatewayCacheResponseObserverLimitNeverBreaksTransport(t *testing.T) {
	d := cacheDiagnosticsForTest(t)
	id := observeCacheRequestForTest(t, d, "s", "t", `{}`)
	o := d.ResponseObserver(id)
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	for range maxBody/len(chunk) + 2 {
		if n, err := o.Write(chunk); n != len(chunk) || err != nil {
			t.Fatal("diagnostic limit leaked into transport")
		}
	}
	_, _ = o.Write([]byte("\n\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n"))
	if err := o.Finish(); err == nil || len(o.line) != 0 || len(o.eventData) != 0 {
		t.Fatal("oversized frame was not released")
	}
	if s := d.Snapshot(); s.Responses != 0 || s.UnfinishedResponses != 1 || s.PendingRequests != 0 {
		t.Fatalf("oversized stream was miscounted: %+v", s)
	}
}
