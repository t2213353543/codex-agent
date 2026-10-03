package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestOfficialHistorySharesPowerPointEvictionAndModeIsolation(t *testing.T) {
	p := &Proxy{BPS: &bpsRuntime{}}
	ctx := withInferenceMode(t.Context(), inferenceAppServer)
	for i := 0; i < bpsHistoryEntries; i++ {
		if err := p.bpsSaveHistory(ctx, "key", fmt.Sprint(i), bpsRedisReviewRecord()); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.bpsSaveHistory(t.Context(), "key", "bps-new", bpsRedisReviewRecord()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.bpsLoadHistory(ctx, "key", "0"); !errors.Is(err, errBPSHistoryMissing) {
		t.Fatal("official history escaped the shared entry budget")
	}
	for _, id := range []string{"1", "513"} {
		row, err := p.bpsLoadHistory(ctx, "key", id)
		if err != nil || row.Expires <= time.Now().Unix() {
			t.Fatalf("retained official history unavailable or expired: %v", err)
		}
		for _, other := range []context.Context{t.Context(), withInferenceMode(t.Context(), inferenceCodexCLI), withInferenceMode(t.Context(), "invalid")} {
			if _, err := p.bpsLoadHistory(other, "key", id); !errors.Is(err, errBPSHistoryMissing) {
				t.Fatal("history escaped mode isolation")
			}
		}
		if _, err := p.bpsLoadHistory(ctx, "other-key", id); !errors.Is(err, errBPSHistoryMissing) {
			t.Fatal("history escaped key isolation")
		}
	}
	if len(p.BPS.history.items) != bpsHistoryEntries || p.BPS.history.bytes > bpsHistoryBudget {
		t.Fatal("shared memory cache exceeded its configured budget")
	}
	if err := p.bpsSaveHistory(withInferenceMode(t.Context(), "invalid"), "key", "invalid", bpsRedisReviewRecord()); err == nil {
		t.Fatal("unsupported mode stored")
	}
}

func TestOfficialHistoryLargeContinuationAndBoundedExpiry(t *testing.T) {
	p := &Proxy{BPS: &bpsRuntime{}}
	ctx := withInferenceMode(t.Context(), inferenceCodexCLI)
	record := bpsRedisReviewRecord()
	record.Request = json.RawMessage(`{"model":"test","input":[{"role":"user","content":"` + strings.Repeat("x", maxBody) + `"}]}`)
	if err := p.bpsSaveHistory(ctx, "key", "large", record); err != nil {
		t.Fatal(err)
	}
	if _, err := p.bpsLoadHistory(ctx, "key", "large"); err != nil {
		t.Fatal(err)
	}
	expanded, session, owner, err := p.bpsExpandHistory(ctx, "key", []byte(`{"previous_response_id":"large","input":[]}`), "")
	if err != nil || len(expanded) <= maxBody || session != record.Session || owner != record.Owner {
		t.Fatalf("large official continuation failed: %v", err)
	}
	if err := p.bpsSaveHistory(t.Context(), "key", "large-bps", record); err == nil {
		t.Fatal("PowerPoint bound changed")
	}
	if _, _, _, err := p.bpsExpandHistory(ctx, "key", []byte(`{"previous_response_id":"large","input":[]}`), "different"); err == nil {
		t.Fatal("cross session accepted")
	}
	// All newly saved history has a finite logical lifetime, including official modes.
	record.Mode = inferenceCodexCLI
	record.Expires = time.Now().Add(-time.Second).Unix()
	record.Request = json.RawMessage(`{"input":[]}`)
	raw, _ := json.Marshal(record)
	p.BPS.history.items[bpsHistoryKey("key", "old")] = raw
	if _, err := p.bpsLoadHistory(ctx, "key", "old"); !errors.Is(err, errBPSHistoryMissing) {
		t.Fatal("expired legacy history accepted")
	}
}

func TestOfficialRedisHistoryHasTTLAndSharesByteBudget(t *testing.T) {
	f := newBPSRedisReviewFixture(t)
	p := f.proxy()
	ctx := withInferenceMode(t.Context(), inferenceAppServer)
	key := f.key("synthetic-key", "official")
	record := bpsRedisReviewRecord()
	if err := p.bpsSaveHistory(ctx, "synthetic-key", "official", record); err != nil {
		t.Fatal(err)
	}
	if ttl, err := p.Store.DB.Redis.TTL(ctx, key).Result(); err != nil || ttl <= 0 || ttl > bpsHistoryTTL {
		t.Fatalf("official entry lacks a bounded TTL: %v %v", ttl, err)
	}
	if _, err := p.Store.DB.Redis.ZScore(ctx, f.prefix()+"index", key).Result(); err != nil {
		t.Fatal("official entry is not tracked in shared index")
	}
	if present, err := p.Store.DB.Redis.HExists(ctx, f.prefix()+"sizes", key).Result(); err != nil || !present {
		t.Fatal("official entry is not counted in shared byte budget")
	}
	fresh := f.proxy()
	loaded, err := fresh.bpsLoadHistory(ctx, "synthetic-key", "official")
	if err != nil || loaded.Expires <= time.Now().Unix() || loaded.Owner != record.Owner || loaded.Session != record.Session {
		t.Fatalf("fresh proxy lost bounded official history: %v", err)
	}
	if _, err := fresh.bpsLoadHistory(t.Context(), "synthetic-key", "official"); !errors.Is(err, errBPSHistoryMissing) {
		t.Fatal("official history entered PowerPoint mode")
	}
	// Synthetic accounting exercises the 512 MiB budget without allocating it.
	if err := p.Store.DB.Redis.HSet(ctx, f.prefix()+"sizes", key, bpsHistoryBudget).Err(); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.DB.Redis.ZAdd(ctx, f.prefix()+"index", redis.Z{Member: key, Score: float64(time.Now().Add(time.Minute).Unix())}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.DB.Redis.Del(ctx, f.prefix()+"accounting").Err(); err != nil {
		t.Fatal(err)
	}
	f.key("synthetic-key", "bps-next")
	if err := p.bpsSaveHistory(t.Context(), "synthetic-key", "bps-next", record); err != nil {
		t.Fatal(err)
	}
	if exists, err := p.Store.DB.Redis.Exists(ctx, key).Result(); err != nil || exists != 0 {
		t.Fatal("official entry escaped cross-mode byte-budget eviction:", err)
	}
	if _, err := fresh.bpsLoadHistory(ctx, "synthetic-key", "official"); !errors.Is(err, errBPSHistoryMissing) {
		t.Fatal("evicted official history remained available:", err)
	}
	if _, err := fresh.bpsLoadHistory(t.Context(), "synthetic-key", "bps-next"); err != nil {
		t.Fatal("new history did not survive shared-budget eviction:", err)
	}
}
