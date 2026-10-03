package server

import (
	"context"
	"encoding/json"
	"net/http"
)

// HTTP authentication/quota failures are definitive account-local rejection.
// Transport errors, server errors, arbitrary error text and other 4xx never
// authorize replay on a different account.
func observeAppServerCLIFailoverHTTP(ctx context.Context, status int, body []byte) {
	if officialFailoverFrom(ctx) == nil {
		return
	}
	code := ""
	if status == http.StatusUnauthorized {
		code = "unauthorized"
	}
	if status == http.StatusTooManyRequests {
		code = "rateLimitExceeded"
	}
	if status == http.StatusPaymentRequired || status >= 400 && gatewayPaymentFailureBody(body) {
		status, code = http.StatusPaymentRequired, "payment_required"
	}
	if code != "" {
		officialObserveAttemptFailure(ctx, &officialProtocolError{message: "upstream rejected account", status: status, code: code, source: "upstream"})
	}
}

func observeAppServerCLIFailoverEvent(ctx context.Context, raw []byte) {
	if officialFailoverFrom(ctx) == nil {
		return
	}
	if fields, err := gatewayWSParseFields(raw); err == nil && gatewayPaymentFailureFields(fields, true) {
		officialObserveAttemptFailure(ctx, &officialProtocolError{message: "upstream account payment required", status: http.StatusPaymentRequired, code: "payment_required", source: "upstream"})
		return
	}
	var event struct {
		Type       string `json:"type"`
		Code       string `json:"code"`
		Status     int    `json:"status"`
		StatusCode int    `json:"status_code"`
		Error      struct {
			Code   string `json:"code"`
			Type   string `json:"type"`
			Status int    `json:"status"`
		} `json:"error"`
		Response struct {
			Error struct {
				Code   string `json:"code"`
				Type   string `json:"type"`
				Status int    `json:"status"`
			} `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &event) != nil || event.Type != "error" && event.Type != "response.failed" {
		return
	}
	status := event.Status
	if status == 0 {
		status = event.StatusCode
	}
	code := event.Code
	for _, value := range []struct {
		code, kind string
		status     int
	}{{event.Code, "", status}, {event.Error.Code, event.Error.Type, event.Error.Status}, {event.Response.Error.Code, event.Response.Error.Type, event.Response.Error.Status}} {
		switch {
		case value.status == 401 || value.code == "unauthorized" || value.code == "authentication_error" || value.code == "invalid_api_key" || value.kind == "authentication_error":
			status = 401
			code = "unauthorized"
		case value.status == 429 || value.code == "usage_limit_reached" || value.code == "rate_limit_exceeded" || value.code == "rateLimitExceeded" || value.code == "usageLimitExceeded" || value.code == "basispoints_route_rate_limit_exceeded" || value.kind == "rate_limit_error":
			status = 429
			code = "rateLimitExceeded"
			if value.code == "usageLimitExceeded" {
				code = value.code
			}
		}
	}
	if status == 401 || status == 429 {
		officialObserveAttemptFailure(ctx, &officialProtocolError{message: "upstream rejected account before completion", status: status, code: code, source: "upstream"})
	}
}

func commitAppServerCLIFailover(w http.ResponseWriter, ctx context.Context, record *RequestRecord) bool {
	state := officialFailoverFrom(ctx)
	if state == nil {
		return true
	}
	if err := state.commitOwner(); err != nil {
		record.Status, record.Error, record.HTTPCode = "error", err.Error(), http.StatusConflict
		observeGatewayError(record, "gateway", "session_binding")
		apiError(w, http.StatusConflict, "session account changed during authentication failover")
		return false
	}
	return true
}
