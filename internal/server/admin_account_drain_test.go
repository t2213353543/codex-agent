package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

func drainTestRequest(account Account) *accountDrainRequest {
	return &accountDrainRequest{OperationID: accountDrainSHA("synthetic-operation:" + account.ID), WorkspaceSHA: accountDrainSHA(account.AccountID), UserSHA: accountDrainSHA(account.Identity.UserID)}
}

func drainPGFixture(t *testing.T) (*Proxy, *Proxy, Account) {
	t.Helper()
	p, _ := adminPGTestProxy(t)
	if p.Store.DB.Redis == nil {
		t.Skip("TEST_REDIS_URL required")
	}
	settings := defaultSettings()
	settings.AdminAPI = true
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.ImportWith("drain-primary", gatewayRevisionCredentials(t, "drain-user", "drain-initial"), AccountDefaults{Concurrency: 4}); err != nil {
		t.Fatal(err)
	}
	account, err := p.Store.Get("drain-primary")
	if err != nil || !gatewayCompleteOwner(account) {
		t.Fatal("missing complete synthetic identity", err)
	}
	return p, &Proxy{Store: p.Store}, account
}

func drainDeleteContextFor(status accountDrainStatus) context.Context {
	return context.WithValue(context.Background(), accountDrainDeleteContextKey{}, accountDrainDeleteFence{status.Token, status.OwnerSHA})
}

func TestAccountDrainFileDoesNotClaimSharedCapability(t *testing.T) {
	p, _ := webTestSetup(t)
	h := p.compatRoutes()
	capability := sub2Data(t, sub2TestRequest(t, h, "GET", "/api/v1/admin/capabilities", nil, 200))
	if _, ok := capability["account_drain"]; ok {
		t.Fatal("file store claimed shared cluster drain")
	}
	sub2TestRequest(t, h, "POST", "/api/v1/admin/accounts/1/drain", map[string]string{}, 503)
	for _, route := range []string{"/api/v1/admin/capabilities", "/api/v1/admin/accounts/1/drain"} {
		r := httptest.NewRequest("GET", route, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("drain management route allowed non-admin")
		}
	}
	if code, retry, recognized := gatewaySelectionFailure(errAccountDraining); code != 503 || retry != "" || !recognized {
		t.Fatal("drain treated as rate limit or introduced wait")
	}
}

