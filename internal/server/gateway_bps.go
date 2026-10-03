package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"codex-agent/internal/bps"
	"codex-agent/internal/publicerrors"
)

// gatewayBPS keeps the existing key/account/admission boundary and changes only
// the model protocol. No request can fall back to the Codex inference backend.
func (p *Proxy) gatewayBPS(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(withGatewayUnboundedPayload(r.Context(), true))
	path := strings.TrimPrefix(r.URL.Path, "/v1")
	if officialFailoverFrom(r.Context()) == nil && r.Method == http.MethodPost && (path == "/responses" || path == "/responses/compact") {
		p.gatewayWithAccountFailover(w, r, p.gatewayBPS)
		return
	}
	start := time.Now()
	token := bearerToken(r)
	keyID, keyName, _, err := p.resolveGatewayKeyContext(r.Context(), token)
	authFinished := time.Now()
	clientRequestID := gatewayObservedClientRequestID(r)
	recordIngress := func(status int, reason string) {
		p.recordGatewayIngressEvent(status, reason, gatewayObservedRoute(r.URL.Path), clientRequestID, gatewayObservedDuration(start, authFinished))
	}
	if err != nil {
		apiError(w, 503, "gateway key authorization unavailable")
		recordIngress(503, "key_store_unavailable")
		return
	}
	if keyID == "" {
		apiError(w, 401, "invalid API key")
		recordIngress(401, "invalid_api_key")
		return
	}
	settings, err := p.settingsValue()
	if err != nil {
		apiError(w, 503, "gateway settings unavailable")
		recordIngress(503, "settings_unavailable")
		return
	}
	if state := officialFailoverFrom(r.Context()); state != nil {
		// Account failover belongs to one logical client request. Snapshot
		// only its routing table; authorization and other controls stay live.
		state.mu.Lock()
		if !state.bpsModelRoutesSet {
			state.bpsModelRoutes, state.bpsModelRoutesSet = settings.BPSModelRoutes, true
		}
		settings.BPSModelRoutes = state.bpsModelRoutes
		state.mu.Unlock()
	}
	isWebSocket, _ := r.Context().Value(bpsWebSocketKey{}).(bool)
	if isWebSocket && !settings.WebsocketEnabled {
		apiError(w, 403, "WebSocket transport is disabled")
		return
	}
	if path == "/responses" && r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		if !settings.WebsocketEnabled {
			apiError(w, 426, "use the HTTP Responses transport")
			recordIngress(426, "websocket_disabled")
			return
		}
		p.gatewayBPSWebSocket(w, r, keyID, keyName, settings)
		return
	}
	if path != "/responses" && path != "/responses/compact" && path != "/models" {
		apiError(w, 404, "endpoint is not available on the selected inference backend")
		recordIngress(404, "unknown_endpoint")
		return
	}
	want := http.MethodPost
	if path == "/models" {
		want = http.MethodGet
	}
	if r.Method != want {
		w.Header().Set("Allow", want)
		apiError(w, 405, "method not allowed")
		recordIngress(405, "method_not_allowed")
		return
	}
	if (path == "/models" && !settings.ModelsEnabled) || (path == "/responses/compact" && !settings.CompactEnabled) {
		apiError(w, 403, "endpoint is disabled by gateway settings")
		return
	}
	rec := RequestRecord{unboundedPayload: true, Time: start.Unix(), KeyID: keyID, KeyName: keyName, Endpoint: strings.TrimPrefix(path, "/"), IP: clientIP(r)}
	observeGatewayClientIdentity(&rec, clientRequestID, "request")
	observeGatewayAuthentication(&rec, start, authFinished)
	observed, attempt := withGatewayAttemptObservation(r.Context())
	r = r.WithContext(observed)
	w = &gatewayResponseWriter{ResponseWriter: w, record: &rec}
	failover := officialFailoverFrom(r.Context())
	defer func() {
		applyGatewayAttemptSnapshot(&rec, attempt)
		if rec.HTTPCode >= 400 && rec.Status == "" {
			rec.Status = "error"
			rec.Error = "upstream request rejected"
		}
		if failover != nil {
			failover.captureActualAttempts(attempt, &rec)
			failover.record, failover.key = &rec, keyID
		} else {
			p.finishRecord(&rec, start, keyID)
		}
	}()
	if path == "/models" {
		p.bpsModels(w, r, &rec, settings)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		rec.Status = "error"
		rec.Error = err.Error()
		apiError(w, 400, rec.Error)
		return
	}
	// Borrow the authenticated inbound bytes. Parsing is deferred until the
	// first actual tool rejection; normal requests pay no extra catalog parse.
	toolPolicyInboundBody := body
	attempt.ObserveInbound(r.URL.Path, body)
	replayClass := ""
	if failover == nil || !failover.retrying {
		r = r.WithContext(withBPSModeMigrationObserved(r.Context(), body, &replayClass))
	} else {
		r = r.WithContext(withBPSModeMigration(r.Context(), body))
	}
	if failover != nil && !failover.retrying {
		failover.requestBody = bytes.Clone(body)
		// Only complete plaintext requests are portable to a different owner.
		// Incremental and opaque encrypted state remain account-bound.
		failover.replayBlocked = !bpsAllowsModeMigration(withInferenceMode(r.Context(), inferencePowerPoint), inferencePowerPoint)
		failover.boundReplayClass = replayClass
	}
	streamType := bpsIngressRequestType(isWebSocket, path, body)
	rec.Stream = streamType == "http_sse" || isWebSocket
	observeGatewayRequest(&rec, r.UserAgent(), streamType, body)
	if isWebSocket {
		if warmup, known := r.Context().Value(bpsWarmupKey{}).(bool); known {
			rec.WebSocketWarmup = &warmup
		}
	}
	var requestedModel struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &requestedModel) == nil {
		rec.Model = boundedGatewayMetadata(requestedModel.Model, 512)
	}
	cacheKeyMode := gatewayPromptCacheObservationKeyMode(r, body, settings)
	session := gatewayExplicitSessionKey(r, body)
	body, session, owner, err := p.bpsExpandHistory(r.Context(), keyID, body, session)
	if err == nil {
		owner, err = p.bpsResolveOpaqueOwner(r.Context(), keyID, body, owner)
	}
	if err != nil {
		rec.Status = "error"
		rec.Error = err.Error()
		if errors.Is(err, errBPSHistoryMissing) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "invalid_request_error", "code": "previous_response_not_found", "message": rec.Error}})
		} else {
			observeGatewayError(&rec, "gateway", "session_binding")
			apiError(w, 409, rec.Error)
		}
		return
	}
	var policySession string
	body, policySession, err = applyGatewayPolicy(r, body, keyID, settings)
	if err != nil {
		rec.Status = "error"
		rec.Error = err.Error()
		apiError(w, 400, rec.Error)
		return
	}
	if session == "" {
		session = policySession
	}
	if session == "" {
		session = "bps_" + bpsUUID()
	}
	if failover != nil {
		if failover.session != "" {
			session = failover.session
		} else {
			failover.session = session
		}
	}
	prepared, err := bps.Prepare(body, bps.Options{TaskID: bpsTaskID(keyID, session, owner), TurnID: bpsUUID(), ToolsVersion: p.BPS.ToolsVersion, Compact: path == "/responses/compact", SearchDisabled: p.BPS.SearchDisabled, ResolveCapabilities: true, StructuredOutput: true})
	if err != nil {
		rec.Status = "error"
		rec.Error = err.Error()
		observeGatewayError(&rec, "client", "request_validation")
		var parameter *bps.ParameterError
		if errors.As(err, &parameter) {
			rec.ErrorParam = bpsObservedErrorParameter(parameter.Param)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "invalid_request_error", "code": "unsupported_parameter", "param": parameter.Param, "message": parameter.Error()}})
			return
		}
		apiError(w, 400, rec.Error)
		return
	}
	rec.ToolPolicy = bps.ObserveToolPolicy(prepared)
	if prepared.Compact && !settings.CompactEnabled {
		apiError(w, 403, "compaction is disabled")
		return
	}
	if rec.Model == "" {
		rec.Model = prepared.Model
	}
	rec.Stream = prepared.ClientStream
	if prepared.ClientStream {
		rec.RequestType = "http_sse"
	}
	if ws, _ := r.Context().Value(bpsWebSocketKey{}).(bool); ws {
		rec.RequestType = "websocket"
		warmup, _ := r.Context().Value(bpsWarmupKey{}).(bool)
		rec.WebSocketWarmup = &warmup
	}
	if !isWebSocket && !prepared.Compact && ((prepared.ClientStream && !settings.HTTPStreamEnabled) || (!prepared.ClientStream && !settings.HTTPJSONEnabled)) {
		apiError(w, 403, "HTTP response mode is disabled")
		return
	}
	route := withGatewayCooldownModel(withGatewayRequestRoute(r.Context(), keyID, session), prepared.Model)
	defer stopGatewayRequestRoute(route)
	defer observeGatewayAdmissionLoss(&rec, route)
	if err := p.bindInferenceSession(route, keyID, session); err != nil {
		rec.Status = "error"
		rec.Error = err.Error()
		observeGatewayError(&rec, "gateway", "session_binding")
		status := http.StatusServiceUnavailable
		if errors.Is(err, errInferenceSessionChanged) {
			status = http.StatusConflict
			rec.Error = "conversation contains state from the previous backend; send the full plaintext conversation or start a new conversation"
		}
		apiError(w, status, rec.Error)
		return
	}
	if owner != "" {
		bound, bindErr := p.sessionAccount(route, keyID, session, owner)
		if bindErr != nil || bound != owner {
			rec.Status, rec.Error = "error", "previous response account binding is unavailable or changed"
			observeGatewayError(&rec, "gateway", "session_binding")
			apiError(w, http.StatusConflict, rec.Error)
			return
		}
	}
	selectStart := time.Now()
	attempt.ObserveStage("request_preparation", authFinished, selectStart)
	var account Account
	var release func()
	var boundCapture gatewayBoundSelectionCapture
	if failover != nil && failover.retrying {
		account, release, err = p.pickFailoverAccount(route, token, keyID)
	} else {
		selectionRoute := context.WithValue(route, gatewayBoundSelectionCaptureKey{}, &boundCapture)
		account, release, err = p.pickSessionAccount(selectionRoute, token, keyID, session)
	}
	attempt.ObserveStage("account_selection", selectStart, time.Now())
	if boundCapture.observation != nil {
		if failover != nil {
			failover.rememberBoundSelection(boundCapture.observation)
		} else {
			rec.BoundSelection = cloneGatewayBoundSelection(boundCapture.observation)
			rec.BoundSelection.ReplayClass = replayClass
		}
	}
	if err != nil {
		selectionObservation := attempt.Snapshot()
		if failover != nil && len(selectionObservation.Attempts) == 0 && (selectionObservation.AttemptsTruncated == nil || !*selectionObservation.AttemptsTruncated) {
			failover.localSelectionFailure = true
		}
		officialObserveAttemptFailure(r.Context(), err)
		var unavailable *gatewayBoundAccountUnavailableError
		if failover != nil && errors.As(err, &unavailable) {
			failover.account, failover.owner = unavailable.Account, unavailable.Owner
		}
		rec.Status = "error"
		rec.Error = err.Error()
		observeGatewayError(&rec, "gateway", "account_selection")
		writeGatewaySelectionError(w, err)
		return
	}
	defer release()
	if failover != nil {
		failover.account = account
		if !failover.retrying {
			failover.owner = gatewayAccountRef(account)
		}
	}
	if owner != "" && owner != gatewayAccountRef(account) {
		rec.Status, rec.Error = "error", "previous response belongs to a different account identity"
		observeGatewayError(&rec, "gateway", "session_binding")
		apiError(w, http.StatusConflict, rec.Error)
		return
	}
	rec.Account = account.Name
	rec.AccountID = account.ID
	rec.AccountCredentialVersion = account.CredentialVersion
	rec.Groups = append([]string(nil), account.Groups...)
	proxyStart := time.Now()
	proxy, err := p.gatewayRequestProxy(route, account)
	attempt.ObserveStage("proxy_selection", proxyStart, time.Now())
	if err != nil {
		apiError(w, 503, "account proxy is unavailable")
		return
	}
	if proxy != nil {
		rec.ProxyName = proxy.Name
	}
	lease, stopLease := p.officialFailoverLease(route, keyID, session, account)
	defer stopLease()
	resolved, err := p.resolveBPSModelRoute(lease, r, account, proxy, keyID, prepared, settings)
	if err == nil && resolved.CatalogChecked {
		err = p.authorizeGatewayRetry(lease, token, keyID, account)
	}
	if err != nil {
		selectionObservation := attempt.Snapshot()
		if failover != nil && len(selectionObservation.Attempts) == 0 && (selectionObservation.AttemptsTruncated == nil || !*selectionObservation.AttemptsTruncated) {
			failover.localSelectionFailure = true
		}
		rec.Status, rec.Error = "error", err.Error()
		status := http.StatusBadGateway
		source, phase := "upstream", "model_routing"
		var routing *bpsModelRoutingError
		if errors.As(err, &routing) {
			status = routing.StatusCode
		}
		var authorization *gatewayRecoveryAuthorizationError
		if errors.As(err, &authorization) {
			status, source, phase = authorization.status, "gateway", "authorization"
		}
		if lease.Err() != nil {
			status, source, phase = gatewayTransportFailureStatus(lease.Err()), "client", "request_canceled"
		}
		observeGatewayError(&rec, source, phase)
		observeAppServerCLIFailoverHTTP(r.Context(), status, nil)
		if routing != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "invalid_request_error", "code": routing.Code, "param": routing.Param, "message": routing.Error()}})
			return
		}
		apiError(w, status, rec.Error)
		return
	}
	lease = withGatewayCooldownModel(lease, prepared.Model)
	if err := p.checkGatewayCooldown(lease, account); err != nil {
		rec.Status, rec.Error = "error", err.Error()
		observeGatewayError(&rec, "gateway", "account_selection")
		if failover != nil {
			failover.localSelectionFailure = true
		}
		officialObserveAttemptFailure(r.Context(), bpsResolvedCooldownFailure(lease, account, err))
		writeGatewaySelectionError(w, err)
		return
	}
	if resolved.Changed {
		w.Header().Set("X-Requested-Model", resolved.RequestedModel)
		w.Header().Set("X-Resolved-Model", resolved.ResolvedModel)
		if resolved.ResolvedEffort != "" {
			w.Header().Set("X-Resolved-Reasoning-Effort", resolved.ResolvedEffort)
		}
	}
	// Account scoping returns a new body when identities change; keep the
	// validated, unscoped request for continuation without copying its history.
	historyBody := body
	cacheSettings := settings
	cacheSettings.PromptCacheKeyEnabled = true
	body, err = prepareGatewayAccountPromptCache(body, keyID, account, cacheSettings, true)
	if err != nil {
		apiError(w, 400, err.Error())
		return
	}
	// The account identity participates in the BPS task namespace and is stable
	// across access-token rotation, while a replaced owner starts a new scope.
	err = bpsBindPreparedAccount(prepared, body, bpsTaskID(keyID, session, gatewayAccountRef(account)))
	if err != nil {
		apiError(w, 400, err.Error())
		return
	}
	if err = applyBPSDefaultPromptCache(prepared, keyID, account, settings); err != nil {
		apiError(w, 400, err.Error())
		return
	}
	if warmup, _ := r.Context().Value(bpsWarmupKey{}).(bool); warmup {
		id := "resp_warmup_" + strings.ReplaceAll(bpsUUID(), "-", "")
		request, _ := bpsHistoryInputs(historyBody, prepared, nil)
		if err := p.bpsSaveHistory(lease, keyID, id, bpsHistoryRecord{Owner: gatewayAccountRef(account), Session: session, Request: request}); err != nil {
			apiError(w, 503, err.Error())
			return
		}
		release()
		rec.RequestType = "websocket"
		rec.Status = "ok"
		w.Header().Set("Content-Type", "text/event-stream")
		response := map[string]any{"id": id, "status": "completed", "output": []any{}, "model": prepared.Model, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}
		for _, kind := range []string{"response.created", "response.completed"} {
			event, _ := json.Marshal(map[string]any{"type": kind, "response": response})
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, event)
		}
		_ = http.NewResponseController(w).Flush()
		return
	}
	if err := p.prepareBPSAttachments(lease, account, proxy, keyID, prepared, settings, func(ctx context.Context) error {
		return p.authorizeGatewayRetry(ctx, token, keyID, account)
	}); err != nil {
		rec.Status, rec.Error = "error", err.Error()
		status := http.StatusBadGateway
		var attachment *bpsAttachmentError
		if errors.As(err, &attachment) {
			status = attachment.StatusCode
		}
		var authorization *gatewayRecoveryAuthorizationError
		if errors.As(err, &authorization) {
			status = authorization.status
		}
		if lease.Err() != nil {
			status = gatewayTransportFailureStatus(lease.Err())
		}
		if attachment != nil && attachment.Stage == "image" {
			observeGatewayError(&rec, "client", "request_validation")
		} else if authorization != nil {
			observeGatewayError(&rec, "gateway", "authorization")
		} else {
			observeGatewayError(&rec, "upstream", "attachment")
		}
		observeAppServerCLIFailoverHTTP(r.Context(), status, nil)
		apiError(w, status, rec.Error)
		return
	}
	upPath := "/responses"
	if prepared.Compact {
		upPath = "/responses/compact"
	}
	if !prepared.Compact && p.PromptCacheObserver != nil {
		if bpsSharedPromptCacheApplied(prepared, keyID, account, settings) {
			cacheKeyMode = "shared_api_key"
		} else if cacheKeyMode == "absent" && gatewayPromptCacheKeyValue(prepared.Body) != "" {
			cacheKeyMode = "generated_prefix"
		}
		// Observe the exact account-bound upstream body, including BPS tool
		// declarations. Task/turn identifiers are deliberately excluded from
		// the observer's configuration digest. No prompt text is persisted.
		rec.PromptCache = p.PromptCacheObserver.Observe(r, prepared.Body, keyID, account, cacheKeyMode)
	}
	res, err := p.bpsRequest(lease, r, account, proxy, keyID, upPath, prepared.Body, settings, &rec)
	if err != nil {
		if recordGatewayHTTPReadCancellation(&rec, r.Context(), err) {
			return
		}
		rec.Status = "error"
		rec.Error = "upstream connection failed"
		observeGatewayTransportError(&rec, err, "response_headers")
		apiError(w, gatewayTransportFailureStatus(err), rec.Error)
		return
	}
	defer res.Body.Close()
	observeGatewayHeaders(&rec, res.Header)
	if res.StatusCode >= 300 {
		observeAppServerCLIFailoverHTTP(r.Context(), res.StatusCode, nil)
		p.bpsWriteUpstreamError(r.Context(), w, res, &rec, attempt)
		return
	}
	recordToolPolicyRejection := func(phase string, value bps.ToolPolicyRejection) {
		if rec.ToolPolicy == nil || rec.ToolPolicy.RejectionPhase != "" {
			return
		}
		value = value.WithRequestDeclarationEvidence(toolPolicyInboundBody, prepared.Body,
			res.Header.Get("X-OpenAI-Internal-Basispoints-Agent-Tools-Sha256"),
			res.Header.Get("X-OpenAI-Internal-Basispoints-Agent-Prompt-Sha256"))
		rec.ToolPolicy.RecordRejection(phase, value)
	}
	observation := gatewayObservation{record: &rec, started: start}
	var cooldownIdentity gatewayHTTPCooldownIdentity
	seenCalls := map[string]bool{}
	options := bps.StreamOptions{Unbounded: true, NormalizeItem: func(item map[string]json.RawMessage) error {
		if prepared.Structured != nil {
			var kind, name string
			_ = json.Unmarshal(item["type"], &kind)
			_ = json.Unmarshal(item["name"], &name)
			// Stubs are checked when their complete identity becomes available.
			// Internal formatting never consumes the client's tool-call budget.
			if (kind == "function_call" && name == "") || bps.IsStructuredFormattingItem(prepared.Structured, item) {
				return nil
			}
		}
		toolItem := bps.CaptureToolPolicyItem(item)
		if err := prepared.Tools.NormalizeItem(item); err != nil {
			recordToolPolicyRejection("normalize_item", toolItem.Resolve(prepared))
			return err
		}
		var kind, id, call, execution string
		_ = json.Unmarshal(item["type"], &kind)
		_ = json.Unmarshal(item["execution"], &execution)
		if kind == "function_call" || kind == "custom_tool_call" || (kind == "tool_search_call" && execution == "client") {
			_ = json.Unmarshal(item["id"], &id)
			_ = json.Unmarshal(item["call_id"], &call)
			identity := id
			if identity == "" {
				identity = call
			}
			if identity != "" && !seenCalls[identity] {
				if prepared.Policy.ParallelToolCalls != nil && !*prepared.Policy.ParallelToolCalls && len(seenCalls) > 0 {
					recordToolPolicyRejection("parallel_policy", toolItem.Resolve(prepared))
					return errors.New("parallel tool calls are disabled")
				}
				seenCalls[identity] = true
			}
		}
		return nil
	}, OnEvent: func(event []byte) error {
		observation.event(event)
		if prepared.Structured == nil {
			p.observeGatewayCooldownEvent(res, event, &cooldownIdentity)
		}
		observeAppServerCLIFailoverEvent(r.Context(), event)
		if isWebSocket {
			return commitBPSWebSocketCreated(w, r.Context(), event)
		}
		return nil
	}}
	options.OnTerminalMetadata = func(metadata []byte) { recordBPSTerminalUsage(&rec, metadata) }
	if settings.CacheCreationAsReadEnabled || settings.CacheReadRatioEnabled {
		options.ClientUsageTransform = func(raw []byte) []byte {
			if settings.CacheCreationAsReadEnabled {
				raw = bps.ExportCacheCreationAsRead(raw)
			}
			if settings.CacheReadRatioEnabled {
				raw = bps.ExportCacheReadRatio(raw, settings.CacheReadRatioPercent)
			}
			return raw
		}
	}
	options.OnUpstreamTerminal = func(event []byte) error {
		actualToken := ""
		if res.Request != nil {
			actualToken = bearerToken(res.Request)
		}
		p.captureBPSNaturalFailure(account, event, actualToken, token)
		if prepared.Structured != nil {
			p.observeGatewayCooldownEvent(res, event, &cooldownIdentity)
		}
		return nil
	}
	options.OnComplete = func(result bps.StreamResult) error {
		if !prepared.Compact {
			toolResponse := bps.CaptureToolPolicyRejection(prepared, nil)
			if err := prepared.Tools.ValidateResponse(result.Output, prepared.Policy); err != nil {
				recordToolPolicyRejection("validate_response", toolResponse)
				return err
			}
		}
		if !result.HistoryComplete {
			return errors.New("response history exceeds continuation cache limits")
		}
		if err := p.authorizeGatewayRetry(lease, token, keyID, account); err != nil {
			var authorization *gatewayRecoveryAuthorizationError
			if errors.As(err, &authorization) {
				rec.ErrorSource, rec.ErrorPhase = "gateway", "authorization"
			}
			return err
		}
		if failover != nil {
			if err := failover.commitOwner(); err != nil {
				return err
			}
		}
		request, output := bpsHistoryInputs(historyBody, prepared, result.Output)
		if err := p.bpsSaveHistoryWithOpaqueOwners(lease, keyID, result.ResponseID, bpsHistoryRecord{Owner: gatewayAccountRef(account), Session: session, Request: request, Output: output}, result.Output); err != nil {
			return err
		}
		// A client can send the next turn immediately after receiving completed.
		// Upstream computation is finished; release before exposing the terminal.
		release()
		return nil
	}
	if prepared.Compact && path == "/responses/compact" {
		raw, err := io.ReadAll(res.Body)
		if recordGatewayHTTPReadCancellation(&rec, r.Context(), err) {
			return
		}
		if err != nil || !json.Valid(raw) {
			apiError(w, 502, "invalid upstream compaction response")
			return
		}
		var result struct {
			Output []json.RawMessage `json:"output"`
			ID     string            `json:"id"`
			Status string            `json:"status"`
			Error  json.RawMessage   `json:"error"`
		}
		decoded := json.Unmarshal(raw, &result) == nil
		meteredEnvelope := decoded && recordBPSCompactResponseUsage(&rec, raw)
		if !meteredEnvelope || !bpsHasCompaction(result.Output) ||
			(result.Status != "" && result.Status != "completed") || (len(result.Error) > 0 && string(result.Error) != "null") {
			// Recording measured usage must not accept unusable compacted state.
			rec.Status = "error"
			if rec.Error == "" {
				rec.Error = "compaction response did not include compacted state"
			}
			observeGatewayError(&rec, "upstream", "bps_protocol")
			apiError(w, 502, "compaction response did not include compacted state")
			return
		}
		if err := options.OnComplete(bps.StreamResult{ResponseID: result.ID, Completed: true, HistoryComplete: true, Output: result.Output, Response: raw}); err != nil {
			rec.Status, rec.Error = "error", err.Error()
			observeGatewaySessionCommitError(&rec, err)
			var authorization *gatewayRecoveryAuthorizationError
			if errors.As(err, &authorization) {
				payload := bpsStructuredValidationFailure(bps.StreamResult{Response: raw}, err)
				writeBPSLocalFailureJSON(w, payload, authorization.status)
				return
			}
			apiError(w, 502, err.Error())
			return
		}
		rec.Status = "ok"
		copyResponseHeaders(w.Header(), res.Header)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bpsClientUsage(raw, settings))
		return
	}
	copyResponseHeaders(w.Header(), res.Header)
	dst := io.Writer(io.Discard)
	if prepared.ClientStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		controller := http.NewResponseController(w)
		_ = controller.SetWriteDeadline(time.Now().Add(60 * time.Second))
		_ = controller.Flush()
		_ = controller.SetWriteDeadline(time.Time{})
		defer controller.SetWriteDeadline(time.Time{})
		options.Flush = func() error {
			_ = controller.SetWriteDeadline(time.Now().Add(60 * time.Second))
			err := controller.Flush()
			_ = controller.SetWriteDeadline(time.Time{})
			return err
		}
		dst = bpsDeadlineWriter{writer: w, controller: controller}
	}
	var result bps.StreamResult
	if prepared.Compact {
		result, err = bps.CompactJSONToSSE(lease, dst, res.Body, options)
	} else {
		result, err = bps.StreamStructured(lease, dst, res.Body, options, prepared.Structured)
	}
	// Event observation records identity and latency, not token usage. Persist
	// the real terminal object for both SSE and buffered JSON, including usage
	// from an explicit failed/incomplete terminal when one was received.
	if len(result.Response) > 0 {
		recordBPSTerminalUsage(&rec, result.Response)
	}
	if err != nil {
		if recordGatewayHTTPReadCancellation(&rec, r.Context(), err) {
			if prepared.ClientStream {
				panic(http.ErrAbortHandler)
			}
			return
		}
		rec.Status = "error"
		rec.Error = err.Error()
		var terminal *bps.TerminalError
		var rejected *bps.StructuredValidationError
		var authorization *gatewayRecoveryAuthorizationError
		completionAccessDenied := errors.As(err, &authorization)
		observeGatewaySessionCommitError(&rec, err)
		if completionAccessDenied || prepared.Structured != nil && errors.As(err, &rejected) && result.TerminalType != "" && len(result.Response) > 0 {
			status := http.StatusBadGateway
			if completionAccessDenied {
				// This checkpoint runs after upstream completion. Preserve the
				// actual downstream HTTP status while reporting the local cause.
				rec.ErrorSource, rec.ErrorPhase = "gateway", "authorization"
				status = authorization.status
			} else {
				rec.StructuredMixed = bps.ObserveStructuredMixed(err)
				observeGatewayError(&rec, "gateway", "response_validation")
			}
			payload := bpsStructuredValidationFailure(result, err)
			if !prepared.ClientStream {
				writeBPSLocalFailureJSON(w, payload, status)
			} else {
				if options.OnEvent != nil {
					if sendErr := options.OnEvent(payload); sendErr != nil {
						panic(http.ErrAbortHandler)
					}
				}
				visible := payload
				if options.ClientUsageTransform != nil {
					visible = options.ClientUsageTransform(payload)
				}
				if _, sendErr := fmt.Fprintf(dst, "data: %s\n\n", visible); sendErr != nil {
					panic(http.ErrAbortHandler)
				}
				if options.Flush != nil {
					if sendErr := options.Flush(); sendErr != nil {
						panic(http.ErrAbortHandler)
					}
				}
			}
			return
		}
		observeGatewayError(&rec, "upstream", "bps_protocol")
		if !prepared.ClientStream {
			writeBPSTerminalJSONError(w, err)
		} else if !errors.As(err, &terminal) {
			panic(http.ErrAbortHandler)
		}
		return
	}
	rec.Status = "ok"
	if !prepared.ClientStream {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bpsClientUsage(result.Response, settings))
	}
}

