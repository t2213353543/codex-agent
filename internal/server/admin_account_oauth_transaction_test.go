package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func oauthApplyReadyForTest(t *testing.T, p *Proxy, target Account, credentials Account) string {
	t.Helper()
	id := managedOAuthRandom()
	state := managedOAuthSession{Owner: "bearer:" + adminKeyHash(webTestKey), Method: "session", Phase: "ready", TargetID: target.ID, TargetVersion: target.Version, Credentials: canonicalManagedCredentials(credentials)}
	if err := p.managementState().Update(t.Context(), managedOAuthNamespace, id, func(entry *managementStateEntry) error {
		entry.ExpiresAt = time.Now().Add(time.Hour).Unix()
		var err error
		entry.Payload, err = json.Marshal(state)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func oauthApplyPoolForTest(t *testing.T, db *Database, size int32, before func(context.Context, *pgx.Conn) bool) *pgxpool.Pool {
	t.Helper()
	old := db.Pool
	config := old.Config()
	config.MaxConns, config.MinConns = size, 0
	if before != nil {
		config.BeforeAcquire = before
	}
	limited, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	db.Pool = limited
	t.Cleanup(func() { db.Pool = old; limited.Close() })
	return old
}

func oauthApplyHTTPForTest(t *testing.T, handler http.Handler, input managedOAuthApplyInput, status int) Account {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/account-oauth/apply", strings.NewReader(string(raw))).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+webTestKey)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != status {
		t.Fatalf("apply: got %d want %d: %s", response.Code, status, response.Body.String())
	}
	var result struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}
	if status == http.StatusOK {
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.ID == "" || result.Version < 1 {
			t.Fatalf("missing persisted result: %s", response.Body.String())
		}
	}
	return Account{ID: result.ID, Version: result.Version}
}

func oauthApplyStateForTest(t *testing.T, p *Proxy, id string) (managementStateEntry, managedOAuthSession) {
	t.Helper()
	entry, err := p.managementState().Get(t.Context(), managedOAuthNamespace, id)
	if err != nil {
		t.Fatal(err)
	}
	var state managedOAuthSession
	if err := json.Unmarshal(entry.Payload, &state); err != nil {
		t.Fatal(err)
	}
	return entry, state
}

func TestPGManagedOAuthApplySingleConnection(t *testing.T) {
	p, h := adminPGTestProxy(t)
	group, err := p.Groups.Create("oauth-group", "")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := p.Proxies.Create(ProxyEntry{Name: "oauth-proxy", Type: "http", Host: "127.0.0.1", Port: 12345})
	if err != nil {
		t.Fatal(err)
	}
	oauthApplyPoolForTest(t, p.Store.DB, 1, nil)
	credentials := Account{AccessToken: "oauth-apply-token", RefreshToken: "oauth-apply-refresh", AccountID: "oauth-upstream"}
	id := oauthApplyReadyForTest(t, p, Account{}, credentials)
	notes, tags, enabled := "preserve metadata", []string{"oauth", "staged"}, false
	input := managedOAuthApplyInput{SessionID: id, Name: "single-connection", Notes: &notes, Tags: &tags, Enabled: &enabled, Defaults: AccountDefaults{Groups: []string{group.ID}, Proxies: []string{proxy.ID}, Concurrency: 4, Weight: 3}}
	result := oauthApplyHTTPForTest(t, h, input, http.StatusOK)
	current, err := p.Store.Get(result.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.AccessToken != credentials.AccessToken || current.RefreshToken != credentials.RefreshToken || current.Enabled || current.Notes != notes || len(current.Tags) != 2 || len(current.Groups) != 1 || current.Groups[0] != group.ID || len(current.Proxies) != 1 || current.Proxies[0] != proxy.ID || current.Concurrency != 4 || current.Weight != 3 {
		t.Fatalf("create lost credentials/defaults/metadata: %+v", accountMetadata(current))
	}
	replay := oauthApplyHTTPForTest(t, h, input, http.StatusOK)
	if replay.ID != result.ID || replay.Version != result.Version {
		t.Fatalf("applied replay changed result: %+v %+v", result, replay)
	}
	_, state := oauthApplyStateForTest(t, p, id)
	if state.Phase != "applied" || len(state.Credentials) != 0 {
		t.Fatal("successful acknowledgement retained staged credentials")
	}

	// Duplicate import must use the native rotation path, retaining identity and management metadata.
	credentials.RefreshToken = "oauth-rotated-refresh"
	duplicate := oauthApplyReadyForTest(t, p, Account{}, credentials)
	dupResult := oauthApplyHTTPForTest(t, h, managedOAuthApplyInput{SessionID: duplicate, Name: "not-a-new-account"}, http.StatusOK)
	rotated, err := p.Store.Get(result.ID)
	if err != nil || dupResult.ID != result.ID || rotated.RefreshToken != credentials.RefreshToken || rotated.Name != current.Name || rotated.Notes != notes || rotated.CredentialVersion != current.CredentialVersion+1 {
		t.Fatalf("duplicate rotation changed native import semantics: %+v %v", accountMetadata(rotated), err)
	}

	// Explicit reauthorization clears an absent refresh token and preserves account metadata.
	incoming := Account{AccessToken: "oauth-reauth-token", AccountID: rotated.AccountID}
	reauth := oauthApplyReadyForTest(t, p, rotated, incoming)
	oauthApplyHTTPForTest(t, h, managedOAuthApplyInput{SessionID: reauth}, http.StatusOK)
	replaced, err := p.Store.Get(result.ID)
	if err != nil || replaced.AccessToken != incoming.AccessToken || replaced.RefreshToken != "" || replaced.Notes != notes || replaced.Enabled {
		t.Fatalf("reauthorization mismatch: %+v %v", accountMetadata(replaced), err)
	}

	// A pre-upgrade account commit may have succeeded before the session acknowledgement.
	legacyIncoming := Account{AccessToken: "oauth-legacy-commit", RefreshToken: "oauth-legacy-refresh", AccountID: replaced.AccountID}
	legacy := oauthApplyReadyForTest(t, p, replaced, legacyIncoming)
	if _, err := p.Store.reauthorizeManagedAccount(t.Context(), replaced.ID, replaced.Version, legacyIncoming, false); err != nil {
		t.Fatal(err)
	}
	committed, err := p.Store.Get(result.ID)
	if err != nil {
		t.Fatal(err)
	}
	var auditBefore int
	if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_logs WHERE action='account.credentials.update'").Scan(&auditBefore); err != nil {
		t.Fatal(err)
	}
	recovered := oauthApplyHTTPForTest(t, h, managedOAuthApplyInput{SessionID: legacy}, http.StatusOK)
	after, err := p.Store.Get(result.ID)
	var auditAfter int
	if e := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_logs WHERE action='account.credentials.update'").Scan(&auditAfter); e != nil {
		t.Fatal(e)
	}
	if err != nil || recovered.Version != committed.Version || after.Version != committed.Version || after.CredentialVersion != committed.CredentialVersion || auditAfter != auditBefore {
		t.Fatal("legacy acknowledgement replay wrote credentials twice", err)
	}
	if p.Store.DB.Pool.Stat().AcquiredConns() != 0 {
		t.Fatal("apply leaked its only connection")
	}
}

