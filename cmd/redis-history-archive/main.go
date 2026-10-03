// redis-history-archive moves permanent encrypted conversation snapshots to PG.
// Redis removal is opt-in, after a committed, byte-identical durable copy.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const lockSQL = `SELECT pg_advisory_xact_lock(hashtextextended($1 || ':' || $2, 92426001))`
const snapshotSQL = `if redis.call('PTTL',KEYS[1]) ~= -1 then return false end return redis.call('GET',KEYS[1])`
const retireSQL = `if redis.call('PTTL',KEYS[1]) == -1 and redis.call('GET',KEYS[1]) == ARGV[1] then return redis.call('UNLINK',KEYS[1]) end return 0`

var historyHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

type counters struct {
	scanned, archived, verified, retired, conflicts, skipped, failed, bytes, freed atomic.Int64
}

func (c *counters) report(status string) {
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"at": time.Now().UTC().Format(time.RFC3339), "status": status,
		"scanned": c.scanned.Load(), "archived": c.archived.Load(),
		"verified": c.verified.Load(), "retired": c.retired.Load(),
		"conflicts": c.conflicts.Load(), "skipped": c.skipped.Load(), "failed": c.failed.Load(),
		"ciphertext_bytes": c.bytes.Load(), "removed_redis_value_bytes": c.freed.Load(),
	})
}

// archiveOne never overwrites a newer PG value. It holds the same per-record
// lock as gateway writers, verifies the committed payload, then uses a Redis
// CAS so a concurrent legacy writer or changed TTL cannot lose its new value.
func archiveOne(ctx context.Context, db *pgxpool.Pool, r *redis.Client, namespace, key string, retire bool, c *counters) error {
	id := strings.TrimPrefix(key, namespace+"bps:history:")
	if !historyHash.MatchString(id) {
		c.skipped.Add(1)
		return nil
	}
	// Capture lifetime and bytes atomically. A separate PTTL/GET pair can
	// archive a bounded replacement that arrived between those two commands.
	text, err := r.Eval(ctx, snapshotSQL, []string{key}).Text()
	if errors.Is(err, redis.Nil) {
		c.skipped.Add(1)
		return nil
	}
	if err != nil {
		return err
	}
	value := []byte(text)
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, lockSQL, namespace, id); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO gateway_response_history(namespace,history_key,ciphertext) VALUES($1,$2,$3) ON CONFLICT(namespace,history_key) DO NOTHING`, namespace, id, value)
	if err != nil {
		return err
	}
	var durable []byte
	if err = tx.QueryRow(ctx, `SELECT ciphertext FROM gateway_response_history WHERE namespace=$1 AND history_key=$2`, namespace, id).Scan(&durable); err != nil {
		return err
	}
	if !bytes.Equal(durable, value) {
		c.conflicts.Add(1)
		return nil
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	c.archived.Add(tag.RowsAffected())
	c.verified.Add(1)
	c.bytes.Add(int64(len(value)))
	if retire {
		removed, err := r.Eval(ctx, retireSQL, []string{key}, value).Int64()
		if err != nil {
			return err
		}
		c.retired.Add(removed)
		c.freed.Add(removed * int64(len(value)))
	}
	return nil
}

func run() error {
	apply := flag.Bool("apply", false, "archive permanent history; without this flag only count candidate keys")
	retire := flag.Bool("retire", false, "CAS-remove archived Redis values; deploy PG-capable readers to every generation first")
	workers := flag.Int("workers", 2, "parallel archive workers, 1 through 4")
	delay := flag.Duration("delay", 3*time.Millisecond, "delay after each record per worker")
	limit := flag.Int64("limit", 0, "maximum matching keys; zero scans the full namespace")
	flag.Parse()
	if *retire && !*apply || *workers < 1 || *workers > 4 || *delay < 0 || *limit < 0 {
		return errors.New("invalid archive options")
	}
	namespace := os.Getenv("REDIS_KEY_PREFIX")
	if namespace == "" {
		namespace = os.Getenv("REDIS_NAMESPACE")
	}
	if namespace == "" || strings.ContainsAny(namespace, "*?[]\\") {
		return errors.New("explicit literal Redis namespace is required")
	}
	namespace = strings.TrimRight(namespace, ":") + ":"
	options, err := redis.ParseURL(os.Getenv("REDIS_URL"))
	if err != nil {
		return errors.New("Redis URL unavailable")
	}
	options.ReadTimeout, options.WriteTimeout = 30*time.Second, 30*time.Second
	options.PoolSize, options.MaxRetries = *workers+1, 0
	r := redis.NewClient(options)
	defer r.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var db *pgxpool.Pool
	if *apply {
		config, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
		if err != nil {
			return errors.New("database URL unavailable")
		}
		config.MaxConns = int32(*workers)
		config.ConnConfig.RuntimeParams["application_name"] = "redis-history-archive"
		config.ConnConfig.RuntimeParams["statement_timeout"] = "60000"
		config.ConnConfig.RuntimeParams["lock_timeout"] = "5000"
		config.ConnConfig.RuntimeParams["synchronous_commit"] = "on"
		db, err = pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			return errors.New("database connection unavailable")
		}
		defer db.Close()
		var exists bool
		if err = db.QueryRow(ctx, `SELECT to_regclass('gateway_response_history') IS NOT NULL`).Scan(&exists); err != nil || !exists {
			return errors.New("durable history migration must be installed first")
		}
	}
	c := new(counters)
	jobs := make(chan string, *workers)
	var wg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for key := range jobs {
				if *apply {
					one, done := context.WithTimeout(ctx, 90*time.Second)
					err := archiveOne(one, db, r, namespace, key, *retire, c)
					done()
					if err != nil {
						c.failed.Add(1)
						cancel()
						return
					}
				}
				if *delay > 0 {
					select {
					case <-ctx.Done():
						return
					case <-time.After(*delay):
					}
				}
			}
		}()
	}
	reportDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.report("running")
			case <-reportDone:
				return
			}
		}
	}()
	var cursor uint64
	var scanErr error
scan:
	for {
		var keys []string
		keys, cursor, scanErr = r.Scan(ctx, cursor, namespace+"bps:history:*", 64).Result()
		if scanErr != nil {
			break
		}
		for _, key := range keys {
			if !historyHash.MatchString(strings.TrimPrefix(key, namespace+"bps:history:")) {
				continue
			}
			if *limit > 0 && c.scanned.Load() >= *limit {
				break scan
			}
			select {
			case jobs <- key:
				c.scanned.Add(1)
			case <-ctx.Done():
				break scan
			}
		}
		if cursor == 0 {
			break
		}
	}
	close(jobs)
	wg.Wait()
	close(reportDone)
	if scanErr != nil || ctx.Err() != nil || c.failed.Load() > 0 {
		c.report("incomplete")
		return errors.New("archive incomplete; committed copies retained, safe to rerun")
	}
	c.report("complete")
	return nil
}

func main() {
	if err := run(); err != nil {
		// Do not print driver errors, URLs, key names or payloads.
		fmt.Fprintln(os.Stderr, "history archive failed; inspect aggregate progress and database availability")
		os.Exit(1)
	}
}
