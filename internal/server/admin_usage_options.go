package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type managedUsageOption struct {
	Value   string `json:"value"`
	Label   string `json:"label"`
	Deleted bool   `json:"deleted"`
}

func (p *Proxy) handleManagedUsageOptions(w http.ResponseWriter, r *http.Request) {
	q, err := parseManagedLogRequest(r)
	if err != nil {
		adminError(w, err)
		return
	}
	field := r.URL.Query().Get("field")
	search := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
	}
	if err != nil || limit < 1 || limit > 200 || len(search) > 256 || !stringIn(field, "account", "key", "model", "group") {
		adminError(w, errInvalid)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err = p.resolveManagedUsageSnapshot(ctx, &q); err != nil {
		p.storeError(w, err)
		return
	}
	current := map[string]string{}
	var candidates []managedUsageOption
	// Account and group labels used to be loaded in full before the historical
	// query ran.  That made this small autocomplete endpoint O(number of
	// configured resources), even when the log result was empty or bounded by a
	// tiny limit.  PostgreSQL can join the current label only for historical
	// resource IDs that survive the same filters, preserving rename/deleted and
	// search semantics without transferring the entire table.
	if p.Store.DB != nil && stringIn(field, "account", "group") {
		candidates, err = p.Metrics.managedUsageOptionsPGCurrent(ctx, q, field, search, limit)
		if err != nil {
			p.storeError(w, err)
			return
		}
	}
	if candidates == nil {
		switch field {
		case "account":
			if p.Store.DB != nil {
				current, err = managedUsageLabelsPG(ctx, p.Store.DB, "SELECT id,display_name FROM accounts")
			} else {
				_, accounts, e := p.Store.ListMetadata()
				err = e
				for _, a := range accounts {
					current[a.ID] = a.Name
				}
			}
		case "key":
			if p.Keys != nil {
				keys, e := p.Keys.List()
				err = e
				for _, k := range keys {
					current[k.ID] = k.Name
				}
			}
		case "group":
			if p.Groups != nil {
				if p.Groups.DB != nil {
					current, err = managedUsageLabelsPG(ctx, p.Groups.DB, "SELECT id,name FROM groups")
				} else {
					groups, e := p.Groups.List()
					err = e
					for _, g := range groups {
						current[g.ID] = g.Name
					}
				}
			}
		}
		if err != nil {
			p.storeError(w, err)
			return
		}
		candidates, err = p.Metrics.managedUsageOptions(ctx, q, field, search, limit, current)
		if err != nil {
			p.storeError(w, err)
			return
		}
	}
	more := len(candidates) > limit
	if more {
		candidates = candidates[:limit]
	}
	writeJSON(w, map[string]any{"items": candidates, "has_more": more, "scope": managedCleanupScope, "checked_at": time.Now().Unix()})
}

