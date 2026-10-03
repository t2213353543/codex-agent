package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	gatewayNativeIdleProcessLimit = 16
	gatewayNativeHeadLimit        = 64 << 10
	gatewayNativeBodyLimit        = 32 << 20
)

var (
	errGatewayNativeUnsupported      = errors.New("native HTTP transport requires Linux")
	errGatewayNativeClosed           = errors.New("native HTTP transport is closed")
	errGatewayNativeCapacity         = errors.New("native HTTP transport capacity exhausted")
	errGatewayNativeProtocol         = errors.New("invalid native HTTP transport protocol")
	errGatewayNativeUnavailable      = errors.New("native HTTP helper unavailable")
	errGatewayNativeBodyLimit        = errors.New("native HTTP request body exceeds limit")
	errGatewayNativeProxy            = errors.New("native HTTP transport does not support this proxy")
	errGatewayNativeLifecycle        = errors.New("native HTTP lifecycle must be cli_request or pooled")
	errGatewayNativeLifecycleReady   = errors.New("native HTTP helper lifecycle missing or mismatched; upgrade helper")
	errGatewayNativeConcurrencyReady = errors.New("native HTTP helper must delegate concurrency to account admission; upgrade helper")
	errGatewayNativeUnboundedReady   = errors.New("native helper unbounded payload capability unavailable; upgrade helper")
)

type gatewayNativeOptions struct {
	idleProcessLimit    int
	idle, startup, stop time.Duration
	network             string
	lifecycle           string
	command             func(string) *exec.Cmd
}

type gatewayNativeKey struct {
	accountScope [32]byte
	proxyID      string
	proxyVersion int64
	fingerprint  [32]byte
	origin       string
	timeout      time.Duration
}

type gatewayNativePool struct {
	mu      sync.Mutex
	binary  string
	digest  [32]byte
	options gatewayNativeOptions
	entries map[gatewayNativeKey]*gatewayNativeProcess
	closed  bool
	stop    chan struct{}
	done    chan struct{}
	workers sync.WaitGroup
}

type gatewayNativeProcess struct {
	key              gatewayNativeKey
	command          *exec.Cmd
	stdin            io.WriteCloser
	done             chan struct{}
	dir              string
	address          string
	token            string
	active           int
	lastUsed         time.Time
	retired          bool
	stopOnce         sync.Once
	ready            chan struct{}
	finished         chan struct{}
	watchDone        chan struct{}
	starting         bool
	startupWaiters   int
	startupAbandoned bool
	closing          bool
	startErr         error
	cancel           context.CancelFunc
	socket           os.FileInfo
	bridge           *gatewayNativeSOCKSBridge
	capabilities     map[string]bool
}

func newGatewayNativePool(binaryPath, expectedSHA256 string) (*gatewayNativePool, error) {
	return newGatewayNativePoolWithLifecycle(binaryPath, expectedSHA256, "cli_request")
}

func newGatewayNativePoolWithLifecycle(binaryPath, expectedSHA256, lifecycle string) (*gatewayNativePool, error) {
	if runtime.GOOS != "linux" {
		return nil, errGatewayNativeUnsupported
	}
	return newGatewayNativePoolWithOptions(binaryPath, expectedSHA256, gatewayNativeOptions{lifecycle: lifecycle})
}

// The private options are a self-exec fixture seam, not a runtime TCP fallback.
func newGatewayNativePoolWithOptions(binaryPath, expectedSHA256 string, options gatewayNativeOptions) (*gatewayNativePool, error) {
	if options.lifecycle == "" {
		options.lifecycle = "cli_request"
	}
	if options.lifecycle != "cli_request" && options.lifecycle != "pooled" {
		return nil, errGatewayNativeLifecycle
	}
	decoded, err := hex.DecodeString(expectedSHA256)
	if err != nil || len(decoded) != sha256.Size || !filepath.IsAbs(binaryPath) {
		return nil, errGatewayNativeUnavailable
	}
	var digest [32]byte
	copy(digest[:], decoded)
	if !gatewayNativeBinaryMatches(binaryPath, digest) {
		return nil, errGatewayNativeUnavailable
	}
	if options.idleProcessLimit <= 0 {
		options.idleProcessLimit = gatewayNativeIdleProcessLimit
	}
	if options.idle <= 0 {
		options.idle = 10 * time.Minute
	}
	if options.startup <= 0 {
		options.startup = 10 * time.Second
	}
	if options.stop <= 0 {
		options.stop = 2 * time.Second
	}
	if options.network == "" {
		options.network = "unix"
	}
	if options.command == nil {
		options.command = func(path string) *exec.Cmd { return exec.Command(path) }
	}
	p := &gatewayNativePool{binary: binaryPath, digest: digest, options: options,
		entries: make(map[gatewayNativeKey]*gatewayNativeProcess), stop: make(chan struct{}), done: make(chan struct{})}
	go p.reapIdle()
	return p, nil
}

