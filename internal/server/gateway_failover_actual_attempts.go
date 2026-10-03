package server

import "errors"

// Only machine classifications produced by the gateway's typed upstream
// protocol errors qualify. Never derive final generation errors from OAuth or
// free-form error messages, and never use the last chronological network item.
func (s *officialFailoverState) captureFinalGenerationFailure(record *RequestRecord) {
	record.FinalUpstreamErrorCode, record.FinalUpstreamErrorSource = "", ""
	if record.Status != "error" || record.SentModel == "" || (record.UpstreamPath != "/responses" && record.UpstreamPath != "/responses/compact") {
		return
	}
	var protocol *officialProtocolError
	var rejected *bpsAccountRejectedError
	if !errors.As(s.failure, &protocol) && !errors.As(s.failure, &rejected) {
		return
	}
	status, code, source := officialFailureHTTP(s.failure, record.HTTPCode)
	if source != "upstream" || status < 400 {
		return
	}
	switch code {
	case "unauthorized", "authentication_error", "invalid_api_key", "authentication_unavailable",
		"rateLimitExceeded", "usageLimitExceeded", "rate_limit_exceeded", "rate_limit_error", "insufficient_quota", "billing_hard_limit_reached",
		"upstream_account_rejected", "serverOverloaded", "server_overloaded", "model_overloaded", "overloaded_error":
		record.FinalUpstreamErrorCode, record.FinalUpstreamErrorSource = code, source
	}
}

// BPS exposes exact transport observations, unlike hosted runtimes whose
// inner network boundary is not observable here. Retain the former without
// also counting the outer handler as another generation request.
type officialFailoverActualAttempts struct {
	items     []GatewayAttemptSummary
	truncated bool
	observer  *gatewayAttemptObservation
	consumed  int
}

func (s *officialFailoverState) captureActualAttempts(observer *gatewayAttemptObservation, record *RequestRecord) {
	s.captureFinalGenerationFailure(record)
	if observer == nil {
		return
	}
	if s.actualAttempts == nil {
		s.actualAttempts = &officialFailoverActualAttempts{}
	}
	actual := s.actualAttempts
	snapshot := observer.Snapshot()
	if actual.observer != observer {
		actual.observer, actual.consumed = observer, 0
	}
	if snapshot.AttemptsTruncated != nil && *snapshot.AttemptsTruncated {
		actual.truncated = true
	}
	for index := actual.consumed; index < len(snapshot.Attempts); index++ {
		if len(actual.items) == gatewayAttemptObservationLimit {
			actual.truncated = true
			break
		}
		entry := snapshot.Attempts[index]
		// Preserve the outer handler's legacy logical outcome on its final
		// generation attempt. The separately captured HTTP status stays unchanged.
		// OAuth attempts keep their own status and are never relabeled as generation.
		if index == len(snapshot.Attempts)-1 && (entry.Phase == "request" || entry.Phase == "websocket" && s.failure != nil) && (snapshot.AttemptsTruncated == nil || !*snapshot.AttemptsTruncated) {
			status, code, source := officialFailureHTTP(s.failure, record.HTTPCode)
			if status >= 100 && status <= 599 {
				entry.StatusCode = new(int)
				*entry.StatusCode = status
			}
			if s.failure == nil {
				// No typed protocol outcome replaces the actual producer's
				// transport error. Keep timeout/cancellation/network evidence.
				code, source = "", record.ErrorSource
				if record.Status != "ok" {
					code, source = entry.ErrorCode, entry.ErrorSource
					if source == "" {
						source = record.ErrorSource
					}
				}
			}
			entry.ErrorCode, entry.ErrorSource = code, source
		}
		entry.Attempt = len(actual.items) + 1
		actual.items = append(actual.items, entry)
	}
	actual.consumed = len(snapshot.Attempts)
}

func (s *officialFailoverState) recordAttempts(record *RequestRecord, fallback []GatewayAttemptSummary) {
	if s.actualAttempts == nil {
		officialFailoverRecordAttempts(record, fallback)
		return
	}
	actual := s.actualAttempts
	record.Attempts = append([]GatewayAttemptSummary(nil), actual.items...)
	// This is the BPS actual-network-ledger contract, including the independent
	// observed HTTP status and selected generation failure provenance.
	record.ObservationSchemaVersion = 2
	record.AttemptsTruncated = nil
	if len(actual.items) > 0 || actual.truncated {
		truncated := actual.truncated
		record.AttemptsTruncated = &truncated
	}
}
