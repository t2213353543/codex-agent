package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func reviewOpaqueContext(t *testing.T, mode string) context.Context {
	t.Helper()
	return withGatewayUnboundedPayload(withInferenceMode(t.Context(), mode), true)
}

func reviewOpaqueItem(kind, id, cipher string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"type": kind, "id": id, "encrypted_content": cipher, "summary": []any{}})
	return raw
}

func reviewOpaqueProofKey(f *bpsRedisReviewFixture, key, mode, cipher string) string {
	return f.track(f.namespace + "bps:opaque-owner:v2:" + bpsOpaqueOwnerKey(key, mode, cipher))
}

func reviewOpaqueRecord(owner string, output []json.RawMessage) bpsHistoryRecord {
	return bpsHistoryRecord{Owner: owner, Session: "synthetic-session", Request: json.RawMessage(`{"model":"gpt-6-astra","input":[{"role":"user","content":"synthetic"}]}`), Output: output}
}

func reviewOpaqueBody(items []json.RawMessage, tools string) []byte {
	raw, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": items, "tools": json.RawMessage(tools), "instructions": "synthetic changed controls", "prompt_cache_key": "different-prefix"})
	return raw
}

func TestBPSOpaqueOwnerRedisFreshProxyExpiryAndIsolation(t *testing.T) {
	f := newBPSRedisReviewFixture(t)
	writer, reader := f.proxy(), f.proxy()
	ctx := reviewOpaqueContext(t, inferencePowerPoint)
	const key, owner, reason, compact = "synthetic-api-key", "synthetic-owner-A", "SYNTHETIC_REASON_α+/=", "SYNTHETIC_COMPACT_β+/="
	output := []json.RawMessage{reviewOpaqueItem("reasoning", "rs_A", reason), reviewOpaqueItem("compaction", "cmp_A", compact)}
	historyKey := f.key(key, "resp_A")
	rKey := reviewOpaqueProofKey(f, key, inferencePowerPoint, reason)
	cKey := reviewOpaqueProofKey(f, key, inferencePowerPoint, compact)
	record := reviewOpaqueRecord(owner, output)
	if err := writer.bpsSaveHistoryWithOpaqueOwners(ctx, key, "resp_A", record, output); err != nil {
		t.Fatal(err)
	}
	stored, err := reader.bpsLoadHistory(ctx, key, "resp_A")
	if err != nil || stored.Owner != owner || stored.Mode != inferencePowerPoint || stored.Expires <= time.Now().Unix() || !reflect.DeepEqual(stored.Output, output) {
		t.Fatal("new process-equivalent proxy failed to read exact completed history")
	}
	wire, err := reader.Store.DB.Redis.Get(ctx, historyKey).Bytes()
	if err != nil || bytes.Contains(wire, []byte(reason)) || bytes.Contains(wire, []byte(compact)) {
		t.Fatal("history not encrypted at rest")
	}
	for _, proof := range []string{rKey, cKey} {
		if value, err := reader.Store.DB.Redis.Get(ctx, proof).Result(); err != nil || value != owner {
			t.Fatal("output owner proof was not persisted")
		}
		if ttl := reader.Store.DB.Redis.TTL(ctx, proof).Val(); ttl <= 0 || ttl > bpsHistoryTTL {
			t.Fatal("ownership proof is missing its bounded TTL")
		}
	}
	// Actual Redis expiration of the old sticky binding, separate from the
	// one-hour proof. Storage helper proof is not a claim of gateway dispatch.
	sessionDigest := sha256.Sum256([]byte(key + "\x00synthetic-session"))
	binding := f.track(fmt.Sprintf("%sgateway:thread:%x", f.namespace, sessionDigest))
	if err := writer.Store.DB.Redis.Set(ctx, binding, owner, time.Millisecond).Err(); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Second)
	for reader.Store.DB.Redis.Exists(ctx, binding).Val() != 0 {
		if time.Now().After(until) {
			t.Fatal("synthetic session binding did not expire")
		}
		time.Sleep(time.Millisecond)
	}
	for _, items := range [][]json.RawMessage{output[:1], output[1:], output} {
		body := reviewOpaqueBody(items, `[{"type":"function","name":"different_tool","parameters":{"type":"object","properties":{}}}]`)
		before := bytes.Clone(body)
		got, err := reader.bpsResolveOpaqueOwner(ctx, key, body, "")
		if err != nil || got != owner || !bytes.Equal(body, before) {
			t.Fatal("binding expiry/changed tools changed known ciphertext owner or request bytes")
		}
	}
	// Escaped JSON spelling decodes to the same opaque string without any
	// base64 decode, cipher rewrite, or dependence on returned item ID.
	escaped := []byte(`{"input":[{"type":"reasoning","id":"rs_new_spelling","encrypted_content":"SYNTHETIC_REASON_\u03b1+/="}],"tools":[]}`)
	if got, err := reader.bpsResolveOpaqueOwner(ctx, key, escaped, ""); err != nil || got != owner {
		t.Fatal("JSON string representation changed owner identity")
	}
	otherKey := "synthetic-other-api-key"
	otherMode := reviewOpaqueContext(t, inferenceCodexCLI)
	for _, test := range []struct {
		ctx context.Context
		key string
	}{{ctx, otherKey}, {otherMode, key}} {
		if got, err := reader.bpsResolveOpaqueOwner(test.ctx, test.key, reviewOpaqueBody(output, `[]`), ""); err != nil || got != "" {
			t.Fatal("owner proof leaked across API key or inference mode")
		}
	}
	// The same string may independently belong to another account in a distinct
	// authenticated scope; do not flag these separate records as collisions.
	for index, test := range []struct {
		ctx       context.Context
		key, mode string
	}{{ctx, otherKey, inferencePowerPoint}, {otherMode, key, inferenceCodexCLI}} {
		response := fmt.Sprintf("resp_scope_%d", index)
		f.key(test.key, response)
		for _, cipher := range []string{reason, compact} {
			reviewOpaqueProofKey(f, test.key, test.mode, cipher)
		}
		if err := reader.bpsSaveHistoryWithOpaqueOwners(test.ctx, test.key, response, reviewOpaqueRecord("synthetic-owner-B", output), output); err != nil {
			t.Fatal("independent scope rejected legitimate own proof")
		}
		if got, err := writer.bpsResolveOpaqueOwner(test.ctx, test.key, reviewOpaqueBody(output, `[]`), ""); err != nil || got != "synthetic-owner-B" {
			t.Fatal("fresh proxy scope lookup changed")
		}
	}
	if _, err := reader.bpsResolveOpaqueOwner(ctx, key, reviewOpaqueBody(output, `[]`), "synthetic-owner-B"); !errors.Is(err, errBPSOpaqueOwnerConflict) {
		t.Fatal("known ciphertext disagreed with previous-response owner without rejection")
	}
	t.Log("two fresh Proxy Redis clients: two completed outputs preserved, real binding expired, changed controls/same decoded cipher resolve A; key/mode independent; no gateway dispatch claimed")
}

