package server

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func bulkImportFixtureItems(count int, prefix string) []ImportItem {
	items := make([]ImportItem, count)
	for i := range items {
		name := fmt.Sprintf("%s-%04d", prefix, i)
		raw, _ := json.Marshal(map[string]any{"access_token": "synthetic-" + name, "refresh_token": "refresh-" + name, "account_id": "workspace-" + name, "client_id": "client", "expires_at": int64(2000000000)})
		items[i] = ImportItem{Name: name, Credentials: raw}
	}
	return items
}

func TestPGNewAccountBulkImportPreservesMetadataAndPreview(t *testing.T) {
	s := accountPGTestStore(t)
	group, err := NewPGGroupStore(s.DB).Create("bulk-group", "")
	if err != nil {
		t.Fatal(err)
	}
	proxies, err := NewPGProxyStore(s.DB)
	if err != nil {
		t.Fatal(err)
	}
	one, err := proxies.Create(ProxyEntry{Name: "bulk-first", Type: "http", Host: "127.0.0.1", Port: 1234})
	if err != nil {
		t.Fatal(err)
	}
	two, err := proxies.Create(ProxyEntry{Name: "bulk-second", Type: "http", Host: "127.0.0.1", Port: 1235})
	if err != nil {
		t.Fatal(err)
	}
	defaults := AccountDefaults{Groups: []string{group.ID}, Proxies: []string{two.ID, one.ID, two.ID}}
	items := bulkImportFixtureItems(100, "fresh")
	items = append(items, ImportItem{Name: "invalid", Credentials: json.RawMessage(`{}`)})
	report, err := s.ImportDetailed(items, defaults, ImportOptions{DryRun: true})
	if err != nil || report.Added != 100 || report.Failed != 1 {
		t.Fatalf("preview: %+v %v", report, err)
	}
	var count int
	for _, table := range []string{"accounts", "account_credentials", "account_groups", "account_proxies"} {
		if err := s.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("preview persisted %s=%d %v", table, count, err)
		}
	}
	report, err = s.ImportDetailed(items, defaults, ImportOptions{})
	if err != nil || report.Added != 100 || report.Failed != 1 {
		t.Fatalf("import: %+v %v", report, err)
	}
	active, accounts, err := s.List()
	if err != nil || len(accounts) != 100 || active != items[0].Name {
		t.Fatalf("active/order: %q %d %v", active, len(accounts), err)
	}
	for i, a := range accounts {
		if a.Name != items[i].Name || a.Version != 1 || a.CredentialVersion != 1 || a.AccessToken != "synthetic-"+a.Name || a.RefreshToken != "refresh-"+a.Name || a.AccountID != "workspace-"+a.Name || !validGatewayOwnerRevision(a.OwnerRevision) || !reflect.DeepEqual(a.Groups, defaults.Groups) || !reflect.DeepEqual(a.Proxies, []string{two.ID, one.ID}) {
			t.Fatalf("metadata/credentials changed: index=%d account=%+v", i, accountMetadata(a))
		}
	}
	if err := s.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_logs WHERE action='account.import'").Scan(&count); err != nil || count != 100 {
		t.Fatalf("audit rows=%d %v", count, err)
	}
	duplicate, err := s.ImportDetailed(items, defaults, ImportOptions{})
	if err != nil || duplicate.Duplicate != 100 || duplicate.Failed != 1 || duplicate.Added != 0 {
		t.Fatalf("duplicate fallback: %+v %v", duplicate, err)
	}
}

func TestPGNewAccountBulkImportAtomicFailures(t *testing.T) {
	for _, mode := range []string{"invalid_group", "audit_rejection"} {
		t.Run(mode, func(t *testing.T) {
			s := accountPGTestStore(t)
			defaults := AccountDefaults{}
			if mode == "invalid_group" {
				defaults.Groups = []string{"missing-group"}
			} else {
				if _, err := s.DB.Pool.Exec(t.Context(), `CREATE FUNCTION reject_bulk_import_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='account.import' THEN RAISE EXCEPTION 'synthetic audit rejection';END IF;RETURN NEW;END $$;
 CREATE TRIGGER reject_bulk_import_audit BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_bulk_import_audit()`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.ImportDetailed(bulkImportFixtureItems(100, "rollback"), defaults, ImportOptions{}); err == nil {
				t.Fatal("invalid import committed")
			}
			for _, table := range []string{"accounts", "account_credentials", "account_groups", "account_proxies"} {
				var count int
				if err := s.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("partial %s=%d %v", table, count, err)
				}
			}
		})
	}
}

func TestPGNewAccountBulkImportOrderedFallback(t *testing.T) {
	s := accountPGTestStore(t)
	items := bulkImportFixtureItems(3, "ordered")
	items[1].Name = items[0].Name
	items[2].Credentials = items[0].Credentials
	report, err := s.ImportDetailed(items, AccountDefaults{}, ImportOptions{})
	if err != nil || report.Added != 1 || report.Failed != 1 || report.Duplicate != 1 || report.Results[1].Code != "name_conflict" || report.Results[2].ID != report.Results[0].ID {
		t.Fatalf("ordered conflicts changed: %+v %v", report, err)
	}
	newItems := bulkImportFixtureItems(3, "different")
	newItems[1].Name = items[0].Name
	report, err = s.ImportDetailed(newItems, AccountDefaults{}, ImportOptions{Replace: true})
	if err != nil || report.Added != 2 || report.Replaced != 1 {
		t.Fatalf("replacement fallback: %+v %v", report, err)
	}
}
