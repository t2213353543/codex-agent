package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Realtime is a transport bridge. Clients own session updates, audio, tool
// handoffs and reconnection. A call stores only its authenticated owner in the
// same distributed routing store used by ordinary gateway sessions.
func appServerRealtimePath(path string) bool {
	switch path {
	case "/v1/realtime/calls", "/v1/realtime", "/v1/live":
		return true
	default:
		return strings.HasPrefix(path, "/v1/live/") && appServerRealtimeValidCallID(strings.TrimPrefix(path, "/v1/live/"))
	}
}

func appServerRealtimeValidCallID(id string) bool {
	if !strings.HasPrefix(id, "rtc_") || len(id) <= 4 {
		if len(id) != 36 {
			return false
		}
		for i, ch := range id {
			if i == 8 || i == 13 || i == 18 || i == 23 {
				if ch != '-' {
					return false
				}
			} else if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' || ch >= 'A' && ch <= 'F') {
				return false
			}
		}
		return true
	}
	for _, ch := range id[4:] {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
			return false
		}
	}
	return true
}

func appServerRealtimeCallFromLocation(location string) (string, error) {
	u, err := url.Parse(location)
	if err != nil || u.RawPath != "" && u.RawPath != u.Path {
		return "", errors.New("invalid realtime call Location")
	}
	parts := strings.Split(strings.TrimRight(u.Path, "/"), "/")
	if id := parts[len(parts)-1]; appServerRealtimeValidCallID(id) {
		return id, nil
	}
	return "", errors.New("realtime call response missing call id")
}

// No client-controlled host, origin or sideband URL may become a dial target.
func appServerRealtimeQuery(raw string, websocketRequest bool) (url.Values, error) {
	query, err := url.ParseQuery(raw)
	if err != nil {
		return nil, errors.New("invalid realtime query")
	}
	for key, values := range query {
		allowed := key == "intent" || key == "architecture"
		if websocketRequest {
			allowed = key == "intent" || key == "model" || key == "call_id"
		}
		if !allowed || len(values) != 1 {
			return nil, errors.New("unsupported realtime query parameter")
		}
	}
	return query, nil
}

func appServerRealtimeBackendBody(raw []byte, contentType string) ([]byte, string, error) {
	media, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, "", errors.New("invalid realtime Content-Type")
	}
	var sdp string
	var session json.RawMessage
	var result []byte
	switch media {
	case "application/json":
		var body struct {
			SDP     *string         `json:"sdp"`
			Session json.RawMessage `json:"session"`
		}
		if json.Unmarshal(raw, &body) != nil || body.SDP == nil {
			return nil, "", errors.New("realtime call requires sdp and session")
		}
		sdp, session, result = *body.SDP, body.Session, raw
	case "multipart/form-data":
		if params["boundary"] == "" {
			return nil, "", errors.New("realtime multipart boundary missing")
		}
		reader := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
		seen := map[string]bool{}
		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, "", errors.New("invalid realtime multipart body")
			}
			name := part.FormName()
			if name != "sdp" && name != "session" || seen[name] {
				return nil, "", errors.New("realtime multipart requires unique sdp and session parts")
			}
			value, err := io.ReadAll(part)
			_ = part.Close()
			if err != nil {
				return nil, "", err
			}
			seen[name] = true
			if name == "sdp" {
				sdp = string(value)
			} else {
				session = value
			}
		}
		if !seen["sdp"] || !seen["session"] {
			return nil, "", errors.New("realtime call requires sdp and session")
		}
		encoded, _ := json.Marshal(sdp)
		result = append(append(append([]byte(`{"sdp":`), encoded...), []byte(`,"session":`)...), session...)
		result = append(result, '}')
	default:
		return nil, "", errors.New("realtime calls require application/json or multipart/form-data")
	}
	var info struct {
		Model string `json:"model"`
	}
	if sdp == "" || len(bytes.TrimSpace(session)) == 0 || bytes.TrimSpace(session)[0] != '{' || json.Unmarshal(session, &info) != nil {
		return nil, "", errors.New("realtime call requires SDP and a session object")
	}
	return result, info.Model, nil
}

