package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"
)

const version = "0.4.25"

// Run starts the service or executes a storage subcommand.
func Run(args []string) error {
	return run(args)
}

func run(args []string) error {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return runStorageCommand(args)
	}
	fs := flag.NewFlagSet("codex-agent", flag.ContinueOnError)
	_ = fs.String("data", "data", "legacy data directory (only used by import-legacy)")
	listen := fs.String("listen", "127.0.0.1:18879", "HTTP listen address")
	webDir := fs.String("web", "frontend/dist", "built frontend directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if len(fs.Args()) != 0 {
		return errors.New("unexpected arguments")
	}
	if err := configureGatewayRuntimeIdentity(os.Getenv); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	db, err := databaseFromEnvironment(ctx)
	cancel()
	if err != nil {
		return err
	}
	defer db.Close()
	if err = db.requireSchema(context.Background()); err != nil {
		return err
	}
	if os.Getenv("REDIS_URL") == "" {
		return errors.New("REDIS_URL is required")
	}
	if err = db.ConfigureRedis(os.Getenv("REDIS_URL")); err != nil {
		return err
	}
	var marker legacyImportMarker
	if err = db.readDocument(context.Background(), db.Pool, "legacy_import", &marker); err != nil {
		return err
	}
	if marker.Manifest != "" && !marker.Completed {
		return errors.New("legacy import is incomplete; resume import-legacy before serving requests")
	}
	key, err := db.initializeAdminKey(context.Background(), os.Getenv("ADMIN_KEY"))
	if err != nil {
		return err
	}
	metrics, err := NewPGMetrics(db)
	if err != nil {
		return err
	}
	defer metrics.Close()
	keys, err := NewPGKeyStore(db)
	if err != nil {
		return err
	}
	proxies, err := NewPGProxyStore(db)
	if err != nil {
		return err
	}
	settings := NewPGSettingsStore(db)
	var exists bool
	if err = db.Pool.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM settings WHERE key='admin_settings')").Scan(&exists); err != nil {
		return safeDBError(err)
	}
	if !exists {
		// Another process may initialize or update the document after EXISTS.
		// Its committed settings win; never overwrite them with startup defaults.
		if err = settings.Set(defaultSettings()); err != nil && !errors.Is(err, errVersionConflict) {
			return err
		}
	}
	if _, err = settings.Get(); err != nil {
		return err
	}
	bpsRuntime, bpsBaseURL, err := newBPSRuntime(os.Getenv)
	if err != nil {
		return err
	}
	p := &Proxy{Store: Store{DB: db}, Groups: NewPGGroupStore(db), Keys: keys, Proxies: proxies, Settings: settings, Metrics: metrics, AdminKey: key, Client: newHTTPClient(), BaseURL: bpsBaseURL, BPS: bpsRuntime, TokenURL: oauthURL, startedAt: time.Now()}
	p.RuntimeModes = true
	p.CodexTransport = &Proxy{Client: newHTTPClient(), Proxies: proxies}
	if err := configureGatewayNativeHTTP(p.CodexTransport, os.Getenv); err != nil {
		return err
	}
	defer p.CodexTransport.closeGatewayClients()
	p.PromptCacheObserver, err = newGatewayPromptCacheObserver()
	if err != nil {
		return err
	}
	// PowerPoint keeps its own HTTP pool. The official CLI mode independently
	// uses the configured Codex Rust transport; selecting a mode is explicit.
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		p.closeGatewayClients()
		return err
	}
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveManaged(signalCtx, p, listener, *webDir)
}

