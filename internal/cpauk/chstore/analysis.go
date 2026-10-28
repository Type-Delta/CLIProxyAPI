package chstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

const analysisLatencySampleLimit = 1000

func (s *Store) Analysis(ctx context.Context, query model.Query) (model.Analysis, error) {
	if err := s.validateQuery(&query, model.OperationAnalysis); err != nil {
		return model.Analysis{}, err
	}
	result := model.Analysis{Meta: responseMeta(query)}
	sectionErrors := make([]error, 0, 5)
	var series model.AnalysisSeriesByCategory
	models, err := s.analysisAccounting(ctx, query, &series)
	if err != nil {
		// Preserve independent section errors for unsupported retained selections.
		series, err = s.analysisSeries(ctx, query)
		if err != nil {
			series.Meta.Partial = true
			sectionErrors = append(sectionErrors, err)
		}
		models, err = s.analysisModels(ctx, query)
	}
	result.SeriesByCategory = &series
	if err != nil {
		models.Meta.Partial = true
		sectionErrors = append(sectionErrors, err)
	}
	result.ModelByTime = &models
	latency, err := s.analysisLatency(ctx, query)
	if err != nil {
		latency.Meta.Partial = true
		sectionErrors = append(sectionErrors, err)
	}
	result.Latency = &latency
	costs, err := s.analysisCosts(ctx, query)
	if err != nil {
		costs.Meta.Partial = true
		sectionErrors = append(sectionErrors, err)
	}
	result.CostComponents = &costs
	matrix, err := s.analysisMatrix(ctx, query)
	if err != nil {
		matrix.Meta.Partial = true
		sectionErrors = append(sectionErrors, err)
	}
	result.KeyModelMatrix = &matrix
	if len(sectionErrors) == 5 {
		return model.Analysis{}, errors.Join(sectionErrors...)
	}
	return result, nil
}

func (s *Store) analysisSeries(ctx context.Context, query model.Query) (model.AnalysisSeriesByCategory, error) {
	width := analysisBucketWidth(query)
	buckets, err := s.activityBuckets(ctx, query, width)
	if err != nil {
		return model.AnalysisSeriesByCategory{}, err
	}
	return model.AnalysisSeriesByCategory{Buckets: buckets}, nil
}

func (s *Store) analysisLatency(ctx context.Context, query model.Query) (model.AnalysisLatency, error) {
	result := model.AnalysisLatency{Samples: []model.AnalysisLatencySample{}}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return result, ErrClosed
	}
	selection, err := s.rawSelection(ctx, query, "requested_at_ns", "attempt_id", "time_to_first_token_ms", "latency_ms", "model", "succeeded", "outcome")
	if err != nil {
		return result, err
	}
	arguments := selection.Args
	metrics, err := s.analysisTimingMetrics(ctx, query)
	if err != nil {
		return result, err
	}
	result.Metrics = metrics
	if partial, err := s.rawRangePartial(ctx, query); err != nil {
		return result, err
	} else if partial {
		return result, ErrRetainedRangePartial
	}
	result.SampleCount = metrics["e2e"].SampleCount
	result.P95TTFTMS, result.MaxTTFTMS = metrics["ttft"].P95MS, metrics["ttft"].MaxMS
	result.P95LatencyMS, result.MaxLatencyMS = metrics["e2e"].P95MS, metrics["e2e"].MaxMS
	rows, err := s.db.Conn().Query(ctx, `WITH ranked AS (
SELECT requested_at_ns,time_to_first_token_ms,latency_ms,model,succeeded,
ROW_NUMBER() OVER (ORDER BY requested_at_ns,attempt_id)-1 AS sample_index,
COUNT(*) OVER () AS sample_count
FROM (`+selection.SQL+`) WHERE (outcome IS NULL OR outcome <> 'canceled'))
SELECT requested_at_ns,time_to_first_token_ms,latency_ms,model,succeeded FROM ranked
WHERE sample_count <= ? OR intDiv((sample_index+1)*?,sample_count) > intDiv(sample_index*?,sample_count)
ORDER BY requested_at_ns`, append(arguments, analysisLatencySampleLimit, analysisLatencySampleLimit, analysisLatencySampleLimit)...)
	if err != nil {
		return result, fmt.Errorf("query analysis latency: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var requestedNS int64
		var ttft sql.NullInt64
		var sample model.AnalysisLatencySample
		if err := rows.Scan(&requestedNS, &ttft, &sample.LatencyMS, &sample.Model, &sample.Succeeded); err != nil {
			return model.AnalysisLatency{}, err
		}
		sample.RequestedAt = time.Unix(0, requestedNS).UTC()
		if ttft.Valid {
			value := ttft.Int64
			sample.TTFTMS = &value
		}
		result.Samples = append(result.Samples, sample)
	}
	result.Sampled = result.SampleCount > analysisLatencySampleLimit
	if err := rows.Err(); err != nil {
		return result, err
	}
	return result, nil
}