func (p *Proxy) gatewayAppServerRealtime(w http.ResponseWriter, r *http.Request) {
	if !appServerRealtimePath(r.URL.Path) || r.URL.RawPath != "" && r.URL.RawPath != r.URL.Path {
		apiError(w, 404, "not found")
		return
	}
	if r.Method == http.MethodPost && (r.URL.Path == "/v1/realtime/calls" || r.URL.Path == "/v1/live") {
		p.gatewayAppServerRealtimeCall(w, r)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path != "/v1/realtime/calls" && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		p.gatewayAppServerRealtimeWebSocket(w, r)
		return
	}
	w.Header().Set("Allow", "POST, GET")
	apiError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func (p *Proxy) gatewayAppServerRealtimeCall(w http.ResponseWriter, r *http.Request) {
	if officialFailoverFrom(r.Context()) == nil {
		p.gatewayWithAccountFailover(w, r, p.gatewayAppServerRealtimeCall)
		return
	}
	started := time.Now()
	token := bearerToken(r)
	keyID, keyName, _, err := p.resolveGatewayKeyContext(r.Context(), token)
	if err != nil {
		apiError(w, 503, "gateway key authorization unavailable")
		return
	}
	if keyID == "" {
		apiError(w, 401, "invalid API key")
		return
	}
	settings, err := p.settingsValue()
	if err != nil {
		apiError(w, 503, "gateway settings unavailable")
		return
	}
	rec := RequestRecord{Time: started.Unix(), KeyID: keyID, KeyName: keyName, Endpoint: "realtime/calls", IP: clientIP(r)}
	ctx, observation := withGatewayAttemptObservation(r.Context())
	r = r.WithContext(ctx)
	w = &gatewayResponseWriter{ResponseWriter: w, record: &rec}
	state := officialFailoverFrom(ctx)
	defer func() { applyGatewayAttemptSnapshot(&rec, observation); state.record, state.key = &rec, keyID }()
	fail := func(code int, message string) { rec.Status, rec.Error = "error", message; apiError(w, code, message) }
	query, err := appServerRealtimeQuery(r.URL.RawQuery, false)
	if err != nil {
		fail(400, err.Error())
		return
	}
	// Both V1 and V3 use the backend request shape and these query values.
	query.Set("intent", "quicksilver")
	query.Set("architecture", "avas")
	raw, err := readBody(w, r)
	if err != nil {
		fail(400, err.Error())
		return
	}
	body, model, err := appServerRealtimeBackendBody(raw, r.Header.Get("Content-Type"))
	if err != nil {
		fail(400, err.Error())
		return
	}
	if !state.retrying {
		state.requestBody = append([]byte(nil), raw...)
	}
	rec.Model = model
	observeGatewayRequest(&rec, r.UserAgent(), "realtime_call", nil)
	observation.ObserveInbound(r.URL.Path, nil)
	session := gatewayExplicitSessionKey(r, nil)
	if session == "" {
		session = gatewayContentSessionKey(raw, gatewayUnboundedPayload(ctx))
	}
	if state.session != "" {
		session = state.session
	} else {
		state.session = session
	}
	route := withGatewayCooldownModel(withGatewayRequestRoute(ctx, keyID, session), model)
	defer stopGatewayRequestRoute(route)
	defer observeGatewayAdmissionLoss(&rec, route)
	route = p.withGatewayNativeDispatchAuthorization(route, token, keyID)
	var account Account
	var release func()
	if state.retrying {
		account, release, err = p.pickFailoverAccount(route, token, keyID)
	} else {
		account, release, err = p.pickSessionAccount(route, token, keyID, session)
	}
	if err != nil {
		officialObserveAttemptFailure(ctx, err)
		var unavailable *gatewayBoundAccountUnavailableError
		if errors.As(err, &unavailable) {
			state.account, state.owner = unavailable.Account, unavailable.Owner
		}
		rec.Status, rec.Error = "error", err.Error()
		writeGatewaySelectionError(w, err)
		return
	}
	defer release()
	state.account = account
	if !state.retrying {
		state.owner = gatewayAccountRef(account)
	}
	rec.Account, rec.AccountID, rec.AccountCredentialVersion = account.Name, account.ID, account.CredentialVersion
	rec.Groups = append([]string(nil), account.Groups...)
	lease, stop := p.officialFailoverLease(route, keyID, session, account)
	defer stop()
	proxy, err := p.gatewayRequestProxy(route, account)
	if err != nil {
		fail(503, "realtime proxy route unavailable")
		return
	}
	if proxy != nil {
		rec.ProxyName = proxy.Name
	}
	target, err := gatewayUpstreamURL(p.gatewayInferenceBaseURL(ctx), "/realtime/calls", query)
	if err != nil {
		fail(500, "invalid realtime upstream")
		return
	}
	up, err := http.NewRequestWithContext(withoutGatewayNativeRequestCompression(lease), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		fail(500, "invalid realtime upstream request")
		return
	}
	if err = appServerRealtimeHeaders(up.Header, r.Header, settings, account, p.AppServerCLI, r.URL.Path == "/v1/live"); err != nil {
		fail(503, err.Error())
		return
	}
	up.Header.Set("Content-Type", "application/json")
	up.Header.Set("Accept", "*/*")
	client := p.gatewayRequestClientWithHeaderTimeout(route, proxy, gatewayClientHTTP, time.Duration(settings.UpstreamResponseHeaderTimeoutSeconds)*time.Second)
	res, err := p.doObservedAccountRequest(&rec, client, up, account, doHTTPAccountRequest, func(updated Account) error { return p.authorizeGatewayRetry(up.Context(), token, keyID, updated) })
	if err != nil {
		officialObserveAttemptFailure(ctx, err)
		fail(gatewayTransportFailureStatus(err), "realtime call upstream request failed")
		return
	}
	defer res.Body.Close()
	observeGatewayHeaders(&rec, res.Header)
	result, err := io.ReadAll(res.Body)
	if err != nil {
		fail(502, "realtime call response interrupted")
		return
	}
	rec.HTTPCode = res.StatusCode
	if res.StatusCode >= 300 {
		observeAppServerCLIFailoverHTTP(ctx, res.StatusCode, result)
		rec.Status, rec.Error = "error", errorFromPayload(result)
	} else {
		callID, err := appServerRealtimeCallFromLocation(res.Header.Get("Location"))
		if err != nil {
			fail(502, err.Error())
			return
		}
		bound, err := p.sessionAccount(lease, keyID, "realtime:"+callID, gatewayAccountRef(account))
		if err != nil || bound != gatewayAccountRef(account) {
			fail(409, "realtime call owner binding unavailable")
			return
		}
		if !commitAppServerCLIFailover(w, ctx, &rec) {
			return
		}
		// The client extracts this call id, then connects its configured gateway
		// sideband URL; never direct a client to a private upstream Location.
		res.Header.Set("Location", r.URL.Path+"/"+callID)
		rec.Status = "ok"
	}
	copyResponseHeaders(w.Header(), res.Header)
	w.Header().Set("Location", res.Header.Get("Location"))
	w.WriteHeader(res.StatusCode)
	if _, err = w.Write(result); err != nil {
		rec.Status, rec.Error = "aborted", "client realtime call response delivery failed"
	}
}

func appServerRealtimeHeaders(dst, src http.Header, settings Settings, account Account, cli *appServerCLI, live bool) error {
	if err := copyCodexRequestHeaders(dst, src, settings); err != nil {
		return err
	}
	// The ordinary Responses header allowlist does not include this Realtime
	// protocol selector. It must survive rather than always becoming a default.
	extra := make(http.Header)
	copyGatewayHeaders(extra, src, func(name string) bool { return name == "openai-alpha" || name == "x-session-id" }, func(string) bool { return true })
	if value := extra.Get("OpenAI-Alpha"); value != "" {
		if value != "quicksilver=v1" && value != "quicksilver=v2" {
			return errors.New("invalid realtime OpenAI-Alpha header")
		}
		dst.Set("OpenAI-Alpha", value)
	}
	if value := extra.Get("X-Session-Id"); value != "" {
		dst.Set("X-Session-Id", value)
	}
	cli.applyIdentity(dst)
	for _, name := range []string{"Content-Encoding", "Content-Length", "Content-Type", "Accept", "Sec-WebSocket-Key", "Sec-WebSocket-Version", "Sec-WebSocket-Protocol", "Sec-WebSocket-Extensions"} {
		dst.Del(name)
	}
	dst.Set("Authorization", "Bearer "+account.AccessToken)
	if account.AccountID != "" {
		dst.Set("ChatGPT-Account-Id", account.AccountID)
	} else {
		dst.Del("ChatGPT-Account-Id")
	}
	if dst.Get("OpenAI-Alpha") == "" {
		value := "quicksilver=v1"
		if live {
			value = "quicksilver=v2"
		}
		dst.Set("OpenAI-Alpha", value)
	}
	return nil
}

type appServerRealtimeDialer func(context.Context, http.Header, *ProxyEntry, string) (gatewayWSUpstream, *http.Response, error)
type appServerRealtimeDialerKey struct{}

func (p *Proxy) keepAppServerRealtimeCall(ctx context.Context, key, call string, account Account, interval time.Duration) (context.Context, func()) {
	lease, cancel := context.WithCancel(ctx)
	if bound, err := p.touchGatewaySession(lease, key, call, gatewayAccountRef(account)); err != nil || bound != gatewayAccountRef(account) {
		cancel()
		return lease, func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-lease.Done():
				return
			case <-ticker.C:
				bound, err := p.touchGatewaySession(lease, key, call, gatewayAccountRef(account))
				if err != nil || bound != gatewayAccountRef(account) {
					cancel()
					return
				}
			}
		}
	}()
	return lease, func() { cancel(); <-done }
}