func TestPGAccountDrainProtocolSharedLeasesRenewAndFencedDelete(t *testing.T) {
	p, peer, account := drainPGFixture(t)
	h := p.compatRoutes()
	path := fmt.Sprintf("/api/v1/admin/accounts/%d", compatNumericID(account.ID))
	capability := sub2Data(t, sub2TestRequest(t, h, "GET", "/api/v1/admin/capabilities", nil, 200))
	if capability["account_drain"] != accountDrainCapability || capability["account_drain_scope"] != accountDrainScope {
		t.Fatal("capability not scoped/versioned")
	}
	sub2TestRequest(t, h, "POST", path+"/drain", map[string]string{}, 400)
	wrongIdentity := *drainTestRequest(account)
	wrongIdentity.UserSHA = accountDrainSHA("different-user")
	sub2TestRequest(t, h, "POST", path+"/drain", wrongIdentity, 409)
	first, ok, err := p.acquireGatewayAccountWithPolicy(t.Context(), account, sharedAdmissionTestPolicy())
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer first.Release()
	second, ok, err := peer.acquireGatewayAccountWithPolicy(t.Context(), account, sharedAdmissionTestPolicy())
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer second.Release()
	body := sub2Data(t, sub2TestRequest(t, h, "POST", path+"/drain", drainTestRequest(account), 200))
	if body["capability"] != accountDrainCapability || body["scope"] != accountDrainScope || body["account_id"] != float64(compatNumericID(account.ID)) || body["status"] != "draining" || body["in_flight"] != float64(2) {
		t.Fatalf("invalid drain response: %v", body)
	}
	if value, exists := body["pending_tool_sessions"]; !exists || value != nil {
		t.Fatal("unknown pending tools reported as zero or omitted")
	}
	for _, proxy := range []*Proxy{p, peer} {
		if l, admitted, err := proxy.acquireGatewayAccountContext(t.Context(), account); admitted || l != nil || !errors.Is(err, errAccountDraining) {
			t.Fatal("draining peer admitted new request", err)
		}
	}
	current, err := p.Store.Get(account.ID)
	if err != nil || !reflect.DeepEqual(current, account) {
		t.Fatal("drain altered enabled/owner/credentials/metadata", err)
	}
	if err := p.checkGatewayAccountAccess(t.Context(), webTestKey, "admin", current); err != nil {
		t.Fatal("drain invalidated existing request authorization", err)
	}
	// Beyond the original TTL, both requests must still hold valid shared leases.
	select {
	case <-time.After(3500 * time.Millisecond):
	case <-first.Context.Done():
		t.Fatal("drain canceled active lease")
	}
	status, err := peer.accountDrain(t.Context(), account.ID, nil)
	if err != nil || status.InFlight != 2 || first.Context.Err() != nil || second.Context.Err() != nil {
		t.Fatal("old leases stopped renewing under drain", status, err)
	}
	if status.Token != body["drain_token"] || status.OwnerSHA != body["owner_ref_sha"] {
		t.Fatal("GET recovery replaced token/owner")
	}
	if err := p.compatDeleteAccount(drainDeleteContextFor(status), account.ID, 0); !errors.Is(err, errAccountDraining) {
		t.Fatal("active lease was deleted", err)
	}
	first.Release()
	second.Release()
	status, err = peer.accountDrain(t.Context(), account.ID, nil)
	if err != nil || status.Status != "draining" || status.InFlight != 0 || status.RemainingSafetyMS <= 0 || status.SafetyReason != "lease_expiry_observation" {
		t.Fatal("released requests did not drain", status, err)
	}
	if err := p.compatDeleteAccount(drainDeleteContextFor(status), account.ID, 0); !errors.Is(err, errAccountDraining) {
		t.Fatal("delete bypassed lease-loss observation interval", err)
	}
	advanceDrainZeroObservation(t, p, account)
	status, err = peer.accountDrain(t.Context(), account.ID, nil)
	if err != nil || status.Status != "drained" || status.RemainingSafetyMS != 0 {
		t.Fatal("stable zero observation did not settle", status, err)
	}
	sub2TestRequest(t, h, "DELETE", path, nil, 409)
	r := httptest.NewRequest("DELETE", path, nil)
	r.Header.Set("X-API-Key", webTestKey)
	r.Header.Set("X-Codex-Drain-Token", status.Token)
	r.Header.Set("X-Codex-Owner-Ref-Sha", status.OwnerSHA)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("fenced delete failed: %d %s", w.Code, w.Body.String())
	}
	var count int
	if err = p.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM account_drains WHERE account_id=$1", account.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("drain history survived FK delete", count, err)
	}
	ttl, err := p.Store.DB.Redis.PTTL(t.Context(), accountDrainKey(p.Store.DB.Namespace, account)).Result()
	if err != nil || ttl <= 0 || ttl > accountDrainFenceTTL {
		t.Fatal("orphan Redis drain has no finite expiry", ttl, err)
	}
}