// managedUsageOptionsPGCurrent resolves account/group labels in the same
// statement as the historical option scan.  The candidate CTE is bounded by
// the request's filters and performs indexed point lookups into the current
// resource table only for IDs that can actually be returned.  This keeps
// deleted IDs (which have no current row) and renamed labels intact while
// avoiding an O(all accounts/groups) metadata transfer on every autocomplete
// request.
func (m *Metrics) managedUsageOptionsPGCurrent(ctx context.Context, q managedLogQuery, field, search string, limit int) ([]managedUsageOption, error) {
	if m.db == nil || !stringIn(field, "account", "group") {
		return nil, fmt.Errorf("current PostgreSQL labels require account or group field")
	}
	table, labelColumn := "accounts", "display_name"
	if field == "group" {
		table, labelColumn = "groups", "name"
	}
	var fastStatement string
	var fastArgs []any
	if managedUsageRollupOrClassEligible(q) {
		source, args := managedUsageRollupSourceWithMaterialization(q, false)
		args = append(args, field, search, limit+1)
		kindParam, searchParam, limitParam := len(args)-2, len(args)-1, len(args)
		statement := source + fmt.Sprintf(`, candidates AS (
 SELECT resource_id,max(name COLLATE "C") AS historical_name
 FROM source
 WHERE kind=$%d AND resource_id<>'' AND
   ($%d='' OR position($%d in lower(resource_id||' '||COALESCE(name,'')))>0 OR
    EXISTS (SELECT 1 FROM %s current_search WHERE current_search.id=resource_id AND
      position($%d in lower(COALESCE(current_search.%s,'')))>0))
 GROUP BY resource_id
 )
 SELECT c.resource_id,COALESCE(NULLIF(current_label.%s,''),c.historical_name,c.resource_id),
        (current_label.id IS NULL)
 FROM candidates c
 LEFT JOIN %s current_label ON current_label.id=c.resource_id
  ORDER BY c.resource_id COLLATE "C" LIMIT $%d`, kindParam, searchParam, searchParam, table, searchParam, labelColumn, labelColumn, table, limitParam)
		if managedUsageRollupEligible(q) {
			return queryManagedUsageCurrentOptions(ctx, m.db, statement, args...)
		}
		fastStatement, fastArgs = statement, args
	}
	source, where, args := q.readSourceSQL()
	source, where = managedUsageAnalyticsSource(source, where), "TRUE"
	idExpr, nameExpr, fromExpr := "COALESCE(option_id,'')", "account_name", "source_rows"
	decode, projection := "account_id text", "observed.account_id AS option_id"
	if field == "group" {
		idExpr, nameExpr = "group_id", "group_id"
		decode, projection = "groups jsonb", "observed.groups AS option_groups"
		fromExpr += " CROSS JOIN LATERAL jsonb_array_elements_text(COALESCE(source_rows.option_groups,'[]'::jsonb)) group_members(group_id)"
	}
	args = append(args, search, limit+1)
	searchParam, limitParam := len(args)-1, len(args)
	statement := fmt.Sprintf(`WITH source_rows AS MATERIALIZED (SELECT account_name,%s FROM %s
 CROSS JOIN LATERAL jsonb_to_record(record) observed(%s) WHERE %s), candidates AS (
 SELECT %s AS resource_id,max(%s COLLATE "C") AS historical_name
 FROM %s
	WHERE %s<>'' AND
   ($%d='' OR position($%d in lower(%s||' '||COALESCE(%s,'')))>0 OR
    EXISTS (SELECT 1 FROM %s current_search WHERE current_search.id=%s AND
      position($%d in lower(COALESCE(current_search.%s,'')))>0))
 GROUP BY %s
 )
 SELECT c.resource_id,COALESCE(NULLIF(current_label.%s,''),c.historical_name,c.resource_id),
        (current_label.id IS NULL)
 FROM candidates c
 LEFT JOIN %s current_label ON current_label.id=c.resource_id
 ORDER BY c.resource_id COLLATE "C" LIMIT $%d`, projection, source, decode, where, idExpr, nameExpr, fromExpr, idExpr, searchParam, searchParam, idExpr, nameExpr, table, idExpr, searchParam, labelColumn, idExpr, labelColumn, table, limitParam)
	if fastStatement != "" {
		statement, args = managedUsageAdaptiveClassSQL(q, fastStatement, fastArgs, statement, args)
		statement = `SELECT * FROM (` + statement + `) choice(value,label,deleted) ORDER BY value COLLATE "C"`
	}
	return queryManagedUsageCurrentOptions(ctx, m.db, statement, args...)
}

func queryManagedUsageCurrentOptions(ctx context.Context, db *Database, statement string, args ...any) ([]managedUsageOption, error) {
	rows, err := db.Pool.Query(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []managedUsageOption{}
	for rows.Next() {
		var option managedUsageOption
		if err := rows.Scan(&option.Value, &option.Label, &option.Deleted); err != nil {
			return nil, err
		}
		if option.Label == "" {
			option.Label = option.Value
		}
		result = append(result, option)
	}
	return result, rows.Err()
}

// Search labels do not need credential metadata, notes or per-account group
// and proxy subqueries. statement is one of the fixed projections above.
func managedUsageLabelsPG(ctx context.Context, db *Database, statement string) (map[string]string, error) {
	rows, err := db.Pool.Query(ctx, statement)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	labels := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		labels[id] = name
	}
	return labels, rows.Err()
}

