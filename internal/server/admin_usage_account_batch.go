package server

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

func (p *Proxy) handleManagedAccountUsageBatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs  []string `json:"ids"`
		From int64    `json:"from"`
		To   int64    `json:"to"`
	}
	if err := readJSON(w, r, &body); err != nil {
		adminError(w, err)
		return
	}
	if len(body.IDs) == 0 || len(body.IDs) > 100 || body.From < 1 || body.To <= body.From || body.To-body.From > 90*86400 {
		adminError(w, errInvalid)
		return
	}
	for _, id := range body.IDs {
		if id == "" || len(id) > 256 {
			adminError(w, errInvalid)
			return
		}
	}
	q, err := parseManagedLogQuery(url.Values{"from": {strconv.FormatInt(body.From, 10)}, "to": {strconv.FormatInt(body.To, 10)}})
	if err != nil {
		adminError(w, err)
		return
	}
	q.AccountIDs = uniqueStrings(body.IDs)
	q.accountsOnly = true
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	err = p.resolveManagedUsageSnapshot(ctx, &q)
	if err != nil {
		p.storeError(w, err)
		return
	}
	stats, err := p.Metrics.managedLogStats(ctx, q)
	if err != nil {
		p.storeError(w, err)
		return
	}
	byID := map[string]managedUsageResource{}
	for _, row := range stats.Accounts {
		byID[row.ID] = row
	}
	result := []managedUsageResource{}
	for _, id := range q.AccountIDs {
		row, ok := byID[id]
		if !ok {
			row.ID = id
		}
		result = append(result, row)
	}
	writeJSON(w, map[string]any{"items": result, "scope": managedCleanupScope, "from": body.From, "to": body.To, "checked_at": stats.CheckedAt})
}
