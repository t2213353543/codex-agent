package server

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGatewaySessionRejectedUseDoesNotExtendTTL(t *testing.T) {
	for _, kind := range []string{"missing_owner", "disabled", "busy", "cooldown", "admitted"} {
		t.Run(kind, func(t *testing.T) {
			p := newTestProxy(t, 1)
			account, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			owner := gatewayAccountRef(account)
			if kind == "missing_owner" {
				owner = "id:deleted-owner"
			}
			key := auditSessionMapKey("admin", "session")
			before := gatewaySessionRoute{Account: owner, Expires: time.Now().Add(20 * time.Second)}
			p.sessionRoutes = map[string]gatewaySessionRoute{key: before}
			if kind == "disabled" {
				if err := p.Store.UpdateAccount(account.ID, func(a *Account) error { a.Enabled = false; return nil }); err != nil {
					t.Fatal(err)
				}
			} else if kind == "busy" {
				if !p.acquire(gatewayAdmissionKey(account), 1) {
					t.Fatal("fixture busy slot missing")
				}
				defer p.release(gatewayAdmissionKey(account))
			} else if kind == "cooldown" {
				enableRateLimitCooldownForTest(t, p)
				if err := p.gatewayCooldown(t.Context(), account, gatewayCooldownScope(t.Context()), time.Minute); !errors.As(err, new(*gatewayCooldownError)) {
					t.Fatal(err)
				}
			}
			// This mirrors BPS history/owner verification before selection.
			if got, err := p.sessionAccount(t.Context(), "admin", "session", owner); err != nil || got != owner {
				t.Fatal(err)
			}
			_, release, err := p.pickSessionAccount(t.Context(), p.AdminKey, "admin", "session")
			if release != nil {
				release()
			}
			if kind == "admitted" {
				if err != nil || !p.sessionRoutes[key].Expires.After(before.Expires) {
					t.Fatal("admitted request did not renew exact owner", err)
				}
			} else if err == nil || p.sessionRoutes[key] != before {
				t.Fatalf("failed request renewed binding: err=%v before=%+v after=%+v", err, before, p.sessionRoutes[key])
			}
		})
	}
}

func TestGatewaySessionTouchPreservesWinnerAndExpiry(t *testing.T) {
	p := newTestProxy(t, 1)
	key := auditSessionMapKey("key", "session")
	before := gatewaySessionRoute{Account: "winner", Expires: time.Now().Add(time.Minute)}
	p.sessionRoutes = map[string]gatewaySessionRoute{key: before}
	for _, proposal := range []string{"", "winner", "old-owner"} {
		if got, err := p.sessionAccount(t.Context(), "key", "session", proposal); err != nil || got != "winner" || p.sessionRoutes[key] != before {
			t.Fatal("lookup/claim extended or replaced existing owner")
		}
	}
	if got, err := p.touchGatewaySession(t.Context(), "key", "session", "old-owner"); err != nil || got != "winner" || p.sessionRoutes[key] != before {
		t.Fatal("losing renewal extended or replaced winner")
	}
	p.sessionRoutes[key] = gatewaySessionRoute{Account: "winner", Expires: time.Now().Add(-time.Second)}
	if got, err := p.touchGatewaySession(t.Context(), "key", "session", "winner"); err != nil || got != "" || p.sessionRoutes[key].Expires.After(time.Now()) {
		t.Fatal("renewal revived an expired owner")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.touchGatewaySession(ctx, "key", "session", "winner"); !errors.Is(err, context.Canceled) {
		t.Fatal("renewal ignored caller cancellation")
	}
}

func TestGatewayRedisSessionLookupDoesNotExtendTTL(t *testing.T) {
	db, key := gatewayRedisSessionFixture(t)
	p := &Proxy{Store: Store{DB: db}}
	ctx := t.Context()
	if _, err := p.sessionAccount(ctx, "key", "session", "winner"); err != nil {
		t.Fatal(err)
	}
	if err := db.Redis.PExpire(ctx, key, 20*time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	for _, proposal := range []string{"", "winner", "old-owner"} {
		if got, err := p.sessionAccount(ctx, "key", "session", proposal); err != nil || got != "winner" {
			t.Fatal("lookup changed owner", err)
		}
		if ttl := db.Redis.PTTL(ctx, key).Val(); ttl > 20*time.Second || ttl <= 0 {
			t.Fatalf("lookup extended expiry: %s", ttl)
		}
	}
	if got, err := p.touchGatewaySession(ctx, "key", "session", "old-owner"); err != nil || got != "winner" || db.Redis.PTTL(ctx, key).Val() > 20*time.Second {
		t.Fatal("losing renewal changed TTL/owner")
	}
	if got, err := p.touchGatewaySession(ctx, "key", "session", "winner"); err != nil || got != "winner" || db.Redis.PTTL(ctx, key).Val() < gatewaySessionTTL-time.Second {
		t.Fatal("matching authenticated renewal failed", err)
	}
	if err := db.Redis.Del(ctx, key).Err(); err != nil {
		t.Fatal(err)
	}
	if got, err := p.touchGatewaySession(ctx, "key", "session", "winner"); err != nil || got != "" || db.Redis.Exists(ctx, key).Val() != 0 {
		t.Fatal("renewal resurrected missing owner")
	}
}