func (m *Metrics) managedUsageOptions(ctx context.Context, q managedLogQuery, field, search string, limit int, current map[string]string) ([]managedUsageOption, error) {
	// Current resources give renamed/deleted labels context; only historical
	// matches produce candidates, so zero-use current accounts do not pretend
	// to belong to the applied query's result set.
	makeOption := func(id, name string) managedUsageOption {
		label, exists := current[id]
		if field == "model" {
			label = id
			exists = true
		}
		if label == "" {
			label = name
		}
		if label == "" {
			label = id
		}
		return managedUsageOption{id, label, !exists}
	}
	result := []managedUsageOption{}
	matches := func(id, name string) bool {
		return search == "" || strings.Contains(strings.ToLower(id+" "+name+" "+current[id]), search)
	}
	if m.db == nil {
		values := map[string]string{}
		_, err := m.walkManagedFile(ctx, q.FileCeiling, func(r RequestRecord) error {
			if !q.matches(r) {
				return nil
			}
			add := func(id, name string) {
				if id != "" && matches(id, name) && name >= values[id] {
					values[id] = name
				}
			}
			switch field {
			case "account":
				add(r.AccountID, r.Account)
			case "key":
				add(r.KeyID, r.KeyName)
			case "model":
				add(r.Model, r.Model)
			case "group":
				for _, id := range r.Groups {
					add(id, id)
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		for id, name := range values {
			result = append(result, makeOption(id, name))
		}
		sort.Slice(result, func(i, j int) bool { return result[i].Value < result[j].Value })
		if len(result) > limit+1 {
			result = result[:limit+1]
		}
		return result, nil
	}
	source, where, args := q.readSourceSQL()
	source, where = managedUsageAnalyticsSource(source, where), "TRUE"
	idExpr, nameExpr, join := "COALESCE(record->>'account_id','')", "account_name", ""
	switch field {
	case "key":
		idExpr = "COALESCE(record->>'key_id','')"
		nameExpr = "key_name"
	case "model":
		idExpr = "model"
		nameExpr = "model"
	case "group":
		idExpr = "group_id"
		nameExpr = "group_id"
		join = " CROSS JOIN LATERAL jsonb_array_elements_text(COALESCE(record->'groups','[]'::jsonb)) g(group_id)"
	}
	// Search the historical label OR a current name. Stable IDs with equal
	// names remain separate. SQL uses POSITION rather than wildcard matching.
	currentMatches := []string{}
	if search != "" {
		for id, name := range current {
			if strings.Contains(strings.ToLower(name), search) {
				currentMatches = append(currentMatches, id)
			}
		}
	}
	if managedUsageRollupEligible(q) {
		return m.managedUsageOptionsRollup(ctx, q, field, search, limit, currentMatches, makeOption)
	}
	args = append(args, search, currentMatches, limit+1)
	n := len(args)
	statement := fmt.Sprintf("SELECT %s,max(%s COLLATE \"C\") FROM %s%s WHERE %s AND %s<>'' AND ($%d='' OR position($%d in lower(%s||' '||%s))>0 OR %s=ANY($%d::text[])) GROUP BY %s ORDER BY %s COLLATE \"C\" LIMIT $%d", idExpr, nameExpr, source, join, where, idExpr, n-2, n-2, idExpr, nameExpr, idExpr, n-1, idExpr, idExpr, n)
	if _, _, eligible := managedUsageClassRollup(q); eligible {
		fastSQL, fastArgs := managedUsageOptionsRollupSQL(q, field, search, limit, currentMatches)
		statement, args = managedUsageAdaptiveClassSQL(q, fastSQL, fastArgs, statement, args)
		statement = `SELECT * FROM (` + statement + `) choice(value,label) ORDER BY value COLLATE "C"`
	}
	rows, err := m.db.Pool.Query(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err = rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		result = append(result, makeOption(id, name))
	}
	return result, rows.Err()
}

func managedUsageOptionsRollupSQL(q managedLogQuery, field, search string, limit int, currentMatches []string) (string, []any) {
	source, args := managedUsageRollupSourceWithMaterialization(q, false)
	id := "resource_id"
	identity := "resource_id<>''"
	if field == "model" {
		id, identity = "name", "name<>''"
	}
	args = append(args, field, search, currentMatches, limit+1)
	n := len(args)
	statement := source + fmt.Sprintf(` SELECT %s,max(name COLLATE "C") FROM source
 WHERE kind=$%d AND %s AND ($%d='' OR position($%d in lower(%s||' '||name))>0 OR %s=ANY($%d::text[]))
 GROUP BY %s ORDER BY %s COLLATE "C" LIMIT $%d`, id, n-3, identity, n-2, n-2, id, id, n-1, id, id, n)
	return statement, args
}

func (m *Metrics) managedUsageOptionsRollup(ctx context.Context, q managedLogQuery, field, search string, limit int, currentMatches []string, makeOption func(string, string) managedUsageOption) ([]managedUsageOption, error) {
	statement, args := managedUsageOptionsRollupSQL(q, field, search, limit, currentMatches)
	rows, err := m.db.Pool.Query(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []managedUsageOption{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		result = append(result, makeOption(id, name))
	}
	return result, rows.Err()
}

func (p *Proxy) registerManagedUsageOptions(mux *http.ServeMux) {
	p.registerManagedIPGeoRoutes(mux)
	mux.HandleFunc("GET /api/logs/errors", p.handleManagedUsageErrors)
	mux.HandleFunc("GET /api/logs/{id}/review", p.handleManagedUsageReview)
	mux.HandleFunc("PUT /api/logs/{id}/review", p.handleManagedUsageReview)
	mux.HandleFunc("GET /api/logs/coverage", p.handleManagedUsageCoverage)
	mux.HandleFunc("POST /api/logs/snapshot", p.handleManagedUsageSnapshot)
	mux.HandleFunc("GET /api/logs/options", p.handleManagedUsageOptions)
	mux.HandleFunc("POST /api/accounts/usage-batch", p.handleManagedAccountUsageBatch)
}
