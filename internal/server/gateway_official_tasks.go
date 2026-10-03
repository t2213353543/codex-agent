package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type officialTask struct {
	mu                                                        sync.Mutex
	id, key, owner, home, work, thread, model, effort, status string
	started, ended                                            time.Time
	events                                                    []json.RawMessage
	bytes                                                     int
	wake                                                      chan struct{}
	done                                                      chan struct{}
	cancel                                                    context.CancelFunc
	err                                                       string
	terminalSuccess                                           bool
	terminalFailure                                           bool
	usage                                                     map[string]int64
}

func (t *officialTask) snapshot() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	end := t.ended
	if end.IsZero() {
		end = time.Now()
	}
	return map[string]any{"id": t.id, "status": t.status, "thread_id": t.thread, "model": t.model, "reasoning_effort": t.effort, "created_at": t.started.Unix(), "elapsed_ms": end.Sub(t.started).Milliseconds(), "event_count": len(t.events), "error": t.err, "events_url": "/v1/codex/tasks/" + t.id + "/events", "files_url": "/v1/codex/tasks/" + t.id + "/files"}
}
func (t *officialTask) append(raw []byte) error {
	if !json.Valid(raw) {
		return errors.New("Codex CLI emitted invalid JSON")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, append(json.RawMessage(nil), raw...))
	t.bytes += len(raw)
	var event struct {
		Type   string           `json:"type"`
		Thread string           `json:"thread_id"`
		Usage  map[string]int64 `json:"usage"`
	}
	_ = json.Unmarshal(raw, &event)
	if event.Type == "thread.started" {
		t.thread = event.Thread
	}
	if event.Type == "turn.completed" {
		t.terminalSuccess = true
		t.usage = event.Usage
	}
	if event.Type == "turn.failed" {
		t.terminalFailure = true
	}
	select {
	case t.wake <- struct{}{}:
	default:
	}
	return nil
}

type officialTaskInput struct {
	Prompt string            `json:"prompt"`
	Model  string            `json:"model"`
	Effort string            `json:"reasoning_effort"`
	Resume string            `json:"resume_task_id"`
	Files  map[string]string `json:"files"`
}

func (rt *officialRuntime) createTask(ctx context.Context, key string, account Account, proxy *ProxyEntry, input officialTaskInput, finished func(*officialTask)) (*officialTask, error) {
	if strings.TrimSpace(input.Prompt) == "" {
		return nil, errors.New("prompt must not be empty")
	}
	if input.Model == "" {
		input.Model = "gpt-6-astra"
	}
	if input.Effort == "" {
		input.Effort = "xhigh"
	}
	if !officialValidEffort(input.Effort) {
		return nil, errors.New("invalid reasoning_effort")
	}
	for path := range input.Files {
		if !filepath.IsLocal(path) || path == "." {
			return nil, errors.New("invalid workspace file")
		}
	}
	// Serialize a resume lineage, including simultaneous resumes of different
	// tasks that share the same official workspace.
	rt.mu.Lock()
	resume := rt.tasks[input.Resume]
	resumeHome := "new"
	if resume != nil {
		resumeHome = resume.home
	}
	rt.mu.Unlock()
	digest := sha256.Sum256([]byte(resumeHome))
	gate := &rt.gates[int(digest[0])%len(rt.gates)]
	gate.Lock()
	defer gate.Unlock()
	var home, work, thread string
	rt.mu.Lock()
	closed := rt.closed
	previous := rt.tasks[input.Resume]
	rt.mu.Unlock()
	if closed {
		return nil, errors.New("official runtime closed")
	}
	if input.Resume != "" {
		if previous == nil || previous.key != key || previous.owner != gatewayAccountRef(account) {
			return nil, errors.New("resume task is unavailable for this key and account")
		}
		previous.mu.Lock()
		active := previous.ended.IsZero()
		home, work, thread = previous.home, previous.work, previous.thread
		previous.mu.Unlock()
		// A terminal status precedes the completion callback and credential
		// cleanup. Reusing the workspace before done closes could let the old
		// task remove a newly written auth.json from its successor.
		select {
		case <-previous.done:
		default:
			active = true
		}
		if active || thread == "" {
			return nil, errors.New("resume task has no completed Codex thread")
		}
		rt.mu.Lock()
		for _, task := range rt.tasks {
			if task.home == home {
				task.mu.Lock()
				active = task.ended.IsZero()
				task.mu.Unlock()
				select {
				case <-task.done:
				default:
					active = true
				}
				if active {
					break
				}
			}
		}
		rt.mu.Unlock()
		if active {
			return nil, errors.New("another task is using this workspace")
		}
	} else {
		var err error
		home, work, err = rt.workspace()
		if err != nil {
			return nil, err
		}
	}
	keepWorkspace := input.Resume != ""
	defer func() {
		if !keepWorkspace && home != "" {
			_ = os.RemoveAll(filepath.Dir(home))
		}
	}()
	for path, content := range input.Files {
		if !filepath.IsLocal(path) || path == "." {
			return nil, errors.New("invalid workspace file")
		}
		root, err := os.OpenRoot(work)
		if err != nil {
			return nil, err
		}
		err = root.MkdirAll(filepath.Dir(path), 0700)
		if err == nil {
			var file *os.File
			file, err = root.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
			if err == nil {
				_, err = io.WriteString(file, content)
				_ = file.Close()
			}
		}
		_ = root.Close()
		if err != nil {
			return nil, errors.New("workspace file could not be written")
		}
	}
	auth, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "OPENAI_API_KEY": nil, "tokens": map[string]string{"id_token": account.AccessToken, "access_token": account.AccessToken, "refresh_token": "", "account_id": account.AccountID}, "last_refresh": time.Now().UTC().Format(time.RFC3339)})
	launched := false
	defer func() {
		if !launched {
			_ = os.Remove(filepath.Join(home, "auth.json"))
		}
	}()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), auth, 0600); err != nil {
		return nil, err
	}
	args := []string{"exec", "--json", "--skip-git-repo-check", "--color", "never", "-m", input.Model, "-c", "model_reasoning_effort=" + strconv.Quote(input.Effort)}
	if thread != "" {
		args = append(args, "resume", thread)
	}
	args = append(args, "-")
	taskCtx, cancel := context.WithCancel(ctx)
	t := &officialTask{id: "task_" + strings.ReplaceAll(bpsUUID(), "-", ""), key: key, owner: gatewayAccountRef(account), home: home, work: work, thread: thread, model: input.Model, effort: input.Effort, status: "running", started: time.Now(), wake: make(chan struct{}, 1), done: make(chan struct{}), cancel: cancel}
	cmd := rt.command(taskCtx, rt.bin, args...)
	officialProcessGroup(cmd)
	cmd.Dir = work
	cmd.Env = officialEnvironment(home, proxy)
	cmd.Stdin = strings.NewReader(input.Prompt)
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		cancel()
		return nil, errors.New("official Codex CLI could not start")
	}
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		cancel()
		_ = cmd.Wait()
		return nil, errors.New("official runtime closed")
	}
	rt.tasks[t.id] = t
	rt.mu.Unlock()
	keepWorkspace = true
	launched = true
	go func() {
		defer cancel()
		defer close(t.done)
		defer func() {
			if finished != nil {
				finished(t)
			}
		}()
		defer os.Remove(filepath.Join(home, "auth.json"))
		err = officialReadJSONLines(stdout, t.append)
		if err != nil {
			// Invalid output or a read failure must not leave the child blocked.
			cancel()
		}
		exitErr := cmd.Wait()
		if err == nil {
			err = exitErr
		}
		t.mu.Lock()
		t.ended = time.Now()
		t.status = "completed"
		if err != nil || !t.terminalSuccess || t.terminalFailure {
			t.status = "failed"
			t.err = "official Codex CLI task failed"
		}
		if taskCtx.Err() != nil && err == exitErr {
			t.status = "cancelled"
			t.err = ""
		}
		t.mu.Unlock()
	}()
	return t, nil
}

