package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var migrationIndexPattern = regexp.MustCompile(`(?s)^CREATE INDEX ([a-z_][a-z0-9_]*) ON ([a-z_][a-z0-9_]*)\b`)

type migrationIndex struct{ name, table, statement, checksum string }

// This deliberately accepts only the known migration's simple index DDL, not
// arbitrary SQL scripts, PL/pgSQL bodies or caller-supplied identifiers.
func parseMigrationIndexes(raw []byte) ([]migrationIndex, error) {
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines[i] = ""
		}
	}
	var result []migrationIndex
	for _, statement := range strings.Split(strings.Join(lines, "\n"), ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		match := migrationIndexPattern.FindStringSubmatch(statement)
		if match == nil {
			return nil, errors.New("online migration must contain only named CREATE INDEX statements")
		}
		sum := sha256.Sum256([]byte(statement))
		result = append(result, migrationIndex{match[1], match[2], strings.Replace(statement, "CREATE INDEX ", "CREATE INDEX CONCURRENTLY ", 1), hex.EncodeToString(sum[:])})
	}
	if len(result) == 0 {
		return nil, errors.New("online migration has no indexes")
	}
	return result, nil
}

func applyOnlineIndexMigration(ctx context.Context, conn *pgxpool.Conn, name string, raw []byte) error {
	indexes, err := parseMigrationIndexes(raw)
	if err != nil {
		return err
	}
	var timeout, lockTimeout, schema string
	if err := conn.QueryRow(ctx, "SELECT current_setting('statement_timeout'),current_setting('lock_timeout'),current_schema()").Scan(&timeout, &lockTimeout, &schema); err != nil {
		return safeDBError(err)
	}
	// Concurrent expression-index validation waits for snapshots even from
	// other schemas. A legitimate bulk transaction can exceed the request
	// lock budget; give only maintenance a bounded two-minute wait, then restore
	// both settings before this connection returns to normal requests.
	if _, err := conn.Exec(ctx, "SET statement_timeout='30min'; SET lock_timeout='2min'"); err != nil {
		return safeDBError(err)
	}
	defer func() {
		restore, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(restore, "SELECT set_config('statement_timeout',$1,false),set_config('lock_timeout',$2,false)", timeout, lockTimeout); err != nil {
			// A connection with a maintenance timeout must never reenter the
			// application's pool. Closing it also releases its migration lock.
			_ = conn.Conn().Close(restore)
		}
	}()
	// Durable claims distinguish our interrupted index builds from preexisting
	// objects. Never silently adopt or delete an unrelated same-named index.
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migration_indexes (
	 migration text NOT NULL,index_name text NOT NULL,statement_checksum text NOT NULL,
	 applied_definition text,
	 PRIMARY KEY(migration,index_name))`); err != nil {
		return safeDBError(err)
	}
	if _, err := conn.Exec(ctx, "ALTER TABLE schema_migration_indexes ADD COLUMN IF NOT EXISTS applied_definition text"); err != nil {
		return safeDBError(err)
	}
	for _, index := range indexes {
		var checksum string
		var appliedDefinition *string
		err := conn.QueryRow(ctx, "SELECT statement_checksum,applied_definition FROM schema_migration_indexes WHERE migration=$1 AND index_name=$2", name, index.name).Scan(&checksum, &appliedDefinition)
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2)`, schema, index.name).Scan(&exists); err != nil {
				return safeDBError(err)
			}
			if exists {
				return fmt.Errorf("index %s already exists without a migration claim", index.name)
			}
			if _, err := conn.Exec(ctx, "INSERT INTO schema_migration_indexes(migration,index_name,statement_checksum) VALUES($1,$2,$3)", name, index.name, index.checksum); err != nil {
				return safeDBError(err)
			}
		} else if err != nil {
			return safeDBError(err)
		} else if checksum != index.checksum {
			return fmt.Errorf("index %s migration checksum mismatch", index.name)
		}
		var kind, table, definition string
		var valid, ready bool
		err = conn.QueryRow(ctx, `SELECT c.relkind::text,COALESCE(t.relname,''),COALESCE(i.indisvalid,false),COALESCE(i.indisready,false),COALESCE(pg_get_indexdef(i.indexrelid),'')
		 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		 LEFT JOIN pg_index i ON i.indexrelid=c.oid LEFT JOIN pg_class t ON t.oid=i.indrelid
		 WHERE n.nspname=$1 AND c.relname=$2`, schema, index.name).Scan(&kind, &table, &valid, &ready, &definition)
		if err == nil {
			if kind != "i" || table != index.table {
				return fmt.Errorf("index %s has an unexpected relation", index.name)
			}
			if valid && ready && appliedDefinition != nil {
				if definition != *appliedDefinition {
					return fmt.Errorf("index %s definition changed after migration build", index.name)
				}
				continue
			}
			// A crash between CREATE and recording its canonical definition is
			// recoverable by rebuilding the claimed index. Do not silently adopt
			// a different valid definition installed during that interruption.
			if _, err := conn.Exec(ctx, "DROP INDEX CONCURRENTLY "+pgx.Identifier{schema, index.name}.Sanitize()); err != nil {
				return safeDBError(err)
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return safeDBError(err)
		}
		created, err := createEmptyTableIndex(ctx, conn, schema, index)
		if err != nil {
			return safeDBError(err)
		}
		if !created {
			if _, err := conn.Exec(ctx, index.statement); err != nil {
				return safeDBError(err)
			}
		}
		if err := conn.QueryRow(ctx, `SELECT i.indisvalid AND i.indisready,pg_get_indexdef(i.indexrelid) FROM pg_index i
		 JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_namespace n ON n.oid=c.relnamespace
		 WHERE n.nspname=$1 AND c.relname=$2`, schema, index.name).Scan(&valid, &definition); err != nil {
			return safeDBError(err)
		}
		if !valid {
			return fmt.Errorf("index %s is not ready", index.name)
		}
		if _, err := conn.Exec(ctx, "UPDATE schema_migration_indexes SET applied_definition=$3 WHERE migration=$1 AND index_name=$2", name, index.name, definition); err != nil {
			return safeDBError(err)
		}
	}
	return nil
}

// An empty new table needs no online backfill. A nonblocking SHARE lock makes
// the emptiness check and ordinary CREATE atomic against writers. This also
// avoids CONCURRENTLY waiting on old snapshots belonging to unrelated tables
// when provisioning an isolated schema in a busy database.
func createEmptyTableIndex(ctx context.Context, conn *pgxpool.Conn, schema string, index migrationIndex) (bool, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, "LOCK TABLE "+pgx.Identifier{schema, index.table}.Sanitize()+" IN SHARE MODE NOWAIT"); err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "55P03" {
			return false, nil
		}
		return false, err
	}
	var nonempty bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+pgx.Identifier{schema, index.table}.Sanitize()+" LIMIT 1)").Scan(&nonempty); err != nil {
		return false, err
	}
	if nonempty {
		return false, nil
	}
	if _, err := tx.Exec(ctx, strings.Replace(index.statement, "CREATE INDEX CONCURRENTLY ", "CREATE INDEX ", 1)); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
