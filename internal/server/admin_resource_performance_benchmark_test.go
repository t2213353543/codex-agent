package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// This harness uses only pre-existing handlers so that the same benchmark file
// can measure both frozen implementations with identical synthetic fixtures.
func systemResourceBenchmarkProxy(b *testing.B, accounts, keyCount int) *Proxy {
	b.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		b.Skip("TEST_DATABASE_URL required for synthetic PostgreSQL benchmark")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		b.Fatal(err)
	}
	schema := "system_resources_" + newID("")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		admin.Close(ctx)
		b.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		b.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := OpenDatabase(ctx, u.String(), base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)), "synthetic-system-benchmark")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close(); _, _ = admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); _ = admin.Close(ctx) })
	if err := db.Migrate(ctx); err != nil {
		b.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		if _, err := db.Pool.Exec(ctx, sql, args...); err != nil {
			b.Fatal(err)
		}
	}
	exec("INSERT INTO groups(id,name) SELECT 'g_'||i,'Synthetic group '||i FROM generate_series(1,100) i")
	exec("INSERT INTO proxies(id,name,type,host,port,enabled) SELECT 'p_'||i,'Synthetic proxy '||i,'http','192.0.2.1',9,false FROM generate_series(1,100) i")
	exec("INSERT INTO accounts(id,display_name,enabled,notes,tags,created_at,updated_at) SELECT 'a_'||i,'Synthetic account '||i,false,repeat('fixture ',32),ARRAY['synthetic'],1800000000,1800000000 FROM generate_series(1,$1::int) i", accounts)
	encrypted, err := db.Encrypt([]byte(`{"access_token":"synthetic-unusable"}`))
	if err != nil {
		b.Fatal(err)
	}
	exec("INSERT INTO account_credentials(account_id,credentials_cipher,token_fingerprint,encryption_key_id,updated_at) SELECT id,$1,$2,'synthetic-system-benchmark',1800000000 FROM accounts", encrypted, []byte("synthetic"))
	exec("INSERT INTO account_groups(account_id,group_id) SELECT 'a_'||i,'g_'||((i-1)%100+1) FROM generate_series(1,$1::int) i", accounts)
	exec("INSERT INTO account_proxies(account_id,proxy_id) SELECT 'a_'||i,'p_'||((i-1)%100+1) FROM generate_series(1,$1::int) i", accounts)
	keys, err := NewPGKeyStore(db)
	if err != nil {
		b.Fatal(err)
	}
	if err := keys.store.Modify(func(s *keyState) error {
		for i := 1; i <= keyCount; i++ {
			s.Keys = append(s.Keys, APIKey{ID: fmt.Sprintf("k_%d", i), Name: fmt.Sprintf("Synthetic %d", i), Key: fmt.Sprintf("synthetic-never-use-%d", i), Version: 1, Groups: []string{fmt.Sprintf("g_%d", (i-1)%100+1)}})
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	exec("INSERT INTO usage_totals(scope,subject,requests,tokens,errors,last_at) SELECT 'key','k_'||i,100,10000,5,1800000000 FROM generate_series(1,$1::int) i", keyCount)
	proxies, err := NewPGProxyStore(db)
	if err != nil {
		b.Fatal(err)
	}
	metrics, err := NewPGMetrics(db)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := db.initializeAdminKey(ctx, webTestKey); err != nil {
		b.Fatal(err)
	}
	exec("ANALYZE")
	return &Proxy{Store: Store{DB: db}, Groups: NewPGGroupStore(db), Proxies: proxies, Keys: keys, Settings: NewPGSettingsStore(db), Metrics: metrics, AdminKey: webTestKey}
}

func BenchmarkSystemPGResourceEndpoints(b *testing.B) {
	for _, scale := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("accounts_%d_keys_100", scale), func(b *testing.B) {
			p := systemResourceBenchmarkProxy(b, scale, 100)
			handler := p.adminRoutes()
			for _, endpoint := range []string{"groups", "proxies", "keys", "overview"} {
				b.Run(endpoint, func(b *testing.B) {
					samples := make([]float64, 0, b.N)
					before := p.Store.DB.Pool.Stat().AcquireCount()
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						start := time.Now()
						r := httptest.NewRequest(http.MethodGet, "/api/"+endpoint, nil)
						r.Header.Set("Authorization", "Bearer "+webTestKey)
						w := httptest.NewRecorder()
						handler.ServeHTTP(w, r)
						samples = append(samples, float64(time.Since(start).Nanoseconds())/1e6)
						if w.Code != 200 {
							b.Fatalf("%s: %d %s", endpoint, w.Code, w.Body.String())
						}
					}
					b.StopTimer()
					b.ReportMetric(float64(p.Store.DB.Pool.Stat().AcquireCount()-before)/float64(b.N), "db_reads/op")
					sort.Float64s(samples)
					b.ReportMetric(samples[(len(samples)-1)/2], "p50_ms")
					b.ReportMetric(samples[(len(samples)*95+99)/100-1], "p95_ms")
					raw, _ := json.Marshal(map[string]any{"name": b.Name(), "samples_ms": samples})
					b.Logf("SYSTEM_SAMPLES %s", raw)
				})
			}
		})
	}
}
