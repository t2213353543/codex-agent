package server

import (
	"context"
	"testing"
	"time"
)

func TestGatewaySessionLeaseRenewsExistingBinding(t *testing.T) {
	p := newTestProxy(t, 1)
	account := Account{ID: "account", Name: "test", AccountID: "upstream"}
	expected := gatewayAccountRef(account)
	if _, err := p.sessionAccount(context.Background(), "key", "session", expected); err != nil {
		t.Fatal(err)
	}
	p.sessionMu.Lock()
	for key, route := range p.sessionRoutes {
		route.Expires = time.Now().Add(time.Second)
		p.sessionRoutes[key] = route
	}
	p.sessionMu.Unlock()
	ctx, stop := p.keepGatewaySessionEvery(context.Background(), "key", "session", account, time.Millisecond)
	defer stop()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("renewing the original account canceled the request")
		case <-deadline.C:
			t.Fatal("active session was not renewed")
		case <-poll.C:
			p.sessionMu.Lock()
			renewed := false
			for _, route := range p.sessionRoutes {
				renewed = route.Account == expected && time.Until(route.Expires) > time.Minute
			}
			p.sessionMu.Unlock()
			if renewed {
				stop()
				if ctx.Err() == nil {
					t.Fatal("stop did not cancel the upstream request context")
				}
				return
			}
		}
	}
}

func TestGatewaySessionLeaseCancelsOnBindingFailure(t *testing.T) {
	for _, failure := range []string{"conflicting-owner", "missing-redis", "expired"} {
		t.Run(failure, func(t *testing.T) {
			p := newTestProxy(t, 1)
			account := Account{ID: "account", Name: "test"}
			if failure == "conflicting-owner" {
				if _, err := p.sessionAccount(context.Background(), "key", "session", "id:other"); err != nil {
					t.Fatal(err)
				}
			} else if failure == "missing-redis" {
				p.Store.DB = &Database{}
			} else {
				p.sessionRoutes = map[string]gatewaySessionRoute{auditSessionMapKey("key", "session"): {Account: gatewayAccountRef(account), Expires: time.Now().Add(-time.Second)}}
			}
			ctx, stop := p.keepGatewaySessionEvery(context.Background(), "key", "session", account, time.Millisecond)
			defer stop()
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("lost session affinity did not cancel active upstream request")
			}
			if failure == "conflicting-owner" {
				got, err := p.sessionAccount(context.Background(), "key", "session", "")
				if err != nil || got != "id:other" {
					t.Fatalf("renewal overwrote conflicting account: %q %v", got, err)
				}
			}
		})
	}
}

func TestGatewaySessionLeaseBypassesUnpinnedRequests(t *testing.T) {
	p := newTestProxy(t, 1)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, stop := p.keepGatewaySession(parent, "key", "", Account{})
	stop()
	if ctx != parent || ctx.Err() != nil {
		t.Fatal("unidentified request received an unnecessary lease context")
	}
	settings, _ := p.Settings.Get()
	settings.StickySessionsEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	ctx, stop = p.keepGatewaySession(parent, "key", "session", Account{})
	stop()
	if ctx != parent || ctx.Err() != nil || len(p.sessionRoutes) != 0 {
		t.Fatal("disabled affinity still created a lease")
	}
}
