package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// All integration state uses an isolated PostgreSQL schema and an exact-key
// Redis namespace. Payloads, encryption keys and account names are synthetic.
func bpsHistoryStorageFixture(t *testing.T) (*bpsRedisReviewFixture, *Proxy) {
	t.Helper()
	f := newBPSRedisReviewFixture(t)
	s := accountPGTestStore(t)
	p := f.proxy()
	p.Store.DB.Pool = s.DB.Pool
	return f, p
}

func bpsHistoryStorageFreshProxy(f *bpsRedisReviewFixture, p *Proxy) *Proxy {
	f.t.Helper()
	fresh := f.proxy()
	fresh.Store.DB.Pool = p.Store.DB.Pool
	return fresh
}

func bpsHistoryStorageCipher(t *testing.T, p *Proxy, record bpsHistoryRecord) []byte {
	t.Helper()
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal("encode synthetic history:", err)
	}
	ciphertext, err := p.Store.DB.Encrypt(raw)
	if err != nil {
		t.Fatal("encrypt synthetic history:", err)
	}
	return ciphertext
}

func bpsHistoryStorageSeedPG(t *testing.T, p *Proxy, keyID, responseID string, ciphertext []byte) {
	t.Helper()
	_, err := p.Store.DB.Pool.Exec(t.Context(), `
INSERT INTO gateway_response_history(namespace,history_key,ciphertext)
VALUES($1,$2,$3)
ON CONFLICT(namespace,history_key) DO UPDATE SET ciphertext=EXCLUDED.ciphertext,updated_at=now()`,
		p.Store.DB.Namespace, bpsHistoryKey(keyID, responseID), ciphertext)
	if err != nil {
		t.Fatal("seed isolated legacy PostgreSQL history:", err)
	}
}

func bpsHistoryStorageReadPG(t *testing.T, p *Proxy, keyID, responseID string) ([]byte, error) {
	t.Helper()
	var ciphertext []byte
	err := p.Store.DB.Pool.QueryRow(t.Context(), `
SELECT ciphertext FROM gateway_response_history WHERE namespace=$1 AND history_key=$2`,
		p.Store.DB.Namespace, bpsHistoryKey(keyID, responseID)).Scan(&ciphertext)
	return ciphertext, err
}

func bpsHistoryStorageRequireContent(t *testing.T, got, want bpsHistoryRecord) {
	t.Helper()
	if got.Owner != want.Owner || got.Session != want.Session || !bytes.Equal(got.Request, want.Request) || !reflect.DeepEqual(got.Output, want.Output) {
		t.Fatal("synthetic history changed its owner, session or content")
	}
}

func bpsHistoryStorageRequireTTL(t *testing.T, p *Proxy, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if ttl, err := p.Store.DB.Redis.TTL(t.Context(), key).Result(); err != nil || ttl <= 0 || ttl > bpsHistoryTTL {
			t.Fatalf("history state lacks a one-hour TTL: ttl=%v err=%v", ttl, err)
		}
	}
}

