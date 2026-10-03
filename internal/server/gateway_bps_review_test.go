package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBPSReviewParallelFalseRejectsSecondCallBeforeDelivery(t *testing.T) {
	callA := `{"type":"function_call","id":"fc_a","call_id":"call_a","name":"echo","arguments":"{}"}`
	callB := `{"type":"function_call","id":"fc_b","call_id":"call_b","name":"echo","arguments":"{}"}`
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":%s}\n\n", callA)
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":%s}\n\n", callB)
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "resp_parallel_policy", "["+callA+","+callB+"]"))
	})
	w := httptest.NewRecorder()
	func() {
		defer func() {
			if caught := recover(); caught != nil && caught != http.ErrAbortHandler {
				panic(caught)
			}
		}()
		p.ServeHTTP(w, proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","stream":true,"parallel_tool_calls":false,"tools":[{"type":"function","name":"echo","parameters":{"type":"object"}}],"input":[]}`))
	}()
	if strings.Contains(w.Body.String(), `"call_id":"call_b"`) {
		t.Fatalf("parallel_tool_calls=false second executable call reached the client before terminal rejection: %s", w.Body.String())
	}
}

func TestBPSReviewIncrementalHistoryKeepsPromptCacheIdentity(t *testing.T) {
	keys := make(chan string, 2)
	var calls atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]json.RawMessage
		_ = json.Unmarshal(raw, &body)
		keys <- bpsGatewayString(body, "prompt_cache_key")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", fmt.Sprintf("resp_cache_%d", calls.Add(1)), "[]"))
	})
	for _, body := range []string{
		`{"model":"gpt-6-astra","stream":false,"prompt_cache_key":"client-original-cache","input":[]}`,
		`{"model":"gpt-6-astra","stream":false,"previous_response_id":"resp_cache_1","input":[]}`,
	} {
		req := proxyRequest(http.MethodPost, "/v1/responses", body)
		req.Header.Set("Session-Id", "review-cache-session")
		w := httptest.NewRecorder()
		p.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request failed: HTTP %d %s", w.Code, w.Body.String())
		}
	}
	first, second := <-keys, <-keys
	if first == "" || first == "client-original-cache" || second != first {
		t.Fatalf("incremental history re-scoped the prior cache identity: first=%q second=%q", first, second)
	}
}

func TestBPSReviewCredentialHeaderRewriteClearsRetiredIdentity(t *testing.T) {
	header := http.Header{
		"Authorization":            {"Bearer old-token"},
		"Chatgpt-Account-Id":       {"old-workspace"},
		"X-Openai-Account-Id":      {"old-workspace"},
		"X-Openai-Account-User-Id": {"old-workspace-user"},
	}
	applyBPSHeaders(header, Account{AccessToken: "new-token"})
	if header.Get("Authorization") != "Bearer new-token" {
		t.Fatal("updated bearer token was not installed")
	}
	for _, key := range []string{"ChatGPT-Account-ID", "X-OpenAI-Account-Id", "X-OpenAI-Account-User-Id"} {
		if header.Get(key) != "" {
			t.Fatalf("retired credential identity survived header rebuild: %s", key)
		}
	}
}

func TestBPSReviewWebSocketSettingsRemainIndependentAndRefreshPerTurn(t *testing.T) {
	var calls atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", fmt.Sprintf("resp_settings_%d", calls.Add(1)), "[]"))
	})
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.HTTPStreamEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(p)
	defer server.Close()
	conn, _ := gatewayWSDial(t, server.URL, "review-settings")
	bpsGatewayWSSend(t, conn, `{"type":"response.create","model":"gpt-6-astra","input":[]}`)
	first := bpsGatewayWSUntilTerminal(t, conn, "")
	if bpsGatewayString(first, "type") != "response.completed" || calls.Load() != 1 {
		t.Fatalf("enabled WebSocket request was incorrectly gated by disabled HTTP SSE: %s", first)
	}
	settings, _ = p.Settings.Get()
	settings.HTTPStreamEnabled = true
	settings.WebsocketEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	bpsGatewayWSSend(t, conn, `{"type":"response.create","model":"gpt-6-astra","input":[]}`)
	next := bpsGatewayWSUntilTerminal(t, conn, "")
	if bpsGatewayString(next, "type") != "error" || calls.Load() != 1 {
		t.Fatalf("existing socket bypassed newly disabled WebSocket policy: %s calls=%d", next, calls.Load())
	}
}

func TestBPSReviewHTTPStreamUsesBoundedDownstreamWrites(t *testing.T) {
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "resp_deadline", "[]"))
	})
	w := &gatewayStreamDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	p.ServeHTTP(w, proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","stream":true,"input":[]}`))
	if w.Code != http.StatusOK || w.set == 0 || !w.last.IsZero() {
		t.Fatalf("BPS SSE did not bound and then clear downstream writes: HTTP %d deadlines=%d last=%v", w.Code, w.set, w.last)
	}
}

