package server

// Official runtimes run the unmodified Codex executable. This file owns process
// lifecycle only; it never constructs a request to a model backend.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type officialRuntime struct {
	bin, root string
	mu        sync.Mutex
	closed    bool
	processes map[*officialRPC]bool
	tasks     map[string]*officialTask
	threads   map[string]*officialThread
	native    map[string]*officialNative
	gates     [64]sync.Mutex
	stop      chan struct{}
	stopped   chan struct{}
	command   func(context.Context, string, ...string) *exec.Cmd
	catalog   officialCatalogCache
}

func newOfficialRuntime(getenv func(string) string) (*officialRuntime, error) {
	bin := strings.TrimSpace(getenv("CODEX_CLI_PATH"))
	if bin == "" {
		bin = "codex"
	}
	root := strings.TrimSpace(getenv("CODEX_RUNTIME_DIR"))
	if root == "" {
		root = filepath.Join(os.TempDir(), "codex-agent-official")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, errors.New("invalid CODEX_RUNTIME_DIR")
	}
	rt := &officialRuntime{bin: bin, root: root, processes: make(map[*officialRPC]bool), tasks: make(map[string]*officialTask), threads: make(map[string]*officialThread), native: make(map[string]*officialNative), stop: make(chan struct{}), stopped: make(chan struct{}), command: exec.CommandContext}
	go rt.reaper()
	return rt, nil
}

func (rt *officialRuntime) Close() error {
	if rt == nil {
		return nil
	}
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		return nil
	}
	rt.closed = true
	close(rt.stop)
	ps := make([]*officialRPC, 0, len(rt.processes))
	for p := range rt.processes {
		ps = append(ps, p)
	}
	tasks := make([]*officialTask, 0, len(rt.tasks))
	for _, t := range rt.tasks {
		tasks = append(tasks, t)
	}
	rt.mu.Unlock()
	rt.catalog.close()
	for _, p := range ps {
		p.close()
	}
	for _, t := range tasks {
		t.cancel()
	}
	for _, t := range tasks {
		select {
		case <-t.done:
		case <-time.After(5 * time.Second):
		}
	}
	<-rt.stopped
	return nil
}

// Idle sessions and completed task workspaces remain available for resume.
// Reap only processes which have actually exited, never a still-live session.
func (rt *officialRuntime) reaper() {
	defer close(rt.stopped)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			rt.reap(now)
		case <-rt.stop:
			return
		}
	}
}

func (rt *officialRuntime) reap(_ time.Time) {
	var processes []*officialRPC
	rt.mu.Lock()
	for key, thread := range rt.threads {
		if thread.mu.TryLock() {
			if !thread.rpc.alive() {
				delete(rt.threads, key)
				processes = append(processes, thread.rpc)
			}
			thread.mu.Unlock()
		}
	}
	for key, native := range rt.native {
		if native.mu.TryLock() {
			if !native.rpc.alive() {
				delete(rt.native, key)
				processes = append(processes, native.rpc)
			}
			native.mu.Unlock()
		}
	}
	rt.mu.Unlock()
	for _, process := range processes {
		process.close()
	}
}

