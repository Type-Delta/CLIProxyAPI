package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

const analysisLatencySampleLimit = 1000

func (s *SQLiteStore) Analysis(ctx context.Context, query model.Query) (model.Analysis, error) {
	if err := s.validateQuery(&query, model.OperationAnalysis); err != nil {
		return model.Analysis{}, err
	}
	result := model.Analysis{Meta: responseMeta(query)}
	sectionErrors := make([]error, 0, 5)
	series, err := s.analysisSeries(ctx, query)
	if err != nil {
		series.Meta.Partial = true
		sectionErrors = append(sectionErrors, err)
	}
	result.SeriesByCategory = &series
	models, err := s.analysisModels(ctx, query)
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

func (s *SQLiteStore) analysisSeries(ctx context.Context, query model.Query) (model.AnalysisSeriesByCategory, error) {
	width := analysisBucketWidth(query)
	buckets, err := s.activityBuckets(ctx, query, width)
	if err != nil {
		return model.AnalysisSeriesByCategory{}, err
	}
	return model.AnalysisSeriesByCategory{Buckets: buckets}, nil
}

func (s *SQLiteStore) analysisModels(ctx context.Context, query model.Query) (model.AnalysisModelByTime, error) {
	dimensionQuery := query
	dimensionQuery.Operation = model.OperationDimensions
	dimensionQuery.Window = ""
	dimensionQuery.BucketWidth = ""
	dimensionQuery.Dimension = "model"
	dimensionQuery.PageSize = 10
	dimensions, err := s.Dimensions(ctx, dimensionQuery)
	if err != nil {
		return model.AnalysisModelByTime{}, fmt.Errorf("query analysis models: %w", err)
	}
	result := model.AnalysisModelByTime{Models: make([]model.AnalysisModel, 0, len(dimensions.Rows))}
	for _, row := range dimensions.Rows {
		item := model.AnalysisModel{
			Model: row.Value, Requests: row.ProxyRequests, InputTokens: row.Tokens.Input,
			OutputTokens: row.Tokens.Output, CachedTokens: row.Tokens.Cached,
			CacheReadTokens: row.Tokens.CacheRead, CacheCreationTokens: row.Tokens.CacheCreation,
			ReasoningTokens: row.Tokens.Reasoning, TotalTokens: row.Tokens.Total, KnownCost: row.KnownCost,
		}
		result.Models = append(result.Models, item)
	}
	width := analysisBucketWidth(query)
	sequence, err := analyticsBucketSequence(query, width)
	if err != nil {
		return model.AnalysisModelByTime{}, err
	}
	buckets := make(map[int64]*model.AnalysisModelBucket, len(sequence))
	for _, bounds := range sequence {
		buckets[bounds.start.UnixNano()] = &model.AnalysisModelBucket{Start: bounds.start, Models: []model.AnalysisModel{}}
	}
	for _, item := range result.Models {
		timeseriesQuery := query
		timeseriesQuery.Operation = model.OperationTimeseries
		timeseriesQuery.Window = ""
		timeseriesQuery.BucketWidth = width
		timeseriesQuery.Filters = make(map[string]json.RawMessage, len(query.Filters)+1)
		for name, value := range query.Filters {
			timeseriesQuery.Filters[name] = value
		}
		encodedModel, _ := json.Marshal([]string{item.Model})
		timeseriesQuery.Filters["model"] = encodedModel
		timeseries, errSeries := s.Timeseries(ctx, timeseriesQuery)
		if errSeries != nil {
			return model.AnalysisModelByTime{}, fmt.Errorf("query analysis model buckets: %w", errSeries)
		}
		for _, point := range timeseries.Points {
			bucket := buckets[point.Start.UnixNano()]
			if bucket == nil {
				bucket = &model.AnalysisModelBucket{Start: point.Start, Models: []model.AnalysisModel{}}
				buckets[point.Start.UnixNano()] = bucket
			}
			modelPoint := model.AnalysisModel{
				Model: item.Model, Requests: point.ProxyRequests, InputTokens: point.Tokens.Input,
				OutputTokens: point.Tokens.Output, CachedTokens: point.Tokens.Cached,
				CacheReadTokens: point.Tokens.CacheRead, CacheCreationTokens: point.Tokens.CacheCreation,
				ReasoningTokens: point.Tokens.Reasoning, TotalTokens: point.Tokens.Total, KnownCost: point.KnownCost,
			}
			bucket.Models = append(bucket.Models, modelPoint)
		}
	}
	starts := make([]int64, 0, len(buckets))
	for start := range buckets {
		starts = append(starts, start)
	}
	slices.Sort(starts)
	result.Buckets = make([]model.AnalysisModelBucket, 0, len(starts))
	for _, start := range starts {
		result.Buckets = append(result.Buckets, *buckets[start])
	}
	return result, nil
}

func (s *SQLiteStore) analysisLatency(ctx context.Context, query model.Query) (model.AnalysisLatency, error) {
	result := model.AnalysisLatency{Samples: []model.AnalysisLatencySample{}}
	where, arguments, err := buildWhere(query)
	if err != nil {
		return result, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return result, ErrClosed
	}
	metrics, err := s.timingMetrics(ctx, query, true)
	if err != nil {
		return result, err
	}
	result.Metrics = metrics
	if !s.retentionCutoff.IsZero() && query.Start.Before(s.retentionCutoff) {
		return result, ErrRetainedRangePartial
	}
	result.SampleCount = metrics["e2e"].SampleCount
	result.P95TTFTMS, result.MaxTTFTMS = metrics["ttft"].P95MS, metrics["ttft"].MaxMS
	result.P95LatencyMS, result.MaxLatencyMS = metrics["e2e"].P95MS, metrics["e2e"].MaxMS
	rows, err := s.db.QueryContext(ctx, `WITH ranked AS (
SELECT requested_at_ns,time_to_first_token_ms,latency_ms,model,succeeded,
ROW_NUMBER() OVER (ORDER BY requested_at_ns,attempt_id)-1 AS sample_index,
COUNT(*) OVER () AS sample_count
FROM events `+where+`)
SELECT requested_at_ns,time_to_first_token_ms,latency_ms,model,succeeded FROM ranked
WHERE sample_count <= ? OR ((sample_index+1)*?/sample_count) > (sample_index*?/sample_count)
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

func percentileOffset(count, numerator, denominator int64) int64 {
	return (numerator*count+denominator-1)/denominator - 1
}

func (s *SQLiteStore) analysisCosts(ctx context.Context, query model.Query) (model.AnalysisCostComponents, error) {
	where, arguments, err := buildWhere(query)
	if err != nil {
		return model.AnalysisCostComponents{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return model.AnalysisCostComponents{}, ErrClosed
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+eventSelect+" FROM events "+where, arguments...)
	if err != nil {
		return model.AnalysisCostComponents{}, fmt.Errorf("query analysis costs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	totals := make([]model.NanoUSD, 4)
	modelTotals := map[string][]model.NanoUSD{}
	pricingIncomplete := false
	var pricedTokens int64
	for rows.Next() {
		event, errScan := scanEvent(rows)
		if errScan != nil {
			return model.AnalysisCostComponents{}, errScan
		}
		current, errPrice := s.price(event)
		if errPrice != nil {
			return model.AnalysisCostComponents{}, errPrice
		}
		if event.KnownCost == nil || current.KnownCost == nil || *event.KnownCost != *current.KnownCost ||
			event.UnpricedTokens != current.UnpricedTokens || event.PriceRuleID != current.RuleID || event.PriceSource != current.Source {
			pricingIncomplete = true
			continue
		}
		pricedTokens += event.Tokens.Total
		uncached := max(int64(0), event.Tokens.Input-event.Tokens.CacheRead-event.Tokens.CacheCreation)
		parts := []model.TokenUsage{{Input: uncached, Total: uncached},
			{Input: event.Tokens.CacheRead, CacheRead: event.Tokens.CacheRead, Total: event.Tokens.CacheRead},
			{Input: event.Tokens.CacheCreation, CacheCreation: event.Tokens.CacheCreation, Total: event.Tokens.CacheCreation},
			{Output: event.Tokens.Output, Total: event.Tokens.Output}}
		componentCosts := make([]model.NanoUSD, len(parts))
		for index, tokens := range parts {
			component := event
			component.Tokens = tokens
			priced, errPrice := s.price(component)
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
			modelTotals[event.Model] = make([]model.NanoUSD, 4)
		}
		for index, cost := range componentCosts {
			modelTotals[event.Model][index] += cost
		}
		for index := range totals {
			totals[index] += componentCosts[index]
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
		result.Models = append(result.Models, model.ModelCostComponents{Model: name, UncachedInputUSD: costs[0].String(), CacheReadUSD: costs[1].String(), CacheCreationUSD: costs[2].String(), OutputUSD: costs[3].String(), TotalUSD: (costs[0] + costs[1] + costs[2] + costs[3]).String()})
	}
	slices.SortFunc(result.Models, func(a, b model.ModelCostComponents) int { return strings.Compare(a.Model, b.Model) })
	result.UncachedInputUSD = totals[0].String()
	result.CacheReadUSD = totals[1].String()
	result.CacheCreationUSD = totals[2].String()
	result.OutputUSD = totals[3].String()
	var totalCost model.NanoUSD
	for _, cost := range totals {
		totalCost += cost
	}
	result.BlendedUSDPerMillion = decimalProductsRatio(int64(totalCost), 1, pricedTokens, 1000)
	if !s.retentionCutoff.IsZero() && query.Start.Before(s.retentionCutoff) {
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

func (s *SQLiteStore) analysisMatrix(ctx context.Context, query model.Query) (model.AnalysisKeyModelMatrix, error) {
	where, arguments, err := buildWhere(query)
	if err != nil {
		return model.AnalysisKeyModelMatrix{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return model.AnalysisKeyModelMatrix{}, ErrClosed
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key_id,model,COUNT(DISTINCT proxy_request_id),
SUM(input_tokens),SUM(output_tokens),SUM(cached_tokens),SUM(cache_read_tokens),SUM(cache_creation_tokens),
SUM(reasoning_tokens),SUM(total_tokens),SUM(COALESCE(known_cost_nano,0)),SUM(generation_time_ms),COUNT(generation_time_ms)
FROM events `+where+` GROUP BY key_id,model`, arguments...)
	if err != nil {
		return model.AnalysisKeyModelMatrix{}, fmt.Errorf("query analysis matrix: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := model.AnalysisKeyModelMatrix{Keys: []string{}, Models: []string{}, Cells: []model.AnalysisMatrixCell{}}
	keys, models := map[string]struct{}{}, map[string]struct{}{}
	for rows.Next() {
		var cell model.AnalysisMatrixCell
		var generation sql.NullInt64
		if err := rows.Scan(&cell.KeyID, &cell.Model, &cell.Requests, &cell.InputTokens, &cell.OutputTokens,
			&cell.CachedTokens, &cell.CacheReadTokens, &cell.CacheCreationTokens, &cell.ReasoningTokens,
			&cell.TotalTokens, &cell.KnownCost, &generation, &cell.GenerationSampleCount); err != nil {
			return model.AnalysisKeyModelMatrix{}, err
		}
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
	if !s.retentionCutoff.IsZero() && query.Start.Before(s.retentionCutoff) {
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
