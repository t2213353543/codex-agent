package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type accountWriteGatedBody struct {
	io.Reader
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *accountWriteGatedBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered); <-b.release })
	return b.Reader.Read(p)
}

func TestFileAccountWriteContextCancellation(t *testing.T) {
	s := accountTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.ImportDetailedContext(ctx, bulkImportFixtureItems(2, "canceled"), AccountDefaults{}, ImportOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("import cancellation: %v", err)
	}
	accountTestImport(t, s, "existing", "existing-token")
	a, err := s.GetMetadata("existing")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	_, err = s.UpdateAccountVersionContext(ctx, a.ID, a.Version, func(v *Account) error { v.Notes = "must rollback"; cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("callback cancellation: %v", err)
	}
	got, err := s.GetMetadata(a.ID)
	if err != nil || got.Notes != a.Notes || got.Version != a.Version {
		t.Fatal("canceled file callback committed")
	}
}

func TestPGAccountWriteStoreContextCancelsAcquire(t *testing.T) {
	s := accountPGTestStore(t)
	a := pgImportForTest(t, s, "context-store", "context-token")
	for _, kind := range []string{"import", "update", "metadata"} {
		t.Run(kind, func(t *testing.T) {
			tracer := &gatewaySelectionPGAcquireTrace{started: make(chan struct{}, 1)}
			config := s.DB.Pool.Config()
			config.MaxConns, config.MinConns = 1, 0
			config.ConnConfig.Tracer = tracer
			pool, err := pgxpool.NewWithConfig(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			db := *s.DB
			db.Pool = pool
			store := Store{DB: &db}
			held, err := pool.Acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer held.Release()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			tracer.armed.Store(true)
			done := make(chan error, 1)
			go func() {
				var err error
				switch kind {
				case "import":
					_, err = store.ImportDetailedContext(ctx, bulkImportFixtureItems(2, "cancel-import"), AccountDefaults{}, ImportOptions{})
				case "update":
					_, err = store.UpdateAccountVersionContext(ctx, a.ID, a.Version, func(v *Account) error { v.Notes = "canceled"; return nil })
				default:
					_, err = store.GetMetadataContext(ctx, a.ID)
				}
				done <- err
			}()
			select {
			case <-tracer.started:
			case <-time.After(time.Second):
				t.Fatal("store did not reach blocked pool")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("store ignored caller cancellation")
			}
			held.Release()
			got, err := s.GetMetadata(a.ID)
			if err != nil || got.Version != a.Version || got.Notes != a.Notes {
				t.Fatal("canceled store wrote metadata")
			}
		})
	}
}

func TestPGAdminAccountWriteHandlersPropagateCancellation(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	a := managedTestAccount(t, p, "cancel-http-account", nil)
	for _, kind := range []string{"import", "update", "batch", "actions"} {
		t.Run(kind, func(t *testing.T) {
			tracer := &gatewaySelectionPGAcquireTrace{started: make(chan struct{}, 1)}
			config := p.Store.DB.Pool.Config()
			config.MaxConns, config.MinConns = 1, 0
			config.ConnConfig.Tracer = tracer
			pool, err := pgxpool.NewWithConfig(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			db := *p.Store.DB
			db.Pool = pool
			writeProxy := &Proxy{Store: Store{DB: &db}, Groups: p.Groups, Proxies: p.Proxies, Keys: p.Keys, Settings: p.Settings, Metrics: p.Metrics, AdminKey: webTestKey}
			method, path := http.MethodPost, "/api/accounts"
			var body any = map[string]any{"accounts": []map[string]string{{"name": "cancel-new-1", "access_token": "cancel-new-1"}, {"name": "cancel-new-2", "access_token": "cancel-new-2"}}}
			patch := map[string]any{"version": a.Version, "notes": "must not commit"}
			switch kind {
			case "update":
				method, path, body = http.MethodPut, "/api/accounts/"+a.ID, patch
			case "batch":
				path = "/api/accounts/batch"
				body = map[string]any{"items": []any{map[string]any{"id": a.ID, "version": a.Version, "patch": map[string]any{"notes": "must not commit"}}}}
			case "actions":
				path = "/api/accounts/actions"
				body = map[string]any{"action": "update", "items": []managedAccountItem{{ID: a.ID, Version: a.Version}}, "patch": map[string]any{"notes": "must not commit"}}
			}
			raw, _ := json.Marshal(body)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			bodyGate := &accountWriteGatedBody{Reader: bytes.NewReader(raw), entered: make(chan struct{}), release: make(chan struct{})}
			r := httptest.NewRequest(method, path, bodyGate).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer "+webTestKey)
			done := make(chan struct{})
			response := httptest.NewRecorder()
			go func() {
				defer close(done)
				writeProxy.adminRoutes().ServeHTTP(response, r)
			}()
			// The body is first consumed after real admin authentication. Hold
			// the only connection at that boundary, then let the real handler
			// proceed to its preflight/write query and cancel that blocked acquire.
			select {
			case <-bodyGate.entered:
			case <-time.After(time.Second):
				close(bodyGate.release)
				t.Fatal("request never reached authenticated body read")
			}
			held, err := pool.Acquire(t.Context())
			if err != nil {
				close(bodyGate.release)
				t.Fatal(err)
			}
			defer held.Release()
			tracer.armed.Store(true)
			close(bodyGate.release)
			select {
			case <-tracer.started:
			case <-time.After(time.Second):
				t.Fatal("handler did not reach blocked write/preflight")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("handler did not stop after HTTP context cancel")
			}
			held.Release()
			if kind == "batch" || kind == "actions" {
				var result struct {
					Succeeded int `json:"succeeded"`
					Failed    int `json:"failed"`
				}
				if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Succeeded != 0 || result.Failed != 1 {
					t.Fatal("canceled batch reported success")
				}
			} else if response.Code < 400 {
				t.Fatalf("canceled handler succeeded: %d", response.Code)
			}
			current, err := p.Store.GetMetadata(a.ID)
			if err != nil || current.Version != a.Version || current.Notes != a.Notes {
				t.Fatal("canceled handler updated account")
			}
			var count int
			if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM accounts").Scan(&count); err != nil || count != 1 {
				t.Fatalf("canceled import persisted: %d %v", count, err)
			}
		})
	}
}
