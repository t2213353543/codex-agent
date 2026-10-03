package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Creating a Sub2 resource must not read the complete resource catalogue
// first. The authoritative create transaction is expected to be the only
// PostgreSQL checkout on each route.
func TestPGSub2ResourceCreateAvoidsMetadataScan(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	for i := 0; i < 200; i++ {
		if _, err := p.Groups.Create("existing-group-"+string(rune('a'+i/26))+string(rune('a'+i%26)), ""); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Proxies.Create(ProxyEntry{Name: "existing-proxy-" + string(rune('a'+i/26)) + string(rune('a'+i%26)), Type: "http", Host: "127.0.0.1", Port: 1}); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name string
		path string
		body map[string]json.RawMessage
		call func(http.ResponseWriter, *http.Request, string, map[string]json.RawMessage)
	}{
		{name: "group", path: "/api/v1/admin/groups", body: map[string]json.RawMessage{
			"name": json.RawMessage(`"created-group"`),
		}, call: p.sub2GroupResource},
		{name: "proxy", path: "/api/v1/admin/proxies", body: map[string]json.RawMessage{
			"name": json.RawMessage(`"created-proxy"`), "protocol": json.RawMessage(`"http"`),
			"host": json.RawMessage(`"127.0.0.1"`), "port": json.RawMessage(`1`),
		}, call: p.sub2ProxyResource},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := p.Store.DB.Pool.Stat().AcquireCount()
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			response := httptest.NewRecorder()
			tc.call(response, req, "", tc.body)
			if response.Code != http.StatusOK {
				t.Fatalf("create returned %d: %s", response.Code, response.Body.String())
			}
			if acquired := p.Store.DB.Pool.Stat().AcquireCount() - before; acquired != 1 {
				t.Fatalf("create checked out %d connections; expected only authoritative write transaction", acquired)
			}
		})
	}
}

func TestPGSub2ResourceListUsesBoundedPage(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	for i := 0; i < 40; i++ {
		if _, err := p.Groups.Create(fmt.Sprintf("page-group-%02d", i), ""); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Proxies.Create(ProxyEntry{Name: fmt.Sprintf("page-proxy-%02d", i), Type: "http", Host: "127.0.0.1", Port: 1}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		path string
		call func(http.ResponseWriter, *http.Request, string, map[string]json.RawMessage)
		want int
	}{
		{name: "groups", path: "/api/v1/admin/groups?page=2&page_size=3", call: p.sub2GroupResource, want: 3},
		{name: "proxies", path: "/api/v1/admin/proxies?page=2&page_size=3", call: p.sub2ProxyResource, want: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := p.Store.DB.Pool.Stat().AcquireCount()
			response := httptest.NewRecorder()
			tc.call(response, httptest.NewRequest(http.MethodGet, tc.path, nil), "", nil)
			if response.Code != http.StatusOK {
				t.Fatalf("list returned %d: %s", response.Code, response.Body.String())
			}
			var envelope struct {
				Data struct {
					Items []json.RawMessage `json:"items"`
					Total int               `json:"total"`
				} `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if len(envelope.Data.Items) != tc.want || envelope.Data.Total != 40 {
				t.Fatalf("page dimensions: got=%d/%d want=%d/40", len(envelope.Data.Items), envelope.Data.Total, tc.want)
			}
			if acquired := p.Store.DB.Pool.Stat().AcquireCount() - before; acquired != 1 {
				t.Fatalf("bounded page checked out %d connections; expected one read-only snapshot", acquired)
			}
		})
	}
	// Search is literal substring matching in the compatibility contract;
	// SQL wildcard characters must not broaden the result set.
	response := httptest.NewRecorder()
	p.sub2GroupResource(response, httptest.NewRequest(http.MethodGet, "/api/v1/admin/groups?search=%25&page_size=10", nil), "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("literal search returned %d: %s", response.Code, response.Body.String())
	}
	var literal struct {
		Data struct {
			Total int `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &literal); err != nil {
		t.Fatal(err)
	}
	if literal.Data.Total != 0 {
		t.Fatalf("literal wildcard search matched %d groups", literal.Data.Total)
	}
}
