package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type bpsOpaqueOwnerLiveTrace struct {
	terminal                             chan []byte
	requestHash, responseHash, ownerHash string
	http                                 int
	originalInputPreserved               bool
}

// The observer compares against the bytes actually sent by this client. JSON
// serialization may compact/escape upstream RawMessage bytes without changing
// any decoded ciphertext. This check keeps both boundaries independently strict.
func bpsOpaqueOwnerClientInput(body []byte, original []json.RawMessage) ([]json.RawMessage, error) {
	var wire struct {
		Input []json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &wire) != nil || len(wire.Input) != len(original) {
		return nil, errors.New("client_input_shape")
	}
	for i, item := range original {
		var before, after map[string]json.RawMessage
		if json.Unmarshal(item, &before) != nil || json.Unmarshal(wire.Input[i], &after) != nil {
			return nil, errors.New("client_item_shape")
		}
		if value, exists := before["encrypted_content"]; exists {
			var a, b string
			if json.Unmarshal(value, &a) != nil || json.Unmarshal(after["encrypted_content"], &b) != nil || a == "" || a != b {
				return nil, errors.New("client_cipher_changed")
			}
		}
	}
	return wire.Input, nil
}

func bpsOpaqueOwnerCompactionCipherMatches(upstream, public []json.RawMessage) bool {
	if len(upstream) != len(public) {
		return false
	}
	found := false
	for i := range upstream {
		var a, b map[string]json.RawMessage
		if json.Unmarshal(upstream[i], &a) != nil || json.Unmarshal(public[i], &b) != nil || bpsGatewayString(a, "type") != bpsGatewayString(b, "type") || bpsGatewayString(a, "id") != bpsGatewayString(b, "id") {
			return false
		}
		if value, exists := a["encrypted_content"]; exists {
			var before, after string
			if json.Unmarshal(value, &before) != nil || json.Unmarshal(b["encrypted_content"], &after) != nil || before == "" || before != after {
				return false
			}
			found = true
		} else if _, exists := b["encrypted_content"]; exists {
			return false
		}
	}
	return found
}

