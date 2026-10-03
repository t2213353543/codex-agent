package server

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// This fixture uses only TEST_REDIS_URL and synthetic encryption. No PostgreSQL
// connection, account fixture, production namespace or caller config is needed.
// Every cleanup key is explicitly registered; it never scans or flushes Redis.
type bpsRedisReviewFixture struct {
	t         *testing.T
	url       string
	namespace string
	mu        sync.Mutex
	keys      map[string]bool
	clients   []*redis.Client
}

func newBPSRedisReviewFixture(t *testing.T) *bpsRedisReviewFixture {
	t.Helper()
	address := os.Getenv("TEST_REDIS_URL")
	if address == "" {
		t.Skip("TEST_REDIS_URL is required for isolated BPS Redis persistence tests")
	}
	f := &bpsRedisReviewFixture{t: t, url: address,
		namespace: "codex-agent:integration:bps-test:" + newID("") + ":", keys: map[string]bool{}}
	f.track(f.prefix() + "index")
	f.track(f.prefix() + "sizes")
	f.track(f.prefix() + "accounting")
	f.track(f.namespace + "bps:opaque-owner:v2:index")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		options, err := redis.ParseURL(f.url)
		if err != nil {
			t.Error("cannot parse isolated Redis address for exact-key cleanup")
			return
		}
		options.MaxRetries = 0
		client := redis.NewClient(options)
		defer client.Close()
		f.mu.Lock()
		keys := make([]string, 0, len(f.keys))
		for key := range f.keys {
			if !strings.HasPrefix(key, f.namespace) {
				f.mu.Unlock()
				t.Error("refusing cleanup outside exact test namespace")
				return
			}
			keys = append(keys, key)
		}
		clients := append([]*redis.Client(nil), f.clients...)
		f.mu.Unlock()
		if len(keys) != 0 {
			if err := client.Del(ctx, keys...).Err(); err != nil {
				t.Error("isolated Redis exact-key cleanup failed")
			}
		}
		for _, previous := range clients {
			_ = previous.Close()
		}
	})
	return f
}

func (f *bpsRedisReviewFixture) prefix() string { return f.namespace + "bps:history:v2:" }

func (f *bpsRedisReviewFixture) track(key string) string {
	f.mu.Lock()
	f.keys[key] = true
	f.mu.Unlock()
	return key
}

func (f *bpsRedisReviewFixture) key(keyID, responseID string) string {
	return f.track(f.prefix() + bpsHistoryKey(keyID, responseID))
}

func (f *bpsRedisReviewFixture) proxy() *Proxy {
	f.t.Helper()
	block, err := aes.NewCipher(bytes.Repeat([]byte{0x53}, 32))
	if err != nil {
		f.t.Fatal("cannot initialize synthetic cipher")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		f.t.Fatal("cannot initialize synthetic GCM")
	}
	db := &Database{cipher: aead, KeyID: "bps-review-synthetic"}
	if err := db.ConfigureRedis(f.url); err != nil {
		f.t.Fatal("cannot configure isolated Redis")
	}
	// ConfigureRedis may read a deployment prefix. Override it before any I/O.
	db.Namespace = f.namespace
	f.mu.Lock()
	f.clients = append(f.clients, db.Redis)
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(f.t.Context(), 5*time.Second)
	defer cancel()
	if err := db.Redis.Ping(ctx).Err(); err != nil {
		f.t.Fatal("isolated Redis is unavailable")
	}
	return &Proxy{Store: Store{DB: db}, BPS: &bpsRuntime{}}
}