func TestBPSHistoryStorageAllModesExpireAndContinueAcrossFreshProxy(t *testing.T) {
	t.Setenv("CODEX_HISTORY_LEGACY_READ_UNTIL_UNIX", "")
	// The retired setting must never bring permanent writes back.
	t.Setenv("CODEX_HISTORY_STORAGE", "postgres")
	f := newBPSRedisReviewFixture(t)
	p := f.proxy()
	for _, mode := range []string{inferencePowerPoint, inferenceAppServer, inferenceCodexCLI} {
		t.Run(mode, func(t *testing.T) {
			ctx := withGatewayUnboundedPayload(withInferenceMode(t.Context(), mode), true)
			const keyID = "synthetic-expiring-key"
			responseID := "resp_synthetic_" + mode
			key := f.key(keyID, responseID)
			record := bpsRedisReviewRecord()
			if err := p.bpsSaveHistory(ctx, keyID, responseID, record); err != nil {
				t.Fatal("save bounded-lifetime history:", err)
			}
			bpsHistoryStorageRequireTTL(t, p, key, f.prefix()+"index", f.prefix()+"sizes", f.prefix()+"accounting")
			ciphertext, err := p.Store.DB.Redis.Get(ctx, key).Bytes()
			if err != nil || bytes.Contains(ciphertext, []byte(record.Owner)) || bytes.Contains(ciphertext, []byte("synthetic-private-prompt")) {
				t.Fatal("synthetic history missing or stored as plaintext:", err)
			}
			fresh := f.proxy()
			loaded, err := fresh.bpsLoadHistory(ctx, keyID, responseID)
			if err != nil || loaded.Mode != mode || loaded.Expires <= time.Now().Unix() || loaded.Expires > time.Now().Add(bpsHistoryTTL).Unix() {
				t.Fatal("fresh proxy lost mode or bounded lifetime:", err)
			}
			bpsHistoryStorageRequireContent(t, loaded, record)
			request, _ := json.Marshal(map[string]any{"previous_response_id": responseID, "input": []any{map[string]string{"role": "user", "content": "synthetic-next-turn"}}})
			expanded, session, owner, err := fresh.bpsExpandHistory(ctx, keyID, request, "")
			var body struct{ Input []json.RawMessage }
			if err != nil || owner != record.Owner || session != record.Session || json.Unmarshal(expanded, &body) != nil || len(body.Input) != 4 || !bytes.Contains(expanded, []byte("synthetic-upstream-cipher")) {
				t.Fatal("fresh proxy continuation lost complete content or ownership:", err)
			}
			for _, expiry := range []int64{0, time.Now().Add(-time.Second).Unix()} {
				loaded.Expires = expiry
				if err := p.Store.DB.Redis.Set(ctx, key, bpsHistoryStorageCipher(t, p, loaded), bpsHistoryTTL).Err(); err != nil {
					t.Fatal(err)
				}
				if _, err := fresh.bpsLoadHistory(ctx, keyID, responseID); !errors.Is(err, errBPSHistoryMissing) {
					t.Fatal("new namespace accepted zero or expired history:", err)
				}
			}
		})
	}
}

func TestBPSHistoryStorageAllModesMemoryLogicalExpiry(t *testing.T) {
	t.Setenv("CODEX_HISTORY_LEGACY_READ_UNTIL_UNIX", strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))
	p := &Proxy{BPS: &bpsRuntime{}}
	for _, mode := range []string{inferencePowerPoint, inferenceAppServer, inferenceCodexCLI} {
		ctx := withGatewayUnboundedPayload(withInferenceMode(t.Context(), mode), true)
		record := bpsRedisReviewRecord()
		if err := p.bpsSaveHistory(ctx, "synthetic-memory-key", mode, record); err != nil {
			t.Fatal(err)
		}
		loaded, err := p.bpsLoadHistory(ctx, "synthetic-memory-key", mode)
		if err != nil || loaded.Expires <= time.Now().Unix() {
			t.Fatal("memory history has no bounded lifetime:", err)
		}
		for _, expiry := range []int64{0, time.Now().Add(-time.Second).Unix()} {
			loaded.Expires = expiry
			raw, _ := json.Marshal(loaded)
			p.BPS.history.items[bpsHistoryKey("synthetic-memory-key", mode)] = raw
			if _, err := p.bpsLoadHistory(ctx, "synthetic-memory-key", mode); !errors.Is(err, errBPSHistoryMissing) {
				t.Fatal("legacy grace incorrectly authorized zero/expired memory history:", err)
			}
		}
	}
}

func TestBPSHistoryStorageLargeRedisContinuationKeepsPayloadAndTTL(t *testing.T) {
	f := newBPSRedisReviewFixture(t)
	p := f.proxy()
	ctx := withGatewayUnboundedPayload(t.Context(), true)
	const keyID, responseID = "synthetic-large-key", "resp_synthetic_large_bounded_cache"
	key := f.key(keyID, responseID)
	record := bpsRedisReviewRecord()
	record.Request = json.RawMessage(`{"model":"gpt-test","input":[{"role":"user","content":"` + strings.Repeat("x", maxBody+128) + `"}]}`)
	if err := p.bpsSaveHistory(ctx, keyID, responseID, record); err != nil {
		t.Fatal("payload larger than 32 MiB rejected below cache budget:", err)
	}
	bpsHistoryStorageRequireTTL(t, p, key)
	fresh := f.proxy()
	expanded, session, owner, err := fresh.bpsExpandHistory(ctx, keyID,
		[]byte(`{"previous_response_id":"resp_synthetic_large_bounded_cache","input":[{"role":"user","content":"synthetic-large-next"}]}`), "")
	if err != nil || len(expanded) <= maxBody || session != record.Session || owner != record.Owner || !bytes.Contains(expanded, []byte("synthetic-large-next")) {
		t.Fatal("large fresh-proxy continuation was truncated or unavailable:", err)
	}
	loaded, err := fresh.bpsLoadHistory(ctx, keyID, responseID)
	if err != nil || !bytes.Equal(loaded.Request, record.Request) || loaded.Expires <= time.Now().Unix() {
		t.Fatal("large bounded-lifetime history changed stored input:", err)
	}
}

