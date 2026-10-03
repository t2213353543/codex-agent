package server

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sync"
	"time"
)

type officialNative struct {
	mu          sync.Mutex
	rpc         *officialRPC
	last        time.Time
	credentials [32]byte
}

// The digest is never persisted or exposed. Credential replacement must refresh
// a reused official process without treating the same account as a new session.
func officialCredentialDigest(account Account) [32]byte {
	h := sha256.New()
	_, _ = h.Write([]byte(account.AccessToken))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(account.AccountID))
	var version [8]byte
	binary.BigEndian.PutUint64(version[:], uint64(account.CredentialVersion))
	_, _ = h.Write(version[:])
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}

func (rt *officialRuntime) nativeSession(ctx context.Context, key, session string, account Account, proxy *ProxyEntry) (*officialNative, error) {
	credentials := officialCredentialDigest(account)
	mapkey := key + "\x00" + gatewayAccountRef(account) + "\x00" + session
	h := sha256.Sum256([]byte(mapkey))
	gate := &rt.gates[int(h[0])%len(rt.gates)]
	gate.Lock()
	defer gate.Unlock()
	rt.mu.Lock()
	native := rt.native[mapkey]
	rt.mu.Unlock()
	if native != nil {
		if !native.mu.TryLock() {
			return nil, errors.New("App Server session is already connected")
		}
		if native.rpc.alive() {
			var refreshErr error
			if native.credentials != credentials {
				refreshCtx, stop := context.WithTimeout(ctx, 60*time.Second)
				_, refreshErr = native.rpc.call(refreshCtx, "account/login/start", map[string]any{"type": "chatgptAuthTokens", "accessToken": account.AccessToken, "chatgptAccountId": account.AccountID})
				stop()
			}
			if refreshErr == nil {
				native.credentials = credentials
				native.rpc.authMu.Lock()
				native.rpc.authAccount = account
				native.rpc.authMu.Unlock()
				native.last = time.Now()
				return native, nil
			}
		}
		native.mu.Unlock()
		native.rpc.close()
	}
	rpc, err := rt.startRPC(ctx, account, proxy)
	if err != nil {
		return nil, err
	}
	native = &officialNative{rpc: rpc, last: time.Now(), credentials: credentials}
	native.mu.Lock()
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		native.mu.Unlock()
		rpc.close()
		return nil, errors.New("official runtime closed")
	}
	rt.native[mapkey] = native
	rt.mu.Unlock()
	return native, nil
}
