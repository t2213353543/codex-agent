package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"codex-agent/internal/bps"
)

type bpsStructuredLiveTrace struct {
	Request, Response                                          json.RawMessage
	Path                                                       string
	CanonicalError                                             error
	DoneReasoningCipherPresent, RawTerminalReasoningCipherDiff bool
}

// The upstream >2^53 enum rounding boundary is preserved separately in
// tools/bps_structured_numeric_probe.go; this suite uses a supported integer
// to isolate the session/formatting protocol from that known model limitation.
// Six generation/compact requests, all synthetic, through the candidate real
// gateway. No production settings, credentials or customer history are changed.
// Only the normal explicit server-side live fixture enables this test.
func TestBPSLiveStructuredOutput(t *testing.T) {
	file, binary, work := os.Getenv("BPS_LIVE_ACCOUNTS_FILE"), os.Getenv("BPS_LIVE_CODEX_BINARY"), os.Getenv("BPS_LIVE_WORK_ROOT")
	if file == "" || binary == "" || work == "" {
		t.Skip("explicit server-only live fixture required")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("live fixture unavailable")
	}
	var fixture struct {
		Accounts []bpsLiveAccount `json:"accounts"`
	}
	if json.Unmarshal(raw, &fixture) != nil || len(fixture.Accounts) == 0 {
		t.Fatal("invalid live fixture")
	}
	a := fixture.Accounts[0]
	var evidence []map[string]any
	defer func() {
		if path := os.Getenv("BPS_LIVE_EVIDENCE_FILE"); path != "" {
			if err := bpsLiveWriteEvidence(path, "bps-structured-output", !t.Failed(), evidence); err != nil {
				t.Error("structured evidence write failed")
			}
		}
	}()
	h := newBPSLiveHarness(t, binary, work, a, []string{a.AccessToken, a.AccountID}, &evidence)
	p := h.recorder.p
	p.BPS.SearchDisabled = true
	base := p.Client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	var mu sync.Mutex
	var traces []*bpsStructuredLiveTrace
	p.Client.Transport = bpsCacheLiveTransport(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost {
			return base.RoundTrip(request)
		}
		mu.Lock()
		if len(traces) >= 6 {
			mu.Unlock()
			return nil, errors.New("structured live six-request boundary")
		}
		mu.Unlock()
		payload, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		_ = request.Body.Close()
		request.Body, request.GetBody = io.NopCloser(bytes.NewReader(payload)), nil
		trace := &bpsStructuredLiveTrace{Request: bytes.Clone(payload), Path: request.URL.Path}
		mu.Lock()
		traces = append(traces, trace)
		mu.Unlock()
		response, err := base.RoundTrip(request)
		if err == nil && response.Body != nil {
			response.Body = &bpsCacheLiveResponseBody{ReadCloser: response.Body, complete: func(wire []byte) {
				canonical, doneCipher, terminalDiff, parseErr := bpsStructuredLiveCanonicalUpstream(wire, strings.HasSuffix(request.URL.Path, "/responses/compact"))
				mu.Lock()
				trace.Response = bytes.Clone(canonical)
				trace.CanonicalError, trace.DoneReasoningCipherPresent, trace.RawTerminalReasoningCipherDiff = parseErr, doneCipher, terminalDiff
				mu.Unlock()
			}}
		}
		return response, err
	})
	formatRaw := json.RawMessage(`{"format":{"type":"json_schema","name":"live_nested_answer","strict":true,"schema":{"type":"object","properties":{"marker":{"type":"string"},"data":{"type":"object","properties":{"answer":{"type":"string","enum":["SCHEMA_OK"]},"large":{"type":"integer","enum":[2147483647]}},"required":["answer","large"],"additionalProperties":false}},"required":["marker","data"],"additionalProperties":false}}}`)
	format, err := bps.ParseStructuredFormat(formatRaw)
	if err != nil || format == nil {
		t.Fatal("actual structured formatter unavailable")
	}
	objectFormatRaw := json.RawMessage(`{"format":{"type":"json_object"}}`)
	objectFormat, err := bps.ParseStructuredFormat(objectFormatRaw)
	if err != nil || objectFormat == nil {
		t.Fatal("actual json_object formatter unavailable")
	}
	marker := "live-marker-" + bpsUUID()
	session := "live-structured-" + bpsUUID()
	message := func(text string) json.RawMessage {
		v, _ := json.Marshal(map[string]any{"type": "message", "role": "user", "content": text})
		return v
	}
	newBody := func(input []json.RawMessage) map[string]any {
		return map[string]any{"model": "gpt-6-astra", "reasoning": map[string]string{"effort": "xhigh"}, "stream": false, "tool_choice": "none", "tools": []any{}, "input": input}
	}
	var encryptedSeen bool
	run := func(name, path string, body map[string]any, expected *bps.StructuredFormat) (json.RawMessage, []json.RawMessage, string) {
		t.Helper()
		row := map[string]any{"kind": "case", "case": name, "passed": false}
		evidence = append(evidence, row)
		payload, e := json.Marshal(body)
		if e != nil {
			t.Fatal("live request encoding failed")
		}
		request := proxyRequest(http.MethodPost, path, string(payload))
		request.Header.Set("Session-Id", session)
		response := httptest.NewRecorder()
		aborted := false
		func() {
			defer func() {
				if failure := recover(); failure != nil {
					if failure == http.ErrAbortHandler {
						aborted = true
					} else {
						panic(failure)
					}
				}
			}()
			p.gatewayBPS(response, request)
		}()
		row["http"], row["aborted"] = response.Code, aborted
		if response.Code != http.StatusOK || aborted {
			if record, ok := p.Metrics.LastFor("test"); ok {
				row["error_summary"], row["error_source"], row["error_phase"] = record.Error, record.ErrorSource, record.ErrorPhase
			}
			t.Fatalf("structured case %s failed HTTP=%d aborted=%t diagnostic=%s", name, response.Code, aborted, h.scrub(response.Body.String()))
		}
		wire := response.Body.Bytes()
		for _, forbidden := range []string{format.ToolName, objectFormat.ToolName, `"function_call"`, `"function_call_output"`, `"additional_tools"`} {
			if bytes.Contains(wire, []byte(forbidden)) {
				t.Fatalf("structured case %s leaked internal tool protocol", name)
			}
		}
		terminal := bpsStructuredLiveTerminal(wire)
		var result struct {
			ID, Status, Object string
			Output             []json.RawMessage `json:"output"`
		}
		if json.Unmarshal(terminal, &result) != nil || result.ID == "" {
			t.Fatalf("structured case %s missing terminal response", name)
		}
		compact := path == "/v1/responses/compact"
		if (!compact && result.Status != "completed") || (compact && (result.Object != "response.compaction" || !bpsHasCompaction(result.Output))) {
			t.Fatalf("structured case %s did not complete", name)
		}
		if stream, _ := body["stream"].(bool); stream {
			var delta strings.Builder
			for _, line := range bytes.Split(wire, []byte("\n")) {
				if !bytes.HasPrefix(line, []byte("data:")) {
					continue
				}
				var event struct{ Type, Delta string }
				_ = json.Unmarshal(bytes.TrimSpace(line[5:]), &event)
				if event.Type == "response.output_text.delta" {
					delta.WriteString(event.Delta)
				}
			}
			if delta.String() == "" || delta.String() != bpsStructuredLiveText(result.Output) {
				t.Fatal("structured SSE deltas differ from terminal text")
			}
		}
		mu.Lock()
		count := len(traces)
		var trace bpsStructuredLiveTrace
		if count > 0 {
			trace = *traces[count-1]
		}
		mu.Unlock()
		if count != len(evidence) {
			t.Fatal("structured case made unexpected upstream retries")
		}
		row["raw_done_reasoning_cipher_present"] = trace.DoneReasoningCipherPresent
		row["raw_terminal_reasoning_cipher_differs_from_canonical"] = trace.RawTerminalReasoningCipherDiff
		row["upstream_canonical_stream_valid"] = trace.CanonicalError == nil
		if trace.CanonicalError != nil {
			t.Fatalf("structured case %s failed ordinary upstream stream integrity verification", name)
		}
		var sent map[string]json.RawMessage
		if json.Unmarshal(trace.Request, &sent) != nil || trace.Response == nil {
			t.Fatal("actual upstream evidence missing")
		}
		if compact {
			if len(sent) != 3 || sent["model"] == nil || sent["input"] == nil || sent["metadata"] == nil || !strings.HasSuffix(trace.Path, "/responses/compact") {
				t.Fatal("structured format polluted compact three-field schema")
			}
		} else if expected != nil {
			if !bytes.Contains(sent["input"], []byte(expected.ToolName)) {
				t.Fatal("actual formatter declaration missing upstream")
			}
		} else if bytes.Contains(trace.Request, []byte(format.ToolName)) || bytes.Contains(trace.Request, []byte(objectFormat.ToolName)) {
			t.Fatal("text:null failed to clear inherited formatter")
		}
		var upstream struct {
			Output []json.RawMessage `json:"output"`
		}
		if json.Unmarshal(trace.Response, &upstream) != nil {
			t.Fatal("actual upstream terminal invalid")
		}
		encrypted, same := bpsStructuredLiveReasoningPreserved(upstream.Output, result.Output)
		if !same {
			t.Fatal("real opaque reasoning was changed by structured projection")
		}
		encryptedSeen = encryptedSeen || encrypted
		value := bpsStructuredLiveText(result.Output)
		if expected != nil {
			if _, err := expected.ValidateArguments(value); err != nil {
				t.Fatalf("structured case %s failed real schema validation", name)
			}
		}
		var down, up bpsCacheLiveTerminal
		if json.Unmarshal(terminal, &down) != nil || json.Unmarshal(trace.Response, &up) != nil {
			t.Fatal("usage decoding failed")
		}
		record, exists := p.Metrics.LastFor("test")
		if !exists || record.Status != "ok" || record.ResponseID != down.ID || record.Model != "gpt-6-astra" || record.SentModel != "gpt-6-astra" || record.ReturnedModel != down.Model || up.Model != down.Model || record.UsageKnown == nil || !*record.UsageKnown {
			t.Fatal("structured actual request/response model or usage observation missing")
		}
		if !compact && (down.Model != "gpt-6-astra" || record.SentReasoningEffort != "xhigh") {
			t.Fatal("structured generation did not use strongest requested model/effort")
		}
		if !bpsStructuredLiveUsageKnown(terminal) || !bpsStructuredLiveUsageKnown(trace.Response) || up.Usage.Input != down.Usage.Input || up.Usage.Output != down.Usage.Output || record.InputTokens != down.Usage.Input || record.OutputTokens != down.Usage.Output || (!compact && down.Usage.Input <= 0) {
			t.Fatal("structured usage changed between upstream, response and record")
		}
		if up.Usage.Details.Cached == nil || down.Usage.Details.Cached == nil || record.CachedTokensKnown == nil || !*record.CachedTokensKnown || *up.Usage.Details.Cached != *down.Usage.Details.Cached || record.CachedTokens != *down.Usage.Details.Cached {
			t.Fatal("structured cache read observation is missing or changed")
		}
		if (up.Usage.Details.Write == nil) != (down.Usage.Details.Write == nil) || (down.Usage.Details.Write == nil) != (record.CacheWriteTokens == nil) {
			t.Fatal("structured unknown cache creation counter was changed")
		}
		if up.Usage.Details.Write != nil && (*up.Usage.Details.Write != *down.Usage.Details.Write || *record.CacheWriteTokens != *down.Usage.Details.Write) {
			t.Fatal("structured cache creation was confused with cache reads")
		}
		row["passed"], row["completed"], row["encrypted_reasoning_present"], row["opaque_reasoning_preserved"] = true, true, encrypted, same
		row["input_tokens"], row["output_tokens"], row["cache_read_tokens"], row["cache_write_tokens"] = record.InputTokens, record.OutputTokens, record.CachedTokens, record.CacheWriteTokens
		row["cache_read_write_matches_upstream"], row["returned_model"], row["sent_effort"], row["latency_ms"] = true, record.ReturnedModel, record.SentReasoningEffort, record.LatencyMs
		var cacheWrite any
		if record.CacheWriteTokens != nil {
			cacheWrite = *record.CacheWriteTokens
		}
		t.Logf("structured_live case=%s HTTP=200 completed=true cache_read=%d cache_write=%v encrypted_reasoning=%t", name, record.CachedTokens, cacheWrite, encrypted)
		return terminal, result.Output, value
	}
	validateAnswer := func(value string) {
		t.Helper()
		var answer struct {
			Marker string `json:"marker"`
			Data   struct {
				Answer string      `json:"answer"`
				Large  json.Number `json:"large"`
			} `json:"data"`
		}
		matched := json.Unmarshal([]byte(value), &answer) == nil && answer.Marker == marker && answer.Data.Answer == "SCHEMA_OK" && answer.Data.Large.String() == "2147483647"
		evidence[len(evidence)-1]["marker_and_nested_values_match"] = matched
		if !matched {
			evidence[len(evidence)-1]["passed"] = false
			t.Fatal("actual structured answer lost remembered marker, nested value or exact integer")
		}
	}
	responseID := func(terminal json.RawMessage) string {
		var id struct{ ID string }
		_ = json.Unmarshal(terminal, &id)
		return id.ID
	}
	firstUser := message("Remember this exact marker for later turns: " + marker + ". Produce an object with that marker and nested data containing answer SCHEMA_OK and integer large=2147483647. Use the requested response format.")
	body := newBody([]json.RawMessage{firstUser})
	body["text"] = formatRaw
	first, outputOne, value := run("strict_nested_json", "/v1/responses", body, format)
	validateAnswer(value)
	if present, _ := bpsStructuredLiveReasoningPreserved(outputOne, outputOne); !present {
		evidence[len(evidence)-1]["passed"] = false
		t.Fatal("first structured turn produced no real encrypted reasoning; opaque continuation cannot be proven")
	}
	secondUser := message("Return exactly the same marker and nested answer and integer from your prior JSON response. Do not invent a new marker.")
	body = newBody([]json.RawMessage{secondUser})
	body["stream"], body["previous_response_id"] = true, responseID(first)
	_, outputTwo, value := run("previous_response_sse_inherits_schema", "/v1/responses", body, format)
	validateAnswer(value)
	history := append([]json.RawMessage{firstUser}, outputOne...)
	history = append(history, secondUser)
	history = append(history, outputTwo...)
	body = newBody(history)
	body["text"] = formatRaw
	_, compactOutput, _ := run("public_history_schema_compact", "/v1/responses/compact", body, nil)
	resumed := append([]json.RawMessage(nil), compactOutput...)
	resumed = append(resumed, message("From the compacted conversation, return the same remembered marker and nested answer and integer. Do not invent values."))
	body = newBody(resumed)
	body["text"] = formatRaw
	fourth, _, value := run("compact_resume_strict_json", "/v1/responses", body, format)
	validateAnswer(value)
	body = newBody([]json.RawMessage{message("Reply exactly FORMAT_CLEARED as plain text, without JSON or tools.")})
	body["previous_response_id"], body["text"] = responseID(fourth), nil
	_, _, value = run("text_null_clears_inherited_schema", "/v1/responses", body, nil)
	if strings.TrimSpace(value) != "FORMAT_CLEARED" {
		t.Fatal("cleared text format did not return requested plain text")
	}
	session = "live-json-object-" + bpsUUID()
	property := "free_" + strings.ReplaceAll(bpsUUID(), "-", "")
	body = newBody([]json.RawMessage{message(fmt.Sprintf("Return a JSON object containing property %q with string value FREE_OK and property nested with object {\"kept\":true}. Return no other text.", property))})
	body["text"] = objectFormatRaw
	_, _, value = run("json_object_free_properties", "/v1/responses", body, objectFormat)
	var object map[string]json.RawMessage
	var free string
	var nested struct{ Kept bool }
	if json.Unmarshal([]byte(value), &object) != nil || json.Unmarshal(object[property], &free) != nil || free != "FREE_OK" || json.Unmarshal(object["nested"], &nested) != nil || !nested.Kept {
		t.Fatal("json_object suppressed free or nested properties")
	}
	if !encryptedSeen {
		t.Fatal("live case produced no real encrypted reasoning; opaque compatibility remains unverified")
	}
}

