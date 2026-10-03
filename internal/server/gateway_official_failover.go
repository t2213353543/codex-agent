package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type officialFailoverKey struct{}

type officialFailoverState struct {
	mu                    sync.Mutex
	started               time.Time
	record                *RequestRecord
	key, session, owner   string
	account               Account
	failure               error
	retrying, committed   bool
	replayBlocked         bool
	localSelectionFailure bool
	bpsModelRoutesSet     bool
	bpsModelRoutes        string
	commit                func() error
	requestBody           []byte
	connectionResult      *appServerConnectionResult
	actualAttempts        *officialFailoverActualAttempts
	boundSelection        *GatewayBoundSelectionObservation
	boundReplayClass      string
}

func officialFailoverFrom(ctx context.Context) *officialFailoverState {
	state, _ := ctx.Value(officialFailoverKey{}).(*officialFailoverState)
	return state
}

func officialObserveAttemptFailure(ctx context.Context, err error) {
	if state := officialFailoverFrom(ctx); state != nil {
		state.mu.Lock()
		state.failure = err
		state.mu.Unlock()
	}
}

func officialFailoverEligible(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, errOfficialModelUnavailable) {
		return true
	}
	var unavailable *gatewayBoundAccountUnavailableError
	if errors.As(err, &unavailable) {
		return true
	}
	var bpsRejected *bpsAccountRejectedError
	if errors.As(err, &bpsRejected) {
		return true
	}
	var protocol *officialProtocolError
	return errors.As(err, &protocol) && (protocol.code == "authentication_unavailable" || protocol.source == "upstream" &&
		(protocol.status == http.StatusUnauthorized || protocol.status == http.StatusPaymentRequired || protocol.status == http.StatusTooManyRequests &&
			(protocol.code == "usageLimitExceeded" || protocol.code == "rateLimitExceeded")))
}

func (s *officialFailoverState) commitOwner() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.committed {
		return nil
	}
	if s.commit != nil {
		if err := s.commit(); err != nil {
			return &gatewaySessionCommitError{cause: err}
		}
	}
	s.committed = true
	return nil
}

// Keep the failed owner's binding while a replacement has not yet produced a
// usable response. First real output (or a successful non-streaming result)
// migrates that exact binding with CAS before delivering anything to the client.
func (p *Proxy) officialFailoverLease(ctx context.Context, key, session string, account Account) (context.Context, func()) {
	s := officialFailoverFrom(ctx)
	if s == nil || !s.retrying || session == "" {
		return p.keepGatewaySession(ctx, key, session, account)
	}
	expected := s.owner
	replacement := gatewayAccountRef(account)
	s.commit = func() error {
		actual, changed, err := p.switchSessionAccount(ctx, key, session, expected, replacement)
		if err != nil {
			return err
		}
		if !changed || actual != replacement {
			return errors.New("session account changed during authentication failover")
		}
		s.owner = replacement
		return nil
	}
	lease, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-lease.Done():
				return
			case <-ticker.C:
				s.mu.Lock()
				owner := s.owner
				s.mu.Unlock()
				current, err := p.touchGatewaySession(lease, key, session, owner)
				// A concurrent request may have committed this exact replacement.
				// The CAS and its in-flight lease must agree on that same owner.
				if err != nil || current != owner && current != replacement {
					cancel()
					return
				}
			}
		}
	}()
	return lease, func() { cancel(); <-done }
}

// Buffers only pre-output protocol events and unsuccessful JSON. Real output
// immediately commits and streams; an already-visible tool/text event is never
// replayed on another account. The body is never interpreted or rewritten.
type officialFailoverWriter struct {
	target    http.ResponseWriter
	header    http.Header
	status    int
	body      bytes.Buffer
	state     *officialFailoverState
	delivered bool
	commitErr error
}