func TestBPSHistoryStorageNeverWritesPostgresInAnyMode(t *testing.T) {
	t.Setenv("CODEX_HISTORY_STORAGE", "postgres")
	t.Setenv("CODEX_HISTORY_LEGACY_READ_UNTIL_UNIX", "")
	f, p := bpsHistoryStorageFixture(t)
	for _, mode := range []string{inferencePowerPoint, inferenceAppServer, inferenceCodexCLI} {
		ctx := withGatewayUnboundedPayload(withInferenceMode(t.Context(), mode), true)
		for _, opaque := range []bool{false, true} {
			responseID := fmt.Sprintf("resp_synthetic_no_pg_%s_%t", mode, opaque)
			record := bpsRedisReviewRecord()
			key := f.key("synthetic-no-pg-key", responseID)
			var err error
			if opaque {
				for _, proof := range bpsOpaqueItemOwnerKeys("synthetic-no-pg-key", mode, record.Output) {
					f.track(f.namespace + "bps:opaque-owner:v2:" + proof)
				}
				err = p.bpsSaveHistoryWithOpaqueOwners(ctx, "synthetic-no-pg-key", responseID, record, record.Output)
			} else {
				err = p.bpsSaveHistory(ctx, "synthetic-no-pg-key", responseID, record)
			}
			if err != nil {
				t.Fatal("finite Redis save failed with a configured PostgreSQL pool:", err)
			}
			bpsHistoryStorageRequireTTL(t, p, key)
			if _, err := bpsHistoryStorageReadPG(t, p, "synthetic-no-pg-key", responseID); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatal("new history leaked into permanent PostgreSQL storage:", err)
			}
		}
	}
}

func TestBPSHistoryStorageLegacyRedisAndOwnerRequireExplicitGrace(t *testing.T) {
	t.Setenv("CODEX_HISTORY_LEGACY_READ_UNTIL_UNIX", "")
	f := newBPSRedisReviewFixture(t)
	p := f.proxy()
	ctx := withGatewayUnboundedPayload(t.Context(), true)
	const keyID, responseID = "synthetic-legacy-key", "resp_synthetic_legacy_grace"
	record := bpsRedisReviewRecord()
	record.Mode = inferencePowerPoint
	legacyKey := f.track(f.namespace + "bps:history:" + bpsHistoryKey(keyID, responseID))
	legacyProof := f.track(f.namespace + "bps:opaque-owner:" + bpsOpaqueOwnerKey(keyID, inferencePowerPoint, "synthetic-upstream-cipher"))
	original := bpsHistoryStorageCipher(t, p, record)
	if err := p.Store.DB.Redis.Set(ctx, legacyKey, original, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.DB.Redis.Set(ctx, legacyProof, record.Owner, 0).Err(); err != nil {
		t.Fatal(err)
	}
	assertHidden := func() {
		t.Helper()
		if _, err := p.bpsLoadHistory(ctx, keyID, responseID); !errors.Is(err, errBPSHistoryMissing) {
			t.Fatal("legacy history escaped the absolute grace deadline:", err)
		}
		if owner, err := p.bpsResolveOpaqueOwner(ctx, keyID, opaqueReviewBody(record.Output), ""); err != nil || owner != "" {
			t.Fatal("legacy ownership proof escaped the absolute grace deadline:", err)
		}
	}
	for _, cutoff := range []string{"", "invalid", strconv.FormatInt(time.Now().Add(-time.Second).Unix(), 10), strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10)} {
		t.Setenv("CODEX_HISTORY_LEGACY_READ_UNTIL_UNIX", cutoff)
		assertHidden()
	}
	t.Setenv("CODEX_HISTORY_LEGACY_READ_UNTIL_UNIX", strconv.FormatInt(time.Now().Add(2*time.Minute).Unix(), 10))
	loaded, err := p.bpsLoadHistory(ctx, keyID, responseID)
	if err != nil {
		t.Fatal("explicit bounded grace could not read legacy encrypted history:", err)
	}
	bpsHistoryStorageRequireContent(t, loaded, record)
	if owner, err := p.bpsResolveOpaqueOwner(ctx, keyID, opaqueReviewBody(record.Output), ""); err != nil || owner != record.Owner {
		t.Fatal("explicit grace could not recover legacy ownership:", err)
	}
	// New writes use only v2 and remain available after grace closes.
	newKey := f.key(keyID, responseID)
	updated := record
	updated.Session = "synthetic-new-bounded-session"
	if err := p.bpsSaveHistory(ctx, keyID, responseID, updated); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HISTORY_LEGACY_READ_UNTIL_UNIX", strconv.FormatInt(time.Now().Add(-time.Second).Unix(), 10))
	loaded, err = f.proxy().bpsLoadHistory(ctx, keyID, responseID)
	if err != nil {
		t.Fatal("closing legacy grace invalidated new bounded history:", err)
	}
	bpsHistoryStorageRequireContent(t, loaded, updated)
	bpsHistoryStorageRequireTTL(t, p, newKey)
	if retained, err := p.Store.DB.Redis.Get(ctx, legacyKey).Bytes(); err != nil || !bytes.Equal(retained, original) {
		t.Fatal("bounded saver unexpectedly rewrote legacy bytes:", err)
	}
	if err := p.Store.DB.Redis.Del(ctx, newKey).Err(); err != nil {
		t.Fatal(err)
	}
	assertHidden()
}