// Ordinary BPS streaming verifies done/terminal identity and preserves a done
// item's already-delivered cipher when the terminal only re-envelopes the same
// reasoning item. Structured projection must match this verified source, not a
// different raw terminal envelope. No formatter projection runs in this helper.
func bpsStructuredLiveCanonicalUpstream(wire []byte, compact bool) (json.RawMessage, bool, bool, error) {
	if compact {
		if !json.Valid(wire) {
			return nil, false, false, errors.New("invalid upstream compact JSON")
		}
		return bytes.Clone(wire), false, false, nil
	}
	doneCipher := false
	for _, line := range bytes.Split(wire, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		var event struct {
			Type string
			Item struct {
				Type   string
				Cipher string `json:"encrypted_content"`
			}
		}
		if json.Unmarshal(bytes.TrimSpace(line[5:]), &event) == nil && event.Type == "response.output_item.done" && event.Item.Type == "reasoning" && event.Item.Cipher != "" {
			doneCipher = true
		}
	}
	result, err := bps.Stream(context.Background(), io.Discard, bytes.NewReader(wire), bps.StreamOptions{Unbounded: true})
	if err != nil {
		return nil, doneCipher, false, err
	}
	if !result.Completed || !result.HistoryComplete {
		return nil, doneCipher, false, errors.New("upstream canonical stream incomplete")
	}
	var rawTerminal struct{ Output []json.RawMessage }
	if json.Unmarshal(bpsStructuredLiveTerminal(wire), &rawTerminal) != nil {
		return nil, doneCipher, false, errors.New("upstream raw terminal unavailable")
	}
	_, same := bpsStructuredLiveReasoningPreserved(rawTerminal.Output, result.Output)
	return result.Response, doneCipher, !same, nil
}

