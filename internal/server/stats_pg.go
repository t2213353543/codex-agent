package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const metricsTimeout = 30 * time.Second
const metricsAggregateLock int64 = 779642907

func NewPGMetrics(db *Database) (*Metrics, error) {
	if db == nil || db.Pool == nil {
		return nil, errors.New("metrics database is required")
	}
	location, err := configuredStatsLocation()
	if err != nil {
		return nil, fmt.Errorf("STATS_TIMEZONE: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), metricsTimeout)
	defer cancel()
	if err := db.Pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("metrics database: %w", err)
	}
	return &Metrics{db: db, location: location}, nil
}

func (m *Metrics) Timezone() string {
	if m.location == nil {
		return "Asia/Shanghai"
	}
	return m.location.String()
}

func (m *Metrics) TodayStart() int64 {
	location := m.location
	if location == nil {
		location, _ = time.LoadLocation("Asia/Shanghai")
	}
	now := time.Now().In(location)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location).Unix()
}

func configuredStatsLocation() (*time.Location, error) {
	zone := os.Getenv("STATS_TIMEZONE")
	if zone == "" {
		zone = "Asia/Shanghai"
	}
	return time.LoadLocation(zone)
}

func (m *Metrics) RetainedTotals() rangeTotals {
	return m.Totals(time.Now().Add(-metricsRetentionDays*24*time.Hour).Unix()/60*60, time.Now().Unix()+60)
}

// HealthError reports unresolved durable writes, including requests buffered for
// retry. A live process with pending writes must not report storage readiness.
func (m *Metrics) HealthError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writeErr
}

func (m *Metrics) reportReadError(err error) {
	m.mu.Lock()
	m.readFailures++
	m.mu.Unlock()
	log.Printf("metrics query failed: %v", safeDBError(err))
}

func (m *Metrics) ReadFailureVersion() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readFailures
}

var secretInMetricError = regexp.MustCompile(`(?i)(bearer\s+|(?:access_token|refresh_token|id_token|password|api_key|authorization)["'\s:=]+)[^\s,"'&}]+`)
var jwtInMetricError = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)?`)
var bearerInMetricError = regexp.MustCompile(`(?i)(bearer\s+)[^\s,"'&}]+`)
var proxyURLInMetricError = regexp.MustCompile(`(?i)((?:https?|socks5)://)[^/\s@]+@`)

func redactMetricError(value string) string {
	value, _ = redactMetricErrorObserved(value)
	return value
}

func redactMetricErrorObserved(value string) (string, bool) {
	value = bearerInMetricError.ReplaceAllString(value, "${1}[REDACTED]")
	value = proxyURLInMetricError.ReplaceAllString(value, "${1}[REDACTED]@")
	value = secretInMetricError.ReplaceAllString(value, "${1}[REDACTED]")
	value = jwtInMetricError.ReplaceAllString(value, "[REDACTED]")
	truncated := len(value) > 2000
	if truncated {
		value = value[:2000]
	}
	return strings.ToValidUTF8(value, ""), truncated
}

func prepareMetricRecord(rec RequestRecord) RequestRecord {
	if rec.Time == 0 {
		rec.Time = time.Now().Unix()
	}
	if rec.EventID == "" {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			panic("system random source unavailable")
		}
		rec.EventID = "e_" + hex.EncodeToString(random[:])
	}
	rec.Error = redactMetricError(rec.Error)
	return rec
}

func (m *Metrics) recordPG(rec RequestRecord) {
	rec = prepareMetricRecord(rec)
	ctx, cancel := context.WithTimeout(context.Background(), metricsTimeout)
	defer cancel()
	_, err := m.persistRecord(ctx, rec)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.pending = append(m.pending, rec)
		m.writeErr = fmt.Errorf("persist request event %s: %w", rec.EventID, safeDBError(err))
		log.Printf("metrics persistence failed: %v", m.writeErr)
	} else {
		m.lastWriteAt = time.Now()
	}
}