// Exactly one native compact POST and one Responses POST, both using account A.
// Session routing is deliberately discarded between them, and B is then given
// higher priority. Only the new same-key cipher-owner index can recover A.
func TestBPSLiveOpaqueOwnerRecovery(t *testing.T) {
	file, binary, work := os.Getenv("BPS_LIVE_ACCOUNTS_FILE"), os.Getenv("BPS_LIVE_CODEX_BINARY"), os.Getenv("BPS_LIVE_WORK_ROOT")
	if file == "" || binary == "" || work == "" {
		t.Skip("explicit server-only two-account live fixture required")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("live credential fixture unavailable")
	}
	var fixture struct {
		Accounts []bpsLiveAccount `json:"accounts"`
	}
	if json.Unmarshal(raw, &fixture) != nil || len(fixture.Accounts) < 2 {
		t.Fatal("two live accounts required")
	}
	a, b := fixture.Accounts[0], fixture.Accounts[1]
	if a.AccessToken == "" || b.AccessToken == "" || a.AccessToken == b.AccessToken || a.AccountID == "" || b.AccountID == "" {
		t.Fatal("two distinct usable credential fixtures required")
	}
	digest := func(value []byte) string { sum := sha256.Sum256(value); return hex.EncodeToString(sum[:]) }
	var evidence []map[string]any
	defer func() {
		if path := os.Getenv("BPS_LIVE_EVIDENCE_FILE"); path != "" {
			if bpsLiveWriteEvidence(path, "opaque-owner-recovery", !t.Failed(), evidence) != nil {
				t.Error("opaque owner evidence write failed")
			}
		}
	}()
	h := newBPSLiveHarness(t, binary, work, a, []string{a.AccessToken, a.AccountID, a.Alias, b.AccessToken, b.AccountID, b.Alias}, &evidence)
	p := h.recorder.p
	p.BPS.SearchDisabled = true
	credential, _ := json.Marshal(map[string]any{"access_token": b.AccessToken, "account_id": b.AccountID, "expires_at": time.Now().Add(time.Hour).Unix()})
	if err = p.Store.ImportWith("owner-b", credential, AccountDefaults{Concurrency: 4, Priority: 1, Weight: 1}); err != nil {
		t.Fatal("secondary fixture import failed")
	}
	for _, name := range []string{"test", "owner-b"} {
		if err = p.Store.UpdateAccount(name, func(account *Account) error {
			account.RefreshToken = ""
			account.ExpiresAt = time.Now().Add(time.Hour).Unix()
			account.Enabled = true
			account.Identity = nil
			identity, e := parseManagedIdentity(nil, nil, *account)
			if e != nil {
				return e
			}
			account.Identity = identity
			if name == "test" {
				account.Priority = 100
			} else {
				account.Priority = 1
			}
			return nil
		}); err != nil {
			t.Fatal("fixture full owner initialization failed")
		}
	}
	accountA, err := p.Store.Get("test")
	if err != nil || !gatewayCompleteOwner(accountA) {
		t.Fatal("A has no complete owner")
	}
	accountB, err := p.Store.Get("owner-b")
	if err != nil || !gatewayCompleteOwner(accountB) || gatewayCacheOwnerNamespace(accountA) == gatewayCacheOwnerNamespace(accountB) {
		t.Fatal("B owner must be complete and distinct")
	}
	p.invalidateAccounts()
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	if !defaultSettings().CacheCreationAsReadEnabled {
		t.Fatal("production billing default differs")
	}
	settings.CacheCreationAsReadEnabled = true
	settings.AutomaticTokenRefreshEnabled = false
	settings.Upstream401RecoveryEnabled = false
	settings.RateLimitCooldownEnabled = false
	if err = p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	base := p.Client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	traces := [2]*bpsOpaqueOwnerLiveTrace{{terminal: make(chan []byte, 1)}, {terminal: make(chan []byte, 1)}}
	var mu sync.Mutex
	posts, step, blockedB := 0, 0, 0
	wrapperCalls := 0
	wrapperRejection := ""
	var expectedResume []json.RawMessage
	reject := func(class string) (*http.Response, error) {
		mu.Lock()
		wrapperRejection = class
		mu.Unlock()
		return nil, errors.New("opaque owner fixture: " + class)
	}
	p.Client.Transport = bpsCacheLiveTransport(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		wrapperCalls++
		index := posts
		activeStep := step
		mu.Unlock()
		suffix := "/responses/compact"
		if activeStep == 1 {
			suffix = "/responses"
		}
		if request.Method != "POST" || !strings.HasSuffix(request.URL.Path, suffix) || index != activeStep || index >= 2 {
			return reject("dispatch_order_or_route")
		}
		if request.Header.Get("Authorization") != "Bearer "+a.AccessToken || request.Header.Get("ChatGPT-Account-ID") != a.AccountID || request.Header.Get("X-OpenAI-Account-Id") != a.AccountID {
			mu.Lock()
			blockedB++
			mu.Unlock()
			return reject("owner_headers_mismatch")
		}
		if request.GetBody == nil {
			return reject("body_unavailable")
		}
		reader, e := request.GetBody()
		if e != nil {
			return reject("body_reader_failed")
		}
		body, e := io.ReadAll(reader)
		reader.Close()
		if e != nil {
			return reject("body_read_failed")
		}
		var wire map[string]json.RawMessage
		var input []json.RawMessage
		if json.Unmarshal(body, &wire) != nil || json.Unmarshal(wire["input"], &input) != nil {
			return reject("outbound_input_shape")
		}
		if activeStep == 1 {
			if len(input) < len(expectedResume) {
				return reject("input_truncated")
			}
			for i, item := range expectedResume {
				if !bytes.Equal(input[i], item) {
					return reject("serialized_client_input_changed")
				}
			}
			traces[index].originalInputPreserved = true
		}
		trace := traces[index]
		trace.requestHash = digest(body)
		trace.ownerHash = digest([]byte(gatewayAccountRef(accountA)))
		mu.Lock()
		posts++
		mu.Unlock()
		// No alternate account dispatch or retry is permitted after any HTTP error.
		request.GetBody = nil
		response, e := base.RoundTrip(request)
		if e == nil {
			trace.http = response.StatusCode
			if response.Body != nil {
				response.Body = &bpsCacheLiveResponseBody{ReadCloser: response.Body, complete: func(value []byte) {
					trace.responseHash = digest(value)
					trace.terminal <- bytes.Clone(bpsStructuredLiveTerminal(value))
				}}
			}
		}
		return response, e
	})
	message := func(role, text string) json.RawMessage {
		data, _ := json.Marshal(map[string]any{"type": "message", "role": role, "content": []map[string]string{{"type": "input_text", "text": text}}})
		return data
	}
	marker := "OPAQUE_OWNER_" + strings.ReplaceAll(bpsUUID(), "-", "")
	padding := strings.Repeat("Synthetic background reference: alpha beta gamma delta. This reference requires no tools or external resources. Preserve the remembered marker for the next turn.\n", 400)
	input := []json.RawMessage{message("user", "Remember this exact marker for a later question: "+marker+". Preserve it when compressing the conversation."), message("user", padding), message("user", "The next turn will ask for the exact remembered marker. Preserve it faithfully in the compacted state.")}
	run := func(index int, path, session string, history []json.RawMessage) bpsCacheBillingLiveResponse {
		t.Helper()
		mu.Lock()
		step = index
		mu.Unlock()
		body, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "reasoning": map[string]string{"effort": "xhigh"}, "stream": false, "input": history, "tools": []any{}, "tool_choice": "none"})
		if index == 1 {
			var clientErr error
			expectedResume, clientErr = bpsOpaqueOwnerClientInput(body, history)
			if clientErr != nil {
				t.Fatal("serialized client input/cipher invariant failed: " + clientErr.Error())
			}
		}
		request := proxyRequest("POST", path, string(body))
		request.Header.Set("Session-Id", session)
		out := httptest.NewRecorder()
		p.gatewayBPS(out, request)
		row := map[string]any{"case": index + 1, "http": out.Code, "passed": false, "requested_model": "gpt-6-astra", "requested_effort": "xhigh"}
		evidence = append(evidence, row)
		record, exists := p.Metrics.LastFor("test")
		mu.Lock()
		row["wrapper_calls"], row["actual_posts"], row["blocked_non_owner"], row["wrapper_rejection"] = wrapperCalls, posts, blockedB, wrapperRejection
		mu.Unlock()
		row["observed_upstream_http"], row["record_exists"], row["record_error_source"], row["record_error_phase"] = traces[index].http, exists, record.ErrorSource, record.ErrorPhase
		row["record_attempts"], row["final_upstream_error_code"], row["final_upstream_error_source"] = len(record.Attempts), record.FinalUpstreamErrorCode, record.FinalUpstreamErrorSource
		row["client_cipher_matches_original"], row["outbound_input_matches_client"] = index == 1, traces[index].originalInputPreserved
		if out.Code != 200 || !exists || record.Status != "ok" {
			row["error"] = bpsLiveDiagnosticError(record.Error)
			t.Fatalf("opaque owner live request failed case%d HTTP%d", index+1, out.Code)
		}
		if record.AccountID != accountA.ID {
			t.Fatal("recorded account differs from the actual original owner")
		}
		var upstreamRaw []byte
		select {
		case upstreamRaw = <-traces[index].terminal:
		case <-time.After(5 * time.Second):
			t.Fatal("upstream completed read observation missing")
		}
		var upstream, public bpsCacheBillingLiveResponse
		if json.Unmarshal(upstreamRaw, &upstream) != nil || json.Unmarshal(out.Body.Bytes(), &public) != nil || upstream.ID == "" || public.ID != upstream.ID || record.ResponseID != upstream.ID {
			t.Fatal("actual/public/record response identity mismatch")
		}
		if index == 0 {
			if public.Status != "" && public.Status != "completed" || !bpsHasCompaction(public.Output) {
				t.Fatal("native compact produced no valid encrypted state")
			}
			if !bpsOpaqueOwnerCompactionCipherMatches(upstream.Output, public.Output) {
				t.Fatal("native compact upstream/public cipher identity changed")
			}
			row["compact_upstream_public_cipher_matches"] = true
		} else if upstream.Status != "completed" || public.Status != "completed" {
			t.Fatal("resumed response did not complete")
		}
		u, v := upstream.Usage, public.Usage
		if u.Input == nil || u.Output == nil || u.Details.Read == nil || v.Input == nil || v.Output == nil || v.Details.Read == nil || record.UsageKnown == nil || !*record.UsageKnown || record.CachedTokensKnown == nil || !*record.CachedTokensKnown {
			t.Fatal("actual compact/generation usage and cache-read knowledge missing")
		}
		if *u.Input < 0 || *u.Output < 0 || *u.Details.Read < 0 || *u.Details.Read > *u.Input {
			t.Fatal("invalid upstream usage counts")
		}
		if record.InputTokens != *u.Input || record.OutputTokens != *u.Output || record.CachedTokens != *u.Details.Read || !reflect.DeepEqual(record.CacheWriteTokens, u.Details.Write) {
			t.Fatal("internal actual usage/read/write differs from upstream")
		}
		if *v.Input != *u.Input || *v.Output != *u.Output {
			t.Fatal("public input/output counts changed")
		}
		if !reflect.DeepEqual(u.Total, v.Total) || public.Model != upstream.Model || record.ReturnedModel != upstream.Model {
			t.Fatal("public total tokens or observed returned model changed")
		}
		if u.Details.Write != nil {
			if *u.Details.Write < 0 || *u.Details.Write > *u.Input-*u.Details.Read || v.Details.Write == nil || *v.Details.Write != 0 || *v.Details.Read != *u.Details.Read+*u.Details.Write {
				t.Fatal("public default cache projection did not preserve actual sum/zero")
			}
		} else if v.Details.Write != nil || *v.Details.Read != *u.Details.Read {
			t.Fatal("unknown upstream cache creation was invented")
		}
		if index == 1 && (upstream.Model != "gpt-6-astra" || public.Model != upstream.Model || record.ReturnedModel != upstream.Model || record.SentReasoningEffort != "xhigh") {
			t.Fatal("resumed model/effort differed")
		}
		mu.Lock()
		count := posts
		wrongOwner := blockedB
		mu.Unlock()
		if count != index+1 || wrongOwner != 0 || traces[index].http != 200 {
			t.Fatal("unexpected extra dispatch or wrong owner")
		}
		row["actual_input"], row["actual_output"], row["actual_cache_read"], row["actual_cache_write"] = *u.Input, *u.Output, *u.Details.Read, u.Details.Write
		row["public_cache_read"], row["public_cache_write"], row["internal_usage_matches_upstream"] = *v.Details.Read, v.Details.Write, true
		row["public_cache_matches_default"], row["account_owner_sha256"], row["request_sha256"], row["response_sha256"] = true, traces[index].ownerHash, traces[index].requestHash, traces[index].responseHash
		row["actual_posts"], row["passed"] = count, true
		return public
	}
	compact := run(0, "/v1/responses/compact", "opaque-owner-original-"+bpsUUID(), input)
	var cipherHashes []string
	for _, item := range compact.Output {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(item, &fields)
		if bpsGatewayString(fields, "type") == "compaction" {
			cipher := bpsGatewayString(fields, "encrypted_content")
			if cipher != "" {
				cipherHashes = append(cipherHashes, digest([]byte(cipher)))
			}
		}
	}
	if len(cipherHashes) == 0 {
		t.Fatal("successful compact did not return actual encrypted_content")
	}
	// Only this private file-backed fixture map is cleared; owner index/history
	// and original output bytes remain intact. No production Redis is involved.
	p.sessionMu.Lock()
	p.sessionRoutes = map[string]gatewaySessionRoute{}
	p.sessionMu.Unlock()
	if err = p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 1; return nil }); err != nil {
		t.Fatal("fixture priority update failed")
	}
	if err = p.Store.UpdateAccount("owner-b", func(a *Account) error { a.Priority = 100; return nil }); err != nil {
		t.Fatal("fixture priority update failed")
	}
	p.invalidateAccounts()
	currentA, errA := p.Store.Get("test")
	currentB, errB := p.Store.Get("owner-b")
	if errA != nil || errB != nil || currentB.Priority <= currentA.Priority || !currentB.Enabled {
		t.Fatal("alternate fixture account is not enabled at higher priority")
	}
	expectedResume = append([]json.RawMessage(nil), compact.Output...)
	expectedResume = append(expectedResume, message("user", "Return only the exact marker from the compacted conversation. Do not invent a new marker and do not call tools."))
	resumed := run(1, "/v1/responses", "opaque-owner-new-session-"+bpsUUID(), expectedResume)
	if strings.TrimSpace(bpsStructuredLiveText(resumed.Output)) != marker {
		t.Fatal("real compacted marker was not recovered")
	}
	if !traces[1].originalInputPreserved || traces[0].ownerHash != traces[1].ownerHash {
		t.Fatal("original ciphertext/history or owner did not survive new session")
	}
	if last, exists := p.Metrics.LastFor("owner-b"); exists && last.ID > 0 {
		t.Fatal("higher-priority alternate account was used")
	}
	for _, account := range []Account{currentA, currentB} {
		if p.inFlight(gatewayAdmissionKey(account)) != 0 {
			t.Fatal("live owner recovery leaked account concurrency")
		}
	}
	evidence[1]["session_binding_cleared_fixture_only"], evidence[1]["alternate_priority_higher"], evidence[1]["original_input_preserved"], evidence[1]["marker_recovered"] = true, true, true, true
	evidence[1]["cipher_sha256"], evidence[1]["alternate_account_calls"], evidence[1]["same_api_key"] = cipherHashes, 0, true
	encoded, _ := json.Marshal(evidence)
	t.Log(string(encoded))
}