// Explicit zero is valid compaction accounting; absent/null is still unknown.
func bpsStructuredLiveUsageKnown(terminal []byte) bool {
	var response struct {
		Usage *struct {
			Input  *int64 `json:"input_tokens"`
			Output *int64 `json:"output_tokens"`
		}
	}
	return json.Unmarshal(terminal, &response) == nil && response.Usage != nil && response.Usage.Input != nil && response.Usage.Output != nil && *response.Usage.Input >= 0 && *response.Usage.Output >= 0
}

func TestBPSStructuredLiveCanonicalReasoningFixture(t *testing.T) {
	const done = `{"type":"reasoning","id":"rs_fixture","summary":[],"encrypted_content":"synthetic-done-cipher"}`
	const terminal = `{"type":"reasoning","id":"rs_fixture","summary":[],"encrypted_content":"synthetic-terminal-envelope"}`
	frame := func(event string) string { return "data: " + event + "\n\n" }
	wire := frame(`{"type":"response.output_item.done","output_index":0,"item":`+done+`}`) + frame(`{"type":"response.completed","response":{"id":"resp_fixture","status":"completed","output":[`+terminal+`],"usage":{"input_tokens":0,"output_tokens":0}}}`)
	canonical, hasDone, different, err := bpsStructuredLiveCanonicalUpstream([]byte(wire), false)
	if err != nil || !hasDone || !different {
		t.Fatal("ordinary verified re-envelope fixture was not recognized")
	}
	var response struct{ Output []json.RawMessage }
	if json.Unmarshal(canonical, &response) != nil {
		t.Fatal("canonical fixture response invalid")
	}
	if present, preserved := bpsStructuredLiveReasoningPreserved(response.Output, []json.RawMessage{json.RawMessage(done)}); !present || !preserved {
		t.Fatal("canonical fixture did not retain the verified delivered cipher")
	}
	if _, preserved := bpsStructuredLiveReasoningPreserved(response.Output, []json.RawMessage{json.RawMessage(terminal)}); preserved {
		t.Fatal("comparison accepted a genuinely different public cipher")
	}
	changed := strings.Replace(terminal, `"summary":[]`, `"summary":[{"type":"summary_text","text":"different reasoning"}]`, 1)
	invalid := frame(`{"type":"response.output_item.done","output_index":0,"item":`+done+`}`) + frame(`{"type":"response.completed","response":{"id":"resp_fixture","status":"completed","output":[`+changed+`]}}`)
	if _, _, _, err := bpsStructuredLiveCanonicalUpstream([]byte(invalid), false); !errors.Is(err, bps.ErrInvalidEvent) {
		t.Fatal("fixture canonicalizer accepted a non-envelope reasoning change")
	}
	const compact = `{"id":"compact_fixture","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"synthetic-compact-cipher"}],"usage":{"input_tokens":0,"output_tokens":0}}`
	copy, hasDone, different, err := bpsStructuredLiveCanonicalUpstream([]byte(compact), true)
	if err != nil || hasDone || different || !bytes.Equal(copy, []byte(compact)) || !bpsStructuredLiveUsageKnown(copy) {
		t.Fatal("compact raw response or explicit zero accounting changed")
	}
	for _, unknown := range []string{`{}`, `{"usage":null}`, `{"usage":{"input_tokens":0}}`, `{"usage":{"input_tokens":null,"output_tokens":0}}`, `{"usage":{"input_tokens":-1,"output_tokens":0}}`} {
		if bpsStructuredLiveUsageKnown([]byte(unknown)) {
			t.Fatal("unknown or invalid accounting became measured zero")
		}
	}
}