func (m *Metrics) savePG() error {
	m.mu.Lock()
	pending := append([]RequestRecord(nil), m.pending...)
	m.mu.Unlock()
	for _, rec := range pending {
		ctx, cancel := context.WithTimeout(context.Background(), metricsTimeout)
		_, err := m.persistRecord(ctx, rec)
		cancel()
		if err != nil {
			return fmt.Errorf("retry request event %s: %w", rec.EventID, safeDBError(err))
		}
		m.mu.Lock()
		for i, item := range m.pending {
			if item.EventID == rec.EventID {
				m.pending = append(m.pending[:i], m.pending[i+1:]...)
				break
			}
		}
		m.lastWriteAt = time.Now()
		if len(m.pending) == 0 {
			m.writeErr = nil
		}
		m.mu.Unlock()
	}
	ctx, cancel := context.WithTimeout(context.Background(), metricsTimeout)
	defer cancel()
	if err := m.db.Pool.Ping(ctx); err != nil {
		return fmt.Errorf("metrics database: %w", err)
	}
	return m.HealthError()
}

// ImportRecords accepts deterministic event IDs from the migration tool. A
// partially completed import can be rerun without incrementing usage twice.
func (m *Metrics) ImportRecords(ctx context.Context, records []RequestRecord) (inserted, duplicates int, err error) {
	if m.db == nil {
		return 0, 0, errors.New("log import requires PostgreSQL")
	}
	for _, rec := range records {
		if rec.EventID == "" {
			return inserted, duplicates, errors.New("import event_id is required")
		}
		ok, e := m.persistRecord(ctx, prepareMetricRecord(rec))
		if e != nil {
			return inserted, duplicates, e
		}
		if ok {
			inserted++
		} else {
			duplicates++
		}
	}
	return inserted, duplicates, nil
}

func (m *Metrics) persistRecord(ctx context.Context, rec RequestRecord) (bool, error) {
	for attempt := 0; ; attempt++ {
		inserted, err := m.persistRecordOnce(ctx, rec)
		var pgerr *pgconn.PgError
		if err == nil || attempt >= 2 || ctx.Err() != nil || !errors.As(err, &pgerr) || (pgerr.Code != "55P03" && pgerr.Code != "40001" && pgerr.Code != "40P01") {
			return inserted, err
		}
	}
}

