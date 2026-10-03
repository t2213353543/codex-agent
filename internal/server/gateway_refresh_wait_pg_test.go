package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func gatewayRefreshWaitGateKey(store Store, account Account) string {
	cfg := store.DB.Pool.Config().ConnConfig
	return fmt.Sprintf("%s:%d/%s/%s/%s", cfg.Host, cfg.Port, cfg.Database, cfg.RuntimeParams["search_path"], account.ID)
}

func gatewayRefreshWaitForGate(t *testing.T, key string, wantRefs int) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		accountRefreshGates.Lock()
		entry := accountRefreshGates.entries[key]
		ready := entry == nil && wantRefs == 0
		if entry != nil {
			ready = entry.refs == wantRefs && len(entry.wait) == 1
		}
		accountRefreshGates.Unlock()
		if ready {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("refresh gate did not reach %d references", wantRefs)
		case <-tick.C:
		}
	}
}

type gatewayRefreshWaitAudit struct {
	total, updates, failed int64
}

func gatewayRefreshWaitReadAudit(t *testing.T, store Store, id string) gatewayRefreshWaitAudit {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var count gatewayRefreshWaitAudit
	err := store.DB.Pool.QueryRow(ctx, `SELECT count(*),
 count(*) FILTER (WHERE action='account.credentials.update' AND result='ok'),
 count(*) FILTER (WHERE action='account.credentials.refresh' AND result='failed')
 FROM audit_logs WHERE entity_id=$1`, id).Scan(&count.total, &count.updates, &count.failed)
	if err != nil {
		t.Fatal("cannot inspect synthetic account audit counts")
	}
	return count
}

