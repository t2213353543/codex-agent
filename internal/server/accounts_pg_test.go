package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func accountPGTestStore(t *testing.T) Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is required for isolated PostgreSQL tests")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect independent test PostgreSQL:", err)
	}
	schema := "test_accounts_" + newID("")
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := OpenDatabase(ctx, u.String(), base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), "account-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		_ = admin.Close(context.Background())
	})
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal("migration was not repeatable:", err)
	}
	// Connection establishment is serialized by test tunnels (SSH channel
	// opens), so a cold pool cannot satisfy concurrent handler bursts inside
	// short request deadlines. Establish every pool connection up front, the
	// way a warm production pool against a local database already looks.
	warmCtx, warmCancel := context.WithTimeout(ctx, 60*time.Second)
	defer warmCancel()
	var warm sync.WaitGroup
	for i := int(db.Pool.Stat().MaxConns()); i > 0; i-- {
		warm.Add(1)
		go func() {
			defer warm.Done()
			var one int
			_ = db.Pool.QueryRow(warmCtx, "SELECT 1").Scan(&one)
		}()
	}
	warm.Wait()
	return Store{DB: db}
}

func pgImportForTest(t *testing.T, s Store, name, token string) Account {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"access_token": token, "refresh_token": "refresh-" + token})
	report, err := s.ImportDetailed([]ImportItem{{Name: name, Credentials: raw}}, AccountDefaults{}, ImportOptions{})
	if err != nil || report.Added != 1 {
		t.Fatalf("import failed: %+v %v", report, err)
	}
	a, err := s.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestPGAccountCredentialsAndMetadataIsolation(t *testing.T) {
	s := accountPGTestStore(t)
	a := pgImportForTest(t, s, "one", "test-secret")
	var cipher []byte
	if err := s.DB.Pool.QueryRow(context.Background(), `SELECT credentials_cipher FROM account_credentials WHERE account_id=$1`, a.ID).Scan(&cipher); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(cipher, []byte("test-secret")) {
		t.Fatal("credential stored in plaintext")
	}
	if _, err := s.DB.Pool.Exec(context.Background(), `UPDATE account_credentials SET credentials_cipher='broken'::bytea WHERE account_id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	metadata, err := s.GetMetadata(a.ID)
	if err != nil || metadata.Name != "one" || !metadata.HasRefreshToken || metadata.AccessToken != "" {
		t.Fatalf("metadata read touched encrypted secret: %+v %v", metadata, err)
	}
	page, err := s.ListPage(AccountListOptions{Limit: 1})
	if err != nil || len(page.Accounts) != 1 {
		t.Fatal("metadata page failed:", err)
	}
	if _, err := s.Get(a.ID); err == nil {
		t.Fatal("broken ciphertext was accepted")
	}
}

func TestPGAccountConcurrentVersionAndForeignKeyRollback(t *testing.T) {
	s := accountPGTestStore(t)
	a := pgImportForTest(t, s, "one", "test-secret")
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := s.UpdateAccountVersion(a.ID, a.Version, func(a *Account) error { a.Notes = "new"; return nil })
			results <- err
		}()
	}
	first, second := <-results, <-results
	if !((first == nil && errors.Is(second, errVersionConflict)) || (second == nil && errors.Is(first, errVersionConflict))) {
		t.Fatalf("concurrent updates were not CAS protected: %v / %v", first, second)
	}
	before, _ := s.GetMetadata(a.ID)
	_, err := s.UpdateAccountVersion(a.ID, before.Version, func(a *Account) error { a.Notes = "must rollback"; a.Groups = []string{"missing-group"}; return nil })
	if !errors.Is(err, errInvalid) {
		t.Fatalf("invalid FK was accepted: %v", err)
	}
	after, _ := s.GetMetadata(a.ID)
	if after.Notes != before.Notes || after.Version != before.Version || len(after.Groups) != 0 {
		t.Fatal("failed association transaction changed metadata")
	}
}

func TestPGAccountRefreshSerializesOnlySameAccount(t *testing.T) {
	s := accountPGTestStore(t)
	a := pgImportForTest(t, s, "one", "old-one")
	b := pgImportForTest(t, s, "two", "old-two")
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	results := make(chan error, 2)
	refresh := func(current Account) (Account, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return Account{AccessToken: "new-one", RefreshToken: "rotated"}, nil
	}
	go func() { _, err := s.Refresh(a.ID, a.AccessToken, refresh); results <- err }()
	<-started
	go func() { _, err := s.Refresh(a.ID, a.AccessToken, refresh); results <- err }()
	independent := make(chan error, 1)
	independentStarted := make(chan struct{})
	go func() {
		_, err := s.Refresh(b.ID, b.AccessToken, func(Account) (Account, error) {
			close(independentStarted)
			return Account{AccessToken: "new-two"}, nil
		})
		independent <- err
	}()
	select {
	case <-independentStarted:
	case err := <-independent:
		close(release)
		t.Fatal("independent refresh failed before its exchange:", err)
	case <-time.After(15 * time.Second):
		close(release)
		t.Fatal("other account refresh blocked behind network")
	}
	_, err := s.UpdateAccountVersion(a.ID, 0, func(a *Account) error { a.Notes = "during network"; return nil })
	if err != nil {
		close(release)
		t.Fatal("metadata transaction blocked behind refresh:", err)
	}
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if err := <-independent; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh callback ran %d times", calls.Load())
	}
	updated, _ := s.Get(a.ID)
	if updated.Notes != "during network" || updated.AccessToken != "new-one" {
		t.Fatal("refresh overwrote metadata or lost credentials")
	}
}

func TestPGEnsureReadyRefreshesDifferentAccountsConcurrently(t *testing.T) {
	s := accountPGTestStore(t)
	a := pgImportForTest(t, s, "one", "expired-one")
	b := pgImportForTest(t, s, "two", "expired-two")
	for _, account := range []*Account{&a, &b} {
		account.ExpiresAt = time.Now().Add(-time.Hour).Unix()
		if err := s.UpdateTokens(account.ID, account.AccessToken, *account); err != nil {
			t.Fatal(err)
		}
	}
	// The caller may hold an account snapshot from before a display-name edit.
	if _, err := s.UpdateAccountVersion(a.ID, 0, func(account *Account) error { account.Name = "renamed-one"; return nil }); err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 2)
	release := make(chan struct{})
	var released atomic.Bool
	releaseExchanges := func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}
	defer releaseExchanges()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		started <- payload["refresh_token"]
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"synthetic-ready-refreshed","refresh_token":"synthetic-ready-rotated","expires_in":3600}`)
	}))
	defer upstream.Close()
	defer releaseExchanges()
	p := &Proxy{Store: s, Client: upstream.Client(), TokenURL: upstream.URL}
	results := make(chan error, 2)
	go func() { _, err := p.ensureReady(context.Background(), a); results <- err }()
	select {
	case <-started:
	case err := <-results:
		t.Fatalf("first ensureReady did not start OAuth: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("first OAuth did not start")
	}
	go func() { _, err := p.ensureReady(context.Background(), b); results <- err }()
	select {
	case <-started:
	case err := <-results:
		releaseExchanges()
		t.Fatalf("second ensureReady did not start OAuth: %v", err)
	case <-time.After(15 * time.Second):
		releaseExchanges()
		t.Fatal("ensureReady blocked another account behind the first OAuth exchange")
	}
	releaseExchanges()
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	for _, account := range []Account{a, b} {
		stored, err := s.Get(account.ID)
		if err != nil || stored.AccessToken != "synthetic-ready-refreshed" || stored.RefreshToken != "synthetic-ready-rotated" {
			t.Fatal("ensureReady did not persist refreshed credentials by stable ID")
		}
	}
}

