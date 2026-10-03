package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func snapshotOrderHTTP(t *testing.T, client *http.Client, method, target string, body any, output any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), method, target, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+webTestKey)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("local management HTTP=%d", response.StatusCode)
	}
	if output != nil {
		if err := json.NewDecoder(response.Body).Decode(output); err != nil {
			t.Fatal(err)
		}
	} else {
		_, _ = io.Copy(io.Discard, response.Body)
	}
}

func TestManagedSnapshotLateOldCredentialCannotReplaceNewUsage(t *testing.T) {
	p, handler := webTestSetup(t)
	account := managedTestAccount(t, p, "snapshot-order", nil)
	oldStarted, releaseOld := make(chan struct{}), make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(releaseOld) }) }
	defer unblock()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/backend-api/wham/usage" {
			t.Error("unexpected source request")
			w.WriteHeader(500)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer " + account.AccessToken:
			close(oldStarted)
			select {
			case <-releaseOld:
			case <-r.Context().Done():
				return
			}
			w.WriteHeader(401)
		case "Bearer synthetic-new-usage-token":
			_, _ = io.WriteString(w, `{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":0,"limit_window_seconds":18000}}}`)
		default:
			t.Error("unexpected synthetic credential")
			w.WriteHeader(500)
		}
	}))
	p.BaseURL = upstream.URL + "/backend-api/codex"
	server := httptest.NewServer(handler)
	defer func() { unblock(); server.Close(); upstream.Close() }()
	endpoint := server.URL + "/api/accounts/" + account.ID
	oldResult := make(chan managedUpstreamSnapshot, 1)
	go func() {
		var snapshot managedUpstreamSnapshot
		snapshotOrderHTTP(t, server.Client(), http.MethodPost, endpoint+"/upstream", map[string]any{"version": account.Version, "kind": "usage"}, &snapshot)
		oldResult <- snapshot
	}()
	select {
	case <-oldStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("old check did not reach held source request")
	}
	snapshotOrderHTTP(t, server.Client(), http.MethodPost, endpoint+"/reauthorize", map[string]any{"version": account.Version, "credentials": map[string]any{"access_token": "synthetic-new-usage-token", "account_id": account.AccountID}}, nil)
	current, err := p.Store.Get(account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.CredentialVersion <= account.CredentialVersion {
		t.Fatal("local replacement did not advance credential epoch")
	}
	var fresh managedUpstreamSnapshot
	snapshotOrderHTTP(t, server.Client(), http.MethodPost, endpoint+"/upstream", map[string]any{"version": current.Version, "kind": "usage"}, &fresh)
	if !fresh.OK || fresh.HTTPCode != 200 || fresh.CredentialVersion != current.CredentialVersion {
		t.Fatal("new check did not save actual success")
	}
	if p.cachedManagedUsage(t.Context(), current, true) == nil {
		t.Fatal("fresh successful usage was not reusable")
	}
	unblock()
	var old managedUpstreamSnapshot
	select {
	case old = <-oldResult:
	case <-time.After(3 * time.Second):
		t.Fatal("old API caller did not receive its result")
	}
	if old.OK || old.HTTPCode != 401 || old.CredentialVersion != account.CredentialVersion {
		t.Fatal("old API caller lost its own source result")
	}
	var view struct {
		Snapshots map[string]managedUpstreamSnapshot `json:"snapshots"`
	}
	snapshotOrderHTTP(t, server.Client(), http.MethodGet, endpoint+"/upstream", nil, &view)
	stored := view.Snapshots["usage"]
	if !stored.OK || stored.HTTPCode != 200 || stored.Stale || stored.CredentialVersion != current.CredentialVersion || p.cachedManagedUsage(t.Context(), current, true) == nil {
		t.Fatalf("late old credential replaced current usage: cv=%d want=%d status=%d stale=%v", stored.CredentialVersion, current.CredentialVersion, stored.HTTPCode, stored.Stale)
	}
	if calls.Load() != 2 {
		t.Fatal("management operation retried or made hidden source requests")
	}
	after, _ := p.Store.Get(account.ID)
	if after.CredentialVersion != current.CredentialVersion || after.Version != current.Version || after.AccessToken != current.AccessToken {
		t.Fatal("snapshot changed account state")
	}
	if records := p.Metrics.RecentForAccount(account.Name, 10); len(records) != 0 {
		t.Fatal("management check entered inference metrics")
	}
}