func gatewayNativeBinaryMatches(path string, expected [32]byte) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return false
	}
	return string(hash.Sum(nil)) == string(expected[:])
}

func gatewayNativeEnvironment() []string {
	var result []string
	for _, key := range []string{"PATH", "SSL_CERT_FILE", "SSL_CERT_DIR", "CODEX_CA_CERTIFICATE"} {
		if value, ok := os.LookupEnv(key); ok {
			result = append(result, key+"="+value)
		}
	}
	return result
}

func (p *gatewayNativePool) RoundTripper(proxy *ProxyEntry, headerTimeout time.Duration) *gatewayNativeRoundTripper {
	t := &gatewayNativeRoundTripper{pool: p, timeout: headerTimeout}
	if proxy != nil {
		snapshot := *proxy
		t.proxy = &snapshot
		t.fingerprint = gatewayProxyFingerprint(snapshot)
		p.mu.Lock()
		for key, child := range p.entries {
			if snapshot.ID != "" && snapshot.Version > 0 && key.proxyID == snapshot.ID && key.proxyVersion < snapshot.Version && key.fingerprint != t.fingerprint {
				child.retired = true
				if child.active == 0 {
					p.stopLocked(key, child)
				}
			}
		}
		p.mu.Unlock()
	}
	return t
}

func (p *gatewayNativePool) reapIdle() {
	defer close(p.done)
	interval := min(p.options.idle/2, time.Minute)
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case now := <-ticker.C:
			p.mu.Lock()
			for key, child := range p.entries {
				if !child.starting && !child.closing && child.active == 0 && child.startupWaiters == 0 && now.Sub(child.lastUsed) >= p.options.idle {
					p.stopLocked(key, child)
				}
			}
			p.mu.Unlock()
		}
	}
}

// Starting and closing reservations remain in entries until their process has
// actually exited, so another request for the same route waits for its cleanup.
func (p *gatewayNativePool) stopLocked(key gatewayNativeKey, child *gatewayNativeProcess) {
	child.retired = true
	if child.starting {
		child.cancel()
		return
	}
	if child.closing {
		return
	}
	child.closing = true
	go p.finish(key, child)
}

func (p *gatewayNativePool) finish(key gatewayNativeKey, child *gatewayNativeProcess) {
	if child.command != nil {
		child.shutdown(p.options.stop)
	}
	if child.watchDone != nil {
		<-child.watchDone
	}
	p.mu.Lock()
	if p.entries[key] == child {
		delete(p.entries, key)
	}
	close(child.finished)
	p.mu.Unlock()
	p.workers.Done()
}

func (p *gatewayNativePool) Close() error {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.stop)
		for key, child := range p.entries {
			p.stopLocked(key, child)
		}
	}
	p.mu.Unlock()
	p.workers.Wait()
	<-p.done
	return nil
}

func (child *gatewayNativeProcess) shutdown(grace time.Duration) {
	child.stopOnce.Do(func() {
		if child.bridge != nil {
			defer child.bridge.Close()
		}
		_ = child.stdin.Close()
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-child.done:
		case <-timer.C:
			_ = child.command.Process.Kill()
			<-child.done
		}
		path := filepath.Join(child.dir, "s")
		// A helper can bind before readiness fails. Reap it before inspecting
		// that private path; never replace an identity already checked at ready.
		if child.socket == nil {
			if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
				child.socket = info
			}
		}
		if current, err := os.Lstat(path); err == nil && child.socket != nil && current.Mode()&os.ModeSocket != 0 && os.SameFile(current, child.socket) {
			_ = os.Remove(path)
		}
		_ = os.Remove(child.dir)
	})
}

