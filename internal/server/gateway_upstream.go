package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// BaseURL is an endpoint prefix, not a request URL. Reject query strings and
// fragments rather than silently appending an API route into either of them.
// This also keeps HTTP and WebSocket upstream configuration consistent.
func gatewayUpstreamURL(baseURL, path string, query url.Values) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil || base == nil || (base.Scheme != "http" && base.Scheme != "https") ||
		base.Hostname() == "" || base.User != nil || base.Opaque != "" ||
		base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || strings.Contains(baseURL, "#") {
		return "", errors.New("invalid gateway upstream base URL")
	}
	// Join the escaped path to preserve encoded slash and Unicode prefixes.
	joined := strings.TrimRight(base.EscapedPath(), "/") + "/" + strings.TrimLeft(path, "/")
	base.Path, err = url.PathUnescape(joined)
	if err != nil {
		return "", errors.New("invalid gateway upstream path")
	}
	base.RawPath = joined
	base.RawQuery = query.Encode()
	return base.String(), nil
}

func doHTTPAccountRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	return client.Do(req)
}

// Only an explicit HTTP 401 can trigger auth recovery. Network failures or
// errors inside an already accepted SSE response must never replay a turn.
func (p *Proxy) doAccountRequest(client *http.Client, req *http.Request, account Account, do func(*http.Client, *http.Request) (*http.Response, error), authorizeRetry func(Account) error) (*http.Response, error) {
	dispatch := func(request *http.Request, current Account) (*http.Response, error) {
		if err := p.checkGatewayPaymentDispatch(request.Context(), current); err != nil {
			return nil, err
		}
		if p.isBPSUpstreamRequest(request) {
			if request.Header == nil {
				request.Header = make(http.Header)
			}
			applyBPSHeaders(request.Header, current)
		}
		request = request.WithContext(withGatewayRequestCredentials(request.Context(), current))
		request = request.WithContext(withGatewayNativeAccountScope(request.Context(), current))
		if authorize, _ := request.Context().Value(gatewayNativeDispatchAuthorizerKey{}).(gatewayNativeDispatchAuthorizer); p.nativeHTTPFor(request.Context()) != nil && authorize != nil {
			request = request.WithContext(withGatewayNativeDispatchGuard(request.Context(), func(dispatchCtx context.Context) error {
				return authorize(dispatchCtx, current)
			}))
		}
		res, err := do(client, request)
		if err == nil {
			switch {
			case res == nil:
				err = errors.New("upstream transport returned no response")
			case res.Body == nil && res.ContentLength > 0:
				err = errors.New("upstream transport returned no response body")
			case res.Body == nil:
				res.Body = http.NoBody
			}
		}
		if err == nil {
			res = gatewayHTTPResponseView(res)
			p.observeGatewayCooldownResponse(request.Context(), current, res)
		}
		return res, err
	}
	res, err := dispatch(req, account)
	if err != nil || res.StatusCode != http.StatusUnauthorized || authorizeRetry == nil || (req.Body != nil && req.Body != http.NoBody && req.GetBody == nil) {
		return res, err
	}
	updated, err := p.recoverUnauthorized(req.Context(), account, client)
	if err != nil {
		if canceled := req.Context().Err(); canceled != nil {
			_ = res.Body.Close()
			return nil, canceled
		}
		var ownerChanged *gatewayOwnerChangedError
		if errors.As(err, &ownerChanged) {
			_ = res.Body.Close()
			return nil, err
		}
		// Keep the original status and diagnostic payload available to the CLI.
		return res, nil
	}
	if err := authorizeRetry(updated); err != nil {
		_ = res.Body.Close()
		return nil, err
	}
	if err := req.Context().Err(); err != nil {
		_ = res.Body.Close()
		return nil, err
	}
	retry := req.Clone(req.Context())
	if req.GetBody != nil {
		retry.Body, err = req.GetBody()
		if err != nil || (retry.Body == nil && req.Body != nil && req.Body != http.NoBody) || (retry.Body == http.NoBody && req.ContentLength > 0) {
			if retry.Body != nil {
				_ = retry.Body.Close()
			}
			if canceled := req.Context().Err(); canceled != nil {
				_ = res.Body.Close()
				return nil, canceled
			}
			return res, nil
		}
	}
	if err := req.Context().Err(); err != nil {
		if retry.Body != nil {
			_ = retry.Body.Close()
		}
		_ = res.Body.Close()
		return nil, err
	}
	if retry.Header == nil {
		retry.Header = make(http.Header)
	}
	retry.Header.Set("Authorization", "Bearer "+updated.AccessToken)
	retry.Header.Del("ChatGPT-Account-Id")
	if updated.AccountID != "" {
		retry.Header.Set("ChatGPT-Account-Id", updated.AccountID)
	}
	_ = res.Body.Close()
	return dispatch(retry, updated)
}