func TestPGManagedOAuthApplyConcurrentBoundedPool(t *testing.T) {
	for _, targetMode := range []bool{false, true} {
		t.Run(fmt.Sprintf("reauthorize=%v", targetMode), func(t *testing.T) {
			p, _ := adminPGTestProxy(t)
			inputs := make([]managedOAuthApplyInput, 2)
			for i := range inputs {
				var target Account
				if targetMode {
					target = pgImportForTest(t, p.Store, fmt.Sprintf("parallel-target-%d", i), fmt.Sprintf("parallel-old-%d", i))
				}
				incoming := Account{AccessToken: fmt.Sprintf("parallel-new-%d", i), RefreshToken: fmt.Sprintf("parallel-refresh-%d", i)}
				inputs[i] = managedOAuthApplyInput{SessionID: oauthApplyReadyForTest(t, p, target, incoming), Name: fmt.Sprintf("parallel-created-%d", i)}
			}
			acquired := make(chan struct{}, 2)
			release := make(chan struct{})
			var acquireCount atomic.Int32
			oauthApplyPoolForTest(t, p.Store.DB, 2, func(ctx context.Context, _ *pgx.Conn) bool {
				if acquireCount.Add(1) <= 2 {
					acquired <- struct{}{}
					select {
					case <-release:
						return true
					case <-ctx.Done():
						return false
					}
				}
				return true
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan error, 2)
			for _, input := range inputs {
				go func(input managedOAuthApplyInput) {
					_, err := p.applyManagedOAuth(ctx, "bearer:"+adminKeyHash(webTestKey), input)
					done <- err
				}(input)
			}
			for i := 0; i < 2; i++ {
				select {
				case <-acquired:
				case <-ctx.Done():
					close(release)
					t.Fatal("outer transactions failed to reach bounded-pool barrier")
				}
			}
			close(release)
			for i := 0; i < 2; i++ {
				select {
				case err := <-done:
					if err != nil {
						t.Fatal("apply failed while both pool connections were held", err)
					}
				case <-ctx.Done():
					t.Fatal("nested pool acquisition starved apply")
				}
			}
			if acquireCount.Load() != 2 {
				t.Fatalf("two applies acquired %d connections; nested pool use returned", acquireCount.Load())
			}
			if p.Store.DB.Pool.Stat().AcquiredConns() != 0 {
				t.Fatal("concurrent apply leaked connections")
			}
			for _, input := range inputs {
				_, state := oauthApplyStateForTest(t, p, input.SessionID)
				if state.Phase != "applied" {
					t.Fatal("concurrent apply not acknowledged")
				}
			}
		})
	}
}

func TestPGManagedOAuthApplyConcurrentReplayAndCAS(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	oauthApplyPoolForTest(t, p.Store.DB, 2, nil)
	owner := "bearer:" + adminKeyHash(webTestKey)
	id := oauthApplyReadyForTest(t, p, Account{}, Account{AccessToken: "replayed-create", RefreshToken: "replayed-refresh"})
	type outcome struct {
		account Account
		err     error
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	results := make(chan outcome, 8)
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			<-start
			a, err := p.applyManagedOAuth(ctx, owner, managedOAuthApplyInput{SessionID: id, Name: "concurrent-replay"})
			results <- outcome{a, err}
		}()
	}
	close(start)
	var first Account
	for i := 0; i < 8; i++ {
		select {
		case r := <-results:
			if r.err != nil {
				t.Fatal("same-session replay failed", r.err)
			}
			if i == 0 {
				first = r.account
			} else if r.account.ID != first.ID || r.account.Version != first.Version {
				t.Fatal("concurrent replay returned different results")
			}
		case <-ctx.Done():
			t.Fatal("concurrent replay did not finish")
		}
	}
	var imports int
	if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_logs WHERE action='account.import' AND entity_id=$1", first.ID).Scan(&imports); err != nil || imports != 1 {
		t.Fatal("same-session replay wrote account more than once", imports, err)
	}
	target, err := p.Store.Get(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		incoming := Account{AccessToken: fmt.Sprintf("cas-winner-%d", i), RefreshToken: fmt.Sprintf("cas-refresh-%d", i)}
		input := managedOAuthApplyInput{SessionID: oauthApplyReadyForTest(t, p, target, incoming)}
		go func() { a, err := p.applyManagedOAuth(ctx, owner, input); results <- outcome{a, err} }()
	}
	successes, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			if r.err == nil {
				successes++
			} else if errors.Is(r.err, errVersionConflict) {
				conflicts++
			} else {
				t.Fatal("unexpected competing CAS error", r.err)
			}
		case <-ctx.Done():
			t.Fatal("competing CAS did not finish")
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("CAS winners/conflicts %d/%d", successes, conflicts)
	}
	after, err := p.Store.Get(target.ID)
	if err != nil || after.Version != target.Version+1 || after.CredentialVersion != target.CredentialVersion+1 {
		t.Fatal("competing CAS wrote twice", err)
	}
}

