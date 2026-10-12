package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

const analysisLatencySampleLimit = 1000

func (s *SQLiteStore) Analysis(ctx context.Context, query model.Query) (model.Analysis, error) {
	if err := s.validateQuery(&query, model.OperationAnalysis); err != nil {
		return model.Analysis{}, err
	}
	return s.cachedAnalysis(ctx, query)
}

func (s *SQLiteStore) analysisUncached(ctx context.Context, query model.Query) (model.Analysis, error) {
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
	// Raw ranges used to run one complete timeseries query per model. That is
	// quadratic in the number of models because every query rescans the same
	// events table. A single grouped pass is substantially cheaper for the
	// common (non-retained) path. Retained ranges still use the established
	// rollup-aware implementation below.
	s.mu.RLock()
	rawOnly := s.retentionCutoff.IsZero() || !query.Start.Before(s.retentionCutoff)
	s.mu.RUnlock()
	if rawOnly {
		return s.analysisModelsRaw(ctx, query)
	}

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
	modelIndexes := make(map[string]int, len(dimensions.Rows))
	for _, row := range dimensions.Rows {
		item := model.AnalysisModel{
			Model: row.Value, Requests: row.ProxyRequests, InputTokens: row.Tokens.Input,
			OutputTokens: row.Tokens.Output, CachedTokens: row.Tokens.Cached,
			CacheReadTokens: row.Tokens.CacheRead, CacheCreationTokens: row.Tokens.CacheCreation,
			ReasoningTokens: row.Tokens.Reasoning, TotalTokens: row.Tokens.Total, KnownCost: row.KnownCost,
			UnpricedTokens: row.UnpricedTokens,
		}
		modelIndexes[row.Value] = len(result.Models)
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
		var generationTotal int64
		var generationSamples int64
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
				UnpricedTokens: point.UnpricedTokens, GenerationTimeMS: point.GenerationTimeMS,
				GenerationSampleCount: point.GenerationSampleCount,
			}
			bucket.Models = append(bucket.Models, modelPoint)
			if point.GenerationTimeMS != nil {
				generationTotal += *point.GenerationTimeMS
			}
			generationSamples += point.GenerationSampleCount
		}
		if generationSamples > 0 {
			index := modelIndexes[item.Model]
			result.Models[index].GenerationTimeMS = &generationTotal
			result.Models[index].GenerationSampleCount = generationSamples
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

type analysisModelBucketState struct {
	point    model.AnalysisModel
	requests map[string]struct{}
}

func (s *SQLiteStore) analysisModelsRaw(ctx context.Context, query model.Query) (model.AnalysisModelByTime, error) {
	width := analysisBucketWidth(query)
	sequence, err := analyticsBucketSequence(query, width)
	if err != nil {
		return model.AnalysisModelByTime{}, err
	}
	where, arguments, err := buildWhere(query)
	if err != nil {
		return model.AnalysisModelByTime{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return model.AnalysisModelByTime{}, ErrClosed
	}
	rows, err := s.db.QueryContext(ctx, `SELECT requested_at_ns,proxy_request_id,model,
input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,
cache_creation_tokens,total_tokens,known_cost_nano,unpriced_tokens,generation_time_ms
FROM events `+where, arguments...)
	if err != nil {
		return model.AnalysisModelByTime{}, fmt.Errorf("query raw analysis model buckets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	buckets := make(map[int64]map[string]*analysisModelBucketState, len(sequence))
	models := make(map[string]*model.AnalysisModel)
	modelRequests := make(map[string]map[string]struct{})
	bucketer, err := aggregate.NewBucketer(query.TimeZone, width)
	if err != nil {
		return model.AnalysisModelByTime{}, err
	}
	for rows.Next() {
		var requestedNS int64
		var requestID, modelName string
		var input, output, reasoning, cached, cacheRead, cacheCreation, total, unpriced int64
		var cost, generation sql.NullInt64
		if err := rows.Scan(&requestedNS, &requestID, &modelName, &input, &output, &reasoning,
			&cached, &cacheRead, &cacheCreation, &total, &cost, &unpriced, &generation); err != nil {
			return model.AnalysisModelByTime{}, fmt.Errorf("scan raw analysis model bucket: %w", err)
		}
		start, _, err := bucketer.Bounds(time.Unix(0, requestedNS).UTC())
		if err != nil {
			return model.AnalysisModelByTime{}, err
		}
		byModel := buckets[start.UnixNano()]
		if byModel == nil {
			byModel = make(map[string]*analysisModelBucketState)
			buckets[start.UnixNano()] = byModel
		}
		state := byModel[modelName]
		if state == nil {
			state = &analysisModelBucketState{point: model.AnalysisModel{Model: modelName}, requests: make(map[string]struct{})}
			byModel[modelName] = state
		}
		state.requests[requestID] = struct{}{}
		state.point.InputTokens += input
		state.point.OutputTokens += output
		state.point.ReasoningTokens += reasoning
		state.point.CachedTokens += cached
		state.point.CacheReadTokens += cacheRead
		state.point.CacheCreationTokens += cacheCreation
		state.point.TotalTokens += total
		state.point.UnpricedTokens += unpriced
		if cost.Valid {
			state.point.KnownCost += model.NanoUSD(cost.Int64)
		}
		if generation.Valid {
			value := generation.Int64
			if state.point.GenerationTimeMS != nil {
				value += *state.point.GenerationTimeMS
			}
			state.point.GenerationTimeMS = &value
			state.point.GenerationSampleCount++
		}
		item := models[modelName]
		if item == nil {
			item = &model.AnalysisModel{Model: modelName}
			models[modelName] = item
			modelRequests[modelName] = make(map[string]struct{})
		}
		modelRequests[modelName][requestID] = struct{}{}
		item.InputTokens += input
		item.OutputTokens += output
		item.ReasoningTokens += reasoning
		item.CachedTokens += cached
		item.CacheReadTokens += cacheRead
		item.CacheCreationTokens += cacheCreation
		item.TotalTokens += total
		item.UnpricedTokens += unpriced
		item.Requests = int64(len(modelRequests[modelName]))
		if cost.Valid {
			item.KnownCost += model.NanoUSD(cost.Int64)
		}
		if generation.Valid {
			value := generation.Int64
			if item.GenerationTimeMS != nil {
				value += *item.GenerationTimeMS
			}
			item.GenerationTimeMS = &value
			item.GenerationSampleCount++
		}
	}
	if err := rows.Err(); err != nil {
		return model.AnalysisModelByTime{}, fmt.Errorf("read raw analysis model buckets: %w", err)
	}

	ordered := make([]model.AnalysisModel, 0, len(models))
	for _, item := range models {
		ordered = append(ordered, *item)
	}
	slices.SortFunc(ordered, func(left, right model.AnalysisModel) int {
		if metric := cmp.Compare(right.TotalTokens, left.TotalTokens); metric != 0 {
			return metric
		}
		return strings.Compare(left.Model, right.Model)
	})
	if len(ordered) > 10 {
		ordered = ordered[:10]
	}
	allowed := make(map[string]struct{}, len(ordered))
	for _, item := range ordered {
		allowed[item.Model] = struct{}{}
	}

	result := model.AnalysisModelByTime{Models: ordered, Buckets: make([]model.AnalysisModelBucket, 0, len(sequence))}
	for _, bounds := range sequence {
		bucket := model.AnalysisModelBucket{Start: bounds.start, Models: make([]model.AnalysisModel, 0)}
		for name, state := range buckets[bounds.start.UnixNano()] {
			if _, ok := allowed[name]; !ok {
				continue
			}
			state.point.Requests = int64(len(state.requests))
			bucket.Models = append(bucket.Models, state.point)
		}
		slices.SortFunc(bucket.Models, func(left, right model.AnalysisModel) int { return strings.Compare(left.Model, right.Model) })
		result.Buckets = append(result.Buckets, bucket)
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
	metrics, err := s.analysisTimingMetrics(ctx, where, arguments)
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

// analysisTimingMetrics computes all latency summaries in one windowed pass. The generic
// timingMetrics helper is also used by summary endpoints, where percentile statistics are not
// needed; analysis has to return five metrics and used to issue a separate count, percentile,
// and median scan for each one.
func (s *SQLiteStore) analysisTimingMetrics(ctx context.Context, where string, arguments []any) (map[string]model.TimingMetric, error) {
	const queryTemplate = `WITH source AS MATERIALIZED (
SELECT latency_ms,first_token_latency_ms,provider_latency_ms,time_to_first_token_ms,generation_time_ms
FROM events ` + "PLACEHOLDER" + `
), samples(name,value) AS (
SELECT 'e2e',latency_ms FROM source WHERE latency_ms IS NOT NULL
UNION ALL SELECT 'latency',first_token_latency_ms FROM source WHERE first_token_latency_ms IS NOT NULL
UNION ALL SELECT 'provider_latency',provider_latency_ms FROM source WHERE provider_latency_ms IS NOT NULL
UNION ALL SELECT 'ttft',time_to_first_token_ms FROM source WHERE time_to_first_token_ms IS NOT NULL
UNION ALL SELECT 'generation',generation_time_ms FROM source WHERE generation_time_ms IS NOT NULL
), ranked AS (
SELECT name,value,COUNT(*) OVER (PARTITION BY name) AS sample_count,
ROW_NUMBER() OVER (PARTITION BY name ORDER BY value)-1 AS sample_index
FROM samples
)
SELECT name,sample_count,SUM(value),MAX(value),
MAX(CASE WHEN sample_index=((95*sample_count+99)/100)-1 THEN value END),
AVG(CASE WHEN sample_index IN ((sample_count-1)/2,sample_count/2) THEN value END)
FROM ranked GROUP BY name,sample_count`
	statement := strings.Replace(queryTemplate, "PLACEHOLDER", where, 1)
	metrics := map[string]model.TimingMetric{
		"e2e":              {Source: "observed"},
		"latency":          {Source: "observed_dispatch_to_first_token"},
		"provider_latency": {Source: "observed_dispatch_to_response"},
		"ttft":             {Source: "observed"},
		"generation":       {Source: "observed_first_to_last_token"},
	}
	rows, err := s.db.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return nil, fmt.Errorf("query analysis timing metrics: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		var metric model.TimingMetric
		var total, maximum, p95 sql.NullInt64
		var median sql.NullFloat64
		if err := rows.Scan(&name, &metric.SampleCount, &total, &maximum, &p95, &median); err != nil {
			return nil, fmt.Errorf("scan analysis timing metrics: %w", err)
		}
		metric.Source = metrics[name].Source
		if total.Valid {
			metric.TotalMS = &total.Int64
			metric.MaxMS = &maximum.Int64
		}
		if p95.Valid {
			value := float64(p95.Int64)
			metric.P95MS = &value
		}
		if median.Valid {
			metric.MedianMS = &median.Float64
		}
		metrics[name] = metric
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read analysis timing metrics: %w", err)
	}
	return metrics, nil
}

func percentileOffset(count, numerator, denominator int64) int64 {
	return (numerator*count+denominator-1)/denominator - 1
}

const analysisCostEventSelect = `provider,model,requested_alias,
input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,total_tokens,
known_cost_nano,unpriced_tokens,price_rule_id,price_source`

type analysisPriceKey struct {
	provider, model, alias   string
	hasAlias                 bool
	input, output, cacheRead int64
	cacheCreation, total     int64
}

type analysisPriceCacheValue struct {
	result aggregate.PriceResult
	err    error
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
	rows, err := s.db.QueryContext(ctx, "SELECT "+analysisCostEventSelect+" FROM events "+where, arguments...)
	if err != nil {
		return model.AnalysisCostComponents{}, fmt.Errorf("query analysis costs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	totals := make([]model.NanoUSD, 4)
	modelTotals := map[string][]model.NanoUSD{}
	priceCache := make(map[analysisPriceKey]analysisPriceCacheValue)
	priceCached := func(event model.Event) (aggregate.PriceResult, error) {
		key := analysisPriceKey{provider: event.Provider, model: event.Model, input: event.Tokens.Input,
			output: event.Tokens.Output, cacheRead: event.Tokens.CacheRead, cacheCreation: event.Tokens.CacheCreation,
			total: event.Tokens.Total}
		if event.RequestedAlias != nil {
			key.alias, key.hasAlias = *event.RequestedAlias, true
		}
		if cached, ok := priceCache[key]; ok {
			return cached.result, cached.err
		}
		priced, priceErr := s.price(event)
		priceCache[key] = analysisPriceCacheValue{result: priced, err: priceErr}
		return priced, priceErr
	}
	pricingIncomplete := false
	var pricedTokens int64
	for rows.Next() {
		var event model.Event
		var requestedAlias, priceRuleID, priceSource sql.NullString
		var knownCost sql.NullInt64
		if err := rows.Scan(&event.Provider, &event.Model, &requestedAlias,
			&event.Tokens.Input, &event.Tokens.Output, &event.Tokens.CacheRead, &event.Tokens.CacheCreation, &event.Tokens.Total,
			&knownCost, &event.UnpricedTokens, &priceRuleID, &priceSource); err != nil {
			return model.AnalysisCostComponents{}, fmt.Errorf("scan analysis costs: %w", err)
		}
		if requestedAlias.Valid {
			event.RequestedAlias = &requestedAlias.String
		}
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
		pricedTokens += max(int64(0), event.Tokens.Total-event.UnpricedTokens)
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
SUM(reasoning_tokens),SUM(total_tokens),SUM(COALESCE(known_cost_nano,0)),SUM(unpriced_tokens),SUM(generation_time_ms),COUNT(generation_time_ms)
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
			&cell.TotalTokens, &cell.KnownCost, &cell.UnpricedTokens, &generation, &cell.GenerationSampleCount); err != nil {
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
