package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReviewLegacyValidationBoundaries(t *testing.T) {
	t.Run("null_document", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(`null`), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readLegacyBundle(dir); err == nil {
			t.Error("accounts.json=null is accepted as an empty valid import")
		}
	})
	t.Run("repeatable_file_identity", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "credentials.json")
		if err := os.WriteFile(path, []byte(`{"name":"review-account","access_token":"synthetic-review-token"}`), 0600); err != nil {
			t.Fatal(err)
		}
		first, err := readLegacyBundle(path)
		if err != nil {
			t.Fatal(err)
		}
		second, err := readLegacyBundle(path)
		if err != nil {
			t.Fatal(err)
		}
		if first.Manifest == second.Manifest && first.Summary.MetadataHash != second.Summary.MetadataHash {
			t.Error("same source manifest produces different stable account IDs / metadata hashes")
		}
	})
	t.Run("duplicate_group_names", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "groups.json"), []byte(`{"groups":[{"id":"g_1","name":"same"},{"id":"g_2","name":"same"}]}`), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readLegacyBundle(dir); err == nil {
			t.Error("dry-run accepts duplicate group names despite PostgreSQL UNIQUE constraint")
		}
	})
}
