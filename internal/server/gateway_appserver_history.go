package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

type appServerRequiredOwnerKey struct{}

func withAppServerRequiredOwner(ctx context.Context) context.Context {
	return context.WithValue(ctx, appServerRequiredOwnerKey{}, true)
}

func appServerRequiresOwner(ctx context.Context) bool {
	value, _ := ctx.Value(appServerRequiredOwnerKey{}).(bool)
	return value
}

// These are the exact endpoints in codex-rs/ext/history-notes/src/tools.rs
// and extension.rs. They access account-owned history/notes, not a generic
// stateless inference service; never use the cross-account failover wrapper.
func appServerHistoryPath(path string) bool {
	switch strings.TrimPrefix(path, "/v1") {
	case "/alpha/history/v2/list_windows", "/alpha/history/v2/list_items",
		"/alpha/history/v2/read_item", "/alpha/history/v2/search_contents",
		"/alpha/notes/v2/list_files_by_prefix", "/alpha/notes/v2/read_file",
		"/alpha/notes/v2/search_contents", "/alpha/notes/v2/append_to_file",
		"/alpha/notes/v2/write_file", "/alpha/notes/v2/thread_hint":
		return true
	}
	return false
}

// Context was injected by the official client executor. Routing consumes its
// exact session identity without manufacturing an agent name or rewriting any
// argument, including encrypted query/text values or opaque window/item IDs.
func appServerHistorySession(r *http.Request, body []byte) (string, error) {
	var envelope struct{ Context json.RawMessage }
	if gatewayWSDecodeExact(body, map[string]any{"context": &envelope.Context}) != nil {
		return "", errors.New("history request must be a JSON object")
	}
	var identity struct{ Session, Agent string }
	if gatewayWSDecodeExact(envelope.Context, map[string]any{"session_id": &identity.Session, "current_agent_name": &identity.Agent}) != nil || strings.TrimSpace(identity.Session) == "" || strings.TrimSpace(identity.Agent) == "" {
		return "", errors.New("history request requires context.session_id and context.current_agent_name")
	}
	if explicit := gatewayExplicitSessionKey(r, nil); explicit != "" && explicit != identity.Session {
		return "", errors.New("history context conflicts with request session identity")
	}
	return identity.Session, nil
}

func copyAppServerHistoryHeaders(dst, src http.Header) {
	copyGatewayHeaders(dst, src, func(name string) bool {
		return name == "x-openai-encrypted-tool-arguments" || name == "x-openai-tool-output-truncation-policy"
	}, func(string) bool { return true })
}

