package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOfficialProcessFixture(t *testing.T) {
	fixture := false
	for _, v := range os.Args {
		if v == "--official-task-fixture" {
			fixture = true
		}
	}
	if !fixture {
		t.Skip("subprocess fixture")
	}
	data, _ := io.ReadAll(os.Stdin)
	prompt := string(data)
	if prompt == "WAIT" {
		time.Sleep(20 * time.Second)
		os.Exit(0)
	}
	if prompt == "OVERSIZED_EVENT" {
		fmt.Println(`{"type":"thread.started","thread_id":"fixture-thread"}`)
		event, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]string{"type": "agent_message", "text": strings.Repeat("x", (32<<20)+1)}})
		fmt.Println(string(event))
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`)
		os.Exit(0)
	}
	if prompt == "INVALID_EVENT" {
		fmt.Println("not-json")
		time.Sleep(20 * time.Second)
		os.Exit(0)
	}
	fmt.Println(`{"type":"thread.started","thread_id":"fixture-thread"}`)
	if prompt == "NO_TERMINAL" {
		os.Exit(0)
	}
	if prompt == "FAILED_TERMINAL" {
		fmt.Println(`{"type":"turn.failed","error":{"message":"fixture failure"}}`)
		os.Exit(0)
	}
	_ = os.WriteFile("result.txt", data, 0600)
	text, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]string{"type": "agent_message", "text": prompt}})
	fmt.Println(string(text))
	fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":17,"output_tokens":3,"cached_input_tokens":4}}`)
	os.Exit(0)
}

