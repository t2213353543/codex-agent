package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Management state is isolated from routing settings and durable usage. PG
// documents reuse the existing authenticated encryption envelope. File mode
// exists for isolated tests/import tools and follows credential file modes.
type managementStateEntry struct {
	Version   int64           `json:"version"`
	ExpiresAt int64           `json:"expires_at,omitempty"`
	UpdatedAt int64           `json:"updated_at"`
	Payload   json.RawMessage `json:"payload"`
}

type managementStateStore struct {
	db        *Database
	directory string
}

// Windows can transiently report Access Denied while a just-removed lock
// directory is still delete-pending. Serialize in-process file mutations;
// the directory lock continues protecting independent processes.
var managementFileMu sync.Mutex

func (p *Proxy) managementState() managementStateStore {
	return managementStateStore{db: p.Store.DB, directory: filepath.Join(filepath.Dir(p.Store.Path), "management")}
}

func managementStateKey(namespace, key string) (string, error) {
	if namespace == "" || len(namespace) > 64 || key == "" || len(key) > 256 {
		return "", errInvalid
	}
	for _, ch := range namespace {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
			return "", errInvalid
		}
	}
	if strings.ContainsAny(key, "\x00\r\n") {
		return "", errInvalid
	}
	return "management/" + namespace + "/" + base64.RawURLEncoding.EncodeToString([]byte(key)), nil
}

func (s managementStateStore) file(name string) string {
	return filepath.Join(s.directory, strings.TrimPrefix(name, "management/")) + ".json"
}

func (s managementStateStore) Get(ctx context.Context, namespace, key string) (managementStateEntry, error) {
	var entry managementStateEntry
	name, err := managementStateKey(namespace, key)
	if err != nil {
		return entry, err
	}
	if err = ctx.Err(); err != nil {
		return entry, err
	}
	if s.db != nil {
		err = s.db.readDocument(ctx, s.db.Pool, name, &entry)
	} else {
		entry, err = newFileStore[managementStateEntry](s.file(name)).Read()
	}
	if err != nil {
		return entry, err
	}
	if entry.Version == 0 || entry.ExpiresAt > 0 && entry.ExpiresAt <= time.Now().Unix() {
		return managementStateEntry{}, errNotFound
	}
	return entry, nil
}

func (s managementStateStore) Update(ctx context.Context, namespace, key string, change func(*managementStateEntry) error) error {
	return s.update(ctx, namespace, key, "", change)
}
func (s managementStateStore) UpdateAudited(ctx context.Context, namespace, key, action string, change func(*managementStateEntry) error) error {
	return s.update(ctx, namespace, key, action, change)
}
func (s managementStateStore) update(ctx context.Context, namespace, key, action string, change func(*managementStateEntry) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name, err := managementStateKey(namespace, key)
	if err != nil {
		return err
	}
	apply := func(entry *managementStateEntry) error {
		return applyManagementStateChange(ctx, entry, change)
	}
	if s.db != nil {
		tx, err := s.db.Pool.Begin(ctx)
		if err != nil {
			return safeDBError(err)
		}
		defer tx.Rollback(context.Background())
		if err = s.updatePGTx(ctx, tx, namespace, key, action, change); err != nil {
			return err
		}
		return safeDBError(tx.Commit(ctx))
	}
	file := s.file(name)
	managementFileMu.Lock()
	defer managementFileMu.Unlock()
	if err = os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	unlock, err := (Store{Path: file}).lock()
	if err != nil {
		return err
	}
	defer unlock()
	return newFileStore[managementStateEntry](file).Modify(apply)
}

func applyManagementStateChange(ctx context.Context, entry *managementStateEntry, change func(*managementStateEntry) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	previous := entry.Version
	if err := change(entry); err != nil {
		return err
	}
	if len(entry.Payload) > 8<<20 || !json.Valid(entry.Payload) || entry.ExpiresAt < 0 {
		return errInvalid
	}
	entry.Version = previous + 1
	entry.UpdatedAt = time.Now().Unix()
	return ctx.Err()
}

// updatePGTx joins an existing transaction for atomic management/account writes.
// The caller owns commit/rollback; change must use this transaction, not the pool.
func (s managementStateStore) updatePGTx(ctx context.Context, tx pgx.Tx, namespace, key, action string, change func(*managementStateEntry) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name, err := managementStateKey(namespace, key)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || $1,91823017))", name); err != nil {
		return safeDBError(err)
	}
	var entry managementStateEntry
	if err = s.db.readDocument(ctx, tx, name, &entry); err != nil {
		return err
	}
	if err = applyManagementStateChange(ctx, &entry, change); err != nil {
		return err
	}
	if err = s.db.writeDocument(ctx, tx, name, entry); err != nil {
		return err
	}
	if action != "" {
		if err = s.db.audit(ctx, tx, action, key); err != nil {
			return safeDBError(err)
		}
	}
	return nil
}

