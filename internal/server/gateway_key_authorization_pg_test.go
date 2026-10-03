package server

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func gatewayKeyPGAssertCachedSnapshot(t *testing.T, keys *KeyStore, expected APIKey) {
	t.Helper()
	entry, found := keys.Lookup(expected.Key)
	if !found || entry == nil || entry.ID != expected.ID || entry.Enabled != expected.Enabled ||
		entry.Version != expected.Version || !reflect.DeepEqual(entry.Groups, expected.Groups) {
		t.Fatal("test no longer holds the original stale reader key index")
	}
}

func gatewayKeyPGAssertSelectionDenied(t *testing.T, f *gatewaySelectionPGFixture, session string) {
	t.Helper()
	f.keepOriginalCacheWarm(t)
	account, release, _, _, err := f.reader.pickAccount(t.Context(), f.key.Key)
	if release != nil {
		release()
	}
	if err == nil || release != nil || account.ID != "" {
		t.Fatal("unbound selection accepted a stale key grant")
	}
	account, release, err = f.reader.pickSessionAccount(t.Context(), f.key.Key, f.key.ID, session)
	if release != nil {
		release()
	}
	if err == nil || release != nil || account.ID != "" {
		t.Fatal("bound HTTP selection accepted a stale key grant")
	}
	if _, err := f.reader.authorizedGatewayWSAccountContext(t.Context(), f.key.Key, f.key.ID, f.primary); err == nil {
		t.Fatal("existing WS next-turn authorizer accepted a stale key grant")
	}
	bound, err := f.reader.sessionAccount(t.Context(), f.key.ID, session, "")
	if err != nil || bound != gatewayAccountRef(f.primary) {
		t.Fatal("key revocation silently changed the existing owner binding")
	}
	if f.reader.inFlight(gatewayAdmissionKey(f.primary)) != 0 || f.transport.calls.Load() != 0 {
		t.Fatal("key revocation admitted a request or attempted outbound HTTP")
	}
}

func TestPGGatewayKeyAuthorizationRevokesStaleIndex(t *testing.T) {
	for _, mutation := range []string{"disabled", "deleted", "rotated", "groups"} {
		t.Run(mutation, func(t *testing.T) {
			f := newGatewaySelectionPGFixture(t, false, false)
			const session = "key-revocation-original-session"
			f.selectNewSession(t, session, &f.primary)
			gatewayKeyPGAssertCachedSnapshot(t, f.reader.Keys, f.key)
			var rotated string
			if mutation == "deleted" {
				if err := f.writer.Keys.Delete(f.key.ID); err != nil {
					t.Fatal("cannot delete persisted gateway key")
				}
			} else {
				if _, err := f.writer.Keys.Update(f.key.ID, func(key *APIKey) error {
					switch mutation {
					case "disabled":
						key.Enabled = false
					case "rotated":
						rotated = newAPIKey()
						key.Key = rotated
					case "groups":
						key.Groups = []string{f.otherGroup}
					}
					return nil
				}); err != nil {
					t.Fatal("cannot mutate persisted gateway key")
				}
			}
			gatewayKeyPGAssertCachedSnapshot(t, f.reader.Keys, f.key)
			current, found, err := f.reader.Keys.LookupCurrent(t.Context(), f.key.Key)
			if err != nil {
				t.Fatal("authoritative lookup failed after a valid key mutation")
			}
			id, _, authorized := f.reader.identifyKeyContext(t.Context(), f.key.Key)
			if mutation == "groups" {
				if !found || current.ID != f.key.ID || !reflect.DeepEqual(current.Groups, []string{f.otherGroup}) ||
					!authorized || id != f.key.ID {
					t.Fatal("group changes should update grants without revoking the key identity")
				}
			} else if found || authorized {
				t.Fatal("reader still authenticates a revoked or replaced token from its stale index")
			}
			gatewayKeyPGAssertSelectionDenied(t, f, session)
			if mutation == "rotated" {
				if _, cached := f.reader.Keys.Lookup(rotated); cached {
					t.Fatal("rotation control requires the new token to be absent from reader index")
				}
				if id, _, valid := f.reader.identifyKeyContext(t.Context(), rotated); !valid || id != f.key.ID {
					t.Fatal("current rotated token was not recognized without refreshing reader index")
				}
			}
			gatewayKeyPGAssertCachedSnapshot(t, f.reader.Keys, f.key)
			currentAccount, err := f.writer.Store.Get(f.primary.ID)
			if err != nil || currentAccount.OwnerRevision != f.primary.OwnerRevision ||
				currentAccount.CredentialVersion != f.primary.CredentialVersion || currentAccount.LastError != f.primary.LastError {
				t.Fatal("key authorization change modified account credentials or OAuth error state")
			}
		})
	}
}

