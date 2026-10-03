package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-agent/internal/bps"
)

// Test-only wire accounting. It observes successful WS body writes in memory,
// never logs frames/tokens, and does not alter transport bytes or deadlines.
type bpsLimitedWire struct {
	mu                                          sync.Mutex
	upgrades, creates, cancels, httpGenerations int
	created, terminal, delta, cancelIdentity    bool
	invalid                                     bool
	requestID, resumeToken                      string
	terminalType, errorCode, errorMessage       string
	transportError, httpProtocol                string
	secrets                                     []string
	createdSignal                               chan struct{}
	createdOnce                                 sync.Once
	read, write                                 bpsLimitedFrameParser
}

type bpsLimitedFrameParser struct {
	pending []byte
	message []byte
	opcode  byte
}

func (p *bpsLimitedFrameParser) add(raw []byte, emit func([]byte), invalid *bool) {
	p.pending = append(p.pending, raw...)
	for len(p.pending) >= 2 {
		first, second := p.pending[0], p.pending[1]
		length, offset := uint64(second&127), 2
		if length == 126 {
			if len(p.pending) < 4 {
				return
			}
			length, offset = uint64(binary.BigEndian.Uint16(p.pending[2:4])), 4
		} else if length == 127 {
			if len(p.pending) < 10 {
				return
			}
			length, offset = binary.BigEndian.Uint64(p.pending[2:10]), 10
		}
		if length > 4<<20 {
			*invalid = true
			p.pending = nil
			return
		}
		masked := second&128 != 0
		var mask [4]byte
		if masked {
			if len(p.pending) < offset+4 {
				return
			}
			copy(mask[:], p.pending[offset:offset+4])
			offset += 4
		}
		if len(p.pending) < offset+int(length) {
			return
		}
		payload := bytes.Clone(p.pending[offset : offset+int(length)])
		p.pending = p.pending[offset+int(length):]
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		opcode := first & 15
		if opcode >= 8 {
			continue
		}
		if first&0x70 != 0 {
			*invalid = true
			continue
		}
		if opcode != 0 {
			p.message = nil
			p.opcode = opcode
		}
		p.message = append(p.message, payload...)
		if len(p.message) > 4<<20 {
			*invalid = true
			p.message = nil
			continue
		}
		if first&128 != 0 {
			if p.opcode == 1 {
				emit(p.message)
			}
			p.message = nil
		}
	}
}

func (w *bpsLimitedWire) observe(raw []byte, outbound bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	parser := &w.read
	if outbound {
		parser = &w.write
	}
	parser.add(raw, func(payload []byte) {
		var fields map[string]json.RawMessage
		if json.Unmarshal(payload, &fields) != nil {
			w.invalid = true
			return
		}
		get := func(key string) string { var value string; _ = json.Unmarshal(fields[key], &value); return value }
		kind := get("type")
		if outbound {
			switch kind {
			case "response.create":
				w.creates++
				w.requestID, w.resumeToken = get("basispoints_request_id"), get("basispoints_resume_token")
			case "basispoints.response.cancel":
				w.cancels++
				w.cancelIdentity = get("request_id") == w.requestID && get("resume_token") != "" && get("resume_token") == w.resumeToken
			}
			return
		}
		switch kind {
		case "basispoints.response.resume_token":
			if get("request_id") == w.requestID && get("resume_token") != "" {
				w.resumeToken = get("resume_token")
			}
		case "response.created":
			w.created = true
			w.createdOnce.Do(func() { close(w.createdSignal) })
		case "response.output_text.delta":
			w.delta = true
		case "response.completed", "response.done", "response.failed", "response.incomplete", "error", "basispoints.response.cancelled":
			w.terminal = true
			w.terminalType = kind
			var detail struct{ Code, Message string }
			if json.Unmarshal(fields["error"], &detail) != nil {
				var response struct {
					Error struct{ Code, Message string }
				}
				_ = json.Unmarshal(fields["response"], &response)
				detail.Code, detail.Message = response.Error.Code, response.Error.Message
			}
			if kind == "error" && detail.Code == "" && detail.Message == "" {
				detail.Code, detail.Message = get("code"), get("message")
			}
			secrets := append(append([]string{}, w.secrets...), w.requestID, w.resumeToken)
			w.errorCode = bpsLimitedDiagnostic(detail.Code, secrets)
			w.errorMessage = bpsLimitedDiagnostic(detail.Message, secrets)
		}
	}, &w.invalid)
}