func TestBPSOpaqueOwnerRedisBatchConflictHasNoPartialWrite(t *testing.T) {
	f := newBPSRedisReviewFixture(t)
	one, two := f.proxy(), f.proxy()
	ctx := reviewOpaqueContext(t, inferencePowerPoint)
	const key = "synthetic-batch-key"
	conflict := reviewOpaqueItem("reasoning", "rs_conflict", "SYNTHETIC_CONFLICT")
	newItem := reviewOpaqueItem("compaction", "cmp_fresh", "SYNTHETIC_FRESH")
	conflictKey := reviewOpaqueProofKey(f, key, inferencePowerPoint, "SYNTHETIC_CONFLICT")
	newKey := reviewOpaqueProofKey(f, key, inferencePowerPoint, "SYNTHETIC_FRESH")
	f.key(key, "resp_original_owner")
	if err := one.bpsSaveHistoryWithOpaqueOwners(ctx, key, "resp_original_owner", reviewOpaqueRecord("owner-A", []json.RawMessage{conflict}), []json.RawMessage{conflict}); err != nil {
		t.Fatal(err)
	}
	historyKey := f.key(key, "resp_target_existing")
	if err := one.bpsSaveHistory(ctx, key, "resp_target_existing", reviewOpaqueRecord("owner-B", nil)); err != nil {
		t.Fatal(err)
	}
	before := one.Store.DB.Redis.Get(ctx, historyKey).Val()
	for _, response := range []string{"resp_target_existing", "resp_target_new"} {
		target := f.key(key, response)
		err := two.bpsSaveHistoryWithOpaqueOwners(ctx, key, response, reviewOpaqueRecord("owner-B", []json.RawMessage{newItem, conflict}), []json.RawMessage{newItem, conflict})
		if !errors.Is(err, errBPSOpaqueOwnerConflict) {
			t.Fatal("last proof conflict did not reject whole transaction")
		}
		if response == "resp_target_existing" {
			if got := one.Store.DB.Redis.Get(ctx, target).Val(); got != before {
				t.Fatal("owner conflict replaced existing history bytes")
			}
		} else if one.Store.DB.Redis.Exists(ctx, target).Val() != 0 {
			t.Fatal("owner conflict partially created new history")
		}
		if one.Store.DB.Redis.Exists(ctx, newKey).Val() != 0 || one.Store.DB.Redis.Get(ctx, conflictKey).Val() != "owner-A" {
			t.Fatal("first proof written before last conflict or existing owner overwritten")
		}
	}
}

