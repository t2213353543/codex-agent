package server

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type gatewayRefreshAdvisoryQueryKey struct{}

type gatewayRefreshAdmissionTrace struct {
	queryEnded     chan error
	acquireStarted chan struct{}
	armed          atomic.Bool
}

func (*gatewayRefreshAdmissionTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, gatewayRefreshAdvisoryQueryKey{}, data.SQL == "SELECT pg_try_advisory_lock($1)")
}

func (tr *gatewayRefreshAdmissionTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if matched, _ := ctx.Value(gatewayRefreshAdvisoryQueryKey{}).(bool); matched {
		select {
		case tr.queryEnded <- data.Err:
		default:
		}
	}
}

func (tr *gatewayRefreshAdmissionTrace) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	if tr.armed.Load() {
		select {
		case tr.acquireStarted <- struct{}{}:
		default:
		}
	}
	return ctx
}

func (*gatewayRefreshAdmissionTrace) TraceAcquireEnd(context.Context, *pgxpool.Pool, pgxpool.TraceAcquireEndData) {
}

func TestPGGatewayRefreshContextCancelsAdvisoryLockWait(t *testing.T) {
	store := accountPGTestStore(t)
	before := pgImportForTest(t, store, "refresh-lock-cancellation", "synthetic-advisory-access")
	beforeAudit := gatewayRefreshAdmissionAuditCount(t, store, before.ID)
	traced, trace := gatewayRefreshAdmissionTracedStore(t, store)
	holder, err := pgx.ConnectConfig(t.Context(), store.DB.Pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal("cannot open independent advisory-lock holder")
	}
	defer holder.Close(context.Background())
	var scope string
	if err := holder.QueryRow(t.Context(), `SELECT current_database() || ':' || current_schema()`).Scan(&scope); err != nil {
		t.Fatal("cannot read advisory-lock fixture scope")
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte("codex-agent:refresh:" + scope + ":" + before.ID))
	lockID := int64(hash.Sum64())
	if _, err := holder.Exec(t.Context(), `SELECT pg_advisory_lock($1)`, lockID); err != nil {
		t.Fatal("cannot hold the exact refresh advisory lock")
	}
	locked := true
	unlock := func() {
		if !locked {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var released bool
		if err := holder.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, lockID).Scan(&released); err != nil || !released {
			t.Error("independent advisory lock could not be released")
		}
		locked = false
	}
	defer unlock()
	ctx, cancel := context.WithCancel(t.Context())
	var callbacks atomic.Int32
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, err := traced.RefreshContext(ctx, before.ID, before.AccessToken, func(current Account) (Account, error) {
			callbacks.Add(1)
			return current, errors.New("cancelled advisory waiter must not enter refresh callback")
		})
		done <- err
	}()
	defer func() {
		cancel()
		unlock()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("advisory-wait fixture did not stop during cleanup")
		}
	}()
	select {
	case queryErr := <-trace.queryEnded:
		if queryErr != nil {
			t.Fatal("advisory-lock attempt failed before entering its wait")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not attempt the externally held advisory lock")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || callbacks.Load() != 0 {
			t.Fatalf("advisory wait lost caller cancellation or entered callback: cancelled=%t callbacks=%d", errors.Is(err, context.Canceled), callbacks.Load())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refresh cancellation did not interrupt advisory-lock waiting")
	}
	gatewayRefreshAdmissionAssertUnchanged(t, store, traced, before, beforeAudit)
	unlock()
	resumeCtx, resumeCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer resumeCancel()
	updated, err := traced.RefreshContext(resumeCtx, before.ID, before.AccessToken, func(current Account) (Account, error) {
		callbacks.Add(1)
		current.AccessToken, current.RefreshToken = "synthetic-after-advisory-cancel", "synthetic-after-advisory-refresh"
		return current, nil
	})
	if err != nil || callbacks.Load() != 1 || updated.AccessToken != "synthetic-after-advisory-cancel" || updated.RefreshToken != "synthetic-after-advisory-refresh" {
		t.Fatal("a cancelled wait left a local gate or connection that prevented a later credential rotation")
	}
	stored, err := store.getAccountByID(before.ID)
	if err != nil || stored.AccessToken != updated.AccessToken || stored.RefreshToken != updated.RefreshToken || stored.CredentialVersion != before.CredentialVersion+1 {
		t.Fatal("later admitted refresh did not persist exactly one credential rotation")
	}
	gatewayRefreshAdmissionAssertReleased(t, traced, before.ID)
}