func TestPGAccountDrainLostResponseRedisLossAndOwnerFence(t *testing.T) {
	p, peer, account := drainPGFixture(t)
	first, err := p.accountDrain(t.Context(), account.ID, drainTestRequest(account))
	if err != nil {
		t.Fatal(err)
	}
	// Model a committed POST whose response never reached the caller.
	recovered, err := peer.accountDrain(t.Context(), account.ID, nil)
	if err != nil || recovered.Token != first.Token || recovered.OwnerSHA != first.OwnerSHA || recovered.Status != first.Status {
		t.Fatal("GET did not recover committed operation", err)
	}
	replayed, err := peer.accountDrain(t.Context(), account.ID, drainTestRequest(account))
	if err != nil || replayed.Token != first.Token || replayed.OwnerSHA != first.OwnerSHA || replayed.Status != first.Status {
		t.Fatal("idempotent replay changed operation", err)
	}
	different := *drainTestRequest(account)
	different.OperationID = accountDrainSHA("second-operation")
	if _, err := peer.accountDrain(t.Context(), account.ID, &different); !errors.Is(err, errAccountDrainConflict) {
		t.Fatal("another operation replaced existing drain", err)
	}
	if err := p.Store.DB.Redis.Del(t.Context(), accountDrainKey(p.Store.DB.Namespace, account)).Err(); err != nil {
		t.Fatal(err)
	}
	if lease, admitted, err := peer.acquireGatewayAccountContext(t.Context(), account); lease != nil || admitted || !errors.Is(err, errAccountDraining) {
		t.Fatal("Redis fence loss reopened account", err)
	}
	if _, err := peer.accountDrain(t.Context(), account.ID, nil); err != nil {
		t.Fatal("read-only recovery failed after Redis loss", err)
	}
	drainReplaceTestAccount(t, p, account.ID, "drain-user", "drain-token-rotation")
	rotated, err := peer.accountDrain(t.Context(), account.ID, nil)
	if err != nil || rotated.Token != first.Token || rotated.OwnerSHA != first.OwnerSHA || rotated.CredentialVersion <= first.CredentialVersion {
		t.Fatal("same-owner credential rotation invalidated retirement", rotated, err)
	}
	if _, err := p.Store.UpdateAccountVersion(account.ID, 0, func(a *Account) error { a.Name = "metadata-rename"; return nil }); err != nil {
		t.Fatal(err)
	}
	if after, err := peer.accountDrain(t.Context(), account.ID, nil); err != nil || after.Token != first.Token {
		t.Fatal("metadata rename invalidated retirement", err)
	}
	drainReplaceTestAccount(t, p, account.ID, "replacement-user", "replacement-token")
	if _, err := peer.accountDrain(t.Context(), account.ID, nil); !errors.Is(err, errAccountDrainConflict) {
		t.Fatal("GET accepted replacement owner", err)
	}
	if err := p.compatDeleteAccount(drainDeleteContextFor(first), account.ID, 0); !errors.Is(err, errAccountDrainConflict) {
		t.Fatal("old token deleted replacement owner", err)
	}
	if _, err := p.Store.Get(account.ID); err != nil {
		t.Fatal("replacement account lost", err)
	}
}

func drainReplaceTestAccount(t *testing.T, p *Proxy, id, user, generation string) {
	t.Helper()
	current, err := p.Store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	incoming, err := parseAccount(current.Name, gatewayRevisionCredentials(t, user, generation))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Store.reauthorizeManagedAccount(t.Context(), current.ID, current.Version, incoming, true); err != nil {
		t.Fatal(err)
	}
}

type accountDrainAcquireBarrier struct {
	entered, resume chan struct{}
	once            sync.Once
}

func (h *accountDrainAcquireBarrier) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *accountDrainAcquireBarrier) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *accountDrainAcquireBarrier) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if cmd.Name() == "eval" && len(args) > 1 && args[1] == gatewaySharedAdmissionAcquire {
			h.once.Do(func() {
				close(h.entered)
				select {
				case <-h.resume:
				case <-ctx.Done():
				}
			})
		}
		return next(ctx, cmd)
	}
}