func (w *officialFailoverWriter) Header() http.Header         { return w.header }
func (w *officialFailoverWriter) Unwrap() http.ResponseWriter { return w.target }
func (w *officialFailoverWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *officialFailoverWriter) Write(raw []byte) (int, error) {
	if w.commitErr != nil {
		return 0, w.commitErr
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.delivered {
		return w.target.Write(raw)
	}
	_, _ = w.body.Write(raw)
	if strings.HasPrefix(w.header.Get("Content-Type"), "text/event-stream") && officialFailoverHasOutput(w.body.Bytes()) {
		if err := w.state.commitOwner(); err != nil {
			w.commitErr = err
			w.body.Reset()
			return 0, err
		}
		if err := w.deliver(); err != nil {
			return 0, err
		}
	}
	return len(raw), nil
}
func (w *officialFailoverWriter) Flush() { _ = w.FlushError() }
func (w *officialFailoverWriter) FlushError() error {
	if !w.delivered {
		return nil
	}
	return http.NewResponseController(w.target).Flush()
}
func (w *officialFailoverWriter) deliver() error {
	if w.delivered {
		return nil
	}
	if w.commitErr != nil {
		w.header = make(http.Header)
		w.header.Set("Content-Type", "application/json")
		w.status = http.StatusConflict
		w.body.Reset()
		w.body.WriteString(`{"error":{"code":"session_binding_conflict","message":"session account changed during authentication failover"}}`)
	}
	for key, values := range w.header {
		w.target.Header()[key] = append([]string(nil), values...)
	}
	if w.status != 0 {
		w.target.WriteHeader(w.status)
	}
	w.delivered = true
	size := w.body.Len()
	n, err := w.target.Write(w.body.Bytes())
	w.body.Reset()
	if err == nil && n != size {
		err = io.ErrShortWrite
	}
	return err
}

// Final account authentication belongs to the gateway's upstream capacity,
// not to the already-authenticated client's API key. Only an uncommitted HTTP
// account-auth failure with typed upstream provenance may change public status.
// Keep the original error and actual attempts for operational diagnostics.
func (w *officialFailoverWriter) projectUpstreamAuthenticationFailure(record *RequestRecord, failure error) {
	if w == nil || w.delivered || w.commitErr != nil || !gatewayUpstreamAccountAuthenticationFailure(record, failure) || w.status != record.HTTPCode {
		return
	}
	w.header.Del("WWW-Authenticate")
	w.header.Del("Content-Length")
	w.header.Del("Content-Encoding")
	w.header.Set("Content-Type", "application/json")
	w.status = http.StatusServiceUnavailable
	w.body.Reset()
	w.body.WriteString(gatewayUpstreamAuthenticationUnavailableBody)
	record.HTTPCode = http.StatusServiceUnavailable
}

const gatewayUpstreamAuthenticationUnavailableBody = `{"error":{"type":"server_error","code":"upstream_authentication_unavailable","message":"upstream account authentication unavailable","param":null}}`

func gatewayUpstreamAccountAuthenticationFailure(record *RequestRecord, failure error) bool {
	if record == nil || record.Status != "error" || record.ErrorSource != "upstream" ||
		record.KeyID == "" || record.AccountID == "" || errors.Is(failure, context.Canceled) || errors.Is(failure, context.DeadlineExceeded) {
		return false
	}
	var protocol *officialProtocolError
	if !errors.As(failure, &protocol) || protocol.source != "upstream" || protocol.status != http.StatusUnauthorized {
		return false
	}
	if record.HTTPCode == http.StatusUnauthorized {
		return true
	}
	// A real HTTP200 response can end with a typed account-auth failure.
	// Buffered JSON translates that same native terminal to502. Neither is
	// the authenticated client's API-key failure. Other200/502 errors are not
	// eligible, even if free text happens to mention authentication.
	return (record.HTTPCode == http.StatusOK || record.HTTPCode == http.StatusBadGateway) &&
		record.TerminalStatus == "failed" && record.ErrorPhase == "response_event"
}

func officialFailoverHasOutput(raw []byte) bool {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		record := &RequestRecord{}
		(gatewayObservation{record: record, started: time.Now()}).event(bytes.TrimPrefix(line, []byte("data: ")))
		if record.TTFTMs != nil {
			return true
		}
	}
	return false
}