func TestPGGatewayRefreshContextCancelsInitialPoolWait(t *testing.T) {
	store := accountPGTestStore(t)
	before := pgImportForTest(t, store, "refresh-pool-cancellation", "synthetic-pool-access")
	beforeAudit := gatewayRefreshAdmissionAuditCount(t, store, before.ID)
	traced, trace := gatewayRefreshAdmissionTracedStore(t, store)
	held, err := traced.DB.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal("cannot occupy the initial account-read pool")
	}
	defer held.Release()
	trace.armed.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	var callbacks atomic.Int32
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, err := traced.RefreshContext(ctx, before.ID, before.AccessToken, func(current Account) (Account, error) {
			callbacks.Add(1)
			return current, errors.New("cancelled pool waiter must not enter refresh callback")
		})
		done <- err
	}()
	defer func() {
		cancel()
		held.Release()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("initial-pool fixture did not stop during cleanup")
		}
	}()
	select {
	case <-trace.acquireStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not reach the blocked initial account read")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || callbacks.Load() != 0 {
			t.Fatal("initial account-read cancellation entered refresh or lost caller cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not interrupt the initial account-read acquire")
	}
	if traced.DB.Pool.Stat().AcquiredConns() != 1 {
		t.Fatal("cancelled initial read altered the independent holder's connection")
	}
	held.Release()
	gatewayRefreshAdmissionAssertUnchanged(t, store, traced, before, beforeAudit)
}

func gatewayRefreshAdmissionTracedStore(t *testing.T, store Store) (Store, *gatewayRefreshAdmissionTrace) {
	t.Helper()
	trace := &gatewayRefreshAdmissionTrace{queryEnded: make(chan error, 1), acquireStarted: make(chan struct{}, 1)}
	config := store.DB.Pool.Config()
	config.MaxConns, config.MinConns = 1, 0
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal("cannot initialize the traced refresh pool")
	}
	t.Cleanup(pool.Close)
	db := *store.DB
	db.Pool = pool
	return Store{DB: &db}, trace
}

func gatewayRefreshAdmissionAuditCount(t *testing.T, store Store, id string) int64 {
	t.Helper()
	var count int64
	if err := store.DB.Pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_logs WHERE entity_id=$1`, id).Scan(&count); err != nil {
		t.Fatal("cannot inspect synthetic refresh audit count")
	}
	return count
}

func gatewayRefreshAdmissionAssertUnchanged(t *testing.T, source, traced Store, before Account, beforeAudit int64) {
	t.Helper()
	after, err := source.getAccountByID(before.ID)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Error("cancelled refresh admission changed persisted account metadata or credentials")
	}
	if gatewayRefreshAdmissionAuditCount(t, source, before.ID) != beforeAudit {
		t.Error("cancelled refresh admission persisted an account audit")
	}
	gatewayRefreshAdmissionAssertReleased(t, traced, before.ID)
}

func gatewayRefreshAdmissionAssertReleased(t *testing.T, store Store, id string) {
	t.Helper()
	config := store.DB.Pool.Config().ConnConfig
	key := fmt.Sprintf("%s:%d/%s/%s/%s", config.Host, config.Port, config.Database, config.RuntimeParams["search_path"], id)
	accountRefreshGates.Lock()
	_, retained := accountRefreshGates.entries[key]
	accountRefreshGates.Unlock()
	if retained || store.DB.Pool.Stat().AcquiredConns() != 0 {
		t.Error("refresh admission retained its local gate or database connection")
	}
}