func (p *gatewayNativePool) acquire(ctx context.Context, key gatewayNativeKey, proxy *ProxyEntry) (*gatewayNativeProcess, error) {
	for {
		p.mu.Lock()
		if err := ctx.Err(); err != nil {
			p.mu.Unlock()
			return nil, err
		}
		if p.closed {
			p.mu.Unlock()
			return nil, errGatewayNativeClosed
		}
		if child := p.entries[key]; child != nil {
			if child.starting {
				if child.startupAbandoned {
					p.mu.Unlock()
					if err := gatewayNativeWait(ctx, child.finished); err != nil {
						return nil, err
					}
					continue
				}
				child.startupWaiters++
				p.mu.Unlock()
				if err := p.waitStartup(ctx, child); err != nil {
					return nil, err
				}
				continue
			}
			if child.closing {
				p.mu.Unlock()
				if err := gatewayNativeWait(ctx, child.finished); err != nil {
					return nil, err
				}
				continue
			}
			select {
			case <-child.done:
				p.stopLocked(key, child)
				p.mu.Unlock()
				if err := gatewayNativeWait(ctx, child.finished); err != nil {
					return nil, err
				}
				// This request has not opened IPC or sent upstream bytes yet.
				// Retire the dead process before selecting a fresh route.
				continue
			default:
			}
			if child.retired {
				p.mu.Unlock()
				return nil, errGatewayProxyUnavailable
			}
			// Account admission owns concurrency. A transport-local ceiling
			// must not reject requests already admitted for this owner/route.
			child.active++
			p.mu.Unlock()
			return child, nil
		}
		// Startup is shared by all current waiters, not owned by its creator.
		// No request content or credentials are attached to this context.
		startCtx, cancel := context.WithTimeout(context.Background(), p.options.startup)
		child := &gatewayNativeProcess{key: key, starting: true, startupWaiters: 1, ready: make(chan struct{}), finished: make(chan struct{}), watchDone: make(chan struct{}), cancel: cancel}
		p.entries[key] = child
		p.workers.Add(1)
		p.mu.Unlock()
		go p.launch(startCtx, key, proxy, child)
		if err := p.waitStartup(ctx, child); err != nil {
			return nil, err
		}
	}
}

func (p *gatewayNativePool) waitStartup(ctx context.Context, child *gatewayNativeProcess) error {
	err := gatewayNativeWait(ctx, child.ready)
	p.mu.Lock()
	defer p.mu.Unlock()
	child.startupWaiters--
	if contextErr := ctx.Err(); contextErr != nil {
		err = contextErr
	}
	if err != nil {
		if child.starting && child.startupWaiters == 0 {
			child.startupAbandoned = true
			p.stopLocked(child.key, child)
		}
		return err
	}
	if p.closed {
		return errGatewayNativeClosed
	}
	// The shared startup deadline is a local helper failure, not this
	// still-live request's cancellation or deadline.
	if errors.Is(child.startErr, context.Canceled) || errors.Is(child.startErr, context.DeadlineExceeded) {
		return errGatewayNativeUnavailable
	}
	if child.startErr == nil {
		// A ready helper is still reserved until the waiter loops into acquire.
		// Its idle age must not include the time spent starting the process.
		child.lastUsed = time.Now()
	}
	return child.startErr
}

