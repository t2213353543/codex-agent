package server

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// A bounded explicit batch reads upstream account data; it never runs inference.
func (p *Proxy) handleManagedUpstreamBatch(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Items []managedAccountItem `json:"items"`
		Kind  string               `json:"kind"`
		Force bool                 `json:"force"`
	}
	if err := readJSONLimit(w, r, &input, 16<<10); err != nil {
		adminError(w, err)
		return
	}
	if err := validateManagedItems(input.Items, 10); err != nil {
		adminError(w, err)
		return
	}
	if input.Kind != "models" && input.Kind != "usage" {
		adminError(w, errors.New("batch supports only models or usage"))
		return
	}
	type result struct {
		ID       string                   `json:"id"`
		OK       bool                     `json:"ok"`
		Code     string                   `json:"code,omitempty"`
		Snapshot *managedUpstreamSnapshot `json:"snapshot,omitempty"`
	}
	rows := make([]result, len(input.Items))
	succeeded := 0
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	for index, item := range input.Items {
		row := result{ID: item.ID}
		a, err := p.Store.Get(item.ID)
		if err != nil || a.ID != item.ID {
			row.Code = "not_found"
		} else if a.Version != item.Version {
			row.Code = "version_conflict"
		} else if ctx.Err() != nil {
			row.Code = "canceled"
		} else if cached := p.cachedManagedUsage(ctx, a, input.Kind == "usage" && !input.Force); cached != nil {
			row.Snapshot = cached
			row.OK = true
		} else {
			checkCtx, checkCancel := context.WithTimeout(ctx, 8*time.Second)
			snapshot := p.checkManagedUpstream(checkCtx, a, input.Kind, "")
			checkCancel()
			row.Snapshot = &snapshot
			row.OK = snapshot.OK
			if err := p.cacheManagedUpstream(ctx, snapshot); err != nil {
				row.OK = false
				row.Code = "storage_error"
			}
			if p.Store.DB != nil {
				outcome := "failed"
				if row.OK {
					outcome = "ok"
				}
				if _, err := p.Store.DB.Pool.Exec(ctx, "INSERT INTO audit_logs(action,entity_id,result) VALUES($1,$2,$3)", "account.check."+input.Kind, a.ID, outcome); err != nil {
					row.OK = false
					row.Code = "storage_error"
				}
			}
		}
		if row.OK {
			succeeded++
		}
		rows[index] = row
	}
	snapshots := make([]*managedUpstreamSnapshot, 0, len(rows))
	for _, row := range rows {
		if row.OK && row.Snapshot != nil {
			snapshots = append(snapshots, row.Snapshot)
		}
	}
	p.enrichManagedWindowCosts(ctx, snapshots)
	writeJSON(w, map[string]any{"results": rows, "succeeded": succeeded, "failed": len(rows) - succeeded})
}