func (p *Proxy) pickAppServerRealtimeOwner(ctx context.Context, token, keyID, owner string) (Account, func(), error) {
	candidates, actualKey, _, err := p.candidatesForContext(ctx, token)
	if err != nil {
		return Account{}, nil, err
	}
	if actualKey != keyID {
		return Account{}, nil, errors.New("gateway key is no longer authorized")
	}
	for _, candidate := range candidates {
		if gatewayAccountRef(candidate) != owner {
			continue
		}
		account, err := p.gatewayCurrentBoundAccountContext(ctx, candidate, owner)
		if err != nil {
			return Account{}, nil, err
		}
		if err = p.checkGatewayBoundAccountAccess(ctx, token, keyID, account); err != nil {
			return Account{}, nil, err
		}
		if err = p.checkGatewayCooldown(ctx, account); err != nil {
			return Account{}, nil, err
		}
		admission, acquired, admissionErr := p.acquireGatewayAccountContext(ctx, account)
		if admissionErr != nil {
			return Account{}, nil, admissionErr
		}
		if !acquired {
			return Account{}, nil, &gatewayAccountBusyError{session: true}
		}
		release := admission.Release
		ready, err := p.ensureReady(admission.Context, account)
		if err != nil {
			admissionCause := context.Cause(admission.Context)
			release()
			if gatewaySharedAdmissionFailure(admissionCause) {
				return Account{}, nil, admissionCause
			}
			p.noteAccountError(account, err)
			return Account{}, nil, err
		}
		if gatewayAccountRef(ready) != owner {
			release()
			return Account{}, nil, &gatewayOwnerChangedError{}
		}
		if err = p.checkGatewayBoundAccountAccess(ctx, token, keyID, ready); err != nil {
			release()
			return Account{}, nil, err
		}
		return ready, release, nil
	}
	return Account{}, nil, errors.New("realtime call owner is unavailable; create a new call")
}

