package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Only structured official errors carry an HTTP classification. Free-form text
// never selects a status, even when it contains words such as "schema"/"error".
type officialProtocolError struct {
	message string
	status  int
	code    string
	source  string
	cause   error
}

func (e *officialProtocolError) Error() string { return e.message }
func (e *officialProtocolError) Unwrap() error { return e.cause }

func officialRPCFailure(method string, raw json.RawMessage) error {
	var value struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return errors.New("App Server rejected " + method)
	}
	status := http.StatusBadGateway
	if value.Code == -32600 || value.Code == -32602 {
		status = http.StatusBadRequest
	}
	message := fmt.Sprintf("App Server rejected %s (rpc_code=%d)", method, value.Code)
	if text := boundedGatewayMetadata(value.Message, 2048); text != "" {
		message += ": " + text
	}
	return &officialProtocolError{message: message, status: status, code: fmt.Sprintf("rpc_%d", value.Code), source: "upstream"}
}

func officialFailureHTTP(err error, fallback int) (int, string, string) {
	if errors.Is(err, errOfficialModelUnavailable) {
		return http.StatusBadRequest, "model_not_available", "gateway"
	}
	var protocol *officialProtocolError
	if errors.As(err, &protocol) {
		return protocol.status, protocol.code, protocol.source
	}
	return fallback, "app_server_error", "upstream"
}

func recordOfficialFailure(rec *RequestRecord, requestContext context.Context, err error, phase string, fallback int) int {
	officialObserveAttemptFailure(requestContext, err)
	status, _, source := officialFailureHTTP(err, fallback)
	if rec == nil {
		return status
	}
	rec.Status = "error"
	rec.Error = boundedGatewayMetadata(err.Error(), 4096)
	rec.ErrorSource = source
	rec.ErrorPhase = phase
	if errors.Is(err, context.Canceled) && errors.Is(requestContext.Err(), context.Canceled) {
		rec.Status = "aborted"
		rec.Error = "client request canceled"
		rec.ErrorSource = "client"
		rec.ErrorPhase = "request_canceled"
		rec.TerminalStatus = "cancelled"
		if rec.HTTPCode == 0 {
			rec.HTTPCode = 499
		}
		return 499
	}
	if errors.Is(err, context.DeadlineExceeded) && errors.Is(requestContext.Err(), context.DeadlineExceeded) {
		rec.Status = "aborted"
		rec.Error = "client request deadline exceeded"
		rec.ErrorSource = "client"
		rec.ErrorPhase = "request_deadline"
		rec.TerminalStatus = "cancelled"
		if rec.HTTPCode == 0 {
			rec.HTTPCode = 499
		}
		return 499
	}
	if errors.Is(err, context.Canceled) {
		rec.ErrorSource = "gateway"
		rec.ErrorPhase = "runtime_canceled"
		status = http.StatusServiceUnavailable
	}
	if rec.HTTPCode == 0 {
		rec.HTTPCode = status
	}
	return status
}

// Captures only a small complete JSON error body after a non-success HTTP
// status. It never reads ordinary model output or stores request content.
type officialErrorResponseWriter struct {
	*gatewayResponseWriter
	body     bytes.Buffer
	status   int
	tooLarge bool
	phase    string
}

func (w *officialErrorResponseWriter) WriteHeader(status int) {
	if w.status == 0 && status >= 200 {
		w.status = status
	}
	w.gatewayResponseWriter.WriteHeader(status)
}
func (w *officialErrorResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.status >= 400 && strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") && !w.tooLarge {
		if w.body.Len()+len(body) <= 16<<10 {
			_, _ = w.body.Write(body)
		} else {
			w.body.Reset()
			w.tooLarge = true
		}
	}
	return w.gatewayResponseWriter.Write(body)
}
func (w *officialErrorResponseWriter) observe() {
	if w.status < 400 || w.record == nil {
		return
	}
	if w.record.Status != "aborted" {
		w.record.Status = "error"
	}
	if w.record.Error == "" && !w.tooLarge {
		var value struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(w.body.Bytes(), &value) == nil {
			w.record.Error = boundedGatewayMetadata(value.Error.Message, 4096)
		}
	}
	if w.record.Error == "" {
		w.record.Error = fmt.Sprintf("official gateway returned HTTP %d", w.status)
	}
	if w.record.ErrorSource == "" {
		w.record.ErrorSource = "gateway"
		w.record.ErrorPhase = w.phase
	}
}
