package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type gatewayRefreshContextPGOutcome struct {
	store              Store
	before, returned   Account
	err                error
	failedAuditsBefore int64
	observed           GatewayAttemptSnapshot
}

func gatewayRefreshContextPGCommittedExchange(t *testing.T, consumer string, status int) gatewayRefreshContextPGOutcome {
	t.Helper()
	store := accountPGTestStore(t)
	raw := gatewayRevisionCredentials(t, "committed-refresh-user", "committed-before")
	if consumer == "ensure_ready" {
		var document map[string]json.RawMessage
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatal("cannot prepare expired synthetic credentials")
		}
		document["expires_at"], _ = json.Marshal(time.Now().Add(-time.Minute).Unix())
		var err error
		raw, err = json.Marshal(document)
		if err != nil {
			t.Fatal("cannot encode expired synthetic credentials")
		}
	}
	if err := store.ImportWith("committed-refresh", raw, AccountDefaults{Concurrency: 2}); err != nil {
		t.Fatal("cannot import isolated known-owner account")
	}
	before, err := store.Get("committed-refresh")
	if err != nil || !gatewayCompleteOwner(before) || before.OwnerRevision == "" {
		t.Fatal("cannot read isolated account ownership")
	}
	if consumer == "ensure_ready" && (before.ExpiresAt == 0 || before.ExpiresAt >= time.Now().Unix()) {
		t.Fatal("expiry refresh requires a persisted expired token")
	}
	var failedBefore int64
	if err := store.DB.Pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_logs WHERE entity_id=$1 AND action='account.credentials.refresh' AND result='failed'`, before.ID).Scan(&failedBefore); err != nil {
		t.Fatal("cannot read initial synthetic refresh audit count")
	}
	entered, allowResponse := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	allow := func() { releaseOnce.Do(func() { close(allowResponse) }) }
	var requests, serverCancellation atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" || requests.Add(1) != 1 {
			t.Error("committed refresh sent an unexpected or repeated request")
			http.Error(w, "synthetic unexpected request", http.StatusBadRequest)
			return
		}
		var payload struct {
			GrantType    string `json:"grant_type"`
			RefreshToken string `json:"refresh_token"`
			ClientID     string `json:"client_id"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&payload) != nil || payload.GrantType != "refresh_token" ||
			payload.RefreshToken != before.RefreshToken || payload.ClientID != before.ClientID {
			t.Error("committed refresh changed the original synthetic OAuth request")
			http.Error(w, "synthetic invalid refresh", http.StatusBadRequest)
			return
		}
		enterOnce.Do(func() { close(entered) })
		select {
		case <-allowResponse:
		case <-r.Context().Done():
			serverCancellation.Add(1)
			return
		}
		if r.Context().Err() != nil {
			serverCancellation.Add(1)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"synthetic-committed-access","refresh_token":"synthetic-committed-refresh","expires_in":3600}`, before.AccountID, before.Identity.UserID))
		} else {
			_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"synthetic committed rejection"}`)
		}
	}))
	t.Cleanup(server.Close)
	p := &Proxy{Store: store, Client: server.Client(), TokenURL: server.URL + "/oauth/token"}
	p.Client.Timeout = 5 * time.Second
	t.Cleanup(p.closeGatewayClients)
	caller, cancelCaller := context.WithCancel(t.Context())
	ctx, observation := withGatewayAttemptObservation(caller)
	type result struct {
		account Account
		err     error
	}
	done := make(chan result, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		var account Account
		var err error
		if consumer == "ensure_ready" {
			account, err = p.ensureReady(ctx, before)
		} else {
			account, err = p.recoverUnauthorized(ctx, before, p.Client)
		}
		done <- result{account: account, err: err}
	}()
	t.Cleanup(func() {
		cancelCaller()
		allow()
		select {
		case <-finished:
		case <-time.After(8 * time.Second):
			t.Error("committed refresh did not stop after releasing the fixture response")
		}
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not reach the real local OAuth handler")
	}
	// The server has consumed the OAuth request. Cancellation now must neither
	// discard returned rotation nor masquerade as a pre-exchange cancellation.
	cancelCaller()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("fixture caller did not become cancelled before OAuth completion")
	}
	allow()
	var outcome result
	select {
	case outcome = <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("committed refresh did not complete after the local OAuth response")
	}
	if requests.Load() != 1 || serverCancellation.Load() != 0 {
		t.Fatal("caller cancellation cancelled or repeated the committed OAuth exchange")
	}
	if p.inFlight(gatewayAdmissionKey(before)) != 0 {
		t.Fatal("refresh helper unexpectedly changed gateway admission")
	}
	if status == http.StatusOK && !errors.Is(p.authorizeGatewayRetry(ctx, "synthetic-unused-key", "synthetic-unused-id", outcome.account), context.Canceled) {
		t.Fatal("successful committed rotation allowed the cancelled caller to retry")
	}
	return gatewayRefreshContextPGOutcome{store: store, before: before, returned: outcome.account, err: outcome.err,
		failedAuditsBefore: failedBefore, observed: observation.Snapshot()}
}