func TestPGGatewayRefreshWaitCancellation(t *testing.T) {
	for _, consumer := range []string{"ensure_ready", "recover_401"} {
		t.Run(consumer, func(t *testing.T) {
			store := accountPGTestStore(t)
			redisDB, _ := gatewayRedisSessionFixture(t)
			store.DB.Redis, store.DB.Namespace = redisDB.Redis, redisDB.Namespace
			raw, _ := json.Marshal(map[string]any{
				"access_token": "synthetic-wait-access", "refresh_token": "synthetic-wait-refresh",
				"chatgpt_account_id": "synthetic-wait-workspace", "chatgpt_user_id": "synthetic-wait-user",
				"expires_at": time.Now().Add(-time.Hour).Unix(),
			})
			if err := store.ImportWith("refresh-wait", raw, AccountDefaults{}); err != nil {
				t.Fatal("cannot import complete synthetic expired account")
			}
			before, err := store.Get("refresh-wait")
			if err != nil || !gatewayCompleteOwner(before) {
				t.Fatal("synthetic account must have a complete owner")
			}
			auditBefore := gatewayRefreshWaitReadAudit(t, store, before.ID)
			var oauthCalls atomic.Int32
			p := &Proxy{Store: store, TokenURL: "http://refresh-wait.invalid/oauth/token"}
			p.Client = &http.Client{Transport: sourceRefreshRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.Body != nil {
					_ = r.Body.Close()
				}
				oauthCalls.Add(1)
				return nil, errors.New("unexpected synthetic OAuth exchange")
			})}
			defer p.closeGatewayClients()
			failureKey := gatewayRefreshFailureKey(before, p.TokenURL)
			redisKey := store.DB.Namespace + "gateway:oauth-failure:" + failureKey
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := store.DB.Redis.Del(ctx, redisKey).Err(); err != nil {
					t.Error("cannot remove synthetic OAuth cooldown key")
				}
			})
			holderEntered, releaseHolder := make(chan struct{}), make(chan struct{})
			holderDone, waiterDone := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseHolder) }) }
			ctx, cancel := context.WithCancel(t.Context())
			var holderErr, waiterErr error
			var holderCalls atomic.Int32
			waiterStarted := false
			defer func() {
				cancel()
				release()
				for _, done := range []chan struct{}{holderDone, waiterDone} {
					if done == waiterDone && !waiterStarted {
						continue
					}
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("synthetic refresh goroutine did not finish during cleanup")
					}
				}
			}()
			go func() {
				defer close(holderDone)
				_, holderErr = store.Refresh(before.ID, before.AccessToken, func(current Account) (Account, error) {
					holderCalls.Add(1)
					close(holderEntered)
					<-releaseHolder
					current.AccessToken, current.RefreshToken = "synthetic-wait-rotated-access", "synthetic-wait-rotated-refresh"
					current.ExpiresAt = time.Now().Add(time.Hour).Unix()
					return current, nil
				})
			}()
			select {
			case <-holderEntered:
			case <-holderDone:
				t.Fatal("first refresh did not enter its callback")
			case <-time.After(5 * time.Second):
				t.Fatal("first refresh did not acquire its account lock")
			}
			waiterStarted = true
			go func() {
				defer close(waiterDone)
				if consumer == "ensure_ready" {
					_, waiterErr = p.ensureReady(ctx, before)
				} else {
					_, waiterErr = p.recoverUnauthorized(ctx, before, p.Client)
				}
			}()
			gateKey := gatewayRefreshWaitGateKey(store, before)
			// The entry proves the second caller reached the occupied local gate;
			// cancellation never depends on an assumed scheduler delay.
			gatewayRefreshWaitForGate(t, gateKey, 2)
			cancel()
			select {
			case <-waiterDone:
				if !errors.Is(waiterErr, context.Canceled) {
					t.Error("cancelled refresh waiter did not preserve context.Canceled")
				}
				gatewayRefreshWaitForGate(t, gateKey, 1)
			case <-time.After(2 * time.Second):
				t.Error("cancelled refresh waiter remained blocked behind the active holder")
			}
			current, err := store.getAccountByID(before.ID)
			if err != nil || current.CredentialStatus != before.CredentialStatus || current.LastError != before.LastError ||
				current.LastCheckedAt != before.LastCheckedAt || current.Version != before.Version ||
				current.CredentialVersion != before.CredentialVersion || current.AccessToken != before.AccessToken || current.RefreshToken != before.RefreshToken {
				t.Error("cancelling a queued refresh changed account state before holder completion")
			}
			if got := gatewayRefreshWaitReadAudit(t, store, before.ID); got != auditBefore {
				t.Error("cancelling a queued refresh created an account audit entry")
			}
			release()
			for _, done := range []chan struct{}{holderDone, waiterDone} {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("refresh did not finish after releasing the holder")
				}
			}
			if holderErr != nil || holderCalls.Load() != 1 {
				t.Error("cancelling a waiter interrupted or repeated the active refresh")
			}
			if !errors.Is(waiterErr, context.Canceled) {
				t.Error("cancelled waiter returned success or another error after holder rotation")
			}
			current, err = store.getAccountByID(before.ID)
			if err != nil || current.AccessToken != "synthetic-wait-rotated-access" || current.RefreshToken != "synthetic-wait-rotated-refresh" ||
				current.CredentialVersion != before.CredentialVersion+1 || gatewayAccountOwnerRef(current) != gatewayAccountOwnerRef(before) ||
				current.CredentialStatus == "refresh_failed" || current.LastError != "" {
				t.Error("holder rotation was not persisted exactly once with its original owner")
			}
			auditAfter := gatewayRefreshWaitReadAudit(t, store, before.ID)
			if auditAfter.total != auditBefore.total+1 || auditAfter.updates != auditBefore.updates+1 || auditAfter.failed != auditBefore.failed {
				t.Error("refresh audit must contain only the holder's one successful credential update")
			}
			if oauthCalls.Load() != 0 {
				t.Error("cancelled waiter attempted an OAuth exchange")
			}
			if _, exists := p.refreshCooldown.get(failureKey, time.Now()); exists {
				t.Error("queued cancellation created a local OAuth cooldown")
			}
			if exists, err := store.DB.Redis.Exists(t.Context(), redisKey).Result(); err != nil || exists != 0 {
				t.Error("queued cancellation created a Redis OAuth cooldown")
			}
			gatewayRefreshWaitForGate(t, gateKey, 0)
		})
	}
}
