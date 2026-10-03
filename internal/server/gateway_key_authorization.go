package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"time"
)

var errGatewayKeyStoreUnavailable = errors.New("gateway key authorization storage unavailable")

// Gateway authorization must not use the management index, which another
// instance cannot invalidate. A returned entry is a detached current snapshot.
func (k *KeyStore) LookupCurrent(ctx context.Context, token string) (APIKey, bool, error) {
	if err := ctx.Err(); err != nil {
		return APIKey{}, false, err
	}
	if k == nil || k.store == nil || token == "" {
		return APIKey{}, false, nil
	}
	var state keyState
	var err error
	if k.store.db != nil {
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		snapshot, readErr := k.readCurrentSnapshot(readCtx)
		if ctx.Err() != nil {
			return APIKey{}, false, ctx.Err()
		}
		if readErr != nil {
			return APIKey{}, false, errGatewayKeyStoreUnavailable
		}
		entry, ok := snapshot.byToken[token]
		if entry.invalid {
			return APIKey{}, false, errGatewayKeyStoreUnavailable
		}
		if !ok || !entry.key.Enabled {
			return APIKey{}, false, nil
		}
		result := entry.key
		result.Groups = append([]string(nil), result.Groups...)
		return result, true, nil
	} else {
		state, err = k.store.Read()
	}
	if ctx.Err() != nil {
		return APIKey{}, false, ctx.Err()
	}
	if err != nil {
		return APIKey{}, false, errGatewayKeyStoreUnavailable
	}
	var found *APIKey
	for i := range state.Keys {
		entry := &state.Keys[i]
		if entry.Key != token {
			continue
		}
		if found != nil || entry.ID == "" {
			return APIKey{}, false, errGatewayKeyStoreUnavailable
		}
		found = entry
	}
	if found == nil || !found.Enabled {
		return APIKey{}, false, nil
	}
	result := *found
	result.Groups = append([]string(nil), found.Groups...)
	return result, true, nil
}

// Resolve identity and grants from one snapshot at each authorization boundary.
// Do not carry it across OAuth refresh or later turns of a persistent socket.
func (p *Proxy) resolveGatewayKeyContext(ctx context.Context, token string) (string, string, *APIKey, error) {
	if err := ctx.Err(); err != nil {
		return "", "", nil, err
	}
	if token == "" {
		return "", "", nil, nil
	}
	if settings, err := p.settingsValue(); err == nil && settings.AdminAPI {
		keyCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		admin, err := p.currentAdminKey(keyCtx)
		cancel()
		if err == nil && admin != "" && subtle.ConstantTimeCompare([]byte(token), []byte(admin)) == 1 {
			return "admin", "admin key", nil, nil
		}
	}
	if p.Keys != nil {
		entry, found, err := p.Keys.LookupCurrent(ctx, token)
		if err != nil {
			return "", "", nil, err
		}
		if found {
			return entry.ID, entry.Name, &entry, nil
		}
	}
	return "", "", nil, nil
}
