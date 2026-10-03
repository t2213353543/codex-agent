package server

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestGatewayBoundCancellationRefreshPreservesCallerAndAccountState(t *testing.T) {
	p := newTestProxy(t, 1)
	const session = "bound-refresh-cancellation"
	original, release, err := p.pickSessionAccount(t.Context(), "test-client-key", "admin", session)
	if err != nil || release == nil {
		t.Fatal("cannot establish the existing bound session")
	}
	release()
	if err := p.Store.UpdateAccount(original.ID, func(account *Account) error {
		account.ExpiresAt = time.Now().Add(-time.Minute).Unix()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := p.Store.getAccountByID(original.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var refreshes atomic.Int32
	p.Client = &http.Client{Transport: sourceRefreshRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Body != nil {
			_ = request.Body.Close()
		}
		refreshes.Add(1)
		cancel()
		return nil, context.Canceled
	})}
	p.TokenURL = "http://bound-cancellation.invalid/oauth/token"
	selected, release, err := p.pickSessionAccount(ctx, "test-client-key", "admin", session)
	if release != nil {
		release()
	}
	if !errors.Is(err, context.Canceled) || selected.ID != "" || release != nil || refreshes.Load() != 1 {
		t.Errorf("bound refresh cancellation changed caller result: cancelled=%t selected=%t admitted=%t refreshes=%d", errors.Is(err, context.Canceled), selected.ID != "", release != nil, refreshes.Load())
	}
	current, readErr := p.Store.getAccountByID(original.ID)
	if readErr != nil || current.LastError != before.LastError || current.CredentialStatus != before.CredentialStatus ||
		current.AccessToken != before.AccessToken || current.RefreshToken != before.RefreshToken || current.OwnerRevision != before.OwnerRevision {
		t.Error("caller cancellation was recorded as an account failure or changed credentials")
	}
	if p.inFlight(gatewayAdmissionKey(original)) != 0 {
		t.Error("cancelled bound refresh retained an admission slot")
	}
	if bound, err := p.sessionAccount(t.Context(), "admin", session, ""); err != nil || bound != gatewayAccountRef(original) {
		t.Error("cancelled bound refresh migrated or removed the existing history binding")
	}
}
