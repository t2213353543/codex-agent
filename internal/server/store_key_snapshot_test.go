package server

import (
	"context"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type keySnapshotCountingCipher struct {
	cipher.AEAD
	decrypts atomic.Int64
}

func (c *keySnapshotCountingCipher) Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error) {
	c.decrypts.Add(1)
	return c.AEAD.Open(dst, nonce, ciphertext, additionalData)
}

func TestPGKeySnapshotAvoidsRepeatedDecryptionWithoutSkippingDatabase(t *testing.T) {
	s := accountPGTestStore(t)
	counter := &keySnapshotCountingCipher{AEAD: s.DB.cipher}
	s.DB.cipher = counter
	keys, err := NewPGKeyStore(s.DB)
	if err != nil {
		t.Fatal(err)
	}
	key, err := keys.Create("snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := keys.LookupCurrent(t.Context(), key.Key); err != nil || !ok {
		t.Fatalf("warm current key: %t %v", ok, err)
	}
	decrypts := counter.decrypts.Load()
	reads := s.DB.Pool.Stat().AcquireCount()
	for i := 0; i < 5; i++ {
		if _, ok, err := keys.LookupCurrent(t.Context(), key.Key); err != nil || !ok {
			t.Fatalf("current key: %t %v", ok, err)
		}
		if _, err := keys.List(); err != nil {
			t.Fatal(err)
		}
	}
	if counter.decrypts.Load() != decrypts || s.DB.Pool.Stat().AcquireCount()-reads != 10 {
		t.Fatalf("unchanged reads should each validate PG without decrypting: decrypts=%d reads=%d", counter.decrypts.Load()-decrypts, s.DB.Pool.Stat().AcquireCount()-reads)
	}
	s.DB.Pool.Close()
	if _, ok, err := keys.LookupCurrent(t.Context(), key.Key); ok || !errors.Is(err, errGatewayKeyStoreUnavailable) {
		t.Fatalf("database failure authenticated from cache: %t %v", ok, err)
	}
}

func TestPGKeySnapshotRejectsRevisionABA(t *testing.T) {
	for _, mutation := range []string{"update_without_version", "delete_recreate_same_version_and_time", "corrupt_without_version"} {
		t.Run(mutation, func(t *testing.T) {
			s := accountPGTestStore(t)
			writer, err := NewPGKeyStore(s.DB)
			if err != nil {
				t.Fatal(err)
			}
			key, err := writer.Create("snapshot-aba", nil)
			if err != nil {
				t.Fatal(err)
			}
			reader, err := NewPGKeyStore(s.DB)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok, err := reader.LookupCurrent(t.Context(), key.Key); err != nil || !ok {
				t.Fatalf("warm current key: %t %v", ok, err)
			}
			var version int64
			var updatedAt time.Time
			if err := s.DB.Pool.QueryRow(t.Context(), "SELECT version,updated_at FROM settings WHERE key='gateway_keys'").Scan(&version, &updatedAt); err != nil {
				t.Fatal(err)
			}
			key.Enabled = false
			plain, _ := json.Marshal(keyState{Keys: []APIKey{key}})
			encrypted, err := s.DB.Encrypt(plain)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"ciphertext": encrypted})
			if mutation == "delete_recreate_same_version_and_time" {
				tx, err := s.DB.Pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(context.Background())
				if _, err := tx.Exec(t.Context(), "DELETE FROM settings WHERE key='gateway_keys'"); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(t.Context(), "INSERT INTO settings(key,value,version,updated_at) VALUES('gateway_keys',$1,$2,$3)", raw, version, updatedAt); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else {
				if mutation == "corrupt_without_version" {
					raw = []byte(`{"ciphertext":"AAAA"}`)
				}
				if _, err := s.DB.Pool.Exec(t.Context(), "UPDATE settings SET value=$1 WHERE key='gateway_keys'", raw); err != nil {
					t.Fatal(err)
				}
			}
			_, ok, err := reader.LookupCurrent(t.Context(), key.Key)
			if ok || mutation == "corrupt_without_version" && !errors.Is(err, errGatewayKeyStoreUnavailable) || mutation != "corrupt_without_version" && err != nil {
				t.Fatalf("changed same-version row trusted old snapshot: %t %v", ok, err)
			}
		})
	}
}

func TestPGKeySnapshotConcurrentDetachedReads(t *testing.T) {
	s := accountPGTestStore(t)
	groups := NewPGGroupStore(s.DB)
	group, err := groups.Create("snapshot-group", "")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := NewPGKeyStore(s.DB)
	if err != nil {
		t.Fatal(err)
	}
	key, err := keys.Create("snapshot-copies", []string{group.ID})
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	errors := make(chan error, 8)
	for i := 0; i < 8; i++ {
		workers.Go(func() {
			for j := 0; j < 5; j++ {
				entry, found, err := keys.LookupCurrent(t.Context(), key.Key)
				if err != nil || !found || len(entry.Groups) != 1 || entry.Groups[0] != group.ID {
					errors <- fmt.Errorf("detached lookup failed: found=%t err=%v", found, err)
					return
				}
				entry.Groups[0] = "caller-mutated"
			}
		})
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
}