func bpsClientUsage(raw []byte, settings Settings) []byte {
	if settings.CacheCreationAsReadEnabled {
		raw = bps.ExportResponseCacheCreationAsRead(raw)
	}
	if settings.CacheReadRatioEnabled {
		raw = bps.ExportResponseCacheReadRatio(raw, settings.CacheReadRatioPercent)
	}
	return raw
}

type bpsDeadlineWriter struct {
	writer     io.Writer
	controller *http.ResponseController
}

func (w bpsDeadlineWriter) Write(raw []byte) (int, error) {
	_ = w.controller.SetWriteDeadline(time.Now().Add(60 * time.Second))
	n, err := w.writer.Write(raw)
	if err != nil {
		_ = w.controller.SetWriteDeadline(time.Time{})
	}
	return n, err
}

func bpsHasCompaction(items []json.RawMessage) bool {
	for _, item := range items {
		var x struct{ Type, EncryptedContent string }
		var fields map[string]json.RawMessage
		if json.Unmarshal(item, &fields) == nil {
			_ = json.Unmarshal(fields["type"], &x.Type)
			_ = json.Unmarshal(fields["encrypted_content"], &x.EncryptedContent)
			if x.Type == "compaction" && x.EncryptedContent != "" {
				return true
			}
		}
	}
	return false
}

