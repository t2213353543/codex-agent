package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

func officialWriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (p *Proxy) gatewayOfficialOnce(w http.ResponseWriter, r *http.Request, mode string) {
	start := time.Now()
	path := strings.TrimPrefix(r.URL.Path, "/v1")
	var key, name string
	var rec *RequestRecord
	var attempt *gatewayAttemptObservation
	var errorWriter *officialErrorResponseWriter
	if (path == "/responses" || path == "/responses/compact") && !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		rec = &RequestRecord{Time: start.Unix(), Endpoint: strings.TrimPrefix(path, "/"), RequestType: "official_app_server", IP: clientIP(r)}
		errorWriter = &officialErrorResponseWriter{gatewayResponseWriter: &gatewayResponseWriter{ResponseWriter: w, record: rec}, phase: "authentication"}
		w = errorWriter
		defer func() {
			errorWriter.observe()
			if rec.Status != "ok" && r.Context().Err() != nil {
				recordOfficialFailure(rec, r.Context(), r.Context().Err(), "request", 503)
			}
			applyGatewayAttemptSnapshot(rec, attempt)
			if rec.Status == "" {
				rec.Status = "ok"
			}
			if failover := officialFailoverFrom(r.Context()); failover != nil {
				failover.record, failover.key = rec, key
			} else {
				p.finishRecord(rec, start, key)
			}
		}()
	}
	token := bearerToken(r)
	var err error
	key, name, _, err = p.resolveGatewayKeyContext(r.Context(), token)
	authFinished := time.Now()
	if rec != nil {
		rec.KeyID, rec.KeyName = key, name
		observeGatewayAuthentication(rec, start, authFinished)
	}
	if err != nil {
		apiError(w, 503, "key authorization unavailable")
		return
	}
	if key == "" {
		apiError(w, 401, "invalid API key")
		return
	}
	if p.Official == nil {
		apiError(w, 503, "official Codex runtime is unavailable")
		return
	}
	if strings.HasPrefix(path, "/codex/tasks") {
		mode = inferenceCodexCLI
	} else if path == "/codex/app-server" {
		mode = inferenceAppServer
	}
	r = r.WithContext(p.officialAuthContext(withInferenceMode(r.Context(), mode), token, key))
	if strings.HasPrefix(path, "/codex/tasks/") {
		parts := strings.Split(strings.TrimPrefix(path, "/codex/tasks/"), "/")
		p.Official.mu.Lock()
		task := p.Official.tasks[parts[0]]
		p.Official.mu.Unlock()
		if task == nil || task.key != key {
			apiError(w, 404, "task not found")
			return
		}
		action := ""
		if len(parts) > 1 {
			action = parts[1]
		}
		if len(parts) > 2 {
			apiError(w, 404, "task endpoint not found")
			return
		}
		if action != "cancel" {
			accounts, _, _, accessErr := p.candidatesForContext(r.Context(), token)
			if accessErr != nil {
				apiError(w, 503, "task account authorization unavailable")
				return
			}
			allowed := false
			for _, a := range accounts {
				if gatewayAccountRef(a) == task.owner {
					allowed = true
					break
				}
			}
			if !allowed {
				apiError(w, 403, "task account is no longer available to this key")
				return
			}
		}
		task.serve(w, r, action)
		return
	}
	if errorWriter != nil {
		errorWriter.phase = "settings"
	}
	settings, err := p.settingsValue()
	if err != nil {
		apiError(w, 503, "settings unavailable")
		return
	}
	if path == "/codex/app-server" && !settings.WebsocketEnabled {
		apiError(w, http.StatusForbidden, "WebSocket is disabled")
		return
	}
	if path == "/responses" && r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		if !settings.WebsocketEnabled {
			apiError(w, 403, "WebSocket is disabled")
			return
		}
		p.gatewayBPSWebSocket(w, r, key, name, settings)
		return
	}
	if path != "/responses" && path != "/responses/compact" && path != "/models" && path != "/codex/tasks" && path != "/codex/app-server" {
		apiError(w, 404, "unknown official runtime endpoint")
		return
	}
	if path == "/codex/app-server" && r.Method != http.MethodGet {
		apiError(w, 405, "use WebSocket GET")
		return
	}
	if path != "/models" && path != "/codex/app-server" && r.Method != http.MethodPost {
		apiError(w, 405, "use POST")
		return
	}
	if errorWriter != nil {
		errorWriter.phase = "request_read"
	}
	var body []byte
	if r.Method == http.MethodPost {
		body, err = readOfficialBody(r)
		if err != nil {
			if r.Context().Err() != nil {
				recordOfficialFailure(rec, r.Context(), r.Context().Err(), "request_read", 400)
				return
			}
			apiError(w, 400, "invalid request body")
			return
		}
		if failover := officialFailoverFrom(r.Context()); failover != nil && !failover.retrying {
			failover.requestBody = append([]byte(nil), body...)
		}
	}
	if rec != nil {
		rec.KeyID, rec.KeyName = key, name
		var incoming struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &incoming)
		rec.Model, rec.Stream = incoming.Model, incoming.Stream
		observeGatewayRequest(rec, r.UserAgent(), "official_app_server", body)
		observeGatewayAuthentication(rec, start, authFinished)
		observeGatewayClientIdentity(rec, gatewayObservedClientRequestID(r), "request")
		var observed context.Context
		observed, attempt = withGatewayAttemptObservation(r.Context())
		r = r.WithContext(observed)
		attempt.ObserveInbound(r.URL.Path, body)
		errorWriter.phase = "request_preparation"
	}
	if path == "/responses/compact" && !settings.CompactEnabled || path == "/models" && !settings.ModelsEnabled {
		apiError(w, 403, "endpoint is disabled")
		return
	}
	session := gatewayExplicitSessionKey(r, body)
	owner := ""
	if path == "/responses" || path == "/responses/compact" {
		body, session, owner, err = p.bpsExpandHistory(r.Context(), key, body, session)
		if err != nil {
			if errors.Is(err, errBPSHistoryMissing) {
				officialWriteJSON(w, 400, map[string]any{"error": map[string]string{"code": "previous_response_not_found", "message": err.Error()}})
			} else {
				apiError(w, 409, err.Error())
			}
			return
		}
		var policySession string
		body, policySession, err = applyGatewayPolicy(r, body, key, settings)
		if err != nil {
			apiError(w, 400, err.Error())
			return
		}
		if session == "" {
			session = policySession
		}
	}
	var taskInput officialTaskInput
	if path == "/codex/tasks" {
		if json.Unmarshal(body, &taskInput) != nil {
			apiError(w, 400, "invalid task request")
			return
		}
		if taskInput.Resume != "" {
			p.Official.mu.Lock()
			prior := p.Official.tasks[taskInput.Resume]
			p.Official.mu.Unlock()
			if prior == nil || prior.key != key {
				apiError(w, 404, "resume task not found")
				return
			}
			owner = prior.owner
			session = "task_" + taskInput.Resume
		}
	}
	if session == "" {
		session = "official_" + bpsUUID()
	}
	failover := officialFailoverFrom(r.Context())
	if failover != nil {
		if failover.session != "" {
			session = failover.session
		} else {
			failover.session = session
		}
	}
	if attempt != nil {
		attempt.ObserveStage("request_preparation", authFinished, time.Now())
	}
	if err := p.bindInferenceSession(r.Context(), key, session); err != nil {
		apiError(w, http.StatusConflict, err.Error())
		return
	}
	routeParent := r.Context()
	if path == "/codex/tasks" {
		// A hosted task outlives its HTTP acknowledgement, but admission loss
		// must still cancel it. Detach only the client lifetime, not the lease.
		routeParent = context.WithoutCancel(routeParent)
	}
	route := withGatewayRequestRoute(routeParent, key, session)
	stopClientCancellation := context.AfterFunc(r.Context(), func() { stopGatewayRequestRoute(route) })
	defer stopClientCancellation()
	transferredRoute := false
	defer func() {
		if !transferredRoute {
			stopGatewayRequestRoute(route)
		}
	}()
	defer observeGatewayAdmissionLoss(rec, route)
	if err = p.bindInferenceSession(route, key, session); err != nil {
		apiError(w, 409, err.Error())
		return
	}
	if owner != "" && (failover == nil || !failover.retrying) {
		bound, err := p.sessionAccount(route, key, session, owner)
		if err != nil || bound != owner {
			apiError(w, 409, "account binding unavailable")
			return
		}
	}
	if errorWriter != nil {
		errorWriter.phase = "account_selection"
	}
	selectionStarted := time.Now()
	var account Account
	var release func()
	if failover != nil && failover.retrying {
		account, release, err = p.pickFailoverAccount(route, token, key)
	} else {
		account, release, err = p.pickSessionAccount(route, token, key, session)
	}
	if attempt != nil {
		attempt.ObserveStage("account_selection", selectionStarted, time.Now())
	}
	if err != nil {
		if failover != nil {
			officialObserveAttemptFailure(r.Context(), err)
		}
		var unavailable *gatewayBoundAccountUnavailableError
		if failover != nil && errors.As(err, &unavailable) {
			failover.account, failover.owner = unavailable.Account, unavailable.Owner
			officialObserveAttemptFailure(r.Context(), err)
		}
		writeGatewaySelectionError(w, err)
		return
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	if owner != "" && gatewayAccountRef(account) != owner && (failover == nil || !failover.retrying) {
		apiError(w, 409, "account identity changed")
		return
	}
	if failover != nil {
		failover.account = account
		if !failover.retrying {
			failover.owner = gatewayAccountRef(account)
		}
	}
	if rec != nil {
		rec.Account = account.Name
		rec.AccountID = account.ID
		rec.AccountCredentialVersion = account.CredentialVersion
		rec.Groups = append([]string(nil), account.Groups...)
	}
	if errorWriter != nil {
		errorWriter.phase = "proxy_selection"
	}
	proxyStarted := time.Now()
	proxy, err := p.gatewayRequestProxy(route, account)
	if attempt != nil {
		attempt.ObserveStage("proxy_selection", proxyStarted, time.Now())
	}
	if err != nil {
		apiError(w, 503, "account proxy unavailable")
		return
	}
	if path == "/codex/app-server" {
		rpcCtx, stop := p.keepGatewaySession(route, key, session, account)
		defer stop()
		p.officialRPCWebSocket(w, r.WithContext(rpcCtx), key, session, account, proxy)
		return
	}
	if path == "/codex/tasks" {
		taskCtx, stopLease := p.keepGatewaySession(route, key, session, account)
		taskCtx, stopTask := context.WithCancel(taskCtx)
		go func() {
			ticker := time.NewTicker(20 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-taskCtx.Done():
					return
				case <-ticker.C:
					checkCtx, stop := context.WithTimeout(taskCtx, 5*time.Second)
					err := p.authorizeGatewayRetry(checkCtx, token, key, account)
					stop()
					if err != nil {
						stopTask()
						return
					}
				}
			}
		}()
		endRelease := release
		task, err := p.Official.createTask(taskCtx, key, account, proxy, taskInput, func(t *officialTask) {
			stopTask()
			endRelease()
			stopLease()
			defer stopGatewayRequestRoute(route)
			rec := RequestRecord{Time: start.Unix(), KeyID: key, KeyName: name, Account: account.Name, AccountID: account.ID, Model: t.model, ReasoningEffort: t.effort, Endpoint: "codex/tasks", RequestType: "official_cli_task", Status: "ok", HTTPCode: 200}
			t.mu.Lock()
			rec.InputTokens = t.usage["input_tokens"]
			rec.OutputTokens = t.usage["output_tokens"]
			rec.CachedTokens = t.usage["cached_input_tokens"]
			if t.status != "completed" {
				rec.Status = t.status
				rec.Error = t.err
			}
			t.mu.Unlock()
			observeGatewayAdmissionLoss(&rec, route)
			p.finishRecord(&rec, start, key)
		})
		if err != nil {
			stopTask()
			stopLease()
			apiError(w, 503, err.Error())
			return
		}
		release = nil
		stopClientCancellation()
		transferredRoute = true
		officialWriteJSON(w, 202, task.snapshot())
		return
	}
	rpcCtx, stopLease := p.officialFailoverLease(route, key, session, account)
	defer stopLease()
	if path == "/models" {
		rpc, err := p.Official.startRPC(rpcCtx, account, proxy)
		if err != nil {
			apiError(w, 503, err.Error())
			return
		}
		defer rpc.close()
		models, err := officialModels(rpcCtx, rpc)
		if err != nil {
			apiError(w, 502, err.Error())
			return
		}
		officialWriteJSON(w, 200, map[string]any{"object": "list", "data": models, "models": models})
		return
	}
	var input officialResponseInput
	if json.Unmarshal(body, &input) != nil || input.Model == "" {
		apiError(w, 400, "model is required")
		return
	}
	if input.Reasoning.Effort == "" {
		input.Reasoning.Effort = "xhigh"
	}
	if len(input.Compact) == 0 {
		var original map[string]json.RawMessage
		_ = json.Unmarshal(body, &original)
		var items []map[string]json.RawMessage
		if json.Unmarshal(original["input"], &items) == nil {
			for _, item := range items {
				if string(item["type"]) == `"compaction_trigger"` {
					input.Compact = json.RawMessage(`{}`)
					break
				}
			}
		}
	}
	if !officialValidEffort(input.Reasoning.Effort) {
		apiError(w, 400, "invalid reasoning effort")
		return
	}
	if ws, _ := r.Context().Value(bpsWebSocketKey{}).(bool); !ws && path == "/responses" && ((input.Stream && !settings.HTTPStreamEnabled) || (!input.Stream && !settings.HTTPJSONEnabled)) {
		apiError(w, 403, "HTTP response mode disabled")
		return
	}
	items, err := officialPrepareInput(&input)
	if err != nil {
		apiError(w, 400, err.Error())
		return
	}
	if warmup, _ := r.Context().Value(bpsWarmupKey{}).(bool); warmup {
		id := "resp_warmup_" + strings.ReplaceAll(bpsUUID(), "-", "")
		if err = p.bpsSaveHistory(rpcCtx, key, id, bpsHistoryRecord{Owner: gatewayAccountRef(account), Session: session, Request: body}); err != nil {
			apiError(w, 503, err.Error())
			return
		}
		response := map[string]any{"id": id, "status": "completed", "model": input.Model, "output": []any{}, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, kind := range []string{"response.created", "response.completed"} {
			v, _ := json.Marshal(map[string]any{"type": kind, "response": response})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", v)
		}
		_ = http.NewResponseController(w).Flush()
		return
	}
	if errorWriter != nil {
		errorWriter.phase = "official_runtime"
	}
	runtimeStarted := time.Now()
	t, delta, err := p.Official.responseThread(rpcCtx, key, session, account, proxy, input, items)
	if rec != nil {
		elapsed := time.Since(runtimeStarted).Milliseconds()
		rec.OfficialRuntimeMs = &elapsed
	}
	if err != nil {
		status := recordOfficialFailure(rec, r.Context(), err, "official_runtime", 502)
		if status != 499 {
			apiError(w, status, err.Error())
		}
		return
	}
	if path == "/responses/compact" || len(input.Compact) > 0 && string(input.Compact) != "null" {
		t.mu.Unlock()
		p.officialCompact(w, r, rpcCtx, t, input, items, body, key, session, account, rec)
		return
	}
	rec.ReasoningEffort = input.Reasoning.Effort
	observation := gatewayObservation{record: rec, started: start}
	emit := func(event map[string]any) error {
		raw, err := json.Marshal(event)
		if err != nil {
			return err
		}
		observation.event(raw)
		observeOfficialModelSource(rec, raw)
		if !input.Stream {
			return nil
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(60 * time.Second))
		_, err = fmt.Fprintf(w, "data: %s\n\n", raw)
		if err == nil {
			err = http.NewResponseController(w).Flush()
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		return err
	}
	inferenceStarted := time.Now()
	result, output, err := t.runResponse(rpcCtx, input, delta, emit)
	elapsed := time.Since(inferenceStarted).Milliseconds()
	rec.OfficialInferenceMs = &elapsed
	if err != nil {
		status := recordOfficialFailure(rec, r.Context(), err, "response_event", 502)
		if status == 499 {
			return
		}
		_, code, source := officialFailureHTTP(err, status)
		errorType := "upstream_error"
		if status >= 400 && status < 500 {
			errorType = "invalid_request_error"
		}
		if input.Stream {
			_ = emit(map[string]any{"type": "error", "status": status, "error": map[string]string{"type": errorType, "code": code, "message": err.Error()}})
			rec.ErrorSource, rec.ErrorPhase = source, "response_event"
		} else {
			apiError(w, status, err.Error())
		}
		return
	}
	recordOfficialResponseOutcome(rec, result)
	var response map[string]json.RawMessage
	_ = json.Unmarshal(result, &response)
	var id string
	_ = json.Unmarshal(response["id"], &id)
	if err = p.authorizeGatewayRetry(rpcCtx, token, key, account); err == nil {
		if failover != nil {
			err = failover.commitOwner()
		}
	}
	if err == nil {
		err = p.bpsSaveHistory(rpcCtx, key, id, bpsHistoryRecord{Owner: gatewayAccountRef(account), Session: session, Request: body, Output: output})
	}
	if err != nil {
		status := recordOfficialFailure(rec, r.Context(), err, "history_persistence", 503)
		if status == 499 {
			return
		}
		rec.ErrorSource = "gateway"
		if input.Stream {
			_ = emit(map[string]any{"type": "error", "status": status, "error": map[string]string{"message": err.Error()}})
		} else {
			apiError(w, status, err.Error())
		}
		rec.ErrorSource, rec.ErrorPhase = "gateway", "history_persistence"
		return
	}
	release()
	release = nil
	rec.Status = "ok"
	rec.HTTPCode = 200
	recordOfficialResponseOutcome(rec, result)
	if input.Stream {
		if err := emit(map[string]any{"type": "response.completed", "response": json.RawMessage(result)}); err != nil {
			rec.Status = "aborted"
			rec.Error = "client response delivery failed"
		}
	} else {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(result); err != nil {
			rec.Status = "aborted"
			rec.Error = "client response delivery failed"
		}
	}
	recordOfficialResponseOutcome(rec, result)
}

func (p *Proxy) officialRPCWebSocket(w http.ResponseWriter, r *http.Request, key, session string, account Account, proxy *ProxyEntry) {
	native, err := p.Official.nativeSession(r.Context(), key, session, account, proxy)
	if err != nil {
		apiError(w, 503, err.Error())
		return
	}
	defer func() { native.last = time.Now(); native.mu.Unlock() }()
	rpc := native.rpc
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(-1)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var turnsMu sync.Mutex
	activeTurns := 0
	defer func() {
		turnsMu.Lock()
		active := activeTurns > 0
		turnsMu.Unlock()
		if active {
			rpc.close()
		}
	}()
	fail := make(chan error, 1)
	go func() {
		defer cancel()
		for {
			raw, err := rpc.receive(ctx)
			if err != nil {
				fail <- err
				return
			}
			if err := p.authorizeGatewayRetry(ctx, bearerToken(r), key, account); err != nil {
				rpc.close()
				fail <- err
				return
			}
			var notice struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(raw, &notice)
			if notice.Method == "turn/completed" {
				turnsMu.Lock()
				if activeTurns > 0 {
					activeTurns--
				}
				turnsMu.Unlock()
			}
			sendCtx, stop := context.WithTimeout(ctx, 60*time.Second)
			err = conn.Write(sendCtx, websocket.MessageText, raw)
			stop()
			if err != nil {
				rpc.requeue(raw)
				fail <- err
				return
			}
		}
	}()
	defer func() {
		cancel()
		_ = conn.CloseNow()
		select {
		case <-fail:
		case <-time.After(5 * time.Second):
		}
	}()
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var request map[string]json.RawMessage
		if json.Unmarshal(raw, &request) != nil {
			return
		}
		var method string
		_ = json.Unmarshal(request["method"], &method)
		if method == "initialize" || method == "initialized" || strings.HasPrefix(method, "account/login/") || method == "account/logout" {
			if id := request["id"]; len(id) > 0 {
				result := map[string]any{"id": id, "result": rpc.initialized}
				if method != "initialize" {
					delete(result, "result")
					result["error"] = map[string]any{"code": -32601, "message": "account login is managed by the gateway"}
				}
				data, _ := json.Marshal(result)
				if conn.Write(ctx, websocket.MessageText, data) != nil {
					return
				}
			}
			continue
		}
		if err = p.authorizeGatewayRetry(ctx, bearerToken(r), key, account); err != nil {
			return
		}
		if method == "thread/start" || method == "thread/resume" || method == "turn/start" {
			var params map[string]json.RawMessage
			_ = json.Unmarshal(request["params"], &params)
			if params == nil {
				params = map[string]json.RawMessage{}
			}
			params["cwd"], _ = json.Marshal(rpc.work)
			if method == "thread/start" {
				params["modelProvider"] = json.RawMessage(`"openai"`)
			}
			delete(params, "config")
			delete(params, "runtimeWorkspaceRoots")
			request["params"], _ = json.Marshal(params)
		}
		if method == "turn/start" || method == "review/start" || method == "command/exec" || method == "process/start" {
			turnsMu.Lock()
			activeTurns++
			turnsMu.Unlock()
		}
		if rpc.send(request) != nil {
			return
		}
	}
}