func bpsLimitedDiagnostic(value string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	value = redactMetricError(value)
	if len(value) > 200 {
		value = value[:200]
	}
	return strings.ToValidUTF8(value, "")
}

type bpsLimitedTransport struct {
	base   http.RoundTripper
	wsBase http.RoundTripper
	wire   *bpsLimitedWire
}

func (t bpsLimitedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/responses") {
		t.wire.mu.Lock()
		blocked := t.wire.httpGenerations+t.wire.creates >= 1
		if !blocked {
			t.wire.httpGenerations++
		}
		t.wire.mu.Unlock()
		if blocked {
			return nil, errors.New("isolated generation request budget exhausted")
		}
	}
	transport := t.base
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") && t.wsBase != nil {
		transport = t.wsBase
	}
	res, err := transport.RoundTrip(r)
	t.wire.mu.Lock()
	if err != nil {
		t.wire.transportError = bpsLimitedDiagnostic(err.Error(), t.wire.secrets)
	}
	if res != nil && r.Method == http.MethodPost {
		t.wire.httpProtocol = res.Proto
	}
	t.wire.mu.Unlock()
	if err == nil && res != nil && res.StatusCode == 101 {
		if rw, ok := res.Body.(io.ReadWriteCloser); ok {
			t.wire.mu.Lock()
			t.wire.upgrades++
			t.wire.mu.Unlock()
			res.Body = &bpsLimitedSocket{ReadWriteCloser: rw, wire: t.wire}
		}
	}
	return res, err
}

type bpsLimitedSocket struct {
	io.ReadWriteCloser
	wire *bpsLimitedWire
}

func (s *bpsLimitedSocket) Read(p []byte) (int, error) {
	n, e := s.ReadWriteCloser.Read(p)
	s.wire.observe(p[:n], false)
	return n, e
}
func (s *bpsLimitedSocket) Write(p []byte) (int, error) {
	n, e := s.ReadWriteCloser.Write(p)
	s.wire.observe(p[:n], true)
	return n, e
}

func TestBPSLimitedLiveWireAccounting(t *testing.T) {
	wire := &bpsLimitedWire{createdSignal: make(chan struct{}), secrets: []string{"fixture-secret", "input-sentinel"}}
	frame := func(text string, masked bool) []byte {
		raw := []byte(text)
		flag := byte(0)
		if masked {
			flag = 128
		}
		packet := []byte{0x81, flag | 126, byte(len(raw) >> 8), byte(len(raw))}
		if masked {
			key := []byte{1, 2, 3, 4}
			packet = append(packet, key...)
			for i := range raw {
				raw[i] ^= key[i%4]
			}
		}
		return append(packet, raw...)
	}
	for _, event := range []struct {
		text string
		sent bool
	}{
		{`{"type":"response.create","basispoints_request_id":"test-request","basispoints_resume_token":"initial"}`, true},
		{`{"type":"basispoints.response.resume_token","request_id":"test-request","resume_token":"issued"}`, false},
		{`{"type":"response.created","response":{"id":"response"}}`, false},
		{`{"type":"basispoints.response.cancel","request_id":"test-request","resume_token":"issued"}`, true},
		{`{"type":"error","error":{"code":"response_create_failed","message":"Invalid input-sentinel Bearer fixture-secret"}}`, false},
	} {
		for _, value := range frame(event.text, event.sent) {
			wire.observe([]byte{value}, event.sent)
		}
	}
	if wire.invalid || wire.creates != 1 || wire.cancels != 1 || !wire.created || !wire.cancelIdentity {
		t.Fatal("wire parser lost split frames or cancellation identity")
	}
	if wire.errorCode != "response_create_failed" || wire.terminalType != "error" || strings.Contains(wire.errorMessage, "fixture-secret") || strings.Contains(wire.errorMessage, "input-sentinel") {
		t.Fatal("terminal diagnostics lost exact machine code or exposed inputs")
	}
}

