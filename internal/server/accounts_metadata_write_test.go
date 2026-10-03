package server

import (
	"errors"
	"reflect"
	"testing"
)

func TestPGMetadataUnchangedBindingsAvoidWrites(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	g1, err := p.Groups.Create("meta-one", "")
	if err != nil {
		t.Fatal(err)
	}
	g2, err := p.Groups.Create("meta-two", "")
	if err != nil {
		t.Fatal(err)
	}
	px1, err := p.Proxies.Create(ProxyEntry{Name: "meta-proxy-one", Type: "http", Host: "127.0.0.1", Port: 1234})
	if err != nil {
		t.Fatal(err)
	}
	px2, err := p.Proxies.Create(ProxyEntry{Name: "meta-proxy-two", Type: "http", Host: "127.0.0.1", Port: 1235})
	if err != nil {
		t.Fatal(err)
	}
	a := managedTestAccount(t, p, "metadata-bindings", func(a *Account) { a.Groups = []string{g1.ID, g2.ID}; a.Proxies = []string{px1.ID, px2.ID} })
	if _, err := p.Store.DB.Pool.Exec(t.Context(), `CREATE FUNCTION reject_unchanged_binding_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'unexpected association write';END $$;
 CREATE TRIGGER reject_group_write BEFORE INSERT OR DELETE OR UPDATE ON account_groups FOR EACH STATEMENT EXECUTE FUNCTION reject_unchanged_binding_write();
 CREATE TRIGGER reject_proxy_write BEFORE INSERT OR DELETE OR UPDATE ON account_proxies FOR EACH STATEMENT EXECUTE FUNCTION reject_unchanged_binding_write()`); err != nil {
		t.Fatal(err)
	}
	updated, err := p.Store.UpdateAccountVersion(a.ID, a.Version, func(v *Account) error {
		v.Notes = "metadata-only"
		v.Enabled = false
		v.Groups = []string{g2.ID, g1.ID, g2.ID}
		v.Proxies = []string{px1.ID, px2.ID, px1.ID}
		return nil
	})
	if err != nil || updated.Version != a.Version+1 || updated.Notes != "metadata-only" || updated.Enabled {
		t.Fatalf("unchanged binding metadata failed: %+v %v", updated, err)
	}
	var audits int
	if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_logs WHERE entity_id=$1 AND action='account.update'", a.ID).Scan(&audits); err != nil || audits < 1 {
		t.Fatalf("audit omitted: %d %v", audits, err)
	}
	if _, err := p.Store.UpdateAccountVersion(a.ID, a.Version, func(v *Account) error { v.Notes = "stale"; return nil }); !errors.Is(err, errVersionConflict) {
		t.Fatalf("stale CAS changed: %v", err)
	}
	if _, err := p.Store.DB.Pool.Exec(t.Context(), "DROP TRIGGER reject_proxy_write ON account_proxies"); err != nil {
		t.Fatal(err)
	}
	// Mutating the callback slice in place must still be detected. The prior
	// relation snapshot cannot alias its caller-editable slice.
	updated, err = p.Store.UpdateAccountVersion(a.ID, updated.Version, func(v *Account) error { v.Proxies[0], v.Proxies[1] = v.Proxies[1], v.Proxies[0]; return nil })
	if err != nil || !reflect.DeepEqual(updated.Proxies, []string{px2.ID, px1.ID}) {
		t.Fatalf("proxy order ignored: %+v %v", updated.Proxies, err)
	}
	if _, err := p.Store.DB.Pool.Exec(t.Context(), "DROP TRIGGER reject_group_write ON account_groups"); err != nil {
		t.Fatal(err)
	}
	_, err = p.Store.UpdateAccountVersion(a.ID, updated.Version, func(v *Account) error { v.Groups = []string{"missing-fk"}; return nil })
	if err == nil {
		t.Fatal("changed invalid relation bypassed FK")
	}
	current, err := p.Store.GetMetadata(a.ID)
	if err != nil || current.Version != updated.Version || !reflect.DeepEqual(current.Groups, updated.Groups) {
		t.Fatalf("failed FK partially committed: %+v %v", current, err)
	}
}

func TestPGMetadataNoBindingWriteAuditStillAtomic(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	a := managedTestAccount(t, p, "metadata-audit", nil)
	if _, err := p.Store.DB.Pool.Exec(t.Context(), `CREATE FUNCTION reject_metadata_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='account.update' THEN RAISE EXCEPTION 'audit rejected';END IF;RETURN NEW;END $$;
 CREATE TRIGGER reject_metadata_audit BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_metadata_audit()`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Store.UpdateAccountVersion(a.ID, a.Version, func(v *Account) error { v.Enabled = false; v.Notes = "must rollback"; return nil }); err == nil {
		t.Fatal("failed audit accepted")
	}
	current, err := p.Store.GetMetadata(a.ID)
	if err != nil || current.Version != a.Version || current.Enabled != a.Enabled || current.Notes != a.Notes {
		t.Fatalf("failed audit partially committed: %+v %v", current, err)
	}
}