// The original request has already passed bps.Prepare validation. Preserve its
// array history by reference when no control removal/conversion is needed;
// bpsSaveHistory owns the final JSON encoding and the exact serialized size check.
func bpsHistoryInputs(original []byte, prepared *bps.Request, output []json.RawMessage) (json.RawMessage, []json.RawMessage) {
	body, reusable := bpsHistoryObjectFieldsReusable(original)
	_, previous := body["previous_response_id"]
	_, trigger := body["compaction_trigger"]
	input := bytes.TrimSpace(body["input"])
	if !prepared.Compact && reusable && !previous && !trigger && len(input) > 0 && input[0] == '[' {
		return original, output
	}
	delete(body, "previous_response_id")
	delete(body, "compaction_trigger")
	if prepared.Compact {
		body["input"], _ = json.Marshal(output)
		output = nil
	} else {
		if len(body["input"]) == 0 || string(body["input"]) == "null" {
			body["input"] = json.RawMessage("[]")
		}
		var s string
		if json.Unmarshal(body["input"], &s) == nil {
			body["input"], _ = json.Marshal([]any{map[string]any{"role": "user", "type": "message", "content": []any{map[string]string{"type": "input_text", "text": s}}}})
		}
	}
	raw, _ := json.Marshal(body)
	return raw, output
}

