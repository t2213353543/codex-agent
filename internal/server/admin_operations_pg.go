package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// Retained second/minute summaries answer every operation slot without
// decoding all request JSON. The newest eight errors use a bounded index probe
// in the same statement snapshot as the summary, including during cleanup.
func (m *Metrics) operationsTrafficPG(ctx context.Context, out operationsSnapshot) (operationsSnapshot, error) {
	statement := operationsRollupSQL()
	rows, err := m.db.Pool.Query(ctx, statement, out.From, out.To, out.Step)
	if err != nil {
		return operationsSnapshot{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, category string
		var point int64
		var totals operationsTotals
		var histogram []int64
		var recent []byte
		if err := rows.Scan(&kind, &point, &category, &totals.Requests, &totals.Success, &totals.Errors, &totals.Aborted,
			&totals.Tokens, &totals.UsageKnown, &totals.LatencySamples, &totals.AverageLatency, &totals.TTFTSamples,
			&totals.AverageTTFT, &histogram, &recent); err != nil {
			return operationsSnapshot{}, err
		}
		switch kind {
		case "total":
			out.Traffic = totals
			for i, count := range histogram {
				out.Latency[i].Requests = count
			}
		case "series":
			if point < 0 || point >= int64(len(out.Series)) {
				return operationsSnapshot{}, fmt.Errorf("invalid operations time bucket")
			}
			out.Series[point].operationsTotals = totals
		case "category":
			out.ErrorCategories = append(out.ErrorCategories, operationsCategory{Name: category, Requests: totals.Requests})
		case "recent":
			if err := json.Unmarshal(recent, &out.RecentErrors); err != nil {
				return operationsSnapshot{}, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return operationsSnapshot{}, err
	}
	sort.Slice(out.ErrorCategories, func(i, j int) bool {
		a, b := out.ErrorCategories[i], out.ErrorCategories[j]
		if a.Requests == b.Requests {
			return a.Name < b.Name
		}
		return a.Requests > b.Requests
	})
	return out, nil
}