func (m *Metrics) persistRecordOnce(ctx context.Context, rec RequestRecord) (bool, error) {
	tx, err := m.db.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background())
	// Writers share this advisory lock; rebuild takes its exclusive counterpart.
	// This guards the rebuild boundary without serializing unrelated writers.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock_shared(hashtextextended(current_schema(),$1))", metricsAggregateLock); err != nil {
		return false, err
	}
	var accountID *string
	if rec.AccountID != "" {
		var id string
		err = tx.QueryRow(ctx, "SELECT id FROM accounts WHERE id=$1", rec.AccountID).Scan(&id)
		if err == nil {
			accountID = &id
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
	} else if rec.Account != "" {
		var ids []string
		err = tx.QueryRow(ctx, "SELECT ARRAY(SELECT id FROM accounts WHERE display_name=$1 LIMIT 2)", rec.Account).Scan(&ids)
		if err != nil {
			return false, err
		}
		if len(ids) == 1 {
			accountID = &ids[0]
			rec.AccountID = ids[0]
		}
	}
	accountRef := rec.AccountID
	if accountRef == "" && rec.Account != "" {
		accountRef = "legacy:" + rec.Account
	}
	when := time.Unix(rec.Time, 0).UTC()
	data, err := json.Marshal(rec)
	if err != nil {
		return false, err
	}
	// The durable event is the idempotency gate for both the detail and all
	// aggregates. Returning only newly inserted events saves one round trip
	// while preserving duplicate suppression inside the existing transaction.
	err = tx.QueryRow(ctx, `WITH inserted AS (
		INSERT INTO usage_events(event_id,created_at,account_ref,account_name,key_id,status,input_tokens,output_tokens,cached_tokens,latency_ms,latency_known)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT(event_id) DO NOTHING RETURNING event_id
	) INSERT INTO request_logs(event_id,account_id,account_ref,account_name,created_at,status,model,key_name,record)
		SELECT event_id,$12,$3,$4,$2,$6,$13,$14,$15 FROM inserted RETURNING id`,
		rec.EventID, when, accountRef, rec.Account, rec.KeyID, rec.Status, rec.InputTokens, rec.OutputTokens, rec.CachedTokens, rec.LatencyMs, rec.LatencyKnown,
		accountID, rec.Model, rec.KeyName, data).Scan(&rec.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	data, err = json.Marshal(rec)
	if err != nil {
		return false, err
	}
	batch := &pgx.Batch{}
	if accountRef != "" {
		batch.Queue(`INSERT INTO account_request_state(account_ref,account_name,last_at,last_log_id,record) VALUES($1,$2,$3,$4,$5)
			ON CONFLICT(account_ref) DO UPDATE SET account_name=EXCLUDED.account_name,last_at=EXCLUDED.last_at,last_log_id=EXCLUDED.last_log_id,record=EXCLUDED.record
			WHERE (account_request_state.last_at,account_request_state.last_log_id)<(EXCLUDED.last_at,EXCLUDED.last_log_id)`, accountRef, rec.Account, when, rec.ID, data)
		if rec.AccountCredentialVersion > 0 {
			batch.Queue(`INSERT INTO account_credential_request_state(account_ref,credential_version,last_at,last_log_id,record)
				SELECT $1,$2,$3,$4,$5 WHERE EXISTS(SELECT 1 FROM account_credentials current_credential WHERE current_credential.account_id=$1 AND current_credential.credential_version=$2)
				ON CONFLICT(account_ref) DO UPDATE SET credential_version=EXCLUDED.credential_version,last_at=EXCLUDED.last_at,last_log_id=EXCLUDED.last_log_id,record=EXCLUDED.record
				WHERE EXCLUDED.credential_version>account_credential_request_state.credential_version
				   OR (EXCLUDED.credential_version=account_credential_request_state.credential_version
				       AND (account_credential_request_state.last_at,account_credential_request_state.last_log_id)<(EXCLUDED.last_at,EXCLUDED.last_log_id))`, accountRef, rec.AccountCredentialVersion, when, rec.ID, data)
		}
	}
	queueMetricAggregates(batch, rec, accountRef)
	if err = tx.SendBatch(ctx, batch).Close(); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

const metricsColumns = "requests,ok,errors,tokens,input,output,cached,latency,latency_n"
const metricsSums = "COALESCE(sum(requests),0)::bigint,COALESCE(sum(ok),0)::bigint,COALESCE(sum(errors),0)::bigint,COALESCE(sum(tokens),0)::bigint,COALESCE(sum(input),0)::bigint,COALESCE(sum(output),0)::bigint,COALESCE(sum(cached),0)::bigint,COALESCE(sum(latency),0)::bigint,COALESCE(sum(latency_n),0)::bigint"

func aggregateUpdates(table string) string {
	parts := make([]string, 0, 9)
	for _, name := range strings.Split(metricsColumns, ",") {
		parts = append(parts, name+"="+table+"."+name+"+EXCLUDED."+name)
	}
	return strings.Join(parts, ",")
}

func queueMetricAggregates(batch *pgx.Batch, rec RequestRecord, accountRef string) {
	var okCount, errorCount, latencyN, latency int64
	if rec.Status == "ok" {
		okCount = 1
	} else {
		errorCount = 1
	}
	if rec.hasMeasuredLatency() {
		latency = rec.LatencyMs
		latencyN = 1
	}
	values := []any{int64(1), okCount, errorCount, rec.tokens(), rec.InputTokens, rec.OutputTokens, rec.CachedTokens, latency, latencyN}
	for _, bucket := range []struct {
		table   string
		seconds int64
	}{{"usage_minute", 60}, {"usage_daily", 86400}} {
		args := append([]any{rec.Time / bucket.seconds * bucket.seconds, accountRef, rec.Account}, values...)
		batch.Queue("INSERT INTO "+bucket.table+"(bucket,account_ref,account_name,"+metricsColumns+") VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT(bucket,account_ref) DO UPDATE SET account_name=EXCLUDED.account_name,"+aggregateUpdates(bucket.table), args...)
	}
	scopes := [][2]string{{"global", ""}}
	if rec.KeyID != "" {
		scopes = append(scopes, [2]string{"key", rec.KeyID})
	}
	if accountRef != "" {
		scopes = append(scopes, [2]string{"account", accountRef})
	}
	for _, scope := range scopes {
		args := append([]any{scope[0], scope[1]}, values...)
		args = append(args, rec.Time)
		batch.Queue("INSERT INTO usage_totals(scope,subject,"+metricsColumns+",last_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT(scope,subject) DO UPDATE SET "+aggregateUpdates("usage_totals")+",last_at=GREATEST(usage_totals.last_at,EXCLUDED.last_at)", args...)
	}
}

func totalsTargets(t *rangeTotals) []any {
	return []any{&t.Requests, &t.OK, &t.Errors, &t.Tokens, &t.Input, &t.Output, &t.Cached, &t.Latency, &t.LatencyN}
}

func (m *Metrics) totalsPG(from, to int64) (totals rangeTotals) {
	ctx, cancel := context.WithTimeout(context.Background(), metricsTimeout)
	defer cancel()
	source, args := globalMetricsRangeSQL(from, to)
	if err := m.db.Pool.QueryRow(ctx, "SELECT "+metricsSums+" FROM ("+source+") selected", args...).Scan(totalsTargets(&totals)...); err != nil {
		m.reportReadError(err)
	}
	return
}

func (m *Metrics) grandTotalsPG() (totals rangeTotals) {
	ctx, cancel := context.WithTimeout(context.Background(), metricsTimeout)
	defer cancel()
	err := m.db.Pool.QueryRow(ctx, "SELECT "+metricsColumns+" FROM usage_totals WHERE scope='global' AND subject=''").Scan(totalsTargets(&totals)...)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		m.reportReadError(err)
	}
	return
}

func (m *Metrics) seriesPG(from, to, step int64) []SeriesPoint {
	if step < 60 {
		step = 60
	}
	if to <= from {
		to = from + step
	}
	if (to-from)/step > 10000 {
		step = (to-from)/10000 + 1
	}
	points := make([]SeriesPoint, int((to-from)/step)+1)
	for i := range points {
		points[i].Ts = from + int64(i)*step
	}
	ctx, cancel := context.WithTimeout(context.Background(), metricsTimeout)
	defer cancel()
	statement := "SELECT (bucket-$1)/$3 AS slot," + metricsSums + " FROM usage_minute WHERE bucket >= $1 AND bucket < $2 GROUP BY slot"
	args := []any{from, to, step}
	if from >= 0 && from%3600 == 0 && step%3600 == 0 {
		// Aligned chart slots contain whole hours. An unaligned final end only
		// splits the last hour; it must not force the entire history back onto
		// account-minute rows. Both branches retain their exact slot bucket.
		if to%3600 == 0 {
			statement = "SELECT (bucket-$1)/$3 AS slot," + metricsSums + " FROM usage_hourly_global WHERE bucket >= $1 AND bucket < $2 GROUP BY slot"
		} else {
			args = append(args, to/3600*3600)
			statement = "SELECT (bucket-$1)/$3 AS slot," + metricsSums + " FROM (SELECT bucket," + metricsColumns + " FROM usage_hourly_global WHERE bucket >= $1 AND bucket < $4 UNION ALL SELECT bucket," + metricsColumns + " FROM usage_minute WHERE bucket >= $4 AND bucket < $2) selected GROUP BY slot"
		}
	} else if from >= 0 && step == 86400 && from%3600 != 0 {
		// Daily slots starting at an unaligned minute straddle exactly one hour
		// per day: the hour holding `from` repeats daily. Serve the 23 complete
		// hours per slot from the hourly rollup and only the straddled hour's
		// minutes from the minute table. The EXISTS series drives one bounded
		// (bucket,account_ref) probe per day instead of scanning the window.
		statement = "SELECT slot," + metricsSums + " FROM (" +
			"SELECT (b.bucket-$1::bigint)/$3::bigint AS slot," + metricsColumns + " FROM usage_hourly_global b" +
			" WHERE b.bucket >= $1::bigint AND b.bucket+3600 <= $2::bigint AND (b.bucket-$1::bigint)%$3::bigint <= $3::bigint-3600" +
			" UNION ALL" +
			" SELECT (b.bucket-$1::bigint)/$3::bigint AS slot," + metricsColumns + " FROM usage_minute b" +
			" WHERE b.bucket >= $1::bigint AND b.bucket < $2::bigint" +
			" AND EXISTS (SELECT 1 FROM generate_series($1::bigint/86400*86400 + $1::bigint%86400/3600*3600, $2::bigint, 86400) g(d)" +
			" WHERE b.bucket >= g.d AND b.bucket < g.d+3600)) selected GROUP BY slot"
	}
	rows, err := m.db.Pool.Query(ctx, statement, args...)
	if err != nil {
		m.reportReadError(err)
		return points
	}
	defer rows.Close()
	for rows.Next() {
		var i int
		var t rangeTotals
		targets := append([]any{&i}, totalsTargets(&t)...)
		if err = rows.Scan(targets...); err != nil {
			m.reportReadError(err)
			return points
		}
		if i >= 0 && i < len(points) {
			points[i] = SeriesPoint{Ts: points[i].Ts, Requests: t.Requests, OK: t.OK, Errors: t.Errors, Tokens: t.Tokens, Input: t.Input, Output: t.Output, Cached: t.Cached, LatencyMs: t.Latency}
		}
	}
	if err = rows.Err(); err != nil {
		m.reportReadError(err)
	}
	return points
}

type AccountMetricsSummary struct {
	Usage   dayCounts
	Last    RequestRecord
	HasLast bool
}

// BatchAccountMetrics executes three bounded queries for the requested page.
// Keys may be stable IDs (preferred) or unambiguous legacy display names.
func (m *Metrics) BatchAccountMetrics(ctx context.Context, starts map[string]int64) (map[string]AccountMetricsSummary, error) {
	result := make(map[string]AccountMetricsSummary, len(starts))
	if m.db == nil {
		for account, start := range starts {
			last, has := m.LastFor(account)
			result[account] = AccountMetricsSummary{Usage: m.AccountWindow(account, start), Last: last, HasLast: has}
		}
		return result, nil
	}
	if len(starts) > 1000 {
		return nil, errors.New("account metrics batch exceeds 1000 accounts")
	}
	keys := make([]string, 0, len(starts))
	for key := range starts {
		keys = append(keys, key)
		result[key] = AccountMetricsSummary{}
	}
	if len(keys) == 0 {
		return result, nil
	}
	rows, err := m.db.Pool.Query(ctx, "SELECT id,display_name FROM accounts WHERE id=ANY($1::text[]) OR display_name=ANY($1::text[])", keys)
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	byName := map[string][]string{}
	for rows.Next() {
		var id, name string
		if err = rows.Scan(&id, &name); err != nil {
			rows.Close()
			return nil, err
		}
		ids[id] = true
		byName[name] = append(byName[name], id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	identities := []string{}
	refs := []string{}
	bounds := []int64{}
	refKeys := map[string][]string{}
	minStart := int64(1<<63 - 1)
	for _, key := range keys {
		selected := []string{key, "legacy:" + key}
		if ids[key] {
			selected = []string{key}
		} else if len(byName[key]) > 0 {
			selected = byName[key]
		}
		start := starts[key] / 86400 * 86400
		if start < minStart {
			minStart = start
		}
		for _, ref := range selected {
			identities = append(identities, key)
			refs = append(refs, ref)
			bounds = append(bounds, start)
			refKeys[ref] = append(refKeys[ref], key)
		}
	}
	// Materializing the indexed account_ref selection makes the storage scan
	// occur once. An OR join on ID/name formerly rescanned every date bucket
	// for each account on a page (hundreds of times for a 500-account page).
	rows, err = m.db.Pool.Query(ctx, `WITH wanted AS (SELECT * FROM unnest($1::text[],$2::text[],$3::bigint[]) AS w(identity,account_ref,start_at)),
		selected AS MATERIALIZED (SELECT * FROM usage_daily WHERE account_ref=ANY($2::text[]) AND bucket >= $4)
		SELECT w.identity,`+metricsSums+` FROM wanted w JOIN selected u ON u.account_ref=w.account_ref AND u.bucket>=w.start_at GROUP BY w.identity`, identities, refs, bounds, minStart)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key string
		var t rangeTotals
		if err = rows.Scan(append([]any{&key}, totalsTargets(&t)...)...); err != nil {
			rows.Close()
			return nil, err
		}
		summary := result[key]
		summary.Usage = dayCounts(t)
		result[key] = summary
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = m.db.Pool.Query(ctx, `SELECT account_ref,record FROM account_request_state WHERE account_ref=ANY($1::text[])`, refs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref string
		var raw []byte
		if err = rows.Scan(&ref, &raw); err != nil {
			return nil, err
		}
		var rec RequestRecord
		if err = json.Unmarshal(raw, &rec); err != nil {
			return nil, err
		}
		for _, key := range refKeys[ref] {
			summary := result[key]
			if !summary.HasLast || rec.Time > summary.Last.Time || (rec.Time == summary.Last.Time && rec.ID > summary.Last.ID) {
				summary.Last = rec
				summary.HasLast = true
				result[key] = summary
			}
		}
	}
	return result, rows.Err()
}

func (m *Metrics) accountWindowPG(account string, start int64) dayCounts {
	ctx, cancel := context.WithTimeout(context.Background(), metricsTimeout)
	defer cancel()
	all, err := m.BatchAccountMetrics(ctx, map[string]int64{account: start})
	if err != nil {
		m.reportReadError(err)
	}
	return all[account].Usage
}

func (m *Metrics) lastForPG(account string) (RequestRecord, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), metricsTimeout)
	defer cancel()
	var raw []byte
	err := m.db.Pool.QueryRow(ctx, `SELECT record FROM (
		SELECT record,last_at,last_log_id FROM account_request_state WHERE account_ref=$1
		UNION ALL
		SELECT record,last_at,last_log_id FROM account_request_state WHERE account_name=$1
	) u ORDER BY last_at DESC,last_log_id DESC LIMIT 1`, account).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return RequestRecord{}, false
	}
	if err != nil {
		m.reportReadError(err)
		return RequestRecord{}, false
	}
	var rec RequestRecord
	if err = json.Unmarshal(raw, &rec); err != nil {
		m.reportReadError(err)
		return RequestRecord{}, false
	}
	return rec, true
}

