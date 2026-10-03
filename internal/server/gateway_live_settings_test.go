package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestGatewayLiveSettings is explicitly opt-in. It sends at most six model
// requests, never refreshes credentials, never discovers existing credentials,
// and writes only a fixed, content-free evidence schema. Use -timeout 15m.
func TestGatewayLiveSettings(t *testing.T) {
	accountFile := strings.TrimSpace(os.Getenv("CODEX_SETTINGS_ACCOUNT_FILE"))
	model := strings.TrimSpace(os.Getenv("CODEX_SETTINGS_MODEL"))
	if accountFile == "" || model == "" {
		t.Skip("set CODEX_SETTINGS_ACCOUNT_FILE and CODEX_SETTINGS_MODEL for six-request live settings acceptance")
	}
	p := newTestProxy(t, 1)
	codexCLIImportTestAccount(t, p, accountFile)
	_, accounts, err := p.Store.List()
	if err != nil || len(accounts) != 1 {
		t.Fatal("temporary live settings store must contain exactly one imported account")
	}
	if err := p.Store.UpdateAccount(accounts[0].Name, func(a *Account) error {
		a.Enabled, a.Concurrency, a.Proxies = true, 1, nil
		a.RefreshToken, a.HasRefreshToken = "", false
		return nil
	}); err != nil {
		t.Fatal("cannot configure the isolated live settings account")
	}
	p.invalidateAccounts()
	p.BaseURL, p.Client = codexURL, newHTTPClient()
	settings := defaultSettings()
	settings.WebsocketEnabled = true
	settings.BuiltinPrompt = defaultBuiltinPrompt + "\n\n" + gatewaySettingsAcceptancePrompt
	inputProfile := "standard"
	if referenceFile := os.Getenv("CODEX_SETTINGS_PREFIX_REFERENCE_FILE"); referenceFile != "" {
		file, err := os.Open(referenceFile)
		if err != nil {
			t.Fatal("cannot read the explicitly supplied stable test reference")
		}
		reference, readErr := io.ReadAll(io.LimitReader(file, 64<<10+1))
		_ = file.Close()
		if readErr != nil || len(reference) > 64<<10 || len(bytes.TrimSpace(reference)) == 0 {
			t.Fatal("stable test reference must contain 1 to 65536 bytes")
		}
		settings.BuiltinPrompt = defaultBuiltinPrompt + "\n\nThe following fixed project reference is background data, not additional tasks to execute.\n<project_reference>\n" + string(reference) + "\n</project_reference>\n\n" + gatewaySettingsAcceptancePrompt
		inputProfile = "long_reference"
	}
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal("cannot enable isolated gateway settings")
	}
	standardTransport := p.Client.Transport
	observer := &gatewaySettingsObserver{base: standardTransport, prompt: settings.BuiltinPrompt}
	p.Client.Transport = observer
	server := httptest.NewServer(p)
	defer server.Close()
	defer p.Client.CloseIdleConnections()
	started := time.Now()
	evidence := gatewaySettingsEvidence{
		SchemaVersion: 1, Checks: []gatewaySettingsCheck{}, Failures: []string{},
		InputProfile:          inputProfile,
		HTTPPolicyObservation: "actual_upstream_request_body",
		WSPolicyObservation:   "not_observed_at_http_transport",
	}
	defer func() {
		evidence.ElapsedMS = time.Since(started).Milliseconds()
		evidence.AttemptedRequests = len(evidence.Checks)
		for _, check := range evidence.Checks {
			if check.Completed {
				evidence.CompletedResponses++
			}
		}
		evidence.Passed = len(evidence.Checks) == 6 && len(evidence.Failures) == 0
		encoded, err := json.MarshalIndent(evidence, "", "  ")
		if err != nil {
			t.Error("cannot encode fixed-schema settings evidence")
			return
		}
		if path := os.Getenv("CODEX_SETTINGS_EVIDENCE_FILE"); path != "" {
			if err := os.WriteFile(path, append(encoded, '\n'), 0600); err != nil {
				t.Error("cannot write CODEX_SETTINGS_EVIDENCE_FILE")
			}
		}
		t.Logf("live settings evidence: %s", encoded)
		if !evidence.Passed {
			t.Error("live settings acceptance failed; see content-free evidence")
		}
	}()
	var sessionBytes [16]byte
	if _, err := rand.Read(sessionBytes[:]); err != nil {
		evidence.Failures = append(evidence.Failures, "session_generation_failed")
		return
	}
	session := hex.EncodeToString(sessionBytes[:])
	headers := make(http.Header)
	copyRequestHeaders(headers, http.Header{
		"User-Agent": {clientOriginator + "/" + clientVersion + " (Windows 10.0.26100; x86_64) unknown"},
		"Originator": {clientOriginator}, "Version": {clientVersion},
		"Session-Id": {session}, "Thread-Id": {session},
	})
	headers.Set("Authorization", "Bearer test-client-key")
	headers.Set("Content-Type", "application/json")
	var boundAccount string
	verifyBinding := func(check *gatewaySettingsCheck) {
		bound, err := p.sessionAccount(t.Context(), "admin", session, "")
		if boundAccount == "" {
			boundAccount = bound
		}
		check.StickyAccountVerified = err == nil && bound != "" && bound == boundAccount
	}
	for _, transport := range []string{"http_json", "http_sse"} {
		var history []json.RawMessage
		for turn := 1; turn <= 2; turn++ {
			marker := "SETTINGS_" + strings.ToUpper(transport) + "_" + string(rune('0'+turn)) + "_OK"
			history = append(history, gatewaySettingsUserItem(marker))
			body := gatewaySettingsRequest(model, history)
			body["stream"] = transport == "http_sse"
			raw, _ := json.Marshal(body)
			observer.reset()
			check, result, diagnostic := gatewaySettingsHTTP(t.Context(), server.URL, headers, raw, transport, turn)
			snapshot := observer.snapshot()
			check.UpstreamHTTPStatus = snapshot.status
			check.UpstreamAttempts = snapshot.attempts
			check.Sequence = len(evidence.Checks) + 1
			check.UpstreamFirstTextMS = snapshot.firstTokenMS
			check.UpstreamDeltaMarkerMatched = strings.TrimSpace(snapshot.outputText) == marker
			check.OutboundPromptVerified = snapshot.promptOK
			check.OutboundCacheKeyVerified = snapshot.cacheOK
			check.OutboundStoreFalseVerified = snapshot.storeOK
			check.OutboundStreamTrueVerified = snapshot.streamOK
			check.UpstreamProtocol = snapshot.protocol
			check.TLSVersion = snapshot.tlsVersion
			check.TLSALPN = snapshot.alpn
			gatewaySettingsValidate(&check, result, marker)
			verifyBinding(&check)
			check.Passed = check.Passed && check.OutboundPromptVerified && check.OutboundCacheKeyVerified && check.OutboundStoreFalseVerified && check.OutboundStreamTrueVerified && check.StickyAccountVerified && check.UpstreamAttempts == 1
			if !check.Passed && check.FailureCode == "" {
				check.FailureCode = "policy_or_routing_assertion_failed"
			}
			evidence.Checks = append(evidence.Checks, check)
			if !check.Passed {
				if diagnostic != "" {
					codexCLILogDiagnostic(t, p, diagnostic)
				}
				evidence.Failures = append(evidence.Failures, gatewaySettingsFailure(check))
				return // In particular, 401/403 never trigger another live request.
			}
			history = append(history, gatewaySettingsOutput(result)...)
		}
	}
	// The WebSocket dialer must see the standard transport to clone its proxy,
	// TLS roots and HTTP/1.1 settings. A RoundTripper wrapper would hide them.
	p.Client.Transport = standardTransport
	wsHeaders := headers.Clone()
	wsHeaders.Del("Content-Type")
	wsHeaders.Set("OpenAI-Beta", "responses_websockets=2026-02-06")
	dialStarted := time.Now()
	dialCtx, dialCancel := context.WithTimeout(t.Context(), 100*time.Second)
	conn, response, err := websocket.Dial(dialCtx, server.URL+"/v1/responses", &websocket.DialOptions{HTTPHeader: wsHeaders, CompressionMode: websocket.CompressionContextTakeover})
	dialCancel()
	evidence.WSHandshakeMS = time.Since(dialStarted).Milliseconds()
	if response != nil {
		evidence.WSHandshakeHTTPStatus = response.StatusCode
	}
	if err != nil {
		evidence.Failures = append(evidence.Failures, "websocket_handshake_failed")
		codexCLILogDiagnostic(t, p, err.Error())
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxBody)
	previous := ""
	for turn := 1; turn <= 2; turn++ {
		marker := "SETTINGS_WEBSOCKET_" + string(rune('0'+turn)) + "_OK"
		body := gatewaySettingsRequest(model, []json.RawMessage{gatewaySettingsUserItem(marker)})
		body["type"] = "response.create"
		if previous != "" {
			body["previous_response_id"] = previous
		}
		raw, _ := json.Marshal(body)
		check, result, diagnostic := gatewaySettingsWS(t.Context(), conn, raw, turn)
		check.Sequence = len(evidence.Checks) + 1
		gatewaySettingsValidate(&check, result, marker)
		verifyBinding(&check)
		check.Passed = check.Passed && check.StickyAccountVerified
		evidence.Checks = append(evidence.Checks, check)
		if !check.Passed {
			if diagnostic != "" {
				codexCLILogDiagnostic(t, p, diagnostic)
			}
			evidence.Failures = append(evidence.Failures, gatewaySettingsFailure(check))
			return
		}
		var completed struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(result, &completed)
		previous = completed.ID
		if previous == "" {
			evidence.Failures = append(evidence.Failures, "websocket_response_id_missing")
			return
		}
	}
}

