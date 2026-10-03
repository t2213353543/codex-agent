package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestStoreNoopUpdatesPreserveValuesAndCAS(t *testing.T) {
	exerciseStoreNoopUpdatesPreserveValuesAndCAS(t, webTestSetup)
}

func TestPGStoreNoopUpdatesPreserveValuesAndCAS(t *testing.T) {
	exerciseStoreNoopUpdatesPreserveValuesAndCAS(t, adminPGTestProxy)
}

func exerciseStoreNoopUpdatesPreserveValuesAndCAS(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	group, err := p.Groups.Create("noop-group", "same description")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := p.Proxies.Create(ProxyEntry{Name: "noop-proxy", Type: "http", Host: "127.0.0.1", Port: 4321, Username: "synthetic-user", Password: "synthetic-password"})
	if err != nil {
		t.Fatal(err)
	}
	// PG create's app timestamp can differ by one second from the rounded
	// persisted projection. Compare no-op output to the authoritative row.
	proxy, err = p.Proxies.GetForAdmin(proxy.ID)
	if err != nil {
		t.Fatal(err)
	}
	key, err := p.Keys.Create("noop-key", []string{group.ID})
	if err != nil {
		t.Fatal(err)
	}
	if p.Store.DB == nil {
		if group.Version != 0 || proxy.Version != 0 {
			t.Fatal("fixture must cover legacy zero-version resources")
		}
		group, err = p.Groups.UpdateVersion(group.ID, group.Name, group.Description, 0)
		if err != nil || group.Version != 1 {
			t.Fatalf("legacy group did not establish its first CAS revision: %+v %v", group, err)
		}
		proxy, err = p.Proxies.UpdateVersion(proxy.ID, 0, func(*ProxyEntry) error { return nil })
		if err != nil || proxy.Version != 1 {
			t.Fatalf("legacy proxy did not establish its first CAS revision: %+v %v", proxy, err)
		}
	}
	var beforeAudit int
	if p.Store.DB != nil {
		// Enforce the optimization at the actual storage boundary. An UPDATE
		// that writes the same values still invokes these triggers and fails.
		_, err := p.Store.DB.Pool.Exec(t.Context(), `CREATE FUNCTION reject_noop_resource_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'unexpected resource rewrite'; END $$;
		 CREATE TRIGGER reject_noop_resource_write BEFORE UPDATE ON groups FOR EACH ROW EXECUTE FUNCTION reject_noop_resource_write();
		 CREATE TRIGGER reject_noop_resource_write BEFORE UPDATE ON proxies FOR EACH ROW EXECUTE FUNCTION reject_noop_resource_write();
		 CREATE TRIGGER reject_noop_resource_write BEFORE UPDATE ON settings FOR EACH ROW EXECUTE FUNCTION reject_noop_resource_write()`)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_logs").Scan(&beforeAudit); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string][]byte{}
	if p.Store.DB == nil {
		for _, path := range []string{p.Groups.store.path, p.Proxies.store.path, p.Keys.store.path} {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			files[path] = raw
			// A known old timestamp proves no atomic replacement occurred.
			if err := os.Chtimes(path, time.Unix(1600000000, 0), time.Unix(1600000000, 0)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for range 2 {
		got, err := p.Groups.UpdateVersion(group.ID, group.Name, group.Description, group.Version)
		if err != nil || got != group {
			t.Fatalf("unchanged group mutated metadata: %+v %v", got, err)
		}
		if _, err := p.Groups.UpdateVersion(group.ID, group.Name, group.Description, group.Version+1); !errors.Is(err, errVersionConflict) {
			t.Fatalf("same value bypassed group CAS: %v", err)
		}
		gotProxy, err := p.Proxies.UpdateVersion(proxy.ID, proxy.Version, func(v *ProxyEntry) error { v.Name = proxy.Name; v.Password = proxy.Password; return nil })
		if err != nil || gotProxy != proxy {
			t.Fatalf("unchanged proxy mutated version/password metadata: %+v %v", gotProxy, err)
		}
		if _, err := p.Proxies.UpdateVersion(proxy.ID, proxy.Version+1, func(v *ProxyEntry) error { return nil }); !errors.Is(err, errVersionConflict) {
			t.Fatalf("same value bypassed proxy CAS: %v", err)
		}
		gotKey, err := p.Keys.Update(key.ID, func(v *APIKey) error {
			if v.Version != key.Version {
				return errVersionConflict
			}
			v.Name = key.Name
			v.Groups = append([]string(nil), key.Groups...)
			return nil
		})
		if err != nil || !reflect.DeepEqual(gotKey, key) {
			t.Fatalf("unchanged key mutated document/version: %+v %v", gotKey, err)
		}
		if _, err := p.Keys.Update(key.ID, func(v *APIKey) error { return errVersionConflict }); !errors.Is(err, errVersionConflict) {
			t.Fatalf("no-op comparison ignored callback CAS: %v", err)
		}
	}
	for path, raw := range files {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(raw, after) {
			t.Fatalf("no-op file content changed: %v", err)
		}
		info, err := os.Stat(path)
		if err != nil || info.ModTime().Unix() != 1600000000 {
			t.Fatalf("no-op rewrote resource file: %s %v", path, err)
		}
	}
	if p.Store.DB != nil {
		var audits int
		if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_logs").Scan(&audits); err != nil || audits-beforeAudit != 6 {
			t.Fatalf("accepted no-ops lost audits or stale CAS was audited: delta=%d err=%v", audits-beforeAudit, err)
		}
		// No-op is an accepted command, not permission to hide a persistence
		// failure in its required audit transaction.
		if _, err := p.Store.DB.Pool.Exec(t.Context(), `CREATE TRIGGER reject_noop_audit BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_noop_resource_write()`); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Groups.UpdateVersion(group.ID, group.Name, group.Description, group.Version); err == nil {
			t.Fatal("no-op group hid audit error")
		}
		if _, err := p.Proxies.UpdateVersion(proxy.ID, proxy.Version, func(v *ProxyEntry) error { return nil }); err == nil {
			t.Fatal("no-op proxy hid audit error")
		}
		if _, err := p.Keys.Update(key.ID, func(v *APIKey) error { return nil }); err == nil {
			t.Fatal("no-op key hid audit error")
		}
	}
}

func TestStoreNoopRefreshesAuthoritativeSingleEntryCache(t *testing.T) {
	exerciseStoreNoopRefreshesAuthoritativeSingleEntryCache(t, webTestSetup)
}

func TestPGStoreNoopRefreshesAuthoritativeSingleEntryCache(t *testing.T) {
	exerciseStoreNoopRefreshesAuthoritativeSingleEntryCache(t, adminPGTestProxy)
}

func exerciseStoreNoopRefreshesAuthoritativeSingleEntryCache(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	old, err := p.Proxies.Create(ProxyEntry{Name: "two-store-proxy", Type: "http", Host: "127.0.0.1", Port: 4321, Password: "old-password"})
	if err != nil {
		t.Fatal(err)
	}
	var writer *ProxyStore
	if p.Store.DB == nil {
		writer = NewProxyStore(p.Proxies.store.path)
	} else {
		writer, err = NewPGProxyStore(p.Store.DB)
		if err != nil {
			t.Fatal(err)
		}
	}
	current, err := writer.UpdateVersion(old.ID, old.Version, func(v *ProxyEntry) error { v.Password = "new-password"; v.Host = "127.0.0.2"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Proxies.UpdateVersion(current.ID, current.Version, func(v *ProxyEntry) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	cached, ok := p.Proxies.Get(current.ID)
	if !ok || cached.Password != got.Password || cached.Host != got.Host {
		t.Fatal("successful authoritative no-op left stale runtime proxy settings")
	}
	newer := got
	newer.Version++
	newer.Password = "newer-inflight-password"
	p.Proxies.publishCurrentEntry(newer)
	p.Proxies.publishCurrentEntry(got)
	cached, _ = p.Proxies.Get(current.ID)
	if cached.Version != newer.Version || cached.Password != newer.Password {
		t.Fatal("late no-op publication replaced newer proxy cache")
	}
	group, err := p.Groups.Create("cache-key-group", "")
	if err != nil {
		t.Fatal(err)
	}
	key, err := p.Keys.Create("cache-key", []string{group.ID})
	if err != nil {
		t.Fatal(err)
	}
	var keyWriter *KeyStore
	if p.Store.DB == nil {
		keyWriter = NewKeyStore(p.Keys.store.path)
	} else {
		keyWriter, err = NewPGKeyStore(p.Store.DB)
		if err != nil {
			t.Fatal(err)
		}
	}
	newKey, err := keyWriter.Update(key.ID, func(k *APIKey) error { k.Enabled = false; return nil })
	if err != nil {
		t.Fatal(err)
	}
	returned, err := p.Keys.Update(key.ID, func(k *APIKey) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if returned.Enabled || returned.Version != newKey.Version {
		t.Fatal("key no-op did not read current document")
	}
	if _, ok := p.Keys.Lookup(key.Key); ok {
		t.Fatal("no-op acknowledged disabled key but retained enabled runtime cache")
	}
	newKey.Enabled = true
	newKey.Version++
	newKey.LastUsed = 1800000100
	p.Keys.publishCurrentEntry(newKey)
	returned.Groups[0] = "caller-mutated"
	p.Keys.publishCurrentEntry(key)
	cachedKey, ok := p.Keys.Lookup(key.Key)
	if !ok || cachedKey.Version != newKey.Version || cachedKey.Groups[0] != group.ID {
		t.Fatal("key publication regressed version or exposed returned Groups backing array")
	}
	p.Keys.TouchLastUsed(key.Key, 1800000200)
	p.Keys.publishCurrentEntry(newKey)
	cachedKey, _ = p.Keys.Lookup(key.Key)
	if cachedKey.LastUsed != 1800000200 {
		t.Fatal("key no-op publication discarded unflushed last-used time")
	}
}

func TestStoreNoopKeyGroupInPlaceMutationPersists(t *testing.T) {
	p, _ := webTestSetup(t)
	key, err := p.Keys.Create("inplace-key", []string{"first"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Keys.Update(key.ID, func(k *APIKey) error { k.Groups[0] = "second"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	current, found, err := p.Keys.LookupCurrent(t.Context(), key.Key)
	if err != nil || !found || len(current.Groups) != 1 || current.Groups[0] != "second" || got.Version != key.Version+1 {
		t.Fatal("in-place group mutation was mistaken for no-op")
	}
}

func TestPGStoreNoopProxyCASAndPasswordReuse(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	proxy, err := p.Proxies.Create(ProxyEntry{Name: "noop-race", Type: "http", Host: "127.0.0.1", Port: 4321, Password: "unchanged-cipher-secret"})
	if err != nil {
		t.Fatal(err)
	}
	var cipherBefore []byte
	if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT password_cipher FROM proxies WHERE id=$1", proxy.ID).Scan(&cipherBefore); err != nil {
		t.Fatal(err)
	}
	var changed ProxyEntry
	_, err = p.Proxies.UpdateVersion(proxy.ID, proxy.Version, func(v *ProxyEntry) error {
		var changeErr error
		changed, changeErr = p.Proxies.UpdateVersion(proxy.ID, proxy.Version, func(current *ProxyEntry) error { current.Name = "concurrent rename"; return nil })
		return changeErr
	})
	if !errors.Is(err, errVersionConflict) {
		t.Fatalf("no-op based on stale read ignored concurrent update: %v", err)
	}
	var cipherAfter []byte
	if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT password_cipher FROM proxies WHERE id=$1", proxy.ID).Scan(&cipherAfter); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cipherBefore, cipherAfter) {
		t.Fatal("metadata-only update re-encrypted unchanged proxy password")
	}
	if changed.Version != proxy.Version+1 {
		t.Fatalf("real metadata update lost revision: %d", changed.Version)
	}
	cleared, err := p.Proxies.UpdateVersion(proxy.ID, changed.Version, func(v *ProxyEntry) error { v.Password = ""; return nil })
	if err != nil || cleared.HasPassword || cleared.Password != "" {
		t.Fatalf("real password clearing changed: %+v %v", cleared, err)
	}
	if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT password_cipher FROM proxies WHERE id=$1", proxy.ID).Scan(&cipherAfter); err != nil || len(cipherAfter) != 0 {
		t.Fatalf("password ciphertext survived explicit clearing: %v", err)
	}
}

func TestPGStoreNoopGroupWaitsForCASAndKeyGroupsRemainValidated(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	group, err := p.Groups.Create("noop-cas", "")
	if err != nil {
		t.Fatal(err)
	}
	key, err := p.Keys.Create("noop-cas", []string{group.ID})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := p.Store.DB.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), "UPDATE groups SET version=version+1 WHERE id=$1", group.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, e := p.Groups.UpdateVersion(group.ID, group.Name, group.Description, group.Version)
		done <- e
	}()
	select {
	case e := <-done:
		t.Fatalf("no-op group did not synchronize current row: %v", e)
	case <-time.After(30 * time.Millisecond):
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-done:
		if !errors.Is(e, errVersionConflict) {
			t.Fatalf("waited no-op ignored changed version: %v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("group no-op blocked after writer commit")
	}
	// An external invalid document must not bypass the old in-transaction
	// group validation just because the requested key values are unchanged.
	if _, err := p.Store.DB.Pool.Exec(t.Context(), "DELETE FROM groups WHERE id=$1", group.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Keys.Update(key.ID, func(v *APIKey) error { return nil }); !errors.Is(err, errInvalid) {
		t.Fatalf("no-op skipped invalid group reference: %v", err)
	}
}
