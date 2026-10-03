package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestKeyManagementCountsReadOnly(t *testing.T) {
	exerciseKeyManagementCountsReadOnly(t, webTestSetup)
}

func TestPGKeyManagementCountsReadOnly(t *testing.T) {
	exerciseKeyManagementCountsReadOnly(t, adminPGTestProxy)
}

func exerciseKeyManagementCountsReadOnly(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	first, err := p.Groups.Create("key-count-first", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Groups.Create("key-count-second", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Keys.store.Modify(func(state *keyState) error {
		state.Keys = []APIKey{
			{ID: "count-a", Key: "synthetic-count-a", Groups: []string{first.ID, first.ID}, Enabled: true},
			{ID: "count-b", Key: "synthetic-count-b", Groups: []string{first.ID, second.ID}, Enabled: false},
			{ID: "count-c", Key: "synthetic-count-c", Groups: []string{second.ID}, Enabled: true},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Keys.List(); err != nil {
		t.Fatal(err)
	}
	// A management count is a pure read: it must not rebuild the live index
	// and discard an unflushed observation written by gateway activity.
	p.Keys.TouchLastUsed("synthetic-count-a", 1800000000)
	for range 2 {
		total, enabled, groups, err := p.Keys.managementCounts(t.Context(), []string{first.ID, first.ID, "missing"})
		if err != nil || total != 3 || enabled != 2 || !reflect.DeepEqual(groups, map[string]int{first.ID: 2, "missing": 0}) {
			t.Fatalf("key count or per-entity group deduplication changed: total=%d enabled=%d groups=%v err=%v", total, enabled, groups, err)
		}
		groups[first.ID] = 999 // Caller cannot mutate the immutable aggregate.
	}
	for _, endpoint := range []string{"groups", "overview"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/"+endpoint, nil)
		if endpoint == "groups" {
			p.handleGroupsList(response, request)
		} else {
			p.handleOverview(response, request)
		}
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", endpoint, response.Code, response.Body.String())
		}
	}
	entry, found := p.Keys.Lookup("synthetic-count-a")
	if !found || entry.LastUsed != 1800000000 {
		t.Fatal("management count read rebuilt gateway index or lost dirty last-used time")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, _, err := p.Keys.managementCounts(canceled, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled count succeeded: %v", err)
	}
}

func TestPGKeyManagementCountsCurrentRevisionAndFailure(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	group, err := p.Groups.Create("key-count-change", "")
	if err != nil {
		t.Fatal(err)
	}
	key, err := p.Keys.Create("key-count-change", []string{group.ID})
	if err != nil {
		t.Fatal(err)
	}
	counter := &keySnapshotCountingCipher{AEAD: p.Store.DB.cipher}
	p.Store.DB.cipher = counter
	if _, _, _, err := p.Keys.managementCounts(t.Context(), []string{group.ID}); err != nil {
		t.Fatal(err)
	}
	decrypts, reads := counter.decrypts.Load(), p.Store.DB.Pool.Stat().AcquireCount()
	for range 5 {
		total, enabled, groups, err := p.Keys.managementCounts(t.Context(), []string{group.ID})
		if err != nil || total != 1 || enabled != 1 || groups[group.ID] != 1 {
			t.Fatalf("warm counts: %d/%d %v %v", total, enabled, groups, err)
		}
	}
	if counter.decrypts.Load() != decrypts || p.Store.DB.Pool.Stat().AcquireCount()-reads != 5 {
		t.Fatal("warm count must verify PG once per call without decoding credentials")
	}
	// Simulate an external writer omitting a revision increment. Tuple identity
	// still invalidates both authorization and precomputed management counts.
	key.Enabled, key.Groups = false, nil
	plain, _ := json.Marshal(keyState{Keys: []APIKey{key}})
	cipher, err := p.Store.DB.Encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"ciphertext": cipher})
	if _, err := p.Store.DB.Pool.Exec(t.Context(), "UPDATE settings SET value=$1 WHERE key='gateway_keys'", raw); err != nil {
		t.Fatal(err)
	}
	total, enabled, groups, err := p.Keys.managementCounts(t.Context(), []string{group.ID})
	if err != nil || total != 1 || enabled != 0 || groups[group.ID] != 0 {
		t.Fatalf("same-version external update left stale counts: %d/%d %v %v", total, enabled, groups, err)
	}
	if _, err := p.Store.DB.Pool.Exec(t.Context(), "DELETE FROM settings WHERE key='gateway_keys'"); err != nil {
		t.Fatal(err)
	}
	total, enabled, groups, err = p.Keys.managementCounts(t.Context(), []string{group.ID})
	if err != nil || total != 0 || enabled != 0 || groups[group.ID] != 0 {
		t.Fatalf("deleted document left stale counts: %d/%d %v %v", total, enabled, groups, err)
	}
	if _, err := p.Store.DB.Pool.Exec(t.Context(), `INSERT INTO settings(key,value) VALUES('gateway_keys','{"ciphertext":"AAAA"}')`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := p.Keys.managementCounts(t.Context(), nil); err == nil {
		t.Fatal("corrupt document returned successful cached count")
	}
}

func TestPGKeyManagementCountsScale(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	group, err := p.Groups.Create("key-count-scale", "")
	if err != nil {
		t.Fatal(err)
	}
	const keyCount = 10000
	if err := p.Keys.store.Modify(func(state *keyState) error {
		state.Keys = make([]APIKey, keyCount)
		for i := range state.Keys {
			state.Keys[i] = APIKey{ID: fmt.Sprintf("scale-key-%d", i), Key: fmt.Sprintf("synthetic-scale-token-%d", i), Groups: []string{group.ID}, Enabled: i%2 == 0}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := p.Keys.managementCounts(t.Context(), []string{group.ID}); err != nil {
		t.Fatal(err)
	}
	measure := func(legacy bool) (time.Duration, float64) {
		started := time.Now()
		allocs := testing.AllocsPerRun(5, func() {
			if legacy {
				keys, err := p.Keys.List()
				if err != nil {
					t.Fatal(err)
				}
				count := 0
				groups := map[string]int{}
				for _, key := range keys {
					if key.Enabled {
						count++
					}
					addManagedGroupCounts(groups, key.Groups)
				}
				if count != keyCount/2 || groups[group.ID] != keyCount {
					t.Fatal("legacy baseline count mismatch")
				}
			} else {
				total, enabled, groups, err := p.Keys.managementCounts(t.Context(), []string{group.ID})
				if err != nil || total != keyCount || enabled != keyCount/2 || groups[group.ID] != keyCount {
					t.Fatalf("bounded count mismatch: %v", err)
				}
			}
		})
		return time.Since(started), allocs
	}
	legacyTime, legacyAllocs := measure(true)
	boundedTime, boundedAllocs := measure(false)
	// The count path must not allocate one detached Groups slice per key.
	// Compare the two observable paths on the same document and connection.
	if boundedAllocs*10 >= legacyAllocs {
		t.Fatalf("counts still allocate with key document size: legacy=%.0f bounded=%.0f", legacyAllocs, boundedAllocs)
	}
	t.Logf("KEY_COUNT_SCALE keys=%d iterations=6 legacy_total_ms=%.3f bounded_total_ms=%.3f legacy_allocs=%.0f bounded_allocs=%.0f", keyCount, float64(legacyTime.Microseconds())/1000, float64(boundedTime.Microseconds())/1000, legacyAllocs, boundedAllocs)
}
