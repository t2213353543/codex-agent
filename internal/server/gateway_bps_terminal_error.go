package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"codex-agent/internal/bps"
	"codex-agent/internal/publicerrors"
)

// The log-safe terminal summary is not the client error payload. Buffered JSON
// clients receive the same sanitized error object as streaming clients; terminal
// failures without an error object keep the existing generic gateway failure.
func writeBPSTerminalJSONError(w http.ResponseWriter, err error) {
	var terminal *bps.TerminalError
	if errors.As(err, &terminal) {
		var event map[string]json.RawMessage
		if json.Unmarshal(terminal.Payload, &event) == nil {
			detail := event["error"]
			if !jsonObject(detail) {
				var response map[string]json.RawMessage
				if json.Unmarshal(event["response"], &response) == nil {
					detail = response["error"]
				}
			}
			// BPS can send a flattened route rejection before response.created.
			// Preserve its explicit code; do not infer quota from message text.
			if !jsonObject(detail) && terminal.Type == "error" && terminal.Code == "basispoints_route_rate_limit_exceeded" {
				selected := make(map[string]json.RawMessage)
				for _, key := range []string{"code", "message", "param"} {
					if value := event[key]; len(value) > 0 {
						selected[key] = value
					}
				}
				detail, _ = json.Marshal(selected)
			}
			if jsonObject(detail) {
				w.Header().Del("Content-Length")
				w.Header().Set("Content-Type", "application/json")
				status := http.StatusBadGateway
				if terminal.Code == "basispoints_route_rate_limit_exceeded" {
					status = http.StatusTooManyRequests
				}
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]json.RawMessage{"error": publicerrors.JSON(detail, true)})
				return
			}
		}
	}
	apiError(w, http.StatusBadGateway, err.Error())
}