func TestBPSJSONAndSSEPreparedWebSocketPayloadParity(t *testing.T) {
	options := bps.Options{TaskID: "fixed-task", TurnID: "fixed-turn", ToolsVersion: bps.DefaultToolsVersion, SearchDisabled: true, ResolveCapabilities: true, StructuredOutput: true}
	jsonRequest, err := bps.Prepare([]byte(`{"model":"gpt-6-astra","input":"Reply exactly WS_OK. Do not use any tools.","stream":false,"reasoning":{"effort":"low"}}`), options)
	if err != nil {
		t.Fatal(err)
	}
	sseRequest, err := bps.Prepare([]byte(`{"model":"gpt-6-astra","input":"Reply exactly WS_OK. Do not use any tools.","stream":true,"reasoning":{"effort":"low"}}`), options)
	if err != nil {
		t.Fatal(err)
	}
	if jsonRequest.ClientStream || !sseRequest.ClientStream || !bytes.Equal(jsonRequest.Body, sseRequest.Body) {
		t.Fatal("downstream JSON/SSE selection changed the prepared upstream generation")
	}
	if got := bpsLimitedDiagnostic("unknown_machine_code Bearer private-token", []string{"private-token"}); strings.Contains(got, "private-token") || !strings.Contains(got, "unknown_machine_code") {
		t.Fatal("safe diagnostics lost machine code or exposed secret")
	}
}