func (p *Proxy) bpsRequest(ctx context.Context, r *http.Request, account Account, proxy *ProxyEntry, keyID, path string, body []byte, settings Settings, rec *RequestRecord) (*http.Response, error) {
	query := url.Values{}
	method := http.MethodPost
	if path == "/responses/access" {
		method = http.MethodGet
		query.Set("include_models", "true")
	}
	target, err := gatewayUpstreamURL(p.BaseURL, path, query)
	if err != nil {
		return nil, err
	}
	up, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyBPSHeaders(up.Header, account)
	up.Header.Set("Accept", "text/event-stream")
	if path != "/responses" {
		up.Header.Set("Accept", "application/json")
	}
	observer := gatewayAttemptObservationFrom(ctx)
	observer.ObserveSent(path, body)
	mode := gatewayClientHTTP
	timeout := time.Duration(settings.UpstreamResponseHeaderTimeoutSeconds) * time.Second
	if path == "/responses/compact" {
		mode = gatewayClientCompact
		timeout = time.Duration(settings.UpstreamCompactTimeoutSeconds) * time.Second
	}
	client := p.gatewayRequestClientWithHeaderTimeout(ctx, proxy, mode, timeout)
	// A blocked or handshake-rate-limited account/egress pairing skips the
	// known-wasted dial and keeps its attempt labelled as the HTTP request it
	// actually starts with.
	preferWebSocket := path == "/responses" && settings.PreferWebsocket && !p.wsDialSkipped(account.ID, proxy, gatewayObservedRoute(path))
	attemptKind := "request"
	if preferWebSocket {
		attemptKind = "websocket"
	}
	return p.doObservedAccountRequest(rec, client, up, account, func(c *http.Client, req *http.Request) (*http.Response, error) {
		// Credential refresh is local to the same selected owner; regenerate BPS
		// identity headers from the actual credential epoch before dispatch.
		var response *http.Response
		var exchangeErr error
		if preferWebSocket {
			response, exchangeErr = p.bpsWebSocketRequest(req, proxy, body, settings)
			if errors.Is(exchangeErr, errPreferredWebSocketUnavailable) {
				first, _ := req.Context().Value(gatewayTransportAttemptKey{}).(*gatewayAttemptHandle)
				finishGatewayObservedAttempt(first, response, exchangeErr)
				if response != nil && response.Body != nil {
					_ = response.Body.Close()
				}
				fallback := gatewayAttemptObservationFrom(req.Context()).Begin("request", req.URL.EscapedPath(), time.Now())
				response, exchangeErr = doStreamingRequestWithIdleTimeout(c, req, time.Duration(settings.UpstreamStreamIdleTimeoutSeconds)*time.Second)
				finishGatewayObservedAttempt(fallback, response, exchangeErr)
			}
		} else if path == "/responses" {
			response, exchangeErr = doStreamingRequestWithIdleTimeout(c, req, time.Duration(settings.UpstreamStreamIdleTimeoutSeconds)*time.Second)
		} else {
			response, exchangeErr = doCompactRequestWithTimeout(c, req, timeout)
		}
		if exchangeErr == nil {
			p.capturePrivate422(req, response, account, proxy, keyID)
		}
		return response, exchangeErr
	}, func(updated Account) error { return p.authorizeGatewayRetry(ctx, bearerToken(r), keyID, updated) }, attemptKind)
}