func TestBPSOpaqueOwnerRedisWrongTypesAndUnavailableFailClosed(t *testing.T) {
	for _, broken := range []string{"owner", "index", "sizes", "history"} {
		t.Run(broken, func(t *testing.T) {
			f := newBPSRedisReviewFixture(t)
			one, two := f.proxy(), f.proxy()
			ctx := reviewOpaqueContext(t, inferencePowerPoint)
			const key = "synthetic-types-key"
			first := reviewOpaqueItem("reasoning", "rs_first", "SYNTHETIC_FIRST")
			last := reviewOpaqueItem("compaction", "cmp_last", "SYNTHETIC_LAST")
			items := []json.RawMessage{first, last}
			history := f.key(key, "resp_protected")
			index, sizes := f.prefix()+"index", f.prefix()+"sizes"
			firstKey := reviewOpaqueProofKey(f, key, inferencePowerPoint, "SYNTHETIC_FIRST")
			lastKey := reviewOpaqueProofKey(f, key, inferencePowerPoint, "SYNTHETIC_LAST")
			if err := one.bpsSaveHistory(ctx, key, "resp_protected", reviewOpaqueRecord("owner-A", nil)); err != nil {
				t.Fatal(err)
			}
			before := one.Store.DB.Redis.Get(ctx, history).Val()
			if err := one.Store.DB.Redis.ZAdd(ctx, index, redis.Z{Member: history, Score: 123}).Err(); err != nil {
				t.Fatal(err)
			}
			if err := one.Store.DB.Redis.HSet(ctx, sizes, history, 123).Err(); err != nil {
				t.Fatal(err)
			}
			badKey := map[string]string{"owner": lastKey, "index": index, "sizes": sizes, "history": history}[broken]
			if err := one.Store.DB.Redis.Del(ctx, badKey).Err(); err != nil {
				t.Fatal(err)
			}
			if broken == "owner" || broken == "history" {
				if err := one.Store.DB.Redis.HSet(ctx, badKey, "synthetic", "private-type-control").Err(); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := one.Store.DB.Redis.Set(ctx, badKey, "synthetic-wrongtype", 0).Err(); err != nil {
					t.Fatal(err)
				}
			}
			badBefore := one.Store.DB.Redis.Dump(ctx, badKey).Val()
			if broken == "owner" {
				if owner, err := two.bpsResolveOpaqueOwner(ctx, key, reviewOpaqueBody(items, `[]`), ""); err == nil || owner != "" || errors.Is(err, errBPSOpaqueOwnerConflict) {
					t.Fatal("wrongtype proof silently became unknown/fallback or owner conflict")
				}
			}
			if err := two.bpsSaveHistoryWithOpaqueOwners(ctx, key, "resp_protected", reviewOpaqueRecord("owner-A", items), items); err == nil || errors.Is(err, errBPSOpaqueOwnerConflict) {
				t.Fatal("storage wrongtype did not return an unavailable error")
			}
			if one.Store.DB.Redis.Dump(ctx, badKey).Val() != badBefore {
				t.Fatal("save mutated malformed storage key")
			}
			if broken != "history" && one.Store.DB.Redis.Get(ctx, history).Val() != before {
				t.Fatal("Lua error occurred after replacing real encrypted history")
			}
			if one.Store.DB.Redis.Exists(ctx, firstKey).Val() != 0 {
				t.Fatal("Lua error partially created first owner proof")
			}
			if broken != "owner" && one.Store.DB.Redis.Exists(ctx, lastKey).Val() != 0 {
				t.Fatal("Lua error partially created last owner proof")
			}
			if broken != "index" && one.Store.DB.Redis.ZScore(ctx, index, history).Val() != 123 {
				t.Fatal("preflight error removed original history index")
			}
			if broken != "sizes" && one.Store.DB.Redis.HGet(ctx, sizes, history).Val() != "123" {
				t.Fatal("preflight error removed original history sizes")
			}
		})
	}
	t.Run("client_closed", func(t *testing.T) {
		f := newBPSRedisReviewFixture(t)
		p := f.proxy()
		ctx := reviewOpaqueContext(t, inferencePowerPoint)
		item := reviewOpaqueItem("reasoning", "rs_error", "SYNTHETIC_ERROR")
		f.key("key", "response")
		reviewOpaqueProofKey(f, "key", inferencePowerPoint, "SYNTHETIC_ERROR")
		if err := p.Store.DB.Redis.Close(); err != nil {
			t.Fatal(err)
		}
		if owner, err := p.bpsResolveOpaqueOwner(ctx, "key", reviewOpaqueBody([]json.RawMessage{item}, `[]`), ""); err == nil || owner != "" {
			t.Fatal("Redis outage silently became unknown/fallback")
		}
		if err := p.bpsSaveHistoryWithOpaqueOwners(ctx, "key", "response", reviewOpaqueRecord("owner-A", nil), []json.RawMessage{item}); err == nil {
			t.Fatal("Redis outage reported saved history")
		}
	})
}

