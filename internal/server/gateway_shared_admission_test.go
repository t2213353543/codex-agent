package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func sharedAdmissionAccount() Account {
	return Account{ID: "synthetic-stable-management-id", Name: "synthetic-old-name", Concurrency: 2}
}
func sharedAdmissionTestPolicy() gatewaySharedAdmissionPolicy {
	return gatewaySharedAdmissionPolicy{ttl: 3 * time.Second, renew: 500 * time.Millisecond, operation: 500 * time.Millisecond, cancelMargin: time.Second}
}

func TestSharedAdmissionFileFallbackAndDatabaseFailClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a := sharedAdmissionAccount()
	a.Concurrency = 1
	p := &Proxy{}
	lease, ok, err := p.acquireGatewayAccountContext(ctx, a)
	if err != nil || !ok || lease == nil {
		t.Fatal("file fixture local admission changed")
	}
	if _, ok, err := p.acquireGatewayAccountContext(ctx, a); ok || err != nil {
		t.Fatal("file local capacity not enforced")
	}
	lease.Release()
	lease.Release()
	if p.inFlight(gatewayAdmissionKey(a)) != 0 {
		t.Fatal("idempotent local release leaked")
	}
	database := &Proxy{Store: Store{DB: &Database{Namespace: "synthetic:"}}}
	if _, ok, err := database.acquireGatewayAccountContext(ctx, a); ok || !gatewaySharedAdmissionFailure(err) || database.inFlight(gatewayAdmissionKey(a)) != 0 {
		t.Fatal("database missing Redis silently fell back to local")
	}
	cancel()
	if _, ok, err := p.acquireGatewayAccountContext(ctx, a); ok || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled caller admitted")
	}
}

func TestSharedAdmissionRedisTwoProxiesCommonLimitAndIdentity(t *testing.T) {
	f := newBPSRedisReviewFixture(t)
	first, second := f.proxy(), f.proxy()
	a := sharedAdmissionAccount()
	f.track(gatewaySharedAdmissionKey(f.namespace, a))
	one, ok, err := first.acquireGatewayAccountContext(t.Context(), a)
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer one.Release()
	two, ok, err := second.acquireGatewayAccountContext(t.Context(), a)
	if err != nil || !ok {
		t.Fatal("second allowed slot was not shared")
	}
	defer two.Release()
	renamed := a
	renamed.Name = "renamed"
	renamed.AccessToken = "rotated-private-token"
	renamed.CredentialVersion = 99
	if _, ok, err := first.acquireGatewayAccountContext(t.Context(), renamed); ok || err != nil {
		t.Fatal("rename/rotation created a new account budget")
	}
	if count := first.Store.DB.Redis.ZCard(t.Context(), gatewaySharedAdmissionKey(f.namespace, a)).Val(); count != 2 {
		t.Fatalf("expected shared count2 got%d", count)
	}
	one.Release()
	one.Release()
	if count := second.Store.DB.Redis.ZCard(t.Context(), gatewaySharedAdmissionKey(f.namespace, a)).Val(); count != 1 {
		t.Fatal("releasing old token removed another request")
	}
	three, ok, err := first.acquireGatewayAccountContext(t.Context(), renamed)
	if err != nil || !ok {
		t.Fatal("released shared slot not available")
	}
	defer three.Release()
	lowered := a
	lowered.Concurrency = 1
	if _, ok, err := second.acquireGatewayAccountContext(t.Context(), lowered); ok || err != nil {
		t.Fatal("current lower limit was ignored")
	}
	other := a
	other.ID = "different-account"
	f.track(gatewaySharedAdmissionKey(f.namespace, other))
	separate, ok, err := second.acquireGatewayAccountContext(t.Context(), other)
	if err != nil || !ok {
		t.Fatal("different account incorrectly shared budget")
	}
	separate.Release()
	// Same identity in a separate deployment namespace must be independent.
	otherNamespace := f.namespace + "separate:"
	second.Store.DB.Namespace = otherNamespace
	f.track(gatewaySharedAdmissionKey(otherNamespace, a))
	isolated, ok, err := second.acquireGatewayAccountContext(t.Context(), a)
	if err != nil || !ok {
		t.Fatal("namespace budget leaked")
	}
	isolated.Release()
	second.Store.DB.Namespace = f.namespace
	two.Release()
	three.Release()
	if count := first.Store.DB.Redis.Exists(t.Context(), gatewaySharedAdmissionKey(f.namespace, a)).Val(); count != 0 {
		t.Fatal("empty lease key leaked")
	}
}

