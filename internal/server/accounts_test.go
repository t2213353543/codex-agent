package server

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func accountTestStore(t *testing.T) Store {
	t.Helper()
	return Store{Path: filepath.Join(t.TempDir(), "private", "accounts.json")}
}

func accountTestImport(t *testing.T, store Store, name, token string) {
	t.Helper()
	if err := store.Import(name, []byte(fmt.Sprintf(`{"access_token":%q}`, token))); err != nil {
		t.Fatal(err)
	}
}

func TestAccountsCRUD(t *testing.T) {
	store := accountTestStore(t)
	active, accounts, err := store.List()
	if err != nil || active != "" || len(accounts) != 0 {
		t.Fatalf("new store: active=%q accounts=%v err=%v", active, accounts, err)
	}
	if _, err := os.Stat(store.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading an absent store created it: %v", err)
	}
	if _, err := store.Active(); err == nil {
		t.Fatal("empty store has an active account")
	}
	accountTestImport(t, store, "first", "token-1")
	accountTestImport(t, store, "second", "token-2")
	account, err := store.Active()
	if err != nil || account.Name != "first" {
		t.Fatalf("first import was not selected: %+v %v", account, err)
	}
	accountTestImport(t, store, "second", "token-2-new")
	active, accounts, err = store.List()
	if err != nil || active != "first" || len(accounts) != 2 || accounts[1].AccessToken != "token-2-new" {
		t.Fatalf("replacement changed selection or appended: %q %+v %v", active, accounts, err)
	}
	if err := store.Use("second"); err != nil {
		t.Fatal(err)
	}
	if err := store.Use("missing"); err == nil {
		t.Fatal("selected a missing account")
	}
	if err := store.Delete("missing"); err == nil {
		t.Fatal("deleted a missing account")
	}
	if err := store.Delete("second"); err != nil {
		t.Fatal(err)
	}
	active, accounts, err = store.List()
	if err != nil || active != "" || len(accounts) != 1 {
		t.Fatalf("delete selected another account: %q %+v %v", active, accounts, err)
	}
	accountTestImport(t, store, "third", "token-3")
	if _, err := store.Active(); err == nil {
		t.Fatal("an import selected an account after the active account was deleted")
	}
	info, err := os.Stat(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("unsafe store permissions: %v", info.Mode())
	}
	if _, err := os.Stat(store.Path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock was not released: %v", err)
	}
}

