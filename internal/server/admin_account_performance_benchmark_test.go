package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// This fixture is synthetic, schema-isolated, and never opens an upstream
// connection. Keeping the same benchmark file in baseline and working trees
// makes comparisons exercise the actual account summary/list implementations.
func accountPerformanceFixture(b *testing.B) (*Proxy, []Account) {
	b.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		b.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		b.Fatal(err)
	}
	schema := "bench_accounts_" + newID("")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		b.Fatal(err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		b.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	db, err := OpenDatabase(ctx, parsed.String(), base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), "benchmark")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		db.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE")
		_ = admin.Close(context.Background())
	})
	if err = db.Migrate(ctx); err != nil {
		b.Fatal(err)
	}
	now := time.Now().Unix() / 86400 * 86400
	for _, sql := range []string{
		`INSERT INTO accounts(id,display_name,created_at,updated_at) SELECT 'perf-account-'||i, 'Account '||i, 1700000000,1700000000 FROM generate_series(1,500)i`,
		`INSERT INTO groups(id,name,description) SELECT 'perf-group-'||i, 'Group '||i, repeat('Synthetic resource description. ',8) FROM generate_series(1,2000)i`,
		`INSERT INTO proxies(id,name,type,host,port,username) SELECT 'perf-proxy-'||i, 'Proxy '||i, 'http','127.0.0.1',12345,'synthetic' FROM generate_series(1,2000)i`,
		`INSERT INTO account_request_state(account_ref,account_name,last_at,last_log_id,record) SELECT id,display_name,now(),1,jsonb_build_object('id',1,'time',1700000000,'status','ok','model','synthetic','error',repeat('synthetic detail ',250)) FROM accounts`,
	} {
		if _, err = db.Pool.Exec(ctx, sql); err != nil {
			b.Fatal(err)
		}
	}
	if _, err = db.Pool.Exec(ctx, `INSERT INTO usage_daily(bucket,account_ref,account_name,requests,ok,tokens,input,output,cached,latency,latency_n)
		SELECT $1::bigint-d*86400,a.id,a.display_name,100,100,12000,10000,2000,8000,5000,100 FROM accounts a CROSS JOIN generate_series(0,29)d`, now); err != nil {
		b.Fatal(err)
	}
	if _, err = db.Pool.Exec(ctx, "ANALYZE accounts,groups,proxies,account_request_state,usage_daily"); err != nil {
		b.Fatal(err)
	}
	p := &Proxy{Store: Store{DB: db}, Groups: NewPGGroupStore(db), Proxies: &ProxyStore{DB: db}, Metrics: &Metrics{db: db}}
	accounts := make([]Account, 500)
	for i := range accounts {
		accounts[i] = Account{ID: fmt.Sprintf("perf-account-%d", i+1), Name: fmt.Sprintf("Account %d", i+1), Version: 1, CredentialVersion: 1, Enabled: true, WindowStart: now - 29*86400, Notes: strings.Repeat("synthetic notes ", 8)}
	}
	return p, accounts
}

func BenchmarkSystemPGAccountSummaries(b *testing.B) {
	p, accounts := accountPerformanceFixture(b)
	for _, count := range []int{50, 500} {
		for _, bound := range []bool{false, true} {
			b.Run(fmt.Sprintf("page%d/bindings_%t", count, bound), func(b *testing.B) {
				rows := append([]Account(nil), accounts[:count]...)
				if bound {
					for i := range rows {
						rows[i].Groups = []string{fmt.Sprintf("perf-group-%d", i+1)}
						rows[i].Proxies = []string{fmt.Sprintf("perf-proxy-%d", i+1)}
					}
				}
				req := httptest.NewRequest(http.MethodGet, "/api/accounts/manage", nil)
				before := p.Store.DB.Pool.Stat().AcquireCount()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					result, err := p.accountSummaries(req, rows)
					if err != nil || len(result) != count {
						b.Fatalf("summary: %d %v", len(result), err)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(p.Store.DB.Pool.Stat().AcquireCount()-before)/float64(b.N), "db_reads/op")
			})
		}
	}
}

func BenchmarkSystemPGManagedAccountsList(b *testing.B) {
	p, _ := accountPerformanceFixture(b)
	for _, count := range []int{50, 500} {
		b.Run(fmt.Sprintf("page%d", count), func(b *testing.B) {
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/accounts/manage?page_size=%d", count), nil)
			before := p.Store.DB.Pool.Stat().AcquireCount()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				response := httptest.NewRecorder()
				p.handleManagedAccounts(response, req)
				if response.Code != http.StatusOK {
					b.Fatalf("list: %d %s", response.Code, response.Body.String())
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(p.Store.DB.Pool.Stat().AcquireCount()-before)/float64(b.N), "db_reads/op")
		})
	}
}

func BenchmarkSystemPGAccountBundleMetadata(b *testing.B) {
	p, accounts := accountPerformanceFixture(b)
	ciphertext, err := p.Store.DB.Encrypt([]byte("synthetic-benchmark-proxy-password"))
	if err != nil {
		b.Fatal(err)
	}
	if _, err = p.Store.DB.Pool.Exec(context.Background(), "UPDATE proxies SET password_cipher=$1", ciphertext); err != nil {
		b.Fatal(err)
	}
	accounts[0].Groups = []string{"perf-group-1"}
	accounts[0].Proxies = []string{"perf-proxy-1"}
	before := p.Store.DB.Pool.Stat().AcquireCount()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resources, err := p.exportManagedBundleResources(b.Context(), accounts[:1], false)
		if err != nil || len(resources.Proxies) != 1 || !resources.Proxies[0].HasCredentials || resources.Proxies[0].Password != "" {
			b.Fatalf("bundle metadata: %+v %v", resources, err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(p.Store.DB.Pool.Stat().AcquireCount()-before)/float64(b.N), "db_reads/op")
}
