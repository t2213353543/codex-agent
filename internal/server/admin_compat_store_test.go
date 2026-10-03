package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func compatStoreFixture(t *testing.T, p *Proxy, name string) adminCompatAccount {
	t.Helper()
	row, err := p.compatMutateAccount(context.Background(), "", 0, func(a *Account, r *adminCompatRecord) error {
		a.Name = name
		a.AccessToken = "synthetic-compat-" + name
		a.RefreshToken = "synthetic-refresh-" + name
		r.FileName = name + ".json"
		r.CPA["id_token"] = json.RawMessage(`"synthetic-private-id-token"`)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func exerciseCompatStore(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	ctx := context.Background()
	a := compatStoreFixture(t, p, "compat-shared")
	numeric, index := compatNumericID(a.Account.ID), compatAuthIndex(a.Account.ID)
	if numeric <= 0 || numeric > 9007199254740991 || len(index) != 16 {
		t.Fatal("invalid stable identifiers")
	}
	updated, err := p.compatMutateAccount(ctx, a.Account.ID, a.Account.Version, func(a *Account, r *adminCompatRecord) error {
		a.Name = "compat-renamed"
		a.AccessToken = "rotated-synthetic-token"
		r.Sub2["extra"] = json.RawMessage(`{"email":"synthetic@example.invalid"}`)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Account.CredentialVersion <= a.Account.CredentialVersion || updated.Account.Version <= a.Account.Version {
		t.Fatal("credential or row generation not advanced")
	}
	if compatNumericID(updated.Account.ID) != numeric || compatAuthIndex(updated.Account.ID) != index {
		t.Fatal("token rotation changed client identity")
	}
	for _, ref := range []string{a.Account.ID, "compat-renamed", "compat-shared.json"} {
		id, err := p.compatResolveAccount(ctx, ref)
		if err != nil || id != a.Account.ID {
			t.Fatalf("resolve %s: %s %v", ref, id, err)
		}
	}
	_, err = p.compatMutateAccount(ctx, a.Account.ID, a.Account.Version, func(a *Account, r *adminCompatRecord) error { a.Enabled = false; return nil })
	if !errors.Is(err, errVersionConflict) {
		t.Fatalf("stale write accepted: %v", err)
	}
	_, err = p.compatMutateAccount(ctx, a.Account.ID, updated.Account.Version, func(a *Account, r *adminCompatRecord) error {
		a.AccessToken = "must-not-commit"
		r.CPA["id_token"] = json.RawMessage(`"must-not-commit"`)
		return errInvalid
	})
	if !errors.Is(err, errInvalid) {
		t.Fatal(err)
	}
	current, err := p.compatGetAccount(ctx, a.Account.ID)
	if err != nil || current.Account.AccessToken != updated.Account.AccessToken || string(current.Record.CPA["id_token"]) != `"synthetic-private-id-token"` {
		t.Fatal("failed callback partially committed", err)
	}
	_, err = p.compatMutateAccount(ctx, "", 0, func(b *Account, r *adminCompatRecord) error {
		b.Name = "other"
		b.AccessToken = "other-token"
		r.FileName = "compat-shared.json"
		return nil
	})
	if !errors.Is(err, errConflict) {
		t.Fatal("alias collision accepted", err)
	}
	// Metadata read does not leak native credentials; record is internal-only.
	rows, err := p.compatListAccounts(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Account.AccessToken != "" || row.Account.RefreshToken != "" {
			t.Fatal("metadata contains token")
		}
	}
	// Two callers with one version cannot both replace credentials.
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Go(func() {
			_, err := p.compatMutateAccount(ctx, a.Account.ID, current.Account.Version, func(a *Account, r *adminCompatRecord) error { a.Notes = "concurrent"; return nil })
			results <- err
		})
	}
	wg.Wait()
	close(results)
	passed, conflicts := 0, 0
	for err := range results {
		if err == nil {
			passed++
		} else if errors.Is(err, errVersionConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if passed != 1 || conflicts != 1 {
		t.Fatalf("lost update: success=%d conflict=%d", passed, conflicts)
	}
	current, err = p.compatGetAccount(ctx, a.Account.ID)
	if err != nil {
		t.Fatal(err)
	}
	current, err = p.compatMutateAccount(ctx, a.Account.ID, current.Account.Version, func(a *Account, r *adminCompatRecord) error {
		a.Enabled = true
		a.ExpiresAt = time.Now().Add(time.Hour).Unix()
		r.Sub2["expires_at"], _ = json.Marshal(time.Now().Add(-time.Hour).Unix())
		r.Sub2["auto_pause_on_expired"] = json.RawMessage(`true`)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := p.getManagedLease(ctx, a.Account.ID)
	if err != nil || current.Account.Enabled || lease.ExpiresAt >= time.Now().Unix() || current.Account.ExpiresAt <= time.Now().Unix() {
		t.Fatal("management lease confused with token expiry", err)
	}
	if err = p.Store.DeleteVersion(a.Account.ID, current.Account.Version); err != nil {
		t.Fatal(err)
	}
	if _, err = p.compatGetAccount(ctx, a.Account.ID); !errors.Is(err, errNotFound) {
		t.Fatal("deleted account still present", err)
	}
	if p.Store.DB == nil {
		state, _ := p.Store.readState()
		if _, ok := state.AdminCompat[a.Account.ID]; ok {
			t.Fatal("native delete retained private compatibility metadata")
		}
	} else {
		if _, err = p.managementState().Get(ctx, compatRecordNamespace, a.Account.ID); !errors.Is(err, errNotFound) {
			t.Fatal("native delete retained encrypted compatibility metadata", err)
		}
	}
}

func TestManagedCompatStoreLifecycle(t *testing.T)   { exerciseCompatStore(t, webTestSetup) }
func TestPGManagedCompatStoreLifecycle(t *testing.T) { exerciseCompatStore(t, adminPGTestProxy) }

func TestPGManagedCompatStoreAtomicFailure(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	ctx := context.Background()
	row := compatStoreFixture(t, p, "atomic-compat")
	if _, err := p.Store.DB.Pool.Exec(ctx, `CREATE FUNCTION reject_compat_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='account.compat.update' THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_compat_audit BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_compat_audit()`); err != nil {
		t.Fatal(err)
	}
	_, err := p.compatMutateAccount(ctx, row.Account.ID, row.Account.Version, func(a *Account, r *adminCompatRecord) error {
		a.AccessToken = "rollback-token"
		a.Name = "rollback-name"
		r.FileName = "rollback-file.json"
		return nil
	})
	if err == nil {
		t.Fatal("injected audit failure succeeded")
	}
	current, err := p.compatGetAccount(ctx, row.Account.ID)
	if err != nil || current.Account.AccessToken != row.Account.AccessToken || current.Account.Name != row.Account.Name || current.Record.FileName != row.Record.FileName {
		t.Fatal("transaction partially committed", err)
	}
	key, _ := managementStateKey(compatRecordNamespace, row.Account.ID)
	var stored string
	if err := p.Store.DB.Pool.QueryRow(ctx, "SELECT value::text FROM settings WHERE key=$1", key).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "synthetic-private-id-token") {
		t.Fatal("compatibility credential metadata stored plaintext")
	}
}