func TestSharedAdmissionRedisConcurrentBoundAcrossProxies(t *testing.T) {
	f := newBPSRedisReviewFixture(t)
	proxies := []*Proxy{f.proxy(), f.proxy(), f.proxy()}
	a := sharedAdmissionAccount()
	a.Concurrency = 3
	f.track(gatewaySharedAdmissionKey(f.namespace, a))
	var mu sync.Mutex
	leases := []*gatewayAccountAdmission{}
	var failures []error
	start := make(chan struct{})
	var group sync.WaitGroup
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			l, ok, e := proxies[index%len(proxies)].acquireGatewayAccountContext(t.Context(), a)
			mu.Lock()
			defer mu.Unlock()
			if e != nil {
				failures = append(failures, e)
			}
			if ok {
				leases = append(leases, l)
			}
		}(i)
	}
	close(start)
	group.Wait()
	defer func() {
		for _, l := range leases {
			l.Release()
		}
	}()
	if len(failures) != 0 || len(leases) != 3 {
		t.Fatalf("global limit breach or store misclassified: admitted=%d errors=%v", len(leases), failures)
	}
	if count := proxies[0].Store.DB.Redis.ZCard(t.Context(), gatewaySharedAdmissionKey(f.namespace, a)).Val(); count != 3 {
		t.Fatal("actual Redis active count does not equal admission")
	}
}

func TestSharedAdmissionRedisRenewalAndLossCancelRealTransport(t *testing.T) {
	f := newBPSRedisReviewFixture(t)
	p, observer := f.proxy(), f.proxy()
	a := sharedAdmissionAccount()
	a.Concurrency = 1
	key := f.track(gatewaySharedAdmissionKey(f.namespace, a))
	parent, cancelParent := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancelParent()
	route := withGatewayRequestRoute(parent, "synthetic-key", "synthetic-session")
	defer stopGatewayRequestRoute(route)
	l, ok, err := p.acquireGatewayAccountWithPolicy(route, a, sharedAdmissionTestPolicy())
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer l.Release()
	// Hold beyond one original TTL to prove successful renewal, not merely a
	// pre-expiry duplicate denial. All waits are bounded by the caller context.
	timer := time.NewTimer(3200 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-l.Context.Done():
		t.Fatalf("healthy lease canceled before renewal proof: %v", context.Cause(l.Context))
	}
	if _, ok, err := observer.acquireGatewayAccountContext(parent, a); ok || err != nil {
		t.Fatal("renewing live request expired out of shared limit")
	}
	entered, stopped := make(chan struct{}), make(chan struct{})
	cleanupHandler := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
			close(stopped)
		case <-cleanupHandler:
		}
	}))
	defer upstream.Close()
	defer close(cleanupHandler)
	p.Client, p.BaseURL = upstream.Client(), upstream.URL
	request := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","input":"synthetic"}`)
	transportDone := make(chan error, 1)
	go func() {
		// The real BPS writer carries the original request route, not the
		// returned lease.Context. Renewal loss must cancel that route too.
		res, e := p.bpsRequest(route, request, a, nil, "synthetic-key", "/responses", []byte(`{"model":"gpt-6-astra","input":[]}`), defaultSettings(), &RequestRecord{})
		if res != nil {
			res.Body.Close()
		}
		transportDone <- e
	}()
	select {
	case <-entered:
	case <-parent.Done():
		t.Fatal("transport did not start")
	}
	// Close only this proxy's Redis pool; other process-equivalent clients stay
	// healthy. Renewal failure must fail closed and cancel the actual transport.
	if err := p.Store.DB.Redis.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-l.Context.Done():
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("renewal failure did not cancel lease before Redis expiry")
	}
	if parent.Err() != nil || !gatewaySharedAdmissionFailure(context.Cause(l.Context)) {
		t.Fatal("independent caller falsely canceled or typed cause missing")
	}
	// failLease cancels the child lease, then the original request route.
	// Observing the first Done does not synchronize the following cancel call;
	// wait for the route's own signal before checking its published cause.
	select {
	case <-route.Done():
	case <-time.After(time.Second):
		t.Fatal("selected route did not receive admission cancellation")
	}
	if !gatewaySharedAdmissionFailure(context.Cause(route)) {
		t.Fatal("selected route did not receive typed admission cancellation")
	}
	select {
	case e := <-transportDone:
		if e == nil {
			t.Fatal("transport succeeded after lost admission")
		}
	case <-time.After(time.Second):
		t.Fatal("request did not observe lease cancellation")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("upstream context not canceled")
	}
	if score := observer.Store.DB.Redis.ZCard(parent, key).Val(); score != 1 {
		t.Fatal("failed renew prematurely freed shared slot before caller cleanup")
	}
	if _, ok, err := p.acquireGatewayAccountContext(parent, a); ok || !gatewaySharedAdmissionFailure(err) {
		t.Fatal("Redis outage reported busy or fell back local")
	}
	l.Release()
	if p.inFlight(gatewayAdmissionKey(a)) != 0 {
		t.Fatal("local observation not released after failed shared cleanup")
	}
}

type sharedAdmissionRenewHook struct {
	entered chan struct{}
	once    sync.Once
}

func (h *sharedAdmissionRenewHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *sharedAdmissionRenewHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *sharedAdmissionRenewHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if cmd.Name() == "eval" && len(args) > 1 && args[1] == gatewaySharedAdmissionRenew {
			h.once.Do(func() { close(h.entered) })
			<-ctx.Done()
			return ctx.Err()
		}
		return next(ctx, cmd)
	}
}

