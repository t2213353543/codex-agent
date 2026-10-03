package server

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// GetMany retrieves just the requested documents in one PostgreSQL snapshot.
// Missing or expired documents have the same meaning as Get's errNotFound.
func (s managementStateStore) GetMany(ctx context.Context, namespace string, keys []string) (map[string]managementStateEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := managementStateKey(namespace, "check"); err != nil {
		return nil, err
	}
	names := make(map[string]string, len(keys))
	for _, key := range keys {
		name, err := managementStateKey(namespace, key)
		if err != nil {
			return nil, err
		}
		names[name] = key
	}
	result := make(map[string]managementStateEntry, len(names))
	if len(names) == 0 {
		return result, nil
	}
	if s.db == nil {
		for _, key := range names {
			entry, err := s.Get(ctx, namespace, key)
			if errors.Is(err, errNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			result[key] = entry
		}
		return result, nil
	}
	wanted := make([]string, 0, len(names))
	for name := range names {
		wanted = append(wanted, name)
	}
	rows, err := s.db.Pool.Query(ctx, "SELECT key,value FROM settings WHERE key=ANY($1::text[])", wanted)
	if err != nil {
		return nil, safeDBError(err)
	}
	defer rows.Close()
	now := time.Now().Unix()
	for rows.Next() {
		var name string
		var raw []byte
		if err := rows.Scan(&name, &raw); err != nil {
			return nil, err
		}
		var envelope struct {
			Ciphertext []byte `json:"ciphertext"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return nil, err
		}
		plain, err := s.db.Decrypt(envelope.Ciphertext)
		if err != nil {
			return nil, err
		}
		var entry managementStateEntry
		if err := json.Unmarshal(plain, &entry); err != nil {
			return nil, err
		}
		if entry.Version > 0 && (entry.ExpiresAt == 0 || entry.ExpiresAt > now) {
			result[names[name]] = entry
		}
	}
	return result, rows.Err()
}
