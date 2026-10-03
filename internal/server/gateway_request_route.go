package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
)

type gatewayRequestRouteKey struct{}

type gatewayRequestRoute struct {
	keyID, session  string
	mu              sync.Mutex
	proxies         map[string]*ProxyEntry
	admissionCancel context.CancelCauseFunc
}

// One admission attempt can refresh OAuth before sending its actual request.
// Keep that exchange on the same selected proxy, even with session affinity off.
func withGatewayRequestRoute(ctx context.Context, keyID, session string) context.Context {
	ctx, cancel := context.WithCancelCause(ctx)
	return context.WithValue(ctx, gatewayRequestRouteKey{}, &gatewayRequestRoute{
		keyID: keyID, session: session, proxies: make(map[string]*ProxyEntry),
		admissionCancel: cancel,
	})
}

// Call when the route's handler finishes, after any successful terminal write.
// Releasing admission earlier from OnComplete must not cancel output delivery.
func stopGatewayRequestRoute(ctx context.Context) {
	if route, _ := ctx.Value(gatewayRequestRouteKey{}).(*gatewayRequestRoute); route != nil && route.admissionCancel != nil {
		route.admissionCancel(context.Canceled)
	}
}

func (p *Proxy) gatewayRequestProxy(ctx context.Context, account Account) (*ProxyEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	route, _ := ctx.Value(gatewayRequestRouteKey{}).(*gatewayRequestRoute)
	var proxy *ProxyEntry
	if route != nil {
		route.mu.Lock()
		defer route.mu.Unlock()
		ref := gatewayAccountOwnerRef(account)
		if selected, ok := route.proxies[ref]; ok {
			// Preserve the exact admitted route, but reject an assignment
			// changed during refresh instead of bypassing the new binding.
			if (selected == nil && len(account.Proxies) > 0) ||
				(selected != nil && !slices.Contains(account.Proxies, selected.ID)) {
				return nil, errGatewayProxyUnavailable
			}
			return selected, nil
		}
		proxySeed := route.session
		if proxySeed == "" {
			proxySeed = gatewayCacheAffinityProxySeed(ctx)
		}
		proxy = p.pickSessionProxy(account, route.keyID, proxySeed)
		if proxy == nil && len(account.Proxies) > 0 {
			return nil, errors.New("account has no available configured outbound proxy")
		}
		route.proxies[ref] = proxy
	} else {
		proxy = p.pickProxy(account)
		if proxy == nil && len(account.Proxies) > 0 {
			return nil, errors.New("account has no available configured outbound proxy")
		}
	}
	return proxy, nil
}

// The ordinary pool resolves a selected proxy ID to its newest configuration.
// Within one admitted request, OAuth and inference instead must use the exact
// selected snapshot. A changed endpoint/credential/version fails this attempt;
// a subsequent request can select the new configuration normally. The pooled
// transport also rechecks its fingerprint at RoundTrip to cover later edits.
func (p *Proxy) gatewayRequestClient(ctx context.Context, proxy *ProxyEntry, mode gatewayClientMode) *http.Client {
	return p.inferenceClient(ctx, proxy, mode, nil)
}

func gatewayPinnedRequestClient(ctx context.Context, proxy *ProxyEntry, client *http.Client) *http.Client {
	if proxy == nil || ctx.Value(gatewayRequestRouteKey{}) == nil {
		// Explicit management refreshes have no admission route and may adopt
		// current configuration, preserving the normal clientFor contract.
		return client
	}
	var fingerprint [32]byte
	var ok bool
	switch transport := client.Transport.(type) {
	case *gatewayClientTransport:
		fingerprint, ok = transport.fingerprint, true
	case *gatewayNativeGuardedTransport:
		fingerprint, ok = transport.fingerprint, true
	}
	if !ok || fingerprint != gatewayProxyFingerprint(*proxy) {
		return &http.Client{Transport: gatewayClientErrorTransport{}}
	}
	return client
}
