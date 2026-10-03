package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestManagedResourceReads(t *testing.T) {
	exerciseManagedResourceReads(t, webTestSetup)
}

func TestPGManagedResourceReads(t *testing.T) {
	exerciseManagedResourceReads(t, adminPGTestProxy)
}

func TestPGManagedGroupCreateUsesPersistedTimestamps(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	// Force the rounding boundary without waiting on wall time. Create must
	// return the database values even when its defaults differ from app time.
	if _, err := p.Store.DB.Pool.Exec(t.Context(), `ALTER TABLE groups
		ALTER COLUMN created_at SET DEFAULT to_timestamp(1700000000.75),
		ALTER COLUMN updated_at SET DEFAULT to_timestamp(1700000001.75)`); err != nil {
		t.Fatal(err)
	}
	created, err := p.Groups.Create("persisted-timestamps", "")
	if err != nil || created.CreatedAt != 1700000001 || created.UpdatedAt != 1700000002 || created.Version != 1 {
		t.Fatalf("create returned app-clock timestamps: %+v %v", created, err)
	}
	read, err := p.Groups.Get(created.ID)
	if err != nil || read != created {
		t.Fatalf("create/get disagree: created=%+v read=%+v err=%v", created, read, err)
	}
	listed, err := p.Groups.List()
	if err != nil || len(listed) != 1 || listed[0] != created {
		t.Fatalf("create/list disagree: created=%+v listed=%+v err=%v", created, listed, err)
	}
}

func exerciseManagedResourceReads(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	group, err := p.Groups.Create("resource-read", "description stays available for detail")
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := p.Groups.Create("resource-unrelated", "not requested")
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Groups.Get(group.ID)
	if err != nil || got != group {
		t.Fatalf("group detail changed: got=%+v want=%+v err=%v", got, group, err)
	}
	if _, err := p.Groups.Get("missing"); !errors.Is(err, errNotFound) {
		t.Fatalf("missing group: %v", err)
	}
	ctx := context.Background()
	names, err := p.Groups.managedNames(ctx, map[string]struct{}{group.ID: {}, "missing": {}})
	if err != nil || len(names) != 1 || names[group.ID] != group.Name || names[unrelated.ID] != "" {
		t.Fatalf("referenced group names: %v %v", names, err)
	}
	groups, err := p.Groups.List()
	if err != nil {
		t.Fatal(err)
	}
	count, err := p.Groups.managedCount(ctx)
	if err != nil || count != len(groups) {
		t.Fatalf("group count: got=%d want=%d err=%v", count, len(groups), err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.Groups.managedCount(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled count: %v", err)
	}
	if _, err := p.Groups.managedNames(canceled, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled names: %v", err)
	}
}

func TestPGManagedResourceReadsEmptyAndUnavailable(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	ctx := context.Background()
	if _, err := p.Store.DB.Pool.Exec(ctx, "ALTER TABLE groups RENAME TO unavailable_groups"); err != nil {
		t.Fatal(err)
	}
	if names, err := p.Groups.managedNames(ctx, nil); err != nil || len(names) != 0 {
		t.Fatalf("empty binding lookup accessed unrelated storage: %v %v", names, err)
	}
	if _, err := p.Groups.managedNames(ctx, map[string]struct{}{"wanted": {}}); err == nil {
		t.Fatal("binding lookup hid storage failure")
	}
	if _, err := p.Groups.managedCount(ctx); err == nil {
		t.Fatal("count hid storage failure")
	}
}
