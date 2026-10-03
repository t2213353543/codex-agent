package server

// Session D regression tests (2026-09-30): single-resource compatibility
// CRUD stays bounded. Resolving one alias must not decrypt the compatibility
// record of every account, deletes must not fail on version drift that the
// delete itself makes irrelevant, and unchanged record/link documents must
// not be rewritten on management-only edits.

import (
	"bytes"
	"context"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
)

// compatRecordDecryptCounter counts decryptions of compatibility record
// documents. Record payloads carry the filename/cpa/sub2 members; credential
// and file-name link documents do not.
type compatRecordDecryptCounter struct {
	cipher.AEAD
	reads atomic.Int64
}

func (c *compatRecordDecryptCounter) Open(dst, nonce, encrypted, extra []byte) ([]byte, error) {
	plain, err := c.AEAD.Open(dst, nonce, encrypted, extra)
	if err == nil && (bytes.Contains(plain, []byte(`"filename"`)) || bytes.Contains(plain, []byte(`"cpa"`)) || bytes.Contains(plain, []byte(`"sub2"`))) {
		c.reads.Add(1)
	}
	return plain, err
}

func TestCompatBoundedAliasResolutionParity(t *testing.T) {
	exerciseCompatBoundedAliasResolution(t, webTestSetup)
}

func TestPGCompatBoundedAliasResolutionParity(t *testing.T) {
	exerciseCompatBoundedAliasResolution(t, adminPGTestProxy)
}

func exerciseCompatBoundedAliasResolution(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		managedTestAccount(t, p, fmt.Sprintf("bounded-native-%02d", i), nil)
	}
	target := compatStoreFixture(t, p, "bounded-alias")
	other := compatStoreFixture(t, p, "other-alias")
	for _, tc := range []struct {
		ref  string
		want string
	}{
		{target.Account.ID, target.Account.ID},
		{"bounded-alias", target.Account.ID},
		{"bounded-alias.json", target.Account.ID},
		{strconv.FormatInt(compatNumericID(target.Account.ID), 10), target.Account.ID},
		{other.Account.ID, other.Account.ID},
	} {
		id, err := p.compatResolveAccount(ctx, tc.ref)
		if err != nil || id != tc.want {
			t.Fatalf("resolve %q: got=%s err=%v", tc.ref, id, err)
		}
	}
	if id, err := p.compatResolveAccount(ctx, "missing-alias.json"); err == nil || id != "" {
		t.Fatalf("missing alias resolved to %q err=%v", id, err)
	}
	if _, err := p.compatResolveAccount(ctx, ""); err == nil {
		t.Fatal("empty alias must be invalid")
	}
}