func TestBPSHistoryStorageLegacyPostgresReadStopsAtDeadline(t *testing.T) {
	t.Setenv("CODEX_HISTORY_LEGACY_READ_UNTIL_UNIX", "")
	f, p := bpsHistoryStorageFixture(t)
	ctx := withGatewayUnboundedPayload(withInferenceMode(t.Context(), inferenceCodexCLI), true)
	const keyID, responseID = "synthetic-legacy-pg-key", "resp_synthetic_legacy_pg"
	record := bpsRedisReviewRecord()
	record.Mode = inferenceCodexCLI
	original := bpsHistoryStorageCipher(t, p, record)
	bpsHistoryStorageSeedPG(t, p, keyID, responseID, original)
	if _, err := p.bpsLoadHistory(ctx, keyID, responseID); !errors.Is(err, errBPSHistoryMissing) {
		t.Fatal("default reader queried permanent PostgreSQL history:", err)
	}
	t.Setenv("CODEX_HISTORY_LEGACY_READ_UNTIL_UNIX", strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))
	loaded, err := bpsHistoryStorageFreshProxy(f, p).bpsLoadHistory(ctx, keyID, responseID)
	if err != nil {
		t.Fatal("explicit grace cannot read authenticated legacy PostgreSQL history:", err)
	}
	bpsHistoryStorageRequireContent(t, loaded, record)
	if _, err := p.bpsLoadHistory(ctx, "synthetic-other-key", responseID); !errors.Is(err, errBPSHistoryMissing) {
		t.Fatal("legacy PostgreSQL fallback crossed API-key scope:", err)
	}
	t.Setenv("CODEX_HISTORY_LEGACY_READ_UNTIL_UNIX", strconv.FormatInt(time.Now().Add(-time.Second).Unix(), 10))
	if _, err := p.bpsLoadHistory(ctx, keyID, responseID); !errors.Is(err, errBPSHistoryMissing) {
		t.Fatal("legacy PostgreSQL remained readable beyond its absolute cutoff:", err)
	}
	retained, err := bpsHistoryStorageReadPG(t, p, keyID, responseID)
	if err != nil || !bytes.Equal(retained, original) {
		t.Fatal("read-only legacy fallback changed PostgreSQL history:", err)
	}
}

