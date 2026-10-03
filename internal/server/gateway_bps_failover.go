package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
)

// The production account-pair probe observed this exact envelope on one
// account, while the same body succeeded on the other with the builtin prompt
// both enabled and disabled. Do not generalize it to arbitrary 403/policy
// responses: explicit policy codes, parameters and other message types stay
// terminal. Existing replay/owner guards still run before choosing a new owner.
type bpsAccountRejectedError struct{}

func (*bpsAccountRejectedError) Error() string {
	return "upstream account rejected request before output"
}
func (*bpsAccountRejectedError) Unwrap() error {
	return &officialProtocolError{message: "upstream account rejected request before output", status: http.StatusForbidden, code: "upstream_account_rejected", source: "upstream"}
}

func bpsObservedAccountRejection(status int, body []byte) bool {
	if status != http.StatusForbidden {
		return false
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil || envelope == nil {
		return false
	}
	var detail map[string]json.RawMessage
	if json.Unmarshal(envelope["error"], &detail) != nil || detail == nil {
		return false
	}
	var kind, message string
	if json.Unmarshal(detail["type"], &kind) != nil || kind != "server_error" || json.Unmarshal(detail["message"], &message) != nil || message != "403: This request was blocked by our usage policy." {
		return false
	}
	for _, field := range []string{"code", "param"} {
		raw, exists := detail[field]
		if !exists {
			return false
		}
		value := bytes.TrimSpace(raw)
		if !bytes.Equal(value, []byte("null")) && !bytes.Equal(value, []byte(`""`)) {
			return false
		}
	}
	return true
}

func observeBPSAccountRejection(ctx context.Context, status int, body []byte) {
	if officialFailoverFrom(ctx) != nil && bpsObservedAccountRejection(status, body) {
		officialObserveAttemptFailure(ctx, &bpsAccountRejectedError{})
	}
}