func bpsStructuredLiveTerminal(wire []byte) json.RawMessage {
	if json.Valid(wire) {
		return wire
	}
	for _, line := range bytes.Split(wire, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		var event struct {
			Type     string
			Response json.RawMessage
		}
		if json.Unmarshal(bytes.TrimSpace(line[5:]), &event) == nil && (event.Type == "response.completed" || event.Type == "response.failed") {
			return event.Response
		}
	}
	return nil
}

func bpsStructuredLiveText(output []json.RawMessage) string {
	var text strings.Builder
	for _, item := range output {
		var value struct {
			Type, Role string
			Content    []struct{ Type, Text string }
		}
		if json.Unmarshal(item, &value) != nil || value.Type != "message" || value.Role != "assistant" {
			continue
		}
		for _, part := range value.Content {
			if part.Type == "output_text" {
				text.WriteString(part.Text)
			}
		}
	}
	return text.String()
}

func bpsStructuredLiveReasoningPreserved(upstream, public []json.RawMessage) (bool, bool) {
	var original, received []string
	collect := func(items []json.RawMessage) []string {
		var out []string
		for _, item := range items {
			var value struct {
				Type      string
				Encrypted string `json:"encrypted_content"`
			}
			if json.Unmarshal(item, &value) == nil && value.Type == "reasoning" && value.Encrypted != "" {
				out = append(out, value.Encrypted)
			}
		}
		return out
	}
	original, received = collect(upstream), collect(public)
	if len(original) != len(received) {
		return len(original) > 0, false
	}
	for i := range original {
		if original[i] != received[i] {
			return true, false
		}
	}
	return len(original) > 0, true
}
