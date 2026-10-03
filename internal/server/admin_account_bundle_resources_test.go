package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestManagedBundleResourceRestore(t *testing.T) { runManagedBundleResourceRestore(t, webTestSetup) }
func TestPGManagedBundleResourceRestore(t *testing.T) {
	runManagedBundleResourceRestore(t, adminPGTestProxy)
}
func runManagedBundleResourceRestore(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	g, err := p.Groups.Create("resource-original", "resource description")
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.createManagedBundleProxy(context.Background(), ProxyEntry{Name: "resource-proxy-original", Type: "http", Host: "127.0.0.1", Port: 9, Username: "synthetic-resource-user", Password: "synthetic-resource-password", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	a := managedTestAccount(t, p, "resource-account", func(a *Account) { a.Groups = []string{g.ID}; a.Proxies = []string{v.ID} })
	body := map[string]any{"items": []managedAccountItem{{ID: a.ID, Version: a.Version}}, "include_resources": true}
	r := managedTestRequest(t, h, "POST", "/api/accounts/export/credentials", body, 200)
	if strings.Contains(r.Body.String(), v.Password) || strings.Contains(r.Body.String(), v.Username) {
		t.Fatal("resource credentials exported without separate consent")
	}
	var bundle managedAccountBundle
	if json.Unmarshal(r.Body.Bytes(), &bundle) != nil || bundle.Resources == nil || len(bundle.Resources.Groups) != 1 || len(bundle.Resources.Proxies) != 1 {
		t.Fatal("associated resources missing")
	}
	body["include_proxy_credentials"] = true
	managedTestRequest(t, h, "POST", "/api/accounts/export/credentials", body, 400)
	body["confirm_proxy_credentials"] = true
	r = managedTestRequest(t, h, "POST", "/api/accounts/export/credentials", body, 200)
	if !strings.Contains(r.Body.String(), v.Password) {
		t.Fatal("explicit proxy backup lost password")
	}
	_ = json.Unmarshal(r.Body.Bytes(), &bundle)
	bundle.Resources.Groups[0].Name = "restored-resource-group"
	bundle.Resources.Proxies[0].Name = "restored-resource-proxy"
	groupsBefore, _ := p.Groups.List()
	proxiesBefore, _ := p.Proxies.List()
	request := map[string]any{"resources": bundle.Resources, "dry_run": true, "confirm_proxy_credentials": true}
	r = managedTestRequest(t, h, "POST", "/api/accounts/import-resources", request, 200)
	if !strings.Contains(r.Body.String(), `"status":"create"`) {
		t.Fatal("resource preview missing creation plan")
	}
	groupsAfter, _ := p.Groups.List()
	proxiesAfter, _ := p.Proxies.List()
	if len(groupsBefore) != len(groupsAfter) || len(proxiesBefore) != len(proxiesAfter) {
		t.Fatal("resource dry run wrote data")
	}
	request["dry_run"] = false
	r = managedTestRequest(t, h, "POST", "/api/accounts/import-resources", request, 200)
	var restored struct {
		GroupMapping map[string]string `json:"group_mapping"`
		ProxyMapping map[string]string `json:"proxy_mapping"`
		Succeeded    int               `json:"succeeded"`
		Failed       int               `json:"failed"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &restored)
	if restored.Succeeded != 2 || restored.Failed != 0 {
		t.Fatalf("resource restore failed %s", r.Body.String())
	}
	proxiesAfter, _ = p.Proxies.List()
	found := false
	for _, proxy := range proxiesAfter {
		if proxy.ID == restored.ProxyMapping[v.ID] {
			found = true
			if proxy.Enabled || proxy.Password != v.Password || proxy.Username != v.Username {
				t.Fatal("proxy backup lost disabled/auth state")
			}
		}
	}
	if !found {
		t.Fatal("restored proxy missing")
	}
	r = managedTestRequest(t, h, "POST", "/api/accounts/import-resources", request, 200)
	if !strings.Contains(r.Body.String(), "name_conflict") {
		t.Fatal("same-name resources silently overwritten")
	}
	request["group_mapping"] = restored.GroupMapping
	request["proxy_mapping"] = restored.ProxyMapping
	r = managedTestRequest(t, h, "POST", "/api/accounts/import-resources", request, 200)
	if !strings.Contains(r.Body.String(), `"status":"mapped"`) {
		t.Fatal("explicit existing-resource mapping failed")
	}
	if err := p.Store.DeleteVersion(a.ID, a.Version); err != nil {
		t.Fatal(err)
	}
	managedTestRequest(t, h, "POST", "/api/accounts/import-bundle", map[string]any{"data": bundle, "group_mapping": restored.GroupMapping, "proxy_mapping": restored.ProxyMapping}, 200)
	recovered, err := p.Store.Get(a.Name)
	if err != nil || len(recovered.Groups) != 1 || recovered.Groups[0] != restored.GroupMapping[g.ID] || len(recovered.Proxies) != 1 || recovered.Proxies[0] != restored.ProxyMapping[v.ID] {
		t.Fatal("resource mappings did not roundtrip account bindings")
	}
}