func (p *Proxy) gatewayAppServerHistory(w http.ResponseWriter, r *http.Request) {
	if !appServerHistoryPath(r.URL.Path) || r.URL.RawQuery != "" || r.URL.RawPath != "" && r.URL.RawPath != r.URL.Path {
		apiError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		apiError(w, http.StatusMethodNotAllowed, "method not allowed")
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
	path := strings.TrimPrefix(r.URL.Path, "/v1")
	ctx, attempt := withGatewayAttemptObservation(withAppServerRequiredOwner(r.Context()))
	r = r.WithContext(ctx)
	rec := RequestRecord{unboundedPayload: gatewayUnboundedPayload(ctx), Time: started.Unix(), KeyID: keyID, KeyName: keyName,
		Endpoint: strings.TrimPrefix(path, "/"), IP: clientIP(r)}
	observeGatewayClientIdentity(&rec, gatewayObservedClientRequestID(r), "request")
	observeGatewayAuthentication(&rec, started, time.Now())
	w = &gatewayResponseWriter{ResponseWriter: w, record: &rec}
	defer func() {
		applyGatewayAttemptSnapshot(&rec, attempt)
		p.finishRecord(&rec, started, keyID)
	}()
	fail := func(status int, message, source, phase string) {
		rec.Status, rec.Error = "error", message
		observeGatewayError(&rec, source, phase)
		apiError(w, status, message)
	}
	raw, err := readBody(w, r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errUnsupportedContentEncoding) {
			status = http.StatusUnsupportedMediaType
		}
		fail(status, "cannot read history request", "client", "request_body")
		return
	}
	session, err := appServerHistorySession(r, raw)
	if err != nil {
		fail(http.StatusBadRequest, err.Error(), "client", "request_body")
		return
	}
	attempt.ObserveInbound(r.URL.Path, raw)
	observeGatewayRequest(&rec, r.UserAgent(), "auxiliary_json", raw)
	if err := p.bindInferenceSession(ctx, keyID, session); err != nil {
		fail(http.StatusConflict, err.Error(), "gateway", "session_binding")
		return
	}
	route := p.withGatewayNativeDispatchAuthorization(withGatewayRequestRoute(ctx, keyID, session), token, keyID)
	defer stopGatewayRequestRoute(route)
	defer observeGatewayAdmissionLoss(&rec, route)
	selectionStarted := time.Now()
	account, release, err := p.pickSessionAccount(route, token, keyID, session)
	attempt.ObserveStage("account_selection", selectionStarted, time.Now())
	if err != nil {
		rec.Status, rec.Error = "error", err.Error()
		observeGatewayError(&rec, "gateway", "account_selection")
		writeGatewaySelectionError(w, err)
		return
	}
	defer release()
	authorizeOwner := func(checkCtx context.Context, current Account) error {
		if gatewayAccountRef(current) != gatewayAccountRef(account) {
			return &gatewayOwnerChangedError{}
		}
		if err := p.authorizeGatewayRetry(checkCtx, token, keyID, current); err != nil {
			return err
		}
		bound, err := p.sessionAccount(checkCtx, keyID, session, "")
		if err != nil {
			return err
		}
		if bound != gatewayAccountRef(account) {
			return errors.New("history session account changed")
		}
		return nil
	}
	// Recheck owner binding at native IPC dispatch too, after cold helper startup
	// or payload preparation. The ordinary HTTP transport checks before Do.
	route = context.WithValue(route, gatewayNativeDispatchAuthorizerKey{}, gatewayNativeDispatchAuthorizer(authorizeOwner))
	rec.Account, rec.AccountID, rec.AccountCredentialVersion = account.Name, account.ID, account.CredentialVersion
	rec.Groups = append([]string(nil), account.Groups...)
	lease, stop := p.keepGatewaySession(route, keyID, session, account)
	defer stop()
	proxyStarted := time.Now()
	proxy, err := p.gatewayRequestProxy(route, account)
	attempt.ObserveStage("proxy_selection", proxyStarted, time.Now())
	if err != nil {
		fail(http.StatusServiceUnavailable, "history proxy route unavailable", "gateway", "proxy_route")
		return
	}
	if proxy != nil {
		rec.ProxyName = proxy.Name
	}
	target, err := gatewayUpstreamURL(p.gatewayInferenceBaseURL(ctx), path, nil)
	if err != nil {
		fail(http.StatusInternalServerError, "invalid history upstream", "gateway", "request_build")
		return
	}
	up, err := http.NewRequestWithContext(withoutGatewayNativeRequestCompression(lease), http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		fail(http.StatusInternalServerError, "invalid history upstream request", "gateway", "request_build")
		return
	}
	if err := copyCodexRequestHeaders(up.Header, r.Header, settings); err != nil {
		fail(http.StatusServiceUnavailable, "invalid Codex fingerprint settings", "gateway", "request_headers")
		return
	}
	copyAppServerHistoryHeaders(up.Header, r.Header)
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
	// doObservedAccountRequest retries only an explicit HTTP 401, after a
	// same-owner credential refresh. Transport ambiguity or any accepted write
	// never causes replay, and this handler never switches accounts.
	res, err := p.doObservedAccountRequest(&rec, client, up, account, func(c *http.Client, req *http.Request) (*http.Response, error) {
		if err := authorizeOwner(req.Context(), account); err != nil {
			return nil, err
		}
		return doHTTPAccountRequest(c, req)
	}, func(updated Account) error {
		return authorizeOwner(up.Context(), updated)
	})
	if err != nil {
		rec.Status, rec.Error = "error", "upstream history request failed"
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
		fail(http.StatusBadGateway, "upstream history response interrupted", "upstream", "response_body")
		return
	}
	rec.HTTPCode = res.StatusCode
	if res.StatusCode >= 300 {
		rec.Status, rec.Error = "error", errorFromPayload(result)
		observeGatewayError(&rec, "upstream", "http_status")
	} else {
		if err := authorizeOwner(lease, account); err != nil {
			fail(http.StatusConflict, "history session ownership is no longer valid", "gateway", "session_binding")
			return
		}
		rec.Status = "ok"
		if input, output, cached, known := usageFromResponse(result); known {
			rec.InputTokens, rec.OutputTokens, rec.CachedTokens = input, output, cached
			rec.UsageKnown, rec.UsageSchemaSource = &known, "auxiliary_usage"
		}
	}
	copyResponseHeaders(w.Header(), res.Header)
	w.WriteHeader(res.StatusCode)
	if _, err := w.Write(result); err != nil {
		rec.Status, rec.Error = "aborted", "client history response delivery failed"
	}
}
