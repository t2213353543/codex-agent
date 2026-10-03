package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

const bpsHistoryPrefix = "bps:history:v2:"
const bpsOpaqueOwnerPrefix = "bps:opaque-owner:v2:"
const bpsOpaqueOwnerEntries = 65536

var errBPSHistoryStorage = errors.New("conversation storage unavailable")
var ErrBPSHistoryAlreadyCached = errors.New("conversation is already present in the bounded cache")

// Old permanent bodies are readable only during an explicitly scheduled,
// absolute migration window. No request extends this deadline. Reject distant
// dates so a configuration mistake cannot silently restore permanent retention.
func bpsHistoryLegacyReadDeadline() int64 {
	deadline, err := strconv.ParseInt(os.Getenv("CODEX_HISTORY_LEGACY_READ_UNTIL_UNIX"), 10, 64)
	now := time.Now().Unix()
	if err != nil || deadline <= now || deadline > now+int64(bpsHistoryTTL/time.Second) {
		return 0
	}
	return deadline
}

func (d *Database) bpsHistoryCipher(ctx context.Context, key string) ([]byte, int64, error) {
	if d.Redis == nil {
		return nil, 0, errBPSHistoryStorage
	}
	value, err := d.Redis.Get(ctx, d.Namespace+bpsHistoryPrefix+key).Bytes()
	if err == nil {
		return value, 0, nil
	}
	if !errors.Is(err, redis.Nil) {
		return nil, 0, errBPSHistoryStorage
	}
	deadline := bpsHistoryLegacyReadDeadline()
	if deadline == 0 {
		return nil, 0, errBPSHistoryMissing
	}
	value, err = d.Redis.Get(ctx, d.Namespace+"bps:history:"+key).Bytes()
	if err == nil {
		return value, deadline, nil
	}
	if !errors.Is(err, redis.Nil) {
		return nil, 0, errBPSHistoryStorage
	}
	if d.Pool == nil {
		return nil, 0, errBPSHistoryMissing
	}
	err = d.Pool.QueryRow(ctx, `SELECT ciphertext FROM gateway_response_history WHERE namespace=$1 AND history_key=$2`, d.Namespace, key).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, errBPSHistoryMissing
	}
	if err != nil {
		return nil, 0, errBPSHistoryStorage
	}
	return value, deadline, nil
}

// All modes share one byte/entry budget. Check every type and owner before any
// mutation, then publish the history and its proofs in the same Lua operation.
// Accounting normally costs O(1); once per minute it reconciles the bounded
// size hash to recover from expiry, migrations or older accounting versions.
const bpsSaveBoundedHistory = `
local function expected_type(key,want)
 local actual=redis.call('TYPE',key).ok
 return actual=='none' or actual==want
end
local store_body=ARGV[11]=='1'
if not expected_type(KEYS[5],'zset') then return redis.error_reply('invalid opaque owner storage type') end
if store_body and (not expected_type(KEYS[1],'zset') or not expected_type(KEYS[2],'hash') or not expected_type(KEYS[3],'string') or not expected_type(KEYS[4],'hash')) then
 return redis.error_reply('invalid history storage type')
end
if ARGV[9]=='1' and redis.call('EXISTS',KEYS[3])==1 then return 2 end
for i=6,#KEYS do
 if not expected_type(KEYS[i],'string') then return redis.error_reply('invalid opaque owner storage type') end
 local owner=redis.call('GET',KEYS[i])
 if owner and owner~=ARGV[8] then return 0 end
end
if store_body then
if tonumber(ARGV[5])>tonumber(ARGV[6]) then return -1 end
local count=redis.call('HLEN',KEYS[2])
local total=tonumber(redis.call('HGET',KEYS[4],'bytes'))
local audited=tonumber(redis.call('HGET',KEYS[4],'audited') or '0')
local saved_count=tonumber(redis.call('HGET',KEYS[4],'count') or '-1')
if not total or total<0 or count~=saved_count or tonumber(ARGV[1])-audited>=60 then
 total=0
 for _,n in ipairs(redis.call('HVALS',KEYS[2])) do total=total+tonumber(n) end
 audited=tonumber(ARGV[1])
end
local function remove(k)
 total=total-tonumber(redis.call('HGET',KEYS[2],k) or '0')
 redis.call('UNLINK',k);redis.call('HDEL',KEYS[2],k);redis.call('ZREM',KEYS[1],k)
end
local expired=redis.call('ZRANGEBYSCORE',KEYS[1],'-inf',ARGV[1],'LIMIT',0,512)
for _,k in ipairs(expired) do remove(k) end
total=total-tonumber(redis.call('HGET',KEYS[2],KEYS[3]) or '0')+tonumber(ARGV[5])
redis.call('SET',KEYS[3],ARGV[3],'EX',ARGV[4])
redis.call('ZADD',KEYS[1],ARGV[2],KEYS[3]);redis.call('HSET',KEYS[2],KEYS[3],ARGV[5])
while total>tonumber(ARGV[6]) or redis.call('ZCARD',KEYS[1])>tonumber(ARGV[7]) do
 local candidates=redis.call('ZRANGE',KEYS[1],0,1)
 local oldest=candidates[1]
 if oldest==KEYS[3] then oldest=candidates[2] end
 if not oldest then break end
 remove(oldest)
end
redis.call('EXPIRE',KEYS[1],3600);redis.call('EXPIRE',KEYS[2],3600)
redis.call('HSET',KEYS[4],'bytes',math.max(0,total),'count',redis.call('HLEN',KEYS[2]),'audited',audited)
redis.call('EXPIRE',KEYS[4],3600)
end
local oldowners=redis.call('ZRANGEBYSCORE',KEYS[5],'-inf',ARGV[1],'LIMIT',0,512)
for _,k in ipairs(oldowners) do redis.call('UNLINK',k);redis.call('ZREM',KEYS[5],k) end
for i=6,#KEYS do
 local expires=math.max(tonumber(redis.call('ZSCORE',KEYS[5],KEYS[i]) or '0'),tonumber(ARGV[2]))
 redis.call('SET',KEYS[i],ARGV[8],'EX',expires-tonumber(ARGV[1]));redis.call('ZADD',KEYS[5],expires,KEYS[i])
end
local ownercount=redis.call('ZCARD',KEYS[5])
if ownercount>tonumber(ARGV[10]) then
 local oldest=redis.call('ZRANGE',KEYS[5],0,ownercount-tonumber(ARGV[10])-1)
 for _,k in ipairs(oldest) do redis.call('UNLINK',k);redis.call('ZREM',KEYS[5],k) end
end
redis.call('EXPIRE',KEYS[5],3600)
return 1`

