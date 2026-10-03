package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
)

func importLegacyStatsChecked(ctx context.Context, m *Metrics, state statsState, manifest string) error {
	return m.ImportLegacyStats(ctx, state, manifest)
}

// ImportLegacyStats preserves the portion of an old snapshot not represented
// by durable events. The caller imports logs first, uses a stable manifest, and
// keeps the legacy writer stopped throughout the migration. Negative deltas
// indicate an inconsistent snapshot and reject the complete baseline transaction.
func (m *Metrics) ImportLegacyStats(ctx context.Context, state statsState, manifest string) error {
	if m.db == nil {
		return errors.New("legacy stats import requires PostgreSQL")
	}
	if manifest == "" {
		return errors.New("legacy stats manifest is required")
	}
	tx, err := m.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(current_schema(),$1))", metricsAggregateLock); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, "INSERT INTO usage_legacy_imports(manifest) VALUES($1) ON CONFLICT DO NOTHING", manifest)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return tx.Commit(ctx)
	}
	for bucket, counts := range state.Minutes {
		if counts == nil {
			continue
		}
		if bucket <= 0 || bucket%60 != 0 {
			return errors.New("legacy stats contain an invalid minute bucket")
		}
		var current rangeTotals
		if err = tx.QueryRow(ctx, "SELECT "+metricsSums+" FROM usage_minute WHERE bucket=$1", bucket).Scan(totalsTargets(&current)...); err != nil {
			return err
		}
		delta, err := legacyStatsDelta(rangeTotals(*counts), current)
		if err != nil {
			return fmt.Errorf("legacy minute %d: %w", bucket, err)
		}
		if delta == (rangeTotals{}) {
			continue
		}
		if err = saveLegacyBaseline(ctx, tx, manifest, "minute", bucket, "", "", delta); err != nil {
			return err
		}
	}
	for name, days := range state.Accounts {
		var ids []string
		if err = tx.QueryRow(ctx, "SELECT ARRAY(SELECT id FROM accounts WHERE display_name=$1 LIMIT 2)", name).Scan(&ids); err != nil {
			return err
		}
		if len(ids) > 1 {
			return errors.New("legacy statistics contain an ambiguous account name")
		}
		subject := "legacy:" + name
		if len(ids) == 1 {
			subject = ids[0]
		}
		for bucket, counts := range days {
			if counts == nil {
				continue
			}
			if bucket <= 0 || bucket%86400 != 0 {
				return errors.New("legacy stats contain an invalid day bucket")
			}
			var current rangeTotals
			if err = tx.QueryRow(ctx, "SELECT "+metricsSums+" FROM usage_daily WHERE bucket=$1 AND (account_ref=$2 OR account_name=$3)", bucket, subject, name).Scan(totalsTargets(&current)...); err != nil {
				return err
			}
			delta, err := legacyStatsDelta(rangeTotals(*counts), current)
			if err != nil {
				return fmt.Errorf("legacy account day %d: %w", bucket, err)
			}
			if delta == (rangeTotals{}) {
				continue
			}
			if err = saveLegacyBaseline(ctx, tx, manifest, "account_day", bucket, subject, name, delta); err != nil {
				return err
			}
		}
	}
	if err = applyLegacyBaselines(ctx, tx, manifest); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func legacyStatsDelta(snapshot, current rangeTotals) (rangeTotals, error) {
	old := []int64{snapshot.Requests, snapshot.OK, snapshot.Errors, snapshot.Tokens, snapshot.Input, snapshot.Output, snapshot.Cached, snapshot.Latency, snapshot.LatencyN}
	now := []int64{current.Requests, current.OK, current.Errors, current.Tokens, current.Input, current.Output, current.Cached, current.Latency, current.LatencyN}
	if snapshot.Requests != snapshot.OK+snapshot.Errors || snapshot.Tokens != snapshot.Input+snapshot.Output {
		return rangeTotals{}, errors.New("snapshot metric totals are inconsistent")
	}
	hasLower, hasHigher := false, false
	for i := range old {
		if old[i] < 0 {
			return rangeTotals{}, errors.New("snapshot metrics cannot be negative")
		}
		if old[i] < now[i] {
			hasLower = true
		}
		if old[i] > now[i] {
			hasHigher = true
		}
	}
	// Logs may include requests committed after the old stats snapshot was
	// saved. Complete event dominance is safe: preserve events without a delta.
	if hasLower && !hasHigher {
		return rangeTotals{}, nil
	}
	if hasLower && hasHigher {
		return rangeTotals{}, errors.New("snapshot metrics and imported events overlap inconsistently; reconcile before cutover")
	}
	for i := range old {
		old[i] -= now[i]
	}
	return rangeTotals{Requests: old[0], OK: old[1], Errors: old[2], Tokens: old[3], Input: old[4], Output: old[5], Cached: old[6], Latency: old[7], LatencyN: old[8]}, nil
}