func gatewayNativeWait(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

func (p *gatewayNativePool) launch(ctx context.Context, key gatewayNativeKey, proxy *ProxyEntry, reservation *gatewayNativeProcess) {
	child, err := p.start(ctx, key, proxy)
	reservation.cancel()
	p.mu.Lock()
	reservation.starting, reservation.startErr = false, err
	if child != nil {
		reservation.command, reservation.stdin, reservation.done = child.command, child.stdin, child.done
		reservation.dir, reservation.address, reservation.token = child.dir, child.address, child.token
		reservation.lastUsed, reservation.socket = child.lastUsed, child.socket
		reservation.bridge, reservation.capabilities = child.bridge, child.capabilities
		go p.watchExit(key, reservation)
	} else {
		close(reservation.watchDone)
	}
	close(reservation.ready)
	if err != nil || reservation.retired || p.closed {
		p.stopLocked(key, reservation)
	}
	p.mu.Unlock()
}

func (p *gatewayNativePool) watchExit(key gatewayNativeKey, child *gatewayNativeProcess) {
	defer close(child.watchDone)
	<-child.done
	p.mu.Lock()
	if p.entries[key] == child {
		p.stopLocked(key, child)
	}
	p.mu.Unlock()
}

func (p *gatewayNativePool) release(child *gatewayNativeProcess) {
	p.mu.Lock()
	defer p.mu.Unlock()
	child.active--
	child.lastUsed = time.Now()
	if child.active == 0 && child.retired && p.entries[child.key] == child {
		p.stopLocked(child.key, child)
	}
	p.trimIdleLocked()
}

// Bound only cached idle helpers, never active routes. Startup waiters still
// own a reservation and must not lose it before they acquire their request.
func (p *gatewayNativePool) trimIdleLocked() {
	var idle []*gatewayNativeProcess
	for _, child := range p.entries {
		if !child.starting && !child.closing && child.active == 0 && child.startupWaiters == 0 {
			idle = append(idle, child)
		}
	}
	if len(idle) <= p.options.idleProcessLimit {
		return
	}
	sort.Slice(idle, func(i, j int) bool { return idle[i].lastUsed.Before(idle[j].lastUsed) })
	for _, child := range idle[:len(idle)-p.options.idleProcessLimit] {
		p.stopLocked(child.key, child)
	}
}

func gatewayNativeProxyURL(proxy *ProxyEntry) (*string, error) {
	if proxy == nil {
		return nil, nil
	}
	if proxy.Type != "http" || proxy.Host == "" || proxy.Port < 1 || proxy.Port > 65535 {
		return nil, errGatewayNativeProxy
	}
	u := &url.URL{Scheme: "http", Host: net.JoinHostPort(proxy.Host, strconv.Itoa(proxy.Port))}
	if proxy.Username != "" || proxy.Password != "" {
		u.User = url.UserPassword(proxy.Username, proxy.Password)
	}
	value := u.String()
	return &value, nil
}

func (p *gatewayNativePool) start(ctx context.Context, key gatewayNativeKey, proxy *ProxyEntry) (*gatewayNativeProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var bridge *gatewayNativeSOCKSBridge
	var proxyURL *string
	var err error
	if proxy != nil && proxy.Type == "socks5" {
		bridge, err = newGatewayNativeSOCKSBridge(*proxy, key.origin, key.timeout)
		if err == nil {
			value := bridge.ProxyURL()
			proxyURL = &value
		}
	} else {
		proxyURL, err = gatewayNativeProxyURL(proxy)
	}
	if err != nil {
		return nil, err
	}
	succeeded := false
	defer func() {
		if !succeeded && bridge != nil {
			_ = bridge.Close()
		}
	}()
	if key.timeout < 0 || key.timeout > time.Hour || !gatewayNativeBinaryMatches(p.binary, p.digest) {
		return nil, errGatewayNativeUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "cnx-")
	if err != nil {
		return nil, errGatewayNativeUnavailable
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		_ = os.Remove(dir)
		return nil, errGatewayNativeUnavailable
	}
	dir = canonical
	if err := os.Chmod(dir, 0700); err != nil {
		_ = os.Remove(dir)
		return nil, errGatewayNativeUnavailable
	}
	child := &gatewayNativeProcess{key: key, dir: dir, done: make(chan struct{}), lastUsed: time.Now(), bridge: bridge}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		_ = os.Remove(dir)
		return nil, errGatewayNativeUnavailable
	}
	child.token = hex.EncodeToString(secret)
	command := p.options.command(p.binary)
	if err := ctx.Err(); err != nil {
		_ = os.Remove(dir)
		return nil, err
	}
	if command.Env == nil {
		command.Env = gatewayNativeEnvironment()
	}
	command.Stderr = io.Discard
	stdin, err := command.StdinPipe()
	if err != nil {
		_ = os.Remove(dir)
		return nil, errGatewayNativeUnavailable
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		_ = os.Remove(dir)
		return nil, errGatewayNativeUnavailable
	}
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = os.Remove(dir)
		return nil, errGatewayNativeUnavailable
	}
	child.command, child.stdin = command, stdin
	go func() { _ = command.Wait(); close(child.done) }()
	startCtx, cancel := context.WithTimeout(ctx, p.options.startup)
	defer cancel()
	stopCancel := context.AfterFunc(startCtx, func() { _ = stdin.Close(); _ = command.Process.Kill() })
	defer stopCancel()
	config := map[string]any{"protocol": 1, "upstream": key.origin, "ipc_token": child.token,
		"proxy": proxyURL, "headers_timeout_ms": key.timeout.Milliseconds(), "idle_timeout_ms": 0,
		"http_client_lifecycle": p.options.lifecycle}
	if p.options.network == "unix" {
		config["unix_socket"] = filepath.Join(dir, "s")
	} else {
		config["listen"] = "127.0.0.1:0"
	}
	if err := gatewayNativeWriteJSON(stdin, config); err != nil {
		child.shutdown(p.options.stop)
		return nil, errGatewayNativeUnavailable
	}
	reader := bufio.NewReaderSize(stdout, 4096)
	line, err := reader.ReadSlice('\n')
	var ready struct {
		Protocol      int      `json:"protocol"`
		Transport     string   `json:"transport"`
		UnixSocket    string   `json:"unix_socket"`
		Listen        string   `json:"listen"`
		MaxBody       int      `json:"max_body"`
		MaxActive     *int     `json:"max_active"`
		Capabilities  []string `json:"capabilities"`
		HTTPLifecycle string   `json:"http_client_lifecycle"`
	}
	if err != nil || json.Unmarshal(line, &ready) != nil || ready.Protocol != 1 || ready.MaxBody < gatewayNativeBodyLimit {
		child.shutdown(p.options.stop)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errGatewayNativeUnavailable
	}
	// Zero explicitly means account-controlled concurrency. Reject old or
	// ambiguous handshakes instead of silently queueing behind their 32 slots.
	if ready.MaxActive == nil || *ready.MaxActive != 0 {
		child.shutdown(p.options.stop)
		return nil, errGatewayNativeConcurrencyReady
	}
	if ready.HTTPLifecycle != p.options.lifecycle {
		child.shutdown(p.options.stop)
		return nil, errGatewayNativeLifecycleReady
	}
	child.capabilities = make(map[string]bool)
	if len(ready.Capabilities) > 8 {
		child.shutdown(p.options.stop)
		return nil, errGatewayNativeUnavailable
	}
	for _, capability := range ready.Capabilities {
		if len(capability) == 0 || len(capability) > 64 || strings.IndexFunc(capability, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 {
			child.shutdown(p.options.stop)
			return nil, errGatewayNativeUnavailable
		}
		child.capabilities[capability] = true
	}
	if p.options.network == "unix" {
		child.address = filepath.Join(dir, "s")
		info, statErr := os.Lstat(child.address)
		if ready.Transport != "unix" || ready.UnixSocket != child.address || statErr != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 {
			child.shutdown(p.options.stop)
			return nil, errGatewayNativeUnavailable
		}
		child.socket = info
	} else {
		host, _, splitErr := net.SplitHostPort(ready.Listen)
		ip := net.ParseIP(host)
		if ready.Transport != "tcp" || splitErr != nil || ip == nil || !ip.IsLoopback() {
			child.shutdown(p.options.stop)
			return nil, errGatewayNativeUnavailable
		}
		child.address = ready.Listen
	}
	if !stopCancel() || startCtx.Err() != nil {
		child.shutdown(p.options.stop)
		return nil, errGatewayNativeUnavailable
	}
	// The helper emits one readiness line; discard any later output without
	// retaining sensitive content or allowing its stdout pipe to block.
	go func() { _, _ = io.Copy(io.Discard, reader) }()
	child.lastUsed = time.Now()
	succeeded = true
	return child, nil
}