func TestPGGatewayKeyAuthorizationAcceptsUnindexedCurrentKey(t *testing.T) {
	f := newGatewaySelectionPGFixture(t, false, false)
	fresh, err := f.writer.Keys.Create("current unindexed key", append([]string(nil), f.key.Groups...))
	if err != nil {
		t.Fatal("cannot create current persisted key")
	}
	if _, cached := f.reader.Keys.Lookup(fresh.Key); cached {
		t.Fatal("new-key control requires a missing reader index entry")
	}
	current, found, err := f.reader.Keys.LookupCurrent(t.Context(), fresh.Key)
	if err != nil || !found || current.ID != fresh.ID || !reflect.DeepEqual(current.Groups, fresh.Groups) {
		t.Fatal("authoritative lookup cannot read a new key absent from local index")
	}
	current.Groups[0] = f.otherGroup
	if next, found, err := f.reader.Keys.LookupCurrent(t.Context(), fresh.Key); err != nil || !found || !reflect.DeepEqual(next.Groups, fresh.Groups) {
		t.Fatal("lookup result mutation changed persisted or shared authorization state")
	}
	if id, _, valid := f.reader.identifyKeyContext(t.Context(), fresh.Key); !valid || id != fresh.ID {
		t.Fatal("gateway identity rejected a current unindexed key")
	}
	f.key = fresh
	f.selectNewSession(t, "current-unindexed-key-session", &f.primary)
	if _, err := f.reader.authorizedGatewayWSAccountContext(t.Context(), fresh.Key, fresh.ID, f.primary); err != nil {
		t.Fatal("WS authorizer rejected a current unindexed key")
	}
	if _, cached := f.reader.Keys.Lookup(fresh.Key); cached {
		t.Fatal("authoritative authorization unexpectedly mutated the reader index")
	}
}

func TestPGGatewayKeyAuthorizationRejectsCorruptCiphertext(t *testing.T) {
	f := newGatewaySelectionPGFixture(t, false, false)
	const session = "key-corruption-original-session"
	f.selectNewSession(t, session, &f.primary)
	gatewayKeyPGAssertCachedSnapshot(t, f.reader.Keys, f.key)
	tag, err := f.writer.Store.DB.Pool.Exec(t.Context(), `UPDATE settings SET value=$1::jsonb,version=version+1 WHERE key='gateway_keys'`, `{"ciphertext":"AAAA"}`)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatal("cannot prepare isolated encrypted key document corruption")
	}
	entry, found, err := f.reader.Keys.LookupCurrent(t.Context(), f.key.Key)
	if !errors.Is(err, errGatewayKeyStoreUnavailable) || found || entry.ID != "" || err.Error() != errGatewayKeyStoreUnavailable.Error() {
		t.Fatal("corrupted authoritative document fell back to valid cached credentials")
	}
	id, _, resolved, err := f.reader.resolveGatewayKeyContext(t.Context(), f.key.Key)
	if !errors.Is(err, errGatewayKeyStoreUnavailable) || id != "" || resolved != nil || err.Error() != errGatewayKeyStoreUnavailable.Error() {
		t.Fatal("gateway resolver did not return a fixed unavailable error for key document corruption")
	}
	if _, _, valid := f.reader.identifyKeyContext(t.Context(), f.key.Key); valid {
		t.Fatal("compatibility identity wrapper authenticated from stale data after corruption")
	}
	gatewayKeyPGAssertSelectionDenied(t, f, session)
	gatewayKeyPGAssertCachedSnapshot(t, f.reader.Keys, f.key)
}

func TestPGGatewayKeyAuthorizationCancelsBlockedAuthoritativeRead(t *testing.T) {
	f := newGatewaySelectionPGFixture(t, false, false)
	tracer := &gatewaySelectionPGAcquireTrace{started: make(chan struct{}, 1)}
	config := f.writer.Store.DB.Pool.Config()
	config.MaxConns, config.MinConns = 1, 0
	config.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal("cannot initialize single-slot key-read pool")
	}
	t.Cleanup(pool.Close)
	keyDB := *f.writer.Store.DB
	keyDB.Pool = pool
	keys, err := NewPGKeyStore(&keyDB)
	if err != nil {
		t.Fatal("cannot initialize isolated key reader with a populated local index")
	}
	held, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal("cannot occupy sole key-read connection")
	}
	defer held.Release()
	if pool.Stat().AcquiredConns() != 1 || pool.Stat().MaxConns() != 1 {
		t.Fatal("authoritative key-read pool is not fully occupied")
	}
	beforeCanceled := pool.Stat().CanceledAcquireCount()
	tracer.armed.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	type outcome struct {
		err   error
		found bool
		id    string
	}
	done := make(chan outcome, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		entry, found, err := keys.LookupCurrent(ctx, f.key.Key)
		done <- outcome{err: err, found: found, id: entry.ID}
	}()
	defer func() {
		cancel()
		held.Release()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("key lookup did not stop after releasing the blocked test pool")
		}
	}()
	select {
	case <-tracer.started:
	case <-time.After(5 * time.Second):
		t.Fatal("key lookup did not reach the blocked authoritative acquire")
	}
	cancel()
	select {
	case result := <-done:
		if !errors.Is(result.err, context.Canceled) || result.found || result.id != "" {
			t.Fatal("cancelled key lookup authenticated or lost caller cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("caller cancellation did not interrupt the blocked key read")
	}
	if pool.Stat().CanceledAcquireCount() != beforeCanceled+1 || pool.Stat().AcquiredConns() != 1 {
		t.Fatal("cancellation did not terminate exactly the waiting key acquire")
	}
	gatewayKeyPGAssertCachedSnapshot(t, keys, f.key)
	if current, found, err := f.writer.Keys.LookupCurrent(t.Context(), f.key.Key); err != nil || !found || current.ID != f.key.ID {
		t.Fatal("cancelled key lookup changed authoritative credentials")
	}
}