// Delete is CAS when version is nonzero; zero explicitly means any version.
func (s managementStateStore) Delete(ctx context.Context, namespace, key string, version int64) error {
	name, err := managementStateKey(namespace, key)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if s.db != nil {
		tx, err := s.db.Pool.Begin(ctx)
		if err != nil {
			return safeDBError(err)
		}
		defer tx.Rollback(context.Background())
		if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || $1,91823017))", name); err != nil {
			return safeDBError(err)
		}
		var entry managementStateEntry
		if err = s.db.readDocument(ctx, tx, name, &entry); err != nil {
			return err
		}
		if entry.Version == 0 {
			return errNotFound
		}
		if version != 0 && entry.Version != version {
			return errVersionConflict
		}
		if _, err = tx.Exec(ctx, "DELETE FROM settings WHERE key=$1", name); err != nil {
			return safeDBError(err)
		}
		return safeDBError(tx.Commit(ctx))
	}
	file := s.file(name)
	managementFileMu.Lock()
	defer managementFileMu.Unlock()
	if _, err = os.Stat(filepath.Dir(file)); errors.Is(err, os.ErrNotExist) {
		return errNotFound
	} else if err != nil {
		return err
	}
	unlock, err := (Store{Path: file}).lock()
	if err != nil {
		return err
	}
	defer unlock()
	entry, err := newFileStore[managementStateEntry](file).Read()
	if err != nil {
		return err
	}
	if entry.Version == 0 {
		return errNotFound
	}
	if version != 0 && entry.Version != version {
		return errVersionConflict
	}
	return os.Remove(file)
}

func (s managementStateStore) List(ctx context.Context, namespace string) (map[string]managementStateEntry, error) {
	return s.list(ctx, namespace, false)
}
func (s managementStateStore) list(ctx context.Context, namespace string, includeExpired bool) (map[string]managementStateEntry, error) {
	name, err := managementStateKey(namespace, "check")
	if err != nil {
		return nil, err
	}
	prefix := name[:strings.LastIndex(name, "/")+1]
	result := map[string]managementStateEntry{}
	accept := func(encoded string, entry managementStateEntry) error {
		key, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			return fmt.Errorf("invalid management state key")
		}
		if entry.Version > 0 && (includeExpired || entry.ExpiresAt == 0 || entry.ExpiresAt > time.Now().Unix()) {
			result[string(key)] = entry
		}
		return nil
	}
	if s.db != nil {
		// Read keys and encrypted envelopes in one query/snapshot, avoiding a query
		// per job. No management payload is exposed through the settings API.
		rows, err := s.db.Pool.Query(ctx, "SELECT key,value FROM settings WHERE starts_with(key,$1) ORDER BY key", prefix)
		if err != nil {
			return nil, safeDBError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			var raw []byte
			if err = rows.Scan(&key, &raw); err != nil {
				return nil, err
			}
			var envelope struct {
				Ciphertext []byte `json:"ciphertext"`
			}
			if err = json.Unmarshal(raw, &envelope); err != nil {
				return nil, err
			}
			plain, err := s.db.Decrypt(envelope.Ciphertext)
			if err != nil {
				return nil, err
			}
			var entry managementStateEntry
			if err = json.Unmarshal(plain, &entry); err != nil {
				return nil, err
			}
			if err = accept(strings.TrimPrefix(key, prefix), entry); err != nil {
				return nil, err
			}
		}
		return result, rows.Err()
	}
	folder := filepath.Join(s.directory, namespace)
	files, err := os.ReadDir(folder)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		if f.Type()&os.ModeSymlink != 0 {
			return nil, errInvalid
		}
		entry, err := newFileStore[managementStateEntry](filepath.Join(folder, f.Name())).Read()
		if err != nil {
			return nil, err
		}
		if err = accept(strings.TrimSuffix(f.Name(), ".json"), entry); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// Expired OAuth PKCE material and pending tokens are physically removed, not
// just hidden by Get. CAS preserves a concurrently extended/replaced entry.
func (s managementStateStore) PruneExpired(ctx context.Context, namespace string) error {
	entries, err := s.list(ctx, namespace, true)
	if err != nil {
		return err
	}
	for key, entry := range entries {
		if entry.ExpiresAt > 0 && entry.ExpiresAt <= time.Now().Unix() {
			if err = s.Delete(ctx, namespace, key, entry.Version); err != nil && !errors.Is(err, errVersionConflict) && !errors.Is(err, errNotFound) {
				return err
			}
		}
	}
	return nil
}