func TestPGAccountRefreshCASAndRestartRecovery(t *testing.T) {
	s := accountPGTestStore(t)
	a := pgImportForTest(t, s, "one", "old")
	_, err := s.Refresh(a.ID, a.AccessToken, func(Account) (Account, error) {
		report, err := s.ImportDetailed([]ImportItem{{Name: a.Name, Credentials: json.RawMessage(`{"access_token":"reimported"}`)}}, AccountDefaults{}, ImportOptions{Replace: true})
		if err != nil || report.Replaced != 1 {
			t.Fatalf("replacement failed: %+v %v", report, err)
		}
		return Account{AccessToken: "stale-refresh"}, nil
	})
	if !errors.Is(err, ErrTokenConflict) {
		t.Fatalf("stale credential refresh was accepted: %v", err)
	}
	updated, err := s.UpdateAccountVersion(a.ID, 0, func(a *Account) error {
		a.CredentialStatus = "failed"
		a.LastCheckedAt = 1700000000
		a.LastError = "check failed"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	reopened := Store{DB: s.DB}
	restored, err := reopened.Get(a.ID)
	if err != nil || restored.AccessToken != "reimported" || restored.CredentialStatus != "failed" || restored.LastCheckedAt != updated.LastCheckedAt || restored.LastError != "check failed" {
		t.Fatalf("persistent state did not recover: %v", err)
	}
}

func TestPGAccountRefreshFailureSurvivesRestartAndDoesNotMarkReplacement(t *testing.T) {
	s := accountPGTestStore(t)
	a := pgImportForTest(t, s, "one", "before-failure")
	failure := errors.New("synthetic upstream failure")
	_, err := s.Refresh(a.ID, a.AccessToken, func(Account) (Account, error) { return Account{}, failure })
	if !errors.Is(err, failure) {
		t.Fatal("refresh failure was not returned")
	}
	a, err = (Store{DB: s.DB}).Get(a.ID)
	if err != nil || a.CredentialStatus != "refresh_failed" || a.LastCheckedAt == 0 || a.LastError != "credential refresh failed" {
		t.Fatalf("refresh failure state was not restored: %v", err)
	}
	_, err = s.Refresh(a.ID, a.AccessToken, func(Account) (Account, error) {
		report, err := s.ImportDetailed([]ImportItem{{Name: a.Name, Credentials: json.RawMessage(`{"access_token":"replacement-after-failure"}`)}}, AccountDefaults{}, ImportOptions{Replace: true})
		if err != nil || report.Replaced != 1 {
			t.Fatal("replacement failed:", err)
		}
		return Account{}, failure
	})
	if !errors.Is(err, failure) {
		t.Fatal("second refresh failure was not returned")
	}
	replaced, err := s.Get(a.ID)
	if err != nil || replaced.AccessToken != "replacement-after-failure" || replaced.CredentialStatus != "unverified" || replaced.LastError != "" {
		t.Fatal("stale refresh failure overwrote successful replacement")
	}
}

func TestPGAccountImportConflictAndDryRun(t *testing.T) {
	s := accountPGTestStore(t)
	items := []ImportItem{{Name: "one", Credentials: json.RawMessage(`{"access_token":"token-a","account_id":"shared"}`)}, {Name: "one", Credentials: json.RawMessage(`{"access_token":"token-b","account_id":"shared"}`)}, {Name: "two", Credentials: json.RawMessage(`{"access_token":"token-b","account_id":"shared"}`)}, {Name: "duplicate", Credentials: json.RawMessage(`{"access_token":"token-a"}`)}}
	for _, dry := range []bool{true, false} {
		report, err := s.ImportDetailed(items, AccountDefaults{}, ImportOptions{DryRun: dry})
		if err != nil || report.Added != 2 || report.Duplicate != 1 || report.Failed != 1 {
			t.Fatalf("dry=%v report=%+v err=%v", dry, report, err)
		}
		if dry {
			_, all, err := s.ListMetadata()
			if err != nil || len(all) != 0 {
				t.Fatal("dry run persisted rows")
			}
		}
	}
	_, all, err := s.ListMetadata()
	if err != nil || len(all) != 2 || all[0].ID == all[1].ID {
		t.Fatal("workspace account ID incorrectly deduplicated accounts")
	}
}