func snapshotOrderRows(t *testing.T, p *Proxy, id string) (managementStateEntry, map[string]managedUpstreamSnapshot) {
	t.Helper()
	entry, err := p.managementState().Get(t.Context(), "account-upstream", id)
	if err != nil {
		t.Fatal(err)
	}
	var rows map[string]managedUpstreamSnapshot
	if err = json.Unmarshal(entry.Payload, &rows); err != nil {
		t.Fatal(err)
	}
	return entry, rows
}

func TestManagedSnapshotOrdersEpochThenStartTimePerKind(t *testing.T) {
	for _, test := range []struct {
		name                           string
		oldCV, newCV, oldTime, newTime int64
		replace                        bool
	}{
		{"older_epoch_later_clock", 2, 1, 100, 900, false},
		{"new_epoch_earlier_clock", 1, 2, 900, 100, true},
		{"same_epoch_earlier", 2, 2, 200, 100, false},
		{"same_epoch_later", 2, 2, 100, 200, true},
		{"same_second_last_completion_compatibility", 2, 2, 100, 100, true},
		{"positive_epoch_replaces_legacy_zero", 0, 1, 900, 100, true},
		{"legacy_zero_cannot_replace_positive", 1, 0, 100, 900, false},
		{"both_legacy_use_time", 0, 0, 200, 100, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newTestProxy(t, 1)
			old := managedUpstreamSnapshot{ID: "snapshot-test", Kind: "usage", CredentialVersion: test.oldCV, Version: 9000, CheckedAt: test.oldTime, HTTPCode: 200, OK: true, Data: map[string]any{"marker": "old"}}
			other := managedUpstreamSnapshot{ID: old.ID, Kind: "models", CredentialVersion: 100, CheckedAt: 9999, Data: map[string]any{"marker": "other-kind"}}
			for _, snapshot := range []managedUpstreamSnapshot{old, other} {
				if err := p.cacheManagedUpstream(t.Context(), snapshot); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := snapshotOrderRows(t, p, old.ID)
			incoming := old
			incoming.CredentialVersion = test.newCV
			incoming.Version = 1
			incoming.CheckedAt = test.newTime
			incoming.HTTPCode = 401
			incoming.OK = false
			incoming.Data = map[string]any{"marker": "incoming"}
			if err := p.cacheManagedUpstream(t.Context(), incoming); err != nil {
				t.Fatal(err)
			}
			after, rows := snapshotOrderRows(t, p, old.ID)
			want := old
			if test.replace {
				want = incoming
			}
			if rows["usage"].CredentialVersion != want.CredentialVersion || rows["usage"].CheckedAt != want.CheckedAt || rows["usage"].HTTPCode != want.HTTPCode {
				t.Fatal("snapshot ordering used arrival, metadata version or another kind")
			}
			if rows["models"].CredentialVersion != 100 || rows["models"].CheckedAt != 9999 {
				t.Fatal("updating usage lost models snapshot")
			}
			if !test.replace && (after.Version != before.Version || !bytes.Equal(after.Payload, before.Payload)) {
				t.Fatal("ignored stale snapshot rewrote management document")
			}
		})
	}
}

func TestManagedSnapshotConcurrentKindsCannotRegress(t *testing.T) {
	p := newTestProxy(t, 1)
	start := make(chan struct{})
	var group sync.WaitGroup
	for _, kind := range []string{"usage", "models", "privacy"} {
		for version := int64(1); version <= 12; version++ {
			for checked := int64(1); checked <= 2; checked++ {
				snapshot := managedUpstreamSnapshot{ID: "concurrent-snapshots", Kind: kind, CredentialVersion: version, Version: 100 - version, CheckedAt: checked, Data: map[string]any{"marker": fmt.Sprint(version, checked)}}
				group.Add(1)
				writer := &Proxy{Store: p.Store}
				go func() {
					defer group.Done()
					<-start
					if err := writer.cacheManagedUpstream(context.Background(), snapshot); err != nil {
						t.Error(err)
					}
				}()
			}
		}
	}
	close(start)
	group.Wait()
	_, rows := snapshotOrderRows(t, p, "concurrent-snapshots")
	if len(rows) != 3 {
		t.Fatal("concurrent kind lost")
	}
	for _, row := range rows {
		if row.CredentialVersion != 12 || row.CheckedAt != 2 {
			t.Fatal("concurrent snapshot order regressed")
		}
	}
}