func TestPGManagedOAuthApplyAtomicRollback(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	oauthApplyPoolForTest(t, p.Store.DB, 1, nil)
	for _, point := range []string{"ack", "audit"} {
		for _, targetMode := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reauthorize=%v", point, targetMode), func(t *testing.T) {
				suffix := fmt.Sprintf("%s-%v", point, targetMode)
				var before Account
				if targetMode {
					before = pgImportForTest(t, p.Store, "rollback-target-"+suffix, "rollback-old-"+suffix)
				}
				incoming := Account{AccessToken: "rollback-new-" + suffix, RefreshToken: "rollback-refresh-" + suffix}
				id := oauthApplyReadyForTest(t, p, before, incoming)
				entryBefore, _ := oauthApplyStateForTest(t, p, id)
				key, _ := managementStateKey(managedOAuthNamespace, id)
				var countBefore, auditBefore int
				if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM accounts),(SELECT count(*) FROM audit_logs)").Scan(&countBefore, &auditBefore); err != nil {
					t.Fatal(err)
				}
				statement := `CREATE OR REPLACE FUNCTION reject_oauth_apply() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.key=TG_ARGV[0] THEN RAISE EXCEPTION 'injected OAuth acknowledgement failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_oauth_apply BEFORE UPDATE ON settings FOR EACH ROW EXECUTE FUNCTION reject_oauth_apply('` + key + `')`
				table := "settings"
				if point == "audit" {
					table = "audit_logs"
					statement = `CREATE OR REPLACE FUNCTION reject_oauth_apply() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action IN ('account.import','account.credentials.update') THEN RAISE EXCEPTION 'injected OAuth audit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_oauth_apply BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_oauth_apply()`
				}
				if _, err := p.Store.DB.Pool.Exec(t.Context(), statement); err != nil {
					t.Fatal(err)
				}
				input := managedOAuthApplyInput{SessionID: id, Name: "rollback-new-account-" + suffix}
				_, applyErr := p.applyManagedOAuth(t.Context(), "bearer:"+adminKeyHash(webTestKey), input)
				if _, err := p.Store.DB.Pool.Exec(t.Context(), "DROP TRIGGER reject_oauth_apply ON "+table); err != nil {
					t.Fatal(err)
				}
				if applyErr == nil {
					t.Fatal("injected failure was accepted")
				}
				var countAfter, auditAfter int
				if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM accounts),(SELECT count(*) FROM audit_logs)").Scan(&countAfter, &auditAfter); err != nil {
					t.Fatal(err)
				}
				entryAfter, state := oauthApplyStateForTest(t, p, id)
				if countAfter != countBefore || auditAfter != auditBefore || entryAfter.Version != entryBefore.Version || string(entryAfter.Payload) != string(entryBefore.Payload) || state.Phase != "ready" {
					t.Fatal("failed transaction partially committed account/audit/session")
				}
				if targetMode {
					after, err := p.Store.Get(before.ID)
					if err != nil || after.Version != before.Version || after.CredentialVersion != before.CredentialVersion || after.AccessToken != before.AccessToken {
						t.Fatal("failed acknowledgement committed target credentials", err)
					}
				}
				if _, err := p.applyManagedOAuth(t.Context(), "bearer:"+adminKeyHash(webTestKey), input); err != nil {
					t.Fatal("retry after rollback failed", err)
				}
			})
		}
	}
}