func TestBPSHistoryStorageOpaqueProofActuallyExpires(t *testing.T) {
	f := newBPSRedisReviewFixture(t)
	p := f.proxy()
	ctx := withGatewayUnboundedPayload(t.Context(), true)
	const keyID, responseID = "synthetic-expiring-owner-key", "resp_synthetic_expiring_owner"
	record := bpsRedisReviewRecord()
	historyKey := f.key(keyID, responseID)
	proof := reviewOpaqueProofKey(f, keyID, inferencePowerPoint, "synthetic-upstream-cipher")
	if err := p.bpsSaveHistoryWithOpaqueOwners(ctx, keyID, responseID, record, record.Output); err != nil {
		t.Fatal(err)
	}
	bpsHistoryStorageRequireTTL(t, p, historyKey, proof)
	if err := p.Store.DB.Redis.PExpire(ctx, historyKey, time.Millisecond).Err(); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.DB.Redis.PExpire(ctx, proof, time.Millisecond).Err(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		n, err := p.Store.DB.Redis.Exists(ctx, historyKey, proof).Result()
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("synthetic Redis history/proof failed to expire")
		}
		time.Sleep(time.Millisecond)
	}
	fresh := f.proxy()
	if _, err := fresh.bpsLoadHistory(ctx, keyID, responseID); !errors.Is(err, errBPSHistoryMissing) {
		t.Fatal("expired payload remained readable:", err)
	}
	if owner, err := fresh.bpsResolveOpaqueOwner(ctx, keyID, opaqueReviewBody(record.Output), ""); err != nil || owner != "" {
		t.Fatal("expired proof still claimed permanent ownership:", err)
	}
}

func TestBPSHistoryStorageWarmPreservesRemainingTTLVerifiedOwnersAndExistingValues(t *testing.T) {
	f := newBPSRedisReviewFixture(t)
	p := f.proxy()
	ctx := withGatewayUnboundedPayload(t.Context(), true)
	const keyID, responseID = "synthetic-warm-key", "resp_synthetic_warm"
	record := bpsRedisReviewRecord()
	record.Mode = inferencePowerPoint
	record.Output = append(record.Output,
		reviewOpaqueItem("reasoning", "synthetic-unknown", "synthetic-unverified-cipher"),
		reviewOpaqueItem("compaction", "synthetic-other-owner", "synthetic-mismatched-owner-cipher"))
	key := f.key(keyID, responseID)
	verified := reviewOpaqueProofKey(f, keyID, inferencePowerPoint, "synthetic-upstream-cipher")
	unknown := reviewOpaqueProofKey(f, keyID, inferencePowerPoint, "synthetic-unverified-cipher")
	mismatched := reviewOpaqueProofKey(f, keyID, inferencePowerPoint, "synthetic-mismatched-owner-cipher")
	oldVerified := f.track(f.namespace + "bps:opaque-owner:" + bpsOpaqueOwnerKey(keyID, inferencePowerPoint, "synthetic-upstream-cipher"))
	oldMismatched := f.track(f.namespace + "bps:opaque-owner:" + bpsOpaqueOwnerKey(keyID, inferencePowerPoint, "synthetic-mismatched-owner-cipher"))
	if err := p.Store.DB.Redis.Set(ctx, oldVerified, record.Owner, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.DB.Redis.Set(ctx, oldMismatched, "synthetic-different-owner", 0).Err(); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(5 * time.Minute).Truncate(time.Second)
	legacyCipher := bpsHistoryStorageCipher(t, p, record)
	if err := WarmBPSHistory(ctx, p.Store.DB, keyID, responseID, legacyCipher, cutoff); err != nil {
		t.Fatal("warm recent synthetic history:", err)
	}
	loaded, err := f.proxy().bpsLoadHistory(ctx, keyID, responseID)
	if err != nil || loaded.Expires != cutoff.Unix() {
		t.Fatal("warm history reset its original expiry deadline:", err)
	}
	bpsHistoryStorageRequireContent(t, loaded, record)
	for _, boundedKey := range []string{key, verified} {
		ttl, err := p.Store.DB.Redis.TTL(ctx, boundedKey).Result()
		if err != nil || ttl <= 0 || ttl > 5*time.Minute {
			t.Fatal("warm payload or verified proof extended remaining retention:", err)
		}
	}
	if count, err := p.Store.DB.Redis.Exists(ctx, unknown, mismatched).Result(); err != nil || count != 0 {
		t.Fatal("warm fabricated unknown/mismatched legacy ownership proofs:", err)
	}
	before, err := p.Store.DB.Redis.Get(ctx, key).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	changed := record
	changed.Session = "synthetic-stale-warm-overwrite"
	if err := WarmBPSHistory(ctx, p.Store.DB, keyID, responseID, bpsHistoryStorageCipher(t, p, changed), time.Now().Add(10*time.Minute)); !errors.Is(err, ErrBPSHistoryAlreadyCached) {
		t.Fatal("warm did not protect existing live v2 history:", err)
	}
	after, err := p.Store.DB.Redis.Get(ctx, key).Bytes()
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("warm replaced existing live v2 bytes:", err)
	}
	for i, invalidCutoff := range []time.Time{time.Now().Add(-time.Second), time.Now().Add(2 * time.Hour)} {
		invalidID := fmt.Sprintf("resp_synthetic_invalid_warm_%d", i)
		invalidKey := f.key(keyID, invalidID)
		if err := WarmBPSHistory(ctx, p.Store.DB, keyID, invalidID, legacyCipher, invalidCutoff); err == nil {
			t.Fatal("warm accepted expired or excessive retention")
		}
		if n, err := p.Store.DB.Redis.Exists(ctx, invalidKey).Result(); err != nil || n != 0 {
			t.Fatal("rejected warm operation published a payload:", err)
		}
	}
}