func officialValidEffort(v string) bool {
	switch v {
	case "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return true
	}
	return false
}

func (t *officialTask) serve(w http.ResponseWriter, r *http.Request, action string) {
	switch action {
	case "":
		if r.Method != http.MethodGet {
			apiError(w, 405, "use GET")
			return
		}
		officialWriteJSON(w, 200, t.snapshot())
	case "cancel":
		if r.Method != http.MethodPost {
			apiError(w, 405, "use POST")
			return
		}
		t.cancel()
		officialWriteJSON(w, 202, map[string]any{"id": t.id, "status": "cancelling"})
	case "events":
		if r.Method != http.MethodGet {
			apiError(w, 405, "use GET")
			return
		}
		cursor := 0
		if raw := r.Header.Get("Last-Event-ID"); raw != "" {
			v, err := strconv.Atoi(raw)
			if err != nil || v < 0 {
				apiError(w, 400, "invalid Last-Event-ID")
				return
			}
			cursor = v + 1
			if cursor <= 0 {
				apiError(w, 400, "invalid Last-Event-ID")
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		for {
			t.mu.Lock()
			batch := append([]json.RawMessage(nil), t.events...)
			done := !t.ended.IsZero()
			t.mu.Unlock()
			for cursor < len(batch) {
				_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(60 * time.Second))
				if _, err := fmt.Fprintf(w, "id: %d\nevent: codex\ndata: %s\n\n", cursor, batch[cursor]); err != nil {
					return
				}
				cursor++
				if http.NewResponseController(w).Flush() != nil {
					return
				}
			}
			if done {
				_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(60 * time.Second))
				raw, _ := json.Marshal(t.snapshot())
				_, _ = fmt.Fprintf(w, "event: task.completed\ndata: %s\n\n", raw)
				_ = http.NewResponseController(w).Flush()
				return
			}
			_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
			select {
			case <-r.Context().Done():
				return
			case <-t.wake:
			case <-t.done:
			}
		}
	case "files":
		if r.Method != http.MethodGet {
			apiError(w, 405, "use GET")
			return
		}
		root, err := os.OpenRoot(t.work)
		if err != nil {
			apiError(w, 404, "workspace unavailable")
			return
		}
		defer root.Close()
		path := r.URL.Query().Get("path")
		if path == "" {
			path = "."
		}
		if path != "." && !filepath.IsLocal(path) {
			apiError(w, 400, "path must stay inside the task workspace")
			return
		}
		f, err := root.Open(path)
		if err != nil {
			apiError(w, 404, "workspace path unavailable")
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			apiError(w, 404, "workspace path unavailable")
			return
		}
		if info.IsDir() {
			entries, err := f.ReadDir(-1)
			if err != nil && err != io.EOF {
				apiError(w, 500, "cannot read workspace directory")
				return
			}
			out := []map[string]any{}
			for _, e := range entries {
				out = append(out, map[string]any{"name": e.Name(), "directory": e.IsDir()})
			}
			officialWriteJSON(w, 200, map[string]any{"path": path, "entries": out})
			return
		}
		if !info.Mode().IsRegular() {
			apiError(w, 400, "file is not a regular file")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.Copy(w, f)
	default:
		apiError(w, 404, "unknown task endpoint")
	}
}
