package server

import (
	"errors"
	"io"
	"net/http"
)

// gatewayResponseWriter records the status actually sent to the caller. A
// successful upstream may still become a local 502 while collecting JSON, so
// the upstream status alone is not the response's status.
type gatewayResponseWriter struct {
	http.ResponseWriter
	record      *RequestRecord
	wroteHeader bool
}

func (w *gatewayResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *gatewayResponseWriter) WriteHeader(status int) {
	// 101 completes the HTTP exchange; other 1xx responses are informational.
	// Let the underlying writer reject invalid statuses before recording one.
	w.ResponseWriter.WriteHeader(status)
	if !w.wroteHeader && (status >= 200 || status == http.StatusSwitchingProtocols) {
		w.wroteHeader = true
		if w.record != nil {
			w.record.HTTPCode = status
		}
	}
}

func (w *gatewayResponseWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(body)
	if n != len(body) && err == nil {
		err = io.ErrShortWrite
	}
	w.observeWriteError(err)
	return n, err
}

func (w *gatewayResponseWriter) FlushError() error {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if !errors.Is(err, http.ErrNotSupported) {
		w.observeWriteError(err)
	}
	return err
}

func (w *gatewayResponseWriter) observeWriteError(err error) {
	if err == nil || w.record == nil {
		return
	}
	if observeGatewaySessionCommitError(w.record, err) {
		return
	}
	if w.record.Status != "error" {
		w.record.Status = "aborted"
	}
	if w.record.Error == "" {
		w.record.Error = "client connection lost"
	}
	observeGatewayError(w.record, "client", "response_write")
}
