package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func gatewayRevisionCredentials(t *testing.T, user, generation string) []byte {
	t.Helper()
	claims := map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "fixture_generation": generation,
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "test-account"}}
	if user != "" {
		claims["https://api.openai.com/auth"].(map[string]any)["chatgpt_user_id"] = user
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	token := "fixture." + base64.RawURLEncoding.EncodeToString(payload) + ".synthetic"
	raw, err := json.Marshal(map[string]any{"name": "test", "access_token": token, "refresh_token": "synthetic-refresh-" + generation,
		"chatgpt_account_id": "test-account", "client_id": "synthetic-client", "owner_revision": "synthetic-untrusted-owner-revision"})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func gatewayRevisionImport(t *testing.T, store Store, raw []byte) Account {
	t.Helper()
	items, err := ParseImportInput(raw)
	if err != nil {
		t.Fatal("synthetic owner import parse failed")
	}
	report, err := store.ImportDetailed(items, AccountDefaults{Concurrency: 2}, ImportOptions{Replace: true})
	if err != nil || report.Failed != 0 || report.Added+report.Replaced+report.Duplicate != 1 {
		t.Fatal("synthetic owner import write failed")
	}
	account, err := store.Get("test")
	if err != nil {
		t.Fatal("synthetic owner import read failed")
	}
	return account
}

func gatewayRevisionAssertOldRejected(t *testing.T, p *Proxy, token, session string, original Account) {
	t.Helper()
	p.invalidateAccounts()
	_, release, err := p.pickSessionAccount(t.Context(), token, "admin", session)
	if release != nil {
		release()
	}
	if err == nil {
		t.Error("persisted A-to-B-to-A account history revived the original thread")
	}
	if _, err := p.authorizedGatewayWSAccount(token, "admin", original); err == nil {
		t.Error("persisted A-to-B-to-A account history revived original WS authorization")
	}
	if bound, err := p.sessionAccount(t.Context(), "admin", session, ""); err != nil || bound != gatewayAccountRef(original) {
		t.Error("owner revision silently rewrote the original encrypted-history binding")
	}
}

func TestGatewayOwnerRevisionImportABASurvivesJSONStoreReopen(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(fmt.Sprintf("known=%v", known), func(t *testing.T) {
			p := newTestProxy(t, 2)
			userA, userB := "", ""
			if known {
				userA, userB = "synthetic-user-a", "synthetic-user-b"
			}
			originalRaw := gatewayRevisionCredentials(t, userA, "a")
			original := gatewayRevisionImport(t, p.Store, originalRaw)
			if original.OwnerRevision == "" || !validGatewayOwnerRevision(original.OwnerRevision) {
				t.Fatal("new import did not persist a server-generated owner revision")
			}
			const session = "owner-revision-json-thread"
			p.invalidateAccounts()
			_, release, err := p.pickSessionAccount(t.Context(), "test-client-key", "admin", session)
			if err != nil {
				t.Fatal("could not establish initial owner thread")
			}
			release()
			if err := p.Store.UpdateAccount(original.ID, func(account *Account) error {
				account.Notes = "metadata-only"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			reopened := Store{Path: p.Store.Path}
			metadata, err := reopened.Get(original.ID)
			if err != nil || gatewayAccountRef(metadata) != gatewayAccountRef(original) {
				t.Fatal("metadata edit changed owner after disk reopen")
			}
			if known {
				rotated := gatewayRevisionImport(t, reopened, gatewayRevisionCredentials(t, userA, "a-rotated"))
				if gatewayAccountRef(rotated) != gatewayAccountRef(original) {
					t.Fatal("same known user/workspace credential replacement changed owner")
				}
			}
			changed := gatewayRevisionImport(t, reopened, gatewayRevisionCredentials(t, userB, "b"))
			if gatewayAccountRef(changed) == gatewayAccountRef(original) {
				t.Fatal("A-to-B transition was not recognized")
			}
			if changed.OwnerRevision == original.OwnerRevision {
				t.Fatal("stored owner transition reused its earlier revision")
			}
			// New Store values force persisted JSON reads at both transition
			// boundaries. The gateway still holds its original thread binding.
			reopened = Store{Path: p.Store.Path}
			restored := gatewayRevisionImport(t, reopened, originalRaw)
			p.Store = Store{Path: p.Store.Path}
			persisted, err := p.Store.Get(original.ID)
			if err != nil || persisted.AccessToken != original.AccessToken || persisted.RefreshToken != original.RefreshToken || restored.ID != original.ID {
				t.Fatal("ABA fixture did not restore the exact stored credentials")
			}
			if gatewayAccountRef(persisted) == gatewayAccountRef(original) {
				t.Error("JSON persisted owner reference returned to its original value after ABA")
			}
			if persisted.OwnerRevision == original.OwnerRevision || persisted.OwnerRevision == changed.OwnerRevision {
				t.Error("restored credentials reused an earlier persisted owner revision")
			}
			gatewayRevisionAssertOldRejected(t, p, "test-client-key", session, original)
		})
	}
}

func TestPGGatewayOwnerRevisionReauthorizeABASurvivesFreshGateway(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(fmt.Sprintf("known=%v", known), func(t *testing.T) {
			p, _, token := adminKeyGatewayPGProxies(t)
			redisDB, _ := gatewayRedisSessionFixture(t)
			p.Store.DB.Redis, p.Store.DB.Namespace = redisDB.Redis, redisDB.Namespace
			userA, userB := "", ""
			if known {
				userA, userB = "synthetic-user-a", "synthetic-user-b"
			}
			originalRaw := gatewayRevisionCredentials(t, userA, "a")
			original := gatewayRevisionImport(t, p.Store, originalRaw)
			const session = "owner-revision-pg-thread"
			threadKey := fmt.Sprintf("%sgateway:thread:%x", redisDB.Namespace, sha256.Sum256([]byte("admin\x00"+session)))
			t.Cleanup(func() { _ = redisDB.Redis.Del(context.Background(), threadKey).Err() })
			_, release, err := p.pickSessionAccount(t.Context(), token, "admin", session)
			if err != nil {
				t.Fatal("could not bind initial persisted owner")
			}
			release()
			reauthorize := func(raw []byte, allow bool) Account {
				current, err := (Store{DB: p.Store.DB}).Get(original.ID)
				if err != nil {
					t.Fatal(err)
				}
				incoming, err := parseAccount("test", raw)
				if err != nil {
					t.Fatal("synthetic CAS credentials invalid")
				}
				if _, err = p.Store.reauthorizeManagedAccount(t.Context(), current.ID, current.Version, incoming, allow); err != nil {
					t.Fatal("synthetic owner CAS failed")
				}
				next, err := (Store{DB: p.Store.DB}).Get(original.ID)
				if err != nil {
					t.Fatal(err)
				}
				return next
			}
			if err := p.Store.UpdateAccount(original.ID, func(account *Account) error { account.Notes = "metadata-only"; return nil }); err != nil {
				t.Fatal(err)
			}
			metadata, err := (Store{DB: p.Store.DB}).Get(original.ID)
			if err != nil || gatewayAccountRef(metadata) != gatewayAccountRef(original) {
				t.Fatal("PG metadata edit changed owner")
			}
			staleIncoming, err := parseAccount("test", gatewayRevisionCredentials(t, userB, "stale-cas"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Store.reauthorizeManagedAccount(t.Context(), metadata.ID, metadata.Version-1, staleIncoming, true); !errors.Is(err, errVersionConflict) {
				t.Fatal("stale owner replacement CAS was not rejected")
			}
			afterStale, err := (Store{DB: p.Store.DB}).Get(original.ID)
			if err != nil || afterStale.OwnerRevision != metadata.OwnerRevision || afterStale.AccessToken != metadata.AccessToken {
				t.Fatal("failed CAS changed persisted credentials or owner revision")
			}
			if known && gatewayAccountRef(reauthorize(gatewayRevisionCredentials(t, userA, "a-rotated"), false)) != gatewayAccountRef(original) {
				t.Fatal("same known owner CAS changed owner reference")
			}
			if gatewayAccountRef(reauthorize(gatewayRevisionCredentials(t, userB, "b"), true)) == gatewayAccountRef(original) {
				t.Fatal("PG A-to-B transition was not recognized")
			}
			restored := reauthorize(originalRaw, true)
			if restored.AccessToken != original.AccessToken || restored.RefreshToken != original.RefreshToken {
				t.Fatal("PG ABA fixture did not restore exact credentials")
			}
			fresh := &Proxy{Store: Store{DB: p.Store.DB}, Settings: NewPGSettingsStore(p.Store.DB), AdminKey: token, startedAt: time.Now()}
			if gatewayAccountRef(restored) == gatewayAccountRef(original) {
				t.Error("PG persisted owner reference returned to its original value after ABA")
			}
			gatewayRevisionAssertOldRejected(t, fresh, token, session, original)
		})
	}
}

func TestGatewayOwnerRevisionInPlaceIdentityMutationAndRollback(t *testing.T) {
	p := newTestProxy(t, 2)
	original := gatewayRevisionImport(t, p.Store, gatewayRevisionCredentials(t, "synthetic-user-a", "identity-original"))
	const session = "owner-revision-in-place-thread"
	p.invalidateAccounts()
	_, release, err := p.pickSessionAccount(t.Context(), "test-client-key", "admin", session)
	if err != nil {
		t.Fatal("could not bind original in-place identity")
	}
	release()
	aborted := errors.New("synthetic aborted mutation")
	if err := p.Store.UpdateAccount(original.ID, func(account *Account) error {
		account.Identity.UserID = "synthetic-aborted-user"
		return aborted
	}); !errors.Is(err, aborted) {
		t.Fatal("aborted mutation did not propagate its error")
	}
	unchanged, err := (Store{Path: p.Store.Path}).Get(original.ID)
	if err != nil || gatewayAccountRef(unchanged) != gatewayAccountRef(original) {
		t.Fatal("failed in-place mutation changed persisted owner")
	}
	// Mutate the existing pointer rather than assigning a fresh Identity. The
	// persistence hook needs a deep prior snapshot to observe this transition.
	for _, user := range []string{"synthetic-user-b", "synthetic-user-a"} {
		if err := p.Store.UpdateAccount(original.ID, func(account *Account) error {
			account.Identity.UserID = user
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	p.Store = Store{Path: p.Store.Path}
	current, err := p.Store.Get(original.ID)
	if err != nil || current.AccessToken != original.AccessToken {
		t.Fatal("in-place identity fixture changed credentials unexpectedly")
	}
	if gatewayAccountRef(current) == gatewayAccountRef(original) {
		t.Error("in-place identity ABA revived original owner after disk reopen")
	}
	gatewayRevisionAssertOldRejected(t, p, "test-client-key", session, original)
}

func TestGatewayOwnerRevisionStableRefreshLegacyAndRecreation(t *testing.T) {
	t.Run("known_refresh_and_unknown_metadata", func(t *testing.T) {
		for _, user := range []string{"synthetic-known-user", ""} {
			store := accountTestStore(t)
			original := gatewayRevisionImport(t, store, gatewayRevisionCredentials(t, user, "stable"))
			updated, err := store.Refresh(original.ID, original.AccessToken, func(account Account) (Account, error) {
				account.Notes = "metadata-only"
				if user != "" {
					fresh, err := parseAccount("test", gatewayRevisionCredentials(t, user, "refresh-rotated"))
					if err != nil {
						return Account{}, err
					}
					account.AccessToken, account.RefreshToken = fresh.AccessToken, fresh.RefreshToken
				}
				account.OwnerRevision = "ffffffffffffffffffffffffffffffff"
				return account, nil
			})
			if err != nil || updated.OwnerRevision != original.OwnerRevision || gatewayAccountRef(updated) != gatewayAccountRef(original) {
				t.Fatal("same proven owner refresh or unchanged unknown credentials advanced revision")
			}
			persisted, err := (Store{Path: store.Path}).Get(original.ID)
			if err != nil || persisted.OwnerRevision != updated.OwnerRevision {
				t.Fatal("Refresh returned a revision different from committed storage")
			}
		}
	})
	t.Run("legacy_empty_revision_survives_read_and_metadata", func(t *testing.T) {
		store := accountTestStore(t)
		legacy, err := parseAccount("test", gatewayRevisionCredentials(t, "", "legacy"))
		if err != nil {
			t.Fatal(err)
		}
		legacy.ID = "synthetic-legacy-id"
		raw, err := json.Marshal(accountState{Active: legacy.Name, Accounts: []Account{legacy}})
		if err != nil || os.MkdirAll(filepath.Dir(store.Path), 0700) != nil || os.WriteFile(store.Path, raw, 0600) != nil {
			t.Fatal("could not write synthetic pre-revision store fixture")
		}
		read, err := store.Get(legacy.ID)
		if err != nil || read.OwnerRevision != "" {
			t.Fatal("read alone changed a legacy owner generation")
		}
		if err := store.UpdateAccount(legacy.ID, func(account *Account) error { account.Notes = "legacy metadata"; return nil }); err != nil {
			t.Fatal(err)
		}
		again, err := (Store{Path: store.Path}).Get(legacy.ID)
		if err != nil || again.OwnerRevision != "" || gatewayAccountRef(again) != gatewayAccountRef(read) {
			t.Fatal("metadata edit invented a legacy owner generation")
		}
	})
	t.Run("foreign_import_revision_and_same_id_recreation", func(t *testing.T) {
		store := accountTestStore(t)
		originalRaw := gatewayRevisionCredentials(t, "synthetic-known-user", "incarnation")
		original := gatewayRevisionImport(t, store, originalRaw)
		var imported map[string]any
		if json.Unmarshal(originalRaw, &imported) != nil {
			t.Fatal("synthetic import fixture invalid")
		}
		imported["owner_revision"] = original.OwnerRevision
		forged, err := json.Marshal(imported)
		if err != nil {
			t.Fatal(err)
		}
		other := gatewayRevisionImport(t, accountTestStore(t), forged)
		if other.OwnerRevision == "" || other.OwnerRevision == original.OwnerRevision {
			t.Fatal("new import accepted a foreign owner revision")
		}
		if err := store.Delete(original.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.ApplyAll(func(state *accountState) error {
			state.Active = original.Name
			state.Accounts = append(state.Accounts, original)
			state.OwnerRevisions = map[string]string{original.ID: original.OwnerRevision}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		recreated, err := (Store{Path: store.Path}).Get(original.ID)
		if err != nil || recreated.ID != original.ID || recreated.AccessToken != original.AccessToken || recreated.OwnerRevision == "" || recreated.OwnerRevision == original.OwnerRevision {
			t.Fatal("same-ID recreation reused the deleted account incarnation")
		}
	})
}
