package server

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestManagedLargeModelCursor(t *testing.T) {
	m, err := NewMetrics(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	long := strings.Repeat("<", 50000)
	m.Record(RequestRecord{Time: 1800000000, EventID: "large-a", Model: long, Status: "error"})
	m.Record(RequestRecord{Time: 1800000001, EventID: "large-b", Model: "z", Status: "error"})
	q := managedQuery(t, "sort_by=model&sort_order=asc&page_size=1")
	page, err := m.managedLogs(t.Context(), q)
	if err != nil || !page.HasMore || len(page.NextCursor) > 1024 {
		t.Fatalf("first huge page %v %v", page, err)
	}
	q.Cursor = page.NextCursor
	c, err := q.decodeCursor()
	if err != nil || c.Version != 2 {
		t.Fatalf("compact boundary %v %v", c, err)
	}
	page, err = m.managedLogs(t.Context(), q)
	if err != nil || len(page.Items) != 1 || page.Items[0].EventID != "large-b" {
		t.Fatal("large model continuation", err)
	}
	c.ModelHash = strings.Repeat("0", 64)
	if err := m.resolveManagedModelBoundary(t.Context(), &c); !errors.Is(err, errInvalid) {
		t.Fatalf("changed reference accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c.ModelHash = ""
	if err := m.resolveManagedModelBoundary(ctx, &c); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestPGManagedLargeModelCursorRemovedOrRepaired(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	r := RequestRecord{EventID: "long-boundary", Time: 1800000000, Status: "ok", Model: strings.Repeat("a", 50000)}
	m.Record(r)
	r.EventID = "long-boundary-peer"
	m.Record(r)
	m.Record(RequestRecord{EventID: "long-after", Time: 1800000001, Status: "ok", Model: "z"})
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	q := managedQuery(t, "sort_by=model&sort_order=asc&page_size=1")
	if _, err := db.Pool.Exec(t.Context(), "UPDATE request_logs SET created_at=created_at+interval '0.75 second' WHERE event_id IN ('long-boundary','long-boundary-peer')"); err != nil {
		t.Fatal(err)
	}
	page, err := m.managedLogs(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	q.Cursor = page.NextCursor
	second, err := m.managedLogs(t.Context(), q)
	if err != nil || len(second.Items) != 1 || second.Items[0].EventID != "long-boundary-peer" {
		t.Fatalf("fractional model seek lost peer: %+v %v", second, err)
	}
	if _, err := db.Pool.Exec(t.Context(), "UPDATE request_logs SET model='changed' WHERE event_id='long-boundary'"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.managedLogs(t.Context(), q); !errors.Is(err, errInvalid) {
		t.Fatalf("changed boundary accepted: %v", err)
	}
	if _, err := db.Pool.Exec(t.Context(), "DELETE FROM request_logs WHERE event_id='long-boundary'"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.managedLogs(t.Context(), q); !errors.Is(err, errInvalid) {
		t.Fatalf("removed boundary accepted: %v", err)
	}
}