func gatewayRefreshContextPGAssertObserved(t *testing.T, outcome gatewayRefreshContextPGOutcome, status int) {
	t.Helper()
	if len(outcome.observed.Attempts) != 1 || outcome.observed.AttemptsTruncated == nil || *outcome.observed.AttemptsTruncated {
		t.Fatal("committed exchange invented or omitted an OAuth attempt")
	}
	attempt := outcome.observed.Attempts[0]
	if attempt.Phase != "refresh" || attempt.Endpoint != "/oauth/token" || attempt.StatusCode == nil || *attempt.StatusCode != status || attempt.DurationMs == nil {
		t.Fatal("committed exchange observation does not match the actual OAuth response")
	}
}

func TestPGGatewayRefreshContextCommittedExchangeSurvivesCallerCancellation(t *testing.T) {
	for _, consumer := range []string{"ensure_ready", "unauthorized_recovery"} {
		t.Run(consumer, func(t *testing.T) {
			outcome := gatewayRefreshContextPGCommittedExchange(t, consumer, http.StatusOK)
			if outcome.err != nil {
				t.Fatal("caller cancellation discarded a successfully committed OAuth exchange")
			}
			current, err := outcome.store.getAccountByID(outcome.before.ID)
			if err != nil || current.AccessToken != "synthetic-committed-access" || current.RefreshToken != "synthetic-committed-refresh" ||
				current.CredentialVersion != outcome.before.CredentialVersion+1 || current.ExpiresAt <= time.Now().Unix() ||
				current.OwnerRevision != outcome.before.OwnerRevision || gatewayAccountRef(current) != gatewayAccountRef(outcome.before) ||
				current.LastError != outcome.before.LastError || current.CredentialStatus == "refresh_failed" {
				t.Fatal("committed token rotation was not durably saved with unchanged ownership")
			}
			if outcome.returned.AccessToken != current.AccessToken || outcome.returned.RefreshToken != current.RefreshToken || outcome.returned.OwnerRevision != current.OwnerRevision {
				t.Fatal("refresh returned credentials different from the committed account")
			}
			var failedAfter int64
			if err := outcome.store.DB.Pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_logs WHERE entity_id=$1 AND action='account.credentials.refresh' AND result='failed'`, current.ID).Scan(&failedAfter); err != nil || failedAfter != outcome.failedAuditsBefore {
				t.Fatal("successful committed refresh created a failure audit after caller cancellation")
			}
			gatewayRefreshContextPGAssertObserved(t, outcome, http.StatusOK)
		})
	}
}

func TestPGGatewayRefreshContextCommittedFailureAfterCancellationIsRecorded(t *testing.T) {
	for _, consumer := range []string{"ensure_ready", "unauthorized_recovery"} {
		t.Run(consumer, func(t *testing.T) {
			outcome := gatewayRefreshContextPGCommittedExchange(t, consumer, http.StatusUnauthorized)
			var oauth *gatewayOAuthRefreshError
			if !errors.As(outcome.err, &oauth) || oauth.Kind != "http" || oauth.StatusCode != http.StatusUnauthorized ||
				errors.Is(outcome.err, errAccountRefreshNotStarted) || errors.Is(outcome.err, context.Canceled) {
				t.Fatal("real OAuth rejection was incorrectly suppressed as pre-exchange cancellation")
			}
			current, err := outcome.store.getAccountByID(outcome.before.ID)
			if err != nil || current.AccessToken != outcome.before.AccessToken || current.RefreshToken != outcome.before.RefreshToken ||
				current.CredentialVersion != outcome.before.CredentialVersion || current.OwnerRevision != outcome.before.OwnerRevision ||
				current.CredentialStatus != "refresh_failed" || current.LastError != "credential refresh failed" || current.LastCheckedAt == 0 {
				t.Fatal("real committed OAuth rejection did not preserve credentials and record failure state")
			}
			var failedAfter int64
			if err := outcome.store.DB.Pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_logs WHERE entity_id=$1 AND action='account.credentials.refresh' AND result='failed'`, current.ID).Scan(&failedAfter); err != nil || failedAfter != outcome.failedAuditsBefore+1 {
				t.Fatal("real OAuth rejection did not create exactly one failure audit")
			}
			gatewayRefreshContextPGAssertObserved(t, outcome, http.StatusUnauthorized)
		})
	}
}
