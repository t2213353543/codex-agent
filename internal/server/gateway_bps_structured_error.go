package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"codex-agent/internal/bps"
	"codex-agent/internal/publicerrors"
)

// A locally rejected completed turn must end the public stream explicitly.
// Retain only actual upstream identity/usage; never expose invalid output or
// pretend that the upstream generated this gateway validation error.
func bpsStructuredValidationFailure(result bps.StreamResult, err error) []byte {
	var upstream map[string]json.RawMessage
	_ = json.Unmarshal(result.Response, &upstream)
	message := publicerrors.Message(err.Error())
	code := "response_validation_failed"
	var checkpoint *gatewayRecoveryAuthorizationError
	if errors.As(err, &checkpoint) {
		// This check can fail when management changes an account during a
		// completed generation. Avoid credential-auth prose in a public stream:
		// downstream gateways may treat it as a revoked client API key.
		code = "response_access_changed"
		message = "gateway response completion is unavailable because its account access changed"
	}
	response := map[string]any{"status": "failed", "output": []any{}, "error": map[string]string{
		"type": "proxy_error", "code": code, "message": message,
	}}
	for _, field := range []string{"id", "model", "usage"} {
		if value, present := upstream[field]; present {
			response[field] = value
		}
	}
	wire, _ := json.Marshal(map[string]any{"type": "response.failed", "response": response})
	return wire
}

func writeBPSStructuredValidationJSON(w http.ResponseWriter, payload []byte) {
	writeBPSLocalFailureJSON(w, payload, http.StatusBadGateway)
}

func writeBPSLocalFailureJSON(w http.ResponseWriter, payload []byte, status int) {
	var event struct {
		Response struct{ Error json.RawMessage }
	}
	_ = json.Unmarshal(payload, &event)
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]json.RawMessage{"error": event.Response.Error})
}
