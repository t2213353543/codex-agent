package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
)

func TestSub2ResourceDeletePreservesNativeErrorStatus(t *testing.T) {
	for _, group := range []bool{false, true} {
		for _, corrupt := range []bool{false, true} {
			t.Run(fmt.Sprintf("group=%t/corrupt=%t", group, corrupt), func(t *testing.T) {
				p, _ := webTestSetup(t)
				path := p.Proxies.store.path
				if group {
					path = p.Groups.store.path
				}
				if corrupt {
					if err := os.WriteFile(path, []byte("invalid-json"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				response := httptest.NewRecorder()
				p.sub2ResourceDelete(response, httptest.NewRequest(http.MethodDelete, "/api/v1/admin/proxies/1", nil), "missing", group)
				want := http.StatusNotFound
				if corrupt {
					want = http.StatusInternalServerError
				}
				var body struct{ Code int }
				if response.Code != want || json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Code != want {
					t.Fatalf("native failure became a false association conflict: status=%d body=%s", response.Code, response.Body.String())
				}
			})
		}
	}
}

func TestPGSub2ResourceLookupNumericParity(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	ids := []string{"g_native", "p_native", "中文-资源", "f_000000000000000000000000", "resource-quote-'", "z"}
	for i, id := range ids {
		if _, err := p.Store.DB.Pool.Exec(t.Context(), "INSERT INTO groups(id,name,description) VALUES($1,$2,$3)", id, "lookup-"+strconv.Itoa(i), "selected-description"); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Store.DB.Pool.Exec(t.Context(), "INSERT INTO proxies(id,name,type,host,port,password_cipher) VALUES($1,$2,'http','127.0.0.1',1234,decode('01','hex'))", id, "lookup-"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ids {
		ref := strconv.FormatInt(compatNumericID(id), 10)
		group, _, err := p.sub2ResourceMetadataPG(t.Context(), ref, true)
		if err != nil || group.ID != id || group.Description != "selected-description" {
			t.Fatalf("group numeric ID parity: %q %v", id, err)
		}
		_, proxy, err := p.sub2ResourceMetadataPG(t.Context(), ref, false)
		if err != nil || proxy.ID != id || !proxy.HasPassword || proxy.Password != "" {
			t.Fatalf("proxy numeric ID/metadata parity: %q %v", id, err)
		}
	}
	if _, _, err := p.sub2ResourceMetadataPG(t.Context(), "281474976710657", true); !errors.Is(err, errNotFound) {
		t.Fatal("out-of-hash-range ID did not remain missing:", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := p.sub2ResourceMetadataPG(ctx, strconv.FormatInt(compatNumericID(ids[0]), 10), false); err == nil {
		t.Fatal("cancelled lookup succeeded")
	}
}

func TestPGGroupDeleteUnreferencedDoesNotRewriteKeys(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	group, err := p.Groups.Create("unreferenced", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Keys.Create("unrelated-key", nil); err != nil {
		t.Fatal(err)
	}
	var version int64
	if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT version FROM settings WHERE key='gateway_keys'").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Store.DB.Pool.Exec(t.Context(), `CREATE FUNCTION reject_unrelated_key_rewrite() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.key='gateway_keys' THEN RAISE EXCEPTION 'unrelated key document rewrite'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER reject_unrelated_key_rewrite BEFORE INSERT OR UPDATE ON settings FOR EACH ROW EXECUTE FUNCTION reject_unrelated_key_rewrite()`); err != nil {
		t.Fatal(err)
	}
	if err := p.Groups.Delete(group.ID); err != nil {
		t.Fatal("unreferenced deletion rewrote the entire key document:", err)
	}
	var after, audit int64
	if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT version,(SELECT count(*) FROM audit_logs WHERE action='group.delete' AND entity_id=$1) FROM settings WHERE key='gateway_keys'", group.ID).Scan(&after, &audit); err != nil || after != version || audit != 1 {
		t.Fatalf("deletion changed key version or lost its own audit: version=%d/%d audit=%d err=%v", version, after, audit, err)
	}
}
