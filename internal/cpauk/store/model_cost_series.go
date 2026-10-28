package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func (s *SQLiteStore) ModelCostSeries(ctx context.Context, query model.Query) (model.ModelCostSeries, error) {
	if err := s.validateQuery(&query, model.OperationModelCostSeries); err != nil {
		return model.ModelCostSeries{}, err
	}
	sequence, err := analyticsBucketSequence(query, analysisBucketWidth(query))
	if err != nil {
		return model.ModelCostSeries{}, err
	}
	bucketer, err := aggregate.NewBucketer(query.TimeZone, analysisBucketWidth(query))
	if err != nil {
		return model.ModelCostSeries{}, err
	}
	where, args, err := buildWhere(query)
	if err != nil {
		return model.ModelCostSeries{}, err
	}
	var names []string
	_ = json.Unmarshal(query.Filters["model"], &names)
	result := model.ModelCostSeries{Meta: responseMeta(query), Model: names[0], Buckets: make([]model.ModelCostSeriesBucket, 0, len(sequence))}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return result, ErrClosed
	}
	result.Partial = !s.retentionCutoff.IsZero() && query.Start.Before(s.retentionCutoff)
	rows, err := s.db.QueryContext(ctx, "SELECT requested_at_ns,"+analysisCostEventSelect+",pricing_breakdown FROM events "+where, args...)
	if err != nil {
		return result, fmt.Errorf("query model cost series: %w", err)
	}
	defer func() { _ = rows.Close() }()
	buckets := map[int64]map[string]*model.ModelCostSeriesTier{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		var ns int64
		var event model.Event
		var alias, requested, used, rule, source, breakdown sql.NullString
		var known, credit sql.NullInt64
		if err := rows.Scan(&ns, &event.Provider, &event.Model, &alias, &requested, &used,
			&event.Tokens.Input, &event.Tokens.Output, &event.Tokens.CacheRead, &event.Tokens.CacheCreation, &event.Tokens.Total,
			&known, &event.UnpricedTokens, &rule, &source, &credit, &breakdown); err != nil {
			return result, fmt.Errorf("scan model cost series: %w", err)
		}
		// Attempts with no tokens and no cost do not contribute a chart layer.
		if event.Tokens.Total == 0 && (!known.Valid || known.Int64 == 0) {
			continue
		}
		event.RequestedAlias = scanNullableString(alias)
		event.ServiceTierRequested, event.ServiceTierUsed = scanNullableString(requested), scanNullableString(used)
		tier := "default"
		if requested.Valid && strings.TrimSpace(requested.String) != "" {
			tier = strings.ToLower(strings.TrimSpace(requested.String))
		}
		if used.Valid && strings.TrimSpace(used.String) != "" {
			tier = strings.ToLower(strings.TrimSpace(used.String))
		}
		point := model.ModelCostSeriesTier{Provider: event.Provider, ServiceTier: tier,
			InputTokens:     max(int64(0), event.Tokens.Input-event.Tokens.CacheRead-event.Tokens.CacheCreation),
			CacheReadTokens: event.Tokens.CacheRead, CacheCreationTokens: event.Tokens.CacheCreation,
			OutputTokens: event.Tokens.Output, TotalTokens: event.Tokens.Total,
			Rates: make([]*string, 4), Costs: make([]*model.NanoUSD, 4)}
		if known.Valid {
			point.KnownCost = model.NanoUSD(known.Int64)
		} else {
			result.Partial = true
		}
		if source.String == "kiro-credits" && known.Valid {
			point.CreditBasedCost = point.KnownCost
		} else {
			var details *model.PricingBreakdown
			if breakdown.Valid {
				var saved model.PricingBreakdown
				if err := json.Unmarshal([]byte(breakdown.String), &saved); err != nil {
					return result, fmt.Errorf("decode stored model pricing: %w", err)
				}
				details = &saved
			} else {
				// One price calculation already includes all component costs and effective rates.
				current, err := s.price(event)
				if err != nil {
					return result, err
				}
				if known.Valid && current.KnownCost != nil && *current.KnownCost == point.KnownCost && current.RuleID == rule.String && current.Source == source.String && current.UnpricedTokens == event.UnpricedTokens {
					details = current.Breakdown
				}
			}
			if details != nil {
				components := []model.PricingComponent{details.Input, details.CacheRead, details.CacheWrite, details.Output}
				var sum model.NanoUSD
				for i := range components {
					point.Rates[i] = &components[i].RatePerMillion
					point.Costs[i] = &components[i].Cost
					sum += components[i].Cost
				}
				point.InputMultiplier, point.OutputMultiplier = details.InputTierMultiplier, details.OutputTierMultiplier
				if details.ServiceTier != "" {
					point.ServiceTier = details.ServiceTier
				}
				if !known.Valid || sum != point.KnownCost {
					result.Partial = true
					point.Rates = make([]*string, 4)
					point.Costs = make([]*model.NanoUSD, 4)
				}
			} else {
				result.Partial = true
			}
		}
		if event.UnpricedTokens > 0 {
			result.Partial = true
		}
		start, _, err := bucketer.Bounds(time.Unix(0, ns).UTC())
		if err != nil {
			return result, err
		}
		keyBytes, _ := json.Marshal([]any{point.Provider, point.ServiceTier, point.Rates, point.InputMultiplier, point.OutputMultiplier})
		key := string(keyBytes)
		group := buckets[start.UnixNano()]
		if group == nil {
			group = map[string]*model.ModelCostSeriesTier{}
			buckets[start.UnixNano()] = group
		}
		if previous := group[key]; previous != nil {
			previous.InputTokens += point.InputTokens
			previous.CacheReadTokens += point.CacheReadTokens
			previous.CacheCreationTokens += point.CacheCreationTokens
			previous.OutputTokens += point.OutputTokens
			previous.TotalTokens += point.TotalTokens
			previous.KnownCost += point.KnownCost
			previous.CreditBasedCost += point.CreditBasedCost
			for i, cost := range point.Costs {
				if cost != nil {
					*previous.Costs[i] += *cost
				}
			}
		} else {
			group[key] = &point
		}
	}
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("read model cost series: %w", err)
	}
	for _, bounds := range sequence {
		bucket := model.ModelCostSeriesBucket{Start: bounds.start, Tiers: []model.ModelCostSeriesTier{}}
		keys := make([]string, 0, len(buckets[bounds.start.UnixNano()]))
		for key := range buckets[bounds.start.UnixNano()] {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			bucket.Tiers = append(bucket.Tiers, *buckets[bounds.start.UnixNano()][key])
		}
		result.Buckets = append(result.Buckets, bucket)
	}
	return result, nil
}
