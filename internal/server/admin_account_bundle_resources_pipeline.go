package server

import (
	"context"
	"encoding/binary"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const managedResourcePipelineChunk = 32
const managedResourcePipelineLargeChunk = 128

const managedResourceGroupSQL = `WITH inserted AS (
 INSERT INTO groups(id,name,description) VALUES($1,$2,$3) RETURNING id
), audited AS (
 INSERT INTO audit_logs(action,entity_id) SELECT 'group.create',$1 FROM inserted
) SELECT id FROM inserted`

const managedResourceProxySQL = `WITH inserted AS (
 INSERT INTO proxies(id,name,type,host,port,username,password_cipher,enabled)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8)
 RETURNING id,extract(epoch from created_at)::bigint AS created_at,
 extract(epoch from updated_at)::bigint AS updated_at,version,
 octet_length(password_cipher)>0 AS has_password
), audited AS (
 INSERT INTO audit_logs(action,entity_id) SELECT 'proxy.restore',$1 FROM inserted
) SELECT id,created_at,updated_at,version,has_password FROM inserted`

type managedResourcePipelineItem struct {
	kind, sourceID, id, statement string
	params                        [][]byte
	paramFormats                  []int16
	resultFormats                 []int16
	proxy                         ProxyEntry
	localResult                   managedBundleResourceResult
}

// The original all-valid fast path remains a subset of the mixed planner.
// Inputs with dependencies and unknown trigger behavior retain the original
// ordered implementation, including post-commit credential validation errors.
func managedResourcePipelineEligible(input managedBundleResourcesInput) bool {
	count := len(input.Resources.Groups) + len(input.Resources.Proxies)
	if input.DryRun || input.Resources.Version != 1 || count < 2 || count > 10000 {
		return false
	}
	ids, names := map[string]bool{}, map[string]bool{}
	for _, g := range input.Resources.Groups {
		if g.ID == "" || ids["g:"+g.ID] || names["g:"+g.Name] || input.GroupMapping[g.ID] != "" || validGroupName(g.Name) != nil || len(g.Description) > 8192 {
			return false
		}
		ids["g:"+g.ID], names["g:"+g.Name] = true, true
	}
	for _, v := range input.Resources.Proxies {
		entry := ProxyEntry{Name: v.Name, Type: v.Type, Host: v.Host, Port: v.Port}
		if v.ID == "" || ids["p:"+v.ID] || names["p:"+v.Name] || input.ProxyMapping[v.ID] != "" || validProxy(entry) != nil || len(v.Username) > 4096 || len(v.Password) > 16384 || v.HasCredentials && !v.CredentialsIncluded || (v.Username != "" || v.Password != "") && !input.ConfirmProxyCredentials {
			return false
		}
		ids["p:"+v.ID], names["p:"+v.Name] = true, true
	}
	return true
}

// Invalid and mapped rows are local decisions, not dependencies between the
// remaining writes. Distinct names keep constraint failures independent: if a
// name repeats, whether a later row may create it depends on the earlier
// transaction result, so that case retains the original ordered fallback.
func managedResourceMixedPipelineEligible(input managedBundleResourcesInput) bool {
	count := len(input.Resources.Groups) + len(input.Resources.Proxies)
	if input.DryRun || input.Resources.Version != 1 || count < 2 || count > 10000 {
		return false
	}
	names := make(map[string]struct{}, count)
	for _, group := range input.Resources.Groups {
		key := "g:" + group.Name
		if _, found := names[key]; found {
			return false
		}
		names[key] = struct{}{}
	}
	for _, proxy := range input.Resources.Proxies {
		key := "p:" + proxy.Name
		if _, found := names[key]; found {
			return false
		}
		names[key] = struct{}{}
	}
	return true
}

// Match the legacy switch precedence and seen-ID behavior. These decisions
// are published only when their ordered position is reached, so cancellation
// does not pre-acknowledge later mapped rows.
func managedResourceLocalResults(input managedBundleResourcesInput, groupIDs, groupNames, proxyIDs, proxyNames map[string]bool) []managedBundleResourceResult {
	result := make([]managedBundleResourceResult, 0, len(input.Resources.Groups)+len(input.Resources.Proxies))
	seen := make(map[string]bool, cap(result))
	for _, g := range input.Resources.Groups {
		row := managedBundleResourceResult{Kind: "group", SourceID: g.ID, Status: "failed"}
		switch {
		case g.ID == "" || seen["g:"+g.ID] || validGroupName(g.Name) != nil || len(g.Description) > 8192:
			row.Code = "invalid_resource"
		case input.GroupMapping[g.ID] != "":
			if id := input.GroupMapping[g.ID]; groupIDs[id] {
				row.Status, row.ID = "mapped", id
			} else {
				row.Code = "mapping_not_found"
			}
		case groupNames[g.Name]:
			row.Code = "name_conflict"
		default:
			row.Status = "create"
		}
		seen["g:"+g.ID] = true
		result = append(result, row)
	}
	for _, v := range input.Resources.Proxies {
		row := managedBundleResourceResult{Kind: "proxy", SourceID: v.ID, Status: "failed"}
		entry := ProxyEntry{Name: v.Name, Type: v.Type, Host: v.Host, Port: v.Port}
		switch {
		case v.ID == "" || seen["p:"+v.ID] || validProxy(entry) != nil || len(v.Username) > 4096 || len(v.Password) > 16384:
			row.Code = "invalid_resource"
		case input.ProxyMapping[v.ID] != "":
			if id := input.ProxyMapping[v.ID]; proxyIDs[id] {
				row.Status, row.ID = "mapped", id
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
			row.Status = "create"
		}
		seen["p:"+v.ID] = true
		result = append(result, row)
	}
	return result
}

// Session locks have to outlive each independently committed item. Acquisition
// order matches the original restore path: name/import lock, then write gate.
// All proxy INSERT/UPDATE/DELETE statements take the shared gate via
// migration 018, including ordinary admin writers and direct SQL writes.
// Administrator DDL (including TRUNCATE/disabled triggers) is not a supported
// concurrent application write. A connection whose unlock cannot be proved is
// removed from the pool.
func releaseManagedResourceConnection(conn *pgxpool.Conn, nameLocked, writeLocked bool) {
	defer conn.Release()
	if !nameLocked && !writeLocked {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	unlock := func(sql string) bool {
		var released bool
		return conn.QueryRow(ctx, sql).Scan(&released) == nil && released
	}
	ok := true
	if writeLocked {
		ok = unlock(`SELECT pg_advisory_unlock(hashtextextended(current_schema() || ':proxy-resource-write',982))`)
	}
	if nameLocked && ok {
		ok = unlock(`SELECT pg_advisory_unlock(hashtextextended(current_schema() || ':proxy-resource-import',981))`)
	}
	if !ok {
		// Hijack prevents reuse even if closing the underlying socket times out.
		_ = conn.Hijack().Close(ctx)
	}
}

func managedResourcePipelineTriggersSafe(ctx context.Context, conn *pgxpool.Conn) (bool, error) {
	var safe bool
	err := conn.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM pg_trigger t WHERE t.tgrelid='proxies'::regclass
  AND t.tgname='proxy_resource_write_gate' AND t.tgenabled IN ('O','A')
  AND t.tgfoid='proxy_resource_write_gate()'::regprocedure)
 AND NOT EXISTS(SELECT 1 FROM pg_trigger t
  WHERE t.tgrelid=ANY(ARRAY['proxies'::regclass,'groups'::regclass,'audit_logs'::regclass])
   AND NOT t.tgisinternal AND t.tgenabled<>'D'
   AND NOT(t.tgrelid='proxies'::regclass AND t.tgname='proxy_resource_write_gate'
    AND t.tgfoid='proxy_resource_write_gate()'::regprocedure))`).Scan(&safe)
	return safe, err
}

func (p *Proxy) managedResourceValidatedProxies(ctx context.Context, conn *pgxpool.Conn) ([]ProxyEntry, error) {
	rows, err := conn.Query(ctx, "SELECT "+proxyColumns+",password_cipher FROM proxies ORDER BY created_at,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ProxyEntry
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var v ProxyEntry
		var cipher []byte
		if err := scanProxy(rows, &v, &cipher); err != nil {
			return nil, err
		}
		if len(cipher) > 0 {
			plain, err := p.Store.DB.Decrypt(cipher)
			if err != nil {
				return nil, err
			}
			v.Password = string(plain)
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

// Full validation is published only after the first successful proxy commit,
// matching the old post-commit List behavior. Later commits update one entry.
func (p *Proxy) publishManagedResourceProxy(v ProxyEntry, validated []ProxyEntry, first bool) {
	p.Proxies.mu.Lock()
	defer p.Proxies.mu.Unlock()
	if first {
		next := make(map[string]*ProxyEntry, len(validated)+1)
		for _, previous := range validated {
			copy := previous
			if current := p.Proxies.index[copy.ID]; current != nil && current.Version > copy.Version {
				copy = *current
			}
			next[copy.ID] = &copy
		}
		p.Proxies.index = next
	}
	if p.Proxies.index == nil {
		p.Proxies.index = map[string]*ProxyEntry{}
	}
	if current := p.Proxies.index[v.ID]; current == nil || current.Version <= v.Version {
		copy := v
		p.Proxies.index[v.ID] = &copy
	}
}

// Pipeline synchronization is an implicit-transaction COMMIT/error recovery
// boundary, not one transaction around the entire batch. Each resource and its
// audit are a single CTE statement. A later constraint failure or cancellation
// cannot roll back a previously acknowledged item. No new chunk is sent after
// cancellation; already dispatched work has the usual ambiguous network/commit
// boundary and is never retried automatically.
func (p *Proxy) importManagedBundleResourcesPipeline(ctx context.Context, input managedBundleResourcesInput) (managedBundleResourceReply, bool) {
	reply := managedBundleResourceReply{Results: []managedBundleResourceResult{}, GroupMapping: map[string]string{}, ProxyMapping: map[string]string{}}
	if p.Store.DB == nil || (!managedResourcePipelineEligible(input) && !managedResourceMixedPipelineEligible(input)) || ctx.Err() != nil {
		return reply, false
	}
	db := p.Store.DB
	changed := false
	defer func() {
		if changed {
			db.invalidate()
		}
	}()
	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		return reply, false
	}
	nameLocked, writeLocked := false, false
	defer func() { releaseManagedResourceConnection(conn, nameLocked, writeLocked) }()
	var validated []ProxyEntry
	// Extra group/audit triggers can change statement-level behavior too, so
	// even an all-group request keeps the original path when they are present.
	safe, e := managedResourcePipelineTriggersSafe(ctx, conn)
	if e != nil || !safe {
		return reply, false
	}
	if len(input.Resources.Proxies) > 0 {
		// Unknown triggers can alter other rows/passwords or audit side effects.
		// Keep their exact existing per-item post-commit validation semantics.
		var locked bool
		if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended(current_schema() || ':proxy-resource-import',981))`).Scan(&locked); err != nil || !locked {
			return reply, false
		}
		nameLocked = true
		if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended(current_schema() || ':proxy-resource-write',982))`).Scan(&locked); err != nil || !locked {
			return reply, false
		}
		writeLocked = true
		// Recheck after acquiring the gate. Runtime administrator DDL/disabled
		// integrity triggers are outside the supported concurrent-write model.
		safe, e = managedResourcePipelineTriggersSafe(ctx, conn)
		if e != nil || !safe {
			return reply, false
		}
		validated, err = p.managedResourceValidatedProxies(ctx, conn)
		if err != nil {
			return reply, false
		}
	}
	groupNames, mappedGroups := make([]string, 0, len(input.Resources.Groups)), make([]string, 0, len(input.Resources.Groups))
	for _, g := range input.Resources.Groups {
		groupNames = append(groupNames, g.Name)
		if id := input.GroupMapping[g.ID]; id != "" {
			mappedGroups = append(mappedGroups, id)
		}
	}
	knownGroupIDs, knownGroupNames, knownProxyIDs, knownProxyNames := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	if len(groupNames) > 0 {
		rows, queryErr := conn.Query(ctx, `SELECT id,name FROM groups WHERE name=ANY($1::text[]) OR id=ANY($2::text[])`, pgx.QueryExecModeExec, groupNames, mappedGroups)
		if queryErr != nil {
			return reply, false
		}
		for rows.Next() {
			var id, name string
			if queryErr = rows.Scan(&id, &name); queryErr != nil {
				break
			}
			knownGroupIDs[id], knownGroupNames[name] = true, true
		}
		rows.Close()
		if queryErr != nil || rows.Err() != nil {
			return reply, false
		}
	}
	for _, proxy := range validated {
		knownProxyIDs[proxy.ID], knownProxyNames[proxy.Name] = true, true
	}
	localResults := managedResourceLocalResults(input, knownGroupIDs, knownGroupNames, knownProxyIDs, knownProxyNames)
	items := make([]managedResourcePipelineItem, 0, len(localResults))
	hasGroupWrites, hasProxyWrites := false, false
	for i, g := range input.Resources.Groups {
		if localResults[i].Status != "create" {
			items = append(items, managedResourcePipelineItem{kind: "group", sourceID: g.ID, localResult: localResults[i]})
			continue
		}
		hasGroupWrites = true
		id := newID("g_")
		items = append(items, managedResourcePipelineItem{kind: "group", sourceID: g.ID, id: id, statement: "managed_resource_group_v1", params: [][]byte{[]byte(id), []byte(g.Name), []byte(g.Description)}})
	}
	for i, v := range input.Resources.Proxies {
		if ctx.Err() != nil {
			return reply, false
		}
		index := len(input.Resources.Groups) + i
		if localResults[index].Status != "create" {
			items = append(items, managedResourcePipelineItem{kind: "proxy", sourceID: v.ID, localResult: localResults[index]})
			continue
		}
		hasProxyWrites = true
		cipher := []byte{}
		if v.Password != "" {
			cipher, err = db.Encrypt([]byte(v.Password))
			if err != nil {
				return reply, false
			}
		}
		id := newID("p_")
		items = append(items, managedResourcePipelineItem{kind: "proxy", sourceID: v.ID, id: id, statement: "managed_resource_proxy_v1", proxy: ProxyEntry{
			ID: id, Name: v.Name, Type: v.Type, Host: v.Host, Port: v.Port,
			Username: v.Username, Password: v.Password, HasPassword: len(cipher) > 0,
			Version: 1, Enabled: v.Enabled,
		}, params: [][]byte{[]byte(id), []byte(v.Name), []byte(v.Type), []byte(v.Host), []byte(strconv.Itoa(v.Port)), []byte(v.Username), cipher, []byte(strconv.FormatBool(v.Enabled))}, paramFormats: []int16{0, 0, 0, 0, 0, 0, 1, 0}, resultFormats: []int16{0, 1, 1, 1, 1}})
	}
	if hasGroupWrites {
		if _, err = conn.Conn().Prepare(ctx, "managed_resource_group_v1", managedResourceGroupSQL); err != nil {
			return reply, false
		}
	}
	if hasProxyWrites {
		if _, err = conn.Conn().Prepare(ctx, "managed_resource_proxy_v1", managedResourceProxySQL); err != nil {
			return reply, false
		}
	}
	reply.Results = make([]managedBundleResourceResult, len(items))
	for i, item := range items {
		reply.Results[i] = managedBundleResourceResult{Kind: item.kind, SourceID: item.sourceID, Status: "failed", Code: "operation_failed"}
	}
	publishLocal := func(i int) {
		if ctx.Err() != nil {
			reply.Results[i].Code = "canceled"
			return
		}
		reply.Results[i] = items[i].localResult
		if row := reply.Results[i]; row.Status == "mapped" {
			if row.Kind == "group" {
				reply.GroupMapping[row.SourceID] = row.ID
			} else {
				reply.ProxyMapping[row.SourceID] = row.ID
			}
			reply.Succeeded++
		}
	}
	proxyPublished := false
	// Keep small chunks for cancellation granularity. Larger imports still
	// synchronize every item independently; the larger batch only amortizes
	// client flush/read boundaries between those per-item commit points.
	chunkSize := managedResourcePipelineChunk
	if len(items) >= managedResourcePipelineLargeChunk {
		chunkSize = managedResourcePipelineLargeChunk
	}
	for start := 0; start < len(items); start += chunkSize {
		if ctx.Err() != nil {
			for i := start; i < len(items); i++ {
				reply.Results[i].Code = "canceled"
			}
			break
		}
		end := min(start+chunkSize, len(items))
		firstWrite := start
		// A later transport failure must not erase the already reached local
		// prefix. Entirely local chunks do not need a protocol pipeline.
		for firstWrite < end && items[firstWrite].statement == "" {
			publishLocal(firstWrite)
			firstWrite++
		}
		if ctx.Err() != nil {
			break
		}
		if firstWrite == end {
			continue
		}
		pipe := conn.Conn().PgConn().StartPipeline(ctx)
		chunkWrites := false
		for _, item := range items[firstWrite:end] {
			if item.statement == "" {
				continue
			}
			chunkWrites = true
			pipe.SendQueryPrepared(item.statement, item.params, item.paramFormats, item.resultFormats)
			pipe.SendPipelineSync()
		}
		if err = pipe.Flush(); err != nil {
			_ = pipe.Close()
			break
		}
		// A transport loss after server commit can make acknowledgement
		// uncertain. Invalidate the aggregate cache even in that case.
		changed = changed || chunkWrites
		fatal := false
		for i := firstWrite; i < end; i++ {
			if items[i].statement == "" {
				publishLocal(i)
				continue
			}
			var fields [][]byte
			var itemErr error
			committed := false
			for !committed {
				value, readErr := pipe.GetResults()
				if readErr != nil {
					itemErr = readErr
					var pgErr *pgconn.PgError
					if !errors.As(readErr, &pgErr) {
						fatal = true
						break
					}
					continue
				}
				switch result := value.(type) {
				case *pgconn.ResultReader:
					data := result.Read()
					if data.Err != nil {
						itemErr = data.Err
					} else if len(data.Rows) == 1 && len(data.Rows[0]) > 0 {
						fields = data.Rows[0]
					} else {
						itemErr = errors.New("resource write result missing")
					}
				case *pgconn.PipelineSync:
					committed = true
				default:
					itemErr = errors.New("resource pipeline result missing")
					fatal = true
				}
				if fatal {
					break
				}
			}
			if itemErr != nil {
				if items[i].kind == "group" && ctx.Err() == nil {
					itemErr = safeDBError(itemErr)
				}
				if ctx.Err() != nil {
					itemErr = ctx.Err()
				}
				reply.Results[i].Code = managedAccountErrorCode(itemErr)
			} else if committed {
				item := items[i]
				valid := len(fields) == 1 && string(fields[0]) == item.id
				if item.kind == "proxy" {
					v := item.proxy
					// The write statement returns only the generated metadata needed
					// to publish the runtime entry. Avoid row_to_json + a second JSON
					// allocation/unmarshal for every imported proxy.
					valid = len(fields) == 5 && string(fields[0]) == item.id
					if valid {
						if len(fields[1]) != 8 || len(fields[2]) != 8 || len(fields[3]) != 8 || len(fields[4]) != 1 {
							itemErr = errors.New("resource write metadata malformed")
						} else {
							v.CreatedAt = int64(binary.BigEndian.Uint64(fields[1]))
							v.UpdatedAt = int64(binary.BigEndian.Uint64(fields[2]))
							v.Version = int64(binary.BigEndian.Uint64(fields[3]))
							v.HasPassword = fields[4][0] != 0
						}
						valid = itemErr == nil && v.ID == item.id
					}
					if valid {
						p.publishManagedResourceProxy(v, validated, !proxyPublished)
						proxyPublished = true
					}
				}
				if valid {
					reply.Results[i].ID, reply.Results[i].Status, reply.Results[i].Code = item.id, "created", ""
					if item.kind == "group" {
						reply.GroupMapping[item.sourceID] = item.id
					} else {
						reply.ProxyMapping[item.sourceID] = item.id
					}
					reply.Succeeded++
					changed = true
				}
			}
			if fatal {
				break
			}
		}
		closeErr := pipe.Close()
		if fatal || closeErr != nil {
			break
		}
	}
	if ctx.Err() != nil {
		for i := range reply.Results {
			if reply.Results[i].Status == "failed" && reply.Results[i].Code == "operation_failed" {
				reply.Results[i].Code = "canceled"
			}
		}
	}
	reply.Failed = len(reply.Results) - reply.Succeeded
	return reply, true
}
