package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type gatewaySelectionPGRejectTransport struct{ calls atomic.Int32 }

func (r *gatewaySelectionPGRejectTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls.Add(1)
	return nil, errors.New("synthetic selection test forbids outbound HTTP")
}

type gatewaySelectionPGFixture struct {
	writer, reader     *Proxy
	key                APIKey
	primary, alternate Account
	otherGroup         string
	warmed             []Account
	transport          *gatewaySelectionPGRejectTransport
}

// Each instance has its own key index. Synchronization happens once before
// warming candidates; only account changes cross instances in these tests.
func newGatewaySelectionPGFixture(t *testing.T, alternate, expiredPrimary bool) *gatewaySelectionPGFixture {
	t.Helper()
	writer, reader, _ := adminKeyGatewayPGProxies(t)
	redisDB, _ := gatewayRedisSessionFixture(t)
	writer.Store.DB.Redis, writer.Store.DB.Namespace = redisDB.Redis, redisDB.Namespace
	groups := NewPGGroupStore(writer.Store.DB)
	allowed, err := groups.Create("selection allowed", "synthetic group")
	if err != nil {
		t.Fatal("cannot create persisted allowed group")
	}
	other, err := groups.Create("selection other", "synthetic group")
	if err != nil {
		t.Fatal("cannot create persisted other group")
	}
	writer.Keys, err = NewPGKeyStore(writer.Store.DB)
	if err != nil {
		t.Fatal("cannot initialize writer key store")
	}
	reader.Keys, err = NewPGKeyStore(reader.Store.DB)
	if err != nil {
		t.Fatal("cannot initialize independent reader key index")
	}
	key, err := writer.Keys.Create("selection key", []string{allowed.ID})
	if err != nil {
		t.Fatal("cannot create grouped gateway key")
	}
	if _, err := reader.Keys.List(); err != nil {
		t.Fatal("cannot synchronize initial reader key index")
	}
	f := &gatewaySelectionPGFixture{writer: writer, reader: reader, key: key, otherGroup: other.ID,
		transport: &gatewaySelectionPGRejectTransport{}}
	reader.Client = &http.Client{Transport: f.transport}
	reader.TokenURL = "http://selection.invalid/oauth/token"
	importAccount := func(name, user string, priority int) Account {
		t.Helper()
		raw := gatewayRevisionCredentials(t, user, name+"-initial")
		if name == "selection-primary" && expiredPrimary {
			var document map[string]json.RawMessage
			if err := json.Unmarshal(raw, &document); err != nil {
				t.Fatal("cannot prepare synthetic expired credentials")
			}
			document["expires_at"], err = json.Marshal(time.Now().Add(-time.Minute).Unix())
			if err != nil {
				t.Fatal("cannot encode synthetic credential expiry")
			}
			raw, err = json.Marshal(document)
			if err != nil {
				t.Fatal("cannot encode synthetic expired credentials")
			}
		}
		if err := writer.Store.ImportWith(name, raw,
			AccountDefaults{Concurrency: 3, Priority: priority, Groups: []string{allowed.ID}}); err != nil {
			t.Fatal("cannot import grouped selection account")
		}
		account, err := writer.Store.Get(name)
		if err != nil || !gatewayCompleteOwner(account) || account.OwnerRevision == "" {
			t.Fatal("cannot read complete persisted account identity")
		}
		return account
	}
	f.primary = importAccount("selection-primary", "selection-primary-user", 100)
	if alternate {
		f.alternate = importAccount("selection-alternate", "selection-alternate-user", 0)
	}
	if expiredPrimary && (f.primary.ExpiresAt == 0 || f.primary.ExpiresAt >= time.Now().Unix()) {
		t.Fatal("persisted primary credentials must be expired before warming candidates")
	}
	candidates, keyID, _, err := reader.candidatesFor(key.Key)
	want := 1
	if alternate {
		want++
	}
	if err != nil || keyID != key.ID || len(candidates) != want {
		t.Fatal("cannot warm the original authorized candidate set")
	}
	reader.accountsMu.Lock()
	f.warmed = append([]Account(nil), reader.accounts...)
	reader.accountsMu.Unlock()
	t.Cleanup(func() {
		if f.transport.calls.Load() != 0 {
			t.Error("selection attempted OAuth or inference before current authorization")
		}
		for _, account := range []Account{f.primary, f.alternate} {
			if account.ID != "" && reader.inFlight(gatewayAdmissionKey(account)) != 0 {
				t.Error("selection left an admission slot in flight")
			}
		}
	})
	return f
}

func (f *gatewaySelectionPGFixture) keepOriginalCacheWarm(t *testing.T) {
	t.Helper()
	f.reader.accountsMu.Lock()
	defer f.reader.accountsMu.Unlock()
	if !reflect.DeepEqual(f.reader.accounts, f.warmed) {
		t.Fatal("test no longer holds the original stale scheduling snapshot")
	}
	// Extend only the TTL, not the cached account values, for slow race runs.
	f.reader.accountsAt = time.Now()
}