func TestPGAccountDrainAdmissionRaceNOWAITAndAbortedFenceRecovery(t *testing.T) {
	p, peer, account := drainPGFixture(t)
	barrier := &accountDrainAcquireBarrier{entered: make(chan struct{}), resume: make(chan struct{})}
	p.Store.DB.Redis.AddHook(barrier)
	type result struct {
		lease    *gatewayAccountAdmission
		admitted bool
		err      error
	}
	resultCh := make(chan result, 1)
	go func() { l, ok, e := p.acquireGatewayAccountContext(t.Context(), account); resultCh <- result{l, ok, e} }()
	select {
	case <-barrier.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("admission did not reach barrier")
	}
	started := time.Now()
	_, err := peer.accountDrain(t.Context(), account.ID, drainTestRequest(account))
	if !errors.Is(err, errAccountDrainConflict) || time.Since(started) > 500*time.Millisecond {
		t.Fatal("drain did not fail closed without waiting on admission", err)
	}
	close(barrier.resume)
	admitted := <-resultCh
	if admitted.err != nil || !admitted.admitted {
		t.Fatal("pre-drain request was lost", admitted.err)
	}
	defer admitted.lease.Release()
	state, err := peer.accountDrain(t.Context(), account.ID, drainTestRequest(account))
	if err != nil || state.InFlight != 1 {
		t.Fatal("drain did not include racing admitted request", state, err)
	}
	// A separate account transaction holds the opposite side of the boundary.
	if err := p.Store.ImportWith("drain-second", gatewayRevisionCredentials(t, "second-user", "second-token"), AccountDefaults{}); err != nil {
		t.Fatal(err)
	}
	second, err := p.Store.Get("drain-second")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := p.Store.DB.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err = lockAccountDrain(t.Context(), tx, second.ID); err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	if l, ok, e := peer.acquireGatewayAccountContext(t.Context(), second); l != nil || ok || !errors.Is(e, errAccountDraining) || time.Since(started) > 500*time.Millisecond {
		t.Fatal("admission waited behind drain", e)
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A failed transaction may leave a short-lived cache fence. The PG row lock
	// proves absence of a durable drain before Lua atomically clears that fence.
	fenceKey := accountDrainKey(p.Store.DB.Namespace, second)
	if err = p.Store.DB.Redis.Set(t.Context(), fenceKey, accountDrainSHA("aborted-operation"), accountDrainFenceTTL).Err(); err != nil {
		t.Fatal(err)
	}
	l, ok, err := peer.acquireGatewayAccountContext(t.Context(), second)
	if err != nil || !ok {
		t.Fatal("aborted drain fence disabled healthy account", err)
	}
	l.Release()
	if p.Store.DB.Redis.Exists(t.Context(), fenceKey).Val() != 0 {
		t.Fatal("aborted fence not cleaned")
	}
	// Force a real failure after Lua succeeds but before PG commit. The next
	// same-operation POST recovers immediately without waiting for cache TTL.
	_, err = p.Store.DB.Pool.Exec(t.Context(), `CREATE FUNCTION fail_drain_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic commit failure'; END $$;
CREATE CONSTRAINT TRIGGER fail_drain_commit AFTER INSERT ON account_drains DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_drain_commit()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.accountDrain(t.Context(), second.ID, drainTestRequest(second)); err == nil {
		t.Fatal("deferred commit failure was reported successful")
	}
	var rows int
	if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM account_drains WHERE account_id=$1", second.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatal("failed transaction retained a drain row", err)
	}
	if p.Store.DB.Redis.Exists(t.Context(), fenceKey).Val() != 1 {
		t.Fatal("test did not reach post-Lua commit failure")
	}
	if _, err = p.Store.DB.Pool.Exec(t.Context(), "DROP TRIGGER fail_drain_commit ON account_drains"); err != nil {
		t.Fatal(err)
	}
	if recovered, err := peer.accountDrain(t.Context(), second.ID, drainTestRequest(second)); err != nil || recovered.Status != "draining" || recovered.InFlight != 0 {
		t.Fatal("aborted transaction fence blocked idempotent recovery", err)
	}
}

// Test-only advancement of the persisted Redis-clock observation. Production
// has no caller-supplied timestamp or configurable short safety interval.
func advanceDrainZeroObservation(t *testing.T, p *Proxy, account Account) {
	t.Helper()
	tag, err := p.Store.DB.Pool.Exec(t.Context(), "UPDATE account_drains SET zero_observed_at_ms=zero_observed_at_ms-$2 WHERE account_id=$1 AND zero_observed_at_ms>$2", account.ID, (gatewaySharedAdmissionDefaults.ttl + time.Second).Milliseconds())
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatal("test has no first-zero observation to advance", err)
	}
}

func TestPGAccountDrainLostLeaseRequiresZeroObservationBeforeDelete(t *testing.T) {
	p, peer, account := drainPGFixture(t)
	l, ok, err := p.acquireGatewayAccountWithPolicy(t.Context(), account, sharedAdmissionTestPolicy())
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer l.Release()
	if err := p.Store.DB.Redis.Del(t.Context(), gatewaySharedAdmissionKey(p.Store.DB.Namespace, account)).Err(); err != nil {
		t.Fatal(err)
	}
	if l.Context.Err() != nil {
		t.Fatal("fixture request was already stopped before lease loss observation")
	}
	status, err := peer.accountDrain(t.Context(), account.ID, drainTestRequest(account))
	if err != nil || status.Status != "draining" || status.InFlight != 0 || status.RemainingSafetyMS <= 0 {
		t.Fatal("missing Redis lease falsely proved drain complete", status, err)
	}
	if err := p.compatDeleteAccount(drainDeleteContextFor(status), account.ID, 0); !errors.Is(err, errAccountDraining) {
		t.Fatal("lease-loss gap allowed destructive retirement", err)
	}
	select {
	case <-l.Context.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("existing request watchdog did not cancel lost lease")
	}
	advanceDrainZeroObservation(t, p, account)
	status, err = peer.accountDrain(t.Context(), account.ID, nil)
	if err != nil || status.Status != "drained" {
		t.Fatal("safe observation could not finish retirement", status, err)
	}
	if err := p.compatDeleteAccount(drainDeleteContextFor(status), account.ID, 0); err != nil {
		t.Fatal(err)
	}
}

func TestPGAccountDrainRejectedDeleteResetsZeroObservation(t *testing.T) {
	p, _, account := drainPGFixture(t)
	status, err := p.accountDrain(t.Context(), account.ID, drainTestRequest(account))
	if err != nil {
		t.Fatal(err)
	}
	advanceDrainZeroObservation(t, p, account)
	now, err := p.Store.DB.Redis.Time(t.Context()).Result()
	if err != nil {
		t.Fatal(err)
	}
	key := gatewaySharedAdmissionKey(p.Store.DB.Namespace, account)
	if err := p.Store.DB.Redis.ZAdd(t.Context(), key, redis.Z{Member: "synthetic-restored-lease", Score: float64(now.Add(gatewaySharedAdmissionDefaults.ttl).UnixMilli())}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := p.compatDeleteAccount(drainDeleteContextFor(status), account.ID, 0); !errors.Is(err, errAccountDraining) {
		t.Fatal("nonzero delete was accepted", err)
	}
	var zero int64
	if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT zero_observed_at_ms FROM account_drains WHERE account_id=$1", account.ID).Scan(&zero); err != nil || zero != 0 {
		t.Fatal("rejected delete rolled back its nonzero observation", zero, err)
	}
	if err := p.Store.DB.Redis.Del(t.Context(), key).Err(); err != nil {
		t.Fatal(err)
	}
	if current, err := p.accountDrain(t.Context(), account.ID, nil); err != nil || current.Status != "draining" || current.RemainingSafetyMS <= 0 {
		t.Fatal("stale zero proof allowed immediate retirement", current, err)
	}
	for _, input := range []*accountDrainRequest{nil, drainTestRequest(account)} {
		advanceDrainZeroObservation(t, p, account)
		now, err := p.Store.DB.Redis.Time(t.Context()).Result()
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Store.DB.Redis.ZAdd(t.Context(), key, redis.Z{Member: "synthetic-restored-lease", Score: float64(now.Add(gatewaySharedAdmissionDefaults.ttl).UnixMilli())}).Err(); err != nil {
			t.Fatal(err)
		}
		if current, err := p.accountDrain(t.Context(), account.ID, input); err != nil || current.InFlight != 1 || current.Status != "draining" {
			t.Fatal("positive lease observation was not preserved", current, err)
		}
		if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT zero_observed_at_ms FROM account_drains WHERE account_id=$1", account.ID).Scan(&zero); err != nil || zero != 0 {
			t.Fatal("GET/POST did not reset zero proof", zero, err)
		}
		if err := p.Store.DB.Redis.Del(t.Context(), key).Err(); err != nil {
			t.Fatal(err)
		}
		if current, err := p.accountDrain(t.Context(), account.ID, nil); err != nil || current.Status != "draining" || current.RemainingSafetyMS != gatewaySharedAdmissionDefaults.ttl.Milliseconds() {
			t.Fatal("lease loss reused older zero observation", current, err)
		}
	}
}

type accountDrainLateAcquireHook struct {
	captured chan []any
	once     sync.Once
}

func (h *accountDrainLateAcquireHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *accountDrainLateAcquireHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *accountDrainLateAcquireHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		intercept := false
		if cmd.Name() == "eval" && len(args) > 1 && args[1] == gatewaySharedAdmissionAcquire {
			h.once.Do(func() { intercept = true })
		}
		if intercept {
			h.captured <- append([]any(nil), args...)
			<-ctx.Done()
			return ctx.Err()
		}
		return next(ctx, cmd)
	}
}

func TestPGAccountDrainLateAcquireCannotDeleteNewFence(t *testing.T) {
	p, peer, account := drainPGFixture(t)
	key := accountDrainKey(p.Store.DB.Namespace, account)
	oldToken := accountDrainSHA("old-orphan")
	if err := p.Store.DB.Redis.Set(t.Context(), key, oldToken, accountDrainFenceTTL).Err(); err != nil {
		t.Fatal(err)
	}
	hook := &accountDrainLateAcquireHook{captured: make(chan []any, 1)}
	p.Store.DB.Redis.AddHook(hook)
	if l, ok, err := p.acquireGatewayAccountWithPolicy(t.Context(), account, sharedAdmissionTestPolicy()); l != nil || ok || !gatewaySharedAdmissionFailure(err) {
		t.Fatal("lost acquire reply was accepted", err)
	}
	args := <-hook.captured
	status, err := peer.accountDrain(t.Context(), account.ID, drainTestRequest(account))
	if err != nil || status.Token == oldToken {
		t.Fatal("new operation not committed after timeout", err)
	}
	// Actually send the captured command to real Redis after its PG lock has
	// gone and a different node has committed the new retirement fence.
	got, err := p.Store.DB.Redis.Do(t.Context(), args...).Int()
	if err != nil || got != -2 {
		t.Fatal("late acquire removed replacement drain fence", got, err)
	}
	if actual := p.Store.DB.Redis.Get(t.Context(), key).Val(); actual != status.Token {
		t.Fatal("late acquire changed new token")
	}
	if count := p.Store.DB.Redis.ZCard(t.Context(), gatewaySharedAdmissionKey(p.Store.DB.Namespace, account)).Val(); count != 0 {
		t.Fatal("late acquire created phantom active lease", count)
	}
}

func TestPGAccountDrainManagementInferenceUsesSharedLease(t *testing.T) {
	for _, kind := range []string{"stream", "compact", "cpa"} {
		t.Run(kind, func(t *testing.T) {
			p, peer, account := drainPGFixture(t)
			started, finish := make(chan struct{}), make(chan struct{})
			var once sync.Once
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method == "GET" {
					fmt.Fprint(w, `{"models":[{"slug":"test-model"}]}`)
					return
				}
				once.Do(func() { close(started) })
				select {
				case <-finish:
				case <-r.Context().Done():
					return
				}
				if strings.HasSuffix(r.URL.Path, "/compact") {
					fmt.Fprint(w, `{"output":[{"type":"compaction","encrypted_content":"synthetic"}]}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
			}))
			defer upstream.Close()
			var finishOnce sync.Once
			releaseUpstream := func() { finishOnce.Do(func() { close(finish) }) }
			defer releaseUpstream()
			p.BaseURL = upstream.URL + "/backend-api/codex"
			call := func() int {
				if kind != "cpa" {
					return p.checkManagedUpstreamOptions(t.Context(), account, kind, "test-model", "", nil, nil).HTTPCode
				}
				body, _ := json.Marshal(cpaAPICallRequest{AuthIndex: compatAuthIndex(account.ID), Method: "POST", URL: upstream.URL + "/v1/responses", Data: `{"model":"test-model","input":"test"}`})
				r := httptest.NewRequest("POST", "/v0/management/api-call", strings.NewReader(string(body)))
				w := httptest.NewRecorder()
				p.handleCPAAPICall(w, r)
				return w.Code
			}
			result := make(chan int, 1)
			go func() { result <- call() }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("management test never reached real provider fixture")
			}
			status, err := peer.accountDrain(t.Context(), account.ID, drainTestRequest(account))
			if err != nil || status.InFlight != 1 || status.Status != "draining" {
				t.Fatal("management inference was absent from shared count", status, err)
			}
			if code := call(); code != 503 || calls.Load() != 1 {
				t.Fatal("new management inference bypassed drain", code, calls.Load())
			}
			if result := p.checkManagedUpstream(t.Context(), account, "models", ""); !result.OK {
				t.Fatal("metadata model lookup was blocked by inference drain", result.Error)
			}
			if calls.Load() != 2 {
				t.Fatal("metadata was not fetched once")
			}
			releaseUpstream()
			select {
			case code := <-result:
				if code != 200 {
					t.Fatal("existing management inference was interrupted", code)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("existing inference did not finish")
			}
			if count := p.Store.DB.Redis.ZCard(t.Context(), gatewaySharedAdmissionKey(p.Store.DB.Namespace, account)).Val(); count != 0 {
				t.Fatal("management lease leaked", count)
			}
		})
	}
}