func saveLegacyBaseline(ctx context.Context, tx pgx.Tx, manifest, scope string, bucket int64, subject, name string, t rangeTotals) error {
	_, err := tx.Exec(ctx, "INSERT INTO usage_legacy_baselines(manifest,scope,bucket,subject,account_name,"+metricsColumns+") VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)", manifest, scope, bucket, subject, name, t.Requests, t.OK, t.Errors, t.Tokens, t.Input, t.Output, t.Cached, t.Latency, t.LatencyN)
	return err
}

// applyLegacyBaselines runs after the event-derived aggregates are rebuilt or
// for exactly one newly inserted manifest. Account snapshots do not contribute
// to global totals a second time.
func applyLegacyBaselines(ctx context.Context, tx pgx.Tx, manifest string) error {
	where := ""
	args := []any{}
	if manifest != "" {
		where = " AND manifest=$1"
		args = append(args, manifest)
	}
	statements := []string{
		"INSERT INTO usage_minute(bucket,account_ref,account_name," + metricsColumns + ") SELECT bucket,'baseline:'||manifest,''," + metricsColumns + " FROM usage_legacy_baselines WHERE scope='minute'" + where + " ON CONFLICT(bucket,account_ref) DO UPDATE SET " + aggregateUpdates("usage_minute"),
		"INSERT INTO usage_daily(bucket,account_ref,account_name," + metricsColumns + ") SELECT bucket,subject,max(account_name)," + metricsSums + " FROM usage_legacy_baselines WHERE scope='account_day'" + where + " GROUP BY bucket,subject ON CONFLICT(bucket,account_ref) DO UPDATE SET " + aggregateUpdates("usage_daily"),
		"INSERT INTO usage_totals(scope,subject," + metricsColumns + ",last_at) SELECT 'global',''," + metricsSums + ",max(bucket) FROM usage_legacy_baselines WHERE scope='minute'" + where + " GROUP BY 1,2 ON CONFLICT(scope,subject) DO UPDATE SET " + aggregateUpdates("usage_totals") + ",last_at=GREATEST(usage_totals.last_at,EXCLUDED.last_at)",
		"INSERT INTO usage_totals(scope,subject," + metricsColumns + ",last_at) SELECT 'account',subject," + metricsSums + ",max(bucket) FROM usage_legacy_baselines WHERE scope='account_day'" + where + " GROUP BY subject ON CONFLICT(scope,subject) DO UPDATE SET " + aggregateUpdates("usage_totals") + ",last_at=GREATEST(usage_totals.last_at,EXCLUDED.last_at)",
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement, args...); err != nil {
			return err
		}
	}
	return nil
}

// ExportLegacyStats includes preserved snapshot baselines; ExportRecords alone
// cannot represent historical usage for which no original request event exists.
func (m *Metrics) ExportLegacyStats(ctx context.Context, writer io.Writer) error {
	if m.db == nil {
		return errors.New("export requires PostgreSQL")
	}
	state := statsState{SavedAt: time.Now().Unix(), Minutes: map[int64]*minuteCounts{}, Accounts: map[string]map[int64]*dayCounts{}}
	cutoff := time.Now().Add(-metricsRetentionDays * 24 * time.Hour).Unix()
	rows, err := m.db.Pool.Query(ctx, "SELECT bucket,"+metricsSums+" FROM usage_minute WHERE bucket >= $1 GROUP BY bucket", cutoff)
	if err != nil {
		return err
	}
	for rows.Next() {
		var bucket int64
		var t rangeTotals
		if err = rows.Scan(append([]any{&bucket}, totalsTargets(&t)...)...); err != nil {
			rows.Close()
			return err
		}
		counts := minuteCounts(t)
		state.Minutes[bucket] = &counts
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = m.db.Pool.Query(ctx, "SELECT COALESCE(a.display_name,u.account_name) AS current_account_name,u.bucket,"+metricsSums+" FROM usage_daily u LEFT JOIN accounts a ON a.id=u.account_ref WHERE u.bucket >= $1 AND u.account_name<>'' GROUP BY current_account_name,u.bucket", cutoff)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var bucket int64
		var t rangeTotals
		if err = rows.Scan(append([]any{&name, &bucket}, totalsTargets(&t)...)...); err != nil {
			return err
		}
		if state.Accounts[name] == nil {
			state.Accounts[name] = map[int64]*dayCounts{}
		}
		counts := dayCounts(t)
		state.Accounts[name][bucket] = &counts
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return json.NewEncoder(writer).Encode(state)
}