func TestPGManagedOAuthApplyCancellationRollsBackAccount(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	id := oauthApplyReadyForTest(t, p, Account{}, Account{AccessToken: "cancel-before-ack", RefreshToken: "cancel-refresh"})
	key, _ := managementStateKey(managedOAuthNamespace, id)
	var pid atomic.Uint32
	monitor := oauthApplyPoolForTest(t, p.Store.DB, 1, func(_ context.Context, conn *pgx.Conn) bool { pid.Store(conn.PgConn().PID()); return true })
	blocker, err := monitor.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(t.Context(), "SELECT key FROM settings WHERE key=$1 FOR UPDATE", key); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := p.applyManagedOAuth(ctx, "bearer:"+adminKeyHash(webTestKey), managedOAuthApplyInput{SessionID: id, Name: "cancelled-new-account"})
		done <- err
	}()
	// The row lock permits reading the ready session and inserting the account,
	// but stops its final settings UPSERT. Cancel precisely after the account DML.
	deadline := time.Now().Add(5 * time.Second)
	waiting := false
	for time.Now().Before(deadline) {
		if pid.Load() != 0 {
			if err := monitor.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock' AND query LIKE 'INSERT INTO settings%')", int64(pid.Load())).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !waiting {
		cancel()
		t.Fatal("apply did not reach the final acknowledgement lock")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled write reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not release apply")
	}
	if err := blocker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM accounts WHERE display_name='cancelled-new-account')").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	_, state := oauthApplyStateForTest(t, p, id)
	if exists || state.Phase != "ready" || len(state.Credentials) == 0 {
		t.Fatal("cancellation left a committed account or acknowledgement")
	}
	if p.Store.DB.Pool.Stat().AcquiredConns() != 0 {
		t.Fatal("cancellation leaked the only pool connection")
	}
}

