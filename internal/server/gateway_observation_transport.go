package server

import (
	"context"
	"errors"
	"net/http"
	"time"
)

type gatewayTransportAttemptKey struct{}

// Capture headers at the actual attempt boundary, not after OAuth recovery.
// A failed retry has no final response headers even if its first attempt did.
func (p *Proxy) doObservedAccountRequest(rec *RequestRecord, client *http.Client, req *http.Request, account Account, do func(*http.Client, *http.Request) (*http.Response, error), authorizeRetry func(Account) error, transport ...string) (*http.Response, error) {
	started := time.Now()
	var observed time.Time
	kind := "request"
	if len(transport) > 0 && transport[0] == "websocket" {
		kind = "websocket"
	}
	res, err := p.doAccountRequest(client, req, account, func(c *http.Client, r *http.Request) (*http.Response, error) {
		observeGatewayRequestCredentials(rec, r.Context())
		attempt := gatewayAttemptObservationFrom(r.Context()).Begin(kind, r.URL.EscapedPath(), time.Now())
		r = r.WithContext(context.WithValue(r.Context(), gatewayTransportAttemptKey{}, attempt))
		// The selected account steers per-account WebSocket availability and
		// socket reuse without widening the dial helpers' signatures.
		if account.ID != "" {
			r = r.WithContext(context.WithValue(r.Context(), gatewayWSAccountKey{}, gatewayWSAccountRoute{ID: account.ID, CredentialVersion: account.CredentialVersion}))
		}
		response, requestErr := do(c, r)
		var unsent *gatewayNativeDispatchUnsentError
		if response == nil && errors.As(requestErr, &unsent) {
			rec.AccountCredentialVersion = 0
			attempt.discardUnsent()
		} else {
			finishGatewayObservedAttempt(attempt, response, requestErr)
		}
		if requestErr == nil && response != nil {
			observed = time.Now()
		}
		return response, requestErr
	}, authorizeRetry)
	if err == nil && res != nil && !observed.IsZero() {
		observeGatewayHeaderTiming(rec, started, observed)
	}
	return res, err
}

func observeGatewayTransportError(rec *RequestRecord, err error, phase string) {
	if errors.Is(err, errNativeWSNoCompressionReady) {
		observeGatewayError(rec, "gateway", "native_websocket_compression_capability")
		return
	}
	if errors.Is(err, errGatewayNativeUnboundedReady) {
		observeGatewayError(rec, "gateway", "native_payload_capability")
		return
	}
	var authorization *gatewayRecoveryAuthorizationError
	if errors.As(err, &authorization) {
		observeGatewayError(rec, "gateway", "authorization")
		return
	}
	if errors.Is(err, errGatewayCompressionCapability) {
		observeGatewayError(rec, "gateway", "native_compression_capability")
		return
	}
	var ownerChanged *gatewayOwnerChangedError
	if errors.As(err, &ownerChanged) {
		observeGatewayError(rec, "gateway", "account_identity")
		return
	}
	if errors.Is(err, errGatewayNativeCapacity) || errors.Is(err, errGatewayNativeClosed) {
		observeGatewayError(rec, "gateway", "native_transport_capacity")
		return
	}
	if errors.Is(err, errGatewayNativeProxy) {
		observeGatewayError(rec, "gateway", "native_proxy_configuration")
		return
	}
	if errors.Is(err, errGatewayProxyUnavailable) {
		observeGatewayError(rec, "gateway", "proxy_route")
		return
	}
	observeGatewayError(rec, "transport", phase)
}