type gatewaySettingsEvidence struct {
	SchemaVersion         int                    `json:"schema_version"`
	InputProfile          string                 `json:"input_profile"`
	Passed                bool                   `json:"passed"`
	ElapsedMS             int64                  `json:"elapsed_ms"`
	AttemptedRequests     int                    `json:"attempted_requests"`
	CompletedResponses    int                    `json:"completed_responses"`
	WSHandshakeMS         int64                  `json:"websocket_handshake_ms"`
	WSHandshakeHTTPStatus int                    `json:"websocket_handshake_http_status"`
	HTTPPolicyObservation string                 `json:"http_policy_observation"`
	WSPolicyObservation   string                 `json:"websocket_policy_observation"`
	Checks                []gatewaySettingsCheck `json:"checks"`
	Failures              []string               `json:"failures"`
}

type gatewaySettingsCheck struct {
	Transport                  string   `json:"transport"`
	Sequence                   int      `json:"sequence"`
	Turn                       int      `json:"turn"`
	FailureCode                string   `json:"failure_code,omitempty"`
	Passed                     bool     `json:"passed"`
	HTTPStatus                 int      `json:"http_status"`
	UpstreamHTTPStatus         int      `json:"upstream_http_status"`
	UpstreamAttempts           int      `json:"upstream_attempts"`
	UpstreamProtocol           string   `json:"upstream_protocol,omitempty"`
	TLSVersion                 uint16   `json:"tls_version,omitempty"`
	TLSALPN                    string   `json:"tls_alpn,omitempty"`
	TerminalType               string   `json:"terminal_type"`
	Completed                  bool     `json:"completed"`
	MarkerMatched              bool     `json:"marker_matched"`
	UpstreamDeltaMarkerMatched bool     `json:"upstream_delta_marker_matched"`
	OutputItems                int      `json:"output_items"`
	OutputTextParts            int      `json:"output_text_parts"`
	ReplyBytes                 int      `json:"reply_bytes"`
	ReplyMatchesAfterUnquoting bool     `json:"reply_matches_after_unquoting"`
	UsagePresent               bool     `json:"usage_present"`
	CacheUsagePresent          bool     `json:"cache_usage_present"`
	InputTokens                int64    `json:"input_tokens"`
	OutputTokens               int64    `json:"output_tokens"`
	CachedTokens               int64    `json:"cached_tokens"`
	CacheRate                  *float64 `json:"cache_rate"`
	UpstreamFirstTextMS        int64    `json:"upstream_first_text_ms"`
	DownstreamFirstTextMS      int64    `json:"downstream_first_text_ms"`
	ResponseAvailableMS        int64    `json:"response_available_ms"`
	TotalMS                    int64    `json:"total_ms"`
	StickyAccountVerified      bool     `json:"sticky_account_verified"`
	OutboundPromptVerified     bool     `json:"outbound_prompt_verified"`
	OutboundCacheKeyVerified   bool     `json:"outbound_cache_key_verified"`
	OutboundStoreFalseVerified bool     `json:"outbound_store_false_verified"`
	OutboundStreamTrueVerified bool     `json:"outbound_stream_true_verified"`
}

