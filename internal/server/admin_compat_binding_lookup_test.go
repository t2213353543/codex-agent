package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSub2BindingLookupPreservesResolution(t *testing.T) {
	exerciseSub2BindingLookup(t, webTestSetup)
}
func TestPGSub2BindingLookupPreservesResolution(t *testing.T) {
	exerciseSub2BindingLookup(t, adminPGTestProxy)
}
func exerciseSub2BindingLookup(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	g, err := p.Groups.Create("compat-binding", "description")
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.Proxies.Create(ProxyEntry{Name: "compat-binding", Type: "http", Host: "127.0.0.1", Port: 9})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Proxies.UpdateVersion(v.ID, 0, func(v *ProxyEntry) error { v.Enabled = false; return nil })
	if err != nil {
		t.Fatal(err)
	}
	groups, err := p.sub2BindingIDs(t.Context(), []int64{compatNumericID(g.ID), compatNumericID(g.ID), -1}, true)
	if err != nil || !reflect.DeepEqual(groups, map[int64]string{compatNumericID(g.ID): g.ID}) {
		t.Fatal("group identity resolution changed", groups, err)
	}
	proxies, err := p.sub2BindingIDs(t.Context(), []int64{compatNumericID(v.ID)}, false)
	if err != nil || proxies[compatNumericID(v.ID)] != v.ID {
		t.Fatal("disabled proxy was rejected", err)
	}
	input := map[string]json.RawMessage{"name": sub2Raw("sub2-bound"), "platform": sub2Raw("openai"), "type": sub2Raw("oauth"), "credentials": sub2Raw(map[string]string{"access_token": "synthetic-sub2-binding"}), "group_ids": sub2Raw([]int64{compatNumericID(g.ID), compatNumericID(g.ID)}), "proxy_id": sub2Raw(compatNumericID(v.ID))}
	a, err := p.sub2ApplyAccount(t.Context(), "", input)
	if err != nil || !reflect.DeepEqual(a.Account.Groups, []string{g.ID}) || !reflect.DeepEqual(a.Account.Proxies, []string{v.ID}) {
		t.Fatal("write binding changed", err)
	}
	if _, err := p.sub2ApplyAccount(t.Context(), a.Account.ID, map[string]json.RawMessage{"group_ids": sub2Raw([]int64{-1})}); !errors.Is(err, errInvalid) {
		t.Fatal("unknown group not rejected", err)
	}
	if _, err := p.sub2ApplyAccount(t.Context(), a.Account.ID, map[string]json.RawMessage{"proxy_id": sub2Raw(int64(281474976710657))}); !errors.Is(err, errInvalid) {
		t.Fatal("unknown proxy not rejected", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.sub2BindingIDs(ctx, nil, false); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
}

func TestPGSub2BindingLookupReturnsOnlyMatchedIDs(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	if _, err := p.Store.DB.Pool.Exec(t.Context(), `INSERT INTO groups(id,name,description) SELECT 'compat-g-'||i,'Compat group '||i,repeat('x',4096) FROM generate_series(1,10000)i;
 INSERT INTO proxies(id,name,type,host,port,username,password_cipher,enabled) SELECT 'compat-p-'||i,'Compat proxy '||i,'http','127.0.0.1',9,repeat('x',1024),decode('ff','hex'),false FROM generate_series(1,10000)i`); err != nil {
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
	for _, group := range []bool{true, false} {
		id := "compat-p-1"
		if group {
			id = "compat-g-1"
		}
		found, err := p.sub2BindingIDs(t.Context(), []int64{compatNumericID(id)}, group)
		if err != nil || found[compatNumericID(id)] != id || len(found) != 1 {
			t.Fatal("identity lookup mismatch", found, err)
		}
	}
	trace.mu.Lock()
	rows, queries := trace.rows, trace.queries
	trace.mu.Unlock()
	if rows != 2 || queries != 2 {
		t.Fatalf("unrelated metadata returned: %d rows %d queries", rows, queries)
	}
	p.Proxies.mu.RLock()
	size := len(p.Proxies.index)
	p.Proxies.mu.RUnlock()
	if size != 0 {
		t.Fatal("binding query rebuilt gateway index")
	}
	before := pool.Stat().AcquireCount()
	if _, err := p.sub2BindingIDs(t.Context(), nil, true); err != nil || before != pool.Stat().AcquireCount() {
		t.Fatal("empty binding query touched DB", err)
	}
	if _, err := pool.Exec(t.Context(), "ALTER TABLE proxies RENAME TO unavailable_compat_proxies"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.sub2BindingIDs(t.Context(), []int64{1}, false); err == nil {
		t.Fatal("required resource error hidden")
	}
}
