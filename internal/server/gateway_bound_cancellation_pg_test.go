package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPGGatewayBoundCancellationInterruptsAuthoritativeRead(t *testing.T) {
	for _, consumer := range []string{"http_bound_selection", "websocket_authorization"} {
		t.Run(consumer, func(t *testing.T) {
			f := newGatewaySelectionPGFixture(t, false, false)
			settings, err := f.writer.Settings.Get()
			if err != nil {
				t.Fatal("cannot read fixture settings")
			}
			settings.AdminAPI = false
			if err := f.writer.Settings.Set(settings); err != nil {
				t.Fatal("cannot isolate ordinary-key authorization")
			}
			f.reader.Settings = NewPGSettingsStore(f.writer.Store.DB)
			const session = "bound-cancel-pg-thread"
			threadKey := fmt.Sprintf("%sgateway:thread:%x", f.reader.Store.DB.Namespace, sha256.Sum256([]byte(f.key.ID+"\x00"+session)))
			t.Cleanup(func() { _ = f.writer.Store.DB.Redis.Del(context.Background(), threadKey).Err() })
			connected, release, err := f.reader.pickSessionAccount(t.Context(), f.key.Key, f.key.ID, session)
			if err != nil || release == nil || connected.ID != f.primary.ID {
				t.Fatal("cannot establish persisted bound account before pool exhaustion")
			}
			release()
			tracer := &gatewaySelectionPGAcquireTrace{started: make(chan struct{}, 1)}
			config := f.writer.Store.DB.Pool.Config()
			config.MaxConns, config.MinConns = 1, 0
			config.ConnConfig.Tracer = tracer
			pool, err := pgxpool.NewWithConfig(t.Context(), config)
			if err != nil {
				t.Fatal("cannot initialize the isolated one-slot account pool")
			}
			t.Cleanup(pool.Close)
			readerDB := *f.writer.Store.DB
			readerDB.Pool = pool
			// Key/settings documents and Redis retain their available dependencies.
			// Only the bound account reload can touch the exhausted connection pool.
			f.reader.Store.DB = &readerDB
			held, err := pool.Acquire(t.Context())
			if err != nil {
				t.Fatal("cannot hold the only authoritative account connection")
			}
			defer held.Release()
			if pool.Stat().AcquiredConns() != 1 || pool.Stat().MaxConns() != 1 {
				t.Fatal("account fixture pool is not fully occupied")
			}
			f.keepOriginalCacheWarm(t)
			beforeCanceled := pool.Stat().CanceledAcquireCount()
			tracer.armed.Store(true)
			ctx, cancel := context.WithCancel(t.Context())
			type outcome struct {
				err      error
				selected bool
				admitted bool
			}
			done := make(chan outcome, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				if consumer == "http_bound_selection" {
					account, release, err := f.reader.pickSessionAccount(ctx, f.key.Key, f.key.ID, session)
					if release != nil {
						release()
					}
					done <- outcome{err: err, selected: account.ID != "", admitted: release != nil}
					return
				}
				account, err := f.reader.authorizedGatewayWSAccountContext(ctx, f.key.Key, f.key.ID, connected)
				done <- outcome{err: err, selected: account.ID != ""}
			}()
			defer func() {
				cancel()
				held.Release()
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Error("bound lookup did not finish after releasing the test pool")
				}
			}()
			select {
			case <-tracer.started:
			case <-time.After(5 * time.Second):
				t.Fatal("bound consumer did not reach its authoritative account acquire")
			}
			cancel()
			select {
			case result := <-done:
				if !errors.Is(result.err, context.Canceled) || result.selected || result.admitted {
					t.Fatalf("bound consumer lost cancellation or granted access: cancelled=%t selected=%t admitted=%t", errors.Is(result.err, context.Canceled), result.selected, result.admitted)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("caller cancellation did not interrupt the blocked bound account read")
			}
			if pool.Stat().CanceledAcquireCount() != beforeCanceled+1 || pool.Stat().AcquiredConns() != 1 {
				t.Fatal("bound cancellation did not cancel exactly one waiting acquire")
			}
			current, err := f.writer.Store.getAccountByID(f.primary.ID)
			if err != nil || current.LastError != f.primary.LastError || current.CredentialStatus != f.primary.CredentialStatus ||
				current.CredentialVersion != f.primary.CredentialVersion || current.OwnerRevision != f.primary.OwnerRevision {
				t.Fatal("cancelled bound account read mutated credentials or failure state")
			}
			if bound, err := f.reader.sessionAccount(t.Context(), f.key.ID, session, ""); err != nil || bound != gatewayAccountRef(connected) {
				t.Fatal("cancelled bound account read changed the original Redis history binding")
			}
			if f.reader.inFlight(gatewayAdmissionKey(f.primary)) != 0 || f.transport.calls.Load() != 0 {
				t.Fatal("cancelled bound lookup retained admission or reached OAuth/upstream")
			}
		})
	}
}