func TestManagedSnapshotStorageFailuresAreNotSuppressed(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		p := newTestProxy(t, 1)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := p.cacheManagedUpstream(ctx, managedUpstreamSnapshot{ID: "failure", Kind: "usage"}); !errors.Is(err, context.Canceled) {
			t.Fatal("canceled store error swallowed")
		}
	})
	t.Run("invalid_existing_shape", func(t *testing.T) {
		p := newTestProxy(t, 1)
		if err := p.managementState().Update(t.Context(), "account-upstream", "failure", func(e *managementStateEntry) error { e.Payload = []byte(`[]`); return nil }); err != nil {
			t.Fatal(err)
		}
		before, _ := p.managementState().Get(t.Context(), "account-upstream", "failure")
		if err := p.cacheManagedUpstream(t.Context(), managedUpstreamSnapshot{ID: "failure", Kind: "usage", CredentialVersion: 1}); err == nil {
			t.Fatal("invalid stored shape overwritten")
		}
		after, _ := p.managementState().Get(t.Context(), "account-upstream", "failure")
		if after.Version != before.Version || !bytes.Equal(after.Payload, before.Payload) {
			t.Fatal("failed write mutated store")
		}
	})
	t.Run("null_existing_object", func(t *testing.T) {
		p := newTestProxy(t, 1)
		if err := p.managementState().Update(t.Context(), "account-upstream", "failure", func(e *managementStateEntry) error { e.Payload = []byte(`null`); return nil }); err != nil {
			t.Fatal(err)
		}
		before, _ := p.managementState().Get(t.Context(), "account-upstream", "failure")
		if err := p.cacheManagedUpstream(t.Context(), managedUpstreamSnapshot{ID: "failure", Kind: "usage", CredentialVersion: 1}); err == nil {
			t.Fatal("null stored object accepted")
		}
		after, _ := p.managementState().Get(t.Context(), "account-upstream", "failure")
		if after.Version != before.Version || !bytes.Equal(after.Payload, before.Payload) {
			t.Fatal("invalid null state changed")
		}
	})
	t.Run("marshal_failure", func(t *testing.T) {
		p := newTestProxy(t, 1)
		if err := p.cacheManagedUpstream(t.Context(), managedUpstreamSnapshot{ID: "failure", Kind: "usage", CredentialVersion: 1, Data: make(chan int)}); err == nil {
			t.Fatal("marshal error swallowed")
		}
		if _, err := p.managementState().Get(t.Context(), "account-upstream", "failure"); !errors.Is(err, errNotFound) {
			t.Fatalf("failed marshal committed store: %v", err)
		}
	})
	t.Run("filesystem_failure", func(t *testing.T) {
		p := newTestProxy(t, 1)
		if err := os.WriteFile(filepath.Join(filepath.Dir(p.Store.Path), "management"), []byte("block directory creation"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := p.cacheManagedUpstream(t.Context(), managedUpstreamSnapshot{ID: "failure", Kind: "usage", CredentialVersion: 1}); err == nil {
			t.Fatal("filesystem error suppressed")
		}
	})
}

func TestManagedSnapshotLegacyMissingEpochMigratesPerKind(t *testing.T) {
	p := newTestProxy(t, 1)
	if err := p.managementState().Update(t.Context(), "account-upstream", "legacy", func(e *managementStateEntry) error {
		e.Payload = []byte(`{"usage":{"id":"legacy","kind":"usage","version":900,"checked_at":900,"http_code":401},"models":{"id":"legacy","kind":"models","checked_at":900,"http_code":200}}`)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := managedUpstreamSnapshot{ID: "legacy", Kind: "usage", Version: 1, CredentialVersion: 1, CheckedAt: 100, OK: true, HTTPCode: 200}
	if err := p.cacheManagedUpstream(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	_, rows := snapshotOrderRows(t, p, "legacy")
	if rows["usage"].CredentialVersion != 1 || rows["usage"].CheckedAt != 100 || !rows["usage"].OK || rows["models"].CredentialVersion != 0 || rows["models"].CheckedAt != 900 {
		t.Fatal("legacy migration mixed kinds or preferred metadata version/time over credential epoch")
	}
}