func (p *Proxy) bpsWriteUpstreamError(ctx context.Context, w http.ResponseWriter, res *http.Response, rec *RequestRecord, attempt *gatewayAttemptObservation) {
	copyResponseHeaders(w.Header(), res.Header)
	var received bytes.Buffer
	prefix, err := copyGatewayErrorBody(&received, res.Body, attempt)
	p.captureBPSNaturalHTTPFailure(res, prefix)
	p.observeGatewayPaymentBody(res, prefix)
	observeAppServerCLIFailoverHTTP(ctx, res.StatusCode, prefix)
	observeBPSAccountRejection(ctx, res.StatusCode, prefix)
	raw := publicerrors.JSON(received.Bytes(), true)
	rec.Status = "error"
	rec.HTTPCode = res.StatusCode
	observeGatewayError(rec, "upstream", "response_headers")
	rec.Error = publicerrors.Message(errorFromPayload(prefix))
	if rec.Error == "" {
		rec.Error = fmt.Sprintf("upstream returned HTTP %d", res.StatusCode)
	}
	status := res.StatusCode
	// Catalog GETs do not enter account failover. This is already their final
	// verified upstream response, so apply the same public auth distinction.
	if officialFailoverFrom(ctx) == nil && gatewayUpstreamAccountAuthenticationFailure(rec, &officialProtocolError{status: status, source: "upstream"}) {
		status = http.StatusServiceUnavailable
		rec.HTTPCode = status
		w.Header().Del("WWW-Authenticate")
		w.Header().Del("Content-Encoding")
		w.Header().Set("Content-Type", "application/json")
		raw = []byte(gatewayUpstreamAuthenticationUnavailableBody)
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(status)
	if _, writeErr := w.Write(raw); err == nil {
		err = writeErr
	}
	if err != nil {
		panic(http.ErrAbortHandler)
	}
}

func (p *Proxy) bpsModels(w http.ResponseWriter, r *http.Request, rec *RequestRecord, settings Settings) {
	route := withGatewayRequestRoute(r.Context(), rec.KeyID, "")
	defer stopGatewayRequestRoute(route)
	defer observeGatewayAdmissionLoss(rec, route)
	account, release, err := p.pickSessionAccount(route, bearerToken(r), rec.KeyID, "")
	if err != nil {
		writeGatewaySelectionError(w, err)
		return
	}
	defer release()
	rec.Account = account.Name
	rec.AccountID = account.ID
	rec.AccountCredentialVersion = account.CredentialVersion
	proxy, err := p.gatewayRequestProxy(route, account)
	if err != nil {
		apiError(w, 503, "account proxy unavailable")
		return
	}
	res, err := p.bpsRequest(route, r, account, proxy, rec.KeyID, "/responses/access", nil, settings, rec)
	if err != nil {
		apiError(w, 502, "upstream model catalog unavailable")
		return
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		p.bpsWriteUpstreamError(route, w, res, rec, gatewayAttemptObservationFrom(route))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		apiError(w, 502, "model catalog read failed")
		return
	}
	// Codex metadata is explicitly negotiated by client_version or the native
	// /models route. The OpenAI /v1/models route uses its standard list shape.
	// Raw access capabilities are cached separately; response envelopes are not.
	var catalog []byte
	if r.URL.Path == "/v1/models" && r.URL.Query().Get("client_version") == "" {
		catalog, err = bps.OpenAIModelsFromAccess(raw)
	} else {
		catalog, err = bps.ModelsFromAccess(raw)
	}
	if err != nil {
		apiError(w, 502, err.Error())
		return
	}
	if access, parseErr := bps.ParseAccessCatalog(raw); parseErr == nil {
		p.rememberBPSModelCatalog(account, proxy, rec.KeyID, access)
	}
	rec.Status = "ok"
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(catalog)
}
