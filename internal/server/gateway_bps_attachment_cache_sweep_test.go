package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBPSAttachmentCacheSweepNeverReturnsExpiredReferenceBetweenSweeps(t *testing.T) {
	now := time.Now()
	oldDeadline := now.Add(-time.Nanosecond)
	cache := &bpsAttachmentCache{nextSweep: now.Add(time.Minute), entries: map[string]bpsAttachmentEntry{
		"expired":           {fileID: "file_old", expires: oldDeadline},
		"unrelated-expired": {fileID: "other_old", expires: oldDeadline},
	}}
	started := time.Now()
	var uploads atomic.Int32
	id, err := cache.file(t.Context(), "expired", func() (string, error) { uploads.Add(1); return "file_fresh", nil })
	if err != nil || id != "file_fresh" || uploads.Load() != 1 {
		t.Fatal("expired reference returned instead of uploading")
	}
	entry := cache.entries["expired"]
	if entry.expires.Before(started.Add(bpsAttachmentTTL)) || entry.expires.After(time.Now().Add(bpsAttachmentTTL)) || bpsAttachmentTTL != time.Hour {
		t.Fatal("new reference TTL changed")
	}
	if len(cache.entries) != 2 {
		t.Fatal("individual expiry lookup unexpectedly swept the whole map")
	}
	deadline, nextSweep := entry.expires, cache.nextSweep
	for range 20 {
		if got, err := cache.file(t.Context(), "expired", func() (string, error) { t.Fatal("live cache hit uploaded"); return "", nil }); err != nil || got != id {
			t.Fatal("fresh entry unavailable")
		}
	}
	if cache.entries["expired"].expires != deadline || cache.nextSweep != nextSweep {
		t.Fatal("cache hit renewed TTL or global sweep schedule")
	}
}

func TestBPSAttachmentCacheSweepReclaimsOnlyExpiredAndNoBackground(t *testing.T) {
	now := time.Now()
	cache := &bpsAttachmentCache{nextSweep: now.Add(-time.Second), entries: map[string]bpsAttachmentEntry{
		"target":     {fileID: "file_target", expires: now.Add(time.Hour)},
		"expired":    {fileID: "file_expired", expires: now.Add(-time.Second)},
		"boundary":   {fileID: "file_boundary", expires: now},
		"other-live": {fileID: "file_other", expires: now.Add(time.Hour)},
	}}
	if id, err := cache.file(t.Context(), "target", func() (string, error) { t.Fatal("target unexpectedly uploaded"); return "", nil }); err != nil || id != "file_target" {
		t.Fatal("live target lost")
	}
	if len(cache.entries) != 2 || cache.entries["other-live"].fileID != "file_other" || !cache.nextSweep.After(now) {
		t.Fatal("due sweep did not reclaim expired references while keeping valid entries")
	}
	before := cache.nextSweep
	cache.entries["later-expired"] = bpsAttachmentEntry{fileID: "unrelated_old", expires: now.Add(-time.Hour)}
	for range 20 {
		_, _ = cache.file(t.Context(), "target", func() (string, error) { return "unexpected", nil })
	}
	if cache.nextSweep != before || len(cache.entries) != 3 {
		t.Fatal("successful hit still performs a repeated whole-map sweep")
	}
	cache.nextSweep = time.Now().Add(-time.Nanosecond)
	_, _ = cache.file(t.Context(), "target", func() (string, error) { return "unexpected", nil })
	if len(cache.entries) != 2 {
		t.Fatal("next due access failed to clear unrelated expired entries")
	}
}

func TestBPSAttachmentCacheSweepConcurrentExpiredKeyStillSingleFlight(t *testing.T) {
	now := time.Now()
	cache := &bpsAttachmentCache{nextSweep: now.Add(time.Minute), entries: map[string]bpsAttachmentEntry{"same": {fileID: "expired", expires: now.Add(-time.Second)}}}
	entered, finish := make(chan struct{}), make(chan struct{})
	var uploads atomic.Int32
	upload := func() (string, error) {
		if uploads.Add(1) == 1 {
			close(entered)
		}
		<-finish
		return "file_replacement", nil
	}
	var group sync.WaitGroup
	for range 20 {
		group.Go(func() {
			id, err := cache.file(context.Background(), "same", upload)
			if err != nil || id != "file_replacement" {
				t.Error("expired-key concurrent result changed")
			}
		})
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("upload never started")
	}
	close(finish)
	group.Wait()
	if uploads.Load() != 1 || len(cache.pending) != 0 || len(cache.entries) != 1 {
		t.Fatal("individual expiry broke successful singleflight")
	}
}

func TestBPSAttachmentCacheSweepDoesNotEvictLiveEntriesOrCacheFailure(t *testing.T) {
	cache := &bpsAttachmentCache{}
	for i := 0; i < 10001; i++ {
		if _, err := cache.file(t.Context(), fmt.Sprint(i), func() (string, error) { return "file_fixture", nil }); err != nil {
			t.Fatal(err)
		}
	}
	if len(cache.entries) != 10001 {
		t.Fatal("candidate introduced a successful reference capacity limit")
	}
	failure := errors.New("synthetic rejected upload")
	_, err := cache.file(t.Context(), "failed", func() (string, error) { return "", failure })
	if err != failure || len(cache.entries) != 10001 || len(cache.pending) != 0 {
		t.Fatal("failed upload changed cache entries or remained pending")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := cache.file(ctx, "0", func() (string, error) { t.Fatal("canceled call uploaded"); return "", nil }); !errors.Is(err, context.Canceled) {
		t.Fatal("valid cache hit bypassed cancellation")
	}
}
