package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// Only account-group metadata changes cross instances here. Key indexes are
// explicitly synchronized before admission; this is not key-cache revocation.
func TestPGGatewayBoundAccountGroupRevocationWithWarmCache(t *testing.T) {
	writer, reader, adminToken := adminKeyGatewayPGProxies(t)
	redisDB, _ := gatewayRedisSessionFixture(t)
	writer.Store.DB.Redis, writer.Store.DB.Namespace = redisDB.Redis, redisDB.Namespace
	groups := NewPGGroupStore(writer.Store.DB)
	first, err := groups.Create("bound access first", "synthetic group")
	if err != nil {
		t.Fatal("cannot create first persisted group")
	}
	second, err := groups.Create("bound access second", "synthetic group")
	if err != nil {
		t.Fatal("cannot create second persisted group")
	}
	shared, err := groups.Create("bound access shared", "synthetic group")
	if err != nil {
		t.Fatal("cannot create shared persisted group")
	}
	writer.Keys, err = NewPGKeyStore(writer.Store.DB)
	if err != nil {
		t.Fatal("cannot initialize writer key store")
	}
	reader.Keys, err = NewPGKeyStore(reader.Store.DB)
	if err != nil {
		t.Fatal("cannot initialize independent reader key index")
	}
	key, err := writer.Keys.Create("bound access key", []string{first.ID, shared.ID})
	if err != nil {
		t.Fatal("cannot create synthetic grouped gateway key")
	}
	// This one explicit refresh makes the regular key known to the reader.
	// No later key mutation or index refresh participates in the test.
	if _, err := reader.Keys.List(); err != nil {
		t.Fatal("cannot synchronize initial reader key index")
	}
	credentials, _ := json.Marshal(map[string]any{"access_token": "synthetic-bound-group-access", "refresh_token": "synthetic-bound-group-refresh",
		"chatgpt_account_id": "synthetic-bound-workspace", "chatgpt_user_id": "synthetic-bound-user", "expires_at": time.Now().Add(time.Hour).Unix()})
	if err := writer.Store.ImportWith("bound-group-account", credentials, AccountDefaults{Concurrency: 2, Groups: []string{first.ID, shared.ID}}); err != nil {
		t.Fatal("cannot import synthetic account with valid persisted group relations")
	}
	original, err := writer.Store.Get("bound-group-account")
	if err != nil || original.OwnerRevision == "" {
		t.Fatal("cannot read initial account ownership")
	}
	const session = "bound-group-session"
	const adminSession = "bound-group-admin-session"
	for _, binding := range []struct{ keyID, session string }{{key.ID, session}, {"admin", adminSession}} {
		threadKey := fmt.Sprintf("%sgateway:thread:%x", redisDB.Namespace, sha256.Sum256([]byte(binding.keyID+"\x00"+binding.session)))
		t.Cleanup(func() { _ = redisDB.Redis.Del(context.Background(), threadKey).Err() })
	}
	selectBound := func(token, keyID, thread string, allowed bool) Account {
		t.Helper()
		account, release, err := reader.pickSessionAccount(t.Context(), token, keyID, thread)
		if release != nil {
			release()
		}
		if allowed && (err != nil || gatewayAccountRef(account) != gatewayAccountRef(original)) {
			t.Fatal("current allowed account could not retain its original bound session")
		}
		if !allowed && err == nil {
			t.Fatal("bound HTTP admission accepted revoked account-group access from warm scheduling cache")
		}
		if reader.inFlight(gatewayAdmissionKey(original)) != 0 {
			t.Fatal("bound authorization check leaked an admission slot")
		}
		return account
	}
	selectBound(key.Key, key.ID, session, true)
	selectBound(adminToken, "admin", adminSession, true)
	reader.accountsMu.Lock()
	warmed := append([]Account(nil), reader.accounts...)
	reader.accountsMu.Unlock()
	if len(warmed) != 1 || !reflect.DeepEqual(warmed[0].Groups, original.Groups) {
		t.Fatal("reader did not establish the original grouped scheduling snapshot")
	}
	keepOriginalCacheWarm := func() {
		t.Helper()
		reader.accountsMu.Lock()
		defer reader.accountsMu.Unlock()
		if len(reader.accounts) != 1 || !reflect.DeepEqual(reader.accounts[0].Groups, original.Groups) {
			t.Fatal("test no longer exercises stale original account-group cache")
		}
		// Keep the same already-observed snapshot inside its existing TTL even
		// under slow race-enabled PostgreSQL. Never replace its account values.
		reader.accountsAt = time.Now()
	}
	updateGroups := func(groupIDs []string, enabled bool) {
		t.Helper()
		if err := writer.Store.UpdateAccount(original.ID, func(account *Account) error {
			account.Groups = append([]string(nil), groupIDs...)
			account.Enabled = enabled
			return nil
		}); err != nil {
			t.Fatal("persisted account-group mutation failed")
		}
		current, err := writer.Store.Get(original.ID)
		if err != nil || current.OwnerRevision != original.OwnerRevision || gatewayAccountRef(current) != gatewayAccountRef(original) {
			t.Fatal("group metadata mutation incorrectly changed account ownership revision")
		}
	}
	assertBindingPreserved := func() {
		t.Helper()
		for _, binding := range []struct{ keyID, session string }{{key.ID, session}, {"admin", adminSession}} {
			bound, err := reader.sessionAccount(t.Context(), binding.keyID, binding.session, "")
			if err != nil || bound != gatewayAccountRef(original) {
				t.Fatal("account-group authorization silently migrated an existing Redis binding")
			}
		}
	}
	updateGroups([]string{second.ID}, true)
	keepOriginalCacheWarm()
	selectBound(key.Key, key.ID, session, false)
	if _, err := reader.authorizedGatewayWSAccount(key.Key, key.ID, original); err == nil {
		t.Fatal("WS next-turn authorization accepted a removed account group from warm cache")
	}
	assertBindingPreserved()
	// A currently valid admin key bypasses groups, but not account enabled or
	// ownership checks. This does not update the ordinary key's cached grants.
	keepOriginalCacheWarm()
	selectBound(adminToken, "admin", adminSession, true)
	if _, err := reader.authorizedGatewayWSAccount(adminToken, "admin", original); err != nil {
		t.Fatal("admin lost group-independent access to the enabled original owner")
	}
	updateGroups([]string{second.ID, shared.ID}, true)
	keepOriginalCacheWarm()
	selectBound(key.Key, key.ID, session, true)
	if _, err := reader.authorizedGatewayWSAccount(key.Key, key.ID, original); err != nil {
		t.Fatal("another current allowed-group intersection was incorrectly rejected")
	}
	updateGroups([]string{first.ID}, true)
	keepOriginalCacheWarm()
	selectBound(key.Key, key.ID, session, true)
	if _, err := reader.authorizedGatewayWSAccount(key.Key, key.ID, original); err != nil {
		t.Fatal("restoring group access did not restore the unchanged owner's original session")
	}
	assertBindingPreserved()
	updateGroups([]string{first.ID}, false)
	keepOriginalCacheWarm()
	selectBound(key.Key, key.ID, session, false)
	if _, err := reader.authorizedGatewayWSAccount(key.Key, key.ID, original); err == nil {
		t.Fatal("disabled account retained WS access through allowed groups")
	}
	selectBound(adminToken, "admin", adminSession, false)
	if _, err := reader.authorizedGatewayWSAccount(adminToken, "admin", original); err == nil {
		t.Fatal("admin group exception bypassed disabled-account rejection")
	}
	assertBindingPreserved()
	if reader.inFlight(gatewayAdmissionKey(original)) != 0 {
		t.Fatal("revocation/restore/disable sequence left admission in flight")
	}
}