func (m *Metrics) keyUsagePG(keyID string) (usage usageCounts) {
	ctx, cancel := context.WithTimeout(context.Background(), metricsTimeout)
	defer cancel()
	err := m.db.Pool.QueryRow(ctx, "SELECT requests,tokens,errors,last_at FROM usage_totals WHERE scope='key' AND subject=$1", keyID).Scan(&usage.Requests, &usage.Tokens, &usage.Errors, &usage.LastAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		m.reportReadError(err)
	}
	return
}

func (m *Metrics) accountTopPG(from, to int64, limit int) []struct {
	Name     string `json:"name"`
	Tokens   int64  `json:"tokens"`
	Requests int64  `json:"requests"`
	Errors   int64  `json:"errors"`
} {
	result := make([]struct {
		Name     string `json:"name"`
		Tokens   int64  `json:"tokens"`
		Requests int64  `json:"requests"`
		Errors   int64  `json:"errors"`
	}, 0)
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	ctx, cancel := context.WithTimeout(context.Background(), metricsTimeout)
	defer cancel()
	statement, args := accountTopQuerySQL(from, to, limit)
	rows, err := m.db.Pool.Query(ctx, statement, args...)
	if err != nil {
		m.reportReadError(err)
		return result
	}
	defer rows.Close()
	for rows.Next() {
		var item struct {
			Name     string `json:"name"`
			Tokens   int64  `json:"tokens"`
			Requests int64  `json:"requests"`
			Errors   int64  `json:"errors"`
		}
		if err = rows.Scan(&item.Name, &item.Tokens, &item.Requests, &item.Errors); err != nil {
			m.reportReadError(err)
			return result
		}
		result = append(result, item)
	}
	if err = rows.Err(); err != nil {
		m.reportReadError(err)
	}
	return result
}