func TestSharedAdmissionRedisReleaseDuringRenewKeepsOutputRouteAlive(t *testing.T) {
	f := newBPSRedisReviewFixture(t)
	p := f.proxy()
	a := sharedAdmissionAccount()
	key := f.track(gatewaySharedAdmissionKey(f.namespace, a))
	hook := &sharedAdmissionRenewHook{entered: make(chan struct{})}
	p.Store.DB.Redis.AddHook(hook)
	route := withGatewayRequestRoute(t.Context(), "key", "session")
	defer stopGatewayRequestRoute(route)
	l, ok, err := p.acquireGatewayAccountWithPolicy(route, a, sharedAdmissionTestPolicy())
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer l.Release()
	select {
	case <-hook.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("renewal did not reach controlled cancellation boundary")
	}
	l.Release()
	if route.Err() != nil {
		t.Fatal("normal successful release canceled downstream terminal delivery")
	}
	if count := p.Store.DB.Redis.Exists(t.Context(), key).Val(); count != 0 {
		t.Fatal("shared lease leaked after concurrent normal release")
	}
	known := int64(7)
	rec := RequestRecord{Status: "ok", InputTokens: 100, CachedTokens: 80, CacheWriteTokens: &known, TTFTMs: &known}
	observeGatewayAdmissionLoss(&rec, route)
	if rec.Status != "ok" || rec.TTFTMs == nil || rec.InputTokens != 100 {
		t.Fatal("successful release relabeled or changed observations")
	}
}

func TestSharedAdmissionRedisExpiredTokenCannotReviveOrReleaseSuccessor(t *testing.T) {
	f := newBPSRedisReviewFixture(t)
	p := f.proxy()
	a := sharedAdmissionAccount()
	key := f.track(gatewaySharedAdmissionKey(f.namespace, a))
	if err := p.Store.DB.Redis.ZAddArgs(t.Context(), key, redisZExpiredForAdmission()).Err(); err != nil {
		t.Fatal(err)
	}
	if got, err := p.Store.DB.Redis.Eval(t.Context(), gatewaySharedAdmissionRenew, []string{key}, "old-token", 30000).Int(); err != nil || got != 0 {
		t.Fatal("expired token was resurrected")
	}
	a.Concurrency = 1
	l, ok, err := p.acquireGatewayAccountContext(t.Context(), a)
	if err != nil || !ok {
		t.Fatal("expired entry not pruned")
	}
	defer l.Release()
	if got, err := p.Store.DB.Redis.Eval(t.Context(), gatewaySharedAdmissionRelease, []string{key}, "old-token").Int(); err != nil || got != 0 {
		t.Fatal("obsolete release affected successor")
	}
	if count := p.Store.DB.Redis.ZCard(t.Context(), key).Val(); count != 1 {
		t.Fatal("new token lost after old release")
	}
}

// Process child uses the same namespace and real Redis, but no shared Go map.
func TestSharedAdmissionRedisChild(t *testing.T) {
	namespace := os.Getenv("SHARED_ADMISSION_CHILD_NAMESPACE")
	if namespace == "" {
		t.Skip("isolated subprocess only")
	}
	if !strings.HasPrefix(namespace, "codex-agent:integration:bps-test:") {
		t.Fatal("unexpected child namespace")
	}
	f := newBPSRedisReviewFixture(t)
	p := f.proxy()
	p.Store.DB.Namespace = namespace
	a := sharedAdmissionAccount()
	a.Concurrency = 1
	l, ok, err := p.acquireGatewayAccountContext(t.Context(), a)
	if l != nil {
		l.Release()
	}
	if err != nil || ok {
		t.Fatal("separate process violated parent-held shared limit")
	}
}

func TestSharedAdmissionRedisSeparateProcessHonorsHeldSlot(t *testing.T) {
	f := newBPSRedisReviewFixture(t)
	p := f.proxy()
	a := sharedAdmissionAccount()
	a.Concurrency = 1
	f.track(gatewaySharedAdmissionKey(f.namespace, a))
	l, ok, err := p.acquireGatewayAccountContext(t.Context(), a)
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer l.Release()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestSharedAdmissionRedisChild$", "-test.v")
	cmd.Env = append(os.Environ(), "SHARED_ADMISSION_CHILD_NAMESPACE="+f.namespace)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("independent process did not respect limit: %v %s", err, output)
	}
	if !strings.Contains(string(output), "PASS") || strings.Contains(string(output), "SKIP") {
		t.Fatal("independent process proof was skipped")
	}
	t.Logf("separate_process_proof=%s", string(mustJSONForAdmission(map[string]any{"passed": true, "child_bytes": len(output), "parent_inflight": p.inFlight(gatewayAdmissionKey(a))})))
}

func mustJSONForAdmission(v any) []byte { raw, _ := json.Marshal(v); return raw }

func redisZExpiredForAdmission() redis.ZAddArgs {
	return redis.ZAddArgs{Members: []redis.Z{{Score: 1, Member: "old-token"}}}
}
