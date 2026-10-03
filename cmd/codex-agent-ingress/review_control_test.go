package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBFrontControlRejectsInvalidNumericPort(t *testing.T) {
	for _, port := range []string{"0", "65536", "-1", "http"} {
		t.Run(port, func(t *testing.T) {
			candidate := target{Revision: 2, Generation: "new", Address: "127.0.0.1:" + port}
			err := validate(candidate)
			t.Logf("port=%q accepted=%t", port, err == nil)
			if err == nil {
				t.Error("switch target accepts an invalid numeric backend port")
			}
		})
	}
}

func TestBFrontControlPersistencePostCommitError(t *testing.T) {
	old := target{Revision: 1, Generation: "old", Address: "127.0.0.1:18890"}
	next := target{Revision: 2, Generation: "new", Address: "127.0.0.1:18891"}
	path := filepath.Join(t.TempDir(), "target.json")
	original, _ := json.Marshal(old)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	// Models the real persist contract: rename succeeds, then opening/syncing
	// the directory returns an error. This is not an actual filesystem outage.
	p, err := newIngress(old, func(value target) error {
		raw, _ := json.Marshal(value)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return &targetCommitError{Err: errors.New("synthetic directory durability error after rename")}
	})
	if err != nil {
		t.Fatal(err)
	}
	switchErr := p.update(old.Revision, next)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored target
	if json.Unmarshal(raw, &stored) != nil {
		t.Fatal("bad saved checkpoint")
	}
	active := p.status()["active"].(target)
	t.Logf("CONTROL_RESULT switch_returned_error=%t active=%s/%d stored=%s/%d", switchErr != nil, active.Generation, active.Revision, stored.Generation, stored.Revision)
	if switchErr == nil {
		t.Fatal("expected modeled postcommit persistence failure")
	}
	if active != stored {
		t.Error("failed switch leaves a different routing target for restart")
	}
}