func TestManagedOAuthApplyOwnerCASAndValidation(t *testing.T) {
	runOAuthApplyOwnerCASAndValidation(t, webTestSetup)
}
func TestPGManagedOAuthApplyOwnerCASAndValidation(t *testing.T) {
	runOAuthApplyOwnerCASAndValidation(t, adminPGTestProxy)
}
func runOAuthApplyOwnerCASAndValidation(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	owner := "bearer:" + adminKeyHash(webTestKey)
	current := managedTestAccount(t, p, "validation-target", nil)
	incoming := Account{AccessToken: "validation-new", RefreshToken: "validation-refresh", AccountID: "different-identity"}
	id := oauthApplyReadyForTest(t, p, current, incoming)
	input := managedOAuthApplyInput{SessionID: id}
	if _, err := p.applyManagedOAuth(t.Context(), "other-owner", input); !errors.Is(err, errNotFound) {
		t.Fatalf("owner binding lost: %v", err)
	}
	if _, err := p.applyManagedOAuth(t.Context(), owner, input); !errors.Is(err, errManagedIdentityChange) {
		t.Fatalf("identity confirmation lost: %v", err)
	}
	input.AllowIdentityChange = true
	changed, err := p.Store.UpdateAccountVersion(current.ID, current.Version, func(a *Account) error { a.Notes = "concurrent change"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.applyManagedOAuth(t.Context(), owner, input); !errors.Is(err, errVersionConflict) {
		t.Fatalf("CAS ignored: %v", err)
	}
	after, err := p.Store.Get(current.ID)
	if err != nil || after.Version != changed.Version || after.AccessToken != current.AccessToken {
		t.Fatal("rejected apply changed account", err)
	}
	other := managedTestAccount(t, p, "duplicate-token-owner", nil)
	duplicate := oauthApplyReadyForTest(t, p, changed, other)
	if _, err := p.applyManagedOAuth(t.Context(), owner, managedOAuthApplyInput{SessionID: duplicate, AllowIdentityChange: true}); !errors.Is(err, errConflict) {
		t.Fatalf("duplicate token accepted: %v", err)
	}
	if err := p.managementState().Update(t.Context(), managedOAuthNamespace, id, func(entry *managementStateEntry) error { entry.ExpiresAt = time.Now().Unix() - 1; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := p.applyManagedOAuth(t.Context(), owner, input); !errors.Is(err, errNotFound) {
		t.Fatalf("expiry ignored: %v", err)
	}
	fresh := oauthApplyReadyForTest(t, p, Account{}, Account{AccessToken: "invalid-groups"})
	if _, err := p.applyManagedOAuth(t.Context(), owner, managedOAuthApplyInput{SessionID: fresh, Name: "invalid-group-account", Defaults: AccountDefaults{Groups: []string{"missing-group"}}}); !errors.Is(err, errInvalid) {
		t.Fatalf("missing group accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.applyManagedOAuth(ctx, owner, managedOAuthApplyInput{SessionID: fresh, Name: "cancelled-before-write"}); err == nil {
		t.Fatal("pre-canceled apply succeeded")
	}
}
