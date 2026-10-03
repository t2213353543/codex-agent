package server

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestPGMixedNewAndUnchangedBatchPreservesOrder(t *testing.T) {
	s := accountPGTestStore(t)
	items := bulkImportFixtureItems(100, "mixed-existing")
	firstReport, err := s.ImportDetailed(items, AccountDefaults{}, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fresh := bulkImportFixtureItems(1, "mixed-new")[0]
	input := []ImportItem{items[99], fresh, items[0], {Name: "bad", Credentials: json.RawMessage(`{}`)}, items[20]}
	beforeActive, _, err := s.ListMetadata()
	if err != nil {
		t.Fatal(err)
	}
	for _, dry := range []bool{true, false} {
		report, err := s.ImportDetailed(input, AccountDefaults{}, ImportOptions{DryRun: dry})
		if err != nil || report.Added != 1 || report.Duplicate != 3 || report.Failed != 1 {
			t.Fatalf("mixed dry=%t %+v err=%v", dry, report, err)
		}
		if report.Results[0].ID != firstReport.Results[99].ID || report.Results[2].ID != firstReport.Results[0].ID || report.Results[4].ID != firstReport.Results[20].ID || report.Results[1].Status != "added" || report.Results[3].Status != "failed" {
			t.Fatalf("report order changed: %+v", report)
		}
		active, accounts, err := s.ListMetadata()
		want := 100
		if !dry {
			want++
		}
		if err != nil || len(accounts) != want || active != beforeActive {
			t.Fatalf("mixed atomic/active: %q %d %v", active, len(accounts), err)
		}
	}
	var imports int
	if err := s.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_logs WHERE action='account.import'").Scan(&imports); err != nil || imports != 101 {
		t.Fatalf("mixed audit %d %v", imports, err)
	}
}

func TestPGMixedImportConflictsFallBackBeforeWrites(t *testing.T) {
	s := accountPGTestStore(t)
	existing := bulkImportFixtureItems(2, "mixed-conflict")
	if _, err := s.ImportDetailed(existing, AccountDefaults{}, ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	fresh := bulkImportFixtureItems(1, "mixed-candidate")[0]
	for _, kind := range []string{"repeated_token", "repeated_name", "existing_name", "refresh_rotation", "corrupt_secret"} {
		t.Run(kind, func(t *testing.T) {
			input := []ImportItem{existing[0], fresh}
			switch kind {
			case "repeated_token":
				input = append(input, existing[0])
			case "repeated_name":
				input[1].Name = input[0].Name
			case "existing_name":
				input[1].Name = existing[1].Name
			case "refresh_rotation":
				var fields map[string]any
				_ = json.Unmarshal(input[0].Credentials, &fields)
				fields["refresh_token"] = "new-refresh"
				input[0].Credentials, _ = json.Marshal(fields)
			}
			parsed, report, err := prepareImport(input, AccountDefaults{})
			if err != nil {
				t.Fatal(err)
			}
			before := report
			before.Results = append([]ImportResult(nil), report.Results...)
			tx, err := s.DB.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if kind == "corrupt_secret" {
				if _, err := tx.Exec(t.Context(), "UPDATE account_credentials SET credentials_cipher='invalid'::bytea WHERE token_fingerprint=$1", tokenFingerprint(parsed[0].AccessToken)); err != nil {
					t.Fatal(err)
				}
			}
			handled, err := s.pgImportUnchangedAccounts(t.Context(), tx, parsed, &report)
			if err != nil || handled || !reflect.DeepEqual(before, report) {
				t.Fatalf("fallback mutated report handled=%t err=%v", handled, err)
			}
			var count int
			if err := tx.QueryRow(t.Context(), "SELECT count(*) FROM accounts").Scan(&count); err != nil || count != 2 {
				t.Fatalf("fallback copied rows before deciding: %d %v", count, err)
			}
		})
	}
}

func TestPGMixedImportAuditFailureRollsBackNewRows(t *testing.T) {
	s := accountPGTestStore(t)
	existing := bulkImportFixtureItems(2, "mixed-rollback")
	if _, err := s.ImportDetailed(existing, AccountDefaults{}, ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Pool.Exec(t.Context(), `CREATE FUNCTION reject_mixed_import_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='account.import' THEN RAISE EXCEPTION 'mixed audit failure';END IF;RETURN NEW;END $$;
 CREATE TRIGGER reject_mixed_import_audit BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_mixed_import_audit()`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportDetailed([]ImportItem{existing[0], bulkImportFixtureItems(1, "mixed-rollback-new")[0]}, AccountDefaults{}, ImportOptions{}); err == nil {
		t.Fatal("mixed failed audit succeeded")
	}
	_, accounts, err := s.ListMetadata()
	if err != nil || len(accounts) != 2 {
		t.Fatalf("partial mixed transaction: %d %v", len(accounts), err)
	}
	// Ensure the failed COPY did not leave an aborted transaction in the pool.
	if err := pgx.BeginFunc(t.Context(), s.DB.Pool, func(tx pgx.Tx) error { _, err := tx.Exec(t.Context(), "SELECT 1"); return err }); err != nil {
		t.Fatal(err)
	}
}