type logCursor struct {
	Time int64 `json:"t"`
	ID   int64 `json:"i"`
}

func encodeLogCursor(rec RequestRecord) string {
	raw, _ := json.Marshal(logCursor{rec.Time, rec.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}
func decodeLogCursor(value string) (logCursor, error) {
	var cursor logCursor
	if len(value) > 256 {
		return cursor, errors.New("invalid log cursor")
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursor, errors.New("invalid log cursor")
	}
	if err = json.Unmarshal(raw, &cursor); err != nil || cursor.Time <= 0 || cursor.ID <= 0 {
		return cursor, errors.New("invalid log cursor")
	}
	return cursor, nil
}

func (m *Metrics) logsPG(query LogQuery) (LogPage, error) {
	page := LogPage{Total: -1, TotalKind: "not_computed", Items: []RequestRecord{}}
	if query.Size <= 0 || query.Size > 500 {
		query.Size = 50
	}
	if query.Page > 100 {
		return page, errors.New("legacy log page exceeds 100; use next_cursor")
	}
	if query.Cursor != "" && query.Page > 1 {
		return page, errors.New("cursor cannot be combined with page > 1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), metricsTimeout)
	defer cancel()
	conditions := []string{"TRUE"}
	args := make([]any, 0, 10)
	add := func(sql string, value any) {
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf(sql, len(args)))
	}
	if query.From != 0 {
		add("created_at >= $%d", time.Unix(query.From, 0).UTC())
	}
	if query.To != 0 {
		add("created_at < $%d", time.Unix(query.To, 0).UTC())
	}
	if query.AccountID != "" {
		add("account_ref = $%d", query.AccountID)
	} else if query.Account != "" {
		args = append(args, query.Account)
		conditions = append(conditions, fmt.Sprintf("(account_ref=$%d OR account_name=$%d)", len(args), len(args)))
	}
	if query.KeyName != "" {
		add("key_name = $%d", query.KeyName)
	}
	if query.Model != "" {
		add("model = $%d", query.Model)
	}
	// The filter-tags GIN projection accelerates the equality filters above;
	// the plain expressions stay as rechecks so column collation and
	// arbitrarily long values keep their exact semantics. A COLLATE "C"
	// BTree here would silently drop values above the partial-index bound.
	if query.KeyName != "" || query.Model != "" {
		tags := struct {
			KeyName string `json:"key_name,omitempty"`
			Model   string `json:"model,omitempty"`
		}{query.KeyName, query.Model}
		raw, _ := json.Marshal(tags)
		add("request_log_filter_tags(record,model,key_name,status) @> $%d::jsonb", string(raw))
	}
	if query.Status == "failed" {
		conditions = append(conditions, "status <> 'ok'")
	} else if query.Status != "" {
		add("status = $%d", query.Status)
	}
	if query.Cursor != "" {
		cursor, err := decodeLogCursor(query.Cursor)
		if err != nil {
			return page, err
		}
		args = append(args, time.Unix(cursor.Time, 0).UTC(), cursor.ID)
		conditions = append(conditions, fmt.Sprintf("(created_at,id)<($%d,$%d)", len(args)-1, len(args)))
	}
	args = append(args, query.Size+1)
	// Page N is served by one indexed seek-and-skip instead of N sequential
	// cursor probes: Page is capped at 100, so OFFSET skips at most 49500
	// index entries — far cheaper than up to 100 round trips. HasMore and
	// NextCursor still derive from the size+1 fetch exactly as before.
	statement := "SELECT id,record FROM request_logs WHERE " + strings.Join(conditions, " AND ") + fmt.Sprintf(" ORDER BY created_at DESC,id DESC LIMIT $%d", len(args))
	if query.Page > 1 {
		args = append(args, (query.Page-1)*query.Size)
		statement += fmt.Sprintf(" OFFSET $%d", len(args))
	}
	rows, err := m.db.Pool.Query(ctx, statement, args...)
	if err != nil {
		return page, err
	}
	items := make([]RequestRecord, 0, query.Size+1)
	for rows.Next() {
		var id int64
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return page, err
		}
		var rec RequestRecord
		if err = json.Unmarshal(raw, &rec); err != nil {
			rows.Close()
			return page, err
		}
		rec.ID = id
		items = append(items, rec)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	page.HasMore = len(items) > query.Size
	if page.HasMore {
		items = items[:query.Size]
	}
	page.Items = items
	page.NextCursor = ""
	if page.HasMore {
		page.NextCursor = encodeLogCursor(items[len(items)-1])
	}
	return page, nil
}

// RebuildAggregates is an explicit maintenance operation; the exclusive lock
// pauses event commits while rebuilding, so no concurrent event is double-counted.
func (m *Metrics) RebuildAggregates(ctx context.Context) error {
	if m.db == nil {
		return errors.New("aggregate rebuild requires PostgreSQL")
	}
	tx, err := m.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	// Large-history rebuilds legitimately exceed the interactive pool deadline;
	// SET LOCAL scopes the raised statement timeout to this transaction only.
	if _, err = tx.Exec(ctx, "SET LOCAL statement_timeout='30min'"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(current_schema(),$1))", metricsAggregateLock); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "TRUNCATE usage_minute,usage_daily,usage_totals"); err != nil {
		return err
	}
	// TRUNCATE already holds ACCESS EXCLUSIVE on usage_minute until commit,
	// and the advisory boundary excludes normal event writers. Replaying its
	// per-row hourly trigger for a million distinct account-minutes repeatedly
	// updates the same few hourly rows. Build those totals once after the
	// canonical minute and legacy state is complete instead. Trigger state is
	// transactional: a failure rolls this change back along with the rebuild.
	if _, err = tx.Exec(ctx, "ALTER TABLE usage_minute DISABLE TRIGGER usage_minute_hourly_change"); err != nil {
		return err
	}
	expressions := `count(*)::bigint,count(*) FILTER(WHERE status='ok')::bigint,count(*) FILTER(WHERE status<>'ok')::bigint,sum(input_tokens+output_tokens)::bigint,sum(input_tokens)::bigint,sum(output_tokens)::bigint,sum(cached_tokens)::bigint,COALESCE(sum(latency_ms) FILTER(WHERE latency_ms>=0 AND COALESCE(latency_known,latency_ms>0)),0)::bigint,count(*) FILTER(WHERE latency_ms>=0 AND COALESCE(latency_known,latency_ms>0))::bigint`
	for _, bucket := range []struct {
		table   string
		seconds int64
	}{{"usage_minute", 60}, {"usage_daily", 86400}} {
		statement := fmt.Sprintf("INSERT INTO %s(bucket,account_ref,account_name,%s) SELECT (extract(epoch FROM created_at)::bigint/%d)*%d AS bucket,account_ref,max(account_name),%s FROM usage_events GROUP BY bucket,account_ref", bucket.table, metricsColumns, bucket.seconds, bucket.seconds, expressions)
		if _, err = tx.Exec(ctx, statement); err != nil {
			return err
		}
	}
	for _, scope := range []struct{ scope, subject, filter string }{{"global", "''", "TRUE"}, {"account", "account_ref", "account_ref<>''"}, {"key", "key_id", "key_id<>''"}} {
		statement := fmt.Sprintf("INSERT INTO usage_totals(scope,subject,%s,last_at) SELECT '%s',%s,%s,max(extract(epoch FROM created_at))::bigint FROM usage_events WHERE %s GROUP BY 1,2", metricsColumns, scope.scope, scope.subject, expressions, scope.filter)
		if _, err = tx.Exec(ctx, statement); err != nil {
			return err
		}
	}
	if err = applyLegacyBaselines(ctx, tx, ""); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO usage_hourly_global(bucket,"+metricsColumns+") SELECT (bucket/3600)*3600,"+metricsSums+" FROM usage_minute GROUP BY 1 ORDER BY 1"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "ALTER TABLE usage_minute ENABLE TRIGGER usage_minute_hourly_change"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Prune removes a bounded batch of old detail rows. Durable usage_events and
// lifetime totals are retained so historical usage remains reconstructible.
func (m *Metrics) Prune(ctx context.Context, before time.Time, limit int) (int64, error) {
	if m.db == nil {
		return 0, errors.New("prune requires PostgreSQL")
	}
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	command, err := m.db.Pool.Exec(ctx, "DELETE FROM request_logs WHERE id IN (SELECT id FROM request_logs WHERE created_at<$1 ORDER BY created_at,id LIMIT $2)", before, limit)
	if err != nil {
		return 0, err
	}
	return command.RowsAffected(), nil
}

// ExportRecords streams every durable usage event, even when its verbose
// request log has expired. Expired detail fields (endpoint/error/IP) remain
// absent; timestamps, identity snapshots and usage counts remain exact.
func (m *Metrics) ExportRecords(ctx context.Context, writer io.Writer) error {
	if m.db == nil {
		return errors.New("export requires PostgreSQL")
	}
	rows, err := m.db.Pool.Query(ctx, `SELECT e.event_id,extract(epoch FROM e.created_at)::bigint,e.account_ref,e.account_name,e.key_id,e.status,e.input_tokens,e.output_tokens,e.cached_tokens,e.latency_ms,e.latency_known,l.record,a.display_name,l.id
		FROM usage_events e LEFT JOIN request_logs l ON l.event_id=e.event_id LEFT JOIN accounts a ON a.id=e.account_ref ORDER BY e.created_at,l.id NULLS FIRST,e.event_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	encoder := json.NewEncoder(writer)
	for rows.Next() {
		var rec RequestRecord
		var accountRef string
		var raw []byte
		var currentName *string
		var logID *int64
		if err = rows.Scan(&rec.EventID, &rec.Time, &accountRef, &rec.Account, &rec.KeyID, &rec.Status, &rec.InputTokens, &rec.OutputTokens, &rec.CachedTokens, &rec.LatencyMs, &rec.LatencyKnown, &raw, &currentName, &logID); err != nil {
			return err
		}
		if len(raw) > 0 {
			if err = json.Unmarshal(raw, &rec); err != nil {
				return err
			}
		} else if !strings.HasPrefix(accountRef, "legacy:") {
			rec.AccountID = accountRef
		}
		if currentName != nil && *currentName != rec.Account {
			rec.AccountNameSnapshot = rec.Account
			rec.Account = *currentName
		}
		if logID != nil {
			rec.ID = *logID
		}
		if err = encoder.Encode(rec); err != nil {
			return err
		}
	}
	return rows.Err()
}
