package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestManagedEphemeralPruneBacklogAndFailureRetry(t *testing.T) {
	for _, tc := range []struct {
		more bool
		err  error
		want time.Duration
	}{{false, nil, 5 * time.Minute}, {true, nil, 2 * time.Second}, {false, context.DeadlineExceeded, 2 * time.Second}} {
		if got := managedEphemeralPruneDelay(tc.more, tc.err); got != tc.want {
			t.Fatalf("cleanup backlog/retry delayed: got %s want %s", got, tc.want)
		}
	}
}

func TestPGManagedEphemeralPruneBoundedBacklogAndActiveRetention(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	db := p.Store.DB
	ctx := t.Context()
	// A statement trigger verifies actual mutation size, not the SQL spelling.
	// It also proves each batch has independently committed before the next.
	_, err := db.Pool.Exec(ctx, `CREATE TABLE prune_batch_observations(rows integer NOT NULL,tx text NOT NULL);
 CREATE FUNCTION observe_prune_batch() RETURNS trigger LANGUAGE plpgsql AS $$
 DECLARE n integer; BEGIN
 SELECT count(*) INTO n FROM pruned_targets;
 IF n>1000 THEN RAISE EXCEPTION 'unbounded target cleanup'; END IF;
 IF n>0 THEN INSERT INTO prune_batch_observations VALUES(n,pg_current_xact_id()::text); END IF;
 RETURN NULL; END $$;
 CREATE TRIGGER observe_prune AFTER DELETE ON management_cleanup_targets
 REFERENCING OLD TABLE AS pruned_targets FOR EACH STATEMENT EXECUTE FUNCTION observe_prune_batch();
 INSERT INTO management_cleanup_targets(preview_id,log_id,expires_at)
 SELECT 'usage_expired',i,to_timestamp(1700000000) FROM generate_series(1,10003)i;
 INSERT INTO management_cleanup_targets(preview_id,log_id,expires_at)
 SELECT 'preview_finished',i,NULL FROM generate_series(1,2700)i;
 INSERT INTO management_cleanup_targets(preview_id,log_id,expires_at)
 SELECT name,i,NULL FROM unnest(ARRAY['preview_pending','preview_running']) name CROSS JOIN generate_series(1,11)i;
 INSERT INTO management_cleanup_targets(preview_id,log_id,expires_at)
 SELECT 'usage_future',i,now()+interval '1 day' FROM generate_series(1,7)i`)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ id, status string }{{"cleanup_preview_finished", "canceled"}, {"cleanup_preview_pending", "pending"}, {"cleanup_preview_running", "running"}} {
		if err := p.managementState().Update(ctx, "cleanup-jobs", item.id, func(entry *managementStateEntry) error {
			entry.Payload, _ = json.Marshal(managedCleanupJob{ID: item.id, Status: item.status})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	more, err := p.pruneManagementEphemeralPass(ctx)
	if err != nil || !more {
		t.Fatalf("first bounded pass must request prompt continuation: more=%t err=%v", more, err)
	}
	var expired, terminal, active, future int
	check := func() {
		t.Helper()
		if err := db.Pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE preview_id='usage_expired'),
 count(*) FILTER(WHERE preview_id='preview_finished'),count(*) FILTER(WHERE preview_id IN('preview_pending','preview_running')),
 count(*) FILTER(WHERE preview_id='usage_future') FROM management_cleanup_targets`).Scan(&expired, &terminal, &active, &future); err != nil {
			t.Fatal(err)
		}
	}
	check()
	if expired != 2003 || terminal != 0 || active != 22 || future != 7 {
		t.Fatalf("first pass incorrect retention: expired=%d terminal=%d active=%d future=%d", expired, terminal, active, future)
	}
	more, err = p.pruneManagementEphemeralPass(ctx)
	if err != nil || more {
		t.Fatalf("second pass should exhaust backlog: more=%t err=%v", more, err)
	}
	check()
	if expired != 0 || terminal != 0 || active != 22 || future != 7 {
		t.Fatalf("cleanup touched retained targets: expired=%d terminal=%d active=%d future=%d", expired, terminal, active, future)
	}
	var maximum, transactions, batches, deleted int
	if err := db.Pool.QueryRow(ctx, "SELECT max(rows),count(DISTINCT tx),count(*),sum(rows) FROM prune_batch_observations").Scan(&maximum, &transactions, &batches, &deleted); err != nil || maximum > 1000 || transactions != batches || deleted != 12703 {
		t.Fatalf("cleanup is not bounded independent transactions: max=%d tx=%d batches=%d deleted=%d err=%v", maximum, transactions, batches, deleted, err)
	}
}

func TestPGManagedEphemeralPruneSkipsInFlightRetentionLocks(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	db := p.Store.DB
	ctx := t.Context()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO management_cleanup_targets VALUES
 ('preview_retaining',1,to_timestamp(1700000000)),('preview_retaining',2,to_timestamp(1700000000)),
 ('usage_other',1,to_timestamp(1700000001)),('usage_other',2,to_timestamp(1700000001))`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `UPDATE management_cleanup_targets SET expires_at=NULL WHERE preview_id='preview_retaining'`); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	removed, err := p.pruneManagedTargetBatch(ctx, time.Now(), nil)
	if err != nil || removed != 2 || time.Since(started) > time.Second {
		t.Fatalf("retention locks blocked unrelated TTL cleanup: removed=%d elapsed=%s err=%v", removed, time.Since(started), err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	more, err := p.pruneManagementEphemeralPass(ctx)
	if err != nil || more {
		t.Fatalf("retained active target treated as expired: more=%t err=%v", more, err)
	}
	var retained int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM management_cleanup_targets WHERE preview_id='preview_retaining' AND expires_at IS NULL`).Scan(&retained); err != nil || retained != 2 {
		t.Fatalf("in-flight job retention lost targets: %d %v", retained, err)
	}
}

func TestPGManagedEphemeralPruneTimeoutAndCommittedBatchRecovery(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	db, ctx := p.Store.DB, t.Context()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO management_cleanup_targets SELECT 'usage_timeout',i,to_timestamp(1700000000) FROM generate_series(1,2500)i;
 CREATE FUNCTION delay_second_prune_batch() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF OLD.log_id=1001 THEN PERFORM pg_sleep(5); END IF; RETURN OLD; END $$;
 CREATE TRIGGER delay_second_prune BEFORE DELETE ON management_cleanup_targets FOR EACH ROW EXECUTE FUNCTION delay_second_prune_batch()`); err != nil {
		t.Fatal(err)
	}
	ctxCanceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.pruneManagementEphemeralPass(ctxCanceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled prune lost cancellation: %v", err)
	}
	more, err := p.pruneManagementEphemeralPass(ctx)
	if !more || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow batch must stop within its local deadline: more=%t err=%v", more, err)
	}
	var remaining, lowest int
	if err := db.Pool.QueryRow(ctx, "SELECT count(*),min(log_id) FROM management_cleanup_targets WHERE preview_id='usage_timeout'").Scan(&remaining, &lowest); err != nil || remaining != 1500 || lowest != 1001 {
		t.Fatalf("timed out batch lost atomicity or committed progress: remaining=%d lowest=%d err=%v", remaining, lowest, err)
	}
	if _, err := db.Pool.Exec(ctx, "DROP TRIGGER delay_second_prune ON management_cleanup_targets"); err != nil {
		t.Fatal(err)
	}
	more, err = p.pruneManagementEphemeralPass(ctx)
	if err != nil || more {
		t.Fatalf("next bounded pass failed to resume after timeout: more=%t err=%v", more, err)
	}
	if err := db.Pool.QueryRow(ctx, "SELECT count(*) FROM management_cleanup_targets").Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("backlog survived recovery: %d %v", remaining, err)
	}
}