func gatewayNativeWriteJSON(writer io.Writer, value any) error {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) == 0 || len(raw) > gatewayNativeHeadLimit {
		return errGatewayNativeProtocol
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(raw)))
	if err := gatewayNativeWriteBytes(writer, size[:]); err != nil {
		return err
	}
	return gatewayNativeWriteBytes(writer, raw)
}

func gatewayNativeWriteBytes(writer io.Writer, raw []byte) error {
	n, err := writer.Write(raw)
	if err == nil && n != len(raw) {
		err = io.ErrShortWrite
	}
	return err
}

type gatewayNativeRoundTripper struct {
	pool        *gatewayNativePool
	proxy       *ProxyEntry
	fingerprint [32]byte
	timeout     time.Duration
	checkProxy  func() error
}

func (t *gatewayNativeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, errGatewayNativeProtocol
	}
	if req.Body != nil {
		// The cancellation callback and normal return can race. A request body
		// only needs one Close to interrupt Read and release its resources.
		var closeOnce sync.Once
		closeBody := func() { closeOnce.Do(func() { _ = req.Body.Close() }) }
		defer closeBody()
		stop := context.AfterFunc(req.Context(), closeBody)
		defer stop()
	}
	if req.URL == nil {
		return nil, errGatewayNativeProtocol
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	u := req.URL
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || len(req.Trailer) != 0 || len(req.TransferEncoding) != 0 || req.Host != "" && req.Host != u.Host {
		return nil, errGatewayNativeProtocol
	}
	if len(u.Host) > gatewayNativeHeadLimit || len(u.Path) > gatewayNativeHeadLimit || len(u.RawPath) > gatewayNativeHeadLimit || len(u.RawQuery) > gatewayNativeHeadLimit || len(req.Method) > gatewayNativeHeadLimit {
		return nil, errGatewayNativeProtocol
	}
	if req.Method != "" && !gatewayNativeHeaderName(req.Method) {
		return nil, gatewayNativeError("method_invalid")
	}
	if req.Method == http.MethodConnect || req.Method == http.MethodTrace {
		return nil, gatewayNativeError("method_unsupported")
	}
	headers, err := gatewayNativeRequestHeaders(req.Header)
	if err != nil {
		return nil, err
	}
	unbounded := gatewayUnboundedPayload(req.Context())
	if !unbounded && req.ContentLength > gatewayNativeBodyLimit {
		return nil, errGatewayNativeBodyLimit
	}
	var body []byte
	if req.Body != nil {
		var err error
		var reader io.Reader = req.Body
		if !unbounded {
			reader = io.LimitReader(reader, gatewayNativeBodyLimit+1)
		}
		body, err = io.ReadAll(reader)
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		if err != nil {
			return nil, err
		}
	}
	if !unbounded && len(body) > gatewayNativeBodyLimit {
		return nil, errGatewayNativeBodyLimit
	}
	if req.ContentLength > 0 && req.ContentLength != int64(len(body)) {
		return nil, io.ErrUnexpectedEOF
	}
	for name, values := range req.Header {
		if strings.EqualFold(name, "Content-Length") && len(values) != 0 {
			length, valid := gatewayNativeContentLength(values)
			if !valid || length != int64(len(body)) {
				return nil, gatewayNativeError("content_length_invalid")
			}
		}
	}
	key := gatewayNativeKey{accountScope: gatewayNativeAccountScope(req.Context(), req.Header), fingerprint: t.fingerprint, origin: u.Scheme + "://" + u.Host, timeout: t.timeout}
	if t.proxy != nil {
		key.proxyID = t.proxy.ID
		key.proxyVersion = t.proxy.Version
	}
	child, err := t.pool.acquire(req.Context(), key, t.proxy)
	if err != nil {
		return nil, err
	}
	// Body preparation and shared helper startup may outlive a proxy edit.
	// Reject the selected snapshot; never silently adopt a different route.
	if t.checkProxy != nil {
		if err := t.checkProxy(); err != nil {
			t.pool.release(child)
			return nil, err
		}
	}
	compress := gatewayNativeRequestCompression(req.Context())
	if unbounded && !child.capabilities["unbounded_payload_v1"] {
		t.pool.release(child)
		return nil, errGatewayNativeUnboundedReady
	}
	if compress && !child.capabilities["request_zstd_v1"] {
		t.pool.release(child)
		return nil, errGatewayCompressionCapability
	}
	conn, err := (&net.Dialer{}).DialContext(req.Context(), t.pool.options.network, child.address)
	if err != nil {
		t.pool.release(child)
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		return nil, errGatewayNativeUnavailable
	}
	responseBody := &gatewayNativeBody{conn: conn, ctx: req.Context(), release: func() { t.pool.release(child) }}
	responseBody.stopCancel = context.AfterFunc(req.Context(), func() {
		_ = conn.Close()
		responseBody.once.Do(responseBody.release)
	})
	failed := true
	defer func() {
		if failed {
			_ = responseBody.Close()
		}
	}()
	head := map[string]any{"protocol": 1, "ipc_token": child.token, "method": req.Method,
		"path": u.RequestURI(), "headers": headers, "body_length": len(body)}
	if unbounded {
		head["unbounded_payload"] = true
	}
	if compress {
		head["request_compression"] = "zstd"
	}
	if req.Method == "" {
		head["method"] = http.MethodGet
	}
	// Opening local IPC does not send upstream bytes. Recheck after that wait
	// before disclosing request metadata or body to the helper.
	if err := checkGatewayNativeDispatch(req.Context()); err != nil {
		return nil, err
	}
	if t.checkProxy != nil {
		if err := t.checkProxy(); err != nil {
			return nil, err
		}
	}
	// Pass only the remaining budget after body preparation/startup/dial. The
	// parent context remains authoritative, including deadlines above the IPC
	// protocol's one-day maximum. Round up so sub-millisecond budgets stay valid.
	if deadline, ok := req.Context().Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, context.DeadlineExceeded
		}
		if remaining <= 24*time.Hour {
			head["total_timeout_ms"] = (remaining + time.Millisecond - 1) / time.Millisecond
		}
	}
	if err := gatewayNativeWriteJSON(conn, head); err != nil {
		return nil, responseBody.mapError(err)
	}
	if err := gatewayNativeWriteBytes(conn, body); err != nil {
		return nil, responseBody.mapError(err)
	}
	kind, raw, err := gatewayNativeReadFrame(conn)
	if err != nil {
		return nil, responseBody.mapError(err)
	}
	if kind == 4 {
		return nil, responseBody.mapError(gatewayNativeRemoteError(raw))
	}
	if kind != 1 {
		return nil, errGatewayNativeProtocol
	}
	var decoded struct {
		Status  int        `json:"status"`
		Version string     `json:"version"`
		Headers [][]string `json:"headers"`
	}
	if json.Unmarshal(raw, &decoded) != nil || decoded.Status < 200 || decoded.Status > 599 || len(decoded.Headers) > 512 {
		return nil, errGatewayNativeProtocol
	}
	major, minor := 0, 0
	switch decoded.Version {
	case "HTTP/0.9":
		major, minor = 0, 9
	case "HTTP/1.0":
		major, minor = 1, 0
	case "HTTP/1.1":
		major, minor = 1, 1
	case "HTTP/2":
		major = 2
	case "HTTP/3":
		major = 3
	default:
		return nil, errGatewayNativeProtocol
	}
	responseHeaders := make(http.Header)
	for _, pair := range decoded.Headers {
		if len(pair) != 2 {
			return nil, errGatewayNativeProtocol
		}
		value, err := base64.StdEncoding.DecodeString(pair[1])
		if err != nil || !gatewayNativeHeaderName(pair[0]) || !gatewayNativeHeaderValue(string(value)) {
			return nil, errGatewayNativeProtocol
		}
		responseHeaders.Add(pair[0], string(value))
	}
	length := int64(-1)
	if values := responseHeaders.Values("Content-Length"); len(values) > 0 {
		var valid bool
		length, valid = gatewayNativeContentLength(values)
		if !valid {
			return nil, errGatewayNativeProtocol
		}
	}
	if req.Method == http.MethodHead || decoded.Status == http.StatusNoContent || decoded.Status == http.StatusNotModified {
		responseBody.expected = 0
	} else {
		responseBody.expected = length
	}
	if req.Method != http.MethodHead && (decoded.Status == http.StatusNoContent || decoded.Status == http.StatusNotModified) {
		length = 0
	}
	failed = false
	return &http.Response{StatusCode: decoded.Status, Status: strconv.Itoa(decoded.Status) + " " + http.StatusText(decoded.Status),
		Proto: decoded.Version, ProtoMajor: major, ProtoMinor: minor, Header: responseHeaders,
		Body: responseBody, ContentLength: length, Request: req}, nil
}

