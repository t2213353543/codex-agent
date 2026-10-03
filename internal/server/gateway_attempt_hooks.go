package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

func gatewayPayloadErrorTruncated(raw []byte) bool {
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
		Message string          `json:"message"`
		Code    string          `json:"code"`
		Detail  json.RawMessage `json:"detail"`
	}
	return json.Unmarshal(raw, &payload) != nil && len(strings.TrimSpace(string(raw))) > 200
}

func applyGatewayAttemptSnapshot(rec *RequestRecord, observer *gatewayAttemptObservation) {
	if observer == nil {
		return
	}
	s := observer.Snapshot()
	rec.ObservationSchemaVersion = 1
	rec.SentModel, rec.InboundPath, rec.UpstreamPath = s.SentModel, s.InboundPath, s.UpstreamPath
	rec.SentReasoningEffort = s.SentReasoningEffort
	if s.ReturnedModel != "" {
		rec.ReturnedModel = s.ReturnedModel
	}
	rec.RequestPreparationMs, rec.AccountSelectionMs, rec.ProxySelectionMs = s.RequestPreparationMs, s.AccountSelectionMs, s.ProxySelectionMs
	rec.Attempts, rec.AttemptsTruncated = s.Attempts, s.AttemptsTruncated
	if s.ErrorTruncated != nil {
		rec.ErrorTruncated = s.ErrorTruncated
	}
}

func finishGatewayObservedAttempt(handle *gatewayAttemptHandle, response *http.Response, err error) {
	if handle == nil {
		return
	}
	source, code := "", ""
	if response != nil {
		handle.Headers(response.StatusCode, time.Now())
	}
	switch {
	case errors.Is(err, context.Canceled):
		source, code = "client", "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		source, code = "transport", "deadline"
	case errors.Is(err, errGatewayProxyUnavailable):
		source, code = "gateway", "proxy_unavailable"
	case errors.Is(err, errGatewayNativeCapacity):
		source, code = "gateway", "capacity"
	case errors.Is(err, errGatewayCompressionCapability):
		source, code = "gateway", "protocol"
	case errors.Is(err, errGatewayNativeProtocol), errors.Is(err, errNativeWSProtocol):
		source, code = "transport", "protocol"
	case response != nil && response.StatusCode >= 300 && response.StatusCode != http.StatusNotModified:
		source, code = "upstream", "http_status"
	case err != nil:
		var oauth *gatewayOAuthRefreshError
		if errors.As(err, &oauth) && oauth.Kind == "invalid" {
			source, code = "upstream", "protocol"
			break
		}
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			source, code = "transport", "timeout"
		} else {
			source, code = "transport", "transport"
		}
	}
	handle.Finish(source, code, time.Now())
}

// Keep the existing 64KiB diagnostic prefix and byte forwarding unchanged;
// report truncation only when bytes beyond the prefix were actually observed.
type gatewayObservedErrorReader struct {
	io.Reader
	read int64
}

func (r *gatewayObservedErrorReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read += int64(n)
	return n, err
}

const gatewayErrorBodyLimit = 2 << 20

var errGatewayErrorBodyLimit = errors.New("upstream error body exceeds forwarding limit")

// Forward at most the existing 2MiB cap, observing one extra byte to distinguish
// a complete body at the limit from truncation. Return transfer failures so the
// caller cannot finish a truncated upstream error as a clean downstream body.
// The existing stream/compact/client read budgets still govern blocked reads.
func copyGatewayErrorBody(w io.Writer, body io.Reader, observer *gatewayAttemptObservation) (prefix []byte, resultErr error) {
	r := &gatewayObservedErrorReader{Reader: body}
	defer func() { observer.ObserveErrorTruncation(r.read > 64<<10 || gatewayPayloadErrorTruncated(prefix)) }()
	buf := make([]byte, 32<<10)
	remaining, emptyReads := gatewayErrorBodyLimit, 0
	for {
		n, readErr := r.Read(buf[:min(len(buf), remaining+1)])
		if n > 0 {
			emptyReads = 0
			forward := min(n, remaining)
			if len(prefix) < 64<<10 {
				prefix = append(prefix, buf[:min(forward, 64<<10-len(prefix))]...)
			}
			if forward > 0 {
				written, writeErr := w.Write(buf[:forward])
				if writeErr != nil {
					return prefix, writeErr
				}
				if written != forward {
					return prefix, io.ErrShortWrite
				}
			}
			remaining -= forward
			if n > forward {
				return prefix, errGatewayErrorBodyLimit
			}
		} else if readErr == nil {
			emptyReads++
			if emptyReads >= 100 {
				return prefix, io.ErrNoProgress
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return prefix, nil
			}
			return prefix, readErr
		}
	}
}