func bpsRedisReviewRecord() bpsHistoryRecord {
	return bpsHistoryRecord{
		Owner: "synthetic-account-owner:revision-1", Session: "synthetic-private-session",
		Request: json.RawMessage(`{"model":"gpt-test","input":[{"role":"user","content":"synthetic-private-prompt"}],"instructions":"synthetic-instruction"}`),
		Output: []json.RawMessage{
			json.RawMessage(`{"type":"reasoning","id":"rs_test","encrypted_content":"synthetic-upstream-cipher","summary":[]}`),
			json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"synthetic-private-answer"}]}`),
		},
	}
}

func TestBPSRedisHistoryPersistsAcrossFreshProxyAndClient(t *testing.T) {
	f := newBPSRedisReviewFixture(t)
	first := f.proxy()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	const keyID, responseID = "synthetic-key-a", "resp_synthetic_persist"
	key := f.key(keyID, responseID)
	record := bpsRedisReviewRecord()
	if err := first.bpsSaveHistory(ctx, keyID, responseID, record); err != nil {
		t.Fatal("save synthetic Redis history:", err)
	}
	ciphertext, err := first.Store.DB.Redis.Get(ctx, key).Bytes()
	if err != nil {
		t.Fatal("saved ciphertext unavailable")
	}
	for _, text := range []string{record.Owner, record.Session, "synthetic-private-prompt", "synthetic-private-answer", "synthetic-upstream-cipher"} {
		if bytes.Contains(ciphertext, []byte(text)) {
			t.Fatal("private synthetic history was stored as plaintext")
		}
	}
	for _, current := range []string{key, f.prefix() + "index", f.prefix() + "sizes"} {
		if ttl, err := first.Store.DB.Redis.TTL(ctx, current).Result(); err != nil || ttl <= 0 || ttl > bpsHistoryTTL {
			t.Fatalf("history/index TTL missing or unbounded: %s %v", ttl, err)
		}
	}
	if err := first.Store.DB.Redis.Close(); err != nil {
		t.Fatal("close first Redis client:", err)
	}
	first = nil
	second := f.proxy() // no in-memory history, new cipher object and Redis pool
	loaded, err := second.bpsLoadHistory(ctx, keyID, responseID)
	if err != nil || loaded.Owner != record.Owner || loaded.Session != record.Session ||
		!bytes.Equal(loaded.Request, record.Request) || !reflect.DeepEqual(loaded.Output, record.Output) || loaded.Expires <= time.Now().Unix() {
		t.Fatalf("new process-equivalent state did not load exact Redis history: %v", err)
	}
	if _, err := second.bpsLoadHistory(ctx, "synthetic-key-b", responseID); !errors.Is(err, errBPSHistoryMissing) {
		t.Fatalf("cross-key history was readable: %v", err)
	}
	raw, session, owner, err := second.bpsExpandHistory(ctx, keyID,
		[]byte(`{"previous_response_id":"resp_synthetic_persist","input":[{"role":"user","content":"next"}]}`), "")
	if err != nil || session != record.Session || owner != record.Owner {
		t.Fatalf("restart continuation did not retain owner/session: %v", err)
	}
	var expanded struct{ Input []json.RawMessage }
	if json.Unmarshal(raw, &expanded) != nil || len(expanded.Input) != 4 || !bytes.Contains(raw, []byte("synthetic-upstream-cipher")) {
		t.Fatal("restart continuation lost native history")
	}
}

func TestBPSRedisHistoryCorruptionExpiryAndMissingFailClosed(t *testing.T) {
	f := newBPSRedisReviewFixture(t)
	p := f.proxy()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	const keyID, responseID = "synthetic-key", "resp_synthetic_corrupt"
	key := f.key(keyID, responseID)
	record := bpsRedisReviewRecord()
	if err := p.bpsSaveHistory(ctx, keyID, responseID, record); err != nil {
		t.Fatal(err)
	}
	encoded, err := p.Store.DB.Redis.Get(ctx, key).Bytes()
	if err != nil || len(encoded) < 32 {
		t.Fatal("fixture ciphertext is missing")
	}
	encoded[len(encoded)-1] ^= 1
	if err := p.Store.DB.Redis.Set(ctx, key, encoded, bpsHistoryTTL).Err(); err != nil {
		t.Fatal("cannot install synthetic tampered ciphertext")
	}
	if _, err := p.bpsLoadHistory(ctx, keyID, responseID); err == nil || errors.Is(err, errBPSHistoryMissing) {
		t.Fatalf("ciphertext authentication failure was not distinguished from missing state: %v", err)
	}
	if _, _, _, err := p.bpsExpandHistory(ctx, keyID, []byte(`{"previous_response_id":"resp_synthetic_corrupt","input":[]}`), ""); err == nil {
		t.Fatal("tampered history was expanded")
	}
	for _, payload := range []json.RawMessage{json.RawMessage(`null`), json.RawMessage(`{}`), json.RawMessage(`invalid-json`)} {
		sealed, err := p.Store.DB.Encrypt(payload)
		if err != nil || p.Store.DB.Redis.Set(ctx, key, sealed, bpsHistoryTTL).Err() != nil {
			t.Fatal("cannot seed synthetic malformed history")
		}
		if _, err := p.bpsLoadHistory(ctx, keyID, responseID); !errors.Is(err, errBPSHistoryMissing) {
			t.Fatalf("malformed persisted history did not fail closed: %v", err)
		}
	}
	record.Expires = time.Now().Add(-time.Second).Unix()
	plain, _ := json.Marshal(record)
	sealed, err := p.Store.DB.Encrypt(plain)
	if err != nil || p.Store.DB.Redis.Set(ctx, key, sealed, bpsHistoryTTL).Err() != nil {
		t.Fatal("cannot seed expired logical history")
	}
	if _, err := p.bpsLoadHistory(ctx, keyID, responseID); !errors.Is(err, errBPSHistoryMissing) {
		t.Fatalf("logical history expiry was ignored: %v", err)
	}
	if err := p.Store.DB.Redis.Del(ctx, key).Err(); err != nil {
		t.Fatal("cannot delete exact synthetic history key")
	}
	for _, id := range []string{responseID, "", strings.Repeat("x", 1025)} {
		if _, err := p.bpsLoadHistory(ctx, keyID, id); !errors.Is(err, errBPSHistoryMissing) {
			t.Fatalf("missing/empty/oversized history id was accepted: %v", err)
		}
	}
	if err := p.bpsSaveHistory(ctx, keyID, "", record); err == nil {
		t.Fatal("empty response id was persisted")
	}
}

func TestBPSRedisHistoryIndexAndEntryLimitRemainBounded(t *testing.T) {
	f := newBPSRedisReviewFixture(t)
	p := f.proxy()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const keyID = "synthetic-capacity-key"
	record := bpsRedisReviewRecord()
	ids := make([]string, bpsHistoryEntries+4)
	for i := range ids {
		ids[i] = fmt.Sprintf("resp_capacity_%d", i)
		f.key(keyID, ids[i])
	}
	// Seed small synthetic encrypted entries in batches so exercising the real
	// 32768-entry bound does not require that many network round trips.
	record.Mode = inferencePowerPoint
	record.Expires = time.Now().Add(bpsHistoryTTL).Unix()
	raw, _ := json.Marshal(record)
	sealed, err := p.Store.DB.Encrypt(raw)
	if err != nil {
		t.Fatal(err)
	}
	index, sizes := f.prefix()+"index", f.prefix()+"sizes"
	for start := 0; start < bpsHistoryEntries; start += 1024 {
		pipe := p.Store.DB.Redis.Pipeline()
		for i := start; i < start+1024 && i < bpsHistoryEntries; i++ {
			key := f.key(keyID, ids[i])
			pipe.Set(ctx, key, sealed, bpsHistoryTTL)
			pipe.ZAdd(ctx, index, redis.Z{Score: float64(time.Now().Add(30 * time.Minute).Unix()), Member: key})
			pipe.HSet(ctx, sizes, key, len(sealed))
		}
		if _, err := pipe.Exec(ctx); err != nil {
			t.Fatal("seed synthetic entry-capacity fixture:", err)
		}
	}
	for _, id := range ids[bpsHistoryEntries:] {
		if err := p.bpsSaveHistory(ctx, keyID, id, record); err != nil {
			t.Fatalf("bounded cache rejected a fresh small entry while evicting older entries: %v", err)
		}
	}
	count, err := p.Store.DB.Redis.ZCard(ctx, index).Result()
	if err != nil || count != bpsHistoryEntries {
		t.Fatalf("entry index is not bounded at its configured capacity: %d %v", count, err)
	}
	ledger, err := p.Store.DB.Redis.HGetAll(ctx, sizes).Result()
	if err != nil || len(ledger) != bpsHistoryEntries {
		t.Fatalf("size ledger diverged from entry bound: %d %v", len(ledger), err)
	}
	var total int
	for key, value := range ledger {
		if !strings.HasPrefix(key, f.namespace) {
			t.Fatal("size ledger crossed the test namespace")
		}
		size, err := strconv.Atoi(value)
		if err != nil || size <= 0 {
			t.Fatal("invalid encrypted-size ledger entry")
		}
		total += size
	}
	if total > bpsHistoryBudget {
		t.Fatal("history byte ledger exceeds its budget")
	}
	if _, err := p.bpsLoadHistory(ctx, keyID, ids[len(ids)-1]); err != nil {
		t.Fatalf("newest cache entry was evicted on insert: %v", err)
	}
}

func TestBPSRedisHistoryPrunesExpiredAndOverBudgetIndexEntries(t *testing.T) {
	f := newBPSRedisReviewFixture(t)
	p := f.proxy()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	index, sizes := f.prefix()+"index", f.prefix()+"sizes"
	old := f.key("synthetic-key", "resp_expired_index")
	if err := p.Store.DB.Redis.Set(ctx, old, "synthetic-expired", time.Minute).Err(); err != nil {
		t.Fatal("cannot seed isolated index fixture")
	}
	if err := p.Store.DB.Redis.ZAdd(ctx, index, redis.Z{Score: float64(time.Now().Add(-time.Second).Unix()), Member: old}).Err(); err != nil {
		t.Fatal("cannot seed expired index score")
	}
	if err := p.Store.DB.Redis.HSet(ctx, sizes, old, 17).Err(); err != nil {
		t.Fatal("cannot seed expired size entry")
	}
	const id = "resp_after_prune"
	f.key("synthetic-key", id)
	if err := p.bpsSaveHistory(ctx, "synthetic-key", id, bpsRedisReviewRecord()); err != nil {
		t.Fatal(err)
	}
	if n, err := p.Store.DB.Redis.Exists(ctx, old).Result(); err != nil || n != 0 {
		t.Fatal("expired indexed payload was not removed")
	}
	if _, err := p.Store.DB.Redis.ZScore(ctx, index, old).Result(); !errors.Is(err, redis.Nil) {
		t.Fatal("expired index member remained")
	}
	if present, err := p.Store.DB.Redis.HExists(ctx, sizes, old).Result(); err != nil || present {
		t.Fatal("expired size ledger member remained")
	}
	// Exercise the production Lua byte-budget branch without allocating 128 MiB
	// of fixture content: the seeded size ledger intentionally fills the budget.
	heavy := f.key("synthetic-key", "resp_large_ledger")
	if err := p.Store.DB.Redis.Set(ctx, heavy, "small-synthetic-size-fixture", bpsHistoryTTL).Err(); err != nil {
		t.Fatal("cannot seed byte-budget fixture")
	}
	if err := p.Store.DB.Redis.ZAdd(ctx, index, redis.Z{Score: float64(time.Now().Add(time.Minute).Unix()), Member: heavy}).Err(); err != nil {
		t.Fatal("cannot seed byte-budget score")
	}
	if err := p.Store.DB.Redis.HSet(ctx, sizes, heavy, bpsHistoryBudget).Err(); err != nil {
		t.Fatal("cannot seed byte-budget ledger")
	}
	f.key("synthetic-key", "resp_after_budget")
	if err := p.bpsSaveHistory(ctx, "synthetic-key", "resp_after_budget", bpsRedisReviewRecord()); err != nil {
		t.Fatal(err)
	}
	if n, err := p.Store.DB.Redis.Exists(ctx, heavy).Result(); err != nil || n != 0 {
		t.Fatal("over-budget old payload was not evicted")
	}
	if _, err := p.bpsLoadHistory(ctx, "synthetic-key", "resp_after_budget"); err != nil {
		t.Fatal("new history did not survive byte-budget pruning")
	}
}