func (d *Database) bpsStoreBoundedHistory(ctx context.Context, key string, ciphertext []byte, expires int64, owner string, proofs []string, onlyMissing bool) error {
	if d.Redis == nil {
		return errBPSHistoryStorage
	}
	now := time.Now().Unix()
	ttl := expires - now
	if ttl <= 0 || ttl > int64(bpsHistoryTTL/time.Second) {
		return errors.New("conversation exceeds the bounded continuation cache; send full history")
	}
	storeBody := "1"
	if len(ciphertext) == 0 || len(ciphertext) > bpsHistoryBudget {
		storeBody = "0"
		ciphertext = nil
	}
	prefix := d.Namespace + bpsHistoryPrefix
	keys := []string{prefix + "index", prefix + "sizes", prefix + key, prefix + "accounting", d.Namespace + bpsOpaqueOwnerPrefix + "index"}
	for _, proof := range proofs {
		keys = append(keys, d.Namespace+bpsOpaqueOwnerPrefix+proof)
	}
	nx := "0"
	if onlyMissing {
		nx = "1"
	}
	stored, err := d.Redis.Eval(ctx, bpsSaveBoundedHistory, keys, now, expires, ciphertext, ttl,
		len(ciphertext), bpsHistoryBudget, bpsHistoryEntries, owner, nx, bpsOpaqueOwnerEntries, storeBody).Int()
	if err != nil {
		return errBPSHistoryStorage
	}
	if stored == 0 && len(proofs) > 0 {
		return errBPSOpaqueOwnerConflict
	}
	if stored == 2 {
		return ErrBPSHistoryAlreadyCached
	}
	if stored != 1 {
		return errBPSHistoryStorage
	}
	return nil
}

// WarmBPSHistory imports only a caller-selected recent history into the same
// bounded cache used by live traffic. expiresAt is the original response time
// plus one hour, not migration time plus one hour. It never writes PostgreSQL.
func WarmBPSHistory(ctx context.Context, db *Database, keyID, responseID string, ciphertext []byte, expiresAt time.Time) error {
	if db == nil || db.Redis == nil || keyID == "" || responseID == "" {
		return errBPSHistoryStorage
	}
	raw, err := db.Decrypt(ciphertext)
	if err != nil {
		return errors.New("conversation storage authentication failed")
	}
	var record bpsHistoryRecord
	if json.Unmarshal(raw, &record) != nil || record.Owner == "" {
		return errBPSHistoryMissing
	}
	mode := record.Mode
	if mode == "" {
		mode = inferencePowerPoint
	}
	if mode != inferencePowerPoint && mode != inferenceAppServer && mode != inferenceCodexCLI {
		return errBPSHistoryMissing
	}
	proofs := bpsOpaqueItemOwnerKeys(keyID, mode, record.Output)
	var verified []string
	if len(proofs) > 0 {
		keys := make([]string, len(proofs))
		for i, proof := range proofs {
			keys[i] = db.Namespace + "bps:opaque-owner:" + proof
		}
		values, err := db.Redis.Eval(ctx, bpsLookupOpaqueOwners, keys).Slice()
		if err != nil || len(values) != len(proofs) {
			return errBPSHistoryStorage
		}
		for i, value := range values {
			if owner, ok := value.(string); ok && owner == record.Owner {
				verified = append(verified, proofs[i])
			}
		}
	}
	record.Expires = expiresAt.Unix()
	raw, err = json.Marshal(record)
	if err != nil {
		return errBPSHistoryStorage
	}
	if len(raw)+28 > bpsHistoryBudget {
		return errors.New("conversation exceeds the bounded continuation cache; send full history")
	}
	sealed, err := db.Encrypt(raw)
	if err != nil {
		return errBPSHistoryStorage
	}
	return db.bpsStoreBoundedHistory(ctx, bpsHistoryKey(keyID, responseID), sealed, record.Expires, record.Owner, verified, true)
}
