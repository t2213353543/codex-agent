package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestSub2AccountPageParity(t *testing.T) {
	exerciseSub2AccountPageParity(t, webTestSetup)
}

func TestPGSub2AccountPageParity(t *testing.T) {
	exerciseSub2AccountPageParity(t, adminPGTestProxy)
}

func TestPGSub2ExportExplicitIDsHydratesOnlySelectedCredentials(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	selected := managedTestAccount(t, p, "sub2-export-selected", nil)
	for i := 0; i < 5; i++ {
		managedTestAccount(t, p, fmt.Sprintf("sub2-export-unselected-%02d", i), nil)
	}
	counter := &compatCredentialReadCounter{AEAD: p.Store.DB.cipher}
	p.Store.DB.cipher = counter
	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/admin/accounts/data?include_proxies=false&ids=%d", compatNumericID(selected.ID)), nil)
	response := httptest.NewRecorder()
	p.sub2ExportData(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("export status: %d %s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data struct {
			Accounts []map[string]json.RawMessage `json:"accounts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data.Accounts) != 1 {
		t.Fatalf("selected export returned %d accounts", len(envelope.Data.Accounts))
	}
	if got := counter.reads.Load(); got != 1 {
		t.Fatalf("decoded credentials for unselected accounts: %d", got)
	}
}

func exerciseSub2AccountPageParity(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	for i := 0; i < 12; i++ {
		managedTestAccount(t, p, fmt.Sprintf("paged-compat-%02d", i), func(a *Account) {
			a.Enabled = i%2 == 0
			a.Priority = i % 3
		})
	}
	var counter *keySnapshotCountingCipher
	if p.Store.DB != nil {
		counter = &keySnapshotCountingCipher{AEAD: p.Store.DB.cipher}
		p.Store.DB.cipher = counter
	}
	for _, query := range []string{
		"page=1&page_size=3", "page=2&page_size=3", "page=99&page_size=3",
		"page=9223372036854775807&page_size=1000", "search=paged-compat-0&page_size=3&sort_by=name&sort_order=desc",
		"page_size=3&status=active&sort_by=priority", "platform=unmatched&page_size=3",
	} {
		t.Run(query, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?"+query, nil)
			all, err := p.sub2FilteredAccounts(request.Context(), request, true)
			if err != nil {
				t.Fatal(err)
			}
			expected := sub2AccountPageRows(request, all)
			if counter != nil {
				counter.decrypts.Store(0)
			}
			actual, total, err := p.sub2AccountPage(request)
			if err != nil || total != len(all) || len(actual) != len(expected) {
				t.Fatalf("page dimensions: got=%d/%d want=%d/%d err=%v", len(actual), total, len(expected), len(all), err)
			}
			for i := range expected {
				want, _ := json.Marshal(sub2AccountDTO(expected[i]))
				got, _ := json.Marshal(sub2AccountDTO(actual[i]))
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("page DTO changed: got=%s want=%s", got, want)
				}
			}
			if counter != nil && counter.decrypts.Load() != int64(len(expected)) {
				t.Fatalf("decoded credentials for undisplayed accounts: got=%d page=%d", counter.decrypts.Load(), len(expected))
			}
		})
	}
}
