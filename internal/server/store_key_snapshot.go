package server

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

type currentKeyEntry struct {
	key     APIKey
	invalid bool
}

// The cached state is immutable. Version alone can repeat after delete/create;
// the tuple identity also changes on direct updates that omit the version bump.
type currentKeySnapshot struct {
	version   int64
	updatedAt time.Time
	xmin      string
	ctid      string
	state     keyState
	byToken   map[string]currentKeyEntry
	enabled   int
	byGroup   map[string]int
}

func (k *KeyStore) readCurrentSnapshot(ctx context.Context) (*currentKeySnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	k.mu.RLock()
	cached := k.currentSnapshot
	k.mu.RUnlock()
	var version int64
	var updatedAt time.Time
	var xmin, ctid string
	if cached != nil {
		version, updatedAt, xmin, ctid = cached.version, cached.updatedAt, cached.xmin, cached.ctid
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	next := new(currentKeySnapshot)
	var raw []byte
	// Validation and the conditional payload share one authoritative statement
	// snapshot. An unchanged row never detoasts or transfers the encrypted blob.
	err := k.store.db.Pool.QueryRow(ctx, `SELECT version,updated_at,xmin::text,ctid::text,
		CASE WHEN version=$2 AND updated_at=$3::timestamptz AND xmin::text=$4 AND ctid::text=$5 THEN NULL ELSE value END
		FROM settings WHERE key=$1`, k.store.key, version, updatedAt, xmin, ctid).Scan(&next.version, &next.updatedAt, &next.xmin, &next.ctid, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		k.mu.Lock()
		k.currentSnapshot = nil
		k.mu.Unlock()
		return &currentKeySnapshot{}, nil
	}
	if err != nil {
		return nil, safeDBError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if raw == nil && cached != nil {
		return cached, nil
	}
	var envelope struct {
		Ciphertext []byte `json:"ciphertext"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, errors.New("invalid encrypted settings")
	}
	plain, err := k.store.db.Decrypt(envelope.Ciphertext)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(plain, &next.state); err != nil {
		return nil, errors.New("invalid settings document")
	}
	next.byToken = make(map[string]currentKeyEntry, len(next.state.Keys))
	next.byGroup = make(map[string]int)
	for _, key := range next.state.Keys {
		_, duplicate := next.byToken[key.Key]
		next.byToken[key.Key] = currentKeyEntry{key: key, invalid: duplicate || key.ID == ""}
		if key.Enabled {
			next.enabled++
		}
		addManagedGroupCounts(next.byGroup, key.Groups)
	}
	k.mu.Lock()
	k.currentSnapshot = next
	k.mu.Unlock()
	return next, nil
}

// Counts belong to the immutable document revision, so overview/group reads
// need neither cloned key credentials nor a rebuilt live token index. Every
// PostgreSQL call still validates the authoritative row (including ABA), then
// copies only the requested group counts out of the cached aggregate.
func (k *KeyStore) managementCounts(ctx context.Context, groupIDs []string) (total, enabled int, groups map[string]int, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	if k.store.db != nil {
		var snapshot *currentKeySnapshot
		snapshot, err = k.readCurrentSnapshot(ctx)
		if err != nil {
			return
		}
		total, enabled = len(snapshot.state.Keys), snapshot.enabled
		if len(groupIDs) > 0 {
			groups = make(map[string]int, len(groupIDs))
			for _, id := range groupIDs {
				groups[id] = snapshot.byGroup[id]
			}
		}
		return
	}
	var state keyState
	state, err = k.store.Read()
	if err != nil {
		return
	}
	total = len(state.Keys)
	if len(groupIDs) > 0 {
		groups = make(map[string]int, len(groupIDs))
		for _, id := range groupIDs {
			groups[id] = 0
		}
	}
	for _, key := range state.Keys {
		if key.Enabled {
			enabled++
		}
		if len(groups) > 0 {
			seen := make(map[string]struct{}, len(key.Groups))
			for _, id := range key.Groups {
				if _, requested := groups[id]; !requested {
					continue
				}
				if _, duplicate := seen[id]; !duplicate {
					seen[id] = struct{}{}
					groups[id]++
				}
			}
		}
	}
	err = ctx.Err()
	return
}

func (k *KeyStore) readCurrentState(ctx context.Context) (keyState, error) {
	if k.store.db == nil {
		return k.store.Read()
	}
	snapshot, err := k.readCurrentSnapshot(ctx)
	if err != nil {
		return keyState{}, err
	}
	state := keyState{Keys: make([]APIKey, len(snapshot.state.Keys))}
	for i, key := range snapshot.state.Keys {
		state.Keys[i] = key
		state.Keys[i].Groups = slices.Clone(key.Groups)
	}
	return state, nil
}
