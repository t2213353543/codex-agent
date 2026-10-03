package server

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
)

func duplicateImportItem(name, token, refresh, accountID string) ImportItem {
	raw, _ := json.Marshal(map[string]any{"access_token": token, "refresh_token": refresh, "account_id": accountID, "notes": "incoming ignored metadata", "enabled": false})
	return ImportItem{Name: name, Credentials: raw}
}

func TestPGUnchangedDuplicateBatchNoWrites(t *testing.T) {
	s := accountPGTestStore(t)
	items := bulkImportFixtureItems(3, "unchanged")
	if _, err := s.ImportDetailed(items, AccountDefaults{}, ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	_, before, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	input := []ImportItem{
		duplicateImportItem("other-name", before[2].AccessToken, before[2].RefreshToken, "different-ignored-workspace"),
		duplicateImportItem("same-name", before[0].AccessToken, "", "different-ignored-workspace"),
		duplicateImportItem("repeat-token", before[2].AccessToken, before[2].RefreshToken, before[2].AccountID),
		{Name: "bad", Credentials: json.RawMessage(`{}`)},
	}
	if _, err := s.DB.Pool.Exec(t.Context(), `CREATE FUNCTION reject_duplicate_mutation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'duplicate attempted mutation';END $$;
 CREATE TRIGGER duplicate_accounts_no_write BEFORE INSERT OR UPDATE OR DELETE ON accounts FOR EACH STATEMENT EXECUTE FUNCTION reject_duplicate_mutation();
 CREATE TRIGGER duplicate_credentials_no_write BEFORE INSERT OR UPDATE OR DELETE ON account_credentials FOR EACH STATEMENT EXECUTE FUNCTION reject_duplicate_mutation();
 CREATE TRIGGER duplicate_audit_no_write BEFORE INSERT ON audit_logs FOR EACH STATEMENT EXECUTE FUNCTION reject_duplicate_mutation()`); err != nil {
		t.Fatal(err)
	}
	for _, dry := range []bool{true, false} {
		report, err := s.ImportDetailed(input, AccountDefaults{}, ImportOptions{DryRun: dry, Replace: true})
		if err != nil || report.Duplicate != 3 || report.Failed != 1 || report.Replaced != 0 || report.Added != 0 {
			t.Fatalf("dry=%t duplicate report: %+v %v", dry, report, err)
		}
		for i, expected := range []string{before[2].ID, before[0].ID, before[2].ID} {
			if report.Results[i].ID != expected || report.Results[i].Status != "duplicate" {
				t.Fatalf("ordered duplicate[%d]=%+v want=%s", i, report.Results[i], expected)
			}
		}
	}
	_, after, err := s.List()
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("unchanged batch changed stored snapshot: %v", err)
	}
}

