package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPGAccountDeletionPreservesRetainedHistoryWithoutRollupWrites(t *testing.T) {
	for _, compatibility := range []bool{false, true} {
		t.Run(fmt.Sprintf("compatibility_%t", compatibility), func(t *testing.T) {
			p, _ := adminPGTestProxy(t)
			a := managedTestAccount(t, p, "delete-linked-account", nil)
			const at int64 = 1800000000
			records := []RequestRecord{
				{EventID: "delete-linked-ok", Time: at, AccountID: a.ID, Account: a.Name, Model: "fixture", Status: "ok", InputTokens: 7},
				{EventID: "delete-linked-error", Time: at + 1, AccountID: a.ID, Account: a.Name, Model: "fixture", Status: "error", InputTokens: 9},
			}
			if _, _, err := p.Metrics.ImportRecords(t.Context(), records); err != nil {
				t.Fatal(err)
			}
			// Deletion must not touch immutable request observations or their
			// aggregates, regardless of how much history the account owns.
			tables := []string{"request_logs", "retained_log_minute", "retained_usage_minute", "retained_usage_daily", "retained_usage_unsafe_minute", "retained_usage_model_minute", "retained_usage_model_daily", "retained_usage_model_unsafe_minute", "retained_operations"}
			if _, err := p.Store.DB.Pool.Exec(t.Context(), `CREATE FUNCTION reject_deletion_rollup_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'unexpected retained aggregate write'; END $$`); err != nil {
				t.Fatal(err)
			}
			for _, table := range tables {
				if _, err := p.Store.DB.Pool.Exec(t.Context(), "CREATE TRIGGER reject_deletion_rollup_write BEFORE INSERT OR UPDATE OR DELETE ON "+table+" FOR EACH ROW EXECUTE FUNCTION reject_deletion_rollup_write()"); err != nil {
					t.Fatal(err)
				}
			}
			remove := func(version int64) error {
				if compatibility {
					return p.compatDeleteAccount(t.Context(), a.ID, version)
				}
				return p.Store.DeleteVersion(a.ID, version)
			}
			if err := remove(a.Version + 1); !errors.Is(err, errVersionConflict) {
				t.Fatalf("stale delete lost CAS protection: %v", err)
			}
			if _, err := p.Store.DB.Pool.Exec(t.Context(), `CREATE TRIGGER reject_deletion_audit BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_deletion_rollup_write()`); err != nil {
				t.Fatal(err)
			}
			if err := remove(a.Version); err == nil {
				t.Fatal("delete ignored audit failure")
			}
			var credentials int
			if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM account_credentials WHERE account_id=$1", a.ID).Scan(&credentials); err != nil || credentials != 1 {
				t.Fatalf("failed deletion did not roll back credential cascade: %d %v", credentials, err)
			}
			if _, err := p.Store.DB.Pool.Exec(t.Context(), "DROP TRIGGER reject_deletion_audit ON audit_logs"); err != nil {
				t.Fatal(err)
			}
			if err := remove(a.Version); err != nil {
				t.Fatalf("account deletion rewrote unchanged historical aggregates: %v", err)
			}
			var retained, historical, durable int
			if err := p.Store.DB.Pool.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER (WHERE account_id=$1 AND account_ref=$1 AND record->>'account_id'=$1) FROM request_logs`, a.ID).Scan(&retained, &historical); err != nil {
				t.Fatal(err)
			}
			if err := p.Store.DB.Pool.QueryRow(t.Context(), `SELECT count(*) FROM usage_events WHERE account_ref=$1`, a.ID).Scan(&durable); err != nil {
				t.Fatal(err)
			}
			if retained != 2 || historical != 2 || durable != 2 {
				t.Fatalf("deletion changed historical observations: retained=%d historical=%d durable=%d", retained, historical, durable)
			}
			// A real repair must still enter the original maintenance body.
			if _, err := p.Store.DB.Pool.Exec(t.Context(), `UPDATE request_logs SET status='error',record=jsonb_set(record,'{status}','"error"'::jsonb) WHERE event_id='delete-linked-ok'`); err == nil {
				t.Fatal("real observation mutation skipped aggregate maintenance")
			}
			for _, table := range tables {
				if _, err := p.Store.DB.Pool.Exec(t.Context(), "DROP TRIGGER reject_deletion_rollup_write ON "+table); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := p.Store.DB.Pool.Exec(t.Context(), `UPDATE request_logs SET status='error',record=jsonb_set(record,'{status}','"error"'::jsonb) WHERE event_id='delete-linked-ok'`); err != nil {
				t.Fatal(err)
			}
			var requests, ok, failures int64
			if err := p.Store.DB.Pool.QueryRow(t.Context(), `SELECT sum(requests)::bigint,sum(ok)::bigint,sum(errors)::bigint FROM retained_log_minute`).Scan(&requests, &ok, &failures); err != nil || requests != 2 || ok != 0 || failures != 2 {
				t.Fatalf("repair failed to maintain retained summary: requests=%d ok=%d errors=%d err=%v", requests, ok, failures, err)
			}
		})
	}
}

func TestPGAccountDeletionConcurrentHistoricalInsert(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	group, err := p.Groups.Create("delete-race-group", "")
	if err != nil {
		t.Fatal(err)
	}
	a := managedTestAccount(t, p, "delete-concurrent-history", func(a *Account) { a.Groups = []string{group.ID} })
	writer, err := p.Store.DB.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(context.Background())
	// Represent an in-flight request that observed the account before deletion.
	// Its historical insert must neither lock the account nor roll back merely
	// because management deletes that account before the request finishes.
	if _, err := writer.Exec(t.Context(), `WITH event AS (
 INSERT INTO usage_events(event_id,created_at,account_ref,account_name,status)
 VALUES('delete-concurrent',to_timestamp(1800000000),$1,$2,'ok') RETURNING event_id)
 INSERT INTO request_logs(event_id,account_id,account_ref,account_name,created_at,status,model,key_name,record)
 SELECT event_id,$1,$1,$2,to_timestamp(1800000000),'ok','fixture','',
 jsonb_build_object('event_id',event_id,'time',1800000000,'account_id',$1::text,'account',$2::text,'status','ok','model','fixture') FROM event`, a.ID, a.Name); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() { deleted <- p.Store.DeleteVersion(a.ID, a.Version) }()
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("account deletion waited for unrelated historical request insert")
	}
	if err := writer.Commit(t.Context()); err != nil {
		t.Fatal("in-flight history could not commit after deletion:", err)
	}
	var history, credentials, bindings, live int
	if err := p.Store.DB.Pool.QueryRow(t.Context(), `SELECT
 (SELECT count(*) FROM request_logs WHERE account_id=$1 AND account_ref=$1),
 (SELECT count(*) FROM account_credentials WHERE account_id=$1),
 (SELECT count(*) FROM account_groups WHERE account_id=$1),
 (SELECT count(*) FROM request_logs l JOIN accounts a ON a.id=l.account_id WHERE l.account_ref=$1)`, a.ID).Scan(&history, &credentials, &bindings, &live); err != nil {
		t.Fatal(err)
	}
	if history != 1 || credentials != 0 || bindings != 0 || live != 0 {
		t.Fatalf("history/live relation separation failed: history=%d credentials=%d bindings=%d live=%d", history, credentials, bindings, live)
	}
	record, err := p.Metrics.managedLogDetail(t.Context(), "delete-concurrent")
	if err != nil || record.AccountID != a.ID || record.Account != a.Name {
		t.Fatalf("public history identity changed after deletion: %+v %v", record, err)
	}
}

func TestPGAccountDeletionWideHistoryPerformance(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("explicit isolated performance fixture required")
	}
	p, _ := adminPGTestProxy(t)
	a := managedTestAccount(t, p, "delete-wide-history", nil)
	const count = 20000
	// Deterministic high-entropy metadata forces a TOASTed record instead of a
	// tiny synthetic JSON object that understates foreign-key deletion cost.
	var metadata strings.Builder
	for i := 0; i < 256; i++ {
		digest := sha256.Sum256([]byte(fmt.Sprintf("delete-wide-history-%d", i)))
		metadata.WriteString(base64.RawStdEncoding.EncodeToString(digest[:]))
	}
	seedStarted := time.Now()
	seed, err := p.Store.DB.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Rollback(context.Background())
	// Fixture construction is outside the deletion timing. The independent
	// regression above checks aggregate parity. This isolated scale fixture
	// keeps the current schema and indexes, while avoiding an unrelated wide
	// aggregate backfill before the operation under measurement.
	if _, err := seed.Exec(t.Context(), "SET LOCAL statement_timeout='5min'; ALTER TABLE request_logs DISABLE TRIGGER USER"); err != nil {
		t.Fatal(err)
	}
	for start := 0; start < count; start += 1000 {
		_, err := seed.Exec(t.Context(), `WITH events AS (
			INSERT INTO usage_events(event_id,created_at,account_ref,account_name,status)
			SELECT 'wide-delete-'||g,to_timestamp(1800000000),$1,$2,'ok' FROM generate_series($4::integer,$4::integer+999) g
			RETURNING event_id,created_at,account_ref,account_name,status
		) INSERT INTO request_logs(event_id,account_id,account_ref,account_name,created_at,status,model,key_name,record)
		SELECT event_id,$1,account_ref,account_name,created_at,status,'fixture','',
		jsonb_build_object('event_id',event_id,'time',1800000000,'account_id',$1::text,'account',$2::text,'model','fixture','status','ok','user_agent',$3::text)
		FROM events`, a.ID, a.Name, metadata.String(), start)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := seed.Exec(t.Context(), "ALTER TABLE request_logs ENABLE TRIGGER USER"); err != nil {
		t.Fatal(err)
	}
	// Only historical association statistics belong to this fixture. ANALYZE without a
	// column list also evaluates every wide JSON expression index, unrelated
	// to the deletion path and outside the interactive statement budget.
	if _, err := seed.Exec(t.Context(), "ANALYZE request_logs(account_id)"); err != nil {
		t.Fatal(err)
	}
	if err := seed.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT pg_total_relation_size('request_logs')").Scan(&before); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = p.Store.DeleteVersion(a.ID, a.Version)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("wide-history deletion exceeded request budget or failed after %s: %v", elapsed, err)
	}
	var retained, historical int
	if err := p.Store.DB.Pool.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER(WHERE account_id=$1 AND account_ref=$1) FROM request_logs`, a.ID).Scan(&retained, &historical); err != nil {
		t.Fatal(err)
	}
	if retained != count || historical != count {
		t.Fatalf("wide-history deletion changed retained data: %d/%d", retained, historical)
	}
	t.Logf("ACCOUNT_DELETE_WIDE rows=%d metadata_bytes=%d relation_bytes=%d seed_seconds=%.3f delete_ms=%.3f retained=%d historical=%d", count, metadata.Len(), before, started.Sub(seedStarted).Seconds(), float64(elapsed.Microseconds())/1000, retained, historical)
	if elapsed >= time.Second {
		t.Fatalf("wide-history deletion exceeds the one-second management target: %s", elapsed)
	}
}
