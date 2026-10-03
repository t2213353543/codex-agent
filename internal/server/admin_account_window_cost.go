package server

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// Reference-only USD estimates, isolated from billing and gateway quotas.
// Like Sub2API's WindowStats, these sum observed token usage in each window;
// seven-day total estimates use local cost * 100 / official utilization.
// This is an account-statistics estimate, not provider billing or credit balance.
//
//go:embed account_window_prices.json
var managedAccountPriceJSON []byte

var managedAccountPriceVersion = fmt.Sprintf("%x", sha256.Sum256(managedAccountPriceJSON))[:12]
var managedAccountDatedModel = regexp.MustCompile(`^(.+)-\d{4}-\d{2}-\d{2}$`)

var managedAccountPrices = func() struct {
	Source       string                        `json:"source"`
	CapturedAt   string                        `json:"captured_at"`
	SourceSHA256 string                        `json:"source_sha256"`
	Models       map[string]map[string]float64 `json:"models"`
} {
	var prices struct {
		Source       string                        `json:"source"`
		CapturedAt   string                        `json:"captured_at"`
		SourceSHA256 string                        `json:"source_sha256"`
		Models       map[string]map[string]float64 `json:"models"`
	}
	if err := json.Unmarshal(managedAccountPriceJSON, &prices); err != nil {
		panic(err)
	}
	return prices
}()

type managedAccountWindowCost struct {
	From                  int64    `json:"from"`
	To                    int64    `json:"to"`
	CheckedAt             int64    `json:"checked_at"`
	USD                   *float64 `json:"usd"`
	Requests              int64    `json:"requests"`
	EstimatedRequests     int64    `json:"estimated_requests"`
	UnknownRequests       int64    `json:"unknown_requests"`
	ReconstructedRequests int64    `json:"reconstructed_requests"`
	Scope                 string   `json:"scope"`
	Source                string   `json:"source"`
	PriceVersion          string   `json:"price_version"`
	Error                 string   `json:"error,omitempty"`
}

