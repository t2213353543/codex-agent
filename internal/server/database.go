package server

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"codex-agent/migrations"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

var migrationFiles = migrations.Files

var errVersionConflict = errors.New("version conflict")

type Database struct {
	Pool         *pgxpool.Pool
	cipher       cipher.AEAD
	KeyID        string
	Redis        *redis.Client
	Namespace    string
	SessionTTL   time.Duration
	CookieSecure bool
}

func OpenDatabase(ctx context.Context, url, encodedKey, keyID string) (*Database, error) {
	key, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("CREDENTIAL_ENCRYPTION_KEY must be base64 of 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, errors.New("invalid DATABASE_URL")
	}
	cfg.MaxConns = int32(envInt("PG_MAX_CONNS", 12, 2, 100))
	cfg.MinConns = 0
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "15000"
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = "5000"
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	// These connections serve short interactive requests. Compiling the wide
	// management aggregates costs seconds before any result is returned.
	// Offline analytics can explicitly opt in without changing the PG server.
	cfg.ConnConfig.RuntimeParams["jit"] = "off"
	if os.Getenv("PG_JIT") == "on" {
		cfg.ConnConfig.RuntimeParams["jit"] = "on"
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errors.New("initialize PostgreSQL failed")
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, errors.New("PostgreSQL unavailable")
	}
	if keyID == "" {
		keyID = "primary"
	}
	return &Database{Pool: pool, cipher: aead, KeyID: keyID, Namespace: "codex-agent:", SessionTTL: 8 * time.Hour, CookieSecure: true}, nil
}

func envInt(name string, fallback, min, max int) int {
	v, err := strconv.Atoi(os.Getenv(name))
	if err != nil || v < min || v > max {
		return fallback
	}
	return v
}

func (d *Database) Encrypt(plain []byte) ([]byte, error) {
	nonce := make([]byte, d.cipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return d.cipher.Seal(nonce, nonce, plain, []byte(d.KeyID)), nil
}

func (d *Database) Decrypt(encrypted []byte) ([]byte, error) {
	n := d.cipher.NonceSize()
	if len(encrypted) < n {
		return nil, errors.New("encrypted value is invalid")
	}
	value, err := d.cipher.Open(nil, encrypted[:n], encrypted[n:], []byte(d.KeyID))
	if err != nil {
		return nil, errors.New("encrypted value authentication failed")
	}
	return value, nil
}

func (d *Database) Migrate(ctx context.Context) error {
	conn, err := d.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err = conn.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended(current_schema() || ':schema_migrations',712498301))"); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtextextended(current_schema() || ':schema_migrations',712498301))")
	if _, err = conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(name text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	files, err := fs.Glob(migrationFiles, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, filename := range files {
		name := "migrations/" + filename
		raw, _ := migrationFiles.ReadFile(filename)
		sum := sha256.Sum256(raw)
		checksum := hex.EncodeToString(sum[:])
		var stored string
		err = conn.QueryRow(ctx, "SELECT checksum FROM schema_migrations WHERE name=$1", name).Scan(&stored)
		if err == nil {
			if stored != checksum {
				return fmt.Errorf("migration checksum mismatch: %s", name)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if filename == "006_metrics_query_indexes.sql" || filename == "013_retained_account_identity_guard.sql" || filename == "015_request_log_filter_indexes.sql" || filename == "016_request_log_status_sort.sql" || filename == "020_request_log_error_class_index.sql" || filename == "023_request_log_error_class_scalar_indexes.sql" {
			// Keep the shipped SQL/checksum stable; its CREATE INDEX statements
			// are executed concurrently on an existing large request log.
			if err := applyOnlineIndexMigration(ctx, conn, name, raw); err != nil {
				return fmt.Errorf("migration %s failed: %w", name, err)
			}
			if _, err := conn.Exec(ctx, "INSERT INTO schema_migrations(name,checksum) VALUES($1,$2)", name, checksum); err != nil {
				return err
			}
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if filename == "007_metrics_hourly_global.sql" || filename == "008_retained_log_rollup.sql" || filename == "009_retained_usage_dimensions.sql" || filename == "010_retained_usage_daily.sql" || filename == "011_retained_operations.sql" || filename == "012_account_top_day.sql" || filename == "017_retained_usage_model.sql" {
			// Backfilling an existing minute history is maintenance work. Its
			// lock timeout stays bounded; ordinary request timeouts stay intact.
			_, err = tx.Exec(ctx, "SET LOCAL statement_timeout='30min'")
		}
		if err == nil {
			_, err = tx.Exec(ctx, string(raw))
		}
		if err == nil {
			_, err = tx.Exec(ctx, "INSERT INTO schema_migrations(name,checksum) VALUES($1,$2)", name, checksum)
		}
		if err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("migration %s failed: %s", name, safeDBError(err))
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

func safeDBError(err error) error {
	if err == nil {
		return nil
	}
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) {
		switch pgerr.Code {
		case "23505":
			return errConflict
		case "23503", "23514", "22001":
			return errInvalid
		}
		return fmt.Errorf("database operation failed (SQLSTATE %s)", pgerr.Code)
	}
	return errors.New("database operation failed")
}

func (d *Database) Close() {
	if d.Redis != nil {
		d.Redis.Close()
	}
	d.Pool.Close()
}

func (d *Database) ConfigureRedis(rawURL string) error {
	opt, err := redis.ParseURL(rawURL)
	if err != nil {
		return errors.New("invalid REDIS_URL")
	}
	opt.PoolSize = 10
	opt.DialTimeout = 2 * time.Second
	opt.ReadTimeout = time.Second
	opt.WriteTimeout = time.Second
	opt.MaxRetries = 0
	d.Redis = redis.NewClient(opt)
	ns := os.Getenv("REDIS_KEY_PREFIX")
	if ns == "" {
		ns = os.Getenv("REDIS_NAMESPACE")
	}
	if ns != "" {
		d.Namespace = strings.TrimRight(ns, ":") + ":"
	}
	d.SessionTTL = time.Duration(envInt("ADMIN_SESSION_TTL", 28800, 60, 86400)) * time.Second
	d.CookieSecure = os.Getenv("ADMIN_COOKIE_SECURE") != "false"
	return nil
}

func (d *Database) audit(ctx context.Context, tx pgx.Tx, action, entity string) error {
	_, err := tx.Exec(ctx, "INSERT INTO audit_logs(action,entity_id) VALUES($1,$2)", action, entity)
	return err
}

func (d *Database) invalidate() {
	if d.Redis == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = d.Redis.Del(ctx, d.Namespace+"overview").Err()
}

// Legacy-shaped key/settings documents are encrypted in PG, never written to JSON in server mode.
func (d *Database) readDocument(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, key string, target any) error {
	var envelope struct {
		Ciphertext []byte `json:"ciphertext"`
	}
	var raw []byte
	err := q.QueryRow(ctx, "SELECT value FROM settings WHERE key=$1", key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return safeDBError(err)
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		return errors.New("invalid encrypted settings")
	}
	plain, err := d.Decrypt(envelope.Ciphertext)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(plain, target); err != nil {
		return errors.New("invalid settings document")
	}
	return nil
}

func (d *Database) writeDocument(ctx context.Context, tx pgx.Tx, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encrypted, err := d.Encrypt(raw)
	if err != nil {
		return err
	}
	envelope, _ := json.Marshal(map[string]any{"ciphertext": encrypted})
	_, err = tx.Exec(ctx, `INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value,version=settings.version+1,updated_at=now()`, key, envelope)
	return safeDBError(err)
}
