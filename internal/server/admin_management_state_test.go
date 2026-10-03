package server

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

func testManagementState(t *testing.T, s managementStateStore) {
	t.Helper()
	ctx := context.Background()
	write := func(e *managementStateEntry) error {
		e.Payload = json.RawMessage(`{"claim":false,"secret":"synthetic-only"}`)
		return nil
	}
	if err := s.Update(ctx, "state-test", "one", write); err != nil {
		t.Fatal(err)
	}
	e, err := s.Get(ctx, "state-test", "one")
	if err != nil || e.Version != 1 {
		t.Fatalf("initial: %v %#v", err, e)
	}
	// A claim is serialized across independent store instances, including the
	// two-read race that a plain read-then-write OAuth callback would permit.
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.Update(ctx, "state-test", "one", func(e *managementStateEntry) error {
				var body map[string]any
				if err := json.Unmarshal(e.Payload, &body); err != nil {
					return err
				}
				if body["claim"] == true {
					return errVersionConflict
				}
				body["claim"] = true
				e.Payload, _ = json.Marshal(body)
				return nil
			})
			if err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			} else if !errors.Is(err, errVersionConflict) {
				t.Errorf("claim: %v", err)
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("claim winners: %d", won)
	}
	if err = s.Delete(ctx, "state-test", "one", 1); !errors.Is(err, errVersionConflict) {
		t.Fatalf("stale delete: %v", err)
	}
	list, err := s.List(ctx, "state-test")
	if err != nil || len(list) != 1 || list["one"].Version != 2 {
		t.Fatalf("list: %v %#v", err, list)
	}
	if err = s.Update(ctx, "state-test", "expired", func(e *managementStateEntry) error {
		e.ExpiresAt = time.Now().Add(-time.Minute).Unix()
		return write(e)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(ctx, "state-test", "expired"); !errors.Is(err, errNotFound) {
		t.Fatalf("expiry: %v", err)
	}
	list, err = s.List(ctx, "state-test")
	if err != nil || len(list) != 1 {
		t.Fatalf("expired list: %v", err)
	}
	if err = s.Delete(ctx, "state-test", "one", 2); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(ctx, "state-test", "one"); !errors.Is(err, errNotFound) {
		t.Fatalf("deleted: %v", err)
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err = s.Update(cancelCtx, "state-test", "canceled", write); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if err = s.Update(ctx, "../outside", "one", write); !errors.Is(err, errInvalid) {
		t.Fatalf("namespace: %v", err)
	}
}

func TestManagementStateFile(t *testing.T) {
	testManagementState(t, managementStateStore{directory: t.TempDir()})
}
func TestPGManagementState(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	testManagementState(t, p.managementState())
}