func TestBPSReviewHistoryRestoresOwnerAfterRoutingExpiry(t *testing.T) {
	var calls atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if got := r.Header.Get("Authorization"); got != "Bearer test-upstream-token" {
			t.Errorf("saved conversation selected another account: %s", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", fmt.Sprintf("resp_owner_ttl_%d", calls.Add(1)), "[]"))
	})
	send := func(body string) *httptest.ResponseRecorder {
		r := proxyRequest(http.MethodPost, "/v1/responses", body)
		r.Header.Set("Session-Id", "history-outlives-routing")
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		return w
	}
	if first := send(`{"model":"gpt-6-astra","stream":false,"input":[]}`); first.Code != http.StatusOK {
		t.Fatalf("cannot establish history: %d %s", first.Code, first.Body.String())
	}
	if err := p.Store.ImportWith("alternate", []byte(`{"access_token":"alternate-token"}`), AccountDefaults{Concurrency: 1}); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("alternate", func(a *Account) error { a.Priority = 100; return nil }); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	// Deterministically model the shorter routing TTL expiring while the
	// independently stored one-hour response history remains available.
	p.sessionMu.Lock()
	for key, route := range p.sessionRoutes {
		route.Expires = time.Now().Add(-time.Second)
		p.sessionRoutes[key] = route
	}
	p.sessionMu.Unlock()
	next := send(`{"model":"gpt-6-astra","stream":false,"previous_response_id":"resp_owner_ttl_1","input":[]}`)
	if next.Code != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("available history did not restore its owner after routing TTL: HTTP %d calls=%d %s", next.Code, calls.Load(), next.Body.String())
	}
}

// A delayed cancellation identifies the old response even when a stream name
// has since been reused. It must never interrupt the newer response.
func TestBPSReviewStaleCancelDoesNotCancelReusedStream(t *testing.T) {
	var calls atomic.Int32
	secondCanceled := make(chan struct{})
	releaseSecond := make(chan struct{})
	defer close(releaseSecond)
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "resp_old", "[]"))
			return
		}
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp_new","status":"in_progress","output":[]}}`+"\n\n")
		_ = http.NewResponseController(w).Flush()
		select {
		case <-r.Context().Done():
			close(secondCanceled)
		case <-releaseSecond:
			_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "resp_new", "[]"))
		}
	})
	server := httptest.NewServer(p)
	defer server.Close()
	conn, _ := gatewayWSDial(t, server.URL, "review-stale-cancel")
	bpsGatewayWSSend(t, conn, `{"type":"response.create","stream_id":"reused","model":"gpt-6-astra","input":[]}`)
	first := bpsGatewayWSUntilTerminal(t, conn, "reused")
	if bpsGatewayWSResponseID(t, first) != "resp_old" {
		t.Fatalf("unexpected initial completion: %s", first)
	}
	bpsGatewayWSSend(t, conn, `{"type":"response.create","stream_id":"reused","model":"gpt-6-astra","input":[]}`)
	created := bpsGatewayWSReadEvent(t, conn)
	if bpsGatewayWSResponseID(t, created) != "resp_new" {
		t.Fatalf("new response did not start: %s", created)
	}
	bpsGatewayWSSend(t, conn, `{"type":"response.cancel","stream_id":"reused","response_id":"resp_old"}`)
	select {
	case <-secondCanceled:
		t.Fatal("stale response cancellation interrupted the new response on the reused stream")
	case <-time.After(250 * time.Millisecond):
	}
}

func TestBPSReviewLegacyCompactFailureIsRejected(t *testing.T) {
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"compact_failed","status":"failed","error":{"message":"upstream failed"},"output":[{"type":"compaction","encrypted_content":"partial-state"}]}`)
	})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest(http.MethodPost, "/v1/responses/compact", `{"model":"gpt-6-astra","input":[]}`))
	if w.Code < 400 {
		t.Fatalf("failed upstream compact response was accepted as success: HTTP %d %s", w.Code, w.Body.String())
	}
}

// Successful headers are not enough: disabling the selected account while its
// compact request is in flight must be rechecked before encrypted state returns.
func TestBPSReviewLegacyCompactRechecksAccountAuthorization(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(arrived)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"compact_revoked","output":[{"type":"compaction","encrypted_content":"owner-private-state"}]}`)
	})
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.ServeHTTP(w, proxyRequest(http.MethodPost, "/v1/responses/compact", `{"model":"gpt-6-astra","input":[]}`))
	}()
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("compact request did not reach fixture")
	}
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Enabled = false; return nil }); err != nil {
		close(release)
		t.Fatal(err)
	}
	p.invalidateAccounts()
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("compact request failed to terminate")
	}
	if w.Code < 400 {
		t.Fatalf("revoked account compact state was delivered as success: HTTP %d %s", w.Code, w.Body.String())
	}
}
