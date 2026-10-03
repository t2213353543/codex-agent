package server

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
)

func TestPGGatewayRefreshPreflightCancellationDoesNotPersistFailure(t *testing.T) {
	store := accountPGTestStore(t)
	account := pgImportForTest(t, store, "preflight-cancellation", "synthetic-preflight-access")
	before, err := store.getAccountByID(account.ID)
	if err != nil {
		t.Fatal("cannot snapshot the synthetic account before refresh")
	}
	auditCount := func() int64 {
		t.Helper()
		var count int64
		if err := store.DB.Pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_logs WHERE entity_id=$1`, account.ID).Scan(&count); err != nil {
			t.Fatal("cannot read the synthetic account audit count")
		}
		return count
	}
	beforeAudit := auditCount()
	reject := &gatewaySelectionPGRejectTransport{}
	p := &Proxy{Store: store, Client: &http.Client{Transport: reject}, TokenURL: "http://refresh-preflight.invalid/oauth/token"}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	callbacks := 0
	_, err = store.Refresh(account.ID, before.AccessToken, func(current Account) (Account, error) {
		callbacks++
		// Cancellation is observed by the real preflight guard while the
		// refresh lock is held, before any OAuth request can be dispatched.
		cancel()
		return p.refreshAccountWithCooldown(ctx, p.Client, current, defaultSettings())
	})
	if !errors.Is(err, context.Canceled) || callbacks != 1 || reject.calls.Load() != 0 {
		t.Fatalf("refresh preflight lost cancellation or sent OAuth: cancelled=%t callbacks=%d outbound=%d", errors.Is(err, context.Canceled), callbacks, reject.calls.Load())
	}
	after, readErr := (Store{DB: store.DB}).getAccountByID(account.ID)
	if readErr != nil {
		t.Fatal("cannot reopen the synthetic account after cancelled preflight")
	}
	if !reflect.DeepEqual(after, before) {
		t.Errorf("zero-dispatch cancellation changed persisted account state: status_changed=%t version_changed=%t checked_changed=%t error_changed=%t updated_changed=%t access_changed=%t refresh_changed=%t credential_version_changed=%t owner_changed=%t",
			after.CredentialStatus != before.CredentialStatus, after.Version != before.Version,
			after.LastCheckedAt != before.LastCheckedAt, after.LastError != before.LastError, after.UpdatedAt != before.UpdatedAt,
			after.AccessToken != before.AccessToken, after.RefreshToken != before.RefreshToken,
			after.CredentialVersion != before.CredentialVersion, after.OwnerRevision != before.OwnerRevision)
	}
	if afterAudit := auditCount(); afterAudit != beforeAudit {
		t.Errorf("zero-dispatch cancellation added an account audit: before=%d after=%d", beforeAudit, afterAudit)
	}
}
