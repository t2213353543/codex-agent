package server

import (
	"crypto/cipher"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
)

type compatCredentialReadCounter struct {
	cipher.AEAD
	reads atomic.Int64
}

func (c *compatCredentialReadCounter) Open(dst, nonce, encrypted, extra []byte) ([]byte, error) {
	plain, err := c.AEAD.Open(dst, nonce, encrypted, extra)
	if err == nil {
		var credential struct {
			AccessToken string `json:"access_token"`
		}
		if json.Unmarshal(plain, &credential) == nil && credential.AccessToken != "" {
			c.reads.Add(1)
		}
	}
	return plain, err
}

func TestCPAFilteredReadParity(t *testing.T) {
	exerciseCPAFilteredReadParity(t, webTestSetup)
}

func TestPGCPAFilteredReadParity(t *testing.T) {
	exerciseCPAFilteredReadParity(t, adminPGTestProxy)
}

func exerciseCPAFilteredReadParity(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	for i := 0; i < 20; i++ {
		managedTestAccount(t, p, fmt.Sprintf("filtered-cpa-%02d", i), nil)
	}
	selected := compatStoreFixture(t, p, "preserved-alias")
	selected, err := p.compatMutateAccount(t.Context(), selected.Account.ID, selected.Account.Version, func(a *Account, record *adminCompatRecord) error {
		a.Name = "renamed-account"
		a.AccountID = "workspace-current"
		a.ClientID = "client-current"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var counter *compatCredentialReadCounter
	if p.Store.DB != nil {
		counter = &compatCredentialReadCounter{AEAD: p.Store.DB.cipher}
		p.Store.DB.cipher = counter
	}
	all, err := p.compatListAccounts(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := p.settingsValue()
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []url.Values{
		{"name": {selected.Account.ID}},
		{"name": {"preserved-alias.json"}},
		{"auth_index": {compatAuthIndex(selected.Account.ID)}},
		{"name": {"renamed-account"}}, // CPA listing accepts filename/ID, not display name.
		{"name": {"preserved-alias.json"}, "auth_index": {"mismatch"}},
		{"name": {"missing.json"}},
	} {
		t.Run(query.Encode(), func(t *testing.T) {
			want := []map[string]any{}
			for _, row := range all {
				if name := query.Get("name"); name != "" && name != row.Account.ID && name != compatFileName(row.Account, row.Record) {
					continue
				}
				if index := query.Get("auth_index"); index != "" && index != compatAuthIndex(row.Account.ID) {
					continue
				}
				want = append(want, p.cpaListEntry(row, nil, settings.WebsocketEnabled))
			}
			if counter != nil {
				counter.reads.Store(0)
			}
			response := httptest.NewRecorder()
			p.handleCPAList(response, httptest.NewRequest(http.MethodGet, "/v0/management/auth-files?"+query.Encode(), nil))
			if response.Code != http.StatusOK {
				t.Fatalf("filtered CPA list: %d %s", response.Code, response.Body.String())
			}
			var got struct {
				Files []map[string]any `json:"files"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			// Normalize the DTO's timestamps/numeric types through its public JSON.
			wantJSON, _ := json.Marshal(want)
			var normalized []map[string]any
			if err := json.Unmarshal(wantJSON, &normalized); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Files, normalized) {
				t.Fatalf("CPA filtered representation changed: got=%v want=%v", got.Files, normalized)
			}
			if counter != nil && counter.reads.Load() != int64(len(want)) {
				t.Fatalf("decoded unrelated credentials: %d for %d displayed rows", counter.reads.Load(), len(want))
			}
		})
	}
}

func TestPGCPAFilteredReadIgnoresUnselectedCredentialFailure(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	a := managedTestAccount(t, p, "healthy-cpa", nil)
	b := managedTestAccount(t, p, "other-cpa", nil)
	if _, err := p.Store.DB.Pool.Exec(t.Context(), "UPDATE account_credentials SET credentials_cipher='invalid'::bytea WHERE account_id=$1", b.ID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		query string
		code  int
	}{
		{"?name=" + url.QueryEscape(a.ID), http.StatusOK},
		{"?name=" + url.QueryEscape(b.ID), http.StatusInternalServerError},
		{"", http.StatusInternalServerError},
	} {
		response := httptest.NewRecorder()
		p.handleCPAList(response, httptest.NewRequest(http.MethodGet, "/v0/management/auth-files"+tc.query, nil))
		if response.Code != tc.code {
			t.Fatalf("query %s: got=%d want=%d", tc.query, response.Code, tc.code)
		}
	}
}

func TestPGCompatReferenceProjectionPreservesAliases(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	row := compatStoreFixture(t, p, "old-filename")
	row, err := p.compatMutateAccount(t.Context(), row.Account.ID, row.Account.Version, func(account *Account, record *adminCompatRecord) error {
		account.Name = "new-display-name"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Alias resolution should be independent of unrelated binding/credential
	// tables. It does not return account credentials to the caller.
	if _, err := p.Store.DB.Pool.Exec(t.Context(), `ALTER TABLE account_groups RENAME TO unavailable_account_groups;
		ALTER TABLE account_proxies RENAME TO unavailable_account_proxies;
		ALTER TABLE account_credentials RENAME TO unavailable_account_credentials`); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{row.Account.ID, row.Account.Name, "old-filename.json", strconv.FormatInt(compatNumericID(row.Account.ID), 10)} {
		id, err := p.compatResolveAccount(t.Context(), ref)
		if err != nil || id != row.Account.ID {
			t.Fatalf("projected alias %q: got=%s err=%v", ref, id, err)
		}
	}
}