func TestPGCompatBoundedAliasSkipsUnrelatedRecords(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		managedTestAccount(t, p, fmt.Sprintf("bounded-unrelated-%02d", i), nil)
	}
	target := compatStoreFixture(t, p, "bounded-target")
	counter := &compatRecordDecryptCounter{AEAD: p.Store.DB.cipher}
	p.Store.DB.cipher = counter
	for _, ref := range []string{
		target.Account.ID,
		"bounded-target",
		"bounded-target.json",
		strconv.FormatInt(compatNumericID(target.Account.ID), 10),
	} {
		counter.reads.Store(0)
		id, err := p.compatResolveAccount(ctx, ref)
		if err != nil || id != target.Account.ID {
			t.Fatalf("resolve %q: got=%s err=%v", ref, id, err)
		}
		if got := counter.reads.Load(); got != 0 {
			t.Fatalf("resolve %q decrypted %d compatibility records before hydration", ref, got)
		}
		row, err := p.compatGetAccount(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.Account.ID != target.Account.ID {
			t.Fatalf("hydrated wrong account %s", row.Account.ID)
		}
		if got := counter.reads.Load(); got != 1 {
			t.Fatalf("resolve %q decrypted %d records after hydration, want 1", ref, got)
		}
	}
	counter.reads.Store(0)
	if _, err := p.cpaFind(ctx, "", compatAuthIndex(target.Account.ID)); err != nil {
		t.Fatal(err)
	}
	if got := counter.reads.Load(); got != 1 {
		t.Fatalf("auth index lookup decrypted %d records, want 1", got)
	}
	counter.reads.Store(0)
	if _, err := p.cpaFind(ctx, "missing-file.json", ""); !errors.Is(err, errNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	if got := counter.reads.Load(); got != 0 {
		t.Fatalf("missing file lookup decrypted %d records", got)
	}
}

func TestPGCPAFilteredListSkipsUnrelatedRecordFailures(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	healthy := compatStoreFixture(t, p, "healthy-file")
	broken := compatStoreFixture(t, p, "broken-file")
	key, err := managementStateKey(compatRecordNamespace, broken.Account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Store.DB.Pool.Exec(t.Context(), "UPDATE settings SET value='null'::jsonb WHERE key=$1", key); err != nil {
		t.Fatal(err)
	}
	healthyName := compatFileName(healthy.Account, healthy.Record)
	response := httptest.NewRecorder()
	p.handleCPAList(response, httptest.NewRequest(http.MethodGet, "/v0/management/auth-files?name="+url.QueryEscape(healthyName), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("filtered list failed through unrelated corruption: %d %s", response.Code, response.Body.String())
	}
	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Files) != 1 || payload.Files[0]["name"] != healthyName {
		t.Fatalf("filtered list returned %+v", payload.Files)
	}
	response = httptest.NewRecorder()
	p.handleCPAList(response, httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("unfiltered list should still surface the corruption: %d", response.Code)
	}
}

func TestPGCPADeleteIgnoresVersionDrift(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	row := compatStoreFixture(t, p, "drift-delete")
	name := compatFileName(row.Account, row.Record)
	// A refresh or lease tick between listing and deleting must not turn the
	// delete into a version conflict report.
	if _, err := p.Store.DB.Pool.Exec(t.Context(), "UPDATE accounts SET version=version+7 WHERE id=$1", row.Account.ID); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	p.handleCPADelete(response, httptest.NewRequest(http.MethodDelete, "/v0/management/auth-files?name="+url.QueryEscape(name), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("delete after version drift: %d %s", response.Code, response.Body.String())
	}
	if _, err := p.Store.GetMetadata(row.Account.ID); !errors.Is(err, errNotFound) {
		t.Fatalf("account survived delete: %v", err)
	}
	response = httptest.NewRecorder()
	p.handleCPADelete(response, httptest.NewRequest(http.MethodDelete, "/v0/management/auth-files?name="+url.QueryEscape(name), nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("second delete should report not found: %d %s", response.Code, response.Body.String())
	}
}

func TestPGCompatMutateSkipsUnchangedRecordWrites(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	ctx := context.Background()
	row := compatStoreFixture(t, p, "skip-write")
	documentVersion := func(namespace, key string) int64 {
		name, err := managementStateKey(namespace, key)
		if err != nil {
			t.Fatal(err)
		}
		var version int64
		if err := p.Store.DB.Pool.QueryRow(ctx, "SELECT version FROM settings WHERE key=$1", name).Scan(&version); err != nil {
			t.Fatal(err)
		}
		return version
	}
	oldName := compatFileName(row.Account, row.Record)
	recordBefore := documentVersion(compatRecordNamespace, row.Account.ID)
	linkBefore := documentVersion("admin-compat-files", oldName)
	if recordBefore < 1 || linkBefore < 1 {
		t.Fatalf("fixture documents missing: record=%d link=%d", recordBefore, linkBefore)
	}
	updated, err := p.compatMutateAccount(ctx, row.Account.ID, 0, func(a *Account, r *adminCompatRecord) error {
		a.Enabled = false
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Account.Enabled {
		t.Fatal("disable mutation not applied")
	}
	if got := documentVersion(compatRecordNamespace, row.Account.ID); got != recordBefore {
		t.Fatalf("unchanged record rewritten: %d -> %d", recordBefore, got)
	}
	if got := documentVersion("admin-compat-files", oldName); got != linkBefore {
		t.Fatalf("unchanged file-name link rewritten: %d -> %d", linkBefore, got)
	}
	// A real record change rewrites the record document, and a file rename
	// moves the link without leaving the old one behind.
	updated, err = p.compatMutateAccount(ctx, row.Account.ID, 0, func(a *Account, r *adminCompatRecord) error {
		r.FileName = "moved-file.json"
		r.Sub2["extra"] = json.RawMessage(`{"email":"moved@example.invalid"}`)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := documentVersion(compatRecordNamespace, row.Account.ID); got == recordBefore {
		t.Fatal("changed record was not rewritten")
	}
	if _, err := p.managementState().Get(ctx, "admin-compat-files", oldName); !errors.Is(err, errNotFound) {
		t.Fatalf("old file link survived rename: %v", err)
	}
	if _, err := p.managementState().Get(ctx, "admin-compat-files", "moved-file.json"); err != nil {
		t.Fatalf("new file link missing: %v", err)
	}
	if id, err := p.compatResolveAccount(ctx, "moved-file.json"); err != nil || id != row.Account.ID {
		t.Fatalf("renamed file no longer resolves: %s %v", id, err)
	}
}

func TestCompatMutateBoundedWriteParity(t *testing.T) {
	// The in-memory store keeps its whole-state persistence model; only the
	// observable alias semantics are asserted here.
	p, _ := webTestSetup(t)
	ctx := context.Background()
	row := compatStoreFixture(t, p, "parity-write")
	updated, err := p.compatMutateAccount(ctx, row.Account.ID, 0, func(a *Account, r *adminCompatRecord) error {
		a.Enabled = false
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Account.Enabled {
		t.Fatal("disable mutation not applied")
	}
	if id, err := p.compatResolveAccount(ctx, "parity-write.json"); err != nil || id != row.Account.ID {
		t.Fatalf("alias resolution changed: %s %v", id, err)
	}
}