func TestPGAccountDrainNonDrainingDeletePreservesUnreadableCredentialBehavior(t *testing.T) {
	p, _, account := drainPGFixture(t)
	if _, err := p.Store.DB.Pool.Exec(t.Context(), "UPDATE account_credentials SET credentials_cipher='broken'::bytea WHERE account_id=$1", account.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.compatDeleteAccount(t.Context(), account.ID, 0); err != nil {
		t.Fatal("non-draining legacy delete began requiring valid credentials", err)
	}
}

func TestPGAccountDrainWarmSelectionSkipsRetiringCandidate(t *testing.T) {
	f := newGatewaySelectionPGFixture(t, true, false)
	if _, err := f.writer.accountDrain(t.Context(), f.primary.ID, drainTestRequest(f.primary)); err != nil {
		t.Fatal(err)
	}
	f.keepOriginalCacheWarm(t)
	account, release, key, _, err := f.reader.pickAccount(t.Context(), f.key.Key)
	if release != nil {
		defer release()
	}
	if err != nil || key != f.key.ID || account.ID != f.alternate.ID {
		t.Fatal("one draining candidate disabled the pool", account.ID, err)
	}
	boundSession := "synthetic-drain-bound"
	if _, err := f.reader.sessionAccount(t.Context(), f.key.ID, boundSession, gatewayAccountRef(f.primary)); err != nil {
		t.Fatal(err)
	}
	bound, releaseBound, err := f.reader.pickSessionAccount(t.Context(), f.key.Key, f.key.ID, boundSession)
	if releaseBound != nil {
		releaseBound()
	}
	if !errors.Is(err, errAccountDraining) || bound.ID != "" {
		t.Fatal("bound draining owner migrated or admitted", err)
	}
}

func TestAccountDrainHeadersAreStrictAndNoSecretsInStatus(t *testing.T) {
	for _, headers := range []map[string][]string{
		{"X-Codex-Drain-Token": {strings.Repeat("a", 64)}},
		{"X-Codex-Drain-Token": {strings.Repeat("a", 64), strings.Repeat("a", 64)}, "X-Codex-Owner-Ref-Sha": {strings.Repeat("b", 64)}},
		{"X-Codex-Drain-Token": {"junk"}, "X-Codex-Owner-Ref-Sha": {strings.Repeat("b", 64)}},
	} {
		r := httptest.NewRequest("DELETE", "/api/v1/admin/accounts/1", nil)
		r.Header = headers
		if _, err := accountDrainDeleteContext(r); !errors.Is(err, errAccountDrainConflict) {
			t.Fatal("ambiguous fence accepted")
		}
	}
	raw, _ := json.Marshal(accountDrainStatus{Capability: accountDrainCapability, Scope: accountDrainScope})
	if !strings.Contains(string(raw), `"pending_tool_sessions":null`) {
		t.Fatal("unknown tools were presented as accounted for")
	}
}

type accountDrainEditTraceKey struct{}
type accountDrainEditTrace struct {
	locked, resume chan struct{}
	once           sync.Once
}

func (tr *accountDrainEditTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, accountDrainEditTraceKey{}, strings.Contains(data.SQL, "FROM accounts WHERE id=$1 FOR "))
}
func (tr *accountDrainEditTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if hold, _ := ctx.Value(accountDrainEditTraceKey{}).(bool); hold && data.Err == nil {
		tr.once.Do(func() {
			close(tr.locked)
			select {
			case <-tr.resume:
			case <-ctx.Done():
			}
		})
	}
}