func officialFailoverRecordAttempts(record *RequestRecord, attempts []GatewayAttemptSummary) {
	if len(attempts) > gatewayAttemptObservationLimit {
		record.Attempts = append([]GatewayAttemptSummary(nil), attempts[:gatewayAttemptObservationLimit]...)
		truncated := true
		record.AttemptsTruncated = &truncated
	} else {
		record.Attempts = append([]GatewayAttemptSummary(nil), attempts...)
	}
}

// The inner handler publishes its record from a defer, including on abort.
// Finish that observation without handling the original panic or writing to the
// response. A secondary accounting failure must not replace the original exit.
func (p *Proxy) finishAbortedAccountFailover(state *officialFailoverState, attempts []GatewayAttemptSummary, attemptStarted time.Time, preceding time.Duration, endpoint string) {
	defer func() { _ = recover() }()
	record := state.record
	if record == nil {
		return
	}
	if record.Status == "" || record.Status == "ok" {
		record.Status = "aborted"
	}
	if record.Error == "" {
		record.Error = "gateway request ended before handler completion"
	}
	if record.ErrorSource == "" {
		record.ErrorSource = "gateway"
	}
	if record.ErrorPhase == "" {
		record.ErrorPhase = "request_completion"
	}
	if !state.localSelectionFailure {
		elapsed := time.Since(attemptStarted).Milliseconds()
		status, code, source := officialFailureHTTP(state.failure, record.HTTPCode)
		if state.failure == nil {
			code, source = "", record.ErrorSource
		}
		var observedStatus *int
		if status > 0 {
			observedStatus = &status
		}
		attempts = append(attempts, GatewayAttemptSummary{Attempt: len(attempts) + 1, Phase: "request", Endpoint: endpoint, StatusCode: observedStatus, DurationMs: &elapsed, ErrorSource: source, ErrorCode: code})
	}
	if record.TTFTMs != nil {
		adjusted := *record.TTFTMs + preceding.Milliseconds()
		record.TTFTMs = &adjusted
	}
	record.Time = state.started.Unix()
	state.recordAttempts(record, attempts)
	// An abnormal exit has no trustworthy final recovery classification.
	state.attachBoundSelection(record, "")
	p.finishRecord(record, state.started, state.key)
}

func (p *Proxy) gatewayOfficial(w http.ResponseWriter, r *http.Request, mode string) {
	p.gatewayWithAccountFailover(w, r, func(w http.ResponseWriter, r *http.Request) { p.gatewayOfficialOnce(w, r, mode) })
}