func (p *Proxy) gatewayAppServerRealtimeWebSocket(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	token := bearerToken(r)
	keyID, keyName, _, err := p.resolveGatewayKeyContext(r.Context(), token)
	if err != nil {
		apiError(w, 503, "gateway key authorization unavailable")
		return
	}
	if keyID == "" {
		apiError(w, 401, "invalid API key")
		return
	}
	settings, err := p.settingsValue()
	if err != nil {
		apiError(w, 503, "gateway settings unavailable")
		return
	}
	if !settings.WebsocketEnabled {
		apiError(w, 426, "WebSocket is disabled")
		return
	}
	rec := RequestRecord{Time: started.Unix(), KeyID: keyID, KeyName: keyName, Endpoint: "realtime", IP: clientIP(r)}
	ctx, observation := withGatewayAttemptObservation(r.Context())
	defer func() { applyGatewayAttemptSnapshot(&rec, observation); p.finishRecord(&rec, started, keyID) }()
	fail := func(code int, message string) {
		rec.Status, rec.Error, rec.HTTPCode = "error", message, code
		apiError(w, code, message)
	}
	query, err := appServerRealtimeQuery(r.URL.RawQuery, true)
	if err != nil {
		fail(400, err.Error())
		return
	}
	callID := query.Get("call_id")
	live := strings.HasPrefix(r.URL.Path, "/v1/live")
	if strings.HasPrefix(r.URL.Path, "/v1/live/") {
		pathID := strings.TrimPrefix(r.URL.Path, "/v1/live/")
		if callID != "" && callID != pathID {
			fail(400, "realtime call id mismatch")
			return
		}
		callID = pathID
		query.Del("call_id")
	}
	if callID == "" {
		// Official 0.155.0 also refuses ChatGPT-only standalone WS startup.
		fail(400, "realtime conversation requires API key auth; create a WebRTC call first")
		return
	}
	if !appServerRealtimeValidCallID(callID) || live && r.URL.Path == "/v1/live" {
		fail(400, "invalid realtime call id")
		return
	}
	session := "realtime:" + callID
	owner, err := p.sessionAccount(ctx, keyID, session, "")
	if err != nil {
		fail(503, "realtime call owner lookup unavailable")
		return
	}
	if owner == "" {
		fail(404, "realtime call is not owned by this API key")
		return
	}
	route := withGatewayRequestRoute(ctx, keyID, session)
	defer stopGatewayRequestRoute(route)
	defer observeGatewayAdmissionLoss(&rec, route)
	account, release, err := p.pickAppServerRealtimeOwner(route, token, keyID, owner)
	if err != nil {
		rec.Status, rec.Error = "error", err.Error()
		rec.HTTPCode, _, _ = gatewaySelectionFailure(err)
		writeGatewaySelectionError(w, err)
		return
	}
	defer release()
	rec.Account, rec.AccountID, rec.AccountCredentialVersion = account.Name, account.ID, account.CredentialVersion
	rec.Groups = append([]string(nil), account.Groups...)
	lease, stop := p.keepAppServerRealtimeCall(route, keyID, session, account, 20*time.Second)
	defer stop()
	proxy, err := p.gatewayRequestProxy(route, account)
	if err != nil {
		fail(503, "realtime proxy route unavailable")
		return
	}
	if proxy != nil {
		rec.ProxyName = proxy.Name
	}
	headers := make(http.Header)
	if err = appServerRealtimeHeaders(headers, r.Header, settings, account, p.AppServerCLI, live); err != nil {
		fail(503, err.Error())
		return
	}
	path := "/v1/realtime"
	if live {
		path = "/v1/live/" + callID
	} else if query.Get("intent") == "" {
		query.Set("intent", "quicksilver")
	}
	// Official sideband joins api.openai.com using the call-create identity.
	target := "https://api.openai.com" + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	lease = withGatewayNativeWebSocketCompressionDisabled(lease)
	lease = p.withGatewayNativeDispatchAuthorization(lease, token, keyID)
	dial, _ := ctx.Value(appServerRealtimeDialerKey{}).(appServerRealtimeDialer)
	if dial == nil {
		dial = func(callCtx context.Context, h http.Header, selected *ProxyEntry, endpoint string) (gatewayWSUpstream, *http.Response, error) {
			return p.dialNativeGatewayWebSocketTargetLifetime(callCtx, callCtx, h, selected, endpoint)
		}
	}
	request, err := http.NewRequestWithContext(lease, http.MethodGet, target, nil)
	if err != nil {
		fail(500, "invalid realtime sideband request")
		return
	}
	request.Header = headers
	var upstream gatewayWSUpstream
	client := p.gatewayRequestClient(route, proxy, gatewayClientHTTP)
	res, err := p.doObservedAccountRequest(&rec, client, request, account, func(_ *http.Client, req *http.Request) (*http.Response, error) {
		var response *http.Response
		var dialErr error
		upstream, response, dialErr = dial(req.Context(), req.Header, proxy, target)
		if response != nil && response.StatusCode != http.StatusSwitchingProtocols {
			if upstream != nil {
				_ = upstream.CloseNow()
				upstream = nil
			}
			// An explicit 401 may refresh credentials and rejoin this same call
			// once. Accepted frames are never replayed; no other account is used.
			return response, nil
		}
		return response, dialErr
	}, func(updated Account) error {
		if gatewayAccountRef(updated) != owner {
			return &gatewayOwnerChangedError{}
		}
		return p.authorizeGatewayRetry(lease, token, keyID, updated)
	})
	if err != nil || upstream == nil {
		if res != nil && res.StatusCode != 101 {
			if res.Body != nil {
				defer res.Body.Close()
			}
			rec.Status, rec.HTTPCode, rec.Error = "error", res.StatusCode, "realtime sideband handshake rejected"
			copyResponseHeaders(w.Header(), res.Header)
			w.WriteHeader(res.StatusCode)
			if res.Body != nil {
				_, _ = io.Copy(w, res.Body)
			}
			return
		}
		fail(gatewayTransportFailureStatus(err), "realtime sideband connection failed")
		return
	}
	defer upstream.CloseNow()
	if res != nil {
		observeGatewayHeaders(&rec, res.Header)
	}
	downstream, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		rec.Status, rec.Error = "aborted", "realtime client upgrade failed"
		return
	}
	defer downstream.CloseNow()
	downstream.SetReadLimit(-1)
	upstream.SetReadLimit(-1)
	rec.HTTPCode, rec.Status, rec.Stream = 101, "ok", true
	stats := appServerRealtimeObservation{record: &rec, started: started}
	bridgeCtx, cancel := context.WithCancel(lease)
	defer cancel()
	finished := make(chan error, 2)
	bridge := func(dst, src gatewayWSUpstream, observe bool) {
		for {
			typ, payload, err := src.Read(bridgeCtx)
			if err != nil {
				finished <- err
				return
			}
			if observe && typ == websocket.MessageText {
				stats.eventAt(payload, time.Now())
			}
			if err = dst.Write(bridgeCtx, typ, payload); err != nil {
				finished <- err
				return
			}
		}
	}
	go bridge(upstream, downstream, false)
	go bridge(downstream, upstream, true)
	err = <-finished
	cancel()
	_ = downstream.CloseNow()
	_ = upstream.CloseNow()
	<-finished
	if status := websocket.CloseStatus(err); status != websocket.StatusNormalClosure && status != websocket.StatusGoingAway && !errors.Is(err, context.Canceled) {
		rec.Status, rec.Error = "aborted", "realtime transport closed"
	}
}
