package chstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"

	"slices"
	"strings"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/ext"
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
	ctx = withReadCutoffs(ctx)
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
	ctx = withReadCutoffs(ctx)
	if s.db == nil {
		return model.AnalysisCostComponents{}, ErrClosed
	}
	selection, err := s.rawSelection(ctx, query, strings.Split(strings.ReplaceAll(analysisCostEventSelect, "\n", ""), ",")...)
	if err != nil {
		return model.AnalysisCostComponents{}, err
	}
	ctx, selection, modelTotals, pricedTokens, pricingIncomplete, err := s.analysisSQLCosts(ctx, selection)
	if err != nil {
		return model.AnalysisCostComponents{}, fmt.Errorf("aggregate analysis costs: %w", err)
	}
	rows, err := s.db.Conn().Query(ctx, "SELECT "+analysisCostEventSelect+",toInt64(count()) FROM ("+selection.SQL+") GROUP BY "+analysisCostEventSelect, selection.Args...)
	if err != nil {
		return model.AnalysisCostComponents{}, fmt.Errorf("query analysis costs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	totals := make([]model.NanoUSD, 5)
	for _, costs := range modelTotals {
		for i, cost := range costs {
			totals[i] += cost
		}
	}
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
	ctx = withReadCutoffs(ctx)
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

// analysisSQLCosts resolves pricing once per dimension tuple, then performs the
// existing per-event rounding and reconciliation before aggregation. Recorded
// breakdowns use cumulative rounding and cannot replace Analysis's independently
// rounded components. Historical totals must still match the current PriceBook.
func (s *Store) analysisSQLCosts(ctx context.Context, raw readSelection) (context.Context, readSelection, map[string][]model.NanoUSD, int64, bool, error) {
	const dimensions = "provider,model,requested_alias,service_tier_requested,service_tier_used"
	key := "toJSONString(tuple(" + dimensions + "))"
	table, err := ext.NewTable("analysis_rates", ext.Column("rate_key", "String"), ext.Column("rate_rule", "String"), ext.Column("rate_source", "String"), ext.Column("denominator", "String"), ext.Column("numerators", "Array(String)"))
	if err != nil {
		return ctx, raw, nil, 0, false, err
	}
	rows, err := s.db.Conn().Query(ctx, "SELECT DISTINCT "+key+","+dimensions+" FROM ("+raw.SQL+") WHERE NOT (coalesce(price_source,'')='kiro-credits' AND price_usd_per_credit_nano IS NOT NULL AND known_cost_nano IS NOT NULL)", raw.Args...)
	if err != nil {
		return ctx, raw, nil, 0, false, err
	}
	for rows.Next() {
		var lookup string
		var event model.Event
		if err = rows.Scan(&lookup, &event.Provider, &event.Model, &event.RequestedAlias, &event.ServiceTierRequested, &event.ServiceTierUsed); err != nil {
			break
		}
		price, priceErr := s.config.PriceBook.Price(event)
		// Unusual arbitrary-precision rates and invalid books keep the Go path,
		// including its original errors. Zero tokens avoid synthetic cost overflow.
		if priceErr != nil || price.Breakdown == nil {
			continue
		}
		components := []model.PricingComponent{price.Breakdown.Input, price.Breakdown.CacheRead, price.Breakdown.CacheWrite, price.Breakdown.Output}
		rates := make([]*big.Rat, 4)
		denominator := big.NewInt(1)
		usable := true
		for i, part := range components {
			rate, ok := new(big.Rat).SetString(part.RatePerMillion)
			if !ok || rate.Sign() < 0 {
				usable = false
				break
			}
			rates[i] = rate.Mul(rate, big.NewRat(1000, 1))
			gcd := new(big.Int).GCD(nil, nil, denominator, rate.Denom())
			denominator.Mul(denominator, new(big.Int).Quo(rate.Denom(), gcd))
		}
		if !usable || denominator.BitLen() > 100 {
			continue
		}
		numerators := make([]string, 4)
		for i, rate := range rates {
			numerator := new(big.Int).Mul(rate.Num(), new(big.Int).Quo(new(big.Int).Set(denominator), rate.Denom()))
			if numerator.BitLen() > 100 {
				usable = false
				break
			}
			numerators[i] = numerator.String()
		}
		if usable {
			err = table.Append(lookup, price.RuleID, price.Source, denominator.String(), numerators)
			if err != nil {
				break
			}
		}
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return ctx, raw, nil, 0, false, err
	}
	ctx = ch.Context(ctx, ch.WithExternalTable(table))
	// Bound the SQL arithmetic well inside Int256. Values outside this bound
	// retain the original big.Rat evaluation, including cost_overflow errors.
	// Joined rate columns are read through ifNull so join_use_nulls=1 sessions
	// see unmatched rows the same way as the default empty-string join.
	base := `WITH selected AS (` + raw.SQL + `), rated AS (SELECT *,
 coalesce(price_source,'')='kiro-credits' AND price_usd_per_credit_nano IS NOT NULL AND known_cost_nano IS NOT NULL AS credit,
 toInt256OrZero(ifNull(denominator,'')) AS d,
 greatest(toInt64(0),input_tokens-cache_read_tokens-cache_creation_tokens) AS uncached,
 [uncached,cache_read_tokens,cache_creation_tokens,output_tokens] AS tokens,
 arrayMap((t,n)->toInt256(t)*toInt256OrZero(n),tokens,if(empty(numerators),['0','0','0','0'],numerators)) AS amounts
 FROM selected LEFT JOIN analysis_rates ON ` + key + `=rate_key), rounded AS (SELECT *,
 arraySum(amounts) AS amount,
 intDiv(2*amount+d,2*greatest(d,toInt256(1))) AS expected,
 arrayMap(a->intDiv(2*a+d,2*greatest(d,toInt256(1))),amounts) AS parts,
 credit OR (d>0 AND arrayAll(t->t>=0 AND t<=1000000000000000,[input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,total_tokens]) AND expected<=9223372036854775807) AS fast
 FROM rated), checked AS (SELECT *,
 known_cost_nano IS NOT NULL AND expected=known_cost_nano AND unpriced_tokens=greatest(toInt64(0),total_tokens-input_tokens-output_tokens)
 AND coalesce(price_rule_id,'')=ifNull(rate_rule,'') AND coalesce(price_source,'')=ifNull(rate_source,'') AS valid,
 expected-arraySum(parts) AS residual FROM rounded), reconciled AS (SELECT *,
 arrayMap(i->parts[i]+if(residual>=0,if(i=indexOf(arrayMap(t->t>0,tokens),true),residual,toInt256(0)),
 -least(parts[i],greatest(toInt256(0),-residual-arraySum(arraySlice(parts,1,i-1))))),range(1,5)) AS costs
 FROM checked) `
	fallback := readSelection{SQL: base + "SELECT " + analysisCostEventSelect + " FROM reconciled WHERE NOT fast", Args: raw.Args}
	rows, err = s.db.Conn().Query(ctx, base+`SELECT model,
 sum(toInt64(if(NOT credit AND valid,costs[1],0))),sum(toInt64(if(NOT credit AND valid,costs[2],0))),
 sum(toInt64(if(NOT credit AND valid,costs[3],0))),sum(toInt64(if(NOT credit AND valid,costs[4],0))),
 sum(if(credit,coalesce(known_cost_nano,0),0)),
 sum(if(credit OR valid,greatest(toInt64(0),total_tokens-unpriced_tokens),0)),
 toUInt8(max(NOT credit AND (NOT valid OR unpriced_tokens>0))),toUInt8(max(credit OR valid))
 FROM reconciled WHERE fast GROUP BY model`, raw.Args...)
	if err != nil {
		return ctx, raw, nil, 0, false, err
	}
	defer func() { _ = rows.Close() }()
	totals := map[string][]model.NanoUSD{}
	var priced int64
	incomplete := false
	for rows.Next() {
		var name string
		var costs [5]int64
		var tokens int64
		var partial, included uint8
		if err := rows.Scan(&name, &costs[0], &costs[1], &costs[2], &costs[3], &costs[4], &tokens, &partial, &included); err != nil {
			return ctx, raw, nil, 0, false, err
		}
		if included != 0 {
			totals[name] = make([]model.NanoUSD, 5)
			for i, cost := range costs {
				totals[name][i] = model.NanoUSD(cost)
			}
		}
		priced += tokens
		incomplete = incomplete || partial != 0
	}
	return ctx, fallback, totals, priced, incomplete, rows.Err()
}