// OpenAI input_tokens include cached_tokens: subtract cached input exactly once.
// Preserve unknown prices/usage instead of displaying unpriced work as $0.
func managedAccountRequestUSD(record RequestRecord) (float64, bool) {
	// Sub2API stores request-time cost. New records keep the same immutable
	// semantics; old records without a price snapshot are explicitly reconstructed.
	if record.WindowCostUSD != nil && record.WindowCostPriceVersion != "" {
		value := *record.WindowCostUSD
		return value, !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
	}
	known := managedUsageKnown(record)
	if known == nil || !*known || record.InputTokens < 0 || record.OutputTokens < 0 || record.CachedTokens < 0 || record.CachedTokens > record.InputTokens {
		return 0, false
	}
	// AccountStats pricing uses the upstream model sent by the gateway. The
	// provider's response declaration cannot silently reprice this account window.
	model := record.SentModel
	if model == "" {
		model = record.UpstreamModel
	}
	if model == "" {
		model = record.Model
	}
	if model == "" {
		model = record.ReturnedModel
	}
	model = strings.ToLower(strings.TrimSpace(model))
	if at := strings.LastIndex(model, "/"); at >= 0 {
		model = model[at+1:]
	}
	model = strings.TrimSuffix(model, "-openai-compact")
	if model == "gpt-6" {
		model = "gpt-6-astra"
	}
	if model == "gpt-5.6" {
		model = "gpt-5.6-sol"
	}
	prices, ok := managedAccountPrices.Models[model]
	if !ok {
		if date := managedAccountDatedModel.FindStringSubmatch(model); len(date) == 2 {
			model = date[1]
			prices, ok = managedAccountPrices.Models[model]
		}
	}
	if !ok {
		return 0, false
	}
	input, inputOK := prices["input_cost_per_token"]
	output, outputOK := prices["output_cost_per_token"]
	cached := prices["cache_read_input_token_cost"]
	write := prices["cache_creation_input_token_cost"]
	if !inputOK || !outputOK {
		return 0, false
	}
	if strings.HasPrefix(model, "gpt-5.6-") && write <= 0 {
		write = input * 1.25
	}
	tier := strings.ToLower(strings.TrimSpace(record.ServiceTier))
	multiplier := 1.0
	switch tier {
	case "priority", "fast":
		// billing_service.go: explicit priority prices win without applying twice;
		// known GPT models force the same business ratio as Sub2API.
		ratio := 0.0
		switch model {
		case "gpt-5.4", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-astra":
			ratio = 2
		case "gpt-5.5":
			ratio = 2.5
		}
		if ratio > 0 {
			// Sub2API applies the model override to token priority prices. A
			// separately priced image component retains its own price; applying
			// a multiplier to the whole total would incorrectly multiply it too.
			input *= ratio
			output *= ratio
			cached *= ratio
			write *= ratio
		} else {
			keys := []string{"input_cost_per_token_priority", "output_cost_per_token_priority", "cache_read_input_token_cost_priority", "cache_creation_input_token_cost_priority"}
			targets := []*float64{&input, &output, &cached, &write}
			explicit := false
			for i, key := range keys {
				if prices[key] > 0 {
					*targets[i] = prices[key]
					explicit = true
				}
			}
			if !explicit {
				multiplier = 2
			}
		}
	case "ultrafast":
		multiplier = 2
	case "flex":
		multiplier = .5
	}
	threshold := prices["long_context_input_token_threshold"]
	if threshold <= 0 {
		if _, ok := prices["input_cost_per_token_above_272k_tokens"]; ok {
			threshold = 272000
		}
	}
	if threshold > 0 && float64(record.InputTokens) > threshold {
		inMult, outMult := prices["long_context_input_cost_multiplier"], prices["long_context_output_cost_multiplier"]
		if inMult <= 0 && prices["input_cost_per_token"] > 0 {
			inMult = prices["input_cost_per_token_above_272k_tokens"] / prices["input_cost_per_token"]
		}
		if outMult <= 0 && prices["output_cost_per_token"] > 0 {
			outMult = prices["output_cost_per_token_above_272k_tokens"] / prices["output_cost_per_token"]
		}
		// Sub2API defaults an unconfigured side to 1, never zero or unknown.
		if inMult <= 0 {
			inMult = 1
		}
		if outMult <= 0 {
			outMult = 1
		}
		input *= inMult
		cached *= inMult
		write *= inMult
		output *= outMult
	}
	cacheWrite, imageInput, imageOutput := int64(0), int64(0), int64(0)
	for _, part := range []struct {
		source *int64
		target *int64
	}{{record.CacheWriteTokens, &cacheWrite}, {record.ImageInputTokens, &imageInput}, {record.ImageOutputTokens, &imageOutput}} {
		if part.source != nil {
			if *part.source < 0 {
				return 0, false
			}
			*part.target = *part.source
		}
	}
	if cacheWrite > record.InputTokens-record.CachedTokens {
		return 0, false
	}
	uncached := record.InputTokens - record.CachedTokens - cacheWrite
	imageInput = min(imageInput, uncached)
	imageOutput = min(imageOutput, record.OutputTokens)
	imageInPrice, imageOutPrice := prices["input_cost_per_image_token"], prices["output_cost_per_image_token"]
	if imageInPrice <= 0 {
		imageInPrice = input
	}
	if imageOutPrice <= 0 {
		imageOutPrice = output
	}
	value := (float64(uncached-imageInput)*input + float64(imageInput)*imageInPrice + float64(record.CachedTokens)*cached + float64(cacheWrite)*write + float64(record.OutputTokens-imageOutput)*output + float64(imageOutput)*imageOutPrice) * multiplier
	return value, !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func managedAccountCostPriceVersion() string {
	return "sub2api-account-v2:" + managedAccountPrices.CapturedAt + ":" + managedAccountPriceVersion
}

func freezeManagedAccountRequestUSD(record *RequestRecord) {
	if record.WindowCostPriceVersion != "" {
		return
	}
	if value, known := managedAccountRequestUSD(*record); known {
		record.WindowCostUSD = &value
		record.WindowCostPriceVersion = managedAccountCostPriceVersion()
	}
}

// A batch makes one bounded scan for all accounts/windows, including when the
// official snapshot comes from cache. Local usage is always refreshed separately.
func (p *Proxy) enrichManagedWindowCosts(ctx context.Context, snapshots []*managedUpstreamSnapshot) {
	if p.Metrics == nil {
		return
	}
	now := time.Now().Unix()
	byAccount := map[string][]*managedAccountWindowCost{}
	query := managedLogQuery{To: now + 1, SortBy: "time", SortOrder: "asc"}
	for _, snapshot := range snapshots {
		if snapshot == nil || !snapshot.OK || snapshot.Kind != "usage" {
			continue
		}
		snapshot.LocalWindowCosts = nil
		raw, err := json.Marshal(snapshot.Data)
		if err != nil {
			continue
		}
		parsed, err := parseManagedUsage(raw)
		if err != nil {
			continue
		}
		rate := parsed.(managedUsage).RateLimit
		if rate == nil {
			continue
		}
		for key, window := range map[string]*managedRateWindow{"primary_window": rate.PrimaryWindow, "secondary_window": rate.SecondaryWindow} {
			if window == nil || window.LimitWindowSeconds == nil || *window.LimitWindowSeconds <= 0 || *window.LimitWindowSeconds > 366*86400 {
				continue
			}
			reset := int64(0)
			if window.ResetAt != nil {
				reset = *window.ResetAt
			} else if window.ResetAfterSeconds != nil && *window.ResetAfterSeconds >= 0 && snapshot.CheckedAt > 0 {
				reset = snapshot.CheckedAt + *window.ResetAfterSeconds
			}
			start := reset - *window.LimitWindowSeconds
			if reset <= now {
				start = now - *window.LimitWindowSeconds
				reset = now + 1
			}
			if start <= 0 || start > now {
				continue
			}
			cost := &managedAccountWindowCost{From: start, To: min(reset, now+1), CheckedAt: now, Scope: "retained_request_details", Source: "local_token_estimate", PriceVersion: managedAccountCostPriceVersion()}
			if snapshot.LocalWindowCosts == nil {
				snapshot.LocalWindowCosts = map[string]*managedAccountWindowCost{}
			}
			snapshot.LocalWindowCosts[key] = cost
			byAccount[snapshot.ID] = append(byAccount[snapshot.ID], cost)
			if query.From == 0 || start < query.From {
				query.From = start
			}
		}
	}
	if len(byAccount) == 0 {
		return
	}
	for id := range byAccount {
		query.AccountIDs = append(query.AccountIDs, id)
	}
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := p.Metrics.walkManagedLogs(readCtx, query, func(record RequestRecord) error {
		value, known := managedAccountRequestUSD(record)
		for _, cost := range byAccount[record.AccountID] {
			if record.Time < cost.From || record.Time >= cost.To {
				continue
			}
			cost.Requests++
			if !known {
				cost.UnknownRequests++
				continue
			}
			cost.EstimatedRequests++
			if record.WindowCostPriceVersion == "" {
				cost.ReconstructedRequests++
			}
			if cost.USD == nil {
				cost.USD = new(float64)
			}
			*cost.USD += value
		}
		return nil
	})
	for _, costs := range byAccount {
		for _, cost := range costs {
			if err != nil {
				cost.USD = nil
				cost.Error = "local_usage_unavailable"
				continue
			}
			if cost.Requests == 0 {
				cost.USD = new(float64)
			}
			if cost.USD != nil {
				*cost.USD = math.Round(*cost.USD*1e8) / 1e8
			}
		}
	}
}
