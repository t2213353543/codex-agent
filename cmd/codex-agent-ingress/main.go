// Candidate stable HTTP ingress. New HTTP requests choose the current backend;
// existing SSE and upgraded WebSockets retain their selected backend until done.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type target struct {
	Revision   int64  `json:"revision"`
	Generation string `json:"generation"`
	Address    string `json:"address"`
	Held       bool   `json:"held"`
}
type generation struct {
	target     target
	proxy      *httputil.ReverseProxy
	transport  *http.Transport
	active     int64
	websockets int64
}
type ingress struct {
	mu                sync.Mutex
	state             target
	gate              chan struct{}
	generations       map[string]*generation
	waiting           int64
	pending           sync.WaitGroup
	save              func(target) error
	lastUpdateDurable *bool
}

// targetCommitError reports a completed rename whose directory durability
// could not be confirmed. Routing follows the committed target; callers must
// reconcile pending control state, not assume that the old target survived.
type targetCommitError struct{ Err error }

func (e *targetCommitError) Error() string {
	return "target committed; directory durability unconfirmed"
}
func (e *targetCommitError) Unwrap() error { return e.Err }
func targetCommitted(err error) bool {
	var committed *targetCommitError
	return errors.As(err, &committed)
}

func validate(t target) error {
	host, port, err := net.SplitHostPort(t.Address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || port == "" || t.Generation == "" || t.Revision < 1 {
		return errors.New("invalid loopback target identity")
	}
	for _, digit := range port {
		if digit < '0' || digit > '9' {
			return errors.New("invalid numeric backend port")
		}
	}
	value, err := strconv.Atoi(port)
	if err != nil || value < 1 || value > 65535 {
		return errors.New("invalid numeric backend port")
	}
	return nil
}
func newGeneration(t target) *generation {
	address := &url.URL{Scheme: "http", Host: t.Address}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: false, MaxIdleConns: 512, MaxIdleConnsPerHost: 256, DisableCompression: true}
	proxy := &httputil.ReverseProxy{Transport: transport, FlushInterval: -1, Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(address)
		r.Out.Host = r.In.Host
		r.Out.GetBody = nil
		r.SetXForwarded()
		host, _, err := net.SplitHostPort(r.In.RemoteAddr)
		if err == nil {
			r.Out.Header.Set("X-Real-Ip", host)
		}
	}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "gateway ingress backend unavailable", http.StatusBadGateway)
	}}
	return &generation{target: t, transport: transport, proxy: proxy}
}
func newIngress(t target, save func(target) error) (*ingress, error) {
	if err := validate(t); err != nil {
		return nil, err
	}
	p := &ingress{state: t, gate: make(chan struct{}), generations: map[string]*generation{t.Generation: newGeneration(t)}, save: save}
	if !t.Held {
		close(p.gate)
	}
	return p, nil
}
func (p *ingress) update(expected int64, t target) error {
	if err := validate(t); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.Revision != expected || t.Revision != expected+1 {
		return errors.New("ingress revision changed")
	}
	if old, exists := p.generations[t.Generation]; exists && old.target.Address != t.Address {
		return errors.New("generation address cannot be replaced")
	}
	var saveErr error
	if p.save != nil {
		saveErr = p.save(t)
		if saveErr != nil && !targetCommitted(saveErr) {
			return saveErr
		}
	}
	old := p.generations[p.state.Generation]
	if _, ok := p.generations[t.Generation]; !ok {
		p.generations[t.Generation] = newGeneration(t)
	}
	if !p.state.Held && t.Held {
		p.gate = make(chan struct{})
	}
	if p.state.Held && !t.Held {
		close(p.gate)
	}
	switched := p.state.Generation != t.Generation
	p.state = t
	if switched || t.Held {
		old.transport.CloseIdleConnections()
	}
	durable := saveErr == nil
	p.lastUpdateDurable = &durable
	return saveErr
}
func (p *ingress) status() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	counts := map[string]any{}
	for name, g := range p.generations {
		counts[name] = map[string]any{"address": g.target.Address, "active_http_and_websocket": g.active, "active_websockets": g.websockets}
	}
	return map[string]any{"active": p.state, "generations": counts, "waiting_requests": p.waiting, "last_update_durable": p.lastUpdateDurable}
}
func (p *ingress) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var g *generation
	health := (r.Method == http.MethodGet || r.Method == http.MethodHead) && (r.URL.Path == "/health" || r.URL.Path == "/health/live" || r.URL.Path == "/health/ready")
	for {
		p.mu.Lock()
		if !p.state.Held || health {
			g = p.generations[p.state.Generation]
			g.active++
			p.pending.Add(1)
			if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				g.websockets++
			}
			p.mu.Unlock()
			break
		}
		gate := p.gate
		p.waiting++
		p.mu.Unlock()
		select {
		case <-gate:
		case <-r.Context().Done():
			p.mu.Lock()
			p.waiting--
			p.mu.Unlock()
			return
		}
		p.mu.Lock()
		p.waiting--
		p.mu.Unlock()
	}
	defer func() {
		p.mu.Lock()
		g.active--
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			g.websockets--
		}
		p.mu.Unlock()
		p.pending.Done()
	}()
	g.proxy.ServeHTTP(w, r)
}
func (p *ingress) admin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "GET" && r.URL.Path == "/status" {
		_ = json.NewEncoder(w).Encode(p.status())
		return
	}
	if r.Method != "POST" || r.URL.Path != "/switch" {
		http.Error(w, "not found", 404)
		return
	}
	var request struct {
		Expected int64  `json:"expected_revision"`
		Target   target `json:"target"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		http.Error(w, "invalid switch document", 400)
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		http.Error(w, "invalid trailing document", 400)
		return
	}
	if err := p.update(request.Expected, request.Target); err != nil {
		if targetCommitted(err) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "target durability unconfirmed", "committed": true, "state": p.status()})
			return
		}
		http.Error(w, err.Error(), 409)
		return
	}
	_ = json.NewEncoder(w).Encode(p.status())
}
func persist(path string, t target) error {
	return persistWithDirectorySync(path, t, syncTargetDirectory)
}
func syncTargetDirectory(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func persistWithDirectorySync(path string, t target, syncDirectory func(string) error) error {
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".target-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	if err := syncDirectory(dir); err != nil {
		return &targetCommitError{Err: err}
	}
	return nil
}
func run() error {
	listen := flag.String("listen", "0.0.0.0:18878", "stable HTTP listener")
	admin := flag.String("admin-socket", "/run/codex-agent-ingress/control.sock", "private Unix socket")
	state := flag.String("target-state", "/var/lib/codex-agent-ingress/target.json", "existing protected target document")
	flag.Parse()
	raw, err := os.ReadFile(*state)
	if err != nil {
		return err
	}
	var t target
	if json.Unmarshal(raw, &t) != nil {
		return errors.New("invalid target state")
	}
	p, err := newIngress(t, func(t target) error { return persist(*state, t) })
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	control, err := net.Listen("unix", *admin)
	if err != nil {
		return err
	}
	defer control.Close()
	defer os.Remove(*admin)
	if err = os.Chmod(*admin, 0600); err != nil {
		return err
	}
	adminServer := &http.Server{Handler: http.HandlerFunc(p.admin), ReadHeaderTimeout: 5 * time.Second}
	go adminServer.Serve(control)
	server := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stopped := make(chan error, 1)
	go func() { stopped <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		err = server.Shutdown(context.Background())
	case err = <-stopped:
	}
	// No IO/idle deadline and no timer-based WS/SSE termination. Backend releases
	// never restart this ingress process; front upgrades require a separate drain.
	p.pending.Wait()
	_ = adminServer.Close()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ingress:", err)
		os.Exit(1)
	}
}