func TestAccountImportFormats(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1900000000,"https://api.openai.com/auth":{"chatgpt_account_id":"jwt-id"}}`))
	jwt := "header." + payload + ".signature"
	tests := []struct {
		name      string
		raw       string
		accountID string
		expiresAt int64
		refresh   string
		client    string
	}{
		{"flat", `{"access_token":"token","refresh_token":"refresh","account_id":"flat-id","client_id":"client","expires_at":1900000000}`, "flat-id", 1900000000, "refresh", "client"},
		{"codex", `{"tokens":{"access_token":"token","refresh_token":"refresh","account_id":"codex-id"}}`, "codex-id", 0, "refresh", ""},
		{"sub2api", `{"name":"upstream","credentials":{"access_token":"token","chatgpt_account_id":"sub-id","expires_at":"1900000000"}}`, "sub-id", 1900000000, "", ""},
		{"rfc3339", `{"access_token":"token","expires_at":"2030-03-17T17:46:40Z"}`, "", 1900000000, "", ""},
		{"jwt", fmt.Sprintf(`{"access_token":%q}`, jwt), "jwt-id", 1900000000, "", ""},
		{"explicit-beats-jwt", fmt.Sprintf(`{"access_token":%q,"account_id":"explicit-id","expires_at":2000000000}`, jwt), "explicit-id", 2000000000, "", ""},
		{"nested-root-expiry", `{"tokens":{"access_token":"token"},"expires_at":1900000000}`, "", 1900000000, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := accountTestStore(t)
			if err := store.Import("local-name", []byte(tt.raw)); err != nil {
				t.Fatal(err)
			}
			account, err := store.Active()
			if err != nil || account.Name != "local-name" || account.AccountID != tt.accountID || account.ExpiresAt != tt.expiresAt || account.RefreshToken != tt.refresh || account.ClientID != tt.client {
				t.Fatalf("unexpected imported account: %+v, %v", account, err)
			}
		})
	}
}

func TestAccountImportRejectsInvalidData(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `"token"`, `{}`, `{"access_token":""}`, `{"access_token":"  "}`,
		`{"access_token":null}`, `{"access_token":123}`, `{"tokens":null}`, `{"tokens":[]}`,
		`{"credentials":{"refresh_token":"refresh"}}`, `{"access_token":"token","expires_at":"yesterday"}`,
		`{"access_token":"token","expires_at":true}`, `{"access_token":"token"} {}`,
	} {
		t.Run(raw, func(t *testing.T) {
			store := accountTestStore(t)
			if err := store.Import("name", []byte(raw)); err == nil {
				t.Fatal("invalid account accepted")
			}
		})
	}
	for _, name := range []string{"", "  ", strings.Repeat("a", 101), "a\nb", "a\rb", "a\x00b", "a\tb", "a\u2028b"} {
		if err := accountTestStore(t).Import(name, []byte(`{"access_token":"token"}`)); err == nil {
			t.Fatalf("invalid name accepted: %q", name)
		}
	}
}

func TestAccountUpdateTokens(t *testing.T) {
	store := accountTestStore(t)
	if err := store.Import("name", []byte(`{"access_token":"old","refresh_token":"refresh","account_id":"id","client_id":"client"}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTokens("name", "old", Account{Name: "changed", AccessToken: "new", ExpiresAt: 1900000000}); err != nil {
		t.Fatal(err)
	}
	account, err := store.Active()
	if err != nil || account.Name != "name" || account.AccessToken != "new" || account.RefreshToken != "refresh" || account.AccountID != "id" || account.ClientID != "client" || account.ExpiresAt != 1900000000 {
		t.Fatalf("refresh did not preserve metadata: %+v %v", account, err)
	}
	if err := store.UpdateTokens("name", "old", Account{AccessToken: "stale"}); !errors.Is(err, ErrTokenConflict) {
		t.Fatalf("stale update was not rejected: %v", err)
	}
	if err := store.UpdateTokens("name", "new", Account{}); err == nil {
		t.Fatal("empty refreshed token accepted")
	}
	if err := store.UpdateTokens("name", "new", Account{AccessToken: "newer", AccountID: "new-id", RefreshToken: "new-refresh"}); err != nil {
		t.Fatal(err)
	}
	account, err = store.Active()
	if err != nil || account.AccountID != "new-id" || account.RefreshToken != "new-refresh" {
		t.Fatalf("new metadata not applied: %+v %v", account, err)
	}
	if err := store.Delete("name"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTokens("name", "newer", Account{AccessToken: "resurrected"}); !errors.Is(err, ErrTokenConflict) {
		t.Fatalf("deleted account update was not rejected: %v", err)
	}
}

func TestAccountsConcurrentMutations(t *testing.T) {
	store := accountTestStore(t)
	var workers sync.WaitGroup
	errorsCh := make(chan error, 12)
	for i := 0; i < cap(errorsCh); i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			errorsCh <- store.Import(fmt.Sprintf("account-%d", i), []byte(`{"access_token":"token"}`))
		}(i)
	}
	workers.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	active, accounts, err := store.List()
	if err != nil || active == "" || len(accounts) != 12 {
		t.Fatalf("concurrent imports lost accounts: %q %d %v", active, len(accounts), err)
	}
	results := make(chan error, 2)
	for _, token := range []string{"new-a", "new-b"} {
		go func(token string) {
			results <- store.UpdateTokens(active, "token", Account{AccessToken: token})
		}(token)
	}
	first, second := <-results, <-results
	if !((first == nil && errors.Is(second, ErrTokenConflict)) || (second == nil && errors.Is(first, ErrTokenConflict))) {
		t.Fatalf("competing token updates were not serialized: %v, %v", first, second)
	}
}