func (f *gatewaySelectionPGFixture) selectNewSession(t *testing.T, session string, expected *Account) Account {
	t.Helper()
	db := f.reader.Store.DB
	threadKey := fmt.Sprintf("%sgateway:thread:%x", db.Namespace, sha256.Sum256([]byte(f.key.ID+"\x00"+session)))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.Redis.Del(ctx, threadKey).Err(); err != nil {
			t.Error("cannot clean isolated selection session binding")
		}
	})
	if bound, err := f.reader.sessionAccount(t.Context(), f.key.ID, session, ""); err != nil || bound != "" {
		t.Fatal("selection requires a new unbound session")
	}
	f.keepOriginalCacheWarm(t)
	account, release, err := f.reader.pickSessionAccount(t.Context(), f.key.Key, f.key.ID, session)
	if release != nil {
		defer release()
	}
	if expected == nil {
		if err == nil || release != nil || account.ID != "" {
			t.Fatal("revoked sole candidate was admitted for a new session")
		}
	} else {
		if err != nil || release == nil || account.ID != expected.ID || gatewayAccountRef(account) != gatewayAccountRef(*expected) {
			t.Fatal("new session did not select the expected current authorized account")
		}
		if account.AccessToken != expected.AccessToken || account.Concurrency != expected.Concurrency {
			t.Fatal("selected account retained cached credentials or concurrency")
		}
		if f.reader.inFlight(gatewayAdmissionKey(account)) != 1 {
			t.Fatal("selected account did not own exactly one admission slot")
		}
	}
	bound, lookupErr := f.reader.sessionAccount(t.Context(), f.key.ID, session, "")
	wantBound := ""
	if expected != nil {
		wantBound = gatewayAccountRef(*expected)
	}
	if lookupErr != nil || bound != wantBound {
		t.Fatal("new Redis session binding does not match the actual selection")
	}
	return account
}

func TestPGGatewaySelectionAuthorizationRejectsStaleCandidates(t *testing.T) {
	for _, mutation := range []string{"groups", "disabled", "deleted"} {
		for _, alternate := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/alternate_%t", mutation, alternate), func(t *testing.T) {
				f := newGatewaySelectionPGFixture(t, alternate, true)
				if mutation == "deleted" {
					if err := f.writer.Store.Delete(f.primary.ID); err != nil {
						t.Fatal("cannot delete persisted primary account")
					}
				} else {
					if err := f.writer.Store.UpdateAccount(f.primary.ID, func(account *Account) error {
						if mutation == "groups" {
							account.Groups = []string{f.otherGroup}
						} else {
							account.Enabled = false
						}
						return nil
					}); err != nil {
						t.Fatal("cannot revoke persisted primary account eligibility")
					}
				}
				var expected *Account
				if alternate {
					expected = &f.alternate
				}
				f.selectNewSession(t, "selection-revoked-primary", expected)
				if mutation != "deleted" {
					current, err := f.writer.Store.Get(f.primary.ID)
					if err != nil || current.LastError != f.primary.LastError || current.CredentialVersion != f.primary.CredentialVersion ||
						current.OwnerRevision != f.primary.OwnerRevision {
						t.Fatal("eligibility denial modified credentials, owner revision, or OAuth error state")
					}
				}
			})
		}
	}
}

func TestPGGatewaySelectionAuthorizationUsesCurrentConcurrency(t *testing.T) {
	for _, alternate := range []bool{false, true} {
		t.Run(fmt.Sprintf("alternate_%t", alternate), func(t *testing.T) {
			f := newGatewaySelectionPGFixture(t, alternate, false)
			held, ok := f.reader.acquireGatewayAccount(f.primary)
			if !ok {
				t.Fatal("cannot establish already-admitted primary request")
			}
			defer held()
			if err := f.writer.Store.UpdateAccount(f.primary.ID, func(account *Account) error {
				account.Concurrency = 1
				return nil
			}); err != nil {
				t.Fatal("cannot lower persisted current concurrency")
			}
			f.keepOriginalCacheWarm(t)
			account, release, keyID, _, err := f.reader.pickAccount(t.Context(), f.key.Key)
			if release != nil {
				defer release()
			}
			if alternate {
				if err != nil || release == nil || account.ID != f.alternate.ID || keyID != f.key.ID {
					t.Fatal("current primary limit did not route new admission to the eligible alternate")
				}
			} else {
				var busy *gatewayAccountBusyError
				if !errors.As(err, &busy) || release != nil {
					t.Fatal("current concurrency limit was bypassed by the higher cached limit")
				}
			}
			if f.reader.inFlight(gatewayAdmissionKey(f.primary)) != 1 {
				t.Fatal("selection changed the existing primary admission or admitted an extra request")
			}
		})
	}
}

