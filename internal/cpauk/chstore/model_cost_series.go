package chstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/ext"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// ModelCostSeries aggregates recorded component costs without repricing history.
// Legacy rows lacking a breakdown use the same exact pricing engine as SQLite;
// only distinct scalar pricing inputs cross that compatibility boundary.
func (s *Store) ModelCostSeries(ctx context.Context, query model.Query) (model.ModelCostSeries, error) {
	if err := s.validateQuery(&query, model.OperationModelCostSeries); err != nil {
		return model.ModelCostSeries{}, err
	}
	width := c2r2BucketWidth(query)
	bucketQuery := query
	bucketQuery.BucketWidth = width
	bucketer, err := aggregate.NewBucketer(query.TimeZone, width)
	if err != nil {
		return model.ModelCostSeries{}, err
	}
	var names []string
	_ = json.Unmarshal(query.Filters["model"], &names)
	result := model.ModelCostSeries{Meta: responseMeta(query), Model: names[0], Buckets: []model.ModelCostSeriesBucket{}}
	start, end, err := bucketer.Bounds(query.Start)
	if err != nil {
		return result, err
	}
	for start.Before(query.End) {
		if len(result.Buckets) >= model.MaxBuckets {
			return model.ModelCostSeries{}, fmt.Errorf("analytics series exceeds %d buckets", model.MaxBuckets)
		}
		result.Buckets = append(result.Buckets, model.ModelCostSeriesBucket{Start: start, Tiers: []model.ModelCostSeriesTier{}})
		start = end
		_, end, err = bucketer.Bounds(start)
		if err != nil {
			return result, err
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ctx = withReadCutoffs(ctx)
	if s.db == nil {
		return result, ErrClosed
	}
	result.Partial = !s.retentionCutoff.IsZero() && query.Start.Before(s.retentionCutoff)
	columns := append([]string{"requested_at_ns", "pricing_breakdown"}, c2r2PricingColumns...)
	raw, err := s.rawSelection(ctx, query, columns...)
	if err != nil {
		return result, err
	}
	legacy, err := s.c2r2LegacyPricing(ctx, raw)
	if err != nil {
		return result, err
	}
	ctx = ch.Context(ctx, ch.WithExternalTable(legacy))
	costNames := []string{"input", "cache_read", "cache_write", "output"}
	var componentSQL, validCosts, rates, costs, totals []string
	for i, name := range costNames {
		alias := fmt.Sprintf("c%d", i)
		// Conversion stays decimal/integer, including values above Float64 precision.
		componentSQL = append(componentSQL, "toInt64(toDecimal128OrZero(JSONExtractString(details,'"+name+"','cost_usd'),9)*1000000000) AS "+alias)
		validCosts = append(validCosts, alias)
		rates = append(rates, "if(complete,JSONExtractString(details,'"+name+"','rate_per_million_usd'),NULL)")
		costs = append(costs, "if(complete,"+alias+",CAST(NULL AS Nullable(Int64))) AS cost"+fmt.Sprint(i))
		totals = append(totals, "accurateCast(sumOrNull(toInt128(cost"+fmt.Sprint(i)+")), 'Nullable(Int64)')")
	}
	statement := `WITH selected AS (` + raw.SQL + `),
 priced AS (SELECT e.*,coalesce(e.pricing_breakdown,l.breakdown) AS details FROM selected AS e LEFT JOIN c2r2_legacy_prices AS l ON ` + c2r2PricingKey("e.") + `=l.lookup_key WHERE total_tokens!=0 OR coalesce(known_cost_nano,0)!=0),
 components AS (SELECT *,` + strings.Join(componentSQL, ",") + `, coalesce(price_source,'')='kiro-credits' AND known_cost_nano IS NOT NULL AS credit FROM priced),
 points AS (SELECT *,NOT credit AND details IS NOT NULL AND known_cost_nano IS NOT NULL AND (` + strings.Join(validCosts, "+") + `)=known_cost_nano AS complete,
 if(NOT credit AND JSONExtractString(details,'service_tier')!='',JSONExtractString(details,'service_tier'),coalesce(nullIf(` + c2r2TrimTierSQL("service_tier_used") + `,''),nullIf(` + c2r2TrimTierSQL("service_tier_requested") + `,''),'default')) AS tier,
 if(credit,NULL,JSONExtract(details,'input_tier_multiplier','Nullable(String)')) AS input_multiplier,
 if(credit,NULL,JSONExtract(details,'output_tier_multiplier','Nullable(String)')) AS output_multiplier FROM components),
 layers AS (SELECT *,[` + strings.Join(rates, ",") + `] AS rates,` + strings.Join(costs, ",") + `,
 known_cost_nano IS NULL OR unpriced_tokens>0 OR (NOT credit AND NOT complete) AS partial FROM points)
 SELECT ` + coreBucketExpression("requested_at_ns", bucketQuery) + ` AS bucket,provider,tier,rates,input_multiplier,output_multiplier,
 sum(greatest(toInt64(0),input_tokens-cache_read_tokens-cache_creation_tokens)),sum(cache_read_tokens),sum(cache_creation_tokens),sum(output_tokens),sum(total_tokens),
 accurateCast(sum(toInt128(coalesce(known_cost_nano,0))),'Int64'),accurateCast(sum(toInt128(if(credit,coalesce(known_cost_nano,0),0))),'Int64'),` + strings.Join(totals, ",") + `,toUInt8(max(partial))
 FROM layers GROUP BY bucket,provider,tier,rates,input_multiplier,output_multiplier ORDER BY bucket,` + c2r2TierOrderSQL() + ` SETTINGS output_format_json_escape_forward_slashes=0`
	rows, err := s.db.Conn().Query(ctx, statement, raw.Args...)
	if err != nil {
		return result, fmt.Errorf("query model cost series: %w", err)
	}
	defer func() { _ = rows.Close() }()
	buckets := map[int64][]model.ModelCostSeriesTier{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		var ns, known, credit int64
		var partial uint8
		point := model.ModelCostSeriesTier{Rates: make([]*string, 4), Costs: make([]*model.NanoUSD, 4)}
		var components [4]*int64
		if err := rows.Scan(&ns, &point.Provider, &point.ServiceTier, &point.Rates, &point.InputMultiplier, &point.OutputMultiplier,
			&point.InputTokens, &point.CacheReadTokens, &point.CacheCreationTokens, &point.OutputTokens, &point.TotalTokens, &known, &credit,
			&components[0], &components[1], &components[2], &components[3], &partial); err != nil {
			return result, err
		}
		point.KnownCost = model.NanoUSD(known)
		point.CreditBasedCost = model.NanoUSD(credit)
		for i, cost := range components {
			if cost != nil {
				value := model.NanoUSD(*cost)
				point.Costs[i] = &value
			}
		}
		result.Partial = result.Partial || partial != 0
		buckets[ns] = append(buckets[ns], point)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	for i := range result.Buckets {
		points := buckets[result.Buckets[i].Start.UnixNano()]
		if len(points) > 0 {
			result.Buckets[i].Tiers = points
		}
	}
	return result, nil
}

// RE2's Unicode White_Space set matches strings.TrimSpace, including NBSP.
func c2r2TrimTierSQL(column string) string {
	spaces := `\\x{0009}-\\x{000D}\\x{0020}\\x{0085}\\x{00A0}\\x{1680}\\x{2000}-\\x{200A}\\x{2028}\\x{2029}\\x{202F}\\x{205F}\\x{3000}`
	return "lowerUTF8(replaceRegexpAll(" + column + ",'^[" + spaces + "]+|[" + spaces + "]+$',''))"
}

// SQLite sorts the Go JSON representation of this tuple, including encoding/json's
// HTML and JavaScript line-separator escapes. Match that ordering in SQL.
func c2r2TierOrderSQL() string {
	expression := "toJSONString(tuple(provider,tier,rates,input_multiplier,output_multiplier))"
	for _, pair := range [][2]string{{"<", "003c"}, {">", "003e"}, {"&", "0026"}, {"\u2028", "2028"}, {"\u2029", "2029"}} {
		expression = "replaceAll(" + expression + ",'" + pair[0] + "',concat(char(92),'u" + pair[1] + "'))"
	}
	return expression
}

func c2r2BucketWidth(query model.Query) string {
	if query.BucketWidth != "" {
		return query.BucketWidth
	}
	switch duration := query.End.Sub(query.Start); {
	case duration <= 24*time.Hour:
		return "5m"
	case duration <= 31*24*time.Hour:
		return "1h"
	default:
		return "1d"
	}
}

var c2r2PricingColumns = []string{"provider", "model", "requested_alias", "service_tier_requested", "service_tier_used", "input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens", "total_tokens", "known_cost_nano", "unpriced_tokens", "price_rule_id", "price_source"}

func c2r2PricingKey(prefix string) string {
	columns := make([]string, len(c2r2PricingColumns))
	for i, column := range c2r2PricingColumns {
		columns[i] = prefix + column
	}
	return "toJSONString(tuple(" + strings.Join(columns, ",") + "))"
}

// The external lookup contains no event JSON and no attempt identifiers. SQL
// deduplicates pricing inputs; SQL also performs every chart sum after the join.
func (s *Store) c2r2LegacyPricing(ctx context.Context, raw readSelection) (*ext.Table, error) {
	table, err := ext.NewTable("c2r2_legacy_prices", ext.Column("lookup_key", "String"), ext.Column("breakdown", "Nullable(String)"))
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Conn().Query(ctx, "SELECT DISTINCT "+c2r2PricingKey("")+","+strings.Join(c2r2PricingColumns, ",")+" FROM ("+raw.SQL+") WHERE pricing_breakdown IS NULL AND (total_tokens!=0 OR coalesce(known_cost_nano,0)!=0) AND NOT (coalesce(price_source,'')='kiro-credits' AND known_cost_nano IS NOT NULL) SETTINGS output_format_json_escape_forward_slashes=0", raw.Args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var key string
		var event model.Event
		var known *int64
		var rule, source *string
		if err := rows.Scan(&key, &event.Provider, &event.Model, &event.RequestedAlias, &event.ServiceTierRequested, &event.ServiceTierUsed, &event.Tokens.Input, &event.Tokens.Output, &event.Tokens.CacheRead, &event.Tokens.CacheCreation, &event.Tokens.Total, &known, &event.UnpricedTokens, &rule, &source); err != nil {
			return nil, err
		}
		current, err := s.config.PriceBook.Price(event)
		if err != nil {
			return nil, err
		}
		var breakdown *string
		if known != nil && current.KnownCost != nil && int64(*current.KnownCost) == *known && rule != nil && current.RuleID == *rule && source != nil && current.Source == *source && current.UnpricedTokens == event.UnpricedTokens && current.Breakdown != nil {
			data, err := json.Marshal(current.Breakdown)
			if err != nil {
				return nil, err
			}
			value := string(data)
			breakdown = &value
		}
		if err := table.Append(key, breakdown); err != nil {
			return nil, err
		}
	}
	return table, rows.Err()
}