func officialEnvironment(home string, proxy *ProxyEntry) []string {
	env := []string{}
	for _, name := range []string{"PATH", "SystemRoot", "WINDIR", "COMSPEC", "PATHEXT", "LANG", "LC_ALL", "TZ", "TMP", "TEMP"} {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	env = append(env, "CODEX_HOME="+home, "HOME="+home, "USERPROFILE="+home, "NO_COLOR=1", "RUST_LOG=off,codex_models_manager=error", "CODEX_REFRESH_TOKEN_URL_OVERRIDE=http://127.0.0.1:1/disabled", "NO_PROXY=127.0.0.1,localhost,::1")
	if proxy != nil {
		u := url.URL{Scheme: proxy.Type, Host: net.JoinHostPort(proxy.Host, fmt.Sprint(proxy.Port))}
		if proxy.Username != "" {
			u.User = url.UserPassword(proxy.Username, proxy.Password)
		}
		v := u.String()
		env = append(env, "HTTPS_PROXY="+v, "HTTP_PROXY="+v, "ALL_PROXY="+v)
	}
	return env
}

func (rt *officialRuntime) workspace() (string, string, error) {
	if err := os.MkdirAll(rt.root, 0700); err != nil {
		return "", "", err
	}
	root, err := os.MkdirTemp(rt.root, "session-")
	if err != nil {
		return "", "", err
	}
	home, work := filepath.Join(root, "home"), filepath.Join(root, "workspace")
	for _, p := range []string{home, work} {
		if err = os.Mkdir(p, 0700); err != nil {
			_ = os.RemoveAll(root)
			return "", "", err
		}
	}
	config := "model_provider = \"openai\"\napproval_policy = \"never\"\nsandbox_mode = \"workspace-write\"\ncli_auth_credentials_store = \"file\"\nproject_doc_max_bytes = 0\n[history]\npersistence = \"none\"\n[analytics]\nenabled = false\n[feedback]\nenabled = false\n"
	if err = os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		_ = os.RemoveAll(root)
		return "", "", err
	}
	return home, work, nil
}

type officialRPC struct {
	rt                  *officialRuntime
	cmd                 *exec.Cmd
	stdin               io.WriteCloser
	root, home, work    string
	mu                  sync.Mutex
	next                int64
	waiters             map[string]chan json.RawMessage
	queue               []json.RawMessage
	queued              int
	wake                chan struct{}
	done                chan struct{}
	writeMu             sync.Mutex
	err                 error
	once                sync.Once
	cancel              context.CancelFunc
	initialized         json.RawMessage
	authContext         context.Context
	authMu              sync.Mutex
	authAccount         Account
	authFailure         error
	modelAuthDiagnostic officialModelAuthDiagnostic
}

func (rt *officialRuntime) startRPC(ctx context.Context, account Account, proxy *ProxyEntry) (*officialRPC, error) {
	return rt.startRPCConfigured(ctx, account, proxy, nil)
}

// configure runs before the official process loads its global model catalog.
func (rt *officialRuntime) startRPCConfigured(ctx context.Context, account Account, proxy *ProxyEntry, configure func(string) error) (*officialRPC, error) {
	if current, ok := ctx.Value(officialAuthCurrentKey{}).(officialAuthCurrent); ok {
		latest, err := current(ctx, account)
		if err != nil {
			return nil, err
		}
		account = latest
	}
	rt.mu.Lock()
	closed := rt.closed
	rt.mu.Unlock()
	if closed {
		return nil, errors.New("official runtime closed")
	}
	home, work, err := rt.workspace()
	if err != nil {
		return nil, err
	}
	if configure != nil {
		if err = configure(home); err != nil {
			_ = os.RemoveAll(filepath.Dir(home))
			return nil, err
		}
	}
	processCtx, cancel := context.WithCancel(context.Background())
	p := &officialRPC{rt: rt, home: home, work: work, root: filepath.Dir(home), waiters: make(map[string]chan json.RawMessage), wake: make(chan struct{}, 1), done: make(chan struct{}), cancel: cancel, authAccount: account}
	p.setAuthContext(ctx)
	p.cmd = rt.command(processCtx, rt.bin, "app-server")
	officialProcessGroup(p.cmd)
	p.cmd.Dir = work
	p.cmd.Env = officialEnvironment(home, proxy)
	p.cmd.Stderr = &p.modelAuthDiagnostic
	p.stdin, err = p.cmd.StdinPipe()
	if err != nil {
		p.close()
		return nil, err
	}
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		p.close()
		return nil, err
	}
	if err = p.cmd.Start(); err != nil {
		p.close()
		return nil, errors.New("official Codex executable could not start; check CODEX_CLI_PATH")
	}
	go p.read(stdout)
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		p.close()
		return nil, errors.New("official runtime unavailable")
	}
	rt.processes[p] = true
	rt.mu.Unlock()
	callCtx, stop := context.WithTimeout(ctx, 60*time.Second)
	defer stop()
	p.initialized, err = p.call(callCtx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "codex_agent_gateway", "version": "1.0"}, "capabilities": map[string]bool{"experimentalApi": true}})
	if err == nil {
		err = p.send(map[string]any{"method": "initialized", "params": map[string]any{}})
	}
	if err == nil {
		_, err = p.call(callCtx, "account/login/start", map[string]any{"type": "chatgptAuthTokens", "accessToken": account.AccessToken, "chatgptAccountId": account.AccountID})
	}
	if err != nil {
		p.close()
		return nil, fmt.Errorf("official App Server initialization failed: %w", err)
	}
	return p, nil
}

