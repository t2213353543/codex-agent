package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestBCommittedRenameKeepsMemoryAndDiskAligned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "target.json")
	old := target{Revision: 1, Generation: "old", Address: "127.0.0.1:18890", Held: true}
	next := target{Revision: 2, Generation: "new", Address: "127.0.0.1:18891", Held: false}
	oldBytes, _ := json.Marshal(old)
	if err := os.WriteFile(path, oldBytes, 0600); err != nil {
		t.Fatal(err)
	}
	syncFailure := errors.New("synthetic directory sync failure")
	p, _ := newIngress(old, func(value target) error {
		return persistWithDirectorySync(path, value, func(string) error { return syncFailure })
	})
	gate := p.gate
	body, _ := json.Marshal(map[string]any{"expected_revision": 1, "target": next})
	response := httptest.NewRecorder()
	p.admin(response, httptest.NewRequest(http.MethodPost, "/switch", bytes.NewReader(body)))
	if response.Code != 503 {
		t.Fatalf("status%d", response.Code)
	}
	var result struct {
		Committed bool `json:"committed"`
		State     struct {
			Active  target `json:"active"`
			Durable *bool  `json:"last_update_durable"`
		} `json:"state"`
	}
	if json.Unmarshal(response.Body.Bytes(), &result) != nil || !result.Committed || result.State.Active != next || result.State.Durable == nil || *result.State.Durable {
		t.Fatal("committed uncertainty not explicit")
	}
	var stored target
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &stored) != nil || stored != next || p.state != stored {
		t.Fatal("actual rename and runtime disagree")
	}
	select {
	case <-gate:
	default:
		t.Fatal("held waiters not released for committed new target")
	}
	restarted, err := newIngress(stored, nil)
	if err != nil || restarted.state != p.state {
		t.Fatal("reload chose different target")
	}
	if err = p.update(1, target{Revision: 2, Generation: "other", Address: "127.0.0.1:18892"}); err == nil {
		t.Fatal("stale revision accepted after committed uncertainty")
	}
	// A new idempotent target revision can confirm directory durability.
	p.save = func(value target) error {
		return persistWithDirectorySync(path, value, func(string) error { return nil })
	}
	next.Revision = 3
	if err = p.update(2, next); err != nil || p.lastUpdateDurable == nil || !*p.lastUpdateDurable {
		t.Fatal("durability retry failed")
	}
	t.Log("actual file rename + injected directory-sync failure: explicit committed503, memory/disk/reload aligned, stale CAS rejected, retry confirmed")
}

func TestBPrecommitFailureKeepsOldTargetAndWaitersHeld(t *testing.T) {
	old := target{Revision: 1, Generation: "old", Address: "127.0.0.1:18890", Held: true}
	p, _ := newIngress(old, func(target) error { return errors.New("synthetic precommit failure") })
	err := p.update(1, target{Revision: 2, Generation: "new", Address: "127.0.0.1:18891"})
	if err == nil || targetCommitted(err) || p.state != old || len(p.generations) != 1 || p.lastUpdateDurable != nil {
		t.Fatal("precommit failure advanced state")
	}
	select {
	case <-p.gate:
		t.Fatal("precommit failure released held requests")
	default:
	}
}

func TestBRealPreRenameFailureIsNotMarkedCommitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "target.json")
	next := target{Revision: 2, Generation: "new", Address: "127.0.0.1:18891"}
	called := false
	err := persistWithDirectorySync(path, next, func(string) error { called = true; return nil })
	if err == nil || targetCommitted(err) || called {
		t.Fatal("pre-rename failure reported commit")
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("failed prepare created checkpoint")
	}
}