func officialTaskFixtureRuntime(t *testing.T) *officialRuntime {
	t.Helper()
	root := t.TempDir()
	rt, err := newOfficialRuntime(func(k string) string {
		if k == "CODEX_RUNTIME_DIR" {
			return root
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	rt.command = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		return exec.CommandContext(ctx, exe, append([]string{"-test.run=^TestOfficialProcessFixture$", "--", "--official-task-fixture"}, args...)...)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

func waitOfficialTask(t *testing.T, task *officialTask) {
	t.Helper()
	select {
	case <-task.done:
	case <-time.After(8 * time.Second):
		t.Fatal("fixture task timeout")
	}
}

func TestOfficialHostedCLICompletionResumeAndIsolation(t *testing.T) {
	rt := officialTaskFixtureRuntime(t)
	account := Account{ID: "account-a", AccessToken: "synthetic", AccountID: "workspace-a"}
	task, err := rt.createTask(context.Background(), "key-a", account, nil, officialTaskInput{Prompt: "FIRST", Files: map[string]string{"input/source.txt": "source"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitOfficialTask(t, task)
	if task.snapshot()["status"] != "completed" || task.usage["input_tokens"] != 17 {
		t.Fatal("completion or usage missing")
	}
	if _, err = os.Stat(filepath.Join(task.home, "auth.json")); !os.IsNotExist(err) {
		t.Fatal("credential file retained after task")
	}
	file, err := os.ReadFile(filepath.Join(task.work, "result.txt"))
	if err != nil || string(file) != "FIRST" {
		t.Fatal("real child file missing")
	}
	if _, err = rt.createTask(context.Background(), "key-b", account, nil, officialTaskInput{Prompt: "SECOND", Resume: task.id}, nil); err == nil {
		t.Fatal("another key resumed task")
	}
	resumed, err := rt.createTask(context.Background(), "key-a", account, nil, officialTaskInput{Prompt: "SECOND", Resume: task.id}, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitOfficialTask(t, resumed)
	if resumed.id == task.id || resumed.home != task.home || resumed.thread != "fixture-thread" {
		t.Fatal("resume lineage mismatch")
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/events", nil)
	req.Header.Set("Last-Event-ID", "0")
	resumed.serve(w, req, "events")
	if strings.Contains(w.Body.String(), "id: 0\n") || !strings.Contains(w.Body.String(), "SECOND") || !strings.Contains(w.Body.String(), "task.completed") {
		t.Fatal("event replay cursor invalid")
	}
	w = httptest.NewRecorder()
	resumed.serve(w, httptest.NewRequest("GET", "/files?path=../home/auth.json", nil), "files")
	if w.Code != 400 {
		t.Fatal("workspace path escaped")
	}
}

func TestOfficialHostedCLIRequiresActualTerminalAndCancels(t *testing.T) {
	rt := officialTaskFixtureRuntime(t)
	a := Account{ID: "a", AccessToken: "synthetic"}
	for _, prompt := range []string{"NO_TERMINAL", "FAILED_TERMINAL"} {
		task, err := rt.createTask(context.Background(), "k", a, nil, officialTaskInput{Prompt: prompt}, nil)
		if err != nil {
			t.Fatal(err)
		}
		waitOfficialTask(t, task)
		if task.snapshot()["status"] != "failed" {
			t.Fatal("process exit zero falsely implied model success")
		}
	}
	task, err := rt.createTask(context.Background(), "k", a, nil, officialTaskInput{Prompt: "WAIT"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	task.cancel()
	waitOfficialTask(t, task)
	if task.snapshot()["status"] != "cancelled" {
		t.Fatal("cancel did not stop task")
	}
}

func TestOfficialHostedCLIResumeWaitsForCleanupAndFailedStartRemovesAuth(t *testing.T) {
	rt := officialTaskFixtureRuntime(t)
	a := Account{ID: "a", AccessToken: "synthetic"}
	entered, finish := make(chan struct{}), make(chan struct{})
	task, err := rt.createTask(context.Background(), "k", a, nil, officialTaskInput{Prompt: "FIRST"}, func(*officialTask) {
		close(entered)
		<-finish
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		select {
		case <-finish:
		default:
			close(finish)
		}
	}()
	select {
	case <-entered:
	case <-time.After(8 * time.Second):
		t.Fatal("callback timeout")
	}
	if _, err := rt.createTask(context.Background(), "k", a, nil, officialTaskInput{Prompt: "SECOND", Resume: task.id}, nil); err == nil {
		t.Fatal("resume started before previous task cleanup completed")
	}
	close(finish)
	waitOfficialTask(t, task)
	lineageEntered, lineageFinish := make(chan struct{}), make(chan struct{})
	second, err := rt.createTask(context.Background(), "k", a, nil, officialTaskInput{Prompt: "SECOND", Resume: task.id}, func(*officialTask) {
		close(lineageEntered)
		<-lineageFinish
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		select {
		case <-lineageFinish:
		default:
			close(lineageFinish)
		}
	}()
	select {
	case <-lineageEntered:
	case <-time.After(8 * time.Second):
		t.Fatal("lineage callback timeout")
	}
	if _, err := rt.createTask(context.Background(), "k", a, nil, officialTaskInput{Prompt: "THIRD", Resume: task.id}, nil); err == nil {
		t.Fatal("ancestor resume bypassed successor cleanup")
	}
	close(lineageFinish)
	waitOfficialTask(t, second)
	rt.command = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, filepath.Join(t.TempDir(), "nonexistent-codex"))
	}
	if _, err := rt.createTask(context.Background(), "k", a, nil, officialTaskInput{Prompt: "SECOND", Resume: task.id}, nil); err == nil {
		t.Fatal("missing executable started")
	}
	if _, err := os.Stat(filepath.Join(task.home, "auth.json")); !os.IsNotExist(err) {
		t.Fatal("failed resumed process retained auth.json")
	}
}

func TestOfficialHostedCLILargeEventCompletesWithoutScannerLimit(t *testing.T) {
	rt := officialTaskFixtureRuntime(t)
	task, err := rt.createTask(context.Background(), "k", Account{ID: "a", AccessToken: "synthetic"}, nil, officialTaskInput{Prompt: "OVERSIZED_EVENT"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitOfficialTask(t, task)
	if task.snapshot()["status"] != "completed" {
		t.Fatal("valid large output interrupted the task")
	}
}

func TestOfficialHostedCLIInvalidJSONTerminatesChild(t *testing.T) {
	rt := officialTaskFixtureRuntime(t)
	task, err := rt.createTask(context.Background(), "k", Account{ID: "a", AccessToken: "synthetic"}, nil, officialTaskInput{Prompt: "INVALID_EVENT"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitOfficialTask(t, task)
	if task.snapshot()["status"] != "failed" {
		t.Fatal("invalid JSON did not fail the task")
	}
}

func TestOfficialHostedCLIConcurrentResumeAndRuntimeShutdown(t *testing.T) {
	rt := officialTaskFixtureRuntime(t)
	a := Account{ID: "a", AccessToken: "synthetic"}
	first, err := rt.createTask(context.Background(), "k", a, nil, officialTaskInput{Prompt: "FIRST"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitOfficialTask(t, first)
	var wg sync.WaitGroup
	accepted := make(chan *officialTask, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if task, err := rt.createTask(context.Background(), "k", a, nil, officialTaskInput{Prompt: "WAIT", Resume: first.id}, nil); err == nil {
				accepted <- task
			}
		}()
	}
	close(start)
	wg.Wait()
	close(accepted)
	var running *officialTask
	count := 0
	for task := range accepted {
		count++
		running = task
	}
	if count != 1 {
		t.Fatalf("simultaneous resumes accepted %d tasks", count)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	waitOfficialTask(t, running)
	if running.snapshot()["status"] != "cancelled" {
		t.Fatal("runtime close left hosted task running")
	}
	if _, err := os.Stat(filepath.Join(running.home, "auth.json")); !os.IsNotExist(err) {
		t.Fatal("runtime shutdown retained task credentials")
	}
}
