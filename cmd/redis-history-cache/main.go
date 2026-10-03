// redis-history-cache warms only the newest bounded continuation state.
// It never writes history into PostgreSQL and never prints request content.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"codex-agent/internal/server"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

type request struct {
	KeyID, ResponseID, Hash string
	At                      time.Time
	Cipher                  []byte
}

func report(value any) { _ = json.NewEncoder(os.Stdout).Encode(value) }

func warm(ctx context.Context, db *server.Database) error {
	rows, err := db.Pool.Query(ctx, `SELECT record->>'key_id',record->>'response_id',
created_at + make_interval(secs => greatest(0,least(coalesce((record->>'latency_ms')::double precision,0),3600000))/1000) FROM request_logs
WHERE created_at>now()-interval '1 hour' AND status='ok'
AND coalesce(record->>'key_id','')<>'' AND coalesce(record->>'response_id','')<>''
ORDER BY created_at DESC LIMIT 32768`)
	if err != nil {
		return err
	}
	var candidates []request
	seen := map[string]bool{}
	for rows.Next() {
		var item request
		if err := rows.Scan(&item.KeyID, &item.ResponseID, &item.At); err != nil {
			rows.Close()
			return err
		}
		sum := sha256.Sum256([]byte(item.KeyID + "\x00" + item.ResponseID))
		item.Hash = hex.EncodeToString(sum[:])
		if !seen[item.Hash] {
			candidates = append(candidates, item)
			seen[item.Hash] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	const budget = 480 << 20 // Leave 32 MiB for new requests during warm-up.
	var selected []request
	var total int64
	var missing, oversize, already int
	for _, item := range candidates {
		if time.Until(item.At.Add(time.Hour)) <= 0 {
			continue
		}
		if n, err := db.Redis.Exists(ctx, db.Namespace+"bps:history:v2:"+item.Hash).Result(); err != nil {
			return err
		} else if n != 0 {
			already++
			continue
		}
		value, err := db.Redis.Get(ctx, db.Namespace+"bps:history:"+item.Hash).Bytes()
		if errors.Is(err, redis.Nil) {
			err = db.Pool.QueryRow(ctx, `SELECT ciphertext FROM gateway_response_history WHERE namespace=$1 AND history_key=$2`, db.Namespace, item.Hash).Scan(&value)
		}
		if errors.Is(err, redis.Nil) || errors.Is(err, pgx.ErrNoRows) {
			missing++
			continue
		}
		if err != nil {
			return err
		}
		if int64(len(value))+total > budget {
			oversize++
			if total > budget*95/100 {
				break
			}
			continue
		}
		item.Cipher = value
		selected = append(selected, item)
		total += int64(len(value))
	}
	warmed := 0
	for i := len(selected) - 1; i >= 0; i-- {
		item := selected[i]
		if time.Until(item.At.Add(time.Hour)) <= 0 {
			continue
		}
		if err := server.WarmBPSHistory(ctx, db, item.KeyID, item.ResponseID, item.Cipher, item.At.Add(time.Hour)); err != nil {
			if errors.Is(err, server.ErrBPSHistoryAlreadyCached) {
				already++
				continue
			}
			return err
		}
		warmed++
	}
	report(map[string]any{"status": "complete", "action": "warm", "candidates": len(candidates), "warmed": warmed, "already_cached": already,
		"missing": missing, "budget_skipped": oversize, "selected_ciphertext_bytes": total, "retention_seconds": 3600, "selected_budget_bytes": budget})
	return nil
}

func prune(ctx context.Context, db *server.Database) error {
	// The caller must first stop every legacy writer and let the absolute
	// compatibility-read deadline elapse. Only v1 hashes are eligible here.
	keyHash := regexp.MustCompile(`^[a-f0-9]{64}$`)
	var removed int64
	for _, suffix := range []string{"bps:history:", "bps:opaque-owner:"} {
		prefix := db.Namespace + suffix
		var cursor uint64
		for {
			keys, next, err := db.Redis.Scan(ctx, cursor, prefix+"*", 256).Result()
			if err != nil {
				return err
			}
			var eligible []string
			for _, key := range keys {
				if keyHash.MatchString(strings.TrimPrefix(key, prefix)) {
					eligible = append(eligible, key)
				}
			}
			if len(eligible) > 0 {
				n, err := db.Redis.Unlink(ctx, eligible...).Result()
				if err != nil {
					return err
				}
				removed += n
			}
			cursor = next
			if cursor == 0 {
				break
			}
			time.Sleep(time.Millisecond)
		}
	}
	if err := db.Redis.Unlink(ctx, db.Namespace+"bps:history:index", db.Namespace+"bps:history:sizes", db.Namespace+"bps:history:accounting").Err(); err != nil {
		return err
	}
	report(map[string]any{"status": "complete", "action": "prune-legacy-redis", "removed_keys": removed, "v2_keys_untouched": true})
	return nil
}

func run() error {
	action := flag.String("action", "warm", "warm or prune-legacy-redis")
	flag.Parse()
	if *action != "warm" && *action != "prune-legacy-redis" {
		return errors.New("unknown action")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	db, err := server.OpenDatabase(ctx, os.Getenv("DATABASE_URL"), os.Getenv("CREDENTIAL_ENCRYPTION_KEY"), os.Getenv("CREDENTIAL_ENCRYPTION_KEY_ID"))
	if err != nil {
		return errors.New("database unavailable")
	}
	defer db.Close()
	if err := db.ConfigureRedis(os.Getenv("REDIS_URL")); err != nil {
		return errors.New("Redis unavailable")
	}
	if *action == "warm" {
		return warm(ctx, db)
	}
	return prune(ctx, db)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "bounded history maintenance failed; inspect isolated validation and service health")
		os.Exit(1)
	}
}