func TestBPSOpaqueOwnerRedisBoundedHistoryDoesNotPublishProof(t *testing.T) {
	f := newBPSRedisReviewFixture(t)
	one, two := f.proxy(), f.proxy()
	const key = "synthetic-bounded-key"
	cipher := "SYNTHETIC_BOUNDED"
	items := []json.RawMessage{reviewOpaqueItem("reasoning", "rs_bounded", cipher)}
	proof := reviewOpaqueProofKey(f, key, inferencePowerPoint, cipher)
	history := f.key(key, "bounded_response")
	if err := one.bpsSaveHistoryWithOpaqueOwners(t.Context(), key, "bounded_response", reviewOpaqueRecord("owner-A", items), items); err != nil {
		t.Fatal(err)
	}
	if two.Store.DB.Redis.Exists(t.Context(), proof).Val() != 0 || two.Store.DB.Redis.TTL(t.Context(), history).Val() <= 0 {
		t.Fatal("bounded route gained permanent ownership/history state")
	}
	if owner, err := two.bpsResolveOpaqueOwner(reviewOpaqueContext(t, inferencePowerPoint), key, reviewOpaqueBody(items, `[]`), ""); err != nil || owner != "" {
		t.Fatal("bounded save appeared as known opaque owner")
	}
	// Unknown omitted proofs and bounded bypass are separate from wrongtype:
	// no index I/O is needed on the old bounded path, even with a malformed key.
	if err := one.Store.DB.Redis.HSet(t.Context(), proof, "synthetic", "wrongtype").Err(); err != nil {
		t.Fatal(err)
	}
	if owner, err := two.bpsResolveOpaqueOwner(t.Context(), key, reviewOpaqueBody(items, `[]`), "known_previous_owner"); err != nil || owner != "known_previous_owner" {
		t.Fatal("bounded route started consulting new index")
	}
	if strings.Contains(proof, cipher) {
		t.Fatal("Redis key leaked opaque bytes")
	}
}
