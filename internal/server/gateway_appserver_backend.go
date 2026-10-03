package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

func appServerBackendPath(path string) bool {
	if appServerBackendReadPath(path) || path == "/backend-api/ps/mcp" {
		return true
	}
	ok, _ := appServerBackendDynamicPath(path)
	return ok || path == "/backend-api/wham/tasks" || path == "/backend-api/wham/usage/thread_usage/query" || path == "/backend-api/wham/rate-limit-reset-credits/consume" || path == "/backend-api/wham/accounts/send_add_credits_nudge_email"
}

func appServerBackendAllowedMethod(path string) (string, bool) {
	if path == "/backend-api/ps/mcp" {
		return "", true // GET/POST/DELETE are all valid for MCP.
	}
	if path == "/backend-api/wham/tasks" || path == "/backend-api/wham/usage/thread_usage/query" || path == "/backend-api/wham/rate-limit-reset-credits/consume" || path == "/backend-api/wham/accounts/send_add_credits_nudge_email" {
		return http.MethodPost, true
	}
	if ok, method := appServerBackendDynamicPath(path); ok {
		return method, true
	}
	if appServerBackendReadPath(path) {
		return http.MethodGet, true
	}
	return "", false
}

// Backend APIs consume the identity the client actually hydrated. A backend
// request must never select a different pooled account merely to return 200.
func (p *Proxy) gatewayAppServerBackend(w http.ResponseWriter, r *http.Request) {
	mcp := r.URL.Path == "/backend-api/ps/mcp"
	dynamic, _ := appServerBackendDynamicPath(r.URL.Path)
	if !appServerBackendPath(r.URL.Path) {
		apiError(w, 404, "not found")
		return
	}
	allowed, _ := appServerBackendAllowedMethod(r.URL.Path)
	if allowed == "" && !mcp {
		allowed = http.MethodGet
	}
	if strings.HasPrefix(r.URL.Path, "/backend-api/public/plugins/workspace/") && dynamic && r.URL.Path != "/backend-api/public/plugins/workspace/upload-url" {
		allowed = http.MethodDelete
		if r.Method == http.MethodPost {
			allowed = http.MethodPost
		}
	}
	if !mcp && r.Method != allowed || mcp && r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodDelete {
		w.Header().Set("Allow", allowed)
		if mcp {
			w.Header().Set("Allow", "GET, POST, DELETE")
		}
		apiError(w, 405, "method not allowed")
		return
	}
	started := time.Now()
	token := bearerToken(r)
	key, name, _, err := p.resolveGatewayKeyContext(r.Context(), token)
	if err != nil {
		apiError(w, 503, "gateway key authorization unavailable")
		return
	}
	if key == "" {
		apiError(w, 401, "invalid API key")
		return
	}
	owner, err := p.sessionAccount(r.Context(), key, appServerClientIdentitySession, "")
	if err != nil {
		apiError(w, 503, "delegated identity storage unavailable")
		return
	}
	if owner == "" {
		apiError(w, 401, "load delegated client identity before requesting account services")
		return
	}
	ctx := context.WithValue(withAppServerRequiredOwner(r.Context()), appServerDelegatedOwnerKey{}, owner)
	ctx = withGatewayRequestRoute(ctx, key, appServerClientIdentitySession)
	defer stopGatewayRequestRoute(ctx)
	ctx = p.withGatewayNativeDispatchAuthorization(ctx, token, key)
	account, release, err := p.pickSessionAccount(ctx, token, key, appServerClientIdentitySession)
	if err != nil {
		writeGatewaySelectionError(w, err)
		return
	}
	defer release()
	authorizeOwner := func(checkCtx context.Context, current Account) error {
		if gatewayAccountRef(current) != owner {
			return &gatewayOwnerChangedError{}
		}
		if err := p.authorizeGatewayRetry(checkCtx, token, key, current); err != nil {
			return err
		}
		bound, err := p.sessionAccount(checkCtx, key, appServerClientIdentitySession, "")
		if err != nil {
			return err
		}
		if bound != owner {
			return errors.New("delegated identity changed during account service request")
		}
		return nil
	}
	ctx = context.WithValue(ctx, gatewayNativeDispatchAuthorizerKey{}, gatewayNativeDispatchAuthorizer(authorizeOwner))
	if requested := r.Header.Get("ChatGPT-Account-Id"); requested != "" && requested != account.AccountID {
		apiError(w, 409, "account service identity does not match delegated login")
		return
	}
	rec := RequestRecord{Time: started.Unix(), KeyID: key, KeyName: name, Endpoint: strings.TrimPrefix(r.URL.Path, "/"), Account: account.Name, AccountID: account.ID, AccountCredentialVersion: account.CredentialVersion, IP: clientIP(r), unboundedPayload: gatewayUnboundedPayload(ctx)}
	ctx, observation := withGatewayAttemptObservation(ctx)
	observeGatewayRequest(&rec, r.UserAgent(), "backend_client", nil)
	w = &gatewayResponseWriter{ResponseWriter: w, record: &rec}
	defer func() { applyGatewayAttemptSnapshot(&rec, observation); p.finishRecord(&rec, started, key) }()
	defer observeGatewayAdmissionLoss(&rec, ctx)
	lease, stop := p.keepGatewaySession(ctx, key, appServerClientIdentitySession, account)
	defer stop()
	proxy, err := p.gatewayRequestProxy(lease, account)
	if err != nil {
		rec.Status, rec.Error = "error", "account service proxy unavailable"
		apiError(w, 503, rec.Error)
		return
	}
	if proxy != nil {
		rec.ProxyName = proxy.Name
	}
	settings, err := p.settingsValue()
	if err != nil {
		rec.Status, rec.Error = "error", "gateway settings unavailable"
		apiError(w, 503, rec.Error)
		return
	}
	var raw []byte
	if r.Method == http.MethodPost || r.Method == http.MethodDelete {
		raw, err = readBody(w, r)
		if err != nil {
			rec.Status, rec.Error = "error", "invalid account service body"
			apiError(w, 400, rec.Error)
			return
		}
		if mcp {
			if mirrored, changed, mirrorErr := appServerMirrorMCPMetadata(raw); mirrorErr != nil {
				rec.Status, rec.Error = "error", "invalid MCP metadata"
				apiError(w, 400, rec.Error)
				return
			} else if changed {
				raw = mirrored
			}
		}
	}
	// r.URL.Path is an exact allowlisted backend path, never a URL/host supplied
	// by the client. Query parameters retain the official consumer's semantics.
	target, err := gatewayUpstreamURL("https://chatgpt.com", r.URL.Path, r.URL.Query())
	if err != nil {
		rec.Status, rec.Error = "error", "invalid account service route"
		apiError(w, 500, rec.Error)
		return
	}
	up, err := http.NewRequestWithContext(withoutGatewayNativeRequestCompression(lease), r.Method, target, bytes.NewReader(raw))
	if err != nil {
		apiError(w, 500, "invalid account service request")
		return
	}
	if err = copyCodexRequestHeaders(up.Header, r.Header, settings); err != nil {
		apiError(w, 503, "invalid client identity profile")
		return
	}
	copyGatewayHeaders(up.Header, r.Header, func(name string) bool {
		switch name {
		case "accept", "content-type", "mcp-session-id", "mcp-protocol-version", "last-event-id", "oai-product-sku", "x-openai-product-sku", "x-session-id":
			return true
		}
		return false
	}, func(string) bool { return true })
	p.AppServerCLI.applyIdentity(up.Header)
	up.Header.Set("Authorization", "Bearer "+account.AccessToken)
	up.Header.Set("ChatGPT-Account-Id", account.AccountID)
	if up.Header.Get("Accept") == "" {
		up.Header.Set("Accept", "application/json")
	}
	observation.ObserveInbound(r.URL.Path, raw)
	observation.ObserveSent(r.URL.Path, raw)
	client := p.gatewayRequestClientWithHeaderTimeout(lease, proxy, gatewayClientHTTP, time.Duration(settings.UpstreamResponseHeaderTimeoutSeconds)*time.Second)
	res, err := p.doObservedAccountRequest(&rec, client, up, account, func(c *http.Client, request *http.Request) (*http.Response, error) {
		if err := authorizeOwner(request.Context(), account); err != nil {
			return nil, err
		}
		return doHTTPAccountRequest(c, request)
	}, func(updated Account) error {
		return authorizeOwner(lease, updated)
	})
	if err != nil {
		rec.Status, rec.Error = "error", "account service upstream failed"
		observeGatewayTransportError(&rec, err, "response_headers")
		apiError(w, gatewayTransportFailureStatus(err), rec.Error)
		return
	}
	defer res.Body.Close()
	if err = p.authorizeGatewayRetry(lease, token, key, account); err != nil {
		rec.Status, rec.Error = "error", "account service authorization changed"
		apiError(w, gatewayTransportFailureStatus(err), rec.Error)
		return
	}
	if current, err := p.sessionAccount(lease, key, appServerClientIdentitySession, ""); err != nil || current != owner {
		rec.Status, rec.Error = "error", "delegated identity changed during account service request"
		apiError(w, 409, rec.Error)
		return
	}
	observeGatewayHeaders(&rec, res.Header)
	copyResponseHeaders(w.Header(), res.Header)
	copyGatewayHeaders(w.Header(), res.Header, func(name string) bool {
		return name == "mcp-session-id" || name == "mcp-protocol-version" || name == "www-authenticate"
	}, func(string) bool { return true })
	rec.Status = "ok"
	if res.StatusCode >= 300 {
		rec.Status, rec.Error = "error", "account service upstream rejected request"
	}
	w.WriteHeader(res.StatusCode)
	if strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		rec.Stream = true
		err = writeStreamPipe(w, http.NewResponseController(w), res, nil, true)
	} else {
		_, err = io.Copy(w, res.Body)
	}
	if err != nil {
		rec.Status, rec.Error = "aborted", "account service response interrupted"
	}
}
