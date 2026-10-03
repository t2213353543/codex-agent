package server

import (
	"context"
	"errors"
	"net/http"
	"time"
)

type managedBackupGroup struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}
type managedBackupProxy struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Type                string `json:"type"`
	Host                string `json:"host"`
	Port                int    `json:"port"`
	Enabled             bool   `json:"enabled"`
	HasCredentials      bool   `json:"has_credentials"`
	CredentialsIncluded bool   `json:"credentials_included"`
	Username            string `json:"username,omitempty"`
	Password            string `json:"password,omitempty"`
}
type managedBundleResources struct {
	Version int                  `json:"version"`
	Groups  []managedBackupGroup `json:"groups"`
	Proxies []managedBackupProxy `json:"proxies"`
}

type managedBundleResourcesInput struct {
	Resources               managedBundleResources `json:"resources"`
	DryRun                  bool                   `json:"dry_run"`
	GroupMapping            map[string]string      `json:"group_mapping"`
	ProxyMapping            map[string]string      `json:"proxy_mapping"`
	ConfirmProxyCredentials bool                   `json:"confirm_proxy_credentials"`
}

type managedBundleResourceResult struct {
	Kind     string `json:"kind"`
	SourceID string `json:"source_id"`
	ID       string `json:"id,omitempty"`
	Status   string `json:"status"`
	Code     string `json:"code,omitempty"`
}

type managedBundleResourceReply struct {
	DryRun       bool                          `json:"dry_run"`
	Results      []managedBundleResourceResult `json:"results"`
	GroupMapping map[string]string             `json:"group_mapping"`
	ProxyMapping map[string]string             `json:"proxy_mapping"`
	Succeeded    int                           `json:"succeeded"`
	Failed       int                           `json:"failed"`
}

func (p *Proxy) exportManagedBundleResources(ctx context.Context, accounts []Account, includeCredentials bool) (*managedBundleResources, error) {
	groupRefs, proxyRefs := map[string]bool{}, map[string]bool{}
	wantedGroups, wantedProxies := map[string]struct{}{}, map[string]struct{}{}
	for _, a := range accounts {
		for _, id := range a.Groups {
			if !groupRefs[id] {
				groupRefs[id] = true
				wantedGroups[id] = struct{}{}
			}
		}
		for _, id := range a.Proxies {
			if !proxyRefs[id] {
				proxyRefs[id] = true
				wantedProxies[id] = struct{}{}
			}
		}
	}
	// An export serializes only the referenced groups and proxies; it must not
	// load the whole directory (or decrypt every proxy password) to filter it.
	groups, err := p.Groups.managedSelectedMetadata(ctx, wantedGroups)
	if err != nil {
		return nil, err
	}
	var proxies []ProxyEntry
	if includeCredentials {
		proxies, err = p.Proxies.managedSelectedCredentials(ctx, wantedProxies)
	} else {
		proxies, err = p.Proxies.managedSelectedMetadata(ctx, wantedProxies)
	}
	if err != nil {
		return nil, err
	}
	resources := &managedBundleResources{Version: 1, Groups: []managedBackupGroup{}, Proxies: []managedBackupProxy{}}
	for _, g := range groups {
		if !groupRefs[g.ID] {
			continue
		}
		delete(groupRefs, g.ID)
		resources.Groups = append(resources.Groups, managedBackupGroup{g.ID, g.Name, g.Description})
	}
	for _, v := range proxies {
		if !proxyRefs[v.ID] {
			continue
		}
		delete(proxyRefs, v.ID)
		row := managedBackupProxy{ID: v.ID, Name: v.Name, Type: v.Type, Host: v.Host, Port: v.Port, Enabled: v.Enabled, HasCredentials: v.Username != "" || v.Password != "" || v.HasPassword, CredentialsIncluded: includeCredentials}
		if includeCredentials {
			row.Username = v.Username
			row.Password = v.Password
		}
		resources.Proxies = append(resources.Proxies, row)
	}
	if len(groupRefs) > 0 || len(proxyRefs) > 0 {
		return nil, errVersionConflict
	}
	return resources, nil
}
func (p *Proxy) createManagedBundleProxy(ctx context.Context, v ProxyEntry) (ProxyEntry, error) {
	if err := validProxy(v); err != nil {
		return v, err
	}
	v.ID = newID("p_")
	v.Version = 1
	v.CreatedAt = time.Now().Unix()
	v.UpdatedAt = v.CreatedAt
	v.HasPassword = v.Password != ""
	if p.Store.DB == nil {
		err := p.Proxies.store.Modify(func(state *proxyState) error {
			for _, existing := range state.Proxies {
				if existing.Name == v.Name {
					return errConflict
				}
			}
			state.Proxies = append(state.Proxies, v)
			return nil
		})
		if err == nil {
			_, err = p.Proxies.List()
		}
		return v, err
	}
	db := p.Store.DB
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return v, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || ':proxy-resource-import', 981))"); err != nil {
		return v, err
	}
	var exists bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM proxies WHERE name=$1)", v.Name).Scan(&exists); err != nil {
		return v, err
	}
	if exists {
		return v, errConflict
	}
	cipher := []byte{}
	if v.Password != "" {
		cipher, err = db.Encrypt([]byte(v.Password))
		if err != nil {
			return v, err
		}
	}
	if _, err = tx.Exec(ctx, "INSERT INTO proxies(id,name,type,host,port,username,password_cipher,enabled) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", v.ID, v.Name, v.Type, v.Host, v.Port, v.Username, cipher, v.Enabled); err != nil {
		return v, err
	}
	if err = db.audit(ctx, tx, "proxy.restore", v.ID); err != nil {
		return v, err
	}
	if err = tx.Commit(ctx); err != nil {
		return v, err
	}
	db.invalidate()
	// Mirror createPG's incremental index publish: a restore importing N
	// proxies must not re-read and re-decrypt the whole table after each one.
	p.Proxies.mu.Lock()
	if current := p.Proxies.index[v.ID]; current == nil || current.Version <= v.Version {
		cached := v
		p.Proxies.index[v.ID] = &cached
	}
	p.Proxies.mu.Unlock()
	return v, nil
}