// Bound count and encoded size before allocating base64 strings or sorting.
// The helper applies the same 128-field and HTTP HeaderValue grammar limits.
func gatewayNativeRequestHeaders(header http.Header) ([][2]string, error) {
	if len(header) > 128 {
		return nil, gatewayNativeError("request_limit")
	}
	count, size := 0, 0
	seenLength := false
	for name, values := range header {
		if len(name) > gatewayNativeHeadLimit || !gatewayNativeHeaderName(name) {
			return nil, gatewayNativeError("header_invalid")
		}
		if len(values) > 128-count {
			return nil, gatewayNativeError("request_limit")
		}
		count += len(values)
		for _, value := range values {
			if len(value) > gatewayNativeHeadLimit || !gatewayNativeHeaderValue(value) {
				return nil, gatewayNativeError("header_invalid")
			}
			size += len(name) + base64.StdEncoding.EncodedLen(len(value)) + 8
			if size > gatewayNativeHeadLimit {
				return nil, gatewayNativeError("request_limit")
			}
		}
		if len(values) != 0 && strings.EqualFold(name, "Content-Length") {
			_, valid := gatewayNativeContentLength(values)
			if seenLength || !valid {
				return nil, gatewayNativeError("content_length_invalid")
			}
			seenLength = true
		}
	}
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, name)
	}
	sort.Strings(names)
	headers := make([][2]string, 0, count)
	for _, name := range names {
		for _, value := range header[name] {
			headers = append(headers, [2]string{name, base64.StdEncoding.EncodeToString([]byte(value))})
		}
	}
	return headers, nil
}

func gatewayNativeHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < ' ' && value[i] != '\t' || value[i] == 0x7f {
			return false
		}
	}
	return true
}

func gatewayNativeContentLength(values []string) (int64, bool) {
	if len(values) != 1 || values[0] == "" {
		return 0, false
	}
	for i := 0; i < len(values[0]); i++ {
		if values[0][i] < '0' || values[0][i] > '9' {
			return 0, false
		}
	}
	length, err := strconv.ParseInt(values[0], 10, 64)
	return length, err == nil
}

func gatewayNativeHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, value := range []byte(name) {
		if !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(value)) && !(value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9') {
			return false
		}
	}
	return true
}

func gatewayNativeReadFrame(reader io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, gatewayNativeFrameReadError(err)
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length > gatewayNativeHeadLimit || header[0] < 1 || header[0] > 4 || header[0] == 3 && length != 0 {
		return 0, nil, errGatewayNativeProtocol
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(reader, data); err != nil {
		return 0, nil, gatewayNativeFrameReadError(err)
	}
	return header[0], data, nil
}

func gatewayNativeFrameReadError(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return io.ErrUnexpectedEOF
	}
	// Every failed frame is truncated, including a helper reset on Windows.
	// Retain the cause so callers can still identify timeout/transport errors.
	return errors.Join(io.ErrUnexpectedEOF, err)
}

