package server

import (
	"net/http"
	"regexp"
	"time"
)

var gatewayClientRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// Only an existing, unambiguous correlation header is observed. Never generate,
// forward or echo a new identifier, or fall back to authentication/session data.
func gatewayObservedClientRequestID(r *http.Request) string {
	values := r.Header.Values("X-Client-Request-ID")
	if len(values) != 1 || !gatewayClientRequestIDPattern.MatchString(values[0]) {
		return ""
	}
	return values[0]
}

func observeGatewayClientIdentity(rec *RequestRecord, id, scope string) {
	if !gatewayClientRequestIDPattern.MatchString(id) {
		return
	}
	rec.ClientRequestID, rec.ClientRequestIDSource, rec.ClientRequestIDScope = id, "request_header", scope
}

func observeGatewayAuthentication(rec *RequestRecord, started, finished time.Time) {
	if duration := gatewayObservedDuration(started, finished); duration != nil {
		rec.AuthenticationMs = duration
		rec.TimingSource = "gateway_handler_v1"
	}
}

func observeGatewayResponseTransfer(rec *RequestRecord, started, finished time.Time) {
	if duration := gatewayObservedDuration(started, finished); duration != nil {
		rec.ResponseTransferMs = duration
		rec.TimingSource = "gateway_handler_v1"
	}
}