func TestPGGatewaySelectionAuthorizationUsesCurrentCredentialsAndOwner(t *testing.T) {
	for _, changedOwner := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed_owner_%t", changedOwner), func(t *testing.T) {
			f := newGatewaySelectionPGFixture(t, false, false)
			user := "selection-primary-user"
			if changedOwner {
				user = "selection-replacement-user"
			}
			report, err := f.writer.Store.ImportDetailed([]ImportItem{{Name: f.primary.Name,
				Credentials: gatewayRevisionCredentials(t, user, "replacement")}}, AccountDefaults{}, ImportOptions{Replace: true})
			if err != nil || report.Replaced != 1 || report.Failed != 0 {
				t.Fatal("cannot replace persisted primary credentials")
			}
			current, err := f.writer.Store.Get(f.primary.ID)
			if err != nil || current.ID != f.primary.ID || current.AccessToken == f.primary.AccessToken ||
				!reflect.DeepEqual(current.Groups, f.primary.Groups) {
				t.Fatal("credential replacement did not preserve the current management account")
			}
			if (gatewayAccountRef(current) != gatewayAccountRef(f.primary)) != changedOwner ||
				(current.OwnerRevision != f.primary.OwnerRevision) != changedOwner {
				t.Fatal("replacement did not establish the intended same-owner or changed-owner case")
			}
			f.selectNewSession(t, "selection-current-owner", &current)
		})
	}
}

type gatewaySelectionPGAcquireTrace struct {
	armed   atomic.Bool
	started chan struct{}
}

func (tr *gatewaySelectionPGAcquireTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (*gatewaySelectionPGAcquireTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func (tr *gatewaySelectionPGAcquireTrace) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	if tr.armed.Load() {
		select {
		case tr.started <- struct{}{}:
		default:
		}
	}
	return ctx
}

func (*gatewaySelectionPGAcquireTrace) TraceAcquireEnd(context.Context, *pgxpool.Pool, pgxpool.TraceAcquireEndData) {
}

func TestPGGatewaySelectionAuthorizationCancelsBlockedAuthoritativeRead(t *testing.T) {
	f := newGatewaySelectionPGFixture(t, false, false)
	settings, err := f.writer.Settings.Get()
	if err != nil {
		t.Fatal("cannot read fixture settings")
	}
	settings.AdminAPI = false
	if err := f.writer.Settings.Set(settings); err != nil {
		t.Fatal("cannot disable admin identification for focused account-read test")
	}
	// Settings keep the original available pool. Only the account store uses
	// the exhausted pool, so the observed acquire is the authoritative read.
	f.reader.Settings = NewPGSettingsStore(f.writer.Store.DB)
	if settings, err := f.reader.Settings.Get(); err != nil || settings.AdminAPI {
		t.Fatal("reader settings did not observe disabled admin identification")
	}
	tracer := &gatewaySelectionPGAcquireTrace{started: make(chan struct{}, 1)}
	config := f.writer.Store.DB.Pool.Config()
	config.MaxConns, config.MinConns = 1, 0
	config.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal("cannot initialize single-slot account-read pool")
	}
	t.Cleanup(pool.Close)
	readerDB := *f.writer.Store.DB
	readerDB.Pool = pool
	f.reader.Store.DB = &readerDB
	held, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal("cannot occupy the sole account-read connection")
	}
	defer held.Release()
	if pool.Stat().AcquiredConns() != 1 || pool.Stat().MaxConns() != 1 {
		t.Fatal("authoritative account-read pool is not fully occupied")
	}
	f.keepOriginalCacheWarm(t)
	beforeCanceled := pool.Stat().CanceledAcquireCount()
	tracer.armed.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	type outcome struct {
		err      error
		admitted bool
		selected bool
	}
	done := make(chan outcome, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		account, release, _, _, err := f.reader.pickAccount(ctx, f.key.Key)
		if release != nil {
			release()
		}
		done <- outcome{err: err, admitted: release != nil, selected: account.ID != ""}
	}()
	defer func() {
		cancel()
		held.Release()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("selection did not stop after releasing the blocked test pool")
		}
	}()
	select {
	case <-tracer.started:
	case <-time.After(5 * time.Second):
		t.Fatal("selection did not reach the blocked authoritative account acquire")
	}
	cancel()
	select {
	case result := <-done:
		if !errors.Is(result.err, context.Canceled) || result.admitted || result.selected {
			t.Fatal("cancelled authoritative read returned admission or lost caller cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("caller cancellation did not interrupt the blocked account read")
	}
	if pool.Stat().CanceledAcquireCount() != beforeCanceled+1 || pool.Stat().AcquiredConns() != 1 {
		t.Fatal("cancellation did not terminate exactly the waiting account acquire")
	}
	current, err := f.writer.Store.Get(f.primary.ID)
	if err != nil || current.LastError != f.primary.LastError || current.CredentialVersion != f.primary.CredentialVersion ||
		current.OwnerRevision != f.primary.OwnerRevision {
		t.Fatal("cancelled pre-admission read altered credential or account error state")
	}
	if f.reader.inFlight(gatewayAdmissionKey(f.primary)) != 0 {
		t.Fatal("cancelled authoritative read retained an admission slot")
	}
}
