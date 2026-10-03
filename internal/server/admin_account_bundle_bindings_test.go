package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestManagedBundleBindingResolution(t *testing.T) {
	exerciseManagedBundleBindingResolution(t, webTestSetup)
}

func TestPGManagedBundleBindingResolution(t *testing.T) {
	exerciseManagedBundleBindingResolution(t, adminPGTestProxy)
}

func exerciseManagedBundleBindingResolution(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, handler := setup(t)
	group, err := p.Groups.Create("bundle-binding", "")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := p.Proxies.Create(ProxyEntry{Name: "bundle-binding", Type: "http", Host: "127.0.0.1", Port: 9})
	if err != nil {
		t.Fatal(err)
	}
	proxy, err = p.Proxies.UpdateVersion(proxy.ID, 0, func(v *ProxyEntry) error { v.Enabled = false; return nil })
	if err != nil {
		t.Fatal(err)
	}
	bundle := managedAccountImportBundle{Type: "codex-agent-accounts", Version: 1, Accounts: []managedAccountBackup{{Name: "bound-import", Concurrency: 1, Weight: 1, WindowDays: 1, Groups: []string{"foreign-group", "foreign-group"}, Proxies: []string{"foreign-proxy"}, Credentials: json.RawMessage(`{"access_token":"synthetic-binding-token"}`)}}}
	groups := map[string]string{"foreign-group": group.ID, "unused": "missing-group"}
	proxies := map[string]string{"foreign-proxy": proxy.ID, "unused": "missing-proxy"}
	input := map[string]any{"data": bundle, "group_mapping": groups, "proxy_mapping": proxies, "dry_run": true}
	var report ImportReport
	preview := managedTestRequest(t, handler, "POST", "/api/accounts/import-bundle", input, 200)
	if json.Unmarshal(preview.Body.Bytes(), &report) != nil || report.Failed != 0 {
		t.Fatal("mapping preview failed")
	}
	if _, err := p.Store.Get("bound-import"); !errors.Is(err, errNotFound) {
		t.Fatal("preview wrote an account", err)
	}
	input["dry_run"] = false
	response := managedTestRequest(t, handler, "POST", "/api/accounts/import-bundle", input, 200)
	if json.Unmarshal(response.Body.Bytes(), &report) != nil || report.Added != 1 || report.Failed != 0 {
		t.Fatal("mapped import failed")
	}
	account, err := p.Store.Get("bound-import")
	if err != nil || !reflect.DeepEqual(account.Groups, []string{group.ID}) || !reflect.DeepEqual(account.Proxies, []string{proxy.ID}) {
		t.Fatal("bound local identity changed", err)
	}
	for _, missing := range []string{"", "missing-local-group"} {
		groups["foreign-group"] = missing
		input["dry_run"] = true
		response = managedTestRequest(t, handler, "POST", "/api/accounts/import-bundle", input, 200)
		if json.Unmarshal(response.Body.Bytes(), &report) != nil || report.Failed != 1 || report.Results[0].Code != "invalid_bundle_account" {
			t.Fatal("explicit invalid mapping was ignored")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := p.managedBundleBindingIDs(ctx, bundle, groups, proxies); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled binding read did not stop", err)
	}
}

type bundleBindingReadTrace struct {
	mu      sync.Mutex
	rows    int64
	queries int
}

func (*bundleBindingReadTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (trace *bundleBindingReadTrace) TraceQueryEnd(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.queries++
	trace.rows += data.CommandTag.RowsAffected()
}

func TestPGManagedBundleBindingReadsOnlyReferencedIDs(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	_, err := p.Store.DB.Pool.Exec(t.Context(), `INSERT INTO groups(id,name,description) SELECT 'binding-g-'||i,'Binding group '||i,repeat('x',2048) FROM generate_series(1,10000)i;
	 INSERT INTO proxies(id,name,type,host,port,username,password_cipher,enabled) SELECT 'binding-p-'||i,'Binding proxy '||i,'http','127.0.0.1',9,repeat('u',512),decode('ff','hex'),false FROM generate_series(1,10000)i`)
	if err != nil {
		t.Fatal(err)
	}
	trace := new(bundleBindingReadTrace)
	config := p.Store.DB.Pool.Config()
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	previous := p.Store.DB.Pool
	p.Store.DB.Pool = pool
	defer func() { p.Store.DB.Pool = previous }()
	bundle := managedAccountImportBundle{Accounts: []managedAccountBackup{{Groups: []string{"foreign", "foreign"}, Proxies: []string{"binding-p-2"}}}}
	g, pids, err := p.managedBundleBindingIDs(t.Context(), bundle, map[string]string{"foreign": "binding-g-1", "unused": "missing"}, nil)
	if err != nil || !reflect.DeepEqual(g, map[string]bool{"binding-g-1": true}) || !reflect.DeepEqual(pids, map[string]bool{"binding-p-2": true}) {
		t.Fatal("unrelated or missing resources leaked into reference result", err)
	}
	trace.mu.Lock()
	rows, queries := trace.rows, trace.queries
	trace.mu.Unlock()
	if rows != 2 || queries != 2 {
		t.Fatalf("reference validation hydrated unrelated resources: rows=%d queries=%d", rows, queries)
	}
	p.Proxies.mu.RLock()
	indexed := len(p.Proxies.index)
	p.Proxies.mu.RUnlock()
	if indexed != 0 {
		t.Fatal("reference validation rewrote gateway proxy index")
	}
	before := pool.Stat().AcquireCount()
	if _, _, err := p.managedBundleBindingIDs(t.Context(), managedAccountImportBundle{}, map[string]string{"unused": "missing"}, nil); err != nil || pool.Stat().AcquireCount() != before {
		t.Fatal("unreferenced mapping caused database reads", err)
	}
	if _, err := pool.Exec(t.Context(), "ALTER TABLE groups RENAME TO unavailable_bundle_groups"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.managedBundleBindingIDs(t.Context(), bundle, nil, nil); err == nil {
		t.Fatal("referenced table failure was hidden")
	}
}

// Real authenticated HTTP; unrelated catalog size is independent of the
// one-account request. No upstream provider or real credentials are involved.
func TestPerformanceBundleBindingImport(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("isolated synthetic performance runner required")
	}
	p, handler := adminPGTestProxy(t)
	deny := new(localWriteNoUpstream)
	p.Client = &http.Client{Transport: deny}
	_, err := p.Store.DB.Pool.Exec(t.Context(), `INSERT INTO groups(id,name,description) SELECT 'scale-g-'||i,'Scale group '||i,repeat('d',2048) FROM generate_series(1,10000)i;
	 INSERT INTO proxies(id,name,type,host,port,username,enabled) SELECT 'scale-p-'||i,'Scale proxy '||i,'http','127.0.0.1',9,repeat('u',512),false FROM generate_series(1,10000)i`)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	client := &http.Client{Timeout: 20 * time.Second}
	defer client.CloseIdleConnections()
	for _, mode := range []string{"no_references", "mapped_references", "preview"} {
		for sample := 0; sample < 3; sample++ {
			name := fmt.Sprintf("binding-scale-%s-%d", mode, sample)
			credentials, _ := json.Marshal(map[string]string{"access_token": "synthetic-" + name})
			account := managedAccountBackup{Name: name, Enabled: false, Concurrency: 1, Weight: 1, WindowDays: 1, Credentials: credentials}
			if mode != "no_references" {
				account.Groups, account.Proxies = []string{"foreign-g"}, []string{"foreign-p"}
			}
			body := map[string]any{"data": managedAccountImportBundle{Type: "codex-agent-accounts", Version: 1, Accounts: []managedAccountBackup{account}}, "group_mapping": map[string]string{"foreign-g": "scale-g-1"}, "proxy_mapping": map[string]string{"foreign-p": "scale-p-1"}, "dry_run": mode == "preview"}
			raw, _ := json.Marshal(body)
			req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/accounts/import-bundle", bytes.NewReader(raw))
			req.Header.Set("Authorization", "Bearer "+webTestKey)
			req.Header.Set("Content-Type", "application/json")
			start := time.Now()
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			data, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			elapsed := float64(time.Since(start).Microseconds()) / 1000
			var report ImportReport
			failure := ""
			if readErr != nil || resp.StatusCode != 200 || json.Unmarshal(data, &report) != nil || report.Failed != 0 || len(report.Results) != 1 {
				failure = "response_contract"
			}
			if mode == "preview" {
				if _, err := p.Store.Get(name); !errors.Is(err, errNotFound) {
					failure = "preview_wrote_account"
				}
			} else {
				persisted, err := p.Store.Get(name)
				if err != nil || persisted.Enabled || !strings.HasPrefix(persisted.AccessToken, "synthetic-") {
					failure = "persisted_contract"
				}
				if mode == "mapped_references" && (!reflect.DeepEqual(persisted.Groups, []string{"scale-g-1"}) || !reflect.DeepEqual(persisted.Proxies, []string{"scale-p-1"})) {
					failure = "mapping_contract"
				}
			}
			performanceSLOEmit(t, "ACCOUNT_WRITE", map[string]any{"name": "bundle_binding_" + mode, "method": "POST", "path": "/api/accounts/import-bundle", "sample": map[string]any{"ms": elapsed, "status": resp.StatusCode, "error": failure}, "threshold_ms": 1000, "slo_pass": failure == "" && elapsed < 1000, "sample_index": sample, "catalog_groups": 10000, "catalog_proxies": 10000, "import_accounts": 1})
			if failure != "" {
				t.Fatal(failure)
			}
			if elapsed >= 1000 {
				t.Errorf("bundle binding import exceeded1s: %.3fms", elapsed)
			}
		}
	}
}