func TestBPSHistoryStorageOversizedBodyPreservesSuccessAndOwnerProof(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			var f *bpsRedisReviewFixture
			p := &Proxy{BPS: &bpsRuntime{}}
			if backend == "redis" {
				f = newBPSRedisReviewFixture(t)
				p = f.proxy()
			}
			ctx := withGatewayUnboundedPayload(t.Context(), true)
			const keyID = "synthetic-oversized-key"
			record := bpsRedisReviewRecord()
			proofs := bpsOpaqueItemOwnerKeys(keyID, inferencePowerPoint, record.Output)
			if f != nil {
				for _, id := range []string{"resp_existing", "resp_oversized", "resp_conflict"} {
					f.key(keyID, id)
				}
				for _, proof := range proofs {
					f.track(f.namespace + "bps:opaque-owner:v2:" + proof)
				}
			}
			if err := p.bpsSaveHistory(ctx, keyID, "resp_existing", record); err != nil {
				t.Fatal(err)
			}
			// A small injected cache budget exercises the production oversize
			// boundary without allocating a 512 MiB synthetic request.
			if err := p.bpsSaveHistoryRecordWithinBudget(ctx, keyID, "resp_oversized", record, proofs, 64); err != nil {
				t.Fatal("oversized body changed a successful request into a storage failure:", err)
			}
			if _, err := p.bpsLoadHistory(ctx, keyID, "resp_oversized"); !errors.Is(err, errBPSHistoryMissing) {
				t.Fatal("oversized body entered the bounded cache:", err)
			}
			kept, err := p.bpsLoadHistory(ctx, keyID, "resp_existing")
			if err != nil {
				t.Fatal("oversized body evicted previously cached history:", err)
			}
			bpsHistoryStorageRequireContent(t, kept, record)
			if owner, err := p.bpsResolveOpaqueOwner(ctx, keyID, opaqueReviewBody(record.Output), ""); err != nil || owner != record.Owner {
				t.Fatal("oversized response lost its completed-output owner proof:", err)
			}
			for _, proof := range proofs {
				if f != nil {
					bpsHistoryStorageRequireTTL(t, p, f.namespace+"bps:opaque-owner:v2:"+proof)
				} else if expiry := p.BPS.history.opaqueExpires[proof]; expiry <= time.Now().Unix() || expiry > time.Now().Add(bpsHistoryTTL).Unix() {
					t.Fatal("oversized response created permanent in-memory ownership")
				}
			}
			freshItem := reviewOpaqueItem("reasoning", "synthetic-fresh-conflict", "synthetic-oversize-new-proof")
			conflicting := record
			conflicting.Owner = "synthetic-conflicting-owner"
			conflicting.Output = append([]json.RawMessage{freshItem}, record.Output...)
			conflictProofs := bpsOpaqueItemOwnerKeys(keyID, inferencePowerPoint, conflicting.Output)
			if f != nil {
				for _, proof := range conflictProofs {
					f.track(f.namespace + "bps:opaque-owner:v2:" + proof)
				}
			}
			if err := p.bpsSaveHistoryRecordWithinBudget(ctx, keyID, "resp_conflict", conflicting, conflictProofs, 64); !errors.Is(err, errBPSOpaqueOwnerConflict) {
				t.Fatal("oversized response bypassed atomic owner conflict checks:", err)
			}
			if owner, err := p.bpsResolveOpaqueOwner(ctx, keyID, opaqueReviewBody([]json.RawMessage{freshItem}), ""); err != nil || owner != "" {
				t.Fatal("oversized conflict partially published its fresh proof:", err)
			}
		})
	}
}