func (p *Proxy) handleManagedBundleResourcesImport(w http.ResponseWriter, r *http.Request) {
	var input managedBundleResourcesInput
	if err := readJSONLimit(w, r, &input, 8<<20); err != nil {
		adminError(w, err)
		return
	}
	if input.Resources.Version != 1 || len(input.Resources.Groups)+len(input.Resources.Proxies) > 10000 {
		adminError(w, errInvalid)
		return
	}
	if reply, handled := p.importManagedBundleResourcesPipeline(r.Context(), input); handled {
		writeJSON(w, reply)
		return
	}
	if err := r.Context().Err(); err != nil {
		p.storeError(w, err)
		return
	}
	groups, err := p.Groups.List()
	if err != nil {
		p.storeError(w, err)
		return
	}
	proxies, err := p.Proxies.managedMetadata()
	if err != nil {
		p.storeError(w, err)
		return
	}
	groupIDs, proxyIDs, groupNames, proxyNames := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, g := range groups {
		groupIDs[g.ID] = true
		groupNames[g.Name] = true
	}
	for _, v := range proxies {
		proxyIDs[v.ID] = true
		proxyNames[v.Name] = true
	}
	groupMapping, proxyMapping := map[string]string{}, map[string]string{}
	type result struct {
		Kind     string `json:"kind"`
		SourceID string `json:"source_id"`
		ID       string `json:"id,omitempty"`
		Status   string `json:"status"`
		Code     string `json:"code,omitempty"`
	}
	results := []result{}
	succeeded := 0
	seen := map[string]bool{}
	for _, g := range input.Resources.Groups {
		row := result{Kind: "group", SourceID: g.ID, Status: "failed"}
		switch {
		case r.Context().Err() != nil:
			row.Code = "canceled"
		case g.ID == "" || seen["g:"+g.ID] || validGroupName(g.Name) != nil || len(g.Description) > 8192:
			row.Code = "invalid_resource"
		case input.GroupMapping[g.ID] != "":
			if id := input.GroupMapping[g.ID]; groupIDs[id] {
				row.Status = "mapped"
				row.ID = id
				groupMapping[g.ID] = id
			} else {
				row.Code = "mapping_not_found"
			}
		case groupNames[g.Name]:
			row.Code = "name_conflict"
		default:
			if input.DryRun {
				row.Status = "create"
				groupNames[g.Name] = true
			} else {
				created, err := p.Groups.Create(g.Name, g.Description)
				if err != nil {
					row.Code = managedAccountErrorCode(err)
				} else {
					row.Status = "created"
					row.ID = created.ID
					groupMapping[g.ID] = created.ID
					groupNames[g.Name] = true
					groupIDs[created.ID] = true
				}
			}
		}
		seen["g:"+g.ID] = true
		if row.Status != "failed" {
			succeeded++
		}
		results = append(results, row)
	}
	for _, v := range input.Resources.Proxies {
		row := result{Kind: "proxy", SourceID: v.ID, Status: "failed"}
		entry := ProxyEntry{Name: v.Name, Type: v.Type, Host: v.Host, Port: v.Port, Enabled: v.Enabled, Username: v.Username, Password: v.Password}
		switch {
		case r.Context().Err() != nil:
			row.Code = "canceled"
		case v.ID == "" || seen["p:"+v.ID] || validProxy(entry) != nil || len(v.Username) > 4096 || len(v.Password) > 16384:
			row.Code = "invalid_resource"
		case input.ProxyMapping[v.ID] != "":
			if id := input.ProxyMapping[v.ID]; proxyIDs[id] {
				row.Status = "mapped"
				row.ID = id
				proxyMapping[v.ID] = id
			} else {
				row.Code = "mapping_not_found"
			}
		case v.HasCredentials && !v.CredentialsIncluded:
			row.Code = "credentials_missing"
		case (v.Username != "" || v.Password != "") && !input.ConfirmProxyCredentials:
			row.Code = "credentials_confirmation_required"
		case proxyNames[v.Name]:
			row.Code = "name_conflict"
		default:
			if input.DryRun {
				row.Status = "create"
				proxyNames[v.Name] = true
			} else {
				created, err := p.createManagedBundleProxy(r.Context(), entry)
				if err != nil {
					row.Code = managedAccountErrorCode(err)
				} else {
					row.Status = "created"
					row.ID = created.ID
					proxyMapping[v.ID] = created.ID
					proxyNames[v.Name] = true
					proxyIDs[created.ID] = true
				}
			}
		}
		seen["p:"+v.ID] = true
		if row.Status != "failed" {
			succeeded++
		}
		results = append(results, row)
	}
	writeJSON(w, map[string]any{"dry_run": input.DryRun, "results": results, "group_mapping": groupMapping, "proxy_mapping": proxyMapping, "succeeded": succeeded, "failed": len(results) - succeeded})
}

func validateManagedResourceExport(resources, credentials, confirm, secretExport bool) error {
	if (resources || credentials || confirm) && !secretExport {
		return errors.New("resource backup requires the explicit credentials export endpoint")
	}
	if credentials && (!resources || !confirm) {
		return errors.New("proxy credentials require include_resources and confirm_proxy_credentials")
	}
	return nil
}