func TestPGUnchangedDuplicateChoosesOriginalFingerprintOrder(t *testing.T) {
	s := accountPGTestStore(t)
	if _, err := s.ImportDetailed(bulkImportFixtureItems(2, "nonunique"), AccountDefaults{}, ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	_, accounts, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	// The schema deliberately permits repeated fingerprints. Match the exact
	// old ORDER BY account_id LIMIT 1 rule, not insertion order or names.
	if _, err := s.DB.Pool.Exec(t.Context(), `UPDATE account_credentials destination SET credentials_cipher=source.credentials_cipher,token_fingerprint=source.token_fingerprint
 FROM account_credentials source WHERE source.account_id=$1 AND destination.account_id=$2`, accounts[0].ID, accounts[1].ID); err != nil {
		t.Fatal(err)
	}
	var want string
	if err := s.DB.Pool.QueryRow(t.Context(), "SELECT account_id FROM account_credentials WHERE token_fingerprint=$1 ORDER BY account_id LIMIT 1", tokenFingerprint(accounts[0].AccessToken)).Scan(&want); err != nil {
		t.Fatal(err)
	}
	input := []ImportItem{duplicateImportItem("first", accounts[0].AccessToken, "", ""), duplicateImportItem("second", accounts[0].AccessToken, "", "")}
	report, err := s.ImportDetailed(input, AccountDefaults{}, ImportOptions{})
	if err != nil || report.Duplicate != 2 || report.Results[0].ID != want || report.Results[1].ID != want {
		t.Fatalf("fingerprint tie changed: %+v want=%s %v", report, want, err)
	}
}

func TestPGUnchangedDuplicateRotationFallsBackInOrder(t *testing.T) {
	s := accountPGTestStore(t)
	if _, err := s.ImportDetailed(bulkImportFixtureItems(2, "rotation"), AccountDefaults{}, ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	_, accounts, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	first := accounts[0]
	input := []ImportItem{
		duplicateImportItem("rotation-one", first.AccessToken, "new-refresh-one", first.AccountID),
		duplicateImportItem("rotation-two", first.AccessToken, "new-refresh-two", first.AccountID),
		duplicateImportItem("unchanged", accounts[1].AccessToken, accounts[1].RefreshToken, accounts[1].AccountID),
	}
	parsed, report, err := prepareImport(input, AccountDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	prior := report
	prior.Results = append([]ImportResult(nil), report.Results...)
	if err := pgx.BeginFunc(t.Context(), s.DB.Pool, func(tx pgx.Tx) error {
		fast, err := s.pgImportUnchangedAccounts(t.Context(), tx, parsed, &report)
		if err != nil {
			return err
		}
		if fast || !reflect.DeepEqual(prior, report) {
			t.Fatal("rotation preflight changed ordered report")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	report, err = s.ImportDetailed(input, AccountDefaults{}, ImportOptions{})
	if err != nil || report.Replaced != 2 || report.Duplicate != 1 {
		t.Fatalf("rotation sequence changed: %+v %v", report, err)
	}
	current, err := s.Get(first.ID)
	if err != nil || current.RefreshToken != "new-refresh-two" || current.CredentialVersion != first.CredentialVersion+2 {
		t.Fatalf("last rotation lost: version=%d refresh=%q err=%v", current.CredentialVersion, current.RefreshToken, err)
	}
	conflict := []ImportItem{duplicateImportItem("conflicting", first.AccessToken, "conflicting-refresh", "wrong-workspace"), duplicateImportItem("known", accounts[1].AccessToken, "", "")}
	report, err = s.ImportDetailed(conflict, AccountDefaults{}, ImportOptions{})
	if err != nil || report.Failed != 1 || report.Results[0].Code != "identity_conflict" || report.Duplicate != 1 {
		t.Fatalf("identity conflict changed: %+v %v", report, err)
	}
}

func TestPGUnchangedDuplicateStillValidatesCiphertext(t *testing.T) {
	for _, kind := range []string{"ciphertext", "owner", "identity_json"} {
		t.Run(kind, func(t *testing.T) {
			s := accountPGTestStore(t)
			items := bulkImportFixtureItems(2, "invalid-secret")
			if _, err := s.ImportDetailed(items, AccountDefaults{}, ImportOptions{}); err != nil {
				t.Fatal(err)
			}
			_, accounts, err := s.List()
			if err != nil {
				t.Fatal(err)
			}
			invalid := []byte("invalid-ciphertext")
			if kind != "ciphertext" {
				secret := fmt.Sprintf(`{"access_token":%q,"refresh_token":%q,"owner_revision":%q}`, accounts[0].AccessToken, accounts[0].RefreshToken, "invalid")
				if kind == "identity_json" {
					secret = fmt.Sprintf(`{"owner_revision":%q,"identity":"invalid-type"}`, accounts[0].OwnerRevision)
				}
				invalid, err = s.DB.Encrypt([]byte(secret))
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.DB.Pool.Exec(t.Context(), "UPDATE account_credentials SET credentials_cipher=$1 WHERE account_id=$2", invalid, accounts[0].ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ImportDetailed(items, AccountDefaults{}, ImportOptions{}); err == nil {
				t.Fatal("duplicate ignored malformed authoritative credentials")
			}
		})
	}
}
