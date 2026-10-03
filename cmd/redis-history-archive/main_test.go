package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type mutationHook struct {
	before func()
	script string
}

func (h *mutationHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) { return next(ctx, network, addr) }
}
func (h *mutationHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "eval" && len(cmd.Args()) > 1 && cmd.Args()[1] == h.script && h.before != nil {
			before := h.before
			h.before = nil
			before()
		}
		return next(ctx, cmd)
	}
}
func (h *mutationHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func TestHistoryArchivePGRedis(t *testing.T) {
	dsn, redisURL := os.Getenv("TEST_DATABASE_URL"), os.Getenv("TEST_REDIS_URL")
	if dsn == "" || redisURL == "" {
		t.Skip("isolated TEST_DATABASE_URL and TEST_REDIS_URL required")
	}
	ctx := context.Background()
	salt := make([]byte, 12)
	_, _ = rand.Read(salt)
	name := "history_archive_" + hex.EncodeToString(salt)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("isolated database unavailable")
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+name); err != nil {
		t.Fatal("isolated schema unavailable")
	}
	t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP SCHEMA "+name+" CASCADE") })
	config, _ := pgxpool.ParseConfig(dsn)
	config.ConnConfig.RuntimeParams["search_path"] = name
	db, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("isolated connection failed")
	}
	defer db.Close()
	_, err = db.Exec(ctx, `CREATE TABLE gateway_response_history(namespace text NOT NULL,history_key text NOT NULL,ciphertext bytea NOT NULL,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),PRIMARY KEY(namespace,history_key))`)
	if err != nil {
		t.Fatal("isolated table failed")
	}
	options, _ := redis.ParseURL(redisURL)
	r := redis.NewClient(options)
	defer r.Close()
	ns := "codex-agent:integration:" + name + ":"
	id := hex.EncodeToString(append(append([]byte{}, salt...), make([]byte, 20)...))
	key := ns + "bps:history:" + id
	t.Cleanup(func() { _ = r.Del(ctx, key).Err() })
	value := bytes.Repeat([]byte{0x80, 0x00, 0xff, 0x31}, 20000)
	put := func(ttl time.Duration) {
		t.Helper()
		if err := r.Set(ctx, key, value, ttl).Err(); err != nil {
			t.Fatal("isolated Redis write failed")
		}
	}
	t.Run("archive_preserves_redis_until_retirement", func(t *testing.T) {
		put(0)
		c := new(counters)
		if err := archiveOne(ctx, db, r, ns, key, false, c); err != nil {
			t.Fatal("archive failed")
		}
		var got []byte
		if err := db.QueryRow(ctx, `SELECT ciphertext FROM gateway_response_history WHERE namespace=$1 AND history_key=$2`, ns, id).Scan(&got); err != nil || !bytes.Equal(got, value) {
			t.Fatal("durable ciphertext changed")
		}
		redisValue, err := r.Get(ctx, key).Bytes()
		if err != nil || !bytes.Equal(redisValue, value) || c.archived.Load() != 1 {
			t.Fatal("archive-only changed Redis")
		}
	})
	t.Run("retirement_is_idempotent", func(t *testing.T) {
		c := new(counters)
		if err := archiveOne(ctx, db, r, ns, key, true, c); err != nil || c.retired.Load() != 1 || c.archived.Load() != 0 {
			t.Fatal("verified existing durable value not retired")
		}
		if err := archiveOne(ctx, db, r, ns, key, true, c); err != nil || c.retired.Load() != 1 {
			t.Fatal("repeat retirement failed")
		}
	})
	t.Run("bounded_history_is_untouched", func(t *testing.T) {
		put(time.Hour)
		c := new(counters)
		if err := archiveOne(ctx, db, r, ns, key, true, c); err != nil || c.skipped.Load() != 1 || c.retired.Load() != 0 {
			t.Fatal("bounded key was archived or removed")
		}
		if ttl := r.TTL(ctx, key).Val(); ttl < 59*time.Minute || ttl > time.Hour {
			t.Fatal("bounded TTL changed")
		}
	})
	t.Run("newer_durable_value_is_never_overwritten", func(t *testing.T) {
		put(0)
		if _, err := db.Exec(ctx, `UPDATE gateway_response_history SET ciphertext=$3 WHERE namespace=$1 AND history_key=$2`, ns, id, []byte("newer")); err != nil {
			t.Fatal("fixture update failed")
		}
		c := new(counters)
		if err := archiveOne(ctx, db, r, ns, key, true, c); err != nil || c.conflicts.Load() != 1 || r.Exists(ctx, key).Val() != 1 {
			t.Fatal("conflicting state was replaced or removed")
		}
		_, _ = db.Exec(ctx, `UPDATE gateway_response_history SET ciphertext=$3 WHERE namespace=$1 AND history_key=$2`, ns, id, value)
	})
	t.Run("concurrent_redis_replacement_survives_cas", func(t *testing.T) {
		put(0)
		other := redis.NewClient(options)
		defer other.Close()
		r.AddHook(&mutationHook{script: retireSQL, before: func() {
			if err := other.Set(ctx, key, "concurrent", 0).Err(); err != nil {
				t.Fatal("concurrent fixture write failed")
			}
		}})
		c := new(counters)
		if err := archiveOne(ctx, db, r, ns, key, true, c); err != nil || c.retired.Load() != 0 || r.Get(ctx, key).Val() != "concurrent" {
			t.Fatal("concurrent replacement was lost")
		}
	})
	t.Run("bounded_replacement_is_not_archived", func(t *testing.T) {
		put(0)
		other := redis.NewClient(options)
		defer other.Close()
		r.AddHook(&mutationHook{script: snapshotSQL, before: func() {
			if err := other.Set(ctx, key, "bounded replacement", time.Hour).Err(); err != nil {
				t.Fatal("bounded fixture write failed")
			}
		}})
		c := new(counters)
		if err := archiveOne(ctx, db, r, ns, key, true, c); err != nil || c.skipped.Load() != 1 || c.verified.Load() != 0 || r.Get(ctx, key).Val() != "bounded replacement" {
			t.Fatal("bounded replacement was archived or removed")
		}
	})
}
