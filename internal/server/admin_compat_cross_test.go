package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"
)

func exerciseManagedCompatCrossCredentials(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	ctx := context.Background()
	row, err := p.compatMutateAccount(ctx, "", 0, func(a *Account, record *adminCompatRecord) error {
		a.Name, a.AccessToken, a.RefreshToken, a.AccountID = "cross-credentials", "access-first", "refresh-first", "workspace-first"
		record.Sub2["credentials"] = json.RawMessage(`{"id_token":"identity-first","organization_id":"organization-first"}`)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertTokens := func(row adminCompatAccount, wantID string) {
		t.Helper()
		var credentials map[string]json.RawMessage
		if json.Unmarshal(row.Record.Sub2["credentials"], &credentials) != nil {
			t.Fatal("missing Sub2API credential metadata")
		}
		for _, encoded := range []json.RawMessage{row.Record.CPA["id_token"], credentials["id_token"]} {
			var value string
			if json.Unmarshal(encoded, &value) != nil || value != wantID {
				t.Fatalf("cross-protocol identity token does not match %q", wantID)
			}
		}
		for key, want := range map[string]string{"access_token": row.Account.AccessToken, "refresh_token": row.Account.RefreshToken, "account_id": row.Account.AccountID, "chatgpt_account_id": row.Account.AccountID, "client_id": row.Account.ClientID} {
			var value string
			if json.Unmarshal(credentials[key], &value) != nil || value != want {
				t.Fatalf("cross-protocol credential field %s is stale", key)
			}
		}
	}
	assertTokens(row, "identity-first")
	first := row
	row, err = p.compatMutateAccount(ctx, row.Account.ID, row.Account.Version, func(a *Account, record *adminCompatRecord) error {
		a.AccessToken, a.RefreshToken, a.AccountID = "access-second", "refresh-second", "workspace-second"
		record.CPA["id_token"] = json.RawMessage(`"identity-second"`)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertTokens(row, "identity-second")
	if row.Account.CredentialVersion <= first.Account.CredentialVersion || row.Account.OwnerRevision == first.Account.OwnerRevision {
		t.Fatal("CPA credential replacement failed to advance credential/owner generations")
	}
	second := row
	row, err = p.compatMutateAccount(ctx, row.Account.ID, row.Account.Version, func(a *Account, record *adminCompatRecord) error {
		a.Notes = "metadata only"
		record.CPA["note"] = json.RawMessage(`"metadata only"`)
		record.Sub2["extra"] = json.RawMessage(`{"external_version":3}`)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertTokens(row, "identity-second")
	if row.Account.Version <= second.Account.Version || row.Account.CredentialVersion != second.Account.CredentialVersion || row.Account.OwnerRevision != second.Account.OwnerRevision {
		t.Fatal("metadata edit changed credential/owner generations or lost optimistic versioning")
	}
	third := row
	_, err = p.compatMutateAccount(ctx, row.Account.ID, row.Account.Version, func(a *Account, record *adminCompatRecord) error {
		a.Name, a.AccessToken = "must-roll-back", "must-roll-back-token"
		record.FileName = "must-roll-back.json"
		record.CPA["id_token"] = json.RawMessage(`"identity-conflict-cpa"`)
		record.Sub2["credentials"] = json.RawMessage(`{"id_token":"identity-conflict-sub2"}`)
		return nil
	})
	if !errors.Is(err, errConflict) {
		t.Fatalf("conflicting identity tokens were accepted: %v", err)
	}
	row, err = p.compatGetAccount(ctx, row.Account.ID)
	if err != nil || !reflect.DeepEqual(row, third) {
		t.Fatal("conflicting identity update partially changed stored account", err)
	}
	_, err = p.compatMutateAccount(ctx, row.Account.ID, second.Account.Version, func(a *Account, record *adminCompatRecord) error {
		a.Enabled = false
		record.CPA["id_token"] = json.RawMessage(`"stale-write-token"`)
		return nil
	})
	if !errors.Is(err, errVersionConflict) {
		t.Fatalf("stale metadata version replaced current token: %v", err)
	}
	row, err = p.compatMutateAccount(ctx, row.Account.ID, row.Account.Version, func(a *Account, record *adminCompatRecord) error {
		delete(record.CPA, "id_token")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var credentials map[string]json.RawMessage
	_ = json.Unmarshal(row.Record.Sub2["credentials"], &credentials)
	if len(row.Record.CPA["id_token"]) != 0 || len(credentials["id_token"]) != 0 || row.Account.AccessToken != third.Account.AccessToken {
		t.Fatal("explicit ID token removal did not clear both compatibility views")
	}
}

func TestManagedCompatCrossCredentials(t *testing.T) {
	exerciseManagedCompatCrossCredentials(t, webTestSetup)
}

func TestPGManagedCompatCrossCredentials(t *testing.T) {
	exerciseManagedCompatCrossCredentials(t, adminPGTestProxy)
}

func exerciseManagedCompatRelationValidation(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	ctx := context.Background()
	row := compatStoreFixture(t, p, "relationship-contract")
	group, proxy, _ := managedTestBindings(t, p)
	for _, missing := range []string{"group", "proxy"} {
		t.Run(missing, func(t *testing.T) {
			_, err := p.compatMutateAccount(ctx, row.Account.ID, row.Account.Version, func(a *Account, record *adminCompatRecord) error {
				a.Name, a.AccessToken = "must-rollback-relationship", "must-rollback-relationship-token"
				record.FileName = "must-rollback-relationship.json"
				if missing == "group" {
					a.Groups = []string{"missing-group"}
				} else {
					a.Proxies = []string{"missing-proxy"}
				}
				return nil
			})
			if !errors.Is(err, errInvalid) {
				t.Fatalf("dangling %s relation accepted or misclassified: %v", missing, err)
			}
			current, err := p.compatGetAccount(ctx, row.Account.ID)
			if err != nil || !reflect.DeepEqual(current, row) {
				t.Fatal("invalid relation partially committed tokens or aliases", err)
			}
		})
	}
	updated, err := p.compatMutateAccount(ctx, row.Account.ID, row.Account.Version, func(a *Account, record *adminCompatRecord) error {
		a.Groups, a.Proxies = []string{group.ID}, []string{proxy.ID}
		return nil
	})
	if err != nil || !reflect.DeepEqual(updated.Account.Groups, []string{group.ID}) || !reflect.DeepEqual(updated.Account.Proxies, []string{proxy.ID}) {
		t.Fatal("valid native relationships did not persist", err)
	}
	if updated.Account.CredentialVersion != row.Account.CredentialVersion {
		t.Fatal("relationship edit invalidated credential generation")
	}
}

func TestManagedCompatRelationValidation(t *testing.T) {
	exerciseManagedCompatRelationValidation(t, webTestSetup)
}

func TestPGManagedCompatRelationValidation(t *testing.T) {
	exerciseManagedCompatRelationValidation(t, adminPGTestProxy)
}