// analysisTimingMetrics computes all latency summaries in one windowed pass. The generic
// timingMetrics helper is also used by summary endpoints, where percentile statistics are not
// needed; analysis has to return five metrics and used to issue a separate count, percentile,
// and median scan for each one.
func (s *Store) analysisTimingMetrics(ctx context.Context, query model.Query) (map[string]model.TimingMetric, error) {
	return s.timingMetrics(ctx, query, true)
}

const analysisCostEventSelect = `provider,model,requested_alias,service_tier_requested,service_tier_used,
input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,total_tokens,
known_cost_nano,unpriced_tokens,price_rule_id,price_source,price_usd_per_credit_nano`

type analysisPriceKey struct {
	provider, model, alias   string
	requestedTier, usedTier  string
	hasAlias                 bool
	input, output, cacheRead int64
	cacheCreation, total     int64
}

type analysisPriceCacheValue struct {
	result aggregate.PriceResult
	err    error
}

func (s *Store) analysisCosts(ctx context.Context, query model.Query) (model.AnalysisCostComponents, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return model.AnalysisCostComponents{}, ErrClosed
	}
	selection, err := s.rawSelection(ctx, query, strings.Split(strings.ReplaceAll(analysisCostEventSelect, "\n", ""), ",")...)
	if err != nil {
		return model.AnalysisCostComponents{}, err
	}
	rows, err := s.db.Conn().Query(ctx, "SELECT "+analysisCostEventSelect+",toInt64(count()) FROM ("+selection.SQL+") GROUP BY "+analysisCostEventSelect, selection.Args...)
	if err != nil {
		return model.AnalysisCostComponents{}, fmt.Errorf("query analysis costs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	totals := make([]model.NanoUSD, 5)
	modelTotals := map[string][]model.NanoUSD{}
	priceCache := make(map[analysisPriceKey]analysisPriceCacheValue)
	priceCached := func(event model.Event) (aggregate.PriceResult, error) {
		key := analysisPriceKey{provider: event.Provider, model: event.Model, input: event.Tokens.Input,
			output: event.Tokens.Output, cacheRead: event.Tokens.CacheRead, cacheCreation: event.Tokens.CacheCreation,
			total: event.Tokens.Total}
		if event.RequestedAlias != nil {
			key.alias, key.hasAlias = *event.RequestedAlias, true
		}
		if event.ServiceTierRequested != nil {
			key.requestedTier = *event.ServiceTierRequested
		}
		if event.ServiceTierUsed != nil {
			key.usedTier = *event.ServiceTierUsed
		}
		if cached, ok := priceCache[key]; ok {
			return cached.result, cached.err
		}
		priced, priceErr := s.config.PriceBook.Price(event)
		priceCache[key] = analysisPriceCacheValue{result: priced, err: priceErr}
		return priced, priceErr
	}
	pricingIncomplete := false
	var pricedTokens int64
	for rows.Next() {
		var event model.Event
		var multiplicity int64
		var requestedAlias, requestedTier, usedTier, priceRuleID, priceSource sql.NullString
		var knownCost, creditRate sql.NullInt64
		if err := rows.Scan(&event.Provider, &event.Model, &requestedAlias, &requestedTier, &usedTier,
			&event.Tokens.Input, &event.Tokens.Output, &event.Tokens.CacheRead, &event.Tokens.CacheCreation, &event.Tokens.Total,
			&knownCost, &event.UnpricedTokens, &priceRuleID, &priceSource, &creditRate, &multiplicity); err != nil {
			return model.AnalysisCostComponents{}, fmt.Errorf("scan analysis costs: %w", err)
		}
		if requestedAlias.Valid {
			event.RequestedAlias = &requestedAlias.String
		}
		event.ServiceTierRequested, event.ServiceTierUsed = nullableStringR3(requestedTier), nullableStringR3(usedTier)
		if knownCost.Valid {
			value := model.NanoUSD(knownCost.Int64)
			event.KnownCost = &value
		}
		if priceRuleID.Valid {
			event.PriceRuleID = priceRuleID.String
		}
		if priceSource.Valid {
			event.PriceSource = priceSource.String
		}
		if event.PriceSource == "kiro-credits" && creditRate.Valid && event.KnownCost != nil {
			if modelTotals[event.Model] == nil {
				modelTotals[event.Model] = make([]model.NanoUSD, 5)
			}
			modelTotals[event.Model][4] += *event.KnownCost * model.NanoUSD(multiplicity)
			totals[4] += *event.KnownCost * model.NanoUSD(multiplicity)
			pricedTokens += multiplicity * max(int64(0), event.Tokens.Total-event.UnpricedTokens)
			continue
		}
		current, errPrice := priceCached(event)
		if errPrice != nil {
			return model.AnalysisCostComponents{}, errPrice
		}
		if event.KnownCost == nil || current.KnownCost == nil || *event.KnownCost != *current.KnownCost ||
			event.UnpricedTokens != current.UnpricedTokens || event.PriceRuleID != current.RuleID || event.PriceSource != current.Source {
			pricingIncomplete = true
			continue
		}
		if event.UnpricedTokens > 0 {
			pricingIncomplete = true
		}
		pricedTokens += multiplicity * max(int64(0), event.Tokens.Total-event.UnpricedTokens)
		uncached := max(int64(0), event.Tokens.Input-event.Tokens.CacheRead-event.Tokens.CacheCreation)
		parts := []model.TokenUsage{{Input: uncached, Total: uncached},
			{Input: event.Tokens.CacheRead, CacheRead: event.Tokens.CacheRead, Total: event.Tokens.CacheRead},
			{Input: event.Tokens.CacheCreation, CacheCreation: event.Tokens.CacheCreation, Total: event.Tokens.CacheCreation},
			{Output: event.Tokens.Output, Total: event.Tokens.Output}}
		componentCosts := make([]model.NanoUSD, len(parts))
		for index, tokens := range parts {
			component := event
			component.Tokens = tokens
			priced, errPrice := priceCached(component)
			if errPrice != nil {
				return model.AnalysisCostComponents{}, errPrice
			}
			if priced.KnownCost != nil {
				componentCosts[index] = *priced.KnownCost
			} else if tokens.Total > 0 {
				pricingIncomplete = true
			}
		}
		reconcileCostComponents(componentCosts, parts, *event.KnownCost)
		if modelTotals[event.Model] == nil {
			modelTotals[event.Model] = make([]model.NanoUSD, 5)
		}
		for index, cost := range componentCosts {
			modelTotals[event.Model][index] += cost * model.NanoUSD(multiplicity)
		}
		for index := range componentCosts {
			totals[index] += componentCosts[index] * model.NanoUSD(multiplicity)
		}
	}
	if err := rows.Err(); err != nil {
		return model.AnalysisCostComponents{}, err
	}
	if err := rows.Close(); err != nil {
		return model.AnalysisCostComponents{}, err
	}
	result := model.AnalysisCostComponents{Models: []model.ModelCostComponents{}}
	for name, costs := range modelTotals {
		result.Models = append(result.Models, model.ModelCostComponents{Model: name, CreditBasedUSD: costs[4].String(), UncachedInputUSD: costs[0].String(), CacheReadUSD: costs[1].String(), CacheCreationUSD: costs[2].String(), OutputUSD: costs[3].String(), TotalUSD: (costs[0] + costs[1] + costs[2] + costs[3] + costs[4]).String()})
	}
	slices.SortFunc(result.Models, func(a, b model.ModelCostComponents) int { return strings.Compare(a.Model, b.Model) })
	result.CreditBasedUSD = totals[4].String()
	result.UncachedInputUSD = totals[0].String()
	result.CacheReadUSD = totals[1].String()
	result.CacheCreationUSD = totals[2].String()
	result.OutputUSD = totals[3].String()
	var totalCost model.NanoUSD
	for _, cost := range totals {
		totalCost += cost
	}
	result.BlendedUSDPerMillion = decimalProductsRatio(int64(totalCost), 1, pricedTokens, 1000)
	if partial, err := s.rawRangePartial(ctx, query); err != nil {
		return result, err
	} else if partial {
		return result, ErrRetainedRangePartial
	}
	if pricingIncomplete {
		return result, errAnalysisPricingIncomplete
	}
	return result, nil
}

var errAnalysisPricingIncomplete = errors.New("analysis pricing coverage is incomplete")

// reconcileCostComponents preserves the stored once-per-event total after the
// individual display components have each been rounded. Ties use category
// order: uncached input, cache read, cache creation, then output.
func reconcileCostComponents(components []model.NanoUSD, tokens []model.TokenUsage, eventTotal model.NanoUSD) {
	var componentTotal model.NanoUSD
	for _, component := range components {
		componentTotal += component
	}
	residual := eventTotal - componentTotal
	if residual >= 0 {
		for index := range components {
			if tokens[index].Total > 0 {
				components[index] += residual
				return
			}
		}
		return
	}
	remaining := -residual
	for index := range components {
		adjustment := min(components[index], remaining)
		components[index] -= adjustment
		remaining -= adjustment
		if remaining == 0 {
			return
		}
	}
}

func (s *Store) analysisMatrix(ctx context.Context, query model.Query) (model.AnalysisKeyModelMatrix, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return model.AnalysisKeyModelMatrix{}, ErrClosed
	}
	selection, err := s.rawSelection(ctx, query, "key_id", "model", "proxy_request_id", "input_tokens", "output_tokens", "cached_tokens", "cache_read_tokens", "cache_creation_tokens", "reasoning_tokens", "total_tokens", "known_cost_nano", "unpriced_tokens", "generation_time_ms")
	if err != nil {
		return model.AnalysisKeyModelMatrix{}, err
	}
	rows, err := s.db.Conn().Query(ctx, `SELECT key_id,model,toInt64(uniqExact(proxy_request_id)),
 sum(input_tokens),sum(output_tokens),sum(cached_tokens),sum(cache_read_tokens),sum(cache_creation_tokens),sum(reasoning_tokens),sum(total_tokens),accurateCast(sum(toInt128(coalesce(known_cost_nano,0))),'Int64'),sum(unpriced_tokens),sumOrNull(generation_time_ms),toInt64(count(generation_time_ms))
 FROM (`+selection.SQL+`) GROUP BY key_id,model`, selection.Args...)
	if err != nil {
		return model.AnalysisKeyModelMatrix{}, fmt.Errorf("query analysis matrix: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := model.AnalysisKeyModelMatrix{Keys: []string{}, Models: []string{}, Cells: []model.AnalysisMatrixCell{}}
	keys, models := map[string]struct{}{}, map[string]struct{}{}
	for rows.Next() {
		var cell model.AnalysisMatrixCell
		var cost int64
		var generation sql.NullInt64
		if err := rows.Scan(&cell.KeyID, &cell.Model, &cell.Requests, &cell.InputTokens, &cell.OutputTokens,
			&cell.CachedTokens, &cell.CacheReadTokens, &cell.CacheCreationTokens, &cell.ReasoningTokens,
			&cell.TotalTokens, &cost, &cell.UnpricedTokens, &generation, &cell.GenerationSampleCount); err != nil {
			return model.AnalysisKeyModelMatrix{}, err
		}
		cell.KnownCost = model.NanoUSD(cost)
		if generation.Valid {
			cell.GenerationTimeMS = &generation.Int64
		}
		result.Cells = append(result.Cells, cell)
		keys[cell.KeyID] = struct{}{}
		models[cell.Model] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return model.AnalysisKeyModelMatrix{}, err
	}
	for key := range keys {
		result.Keys = append(result.Keys, key)
	}
	for modelName := range models {
		result.Models = append(result.Models, modelName)
	}
	slices.Sort(result.Keys)
	slices.Sort(result.Models)
	slices.SortFunc(result.Cells, func(left, right model.AnalysisMatrixCell) int {
		if left.KeyID < right.KeyID {
			return -1
		}
		if left.KeyID > right.KeyID {
			return 1
		}
		if left.Model < right.Model {
			return -1
		}
		if left.Model > right.Model {
			return 1
		}
		return 0
	})
	if partial, err := s.rawRangePartial(ctx, query); err != nil {
		return result, err
	} else if partial {
		return result, ErrRetainedRangePartial
	}
	return result, nil
}

func analysisBucketWidth(query model.Query) string {
	if query.BucketWidth != "" {
		return query.BucketWidth
	}
	duration := query.End.Sub(query.Start)
	switch {
	case duration <= 24*time.Hour:
		return "5m"
	case duration <= 31*24*time.Hour:
		return "1h"
	default:
		return "1d"
	}
}