func (p *Proxy) officialCompact(w http.ResponseWriter, r *http.Request, ctx context.Context, t *officialThread, input officialResponseInput, items []json.RawMessage, body []byte, key, session string, account Account, rec *RequestRecord) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.awaiting {
		apiError(w, 409, "complete pending tools before compacting this App Server thread")
		return
	}
	started := time.Now()
	output, err := t.compactResponse(ctx, items)
	if rec != nil {
		elapsed := time.Since(started).Milliseconds()
		rec.OfficialInferenceMs = &elapsed
		rec.SentModel = input.Model
		yes := true
		rec.NativeCompaction = &yes
	}
	if err != nil {
		status := recordOfficialFailure(rec, r.Context(), err, "compaction", 502)
		if status != 499 {
			apiError(w, status, err.Error())
		}
		return
	}
	id := "resp_compact_" + strings.ReplaceAll(bpsUUID(), "-", "")
	response := map[string]any{"id": id, "object": "response.compaction", "status": "completed", "output": output}
	for key, value := range t.compactResponseDetails {
		response[key] = value
	}
	if rec != nil {
		raw, _ := json.Marshal(response)
		recordOfficialResponseOutcome(rec, raw)
	}
	request, _ := json.Marshal(map[string]any{"model": input.Model, "input": output})
	if err = p.authorizeGatewayRetry(ctx, bearerToken(r), key, account); err == nil {
		if failover := officialFailoverFrom(ctx); failover != nil {
			err = failover.commitOwner()
		}
	}
	if err == nil {
		err = p.bpsSaveHistory(ctx, key, id, bpsHistoryRecord{Owner: gatewayAccountRef(account), Session: session, Request: request})
	}
	if err != nil {
		status := recordOfficialFailure(rec, r.Context(), err, "history_persistence", 503)
		if rec != nil {
			rec.ErrorSource = "gateway"
		}
		if status != 499 {
			apiError(w, status, err.Error())
		}
		return
	}
	if input.Stream && strings.HasSuffix(r.URL.Path, "/responses") {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, kind := range []string{"response.created", "response.completed"} {
			data, _ := json.Marshal(map[string]any{"type": kind, "response": response})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		}
		_ = http.NewResponseController(w).Flush()
	} else {
		officialWriteJSON(w, 200, response)
	}
}