// Pause each actual management path immediately after PostgreSQL has taken its
// account lock. Inference must complete admission while that edit is still
// uncommitted; only a real retirement/deletion needs to exclude new admission.
func TestPGAccountDrainOrdinaryManagementEditsDoNotBlockAdmission(t *testing.T) {
	for _, kind := range []string{"compat_metadata", "managed_lease", "reauthorize", "crs_duplicate"} {
		t.Run(kind, func(t *testing.T) {
			p, peer, account := drainPGFixture(t)
			trace := &accountDrainEditTrace{locked: make(chan struct{}), resume: make(chan struct{})}
			config := p.Store.DB.Pool.Config()
			config.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			writerDB := *p.Store.DB
			writerDB.Pool = pool
			p.Store.DB = &writerDB
			var finishOnce sync.Once
			finish := func() { finishOnce.Do(func() { close(trace.resume) }) }
			defer finish()
			done := make(chan error, 1)
			go func() {
				var err error
				switch kind {
				case "compat_metadata":
					_, err = p.compatMutateAccount(t.Context(), account.ID, account.Version, func(a *Account, _ *adminCompatRecord) error { a.Notes = "metadata change while serving"; return nil })
				case "managed_lease":
					err = p.updateManagedLease(t.Context(), account.ID, "", func(lease *managedAccountLease, _ *Account) error {
						lease.ExpiresAt = time.Now().Add(time.Hour).Unix()
						return nil
					})
				case "reauthorize":
					_, err = p.Store.reauthorizeManagedAccount(t.Context(), account.ID, account.Version, account, false)
				case "crs_duplicate":
					_, _, _, err = p.commitManagedCRSAccountPG(t.Context(), "synthetic-drain-crs-link", managedCRSRow{}, account, account, false)
				}
				done <- err
			}()
			select {
			case <-trace.locked:
			case err := <-done:
				t.Fatal("management path missed its actual account lock", err)
			case <-time.After(3 * time.Second):
				t.Fatal("management path did not reach account lock")
			}
			started := time.Now()
			lease, admitted, err := peer.acquireGatewayAccountContext(t.Context(), account)
			if err != nil || !admitted || lease == nil {
				finish()
				<-done
				t.Fatal("ordinary management edit was misclassified as account drain", err)
			}
			defer lease.Release()
			if time.Since(started) > 500*time.Millisecond {
				t.Fatal("admission waited for unrelated metadata edit")
			}
			select {
			case err := <-done:
				t.Fatal("edit committed before concurrency assertion", err)
			default:
			}
			finish()
			if err := <-done; err != nil {
				t.Fatal("ordinary edit failed after concurrent admission", err)
			}
			current, err := p.Store.Get(account.ID)
			if err != nil {
				t.Fatal(err)
			}
			status, err := p.accountDrain(t.Context(), account.ID, drainTestRequest(current))
			if err != nil || status.Status != "draining" || status.InFlight != 1 {
				t.Fatal("true drain failed to count previously allowed lease", status, err)
			}
			if fresh, ok, err := peer.acquireGatewayAccountContext(t.Context(), current); fresh != nil || ok || !errors.Is(err, errAccountDraining) {
				t.Fatal("real drain no longer fences subsequent admission", err)
			}
			if lease.Context.Err() != nil {
				t.Fatal("management edit or drain canceled existing lease")
			}
		})
	}
}