func TestBPSLiveWSLimitedMatrix(t *testing.T) {
	if os.Getenv("BPS_WS_LIMITED_LIVE") != "1" {
		t.Skip("explicit three-request isolated live approval required")
	}
	var fixture struct {
		Accounts []bpsLiveAccount `json:"accounts"`
	}
	raw, err := os.ReadFile(os.Getenv("BPS_LIVE_ACCOUNTS_FILE"))
	if err != nil || json.Unmarshal(raw, &fixture) != nil || len(fixture.Accounts) < 1 || len(fixture.Accounts) > 2 {
		t.Fatal("one or two server-only accounts required")
	}
	var results []map[string]any
	allPassed, realWSComplete, realWSCancel := true, 0, false
	for index, scenario := range []string{"sse", "json", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			p := newTestProxy(t, 1)
			account := fixture.Accounts[index%len(fixture.Accounts)]
			p.BPS, p.BaseURL = &bpsRuntime{ToolsVersion: bps.DefaultToolsVersion, SearchDisabled: true}, bps.BaseURL
			if p.Store.UpdateAccount("test", func(a *Account) error {
				a.AccessToken, a.AccountID = account.AccessToken, account.AccountID
				a.RefreshToken = ""
				a.Identity = nil
				a.ExpiresAt = time.Now().Add(time.Hour).Unix()
				return nil
			}) != nil {
				t.Fatal("isolated account fixture failed")
			}
			settings, _ := p.Settings.Get()
			settings.PreferWebsocket, settings.WebsocketEnabled = true, true
			settings.AutomaticTokenRefreshEnabled, settings.Upstream401RecoveryEnabled, settings.BuiltinPromptEnabled = false, false, false
			if p.Settings.Set(settings) != nil {
				t.Fatal("isolated settings fixture failed")
			}
			// Mirror main.go: the configured Rust helper belongs only to the
			// official Codex transport. BPS keeps its own Go HTTP/WS pool.
			p.RuntimeModes = true
			p.CodexTransport = &Proxy{Client: newHTTPClient(), Proxies: p.Proxies}
			if configureGatewayNativeHTTP(p.CodexTransport, os.Getenv) != nil || p.CodexTransport.NativeHTTP == nil || !p.CodexTransport.NativeWebSocket {
				t.Fatal("explicit production-helper fixture required")
			}
			t.Cleanup(p.CodexTransport.closeGatewayClients)
			t.Cleanup(p.closeGatewayClients)
			wire := &bpsLimitedWire{createdSignal: make(chan struct{})}
			base := p.Client.Transport.(*http.Transport).Clone()
			base.Proxy = nil
			base.ResponseHeaderTimeout = time.Duration(settings.UpstreamResponseHeaderTimeoutSeconds) * time.Second
			wsBase := websocketHTTPClient(&http.Client{Transport: base}).Transport.(*http.Transport)
			defer base.CloseIdleConnections()
			defer wsBase.CloseIdleConnections()
			p.Client.Transport = bpsLimitedTransport{base: base, wsBase: wsBase, wire: wire}
			handlerDone := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(handlerDone); p.ServeHTTP(w, r) }))
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			prompt := "Reply exactly WS_OK. Do not use any tools."
			if scenario == "cancel" {
				prompt = "Count from 1 to 500, one integer per line. Do not use tools."
			}
			wire.secrets = []string{account.AccessToken, account.AccountID, prompt}
			payload, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": prompt, "stream": scenario != "json", "reasoning": map[string]string{"effort": "low"}})
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/responses", bytes.NewReader(payload))
			req.Header.Set("Authorization", "Bearer test-client-key")
			var status int
			var completed, delta, expected bool
			var requestError string
			var responseErrorCode string
			var responseErrorMessage string
			done := make(chan struct{})
			start := time.Now()
			go func() {
				defer close(done)
				res, e := server.Client().Do(req)
				if e != nil {
					requestError = "transport"
					return
				}
				defer res.Body.Close()
				status = res.StatusCode
				if scenario == "json" {
					data, e := io.ReadAll(res.Body)
					if e != nil {
						requestError = "read"
						return
					}
					var value struct {
						Status string
						Error  struct{ Code, Message string }
						Output []struct {
							Type    string
							Content []struct{ Text string }
						}
					}
					if json.Unmarshal(data, &value) != nil {
						requestError = "json"
						return
					}
					completed = value.Status == "completed"
					responseErrorCode = bpsLimitedDiagnostic(value.Error.Code, wire.secrets)
					responseErrorMessage = bpsLimitedDiagnostic(value.Error.Message, wire.secrets)
					var output strings.Builder
					for _, item := range value.Output {
						if item.Type == "message" {
							for _, part := range item.Content {
								output.WriteString(part.Text)
							}
						}
					}
					expected = strings.TrimSpace(output.String()) == "WS_OK"
					return
				}
				scan := bufio.NewScanner(res.Body)
				scan.Buffer(make([]byte, 4096), 4<<20)
				var output strings.Builder
				for scan.Scan() {
					line := scan.Text()
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					var event struct {
						Type     string
						Delta    string
						Response struct{ Status string }
					}
					if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
						continue
					}
					if event.Type == "response.output_text.delta" {
						delta = true
						output.WriteString(event.Delta)
					}
					if event.Type == "response.completed" || event.Type == "response.done" {
						completed = event.Response.Status == "completed"
					}
				}
				if scan.Err() != nil {
					requestError = "read"
				}
				expected = strings.TrimSpace(output.String()) == "WS_OK"
			}()
			cancelAfterCreated := false
			if scenario == "cancel" {
				select {
				case <-wire.createdSignal:
					cancelAfterCreated = true
					cancel()
				case <-done:
				case <-ctx.Done():
				}
			}
			select {
			case <-done:
			case <-time.After(95 * time.Second):
				t.Fatal("isolated request exceeded deadline")
			}
			select {
			case <-handlerDone:
			case <-time.After(5 * time.Second):
				t.Fatal("isolated handler failed to release after request")
			}
			wire.mu.Lock()
			upgrades, creates, cancels, validCancel, observedCreated, observedTerminal, invalid := wire.upgrades, wire.creates, wire.cancels, wire.cancelIdentity, wire.created, wire.terminal, wire.invalid
			httpGenerations := wire.httpGenerations
			terminalType, wireErrorCode, wireErrorMessage := wire.terminalType, wire.errorCode, wire.errorMessage
			transportError, httpProtocol := wire.transportError, wire.httpProtocol
			wire.mu.Unlock()
			records := p.Metrics.RecentForAccount("test", 3)
			wsAttempts, httpAttempts, observed101 := 0, 0, 0
			recordStatus := ""
			var attempts []GatewayAttemptSummary
			var recordErrorSource, recordErrorPhase, finalUpstreamCode string
			for _, record := range records {
				recordStatus = record.Status
				attempts = append(attempts, record.Attempts...)
				recordErrorSource, recordErrorPhase, finalUpstreamCode = record.ErrorSource, record.ErrorPhase, record.FinalUpstreamErrorCode
				for _, attempt := range record.Attempts {
					if attempt.Phase == "websocket" {
						wsAttempts++
						if attempt.ObservedHTTPStatusCode != nil && *attempt.ObservedHTTPStatusCode == 101 {
							observed101++
						}
					} else if attempt.Phase == "request" {
						httpAttempts++
					}
				}
			}
			trueWS := observed101 == 1 && upgrades == 1 && creates == 1 && httpAttempts == 0
			passed := !invalid && len(records) == 1 && wsAttempts == 1 && creates+httpGenerations <= 1
			if scenario == "cancel" {
				passed = passed && trueWS && cancelAfterCreated && observedCreated && !completed && cancels == 1 && validCancel && recordStatus == "aborted"
				realWSCancel = passed
			} else {
				passed = passed && status == 200 && completed && expected && recordStatus == "ok" && (trueWS || observed101 == 0 && creates == 0 && httpAttempts == 1)
				if scenario == "sse" {
					passed = passed && delta
				}
				if passed && trueWS {
					realWSComplete++
				}
			}
			result := map[string]any{"case": scenario, "account_fixture_index": index % len(fixture.Accounts), "http_status": status, "record_status": recordStatus, "completed": completed, "expected_text": expected, "delta_observed": delta, "request_error_kind": requestError, "response_error_code": responseErrorCode, "attempts": attempts, "record_error_source": recordErrorSource, "record_error_phase": recordErrorPhase, "final_upstream_error_code": finalUpstreamCode, "websocket_attempts": wsAttempts, "http_attempts": httpAttempts, "observed_101": observed101, "wire_upgrades": upgrades, "generation_frames": creates, "cancel_frames": cancels, "cancel_identity_matches": validCancel, "cancel_after_created": cancelAfterCreated, "upstream_created": observedCreated, "upstream_terminal": observedTerminal, "true_ws": trueWS, "elapsed_ms": time.Since(start).Milliseconds(), "passed": passed}
			result["response_error_message"], result["upstream_terminal_type"], result["upstream_error_code"], result["upstream_error_message"] = responseErrorMessage, terminalType, wireErrorCode, wireErrorMessage
			result["http_generation_requests"], result["production_transport_wiring"] = httpGenerations, "bps_go_pool; native_helper_on_codex_transport_only"
			result["transport_error_message"], result["http_upstream_protocol"] = transportError, httpProtocol
			results = append(results, result)
			allPassed = allPassed && passed
			encoded, _ := json.Marshal(result)
			t.Log(string(encoded))
			if !passed {
				t.Error("limited live protocol validation failed")
			}
		})
	}
	if path := os.Getenv("BPS_LIVE_EVIDENCE_FILE"); path != "" {
		if err := bpsLiveWriteEvidence(path, "bps_ws_limited_three_requests", allPassed, results); err != nil {
			t.Fatal("safe evidence write failed")
		}
	}
	t.Logf("LIMITED_WS_EVIDENCE all_selected_passed=%t full_matrix_verified=%t true_ws_completions=%d true_ws_cancel=%t production_settings_modified=false requests=%d", allPassed, allPassed && realWSComplete == 2 && realWSCancel, realWSComplete, realWSCancel, len(results))
}