// Shared by the real process and the process-level shutdown test.
func serveManaged(parent context.Context, p *Proxy, listener net.Listener, webDir string) error {
	// This function owns the listener even when startup fails before Serve.
	defer listener.Close()
	defer p.closeGatewayClients()
	if err := p.startGatewayIngressEvents(); err != nil {
		return err
	}
	ctx, stop := context.WithCancel(parent)
	defer stop()
	workerCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()
	managementDone := p.startManagementWorkers(workerCtx)
	p.lifecycle = &runtimeLifecycle{stopWorkers: stopWorkers, workersDone: managementDone}
	server := &http.Server{Handler: managementRuntimeHandler(p, webHandler(p, webDir)), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 << 10}
	server.Handler = withTrustedIngressClientIP(server.Handler, os.Getenv("CODEX_TRUSTED_LOOPBACK_INGRESS") == "1")
	var active sync.WaitGroup
	base := server.Handler
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if runtimeLifecycleHandler(p, w, r) {
			return
		}
		active.Add(1)
		defer active.Done()
		p.lifecycle.active.Add(1)
		defer p.lifecycle.active.Add(-1)
		if r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			// Shutdown does not close hijacked connections. Cancel WebSocket
			// handlers explicitly so they release upstream sockets and persist
			// final usage before active.Wait and the final metrics flush.
			wsCtx, cancelWS := context.WithCancel(r.Context())
			stopShutdown := context.AfterFunc(ctx, cancelWS)
			defer func() { stopShutdown(); cancelWS() }()
			r = r.WithContext(wsCtx)
		}
		base.ServeHTTP(w, r)
	})
	saveDone := make(chan struct{})
	go func() {
		defer close(saveDone)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := p.Metrics.Save(); err != nil {
					log.Printf("metrics flush failed: %v", err)
				}
				if err := p.Keys.FlushLastUsed(); err != nil {
					log.Printf("key usage flush failed: %v", err)
				}
			}
		}
	}()
	drainDone := make(chan error, 1)
	go func() {
		<-ctx.Done()
		drain, cancel := context.WithTimeout(context.Background(), 55*time.Second)
		defer cancel()
		err := server.Shutdown(drain)
		if err != nil {
			_ = server.Close()
		}
		drainDone <- err
	}()
	log.Printf("Listening on %s; PostgreSQL storage enabled", listener.Addr())
	serveErr := server.Serve(listener)
	stop()
	drainErr := <-drainDone
	<-saveDone
	<-managementDone
	// Server no longer accepts requests; wait until handlers have persisted final events and refresh results.
	active.Wait()
	if p.Official != nil {
		_ = p.Official.Close()
	}
	ingressCtx, ingressCancel := context.WithTimeout(context.Background(), 5*time.Second)
	ingressErr := p.IngressEvents.Close(ingressCtx)
	ingressCancel()
	flushErr := errors.Join(p.Metrics.Save(), p.Keys.FlushLastUsed(), ingressErr)
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, drainErr, flushErr)
}

func databaseFromEnvironment(ctx context.Context) (*Database, error) {
	if os.Getenv("DATABASE_URL") == "" {
		return nil, errors.New("DATABASE_URL is required; JSON is import-only")
	}
	return OpenDatabase(ctx, os.Getenv("DATABASE_URL"), os.Getenv("CREDENTIAL_ENCRYPTION_KEY"), os.Getenv("CREDENTIAL_ENCRYPTION_KEY_ID"))
}
func (d *Database) requireSchema(ctx context.Context) error {
	files, err := fs.Glob(migrationFiles, "*.sql")
	if err != nil {
		return err
	}
	for _, filename := range files {
		name := "migrations/" + filename
		raw, _ := migrationFiles.ReadFile(filename)
		sum := sha256.Sum256(raw)
		var saved string
		if err = d.Pool.QueryRow(ctx, "SELECT checksum FROM schema_migrations WHERE name=$1", name).Scan(&saved); err != nil {
			return errors.New("database schema not ready; run migrate explicitly")
		}
		if saved != hex.EncodeToString(sum[:]) {
			return errors.New("database migration checksum mismatch")
		}
	}
	return nil
}
func managementRuntimeHandler(p *Proxy, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health/live" {
			writeJSON(w, map[string]string{"status": "ok"})
			return
		}
		if r.URL.Path == "/health/ready" {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			db := p.Store.DB
			if err := db.Pool.Ping(ctx); err != nil {
				sessionError(w, 503, "postgres_unavailable", "PostgreSQL unavailable")
				return
			}
			if err := db.Redis.Ping(ctx).Err(); err != nil {
				sessionError(w, 503, "redis_unavailable", "Redis unavailable")
				return
			}
			if err := p.Metrics.HealthError(); err != nil {
				sessionError(w, 503, "persistence_failed", "pending persistence failure")
				return
			}
			// Read under the readiness deadline instead of SettingsStore's
			// independent database timeout or a potentially stale runtime cache.
			var state settingsState
			if err := db.readDocument(ctx, db.Pool, "admin_settings", &state); err != nil {
				sessionError(w, 503, "settings_unavailable", "gateway settings unavailable")
				return
			}
			if _, err := state.validatedSettings(); err != nil {
				sessionError(w, 503, "settings_unavailable", "gateway settings unavailable")
				return
			}
			writeJSON(w, map[string]string{"status": "ready"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loadClientKey(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		var random [32]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", err
		}
		b = []byte("ca_" + hex.EncodeToString(random[:]))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if errors.Is(err, os.ErrExist) {
			return loadClientKey(path)
		}
		if err != nil {
			return "", err
		}
		_, writeErr := f.Write(b)
		closeErr := f.Close()
		if writeErr != nil {
			return "", writeErr
		}
		if closeErr != nil {
			return "", closeErr
		}
	} else if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(b))
	if len(key) < 32 {
		return "", errors.New("client-key must contain at least 32 characters")
	}
	return key, nil
}
