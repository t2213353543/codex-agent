package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const gatewayWSWriteTimeout = 60 * time.Second

// Official ResponsesEndpoint is shared by its HTTP and WebSocket clients.
// Fixed allowlist: never turn a client path into an arbitrary upstream target.
func gatewayWSModelEndpoint(path string) (string, error) {
	switch path {
	case "/responses", "/v1/responses":
		return "/responses", nil
	case "/guardian", "/v1/guardian":
		return "/guardian", nil
	case "/guardian-classifier", "/v1/guardian-classifier":
		return "/guardian-classifier", nil
	default:
		return "", errors.New("unsupported model WebSocket endpoint")
	}
}

func gatewayWSValidStreamID(id string, unbounded ...bool) bool {
	if len(id) == 0 || len(id) > 256 && !gatewayPayloadUnboundedOption(unbounded) {
		return false
	}
	for i := range len(id) {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// Upstream framing belongs to the selected transport. Downstream connections
// continue using coder/websocket; a native message stream is not a fake 101.
type gatewayWSUpstream interface {
	Read(context.Context) (websocket.MessageType, []byte, error)
	Write(context.Context, websocket.MessageType, []byte) error
	CloseNow() error
	SetReadLimit(int64)
}

// websocketHTTPClient preserves account proxy routing and custom roots, while
// negotiating the HTTP/1.1 Upgrade used by Responses. Never mutate the shared
// HTTP transport: HTTP/SSE can independently keep using HTTP/2.
func websocketHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	result := *client
	result.Timeout = 0 // A WebSocket may outlive the ordinary HTTP request timeout.
	result.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if base, ok := transport.(*http.Transport); ok {
		clone := base.Clone()
		clone.DisableCompression = true // HTTP content codings do not apply to WS frames.
		clone.ForceAttemptHTTP2 = false
		clone.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
		clone.Protocols = new(http.Protocols)
		clone.Protocols.SetHTTP1(true)
		if clone.TLSClientConfig != nil {
			clone.TLSClientConfig = clone.TLSClientConfig.Clone()
			clone.TLSClientConfig.NextProtos = []string{"http/1.1"}
		}
		result.Transport = clone
	}
	return &result
}

func (p *Proxy) dialGatewayWebSocket(ctx context.Context, r *http.Request, account Account, proxy *ProxyEntry, settings Settings) (upstream gatewayWSUpstream, response *http.Response, dialErr error) {
	return p.dialGatewayWebSocketPrepared(ctx, r, account, proxy, settings, nil)
}

func (p *Proxy) dialGatewayWebSocketPrepared(ctx context.Context, r *http.Request, account Account, proxy *ProxyEntry, settings Settings, prepared []byte) (upstream gatewayWSUpstream, response *http.Response, dialErr error) {
	endpoint, err := gatewayWSModelEndpoint(r.URL.Path)
	if err != nil {
		return nil, nil, err
	}
	ctx = withGatewayNativeAccountScope(ctx, account)
	if route, _ := ctx.Value(gatewayRequestRouteKey{}).(*gatewayRequestRoute); p.nativeWebSocketFor(ctx) && route != nil {
		ctx = withGatewayNativeDispatchGuard(ctx, func(dispatchCtx context.Context) error {
			return p.authorizeGatewayRetry(dispatchCtx, bearerToken(r), route.keyID, account)
		})
	}
	headers := make(http.Header)
	if err := copyCodexRequestHeaders(headers, r.Header, settings); err != nil {
		return nil, nil, err
	}
	if requestInferenceMode(ctx) == inferenceCodexCLI {
		p.AppServerCLI.applyIdentity(headers)
	}
	if endpoint == "/responses" && requestInferenceMode(ctx) == inferenceCodexCLI && p.AppServerCLI != nil {
		_, headers, _, err = appServerMirrorNativeMetadata(prepared, headers)
		if err != nil {
			return nil, nil, err
		}
	}
	headers.Set("Authorization", "Bearer "+account.AccessToken)
	if account.AccountID != "" {
		headers.Set("ChatGPT-Account-Id", account.AccountID)
	}
	if route, _ := ctx.Value(gatewayRequestRouteKey{}).(*gatewayRequestRoute); route != nil {
		applyGatewayCacheHeaders(headers, p.gatewayInferenceBaseURL(ctx), endpoint, route.keyID, account, gatewayPromptCacheKeyValue(prepared), settings, true)
	}
	applyGatewayRoutingHint(headers, p.gatewayInferenceBaseURL(ctx), endpoint, account, prepared)
	attempt := gatewayAttemptObservationFrom(ctx).Begin("websocket", endpoint, time.Now())
	defer func() {
		var unsent *gatewayNativeDispatchUnsentError
		if response == nil && errors.As(dialErr, &unsent) {
			attempt.discardUnsent()
		} else {
			finishGatewayObservedAttempt(attempt, response, dialErr)
		}
	}()
	if p.nativeWebSocketFor(ctx) {
		return p.dialNativeGatewayWebSocketEndpointLifetime(ctx, ctx, headers, proxy, endpoint)
	}
	target, err := gatewayUpstreamURL(p.gatewayInferenceBaseURL(ctx), endpoint, nil)
	if err != nil {
		return nil, nil, err
	}
	client := p.gatewayRequestClient(ctx, proxy, gatewayClientWebSocket)
	// Dial only uses this context through the handshake. The returned connection
	// receives the full session context separately for each read/write.
	dialCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(dialCtx, target, &websocket.DialOptions{
		HTTPClient: client, HTTPHeader: headers,
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if conn == nil {
		return nil, response, err
	}
	return conn, response, err
}

func gatewayWSWrite(ctx context.Context, conn gatewayWSUpstream, typ websocket.MessageType, payload []byte) error {
	return gatewayWSWriteWithTimeout(ctx, conn, typ, payload, gatewayWSWriteTimeout)
}

func gatewayWSWriteWithTimeout(ctx context.Context, conn gatewayWSUpstream, typ websocket.MessageType, payload []byte, timeout time.Duration) error {
	writeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return conn.Write(writeCtx, typ, payload)
}

func gatewayWSError(ctx context.Context, conn *websocket.Conn, code int, message string) {
	gatewayWSRequestError(ctx, conn, nil, code, message)
}

func gatewayWSRequestError(ctx context.Context, conn *websocket.Conn, request []byte, code int, message string, retryAfter ...string) {
	event := map[string]any{
		"type": "error", "status": code,
		"error": map[string]string{"type": "proxy_error", "code": "gateway_error", "message": hidePaths(message)},
	}
	if len(retryAfter) > 0 && retryAfter[0] != "" {
		event["headers"] = map[string]string{"Retry-After": retryAfter[0]}
	}
	var incoming struct {
		StreamID string `json:"stream_id"`
	}
	if gatewayWSDecodeExact(request, map[string]any{"stream_id": &incoming.StreamID}) == nil && incoming.StreamID != "" {
		event["stream_id"] = incoming.StreamID
	}
	data, _ := json.Marshal(event)
	_ = gatewayWSWrite(ctx, conn, websocket.MessageText, data)
}

func gatewayWSStatus(err error) int {
	var authorization *gatewayRecoveryAuthorizationError
	if errors.As(err, &authorization) {
		return authorization.status
	}
	var failure *gatewayWSFailure
	if errors.As(err, &failure) {
		return failure.status
	}
	if status, _, recognized := gatewaySelectionFailure(err); recognized {
		return status
	}
	return http.StatusServiceUnavailable
}

func gatewayWSConnectionMessage(err error) string {
	var authorization *gatewayRecoveryAuthorizationError
	if errors.As(err, &authorization) {
		return authorization.Error()
	}
	return "upstream WebSocket connection failed"
}

type gatewayWSFailure struct {
	status     int
	message    string
	retryAfter string
}

func (e *gatewayWSFailure) Error() string { return e.message }

// gatewayWebSocket pins one upstream connection/account for its whole lifetime.
// No message is replayed after an ambiguous write or upstream disconnect.
func (p *Proxy) gatewayWebSocket(w http.ResponseWriter, r *http.Request, keyID, keyName string, settings Settings) {
	endpoint, endpointErr := gatewayWSModelEndpoint(r.URL.Path)
	if endpointErr != nil {
		apiError(w, http.StatusNotFound, "not found")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	ctx = withGatewayRequestRoute(ctx, keyID, "")
	defer stopGatewayRequestRoute(ctx)
	ctx, connectionObservation := withGatewayAttemptObservation(ctx)
	var account Account
	var proxy *ProxyEntry
	var upstream gatewayWSUpstream
	var headerStarted, headerObserved time.Time
	defer func() {
		if upstream != nil {
			_ = upstream.CloseNow()
		}
	}()
	connect := func(session, model string, prepared []byte) (*http.Response, error) {
		if !settings.StickySessionsEnabled {
			session = ""
		}
		routeCtx := withGatewayRequestRoute(ctx, keyID, session)
		routeCtx = withGatewayCacheAffinity(routeCtx, keyID, session, prepared, settings)
		if model != "" {
			routeCtx = withGatewayCooldownModel(routeCtx, model)
		}
		selectionStarted := time.Now()
		selected, release, err := p.pickSessionAccount(routeCtx, bearerToken(r), keyID, session)
		connectionObservation.ObserveStage("account_selection", selectionStarted, time.Now())
		if err != nil {
			return nil, err
		}
		defer release() // Idle sockets must not consume an inference slot.
		account = selected
		proxyStarted := time.Now()
		proxy, err = p.gatewayRequestProxy(routeCtx, account)
		connectionObservation.ObserveStage("proxy_selection", proxyStarted, time.Now())
		if err != nil {
			return nil, err
		}
		var response *http.Response
		headerStarted = time.Now()
		upstream, response, err = p.dialGatewayWebSocketPrepared(routeCtx, r, account, proxy, settings, prepared)
		if response != nil {
			headerObserved = time.Now()
		}
		if err != nil && response != nil && response.StatusCode == http.StatusUnauthorized {
			// The upgrade was rejected before any response.create was sent.
			// Recover this account once; never replay frames on an accepted socket.
			updated, recoverErr := p.recoverUnauthorized(routeCtx, account, p.gatewayRequestClient(routeCtx, proxy, gatewayClientHTTP))
			var ownerChanged *gatewayOwnerChangedError
			if errors.As(recoverErr, &ownerChanged) {
				if response.Body != nil {
					_ = response.Body.Close()
				}
				return nil, recoverErr
			}
			if recoverErr == nil {
				if response.Body != nil {
					_ = response.Body.Close()
				}
				if err := p.authorizeGatewayRetry(routeCtx, bearerToken(r), keyID, updated); err != nil {
					return nil, err
				}
				account = updated
				upstream, response, err = p.dialGatewayWebSocketPrepared(routeCtx, r, account, proxy, settings, prepared)
				if response != nil {
					headerObserved = time.Now()
				}
			}
		}
		if upstream != nil {
			if gatewayUnboundedPayload(ctx) {
				upstream.SetReadLimit(-1)
			} else {
				upstream.SetReadLimit(maxBody)
			}
		}
		p.observeGatewayCooldownResponse(routeCtx, account, response)
		return response, err
	}
	// A native CLI sends its session identity in headers. Connect first in that
	// case so its x-codex-turn-state and other upstream handshake metadata can be
	// forwarded. A body-only identity requires accepting, then reading create.
	session := gatewaySessionKey(r, nil)
	var response *http.Response
	if session != "" {
		var err error
		response, err = connect(session, "", nil)
		if err != nil {
			status := gatewayWSStatus(err)
			if _, retryAfter, recognized := gatewaySelectionFailure(err); recognized && retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			if response != nil {
				if response.Body != nil {
					defer response.Body.Close()
				}
				copyResponseHeaders(w.Header(), response.Header)
				if response.StatusCode >= 300 {
					status = response.StatusCode
					w.WriteHeader(status)
					if response.Body != nil {
						var body io.Reader = response.Body
						if !gatewayUnboundedPayload(ctx) {
							body = io.LimitReader(body, 2<<20)
						}
						copyErrorBody(w, body)
					}
					return
				}
			}
			apiError(w, status, gatewayWSConnectionMessage(err))
			return
		}
		copyResponseHeaders(w.Header(), response.Header)
	}
	downstream, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover})
	if err != nil {
		return
	}
	defer downstream.CloseNow()
	if gatewayUnboundedPayload(ctx) {
		downstream.SetReadLimit(-1)
	} else {
		downstream.SetReadLimit(maxBody)
	}
	// Read independently of routing/dialing: after Hijack the HTTP request
	// context alone cannot detect a client disappearing during upstream dial.
	type clientFrame struct {
		typ     websocket.MessageType
		payload []byte
	}
	frames := make(chan clientFrame, 1)
	readDone := make(chan struct{})
	turns := gatewayWSTurns{proxy: p, keyID: keyID, keyName: keyName, ip: clientIP(r), userAgent: r.UserAgent(), endpoint: endpoint, pending: make(map[string][]*gatewayWSTurn), bindings: make(map[string]string), unboundedPayload: gatewayUnboundedPayload(ctx)}
	turns.idleChanged = make(chan struct{}, 1)
	idleDone := make(chan struct{})
	go func() { defer close(idleDone); turns.watchIdle(ctx, cancel) }()
	defer func() { cancel(); <-idleDone }()
	turns.clientRequestID = gatewayObservedClientRequestID(r)
	turns.setHeaderTiming(headerStarted, headerObserved)
	go func() {
		defer close(readDone)
		for {
			typ, payload, err := downstream.Read(ctx)
			if err != nil {
				if ctx.Err() == nil {
					turns.observeError("client", "request_read")
				}
				cancel()
				return
			}
			select {
			case frames <- clientFrame{typ, payload}:
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() { cancel(); _ = downstream.CloseNow(); <-readDone }()
	defer func() {
		// Cover failures before the two pumps are running, including an
		// ambiguous first write. Slots belong to potentially live upstream
		// work until both sides have actually been closed.
		cancel()
		if upstream != nil {
			_ = upstream.CloseNow()
		}
		_ = downstream.CloseNow()
		turns.abortContext(ctx)
	}()
	if upstream != nil && settings.StickySessionsEnabled && session != "" {
		if err := turns.bind(ctx, session, account); err != nil {
			gatewayWSError(ctx, downstream, http.StatusServiceUnavailable, err.Error())
			return
		}
	}
	// The shortest configurable TTL is 60 seconds. Renew during long model
	// turns and idle connections to keep encrypted history on its account.
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := turns.renew(ctx); err != nil {
					if ctx.Err() == nil {
						turns.observeError("gateway", "session_lease")
					}
					gatewayWSError(ctx, downstream, http.StatusServiceUnavailable, err.Error())
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-renewDone }()
	requestWriteTimeout := gatewayWSWriteTimeout
	prepare := func(payload []byte) ([]byte, error) {
		requestWriteTimeout = gatewayWSWriteTimeout
		var event struct {
			Type string `json:"type"`
		}
		if gatewayWSDecodeExact(payload, map[string]any{"type": &event.Type}) != nil || event.Type == "" {
			return nil, errors.New("WebSocket messages must be JSON events with a type")
		}
		if event.Type != "response.create" {
			if upstream == nil {
				return nil, errors.New("first WebSocket event must be response.create")
			}
			return payload, nil
		}
		p.observeDashboardRealtimeWSCreate(payload)
		currentSettings, err := p.settingsValue()
		if err != nil {
			return nil, &gatewayWSFailure{status: http.StatusServiceUnavailable, message: "gateway settings unavailable"}
		}
		if !currentSettings.WebsocketEnabled {
			return nil, &gatewayWSFailure{status: http.StatusForbidden, message: "WebSocket gateway is disabled"}
		}
		// The upstream handshake fixes the connection's identity. A new turn
		// must reconnect after a profile change instead of silently using the
		// previous headers for the lifetime of a persistent WebSocket.
		if upstream != nil && (settings.CodexFingerprintEnabled != currentSettings.CodexFingerprintEnabled ||
			(currentSettings.CodexFingerprintEnabled && settings.CodexFingerprintProfile != currentSettings.CodexFingerprintProfile)) {
			return nil, &gatewayWSFailure{status: http.StatusConflict, message: "Codex fingerprint settings changed; reconnect the WebSocket"}
		}
		if upstream == nil {
			settings = currentSettings
		}
		// The CLI already chose a delta against a prior response using the
		// original cache key. Gateway setting changes cannot alter that chain.
		cacheSettings := currentSettings
		if endpoint != "/responses" {
			// Guardian frames are authored by the client reviewer/classifier.
			// Responses prompt/cache policies must not rewrite their context.
			cacheSettings.PromptCacheKeyEnabled = false
		}
		var priorCachePolicy gatewayWSCachePolicy
		var priorCacheKnown bool
		var previous string
		if err := gatewayWSDecodeExact(payload, map[string]any{"previous_response_id": &previous}); err != nil {
			return nil, errors.New("previous_response_id must be a string or null")
		}
		if previous != "" && endpoint == "/responses" {
			if policy, known := turns.cachePolicyForResponse(previous); known {
				priorCachePolicy, priorCacheKnown = policy, true
				if policy.rewriteTracked && policy.rewriteDigest != gatewayWSRewritePolicyDigest(currentSettings) {
					return nil, errors.New("gateway prompt or compatibility policy changed for previous_response_id; resend full input without previous_response_id")
				}
				cacheSettings.PromptCacheKeyEnabled = policy.enabled
			}
		}
		prepared, nextSession := payload, gatewaySessionKey(r, payload)
		if endpoint == "/responses" {
			prepared, nextSession, err = applyGatewayPolicy(r, payload, keyID, cacheSettings)
			if err != nil {
				return nil, err
			}
			if requestInferenceMode(ctx) == inferenceCodexCLI && p.AppServerCLI != nil {
				prepared, _, _, err = appServerMirrorNativeMetadata(prepared, nil)
				if err != nil {
					return nil, err
				}
			}
		}
		// Every existing nonempty key now receives Sub2API-style isolation;
		// custom keys retain their grouping but are tracked across WS chains too.
		cachePresent := endpoint == "/responses" && gatewayPromptCacheKeyValue(prepared) != ""
		_, defaultCache := defaultGatewayPromptCacheSpan(prepared)
		defaultCache = defaultCache && endpoint == "/responses"
		if previous != "" && !priorCacheKnown && cacheSettings.PromptCacheKeyEnabled && keyID != "" && defaultCache {
			return nil, errors.New("cache policy for previous_response_id is unavailable; resend full input without previous_response_id")
		}
		cacheKind := "prompt-cache"
		if defaultCache {
			cacheKind = "session"
		}
		if priorCacheKnown && (priorCachePolicy.kind == "session" || priorCachePolicy.kind == "prompt-cache") {
			cacheKind = priorCachePolicy.kind
		}
		// Account-independent UUID sizing includes metadata; reject before a
		// body-routed request establishes an upstream connection or binding.
		if endpoint == "/responses" {
			if err := validateGatewayAccountPromptCacheSize(prepared, keyID, cacheSettings, gatewayUnboundedPayload(ctx)); err != nil {
				return nil, err
			}
		}
		var create struct {
			Model              string  `json:"model"`
			Instructions       *string `json:"instructions"`
			StreamID           *string `json:"stream_id"`
			PreviousResponseID *string `json:"previous_response_id"`
			Generate           *bool   `json:"generate"`
		}
		if gatewayWSDecodeExact(prepared, map[string]any{
			"model": &create.Model, "instructions": &create.Instructions, "stream_id": &create.StreamID,
			"previous_response_id": &create.PreviousResponseID, "generate": &create.Generate,
		}) != nil || strings.TrimSpace(create.Model) == "" {
			return nil, errors.New("response.create requires a model and valid parameter types")
		}
		if create.StreamID != nil && !gatewayWSValidStreamID(*create.StreamID, gatewayUnboundedPayload(ctx)) {
			if gatewayUnboundedPayload(ctx) {
				return nil, errors.New("stream_id must contain ASCII letters, digits, underscores, hyphens or periods")
			}
			return nil, errors.New("stream_id must contain 1 to 256 ASCII letters, digits, underscores, hyphens or periods")
		}
		if !gatewayUnboundedPayload(ctx) && (len(create.Model) > 512 || len(nextSession) > 4096 || (create.PreviousResponseID != nil && len(*create.PreviousResponseID) > 1024)) {
			return nil, errors.New("WebSocket model or session identifier is too long")
		}
		if upstream == nil {
			response, err = connect(nextSession, create.Model, prepared)
			if err != nil {
				status := gatewayWSStatus(err)
				_, retryAfter, _ := gatewaySelectionFailure(err)
				if response != nil && response.Body != nil {
					if response.Header.Get("Retry-After") != "" {
						retryAfter = response.Header.Get("Retry-After")
					}
					if response.StatusCode >= 300 {
						status = response.StatusCode
					}
					_ = response.Body.Close()
				}
				return nil, &gatewayWSFailure{status: status, message: gatewayWSConnectionMessage(err), retryAfter: retryAfter}
			}
			session = nextSession
			turns.setHeaderTiming(headerStarted, headerObserved)
		}
		authStarted := time.Now()
		current, err := p.authorizedGatewayWSAccountContext(ctx, bearerToken(r), keyID, account)
		authFinished := time.Now()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, errGatewayKeyStoreUnavailable) {
				return nil, &gatewayWSFailure{status: http.StatusServiceUnavailable, message: err.Error()}
			}
			return nil, &gatewayWSFailure{status: http.StatusForbidden, message: err.Error()}
		}
		// The socket still authenticates with its handshake credentials even
		// if the management record was refreshed or re-imported meanwhile.
		if err := p.checkGatewayCooldown(withGatewayCooldownModel(ctx, create.Model), account); err != nil {
			return nil, err
		}
		if currentSettings.StickySessionsEnabled && nextSession != "" {
			if err := turns.bind(ctx, nextSession, current); err != nil {
				return nil, &gatewayWSFailure{status: http.StatusServiceUnavailable, message: err.Error()}
			}
		}
		if endpoint == "/responses" {
			prepared, err = prepareGatewayAccountPromptCacheWithKind(prepared, keyID, current, cacheSettings, cacheKind, gatewayUnboundedPayload(ctx))
			if err != nil {
				return nil, err
			}
		}
		cacheTracked := cachePresent || (priorCacheKnown && priorCachePolicy.tracked)
		if priorCacheKnown && cacheTracked && gatewayWSPromptCacheDigest(prepared) != priorCachePolicy.keyDigest {
			return nil, errors.New("prompt_cache_key changed for previous_response_id; resend full input without previous_response_id")
		}
		admission, acquired, admissionErr := p.acquireGatewayAccountContext(ctx, current)
		if admissionErr != nil {
			return nil, admissionErr
		}
		if !acquired {
			return nil, &gatewayAccountBusyError{session: true}
		}
		release := admission.Release
		if currentSettings.StickySessionsEnabled && nextSession != "" {
			bound, err := p.touchGatewaySession(ctx, keyID, nextSession, gatewayAccountRef(current))
			if err != nil || bound != gatewayAccountRef(current) {
				release()
				if err != nil {
					return nil, err
				}
				return nil, errors.New("session account binding expired or changed during admission")
			}
		}
		if response != nil {
			turns.setResponseHeaders(response.Header)
		}
		if err := turns.start(prepared, current, proxy, release, time.Duration(currentSettings.UpstreamStreamIdleTimeoutSeconds)*time.Second); err != nil {
			release()
			return nil, err
		}
		cacheStreamID := ""
		if create.StreamID != nil {
			cacheStreamID = *create.StreamID
		}
		// Current metadata governs admission, but this socket still authenticates
		// with the credentials used by its successful upgrade, even after refresh.
		turns.observeConnectionCredentials(cacheStreamID, account)
		if endpoint == "/responses" {
			turns.setLatestCachePolicy(cacheStreamID, prepared, cacheSettings, cacheTracked, cacheKind)
		}
		turns.observeAuthentication(prepared, authStarted, authFinished)
		turns.observeAttemptStart(payload, prepared, r.URL.Path, connectionObservation)
		if p.PromptCacheObserver != nil && endpoint == "/responses" {
			cacheObservation := p.PromptCacheObserver.Observe(r, prepared, keyID, current,
				gatewayPromptCacheObservationKeyMode(r, payload, cacheSettings))
			turns.observePromptCache(cacheStreamID, cacheObservation)
		}
		connectionObservation = nil // Handshake attempts belong only to the first admitted turn.
		requestWriteTimeout = time.Duration(currentSettings.UpstreamStreamIdleTimeoutSeconds) * time.Second
		if requestWriteTimeout <= 0 {
			requestWriteTimeout = upstreamStreamIdleTimeout
		}
		return prepared, nil
	}
	// Receive the first frame before starting upstream reads if account routing
	// depends on its prompt_cache_key. Limit idle unaffiliated connections.
	firstTimeout := time.NewTimer(90 * time.Second)
	defer firstTimeout.Stop()
	var prepared []byte
	var first clientFrame
	for {
		select {
		case first = <-frames:
		case <-ctx.Done():
			return
		case <-firstTimeout.C:
			return
		}
		if first.typ != websocket.MessageText {
			gatewayWSError(ctx, downstream, http.StatusBadRequest, "Responses WebSocket expects text JSON events")
			return
		}
		prepared, err = prepare(first.payload)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		status := http.StatusBadRequest
		retryAfter := ""
		if selectedStatus, selectedRetry, recognized := gatewaySelectionFailure(err); recognized {
			status, retryAfter = selectedStatus, selectedRetry
		}
		var failure *gatewayWSFailure
		if errors.As(err, &failure) {
			status, retryAfter = failure.status, failure.retryAfter
		}
		gatewayWSRequestError(ctx, downstream, first.payload, status, err.Error(), retryAfter)
		if status != http.StatusBadRequest && status != http.StatusTooManyRequests {
			return
		}
	}
	if err := gatewayWSWriteWithTimeout(ctx, upstream, first.typ, prepared, requestWriteTimeout); err != nil {
		if ctx.Err() == nil {
			turns.observeError("upstream", "request_write")
		}
		return
	}
	ended := make(chan error, 2)
	go func() {
		for {
			var frame clientFrame
			select {
			case frame = <-frames:
			case <-ctx.Done():
				ended <- ctx.Err()
				return
			}
			typ, payload := frame.typ, frame.payload
			if typ != websocket.MessageText {
				turns.observeError("client", "request_frame")
				gatewayWSError(ctx, downstream, http.StatusBadRequest, "Responses WebSocket expects text JSON events")
				ended <- errors.New("unexpected binary message")
				return
			}
			prepared, err := prepare(payload)
			if err != nil {
				if ctx.Err() != nil {
					ended <- ctx.Err()
					return
				}
				status := http.StatusBadRequest
				retryAfter := ""
				if selectedStatus, selectedRetry, recognized := gatewaySelectionFailure(err); recognized {
					status, retryAfter = selectedStatus, selectedRetry
				}
				var failure *gatewayWSFailure
				if errors.As(err, &failure) {
					status, retryAfter = failure.status, failure.retryAfter
				}
				gatewayWSRequestError(ctx, downstream, payload, status, err.Error(), retryAfter)
				if status == http.StatusTooManyRequests || status == http.StatusBadRequest {
					continue
				}
				turns.observeError("gateway", "request_prepare")
				ended <- err
				return
			}
			if err := gatewayWSWriteWithTimeout(ctx, upstream, typ, prepared, requestWriteTimeout); err != nil {
				if ctx.Err() == nil {
					turns.observeError("upstream", "request_write")
				}
				ended <- err
				return
			}
		}
	}()
	go func() {
		for {
			typ, payload, err := upstream.Read(ctx)
			if err != nil {
				if ctx.Err() == nil {
					turns.observeError("upstream", "response_read")
				}
				ended <- err
				return
			}
			closeConnection := false
			var delivered func(error)
			if typ == websocket.MessageText {
				closeConnection, delivered = turns.observeFrameDelivery(ctx, payload, account)
			}
			turns.pauseIdleForDownstreamWrite()
			writeErr := gatewayWSWrite(ctx, downstream, typ, payload)
			if writeErr == nil {
				if ctx.Err() == nil {
					turns.resumeIdleAfterDownstreamWrite()
				}
			} else {
				// Do not wake an elapsed upstream timer on a failed consumer
				// write: establish its actual cause before canceling peers.
				if ctx.Err() == nil {
					turns.observeError("client", "response_write")
				}
				cancel()
			}
			if delivered != nil {
				delivered(writeErr)
			}
			if err := writeErr; err != nil {
				ended <- err
				return
			}
			if closeConnection {
				// An unscoped protocol error does not prove an in-flight model
				// turn stopped. End this connection before releasing its slots.
				cancel()
				ended <- errors.New("upstream WebSocket error")
				return
			}
		}
	}()
	<-ended
	// Cancel and close both sockets before joining the pumps; blocked peer writes
	// or reads must never retain an account slot after the other peer leaves.
	cancel()
	_ = upstream.CloseNow()
	_ = downstream.CloseNow()
	<-ended
}

type gatewayWSTurn struct {
	rec             RequestRecord
	started         time.Time
	responseStarted time.Time
	responseID      string
	release         func()
	lastActivity    time.Time
	idleTimeout     time.Duration
	cachePolicy     gatewayWSCachePolicy
}

func (t *gatewayWSTurns) observePromptCache(streamID string, observation *GatewayPromptCacheObservation) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if queue := t.pending[streamID]; len(queue) > 0 {
		queue[len(queue)-1].rec.PromptCache = observation
	}
}

func (t *gatewayWSTurns) observeAttemptStart(inbound, sent []byte, path string, connection *gatewayAttemptObservation) {
	var event struct {
		StreamID string `json:"stream_id"`
	}
	if gatewayWSDecodeExact(sent, map[string]any{"stream_id": &event.StreamID}) != nil {
		return
	}
	_, observer := withGatewayAttemptObservation(context.Background())
	observer.ObserveInbound(path, gatewayWSObservationView(inbound))
	endpoint := t.endpoint
	if endpoint == "" {
		endpoint = "/responses"
	}
	observer.ObserveSent(endpoint, gatewayWSObservationView(sent))
	t.mu.Lock()
	defer t.mu.Unlock()
	queue := t.pending[event.StreamID]
	if len(queue) == 0 {
		return
	}
	rec := &queue[len(queue)-1].rec
	if existing := rec.turnCostObservation; existing != nil {
		// start sees the account-scoped wire payload. Preserve the client's
		// original thread identity for its later cost query, then bind the
		// observation to the exact metadata actually sent upstream.
		rec.turnCostObservation = newAppServerCostObservation(inbound, nil)
		if rec.turnCostObservation != nil {
			rec.turnCostObservation.Owner = existing.Owner
			rec.turnCostObservation.BindSent(sent, nil)
		}
	}
	applyGatewayAttemptSnapshot(rec, observer)
	if connection != nil {
		s := connection.Snapshot()
		rec.Attempts, rec.AttemptsTruncated = s.Attempts, s.AttemptsTruncated
		rec.AccountSelectionMs, rec.ProxySelectionMs = s.AccountSelectionMs, s.ProxySelectionMs
	}
}

type gatewayWSTurns struct {
	unboundedPayload       bool
	mu                     sync.Mutex
	proxy                  *Proxy
	keyID, keyName, ip     string
	pending                map[string][]*gatewayWSTurn
	completed              gatewayWSResponseHistory
	bindings               map[string]string
	count                  int
	userAgent              string
	endpoint               string
	clientRequestID        string
	responseHeaders        http.Header
	headerStarted          time.Time
	headerObserved         time.Time
	idleChanged            chan struct{}
	downstreamWriteStarted time.Time
}

// Read under the turn mutex: created responses may be referenced before a
// terminal event, and completed responses retain their policy in the bounded FIFO.
func (t *gatewayWSTurns) cachePolicyForResponse(id string) (gatewayWSCachePolicy, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if value, ok := t.completed.cachePolicy(id); ok {
		return value, true
	}
	for _, queue := range t.pending {
		for _, turn := range queue {
			if turn.responseID == id {
				return turn.cachePolicy, true
			}
		}
	}
	return gatewayWSCachePolicy{}, false
}

// Called after start succeeds and before the writer can send this request.
func (t *gatewayWSTurns) setLatestCachePolicy(streamID string, payload []byte, settings Settings, tracked bool, kinds ...string) {
	policy := gatewayWSCachePolicy{enabled: settings.PromptCacheKeyEnabled, tracked: tracked, keyDigest: gatewayWSPromptCacheDigest(payload), rewriteTracked: true, rewriteDigest: gatewayWSRewritePolicyDigest(settings)}
	if len(kinds) > 0 && (kinds[0] == "session" || kinds[0] == "prompt-cache") {
		policy.kind = kinds[0]
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	queue := t.pending[streamID]
	if len(queue) > 0 {
		queue[len(queue)-1].cachePolicy = policy
	}
}

func (t *gatewayWSTurns) setHeaderTiming(started, observed time.Time) {
	if started.IsZero() || observed.IsZero() {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.headerStarted, t.headerObserved = started, observed
}

func (t *gatewayWSTurns) observeError(source, phase string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.observeErrorLocked(source, phase)
}

func (t *gatewayWSTurns) observeErrorLocked(source, phase string) {
	for _, queue := range t.pending {
		for _, turn := range queue {
			observeGatewayError(&turn.rec, source, phase)
		}
	}
}

func (t *gatewayWSTurns) setResponseHeaders(headers http.Header) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.responseHeaders = headers.Clone()
}

func (t *gatewayWSTurns) start(payload []byte, account Account, proxy *ProxyEntry, release func(), idle ...time.Duration) error {
	var event struct {
		Model    string `json:"model"`
		StreamID string `json:"stream_id"`
	}
	if err := gatewayWSDecodeExact(payload, map[string]any{"model": &event.Model, "stream_id": &event.StreamID}); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.completed.unbounded = t.unboundedPayload
	if t.count >= 128 && !t.unboundedPayload {
		return errors.New("too many pending WebSocket responses")
	}
	now := time.Now()
	idleTimeout := upstreamStreamIdleTimeout
	if len(idle) > 0 && idle[0] > 0 {
		idleTimeout = idle[0]
	}
	endpoint := t.endpoint
	if endpoint == "" {
		endpoint = "/responses"
	}
	turn := &gatewayWSTurn{started: now, release: release, rec: RequestRecord{unboundedPayload: t.unboundedPayload, Time: now.Unix(), KeyID: t.keyID, KeyName: t.keyName,
		Account: account.Name, AccountID: account.ID, AccountNameSnapshot: account.Name, AccountCredentialVersion: account.CredentialVersion,
		Groups: append([]string(nil), account.Groups...), Model: event.Model,
		Endpoint: strings.TrimPrefix(endpoint, "/"), Stream: true, IP: t.ip, HTTPCode: http.StatusSwitchingProtocols}}
	turn.lastActivity, turn.idleTimeout = now, idleTimeout
	if t.proxy != nil && t.proxy.AppServerCLI != nil && endpoint == "/responses" {
		turn.rec.turnCostObservation = newAppServerCostObservation(payload, nil)
		if turn.rec.turnCostObservation != nil {
			turn.rec.turnCostObservation.Owner = gatewayAccountRef(account)
			turn.rec.turnCostObservation.BindSent(payload, nil)
		}
	}
	observeGatewayRequest(&turn.rec, t.userAgent, "websocket", gatewayWSObservationView(payload))
	observeGatewayClientIdentity(&turn.rec, t.clientRequestID, "connection")
	observeGatewayHeaders(&turn.rec, t.responseHeaders)
	if !t.headerStarted.IsZero() && !t.headerObserved.IsZero() {
		observeGatewayHeaderTiming(&turn.rec, t.headerStarted, t.headerObserved)
		t.headerStarted, t.headerObserved = time.Time{}, time.Time{}
	}
	if proxy != nil {
		turn.rec.ProxyName = proxy.Name
	}
	if t.pending == nil {
		t.pending = make(map[string][]*gatewayWSTurn)
	}
	t.pending[event.StreamID] = append(t.pending[event.StreamID], turn)
	t.count++
	t.notifyIdleLocked()
	return nil
}

// observe returns true only for a connection-scoped protocol error. A named
// lane's error is terminal for its FIFO head, and must not abort other lanes.
func (t *gatewayWSTurns) observe(payload []byte) bool {
	fields, err := gatewayWSParseFields(payload)
	if err != nil {
		return false
	}
	return t.observeFields(payload, fields)
}

// Both production observers share one envelope parse. Large opaque item/delta
// fields remain untouched and are never copied into a second envelope map.
func (t *gatewayWSTurns) observeFrame(ctx context.Context, payload []byte, connected Account) bool {
	stop, delivered := t.observeFrameDelivery(ctx, payload, connected)
	if delivered != nil {
		delivered(nil)
	}
	return stop
}

// Completion proves upstream work stopped, so release/promote exactly as before.
// Persist the detached terminal record only after its downstream write result is
// known. The writer can fail after the terminal has already left pending turns.
func (t *gatewayWSTurns) observeFrameDelivery(ctx context.Context, payload []byte, connected Account) (bool, func(error)) {
	fields, err := gatewayWSParseFields(payload)
	if err != nil {
		return false, nil
	}
	t.observeCooldownFields(ctx, payload, connected, fields)
	stop, terminal := t.observeFieldsPendingDelivery(payload, fields)
	if terminal == nil {
		return stop, nil
	}
	var once sync.Once
	return stop, func(writeErr error) {
		once.Do(func() { t.finishTerminalDelivery(terminal, writeErr) })
	}
}

func (t *gatewayWSTurns) finishTerminalDelivery(turn *gatewayWSTurn, writeErr error) {
	if writeErr != nil {
		if turn.rec.Status != "error" {
			turn.rec.Status = "aborted"
		}
		if turn.rec.Error == "" {
			turn.rec.Error = "client connection lost"
		}
		observeGatewayError(&turn.rec, "client", "response_write")
	}
	t.proxy.finishRecord(&turn.rec, turn.started, t.keyID)
}

func (t *gatewayWSTurns) observeFields(payload []byte, fields map[string]json.RawMessage) bool {
	stop, terminal := t.observeFieldsPendingDelivery(payload, fields)
	if terminal != nil {
		t.finishTerminalDelivery(terminal, nil)
	}
	return stop
}

func (t *gatewayWSTurns) observeFieldsPendingDelivery(payload []byte, fields map[string]json.RawMessage) (bool, *gatewayWSTurn) {
	var event struct {
		Type       string          `json:"type"`
		StreamID   string          `json:"stream_id"`
		ResponseID string          `json:"response_id"`
		Response   json.RawMessage `json:"response"`
	}
	if gatewayWSDecodeFields(fields, map[string]any{"type": &event.Type, "stream_id": &event.StreamID, "response_id": &event.ResponseID, "response": &event.Response}) != nil {
		return false, nil
	}
	var id string
	if len(event.Response) > 0 && string(event.Response) != "null" && gatewayWSDecodeExact(event.Response, map[string]any{"id": &id}) != nil {
		return false, nil
	}
	if id != "" && event.ResponseID != "" && id != event.ResponseID {
		return false, nil
	}
	if id == "" {
		id = event.ResponseID
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.completed.contains(id) {
		return false, nil
	}
	explicitLane := event.StreamID != ""
	streamID, matched, unique := t.responseLaneLocked(event.StreamID, id, explicitLane)
	if !unique {
		return false, nil
	}
	if event.Type == "error" && streamID == "" && !matched {
		t.observeErrorLocked("upstream", "response_event")
		return true, nil
	}
	queue := t.pending[streamID]
	if len(queue) == 0 {
		return false, nil
	}
	turn := queue[0]
	if turn.responseID != "" && id != "" && turn.responseID != id {
		return false, nil
	}
	if id != "" && (len(id) <= 1024 || t.unboundedPayload) {
		turn.responseID = id
	}
	if turn.responseStarted.IsZero() {
		turn.responseStarted = time.Now()
	}
	turn.lastActivity = time.Now()
	t.notifyIdleLocked()
	observedPayload := gatewayWSObservationFields(payload, fields)
	(gatewayObservation{record: &turn.rec, started: turn.started}).event(observedPayload)
	if turn.rec.turnCostObservation != nil && gatewayResponseIDMatchesRecord(&turn.rec, id) {
		turn.rec.turnCostObservation.ObserveResponse(observedPayload)
	}
	raw, terminal, err := gatewayWSTerminalFields(payload, fields)
	if !terminal {
		return false, nil
	}
	t.completed.add(turn.responseID, turn.cachePolicy)
	if len(raw) > 0 {
		var observation struct {
			Response json.RawMessage `json:"response"`
		}
		_ = json.Unmarshal(observedPayload, &observation)
		recordResponseOutcome(&turn.rec, observation.Response)
	}
	if err != nil {
		turn.rec.Status, turn.rec.Error = "error", err.Error()
		var failure *upstreamResponseError
		if errors.As(err, &failure) {
			turn.rec.Error = errorFromPayload(failure.Payload)
		}
	}
	observeGatewayResponseTransfer(&turn.rec, turn.responseStarted, time.Now())
	if turn.release != nil {
		turn.release()
	}
	queue[0] = nil
	if len(queue) == 1 {
		delete(t.pending, streamID)
	} else {
		t.pending[streamID] = queue[1:]
		// Time spent queued behind the previous response is not an upstream
		// stall for this turn. Each lane starts its next timeout at promotion.
		queue[1].lastActivity = time.Now()
	}
	t.count--
	return false, turn
}

func (t *gatewayWSTurns) abort() {
	t.abortContext(nil)
}

func (t *gatewayWSTurns) abortContext(ctx context.Context) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, queue := range t.pending {
		for _, turn := range queue {
			turn.rec.Status = "aborted"
			if turn.rec.Error == "" {
				turn.rec.Error = "WebSocket closed before a terminal response"
			}
			observeGatewayAdmissionLoss(&turn.rec, ctx)
			observeGatewayResponseTransfer(&turn.rec, turn.responseStarted, time.Now())
			t.proxy.finishRecord(&turn.rec, turn.started, t.keyID)
			if turn.release != nil {
				turn.release()
			}
		}
	}
	clear(t.pending)
	t.count = 0
	t.notifyIdleLocked()
}

func (t *gatewayWSTurns) notifyIdleLocked() {
	select {
	case t.idleChanged <- struct{}{}:
	default:
	}
}

// While the serial response pump writes to its consumer, it cannot poll any
// upstream lane. This interval is delivery backpressure, not upstream idle.
// The independent Write deadline and parent context still bound the operation.
func (t *gatewayWSTurns) pauseIdleForDownstreamWrite() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.downstreamWriteStarted.IsZero() {
		t.downstreamWriteStarted = time.Now()
		t.notifyIdleLocked()
	}
}

func (t *gatewayWSTurns) resumeIdleAfterDownstreamWrite() {
	t.mu.Lock()
	defer t.mu.Unlock()
	paused := t.downstreamWriteStarted
	if paused.IsZero() {
		return
	}
	now := time.Now()
	for _, queue := range t.pending {
		if len(queue) == 0 {
			continue
		}
		turn := queue[0]
		// Existing idle age is preserved. A newly admitted/promoted head only
		// excludes the portion of the write interval in which it existed.
		since := paused
		if turn.lastActivity.After(since) {
			since = turn.lastActivity
		}
		if now.After(since) {
			turn.lastActivity = turn.lastActivity.Add(now.Sub(since))
		}
	}
	t.downstreamWriteStarted = time.Time{}
	t.notifyIdleLocked()
}

// The CLI applies stream_idle_timeout only while sending/receiving a response.
// Keep an empty connection reusable indefinitely, and do not let traffic on a
// different named lane conceal a stalled response. The owner closes both peers
// before abort releases any inference slots.
func (t *gatewayWSTurns) watchIdle(ctx context.Context, cancel context.CancelFunc) {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		t.mu.Lock()
		var deadline time.Time
		now := time.Now()
		expired := false
		if t.downstreamWriteStarted.IsZero() {
			for _, queue := range t.pending {
				if len(queue) == 0 {
					continue
				}
				turn := queue[0]
				due := turn.lastActivity.Add(turn.idleTimeout)
				if !now.Before(due) {
					turn.rec.Error = errUpstreamStreamIdleTimeout.Error()
					observeGatewayError(&turn.rec, "upstream", "response_idle")
					expired = true
				}
				if deadline.IsZero() || due.Before(deadline) {
					deadline = due
				}
			}
		}
		t.mu.Unlock()
		if expired {
			cancel()
			return
		}
		var tick <-chan time.Time
		if !deadline.IsZero() {
			timer.Reset(time.Until(deadline))
			tick = timer.C
		}
		select {
		case <-ctx.Done():
			return
		case <-t.idleChanged:
		case <-tick:
		}
		timer.Stop()
	}
}

func (t *gatewayWSTurns) observeAuthentication(payload []byte, started, finished time.Time) {
	var event struct {
		StreamID string `json:"stream_id"`
	}
	if gatewayWSDecodeExact(payload, map[string]any{"stream_id": &event.StreamID}) != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	queue := t.pending[event.StreamID]
	if len(queue) > 0 {
		observeGatewayAuthentication(&queue[len(queue)-1].rec, started, finished)
	}
}

func (p *Proxy) authorizedGatewayWSAccount(token, keyID string, connected Account) (Account, error) {
	return p.authorizedGatewayWSAccountContext(context.Background(), token, keyID, connected)
}

func (p *Proxy) authorizedGatewayWSAccountContext(ctx context.Context, token, keyID string, connected Account) (Account, error) {
	candidates, currentKeyID, _, err := p.candidatesForContext(ctx, token)
	if err != nil {
		return Account{}, err
	}
	if currentKeyID != keyID {
		return Account{}, errors.New("gateway key is no longer authorized")
	}
	for _, candidate := range candidates {
		if gatewayAccountRef(candidate) == gatewayAccountRef(connected) {
			current, err := p.gatewayCurrentBoundAccountContext(ctx, candidate, gatewayAccountRef(connected))
			if err != nil {
				if ctx.Err() != nil {
					return Account{}, ctx.Err()
				}
				return Account{}, errors.New("session account ownership is no longer authorized")
			}
			if err := p.checkGatewayBoundAccountAccess(ctx, token, keyID, current); err != nil {
				return Account{}, err
			}
			return current, nil
		}
	}
	return Account{}, errors.New("session account is no longer enabled or allowed for this key")
}

func (t *gatewayWSTurns) bind(ctx context.Context, session string, account Account) error {
	if len(session) > 4096 && !t.unboundedPayload {
		return errors.New("WebSocket session identifier is too long")
	}
	t.mu.Lock()
	_, exists := t.bindings[session]
	full := len(t.bindings) >= 128 && !exists && !t.unboundedPayload
	t.mu.Unlock()
	if full {
		return errors.New("too many sessions on one WebSocket connection")
	}
	ref := gatewayAccountRef(account)
	bound, err := t.proxy.sessionAccount(ctx, t.keyID, session, ref)
	if err != nil {
		return err
	}
	if bound != ref {
		return errors.New("session is bound to another account; open a new WebSocket connection")
	}
	t.mu.Lock()
	t.bindings[session] = ref
	t.mu.Unlock()
	return nil
}

func (t *gatewayWSTurns) renew(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	t.mu.Lock()
	bindings := make(map[string]string, len(t.bindings))
	for session, ref := range t.bindings {
		bindings[session] = ref
	}
	t.mu.Unlock()
	for session, ref := range bindings {
		bound, err := t.proxy.touchGatewaySession(ctx, t.keyID, session, ref)
		if err != nil {
			return err
		}
		if bound != ref {
			return errors.New("session account binding changed; open a new WebSocket connection")
		}
	}
	return nil
}