// gatewayWithAccountFailover shares admission, exclusion and first-output CAS
// between the hosted runtime and the lightweight client-owned Responses path.
// The callback must publish its record/account/failure and use the guarded lease.
func (p *Proxy) gatewayWithAccountFailover(w http.ResponseWriter, r *http.Request, once func(http.ResponseWriter, *http.Request)) {
	path := strings.TrimPrefix(r.URL.Path, "/v1")
	if (path != "/responses" && path != "/responses/compact" && !appServerAuxiliaryPath(path) && !appServerRealtimePath(r.URL.Path)) || r.Method != http.MethodPost || strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		once(w, r)
		return
	}
	state := &officialFailoverState{started: time.Now()}
	ctx := context.WithValue(withGatewayAccountFailover(r.Context()), officialFailoverKey{}, state)
	if p.AppServerCLI != nil && requestInferenceMode(ctx) == inferenceCodexCLI {
		ctx = withAppServerRequestAllocation(ctx)
	}
	var attempts []GatewayAttemptSummary
	var preceding time.Duration
	var priorWriter *officialFailoverWriter
	var priorRecord *RequestRecord
	var priorFailure error
	for {
		state.record = nil
		state.account = Account{}
		state.failure = nil
		state.localSelectionFailure = false
		state.committed = false
		state.commit = nil
		state.connectionResult = nil
		attemptStarted := time.Now()
		writer := &officialFailoverWriter{target: w, header: make(http.Header), state: state}
		request := r.Clone(ctx)
		if state.retrying {
			request.Body = io.NopCloser(bytes.NewReader(state.requestBody))
			request.ContentLength = int64(len(state.requestBody))
			request.Header.Del("Content-Encoding")
		}
		func() {
			returned := false
			defer func() {
				if !returned && state.connectionResult != nil {
					state.connectionResult.Confirm(false)
				}
				if !returned {
					p.finishAbortedAccountFailover(state, attempts, attemptStarted, preceding, gatewayObservedRoute(r.URL.Path))
				}
			}()
			once(writer, request)
			returned = true
		}()
		record := state.record
		if record == nil {
			if state.connectionResult != nil {
				state.connectionResult.Confirm(false)
			}
			_ = writer.deliver()
			return
		}
		// Exhaustion adds no upstream attempt. Preserve the last real account's
		// structured failure instead of replacing it with a generic no-candidate
		// selection error (e.g. a genuinely unavailable model remains HTTP 400).
		if state.retrying && state.account.ID == "" && errors.Is(state.failure, errGatewayAccountCandidatesExhausted) && priorRecord != nil && ctx.Err() == nil {
			if state.connectionResult != nil {
				state.connectionResult.Confirm(false)
			}
			record, writer = priorRecord, priorWriter
			writer.projectUpstreamAuthenticationFailure(record, priorFailure)
			record.Time = state.started.Unix()
			state.recordAttempts(record, attempts)
			if err := writer.deliver(); err != nil {
				record.Status = "aborted"
				record.Error = "client response delivery failed"
			}
			state.finishBoundSelection(ctx, record, true)
			p.finishRecord(record, state.started, state.key)
			return
		}
		elapsed := time.Since(attemptStarted).Milliseconds()
		status, code, source := officialFailureHTTP(state.failure, record.HTTPCode)
		if state.failure == nil {
			code = ""
			source = record.ErrorSource
		}
		if !state.localSelectionFailure {
			attempts = append(attempts, GatewayAttemptSummary{Attempt: len(attempts) + 1, Phase: "request", Endpoint: gatewayObservedRoute(r.URL.Path), StatusCode: &status, DurationMs: &elapsed, ErrorSource: source, ErrorCode: code})
		}
		if !writer.delivered && !state.replayBlocked && appServerDelegatedOwner(ctx) == "" && ctx.Err() == nil && (state.account.ID != "" || state.owner != "") && officialFailoverEligible(state.failure) {
			if state.connectionResult != nil {
				state.connectionResult.Confirm(false)
			}
			if state.account.ID != "" {
				gatewayExcludeFailedAccount(ctx, state.account)
			}
			if !state.localSelectionFailure || priorRecord == nil {
				priorWriter, priorRecord, priorFailure = writer, record, state.failure
			}
			state.retrying = true
			preceding += time.Since(attemptStarted)
			continue
		}
		if record.TTFTMs != nil {
			adjusted := *record.TTFTMs + preceding.Milliseconds()
			record.TTFTMs = &adjusted
		}
		record.Time = state.started.Unix()
		state.recordAttempts(record, attempts)
		// An empty successful generation can terminate without a TTFT event.
		// Commit its owner before exposing that successful terminal response too.
		if !writer.delivered && state.failure == nil && record.Status == "ok" {
			if err := state.commitOwner(); err != nil {
				writer.commitErr = err
			}
		}
		if writer.commitErr != nil {
			record.Status = "error"
			record.HTTPCode = 409
			record.Error = "session account changed during authentication failover"
			record.ErrorSource = "gateway"
			record.ErrorPhase = "session_binding"
		}
		writer.projectUpstreamAuthenticationFailure(record, state.failure)
		if err := writer.deliver(); err != nil {
			record.Status = "aborted"
			record.Error = "client response delivery failed"
		}
		if state.connectionResult != nil {
			state.connectionResult.Confirm(record.Status == "ok" && state.failure == nil && state.committed && writer.commitErr == nil && ctx.Err() == nil)
		}
		state.finishBoundSelection(ctx, record, false)
		p.finishRecord(record, state.started, state.key)
		return
	}
}
