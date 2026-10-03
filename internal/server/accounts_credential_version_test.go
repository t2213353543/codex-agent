package server

import (
	"encoding/json"
	"os"
	"testing"
)

func TestFileCredentialGenerationPersistsAndOnlyCredentialsAdvance(t *testing.T) {
	p := newTestProxy(t, 1)
	a, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	if a.CredentialVersion < 1 {
		t.Fatal("missing initial credential epoch")
	}
	original := a.CredentialVersion
	if _, err := p.Store.UpdateAccountVersion(a.ID, a.Version, func(a *Account) error {
		a.Name = "renamed"
		a.Notes = "note"
		a.Groups = []string{"group"}
		a.CredentialStatus = "invalid"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a, err = p.Store.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.CredentialVersion != original {
		t.Fatal("metadata changed credential generation")
	}
	for _, field := range []string{"access", "refresh", "client", "workspace", "identity"} {
		prior := a.CredentialVersion
		if err := p.Store.UpdateAccount(a.ID, func(a *Account) error {
			switch field {
			case "access":
				a.AccessToken += "-next"
			case "refresh":
				a.RefreshToken += "-next"
			case "client":
				a.ClientID += "-next"
			case "workspace":
				a.AccountID += "-next"
			case "identity":
				a.Identity = &managedAccountIdentity{UserID: "synthetic-new-owner", WorkspaceID: a.AccountID}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		a, err = (Store{Path: p.Store.Path}).Get(a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if a.CredentialVersion != prior+1 {
			t.Fatalf("%s did not advance stable persisted credential version", field)
		}
		metadata, err := p.Store.GetMetadata(a.ID)
		if err != nil || metadata.CredentialVersion != a.CredentialVersion || metadata.AccessToken != "" {
			t.Fatal("metadata credential epoch unavailable or leaked token")
		}
	}
	// Explicit reauthorization advances the generation even if token bytes are
	// the same; imported duplicate-with-no-rotation remains a no-op elsewhere.
	prior := a.CredentialVersion
	if err := p.Store.UpdateTokens(a.ID, a.AccessToken, a); err != nil {
		t.Fatal(err)
	}
	a, err = p.Store.Get(a.ID)
	if err != nil || a.CredentialVersion != prior+1 {
		t.Fatal("explicit token update did not advance epoch")
	}
	if err := p.Store.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p.Store.Path)
	if err != nil {
		t.Fatal(err)
	}
	var state accountState
	if json.Unmarshal(raw, &state) != nil || len(state.CredentialVersions) != 0 {
		t.Fatal("deleted account generation remains")
	}
}

func TestFileCredentialGenerationLegacyMigrationKeepsOneOnMetadataWrite(t *testing.T) {
	p := newTestProxy(t, 1)
	raw, err := os.ReadFile(p.Store.Path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil {
		t.Fatal("invalid fixture")
	}
	delete(root, "credential_versions")
	raw, _ = json.Marshal(root)
	if err := os.WriteFile(p.Store.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := p.Store.Get("test")
	if err != nil || a.CredentialVersion != 1 {
		t.Fatal("legacy epoch did not initialize at one")
	}
	if err := p.Store.UpdateAccount(a.ID, func(a *Account) error { a.Notes = "migration metadata"; return nil }); err != nil {
		t.Fatal(err)
	}
	a, err = (Store{Path: p.Store.Path}).Get(a.ID)
	if err != nil || a.CredentialVersion != 1 {
		t.Fatal("legacy metadata write advanced credentials")
	}
	raw, _ = os.ReadFile(p.Store.Path)
	var state accountState
	if json.Unmarshal(raw, &state) != nil || state.CredentialVersions[a.ID] != 1 {
		t.Fatal("legacy generation not persisted")
	}
}
