package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// Fixed JSON endpoints used by official client-owned extension executors.
// Keep the exact protocol body and response; no server tool loop is involved.
func appServerAuxiliaryPath(path string) bool {
	switch strings.TrimPrefix(path, "/v1") {
	case "/alpha/search", "/images/generations", "/images/edits", "/memories/trace_summarize":
		return true
	default:
		return false
	}
}

func (p *Proxy) gatewayAppServerAuxiliary(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1")
	if !appServerAuxiliaryPath(path) {
		apiError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		apiError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if officialFailoverFrom(r.Context()) == nil {
		p.gatewayWithAccountFailover(w, r, p.gatewayAppServerAuxiliary)
		return
	}
	started := time.Now()
	token := bearerToken(r)
	keyID, keyName, _, err := p.resolveGatewayKeyContext(r.Context(), token)
	if err != nil {
		apiError(w, http.StatusServiceUnavailable, "gateway key authorization unavailable")
		return
	}
	if keyID == "" {
		apiError(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	settings, err := p.settingsValue()
	if err != nil {
		apiError(w, http.StatusServiceUnavailable, "gateway settings unavailable")
		return
	}
	rec := RequestRecord{Time: started.Unix(), KeyID: keyID, KeyName: keyName, Endpoint: strings.TrimPrefix(path, "/"), IP: clientIP(r)}
	observed, attempt := withGatewayAttemptObservation(r.Context())
	r = r.WithContext(observed)
	observeGatewayClientIdentity(&rec, gatewayObservedClientRequestID(r), "request")
	observeGatewayAuthentication(&rec, started, time.Now())
	w = &gatewayResponseWriter{ResponseWriter: w, record: &rec}
	failover := officialFailoverFrom(r.Context())
	defer func() {
		applyGatewayAttemptSnapshot(&rec, attempt)
		failover.record, failover.key = &rec, keyID
	}()
	raw, err := readBody(w, r)
	if err != nil {
		rec.Status, rec.Error = "error", err.Error()
		observeGatewayError(&rec, "client", "request_body")
		status := http.StatusBadRequest
		if errors.Is(err, errUnsupportedContentEncoding) {
			status = http.StatusUnsupportedMediaType
		} else if strings.Contains(err.Error(), "32 MiB") {
			status = http.StatusRequestEntityTooLarge
		}
		apiError(w, status, rec.Error)
		return
	}
	var envelope struct {
		Model string `json:"model"`
		ID    string `json:"id"`
	}
	if !json.Valid(raw) || json.Unmarshal(raw, &envelope) != nil {
		rec.Status, rec.Error = "error", "invalid search request JSON"
		apiError(w, http.StatusBadRequest, rec.Error)
		return
	}
	rec.Model = envelope.Model
	observeGatewayRequest(&rec, r.UserAgent(), "auxiliary_json", raw)
	attempt.ObserveInbound(r.URL.Path, raw)
	if !failover.retrying {
		failover.requestBody = append([]byte(nil), raw...)
	}
	session := gatewayExplicitSessionKey(r, raw)
	if session == "" {
		// SearchRequest.id is the official client's session id. Do not put it
		// into the request body again or derive/store a conversation here.
		session = envelope.ID
	}
	if session == "" {
		session = gatewayContentSessionKey(raw, gatewayUnboundedPayload(r.Context()))
	}
	if failover.session != "" {
		session = failover.session
	} else {
		failover.session = session
	}
	if err = p.bindInferenceSession(r.Context(), keyID, session); err != nil {
		rec.Status, rec.Error = "error", err.Error()
		apiError(w, http.StatusConflict, rec.Error)
		return
	}
	route := withGatewayCooldownModel(withGatewayRequestRoute(r.Context(), keyID, session), envelope.Model)
	defer stopGatewayRequestRoute(route)
	defer observeGatewayAdmissionLoss(&rec, route)
	route = p.withGatewayNativeDispatchAuthorization(route, token, keyID)
	var account Account
	var release func()
	if failover.retrying {
		account, release, err = p.pickFailoverAccount(route, token, keyID)
	} else {
		account, release, err = p.pickSessionAccountAwait(route, token, keyID, session)
	}
	if err != nil {
		officialObserveAttemptFailure(r.Context(), err)
		var unavailable *gatewayBoundAccountUnavailableError
		if errors.As(err, &unavailable) {
			failover.account, failover.owner = unavailable.Account, unavailable.Owner
		}
		rec.Status, rec.Error = "error", err.Error()
		observeGatewayError(&rec, "gateway", "account_selection")
		writeGatewaySelectionError(w, err)
		return
	}
	defer release()
	failover.account = account
	if !failover.retrying {
		failover.owner = gatewayAccountRef(account)
	}
	rec.Account, rec.AccountID, rec.AccountCredentialVersion = account.Name, account.ID, account.CredentialVersion
	rec.Groups = append([]string(nil), account.Groups...)
	lease, stop := p.officialFailoverLease(route, keyID, session, account)
	defer stop()
	proxy, err := p.gatewayRequestProxy(route, account)
	if err != nil {
		rec.Status, rec.Error = "error", "search proxy route unavailable"
		observeGatewayError(&rec, "gateway", "proxy_route")
		apiError(w, http.StatusServiceUnavailable, rec.Error)
		return
	}
	if proxy != nil {
		rec.ProxyName = proxy.Name
	}
	target, err := gatewayUpstreamURL(p.gatewayInferenceBaseURL(r.Context()), path, nil)
	if err != nil {
		rec.Status, rec.Error = "error", "invalid search upstream"
		apiError(w, http.StatusInternalServerError, rec.Error)
		return
	}
	up, err := http.NewRequestWithContext(withoutGatewayNativeRequestCompression(lease), http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		rec.Status, rec.Error = "error", "invalid search upstream request"
		apiError(w, http.StatusInternalServerError, rec.Error)
		return
	}
	if err = copyCodexRequestHeaders(up.Header, r.Header, settings); err != nil {
		rec.Status, rec.Error = "error", "invalid Codex fingerprint settings"
		apiError(w, http.StatusServiceUnavailable, rec.Error)
		return
	}
	p.AppServerCLI.applyIdentity(up.Header)
	up.Header.Del("Content-Encoding")
	up.Header.Del("Content-Length")
	up.Header.Set("Content-Type", "application/json")
	up.Header.Set("Accept", "application/json")
	up.Header.Set("Authorization", "Bearer "+account.AccessToken)
	if account.AccountID != "" {
		up.Header.Set("ChatGPT-Account-Id", account.AccountID)
	}
	attempt.ObserveSent(path, raw)
	client := p.gatewayRequestClientWithHeaderTimeout(route, proxy, gatewayClientHTTP, time.Duration(settings.UpstreamResponseHeaderTimeoutSeconds)*time.Second)
	res, err := p.doObservedAccountRequest(&rec, client, up, account, doHTTPAccountRequest,
		func(updated Account) error { return p.authorizeGatewayRetry(up.Context(), token, keyID, updated) })
	if err != nil {
		officialObserveAttemptFailure(r.Context(), err)
		rec.Status, rec.Error = "error", "upstream search request failed"
		observeGatewayTransportError(&rec, err, "response_headers")
		apiError(w, gatewayTransportFailureStatus(err), rec.Error)
		return
	}
	defer res.Body.Close()
	observeGatewayHeaders(&rec, res.Header)
	responseStarted := time.Now()
	result, err := io.ReadAll(res.Body)
	observeGatewayResponseTransfer(&rec, responseStarted, time.Now())
	if err != nil {
		rec.Status, rec.Error = "error", "upstream search response interrupted"
		observeGatewayError(&rec, "upstream", "response_body")
		apiError(w, http.StatusBadGateway, rec.Error)
		return
	}
	rec.HTTPCode = res.StatusCode
	if res.StatusCode >= 300 {
		observeAppServerCLIFailoverHTTP(r.Context(), res.StatusCode, result)
		rec.Status, rec.Error = "error", errorFromPayload(result)
		observeGatewayError(&rec, "upstream", "http_status")
	} else {
		if !commitAppServerCLIFailover(w, r.Context(), &rec) {
			return
		}
		rec.Status = "ok"
		// Only observe an actual standard usage object. Search responses often
		// have none; no token count or model completion is fabricated.
		if input, output, cached, known := usageFromResponse(result); known {
			rec.InputTokens, rec.OutputTokens, rec.CachedTokens = input, output, cached
			rec.UsageKnown = &known
			rec.UsageSchemaSource = "auxiliary_usage"
		}
	}
	copyResponseHeaders(w.Header(), res.Header)
	w.WriteHeader(res.StatusCode)
	if _, err = w.Write(result); err != nil {
		rec.Status, rec.Error = "aborted", "client search response delivery failed"
	}
}
