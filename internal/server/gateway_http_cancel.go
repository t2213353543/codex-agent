package server

import (
	"context"
	"errors"
)

// Attribute only an observed caller cancellation carried by this read error.
// A later ctx cancellation cannot replace a real upstream EOF/idle error, and
// an internal compact/native deadline does not imply the caller ended its turn.
func recordGatewayHTTPReadCancellation(rec *RequestRecord, ctx context.Context, readErr error) bool {
	if rec == nil || ctx == nil || readErr == nil || rec.httpCompleted || rec.TerminalStatus != "" || rec.Status == "error" {
		return false
	}
	cause := ctx.Err()
	if cause == nil || !errors.Is(readErr, cause) {
		return false
	}
	switch cause {
	case context.Canceled:
		rec.Status, rec.Error = "aborted", "client request canceled"
		rec.ErrorSource, rec.ErrorPhase = "client", "canceled"
	case context.DeadlineExceeded:
		rec.Status, rec.Error = "aborted", "client request deadline exceeded"
		rec.ErrorSource, rec.ErrorPhase = "client", "timeout"
	default:
		return false
	}
	return true
}