func TestAccountsLockWaitAndStale(t *testing.T) {
	store := accountTestStore(t)
	accountTestImport(t, store, "account", "token")
	if err := os.Mkdir(store.Path+".lock", 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.List(); err != nil {
		t.Fatalf("a writer lock blocked a read: %v", err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(75 * time.Millisecond)
		released <- os.Remove(store.Path + ".lock")
	}()
	if err := store.Use("account"); err != nil {
		t.Fatal(err)
	}
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(store.Path+".lock", 0700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-accountLockStale - time.Minute)
	if err := os.Chtimes(store.Path+".lock", old, old); err != nil {
		t.Fatal(err)
	}
	if err := store.Use("account"); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale lock not reported: %v", err)
	}
	if _, err := os.Stat(store.Path + ".lock"); err != nil {
		t.Fatalf("stale lock was removed: %v", err)
	}
}

func TestAccountsConcurrentRefresh(t *testing.T) {
	store := accountTestStore(t)
	accountTestImport(t, store, "account", "old")
	var calls atomic.Int32
	refresh := func(current Account) (Account, error) {
		calls.Add(1)
		time.Sleep(75 * time.Millisecond)
		return Account{AccessToken: "new", RefreshToken: "rotated"}, nil
	}
	type refreshResult struct {
		account Account
		err     error
	}
	results := make(chan refreshResult, 2)
	for _, instance := range []Store{store, {Path: store.Path}} {
		go func(instance Store) {
			account, err := instance.Refresh("account", "old", refresh)
			results <- refreshResult{account, err}
		}(instance)
	}
	for i := 0; i < 2; i++ {
		result := <-results
		if result.err != nil || result.account.Name != "account" || result.account.AccessToken != "new" || result.account.RefreshToken != "rotated" {
			t.Fatalf("concurrent refresh failed: %+v %v", result.account, result.err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh callback ran %d times", calls.Load())
	}
	accountTestImport(t, store, "account", "reimported")
	account, err := store.Refresh("account", "new", refresh)
	if err != nil || account.AccessToken != "reimported" || calls.Load() != 1 {
		t.Fatalf("refresh overwrote a reimported account: %+v %v", account, err)
	}
}

func TestAccountsRefreshFailurePreservesTokens(t *testing.T) {
	store := accountTestStore(t)
	accountTestImport(t, store, "account", "old")
	failure := errors.New("exchange failed")
	if _, err := store.Refresh("account", "old", func(Account) (Account, error) {
		return Account{}, failure
	}); !errors.Is(err, failure) {
		t.Fatalf("refresh failure was not propagated: %v", err)
	}
	if _, err := store.Refresh("account", "old", func(Account) (Account, error) {
		return Account{}, nil
	}); err == nil {
		t.Fatal("empty refreshed token accepted")
	}
	account, err := store.Active()
	if err != nil || account.AccessToken != "old" {
		t.Fatalf("failed refresh changed token: %+v %v", account, err)
	}
	if _, err := os.Stat(store.Path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed refresh leaked lock: %v", err)
	}
}

func TestAccountsLockTimeout(t *testing.T) {
	store := accountTestStore(t)
	accountTestImport(t, store, "account", "token")
	if err := os.Mkdir(store.Path+".lock", 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.Use("account"); err == nil || !strings.Contains(err.Error(), "is locked") {
		t.Fatalf("busy lock was not reported: %v", err)
	}
}

func TestAccountsCorruptStoreIsNotOverwritten(t *testing.T) {
	store := accountTestStore(t)
	accountTestImport(t, store, "account", "token")
	for _, raw := range []string{`null`, `[]`, `{`, `{"active":"missing","accounts":[]}`, `{"accounts":[{"name":"one","access_token":""}]}`} {
		if err := os.WriteFile(store.Path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if err := store.Import("another", []byte(`{"access_token":"new"}`)); err == nil {
			t.Fatalf("corrupt store was accepted: %s", raw)
		}
		got, err := os.ReadFile(store.Path)
		if err != nil || string(got) != raw {
			t.Fatalf("corrupt store was overwritten: %q %v", got, err)
		}
	}
}