type gatewayNativeError string

func (e gatewayNativeError) Error() string   { return "native HTTP transport: " + string(e) }
func (e gatewayNativeError) Timeout() bool   { return strings.HasSuffix(string(e), "_timeout") }
func (e gatewayNativeError) Temporary() bool { return false }

func gatewayNativeRemoteError(raw []byte) error {
	switch string(raw) {
	case "upstream_timeout", "upstream_connect", "upstream_body", "upstream_transport", "headers_timeout", "idle_timeout", "total_timeout",
		"request_compression_unsupported", "request_compression_json", "request_compression_json_noncanonical", "request_compression_conflict", "request_compression_limit", "request_compression_failed",
		"request_compression_encoding_conflict", "request_compression_not_supported_for_operation", "request_compression_operation",
		"ipc_auth", "request_limit", "path_invalid", "method_invalid", "method_unsupported", "timeout_invalid", "header_invalid",
		"header_hop_by_hop", "content_length_invalid", "ipc_read", "ipc_timeout", "request_json", "response_header_limit", "upgrade_unsupported":
		return gatewayNativeError(raw)
	default:
		return errGatewayNativeProtocol
	}
}

type gatewayNativeBody struct {
	conn       net.Conn
	ctx        context.Context
	stopCancel func() bool
	release    func()
	once       sync.Once
	closed     atomic.Bool
	readMu     sync.Mutex
	pending    []byte
	expected   int64
	read       int64
	terminal   error
}

func (b *gatewayNativeBody) mapError(err error) error {
	if b.ctx.Err() != nil {
		return b.ctx.Err()
	}
	return err
}

func (b *gatewayNativeBody) Read(data []byte) (int, error) {
	b.readMu.Lock()
	defer b.readMu.Unlock()
	if b.terminal == nil {
		if err := b.ctx.Err(); err != nil {
			b.pending, b.terminal = nil, err
			_ = b.Close()
		} else if b.closed.Load() {
			// Closing IPC alone cannot discard the remainder of an already
			// decoded frame. Never hand that output to an abandoned consumer.
			b.pending, b.terminal = nil, http.ErrBodyReadAfterClose
		}
	}
	if len(data) == 0 {
		return 0, nil
	}
	for len(b.pending) == 0 && b.terminal == nil {
		kind, payload, err := gatewayNativeReadFrame(b.conn)
		if contextErr := b.ctx.Err(); contextErr != nil {
			err = contextErr
		}
		if err != nil {
			b.terminal = b.mapError(err)
		} else {
			switch kind {
			case 2:
				if b.expected >= 0 && b.read+int64(len(payload)) > b.expected {
					b.terminal = errGatewayNativeProtocol
				} else {
					b.pending = payload
					b.read += int64(len(payload))
				}
			case 3:
				b.terminal = io.EOF
				if b.expected >= 0 && b.read != b.expected {
					b.terminal = io.ErrUnexpectedEOF
				}
			case 4:
				b.terminal = b.mapError(gatewayNativeRemoteError(payload))
			default:
				b.terminal = errGatewayNativeProtocol
			}
		}
		if b.terminal != nil {
			_ = b.Close()
		}
	}
	if len(b.pending) > 0 {
		n := copy(data, b.pending)
		b.pending = b.pending[n:]
		if len(b.pending) == 0 {
			b.pending = nil
		}
		return n, nil
	}
	return 0, b.terminal
}

func (b *gatewayNativeBody) Close() error {
	b.closed.Store(true)
	b.once.Do(func() {
		b.stopCancel()
		_ = b.conn.Close()
		b.release()
	})
	return nil
}

var _ http.RoundTripper = (*gatewayNativeRoundTripper)(nil)
