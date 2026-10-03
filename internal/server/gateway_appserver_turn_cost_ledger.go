package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

var errAppServerTurnCostConflict = errors.New("turn cost provenance changed")

type appServerTurnCostEntry struct {
	Mapping appServerTurnCostMapping
	Expires time.Time
	Invalid bool
}

// Redis stores only protocol IDs/account references. No credential, prompt or
// response content is stored. Client key + client turn are hashed together.
type appServerTurnCostLedger struct {
	db      *Database
	ttl     time.Duration
	mu      sync.Mutex
	entries map[string]appServerTurnCostEntry
}

func newAppServerTurnCostLedger(db *Database) *appServerTurnCostLedger {
	return &appServerTurnCostLedger{db: db, ttl: 7 * 24 * time.Hour, entries: map[string]appServerTurnCostEntry{}}
}

func appServerTurnCostIdentity(key, clientTurn string) (string, error) {
	if key == "" || clientTurn == "" {
		return "", errors.New("turn cost key and turn id are required")
	}
	hash := sha256.Sum256([]byte(key + "\x00" + clientTurn))
	return hex.EncodeToString(hash[:]), nil
}

// Record atomically merges response IDs only if the owner, upstream thread
// and upstream turn remain identical. The caller passes actual upstream IDs;
// it must not substitute an incoming header for the final written turn ID.
func (l *appServerTurnCostLedger) Record(ctx context.Context, key, clientTurn string, value appServerTurnCostMapping) error {
	id, err := appServerTurnCostIdentity(key, clientTurn)
	if err != nil {
		return err
	}
	if value.Owner == "" || value.ThreadID == "" || value.TurnID == "" {
		return errors.New("incomplete turn cost provenance")
	}
	for _, response := range value.ResponseIDs {
		if response == "" {
			return errors.New("empty turn cost response id")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.db != nil {
		if l.db.Redis == nil {
			return errors.New("turn cost routing store unavailable")
		}
		prefix := l.db.Namespace + "gateway:turn-cost:{" + id + "}"
		args := []any{value.Owner, value.ThreadID, value.TurnID, int64(l.ttl / time.Second), value.ClientThreadID}
		for _, response := range value.ResponseIDs {
			args = append(args, response)
		}
		result, err := l.db.Redis.Eval(ctx, `
local owner=redis.call('HGET',KEYS[1],'owner')
if redis.call('HGET',KEYS[1],'invalid')=='true' then return 0 end
if owner and (owner~=ARGV[1] or redis.call('HGET',KEYS[1],'thread')~=ARGV[2] or redis.call('HGET',KEYS[1],'turn')~=ARGV[3] or (redis.call('HGET',KEYS[1],'client_thread') or '')~=ARGV[5]) then
redis.call('HSET',KEYS[1],'invalid','true');redis.call('EXPIRE',KEYS[1],ARGV[4]);redis.call('EXPIRE',KEYS[2],ARGV[4]);return 0 end
redis.call('HSET',KEYS[1],'owner',ARGV[1],'thread',ARGV[2],'turn',ARGV[3],'client_thread',ARGV[5])
for i=6,#ARGV do redis.call('SADD',KEYS[2],ARGV[i]) end
redis.call('EXPIRE',KEYS[1],ARGV[4]);redis.call('EXPIRE',KEYS[2],ARGV[4]);return 1`, []string{prefix + ":identity", prefix + ":responses"}, args...).Int()
		if err != nil {
			return err
		}
		if result != 1 {
			return errAppServerTurnCostConflict
		}
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	now := time.Now()
	prior, exists := l.entries[id]
	if exists && now.Before(prior.Expires) {
		if prior.Invalid {
			return errAppServerTurnCostConflict
		}
		if prior.Mapping.Owner != value.Owner || prior.Mapping.ThreadID != value.ThreadID || prior.Mapping.TurnID != value.TurnID || prior.Mapping.ClientThreadID != value.ClientThreadID {
			prior.Invalid, prior.Expires = true, now.Add(l.ttl)
			l.entries[id] = prior
			return errAppServerTurnCostConflict
		}
		value.ResponseIDs = append(append([]string(nil), prior.Mapping.ResponseIDs...), value.ResponseIDs...)
	}
	set := map[string]bool{}
	for _, response := range value.ResponseIDs {
		set[response] = true
	}
	value.ResponseIDs = make([]string, 0, len(set))
	for response := range set {
		value.ResponseIDs = append(value.ResponseIDs, response)
	}
	sort.Strings(value.ResponseIDs)
	l.entries[id] = appServerTurnCostEntry{Mapping: value, Expires: now.Add(l.ttl)}
	return nil
}

func (l *appServerTurnCostLedger) Lookup(ctx context.Context, key, clientTurn string) (appServerTurnCostMapping, bool, error) {
	id, err := appServerTurnCostIdentity(key, clientTurn)
	if err != nil {
		return appServerTurnCostMapping{}, false, err
	}
	if err = ctx.Err(); err != nil {
		return appServerTurnCostMapping{}, false, err
	}
	if l.db != nil {
		if l.db.Redis == nil {
			return appServerTurnCostMapping{}, false, errors.New("turn cost routing store unavailable")
		}
		prefix := l.db.Namespace + "gateway:turn-cost:{" + id + "}"
		values, err := l.db.Redis.Eval(ctx, `
local owner=redis.call('HGET',KEYS[1],'owner');if not owner or redis.call('HGET',KEYS[1],'invalid')=='true' then return {} end
local result={owner,redis.call('HGET',KEYS[1],'thread'),redis.call('HGET',KEYS[1],'turn'),redis.call('HGET',KEYS[1],'client_thread') or ''}
for _,id in ipairs(redis.call('SMEMBERS',KEYS[2])) do table.insert(result,id) end
return result`, []string{prefix + ":identity", prefix + ":responses"}).StringSlice()
		if errors.Is(err, redis.Nil) || err == nil && len(values) == 0 {
			return appServerTurnCostMapping{}, false, nil
		}
		if err != nil {
			return appServerTurnCostMapping{}, false, err
		}
		if len(values) < 4 {
			return appServerTurnCostMapping{}, false, errors.New("corrupt turn cost provenance")
		}
		value := appServerTurnCostMapping{Owner: values[0], ThreadID: values[1], TurnID: values[2], ClientThreadID: values[3], ResponseIDs: values[4:]}
		sort.Strings(value.ResponseIDs)
		return value, true, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err = ctx.Err(); err != nil {
		return appServerTurnCostMapping{}, false, err
	}
	entry, ok := l.entries[id]
	if !ok || !time.Now().Before(entry.Expires) {
		delete(l.entries, id)
		return appServerTurnCostMapping{}, false, nil
	}
	if entry.Invalid {
		return appServerTurnCostMapping{}, false, nil
	}
	value := entry.Mapping
	value.ResponseIDs = append([]string(nil), value.ResponseIDs...)
	return value, true, nil
}
