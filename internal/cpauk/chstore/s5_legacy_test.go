package chstore

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func (s *Store) s5LegacyAnalysisCosts(ctx context.Context, query model.Query) (model.AnalysisCostComponents, error) {
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