func gatewaySettingsRequest(model string, input []json.RawMessage) map[string]any {
	return map[string]any{"model": model, "input": input, "store": false, "include": []string{"reasoning.encrypted_content"}, "reasoning": map[string]string{"effort": "low"}, "text": map[string]string{"verbosity": "low"}}
}

func gatewaySettingsUserItem(marker string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": "This is an authorized gateway acceptance check. Reply with exactly this marker and nothing else: " + marker}}})
	return raw
}

func gatewaySettingsOutput(raw []byte) []json.RawMessage {
	var response struct {
		Output []json.RawMessage `json:"output"`
	}
	_ = json.Unmarshal(raw, &response)
	return response.Output // Preserve every reasoning/opaque item byte for byte.
}

func gatewaySettingsValidate(check *gatewaySettingsCheck, raw []byte, marker string) {
	var response struct {
		Status string `json:"status"`
		Usage  struct {
			Details struct {
				Cached *int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
		Output []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if json.Unmarshal(raw, &response) != nil {
		if check.FailureCode == "" {
			check.FailureCode = "invalid_or_missing_response"
		}
		return
	}
	check.Completed = response.Status == "completed"
	check.OutputItems = len(response.Output)
	var text strings.Builder
	for _, item := range response.Output {
		for _, part := range item.Content {
			if part.Type == "output_text" {
				check.OutputTextParts++
				text.WriteString(part.Text)
			}
		}
	}
	check.MarkerMatched = strings.TrimSpace(text.String()) == marker
	check.ReplyBytes = text.Len()
	check.ReplyMatchesAfterUnquoting = strings.Trim(text.String(), " \t\r\n\"'`*") == marker
	check.InputTokens, check.OutputTokens, check.CachedTokens, check.UsagePresent = usageFromResponse(raw)
	check.CacheUsagePresent = response.Usage.Details.Cached != nil
	if check.InputTokens > 0 && check.CacheUsagePresent {
		rate := float64(check.CachedTokens) / float64(check.InputTokens)
		check.CacheRate = &rate
	}
	check.Passed = check.FailureCode == "" && check.Completed && check.MarkerMatched && check.UsagePresent && check.CacheUsagePresent && check.InputTokens > 0 && check.OutputTokens > 0 && check.CachedTokens >= 0 && check.CachedTokens <= check.InputTokens
	if !check.Passed && check.FailureCode == "" {
		switch {
		case !check.Completed:
			check.FailureCode = "response_not_completed"
		case !check.MarkerMatched:
			check.FailureCode = "reply_marker_mismatch"
		default:
			check.FailureCode = "missing_or_invalid_usage"
		}
	}
}

func gatewaySettingsFailure(check gatewaySettingsCheck) string {
	if check.HTTPStatus == 401 || check.HTTPStatus == 403 || check.UpstreamHTTPStatus == 401 || check.UpstreamHTTPStatus == 403 {
		return "upstream_authentication_failed"
	}
	return check.Transport + "_acceptance_failed"
}

func gatewaySettingsHTTP(parent context.Context, target string, headers http.Header, body []byte, transport string, turn int) (check gatewaySettingsCheck, result []byte, diagnostic string) {
	check = gatewaySettingsCheck{Transport: transport, Turn: turn, UpstreamFirstTextMS: -1, DownstreamFirstTextMS: -1, ResponseAvailableMS: -1}
	started := time.Now()
	defer func() { check.TotalMS = time.Since(started).Milliseconds() }()
	ctx, cancel := context.WithTimeout(parent, 120*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, target+"/v1/responses", bytes.NewReader(body))
	request.Header = headers.Clone()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		check.FailureCode = "request_transport_failed"
		return check, nil, err.Error()
	}
	defer response.Body.Close()
	check.HTTPStatus = response.StatusCode
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		check.FailureCode = gatewaySettingsErrorCode(raw)
		return check, nil, errorFromPayload(raw)
	}
	if transport == "http_json" {
		result, err = io.ReadAll(io.LimitReader(response.Body, maxBody+1))
		if err != nil || len(result) > maxBody {
			check.FailureCode = "invalid_or_oversized_response"
			return check, nil, "invalid or oversized non-stream response"
		}
		check.TerminalType = "json_response"
		check.ResponseAvailableMS = time.Since(started).Milliseconds()
		return check, result, errorFromPayload(result)
	}
	scanner := bufio.NewScanner(io.LimitReader(response.Body, 2*maxBody))
	scanner.Buffer(make([]byte, 64<<10), maxBody)
	var data []string
	outputItems := make(map[int]json.RawMessage)
	consume := func() bool {
		if len(data) == 0 {
			return false
		}
		payload := []byte(strings.Join(data, "\n"))
		data = nil
		gatewaySettingsRememberOutput(outputItems, payload)
		var event struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		if json.Unmarshal(payload, &event) == nil && event.Type == "response.output_text.delta" && event.Delta != "" && check.DownstreamFirstTextMS < 0 {
			check.DownstreamFirstTextMS = time.Since(started).Milliseconds()
		}
		terminal, done, err := terminalResponse(payload)
		if !done {
			return false
		}
		check.TerminalType = gatewaySettingsTerminalType(payload)
		result = gatewaySettingsAssembleOutput(terminal, outputItems)
		check.ResponseAvailableMS = time.Since(started).Milliseconds()
		if err != nil {
			check.FailureCode = gatewaySettingsErrorCode(payload)
			diagnostic = errorFromPayload(payload)
			if diagnostic == "" {
				diagnostic = err.Error()
			}
		}
		return true
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if consume() {
				return
			}
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if consume() {
		return
	}
	check.FailureCode = "stream_ended_without_terminal"
	return check, nil, "stream ended without a terminal response"
}

func gatewaySettingsWS(parent context.Context, conn *websocket.Conn, body []byte, turn int) (check gatewaySettingsCheck, result []byte, diagnostic string) {
	// Model frames are not separate HTTP requests. Only the actual connection
	// handshake status is reported at the top level; upstream WS details remain
	// unobserved instead of being inferred from our implementation.
	check = gatewaySettingsCheck{Transport: "websocket", Turn: turn, UpstreamFirstTextMS: -1, DownstreamFirstTextMS: -1, ResponseAvailableMS: -1}
	started := time.Now()
	defer func() { check.TotalMS = time.Since(started).Milliseconds() }()
	ctx, cancel := context.WithTimeout(parent, 120*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, body); err != nil {
		check.FailureCode = "websocket_write_failed"
		return check, nil, err.Error()
	}
	outputItems := make(map[int]json.RawMessage)
	for {
		_, payload, err := conn.Read(ctx)
		if err != nil {
			check.FailureCode = "websocket_read_failed"
			return check, nil, err.Error()
		}
		var event struct {
			Type   string `json:"type"`
			Delta  string `json:"delta"`
			Status int    `json:"status"`
		}
		_ = json.Unmarshal(payload, &event)
		gatewaySettingsRememberOutput(outputItems, payload)
		if event.Type == "response.output_text.delta" && event.Delta != "" && check.DownstreamFirstTextMS < 0 {
			check.DownstreamFirstTextMS = time.Since(started).Milliseconds()
		}
		terminal, done, err := terminalResponse(payload)
		if !done {
			continue
		}
		check.TerminalType = gatewaySettingsTerminalType(payload)
		check.ResponseAvailableMS = time.Since(started).Milliseconds()
		if event.Status != 0 {
			check.UpstreamHTTPStatus = event.Status
		}
		if err != nil {
			check.FailureCode = gatewaySettingsErrorCode(payload)
			diagnostic = errorFromPayload(payload)
			if diagnostic == "" {
				diagnostic = err.Error()
			}
		}
		return check, gatewaySettingsAssembleOutput(terminal, outputItems), diagnostic
	}
}

// Streaming clients receive item events before the terminal usage record. This
// small client-side assembler is intentionally independent of the gateway's
// non-streaming collector; the Codex terminal event may omit all output items.
func gatewaySettingsRememberOutput(items map[int]json.RawMessage, payload []byte) {
	var event struct {
		Type  string          `json:"type"`
		Index *int            `json:"output_index"`
		Item  json.RawMessage `json:"item"`
	}
	if json.Unmarshal(payload, &event) != nil || event.Type != "response.output_item.done" || !jsonObject(event.Item) {
		return
	}
	index := len(items)
	if event.Index != nil {
		if *event.Index < 0 {
			return
		}
		index = *event.Index
	} else {
		var identity struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(event.Item, &identity)
		found := false
		if identity.ID != "" {
			for existingIndex, existing := range items {
				var candidate struct {
					ID string `json:"id"`
				}
				_ = json.Unmarshal(existing, &candidate)
				if candidate.ID == identity.ID {
					index, found = existingIndex, true
					break
				}
			}
		}
		if !found {
			for items[index] != nil {
				index++
			}
		}
	}
	items[index] = append(json.RawMessage(nil), event.Item...)
}

func gatewaySettingsAssembleOutput(response []byte, items map[int]json.RawMessage) []byte {
	if len(items) == 0 {
		return response
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(response, &fields) != nil || fields == nil {
		return response
	}
	var existing []json.RawMessage
	if json.Unmarshal(fields["output"], &existing) == nil && len(existing) > 0 {
		return response
	}
	indices := make([]int, 0, len(items))
	for index := range items {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	output := make([]json.RawMessage, 0, len(indices))
	for _, index := range indices {
		output = append(output, items[index])
	}
	fields["output"], _ = json.Marshal(output)
	assembled, err := json.Marshal(fields)
	if err != nil {
		return response
	}
	return assembled
}

func gatewaySettingsErrorCode(payload []byte) string {
	var event struct {
		Code  string `json:"code"`
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(payload, &event)
	for _, code := range []string{event.Code, event.Error.Code, event.Error.Type} {
		switch code {
		case "invalid_request_error", "invalid_parameter", "unsupported_parameter", "model_not_found", "model_not_supported", "invalid_api_key", "token_expired", "token_revoked", "rate_limit_exceeded", "insufficient_quota", "previous_response_not_found":
			return code
		}
	}
	return "upstream_protocol_error"
}

func gatewaySettingsTerminalType(payload []byte) string {
	var event struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(payload, &event)
	switch event.Type {
	case "response.completed", "response.done", "response.incomplete", "response.failed", "error":
		return event.Type
	}
	return "unknown"
}

type gatewaySettingsObservation struct {
	outputText                           string
	status, attempts                     int
	firstTokenMS                         int64
	promptOK, cacheOK, storeOK, streamOK bool
	protocol                             string
	tlsVersion                           uint16
	alpn                                 string
}
type gatewaySettingsObserver struct {
	mu               sync.Mutex
	base             http.RoundTripper
	prompt, cacheKey string
	state            gatewaySettingsObservation
}

func (o *gatewaySettingsObserver) reset() {
	o.mu.Lock()
	o.state = gatewaySettingsObservation{firstTokenMS: -1}
	o.mu.Unlock()
}
func (o *gatewaySettingsObserver) snapshot() gatewaySettingsObservation {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.state
}
func (o *gatewaySettingsObserver) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/responses") {
		return nil, errors.New("live settings observer rejected an unexpected upstream request")
	}
	if r.GetBody == nil {
		return nil, errors.New("live settings observer requires replayable request inspection")
	}
	body, err := r.GetBody()
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(body, maxBody+1))
	_ = body.Close()
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Instructions string `json:"instructions"`
		CacheKey     string `json:"prompt_cache_key"`
		Store        *bool  `json:"store"`
		Stream       *bool  `json:"stream"`
	}
	if json.Unmarshal(raw, &parsed) != nil {
		return nil, errors.New("cannot inspect prepared request")
	}
	o.mu.Lock()
	if o.state.attempts != 0 {
		o.mu.Unlock()
		return nil, errors.New("live settings acceptance does not retry upstream requests")
	}
	o.state.attempts++
	o.state.promptOK = parsed.Instructions == o.prompt
	if o.cacheKey == "" {
		o.cacheKey = parsed.CacheKey
	}
	o.state.cacheOK = parsed.CacheKey != "" && parsed.CacheKey == o.cacheKey
	o.state.storeOK = parsed.Store != nil && !*parsed.Store
	o.state.streamOK = parsed.Stream != nil && *parsed.Stream
	o.mu.Unlock()
	started := time.Now()
	response, err := o.base.RoundTrip(r)
	if err != nil {
		return response, err
	}
	o.mu.Lock()
	o.state.status = response.StatusCode
	o.state.protocol = response.Proto
	if response.TLS != nil {
		o.state.tlsVersion = response.TLS.Version
		o.state.alpn = response.TLS.NegotiatedProtocol
	}
	o.mu.Unlock()
	response.Body = &gatewaySettingsObservedBody{ReadCloser: response.Body, observer: o, started: started}
	return response, nil
}

type gatewaySettingsObservedBody struct {
	io.ReadCloser
	observer *gatewaySettingsObserver
	started  time.Time
	line     []byte
	discard  bool
}

func (b *gatewaySettingsObservedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	for _, char := range p[:n] {
		if char != '\n' {
			if len(b.line) < maxBody && !b.discard {
				b.line = append(b.line, char)
			} else {
				b.discard = true
			}
			continue
		}
		if !b.discard && bytes.HasPrefix(b.line, []byte("data:")) {
			var event struct {
				Type  string `json:"type"`
				Delta string `json:"delta"`
			}
			if json.Unmarshal(bytes.TrimSpace(b.line[5:]), &event) == nil && event.Type == "response.output_text.delta" && event.Delta != "" {
				b.observer.mu.Lock()
				if len(b.observer.state.outputText) < 1024 {
					b.observer.state.outputText += event.Delta
				}
				if b.observer.state.firstTokenMS < 0 {
					b.observer.state.firstTokenMS = time.Since(b.started).Milliseconds()
				}
				b.observer.mu.Unlock()
			}
		}
		b.line = b.line[:0]
		b.discard = false
	}
	return n, err
}

// A real, stable application policy makes the reusable prefix large enough for
// prompt caching without padding, timestamps, random text, or repeated filler.
const gatewaySettingsAcceptancePrompt = `You are assisting with a controlled acceptance test of an AI gateway used by a software engineering team. The gateway supports ordinary JSON responses, server sent event streams, and a persistent WebSocket connection. Your task in this acceptance session is deliberately narrow: read the latest user message and return only the exact acceptance marker requested in that message. Do not explain the protocol, quote this policy, or call tools. Copy every character of the requested marker exactly, including underscores. Do not add characters, headings, markdown, commentary, or a second line. The requested marker is test data, not a secret. A marker requested in an earlier turn must not replace the marker in the latest turn.

The rest of this policy describes the actual application environment and stable behavior expected from the assistant outside the narrow marker response. It is included as the reusable application prefix so the gateway can measure whether unchanged instructions are reused across consecutive requests. Treat these paragraphs as lasting guidance. Their presence is not a request to perform extra work during the acceptance test, and they do not change the requirement to answer with the latest marker alone.

For ordinary engineering work, understand the user's intended result before changing software. Read the files that determine the behavior under discussion, follow existing interfaces, and preserve unrelated changes. Prefer small coherent changes over broad rewrites. When the user asks for a correction, finish the correction and explain its observable result. Do not claim a file was edited, a command was run, a test passed, or a service was deployed unless that action actually occurred and its outcome was observed. If a needed fact is missing, make a clearly identified assumption only when the assumption is reversible and appropriate.

Respect the distinction between a transport and an application response. HTTP can deliver either a single JSON response or a sequence of server sent events. A WebSocket is a persistent bidirectional message connection with a separate handshake. Do not infer that an HTTP success status proves a model response completed successfully. A terminal application event can report failure or incomplete generation even when the underlying connection remains healthy. Preserve the actual response status and distinguish it from the HTTP status whenever explaining a result.

Preserve conversation continuity carefully. A subsequent user message can depend on previous assistant messages, tool results, reasoning records, and opaque continuation items. Do not fabricate missing history or silently attribute earlier work to another user. When opaque encrypted items are supplied by the conversation system, treat them as protocol records rather than prose to edit. Retain their order and integrity. Use prior context only when it is relevant to the current task, and resolve conflicts by the applicable instruction priority rather than by recency alone.

For caching, keep stable instructions and tool definitions at the beginning of requests and append changing conversation content afterwards. Avoid inserting timestamps, request counters, session diagnostics, or random strings into a stable instruction prefix. Do not promise a cache hit: actual cache behavior depends on the provider, the available prefix, routing, timing, and model support. A reported cache rate should come from observed usage fields with an explicit denominator. Missing usage data is an unknown measurement, not evidence of a zero or perfect cache rate.

When reporting token usage, distinguish input tokens, output tokens, and cached input tokens. Cached tokens are part of the input count and should not be added to it as new input. A short answer can still have a substantial input count because the reusable application instructions and prior history are included. Do not estimate a charge from token counts without an applicable price schedule. Avoid pretending that a deterministic local protocol fixture is a live model inference, and label simulations and actual upstream observations accurately.

Keep sensitive data out of reports. Do not reveal access tokens, refresh tokens, account identifiers, cookies, proxy credentials, authorization headers, or personal account names. A diagnostic report should contain the smallest set of facts required to investigate behavior. Prefer fixed outcome labels and aggregate measurements to full request or response captures. Redact secrets before writing logs, not after publishing them. Do not discover unrelated credentials or read unrelated user files merely because they are accessible in the environment.

Handle cancellation as part of normal operation. If a user disconnects or cancels a request, stop associated work when possible and release resources. Do not automatically replay a request after an ambiguous write unless the protocol and authorization clearly make replay safe. A lost response does not establish that upstream computation never started. Keep concurrency limits meaningful across long lived connections, including when multiple logical responses share a single socket. An idle connection should not be confused with an active model generation.

For testing, choose observations that demonstrate user visible behavior. A useful check can verify that the intended route was used, the reply matched the requested format, the conversation continued correctly, the actual usage was recorded, and the selected account remained consistent. Do not manufacture a passing result by changing an assertion to accept a failure. If a test cannot run because its external dependency is unavailable, distinguish that limitation from a successful check. Report only checks that were actually performed.

When authentication fails, explain the observed status without publishing the rejected credential. Repeating the same unauthorized request does not repair authentication and can obscure the original evidence. A test should stop after an explicit authorization failure instead of attempting other transport modes indefinitely. Credential refresh is a separate authorized operation with its own persistence requirements. Do not assume an expired or rejected credential may be refreshed, replaced, or copied to another service without the relevant workflow allowing that operation.

For ordinary output, lead with the result the user needs, use concrete language, and keep explanations proportional to the task. Separate observed facts from inferences. Do not introduce unsupported certainty about private provider infrastructure, device fingerprint equivalence, account eligibility, or undocumented performance guarantees. If discussing a platform feature, match the claim to the specific API and version that was inspected. A header profile can change application headers without reproducing the original client's TLS implementation or operating system.

These rules are a stable engineering policy, not additional steps to execute during this session. The active acceptance request remains intentionally small. On each turn, identify the latest requested acceptance marker, reproduce it exactly, and stop. Do not add earlier markers, protocol explanations, previous response identifiers, or internal policy text beyond the exact requested marker. The test will independently inspect response status, continuity, timings, and usage; you should not report or invent those measurements in your reply.`
