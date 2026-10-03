package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOfficialRuntimeDoesNotRejectAtFormerProcessLimit(t *testing.T) {
	p, account := officialManagedFixture(t, false)
	rt := p.Official
	// Synthetic reservations exercise both admission checks without spawning
	// dozens of processes. Only the extra fixture RPC starts a real child.
	reserved := []*officialRPC{}
	rt.mu.Lock()
	for i := 0; i < 65; i++ {
		placeholder := &officialRPC{}
		rt.processes[placeholder] = true
		reserved = append(reserved, placeholder)
	}
	rt.mu.Unlock()
	defer func() {
		rt.mu.Lock()
		for _, placeholder := range reserved {
			delete(rt.processes, placeholder)
		}
		rt.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	rpc, err := rt.startRPC(ctx, account, nil)
	if err != nil {
		t.Fatalf("request rejected at removed process ceiling: %v", err)
	}
	rpc.close()
}

func TestOfficialRuntimeLargeEventsFilesAndTaskDeadline(t *testing.T) {
	large := strings.Repeat("x", (32<<20)+1)
	var seen int
	if err := officialReadJSONLines(strings.NewReader(large+"\nlast"), func(line []byte) error {
		if seen == 0 && string(line) != large {
			t.Fatal("large line changed")
		}
		seen++
		return nil
	}); err != nil || seen != 2 {
		t.Fatalf("read error %v seen=%d", err, seen)
	}
	broken := io.MultiReader(strings.NewReader("ok\n"), officialErrorReader{})
	if err := officialReadJSONLines(broken, func([]byte) error { return nil }); err == nil {
		t.Fatal("read error discarded")
	}
	raw, _ := json.Marshal(map[string]string{"type": "test", "body": large})
	task := &officialTask{wake: make(chan struct{}, 1), bytes: 64 << 20}
	if err := task.append(raw); err != nil {
		t.Fatalf("valid event rejected at old aggregate bound: %v", err)
	}
	rpc := &officialRPC{wake: make(chan struct{}, 1), queued: 64 << 20, cancel: func() {}}
	rpc.requeue(json.RawMessage(`{"type":"event"}`))
	if rpc.err != nil || len(rpc.queue) != 1 {
		t.Fatal("requeue rejected at former budget")
	}
	var written bytes.Buffer
	rpc.stdin = officialResponsesFixtureWriter{write: written.Write}
	if err := rpc.send(json.RawMessage(raw)); err != nil || written.Len() <= 32<<20 {
		t.Fatalf("large outgoing RPC rejected: %v", err)
	}
	rt := officialTaskFixtureRuntime(t)
	original := rt.command
	rt.command = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if _, ok := ctx.Deadline(); ok {
			t.Error("task has an artificial total deadline")
		}
		return original(ctx, name, args...)
	}
	content := strings.Repeat("f", (16<<20)+1)
	created, err := rt.createTask(context.Background(), "k", Account{ID: "a", AccessToken: "synthetic"}, nil, officialTaskInput{Prompt: strings.Repeat("p", (4<<20)+1), Files: map[string]string{"large.txt": content}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitOfficialTask(t, created)
	response := httptest.NewRecorder()
	created.serve(response, httptest.NewRequest("GET", "/files?path=large.txt", nil), "files")
	if response.Code != 200 || response.Body.String() != content {
		t.Fatal("large workspace download was rejected or truncated")
	}
}

type officialErrorReader struct{}

func (officialErrorReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestOfficialCatalogConcurrentFetchesHaveNoAdmissionCeiling(t *testing.T) {
	cache := officialCatalogCache{entries: map[[32]byte]officialCatalogEntry{}, flights: map[[32]byte]*officialCatalogFlight{}}
	for i := 0; i < 65; i++ {
		cache.flights[[32]byte{byte(i)}] = &officialCatalogFlight{}
	}
	raw, _, err := cache.get(t.Context(), [32]byte{100}, func(context.Context) ([]byte, error) { return []byte("fresh catalog"), nil })
	if err != nil || string(raw) != "fresh catalog" {
		t.Fatalf("catalog fetch rejected at former ceiling: %v", err)
	}
}

func TestOfficialRuntimeDoesNotRejectAtFormerTaskLimit(t *testing.T) {
	rt := officialTaskFixtureRuntime(t)
	rt.mu.Lock()
	for i := 0; i < 129; i++ {
		rt.tasks[fmt.Sprint(i)] = &officialTask{}
	}
	rt.mu.Unlock()
	defer func() {
		rt.mu.Lock()
		for i := 0; i < 129; i++ {
			delete(rt.tasks, fmt.Sprint(i))
		}
		rt.mu.Unlock()
	}()
	task, err := rt.createTask(context.Background(), "key", Account{ID: "account", AccessToken: "synthetic", AccountID: "workspace"}, nil, officialTaskInput{Prompt: "CAPACITY_OK"}, nil)
	if err != nil {
		t.Fatalf("request rejected at removed task ceiling: %v", err)
	}
	waitOfficialTask(t, task)
	if task.snapshot()["status"] != "completed" {
		t.Fatal("task did not complete")
	}
}

func TestOfficialRuntimeReaperKeepsIdleSessionsAndCompletedWorkspaces(t *testing.T) {
	root := t.TempDir()
	makeRPC := func(dead bool) *officialRPC {
		workspace, err := os.MkdirTemp(root, "rpc-")
		if err != nil {
			t.Fatal(err)
		}
		rpc := &officialRPC{root: workspace, cancel: func() {}, wake: make(chan struct{}, 1)}
		if dead {
			rpc.err = errors.New("exited")
		}
		return rpc
	}
	liveResponse, deadResponse, liveNative, deadNative := makeRPC(false), makeRPC(true), makeRPC(false), makeRPC(true)
	taskRoot := filepath.Join(root, "task")
	if err := os.MkdirAll(filepath.Join(taskRoot, "home"), 0700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-365 * 24 * time.Hour)
	rt := &officialRuntime{threads: map[string]*officialThread{"live": {rpc: liveResponse, last: old}, "dead": {rpc: deadResponse, last: old}}, native: map[string]*officialNative{"live": {rpc: liveNative, last: old}, "dead": {rpc: deadNative, last: old}}, tasks: map[string]*officialTask{"completed": {home: filepath.Join(taskRoot, "home"), ended: old}}}
	rt.reap(time.Now())
	if len(rt.threads) != 1 || rt.threads["live"] == nil || len(rt.native) != 1 || rt.native["live"] == nil || len(rt.tasks) != 1 {
		t.Fatal("idle session or completed task was evicted, or dead process was retained")
	}
	for _, path := range []string{liveResponse.root, liveNative.root, taskRoot} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("resumable workspace removed: %v", err)
		}
	}
	for _, path := range []string{deadResponse.root, deadNative.root} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("dead process workspace retained")
		}
	}
}
