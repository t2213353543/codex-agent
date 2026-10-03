package server

import (
	"context"
	"sort"
)

type overviewDimension struct {
	Name       string `json:"name"`
	Requests   int64  `json:"requests"`
	Errors     int64  `json:"errors"`
	Tokens     int64  `json:"tokens"`
	UsageKnown int64  `json:"usage_known_requests"`
}

type overviewBreakdown struct {
	Scope           string              `json:"scope"`
	Requests        int64               `json:"requests"`
	Errors          int64               `json:"errors"`
	Models          []overviewDimension `json:"models"`
	ErrorCategories []overviewDimension `json:"error_categories"`
}

// OverviewBreakdown reads only the two dimensions needed by the dashboard.
// Its denominator is retained request details, not the longer-lived aggregates.
// The previous comparison range never requests this optional breakdown.
func (m *Metrics) OverviewBreakdown(ctx context.Context, from, to int64) (overviewBreakdown, error) {
	out := overviewBreakdown{Scope: "retained_request_details", Models: []overviewDimension{}, ErrorCategories: []overviewDimension{}}
	models, categories := map[string]*overviewDimension{}, map[string]*overviewDimension{}
	add := func(model, category string, requests, errors, tokens, usageKnown int64) {
		out.Requests += requests
		out.Errors += errors
		if models[model] == nil {
			models[model] = &overviewDimension{Name: model}
		}
		v := models[model]
		v.Requests += requests
		v.Errors += errors
		v.Tokens += tokens
		v.UsageKnown += usageKnown
		if errors > 0 {
			if categories[category] == nil {
				categories[category] = &overviewDimension{Name: category}
			}
			categories[category].Requests += errors
			categories[category].Errors += errors
		}
	}
	if m.db != nil {
		// The retained minute rollup answers both dashboard dimensions in one
		// bounded index scan; minutes without complete summaries fall back to
		// details inside the same statement. Step only shapes the unused series
		// grouping, so a daily bucket keeps that work minimal.
		stats, err := m.managedLogStatsPG(ctx, managedLogQuery{From: from, To: to, Timezone: "UTC"}, managedUsageStats{Step: 86400})
		if err != nil {
			return out, err
		}
		out.Requests = stats.Requests
		out.Errors = stats.Errors
		for _, value := range stats.Models {
			models[value.Name] = &overviewDimension{Name: value.Name, Requests: value.Requests, Errors: value.Errors, Tokens: value.Tokens, UsageKnown: value.UsageKnownRequests}
		}
		// Category rows only exist for failed requests in both paths.
		for _, value := range stats.ErrorCategories {
			categories[value.Name] = &overviewDimension{Name: value.Name, Requests: value.Requests, Errors: value.Errors}
		}
	} else {
		err := m.walkManagedLogs(ctx, managedLogQuery{From: from, To: to}, func(r RequestRecord) error {
			var errors int64
			if r.Status != "ok" {
				errors = 1
			}
			var known int64
			if value := managedUsageKnown(r); value != nil && *value {
				known = 1
			}
			add(r.Model, managedFailureCategory(r), 1, errors, r.tokens(), known)
			return nil
		})
		if err != nil {
			return out, err
		}
	}
	ordered := func(values map[string]*overviewDimension) []overviewDimension {
		result := make([]overviewDimension, 0, len(values))
		for _, value := range values {
			result = append(result, *value)
		}
		sort.Slice(result, func(i, j int) bool {
			if result[i].Requests != result[j].Requests {
				return result[i].Requests > result[j].Requests
			}
			return result[i].Name < result[j].Name
		})
		return result
	}
	out.Models = ordered(models)
	out.ErrorCategories = ordered(categories)
	return out, nil
}

func (p *Proxy) overviewInFlight() int64 {
	p.admissionMu.Lock()
	defer p.admissionMu.Unlock()
	var total int64
	for _, count := range p.admission {
		if count != nil {
			total += *count
		}
	}
	return total
}
