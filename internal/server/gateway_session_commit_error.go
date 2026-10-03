package server

import "errors"

// A binding commit can fail before the downstream writer is ever called.
// Preserve the exact cause for cancellation and existing protocol handling.
type gatewaySessionCommitError struct{ cause error }

func (e *gatewaySessionCommitError) Error() string { return e.cause.Error() }
func (e *gatewaySessionCommitError) Unwrap() error { return e.cause }

func observeGatewaySessionCommitError(record *RequestRecord, err error) bool {
	var commit *gatewaySessionCommitError
	if record == nil || !errors.As(err, &commit) {
		return false
	}
	record.Status, record.ErrorSource, record.ErrorPhase = "error", "gateway", "session_binding"
	record.Error = commit.Error()
	// The commit barrier rejected delivery. An observed upstream delta here
	// is not a first output successfully delivered to the client.
	record.TTFTMs = nil
	record.FirstOutputKind = ""
	return true
}