func (p *officialRPC) alive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err == nil
}

func (p *officialRPC) read(stdout io.Reader) {
	err := officialReadJSONLines(stdout, func(line []byte) error {
		raw := append(json.RawMessage(nil), line...)
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(raw, &m) != nil {
			return errors.New("official runtime emitted invalid JSON")
		}
		if p.handleAuthRequest(raw) {
			return nil
		}
		p.mu.Lock()
		if len(m.ID) > 0 && m.Method == "" {
			if ch := p.waiters[string(m.ID)]; ch != nil {
				delete(p.waiters, string(m.ID))
				p.mu.Unlock()
				ch <- raw
				return nil
			}
		}
		p.queue = append(p.queue, raw)
		p.queued += len(raw)
		p.mu.Unlock()
		select {
		case p.wake <- struct{}{}:
		default:
		}
		return nil
	})
	if err == nil {
		err = io.EOF
	}
	p.fail(err)
	_ = p.cmd.Wait()
	close(p.done)
}

func (p *officialRPC) fail(err error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.mu.Unlock()
	p.cancel()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}
func (p *officialRPC) close() {
	p.once.Do(func() {
		p.fail(errors.New("official runtime closed"))
		p.cancel()
		if p.stdin != nil {
			_ = p.stdin.Close()
		}
		if p.cmd != nil && p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
			select {
			case <-p.done:
			case <-time.After(5 * time.Second):
			}
		}
		if p.rt != nil {
			p.rt.mu.Lock()
			delete(p.rt.processes, p)
			p.rt.mu.Unlock()
		}
		if p.root != "" {
			_ = os.RemoveAll(p.root)
		}
	})
}
func (p *officialRPC) send(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_, err = p.stdin.Write(append(raw, '\n'))
	return err
}
func (p *officialRPC) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	p.setAuthContext(ctx)
	p.mu.Lock()
	p.next++
	id := fmt.Sprintf("gateway_internal_%d", p.next)
	ch := make(chan json.RawMessage, 1)
	idRaw, _ := json.Marshal(id)
	key := string(idRaw)
	p.waiters[key] = ch
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.waiters, key); p.mu.Unlock() }()
	if err := p.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case raw := <-ch:
		p.mu.Lock()
		authFailure := p.authFailure
		p.mu.Unlock()
		if authFailure != nil {
			return nil, authFailure
		}
		var m struct {
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		_ = json.Unmarshal(raw, &m)
		if len(m.Error) > 0 && string(m.Error) != "null" {
			return nil, officialRPCFailure(method, m.Error)
		}
		return m.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.done:
		return nil, errors.New("official App Server exited")
	}
}
func (p *officialRPC) receive(ctx context.Context) (json.RawMessage, error) {
	p.setAuthContext(ctx)
	for {
		p.mu.Lock()
		if p.authFailure != nil {
			err := p.authFailure
			p.mu.Unlock()
			return nil, err
		}
		if len(p.queue) > 0 {
			raw := p.queue[0]
			p.queue[0] = nil
			p.queue = p.queue[1:]
			p.queued -= len(raw)
			p.mu.Unlock()
			return raw, nil
		}
		err := p.err
		p.mu.Unlock()
		if err != nil {
			return nil, errors.New("official App Server stream ended")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.wake:
		case <-p.done:
		}
	}
}

func (p *officialRPC) requeue(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	p.mu.Lock()
	p.queue = append([]json.RawMessage{raw}, p.queue...)
	p.queued += len(raw)
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}
