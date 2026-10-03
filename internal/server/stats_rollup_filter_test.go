package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func assertFilteredRollupParity(t *testing.T, m *Metrics, filters ...string) {
	t.Helper()
	for _, filter := range filters {
		q := managedQuery(t, filter)
		got, err := m.managedLogStats(t.Context(), q)
		if err != nil {
			t.Fatalf("filtered rollup %q: %v", filter, err)
		}
		q.statsQueryer = m.db.Pool
		want, err := m.managedLogStats(t.Context(), q)
		if err != nil {
			t.Fatalf("raw %q: %v", filter, err)
		}
		got.CheckedAt, want.CheckedAt = 0, 0
		a, _ := json.Marshal(got)
		b, _ := json.Marshal(want)
		if string(a) != string(b) {
			t.Fatalf("filter %q\ngot %s\nwant %s", filter, a, b)
		}
	}
}

func TestPGManagedUsageConstantFilterParity(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	const base int64 = 1710028800
	filters := []string{"model=missing", "endpoint=responses", "model=main", "model=main&status=failed", "model=other&status=ok", "endpoint=compact", "model=main&endpoint=responses"}
	for _, step := range []int64{60, 300, 3600, 86400, 604800, 2592000} {
		for _, zone := range []string{"UTC", "America%2FNew_York", "Asia%2FKathmandu"} {
			filters = append(filters, fmt.Sprintf("model=main&from=%d&to=%d&step=%d&timezone=%s", base+1, base+2*86400+17, step, zone))
		}
	}
	assertFilteredRollupParity(t, m, filters...)
	for i := 0; i < 8; i++ {
		r := managedPerformanceRecord(i)
		r.EventID, r.Model, r.Time = fmt.Sprintf("constant-%d", i), "main", base+int64(i)*21901
		r.Endpoint = "responses"
		if i%2 == 0 {
			r.Status = "error"
		}
		m.Record(r)
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertFilteredRollupParity(t, m, filters...)
	// The proof becomes mixed after a conflicting model/endpoint is committed;
	// all other dimensions must then retain the exact raw-filtered result.
	m.Record(RequestRecord{EventID: "mixed", Time: base + 41, Status: "ok", Model: "other", Endpoint: "compact", InputTokens: 712})
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertFilteredRollupParity(t, m, filters...)
	if _, err := db.Pool.Exec(t.Context(), `UPDATE request_logs SET created_at=created_at+interval '0.75 second',record=record||jsonb_build_object('groups',jsonb_build_array($1::text)) WHERE event_id='mixed'`, strings.Repeat("long", 500)); err != nil {
		t.Fatal(err)
	}
	assertFilteredRollupParity(t, m, filters...)
	if _, err := db.Pool.Exec(t.Context(), "DELETE FROM request_logs WHERE event_id='mixed'"); err != nil {
		t.Fatal(err)
	}
	assertFilteredRollupParity(t, m, filters...)
	// Missing endpoint/model names are real values, so an unnamed row prevents
	// a false proof that every request has the selected nonempty dimension.
	m.Record(RequestRecord{EventID: "unnamed", Time: base + 5, Status: "ok", InputTokens: 991})
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertFilteredRollupParity(t, m, filters...)
}

func TestPGManagedUsageConstantKeyIdentityParity(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	filters := []string{"key_id=name%3Areal", "key_id=missing", "key_id=name%3Areal&status=ok", "key_id=name%3Areal&from=1699999981&to=1700000100&step=60", "key_id=name%3Areal&model=other"}
	for i := 0; i < 4; i++ {
		m.Record(RequestRecord{EventID: fmt.Sprintf("key-constant-%d", i), Time: 1700000000 + int64(i), KeyID: "name:real", KeyName: "name:real", Status: "ok", InputTokens: int64(i)})
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertFilteredRollupParity(t, m, filters...)
	// An empty ID with the same label must force the mixed branch; it is not
	// the real key ID even if the legacy bucket and real ID share a prefix.
	m.Record(RequestRecord{EventID: "key-legacy", Time: 1700000005, KeyName: "name:real", Status: "ok", InputTokens: 991})
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertFilteredRollupParity(t, m, filters...)
	if _, err := db.Pool.Exec(t.Context(), "DELETE FROM request_logs WHERE event_id='key-legacy'"); err != nil {
		t.Fatal(err)
	}
	assertFilteredRollupParity(t, m, filters...)
}

func TestPGManagedUsageConstantGroupMembershipParity(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	filters := []string{"group_id=shared", "group_id=missing", "group_id=shared&status=ok", "group_id=shared&from=1699999981&to=1700000100&step=60", "group_id=shared&model=other"}
	assertFilteredRollupParity(t, m, filters...)
	for i := 0; i < 4; i++ {
		m.Record(RequestRecord{EventID: fmt.Sprintf("group-constant-%d", i), Time: 1700000000 + int64(i), Groups: []string{"shared", "shared", fmt.Sprintf("extra-%d", i)}, Status: "ok", InputTokens: int64(i + 3)})
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertFilteredRollupParity(t, m, filters...)
	m.Record(RequestRecord{EventID: "group-nonmember", Time: 1700000005, Groups: []string{"unrelated"}, Status: "ok", InputTokens: 991})
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertFilteredRollupParity(t, m, filters...)
	if _, err := db.Pool.Exec(t.Context(), "DELETE FROM request_logs WHERE event_id='group-nonmember'"); err != nil {
		t.Fatal(err)
	}
	assertFilteredRollupParity(t, m, filters...)
	for _, statement := range []string{
		`UPDATE request_logs SET record=record||'{"groups":["rewritten","rewritten"]}'::jsonb WHERE event_id='group-constant-0'`,
		`UPDATE request_logs SET record=record-'groups' WHERE event_id='group-constant-1'`,
		`UPDATE request_logs SET record=record||'{"groups":["shared","shared"]}'::jsonb WHERE event_id IN ('group-constant-0','group-constant-1')`,
	} {
		if _, err := db.Pool.Exec(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
		assertFilteredRollupParity(t, m, filters...)
	}
}

func TestPGManagedUsageConstantFilterSnapshotAndGate(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	const base int64 = 1700000000
	m.Record(RequestRecord{EventID: "before-gate", Time: base, Model: "constant", Endpoint: "responses", Status: "ok", InputTokens: 11})
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	q := managedQuery(t, "model=constant&timezone=UTC")
	guard, args, ok := q.rollupFilterGateSQL()
	if !ok {
		t.Fatal("constant query did not expose its gate")
	}
	var matched, different bool
	if err := db.Pool.QueryRow(t.Context(), guard, args...).Scan(&matched, &different); err != nil || !matched || different {
		t.Fatalf("constant gate: %v %v %v", matched, different, err)
	}
	// Capture a repeatable statement source, commit a conflicting writer, then
	// execute the same combined SQL inside that snapshot. This checks that the
	// gate and aggregate derive from MVCC data, not process/TTL state.
	tx, err := db.Pool.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := tx.QueryRow(t.Context(), guard, args...).Scan(&matched, &different); err != nil {
		t.Fatal(err)
	}
	m.Record(RequestRecord{EventID: "after-gate", Time: base + 1, Model: "other", Status: "ok", InputTokens: 999})
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(t.Context(), guard, args...).Scan(&matched, &different); err != nil || !matched || different {
		t.Fatalf("snapshot gate changed across concurrent commit: %v %v %v", matched, different, err)
	}
	if err := db.Pool.QueryRow(t.Context(), guard, args...).Scan(&matched, &different); err != nil || !matched || !different {
		t.Fatalf("new snapshot missed committed conflict: %v %v %v", matched, different, err)
	}
	assertFilteredRollupParity(t, m, "model=constant&timezone=UTC")
	for _, filter := range []string{"model=constant&account_id=a", "model=constant&request_id=x", "model=constant&as_of=usage_test", "model=constant&upstream_model=x"} {
		if _, _, ok := managedQuery(t, filter).rollupFilterGateSQL(); ok {
			t.Fatalf("joint filter received a marginal proof: %s", filter)
		}
	}
}

func TestPGManagedUsageConjunctiveFilterProof(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	filters := []string{
		"model=main&key_id=name%3Areal", "model=main&endpoint=responses&key_id=name%3Areal&group_id=shared",
		"model=main&endpoint=compact", "model=other&key_id=name%3Areal", "key_id=other&group_id=shared",
		"model=missing&key_id=name%3Areal", "model=main&group_id=missing", "key_id=missing&endpoint=responses",
		"model=main&endpoint=responses&key_id=name%3Areal&group_id=shared&status=ok&from=1699999999&to=1700000100&step=60",
	}
	assertFilteredRollupParity(t, m, filters...)
	m.Record(RequestRecord{EventID: "conjunction-all", Time: 1700000000, Model: "main", Endpoint: "responses", KeyID: "name:real", KeyName: "Label", Groups: []string{"shared", "shared", "extra"}, Status: "ok", InputTokens: 11})
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertFilteredRollupParity(t, m, filters...)
	q := managedQuery(t, filters[1])
	guard, args, ok := q.rollupFilterGateSQL()
	if !ok {
		t.Fatal("four supported conjuncts not eligible")
	}
	var matched, different bool
	if err := db.Pool.QueryRow(t.Context(), guard, args...).Scan(&matched, &different); err != nil || !matched || different {
		t.Fatalf("universal conjunction was not proven: %v %v %v", matched, different, err)
	}
	// Each marginal below exists, but the main+compact intersection is empty.
	// It MUST use exact raw filtering, rather than return either marginal.
	m.Record(RequestRecord{EventID: "conjunction-other", Time: 1700000001, Model: "other", Endpoint: "compact", KeyID: "other", Groups: []string{"unrelated"}, Status: "ok", InputTokens: 999})
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	q = managedQuery(t, "model=main&endpoint=compact")
	guard, args, ok = q.rollupFilterGateSQL()
	if !ok {
		t.Fatal("mixed conjunction not eligible for a conservative guard")
	}
	if err := db.Pool.QueryRow(t.Context(), guard, args...).Scan(&matched, &different); err != nil || !matched || !different {
		t.Fatalf("partial disjoint marginals were mistaken for all/absent: %v %v %v", matched, different, err)
	}
	stats, err := m.managedLogStats(t.Context(), q)
	if err != nil || stats.Requests != 0 {
		t.Fatalf("disjoint intersection produced records: %d %v", stats.Requests, err)
	}
	assertFilteredRollupParity(t, m, filters...)
	// Missing identities/names and groups also falsify universal coverage.
	m.Record(RequestRecord{EventID: "conjunction-unknown", Time: 1700000002, Status: "ok", InputTokens: 712})
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertFilteredRollupParity(t, m, filters...)
	if _, err := db.Pool.Exec(t.Context(), "DELETE FROM request_logs WHERE event_id IN ('conjunction-other','conjunction-unknown')"); err != nil {
		t.Fatal(err)
	}
	assertFilteredRollupParity(t, m, filters...)
}
