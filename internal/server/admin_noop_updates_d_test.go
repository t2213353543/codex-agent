package server

// Session D (2026-09-30): same-value update skips for management CRUD.
// Settings/lease/schedule PUTs that reproduce the stored state must not bump
// versions, rewrite documents or invalidate caches — mirroring the deployed
// native no-op behaviour for keys/proxies/groups.

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSettingsSameValueSetSkipsWriteAndVersionBump(t *testing.T) {
	s := NewSettingsStore(filepath.Join(t.TempDir(), "settings.json"))
	first, err := s.Get()
	if err != nil {
		t.Fatal(err)
	}
	first.RateLimitCooldownEnabled = !first.RateLimitCooldownEnabled
	if err = s.Set(first); err != nil {
		t.Fatal(err)
	}
	changed, err := s.Get()
	if err != nil {
		t.Fatal(err)
	}
	if changed.Version < 1 {
		t.Fatalf("precondition: persisted version must be positive, got %d", changed.Version)
	}
	before, err := s.Get()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if err = s.Set(before); err != nil {
		t.Fatalf("same-value Set must succeed: %v", err)
	}
	after, err := s.Get()
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version {
		t.Fatalf("same-value Set must not bump settings_version: %d -> %d", before.Version, after.Version)
	}
	if after.RateLimitCooldownEnabled != before.RateLimitCooldownEnabled {
		t.Fatal("same-value Set changed a field")
	}
	// A real change still conflicts against a stale version and bumps on the
	// current one.
	stale := after
	stale.Version--
	stale.RateLimitCooldownEnabled = !stale.RateLimitCooldownEnabled
	if err = s.Set(stale); !errors.Is(err, errVersionConflict) {
		t.Fatalf("stale version must keep conflicting: %v", err)
	}
	current := after
	current.RateLimitCooldownEnabled = !current.RateLimitCooldownEnabled
	if err = s.Set(current); err != nil {
		t.Fatal(err)
	}
	bumped, err := s.Get()
	if err != nil {
		t.Fatal(err)
	}
	if bumped.Version != after.Version+1 {
		t.Fatalf("a real change must bump the version: %d -> %d", after.Version, bumped.Version)
	}
}

func TestManagedLeaseSameValueUpdateSkipsWrite(t *testing.T) {
	p := newTestProxy(t, 1)
	_, accounts, err := p.Store.ListMetadata()
	if err != nil || len(accounts) == 0 {
		t.Fatalf("precondition account: %v %d", err, len(accounts))
	}
	id := accounts[0].ID
	expires := time.Now().Add(24 * time.Hour).Unix()
	set := func(expiresAt int64, autoDisable, restore bool) error {
		return p.updateManagedLease(t.Context(), id, "account.lease.update", func(lease *managedAccountLease, a *Account) error {
			lease.Version++
			lease.ExpiresAt, lease.AutoDisable, lease.RestoreOnExtend = expiresAt, autoDisable, restore
			lease.RestoreSkippedReason = ""
			return nil
		})
	}
	if err := set(expires, true, false); err != nil {
		t.Fatal(err)
	}
	first, leaseErr := p.getManagedLease(t.Context(), id)
	if leaseErr != nil {
		t.Fatal(leaseErr)
	}
	if first.Version < 1 {
		t.Fatalf("precondition: lease version must be positive, got %d", first.Version)
	}
	// Same payload through the PUT handler's comparison: unchanged fields with
	// the stored lease keep the version.
	next := first
	next.Version++
	if !next.unchangedFrom(first) {
		t.Fatal("identical lease must compare unchanged")
	}
	next.ExpiresAt = first.ExpiresAt + 1
	if next.unchangedFrom(first) {
		t.Fatal("a changed expiry must not compare unchanged")
	}
}

func TestManagedScheduleUnchangedComparison(t *testing.T) {
	plan := managedAccountSchedule{Kind: "usage", Cron: "0 * * * *", Timezone: "UTC", Enabled: true, MaxResults: 20, NextRunAt: 100}
	same := plan
	same.Version = plan.Version + 1
	same.LastRunAt = plan.LastRunAt + 5 // scheduler-owned, not PUT-set
	if !same.unchangedFrom(plan) {
		t.Fatal("version/last-run movement must not count as a PUT change")
	}
	different := plan
	different.Enabled = false
	if different.unchangedFrom(plan) {
		t.Fatal("an enabled flip must not compare unchanged")
	}
}
