package chstore

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// r1GroupedSelection keeps accounting facts separate from exact request identities.
// A request shared by raw and retained history counts once in each dimension.
func (s *Store) r1GroupedSelection(ctx context.Context, q model.Query, dimension string) (readSelection, error) {
	expression, ok := dimensionExpression(dimension)
	if !ok {
		return readSelection{}, fmt.Errorf("unsupported dimension %q", dimension)
	}
	dependencies := map[string][]string{
		"provider": {"provider"}, "model": {"model"}, "credential": {"credential_id"}, "key": {"key_id"},
		"endpoint": {"endpoint_class"}, "failure": {"error_class"}, "latency": {"latency_ms"},
		"cache": {"cache_read_tokens"}, "service_tier": {"service_tier_used", "service_tier_requested"}, "source": {"import_batch_id"},
	}[dimension]
	retainedExpression := map[string]string{
		"provider": "provider", "model": "model", "credential": "if(credential_id='','Unknown',credential_id)",
		"key": "key_id", "endpoint": "endpoint_class", "failure": "if(error_class='','success',error_class)",
		"latency": "latency_bucket", "cache": "cache_class", "service_tier": "if(service_tier='','Unknown',service_tier)",
		"source": "if(import_batch_id='','native','import')",
	}[dimension]
	fields := "input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,cache_creation_tokens,total_tokens,token_quality,known_cost_nano,unpriced_tokens"
	columns := append(strings.Split(fields, ","), dependencies...)
	// cache_read_tokens is both a metric and a dimension dependency.
	unique := make([]string, 0, len(columns))
	seen := map[string]bool{}
	for _, c := range columns {
		if !seen[c] {
			unique = append(unique, c)
			seen[c] = true
		}
	}
	raw, err := s.rawSelection(ctx, q, unique...)
	if err != nil {
		return readSelection{}, err
	}
	retained, err := s.retainedSelection(ctx, q, false, retainedExpression+" AS dimension_value", "upstream_attempts", fields)
	if err != nil {
		return readSelection{}, err
	}
	requestColumns := append([]string{"proxy_request_id"}, dependencies...)
	rawIDs, err := s.rawSelection(ctx, q, requestColumns...)
	if err != nil {
		return readSelection{}, err
	}
	retainedIDs, err := s.retainedSelection(ctx, q, true, retainedExpression+" AS dimension_value", "proxy_request_id")
	if err != nil {
		return readSelection{}, err
	}
	sql := `WITH facts AS (SELECT ` + expression + ` AS dimension_value,toInt64(1) AS upstream_attempts,` + fields + ` FROM (` + raw.SQL + `) UNION ALL ` + retained.SQL + `),
 identities AS (SELECT ` + expression + ` AS dimension_value,proxy_request_id FROM (` + rawIDs.SQL + `) UNION ALL ` + retainedIDs.SQL + `),
 accounting AS (SELECT dimension_value,sum(upstream_attempts) AS attempts,
 sum(input_tokens) AS input,sum(output_tokens) AS output,sum(reasoning_tokens) AS reasoning,
 sum(cached_tokens) AS cached,sum(cache_read_tokens) AS cache_read,sum(cache_creation_tokens) AS cache_creation,
 sum(total_tokens) AS token_total,accurateCast(sum(toInt128(coalesce(known_cost_nano,0))),'Int64') AS cost,sum(unpriced_tokens) AS unpriced,
 if(countIf(token_quality='missing')>0,'missing',if(countIf(token_quality='estimated')>0,'estimated','exact')) AS quality
 FROM facts GROUP BY dimension_value),
 request_counts AS (SELECT dimension_value,toInt64(uniqExact(proxy_request_id)) AS requests FROM identities GROUP BY dimension_value)
 SELECT a.*,coalesce(r.requests,0) AS requests FROM accounting a LEFT JOIN request_counts r ON a.dimension_value=r.dimension_value`
	args := append(raw.Args, retained.Args...)
	args = append(args, rawIDs.Args...)
	args = append(args, retainedIDs.Args...)
	return readSelection{sql, args}, nil
}

func (s *Store) Dimensions(ctx context.Context, query model.Query) (model.DimensionPage, error) {
	if err := s.validateQuery(&query, model.OperationDimensions); err != nil {
		return model.DimensionPage{}, err
	}
	if _, ok := dimensionExpression(query.Dimension); !ok {
		return model.DimensionPage{}, fmt.Errorf("unsupported dimension %q", query.Dimension)
	}
	selection, err := query.SelectionDigest()
	if err != nil {
		return model.DimensionPage{}, err
	}
	var cursor model.Cursor
	if query.Cursor != "" {
		cursor, err = s.config.CursorCodec.Decode(query.Cursor)
		if err != nil {
			return model.DimensionPage{}, err
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return model.DimensionPage{}, ErrClosed
	}
	if err := s.validateRetainedRange(ctx, query); err != nil {
		return model.DimensionPage{}, err
	}
	grouped, err := s.r1GroupedSelection(ctx, query, query.Dimension)
	if err != nil {
		return model.DimensionPage{}, err
	}
	statement := "SELECT * FROM (" + grouped.SQL + ")"
	if cursor.Rank > 0 {
		metric, _ := strconv.ParseInt(cursor.Metric, 10, 64)
		statement += " WHERE token_total < ? OR (token_total = ? AND dimension_value > ?)"
		grouped.Args = append(grouped.Args, metric, metric, cursor.Value)
	}
	statement += " ORDER BY token_total DESC,dimension_value ASC LIMIT ?"
	grouped.Args = append(grouped.Args, query.PageSize+1)
	rows, err := s.db.Conn().Query(ctx, statement, grouped.Args...)
	if err != nil {
		return model.DimensionPage{}, err
	}
	defer func() { _ = rows.Close() }()
	result := model.DimensionPage{Meta: responseMeta(query), Dimension: query.Dimension}
	for rows.Next() {
		var row model.DimensionRow
		var quality string
		var cost int64
		if err := rows.Scan(r1DimensionArgs(&row, &quality, &cost)...); err != nil {
			return model.DimensionPage{}, err
		}
		row.KnownCost = model.NanoUSD(cost)
		row.Tokens.Schema = "normalized-v1"
		row.Tokens.Quality = model.TokenQuality(quality)
		result.Rows = append(result.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return model.DimensionPage{}, err
	}
	if len(result.Rows) > query.PageSize {
		result.Rows = result.Rows[:query.PageSize]
		last := result.Rows[len(result.Rows)-1]
		result.Meta.NextCursor, err = s.config.CursorCodec.Encode(model.Cursor{Version: 1, Operation: model.OperationDimensions, Selection: selection, Metric: strconv.FormatInt(last.Tokens.Total, 10), Value: last.Value, Rank: cursor.Rank + len(result.Rows)})
		if err != nil {
			return model.DimensionPage{}, err
		}
	}
	return result, nil
}

func r1DimensionArgs(row *model.DimensionRow, quality *string, cost *int64) []any {
	return []any{&row.Value, &row.UpstreamAttempts, &row.Tokens.Input, &row.Tokens.Output, &row.Tokens.Reasoning, &row.Tokens.Cached, &row.Tokens.CacheRead, &row.Tokens.CacheCreation, &row.Tokens.Total, cost, &row.UnpricedTokens, quality, &row.ProxyRequests}
}

func (s *Store) Leaderboard(ctx context.Context, query model.Query) (model.LeaderboardPage, error) {
	if err := s.validateQuery(&query, model.OperationLeaderboard); err != nil {
		return model.LeaderboardPage{}, err
	}
	selection, err := query.SelectionDigest()
	if err != nil {
		return model.LeaderboardPage{}, err
	}
	var cursor model.Cursor
	if query.Cursor != "" {
		cursor, err = s.config.CursorCodec.Decode(query.Cursor)
		if err != nil {
			return model.LeaderboardPage{}, err
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return model.LeaderboardPage{}, ErrClosed
	}
	if err := s.validateRetainedRange(ctx, query); err != nil {
		return model.LeaderboardPage{}, err
	}
	grouped, err := s.r1GroupedSelection(ctx, query, "key")
	if err != nil {
		return model.LeaderboardPage{}, err
	}
	metric := "token_total"
	if query.SortBy == model.LeaderboardSortCost {
		metric = "cost"
	}
	var cursorMetric int64
	if cursor.Rank > 0 {
		if cursor.SortBy == model.LeaderboardSortCost {
			v, e := model.ParseNanoUSD(cursor.Metric)
			err = e
			cursorMetric = int64(v)
		} else {
			cursorMetric, err = strconv.ParseInt(cursor.Metric, 10, 64)
		}
		if err != nil {
			return model.LeaderboardPage{}, err
		}
		var count uint64
		args := append(append([]any(nil), grouped.Args...), cursor.KeyID, cursorMetric)
		if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM ("+grouped.SQL+") WHERE dimension_value=? AND "+metric+"=?", args...).Scan(&count); err != nil {
			return model.LeaderboardPage{}, err
		}
		if count == 0 {
			return model.LeaderboardPage{}, fmt.Errorf("leaderboard cursor is stale")
		}
	}
	// Lexical neighbors suffice to find every prefix collision. Window functions
	// run before the cursor predicate, preserving global IDs, ranks and denominator.
	statement := `SELECT dimension_value,attempts,input,output,reasoning,cached,cache_read,cache_creation,token_total,cost,unpriced,quality,requests,
 global_rank,metric_total,
 substring(dimension_value,1,arrayFirst(n -> substring(dimension_value,1,n) != substring(previous_id,1,n) AND substring(dimension_value,1,n) != substring(next_id,1,n),range(toUInt64(` + strconv.Itoa(model.ShortKeyIDMinLength) + `),toUInt64(65),toUInt64(2)))) AS short_id
 FROM (SELECT *,row_number() OVER (ORDER BY ` + metric + ` DESC,dimension_value ASC) AS global_rank,
 accurateCast(sum(toInt128(` + metric + `)) OVER (), 'Int64') AS metric_total,
 lagInFrame(dimension_value,1,'') OVER (ORDER BY dimension_value ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) AS previous_id,
 leadInFrame(dimension_value,1,'') OVER (ORDER BY dimension_value ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) AS next_id
 FROM (` + grouped.SQL + `))`
	args := append([]any(nil), grouped.Args...)
	if cursor.Rank > 0 {
		statement += " WHERE " + metric + " < ? OR (" + metric + " = ? AND dimension_value > ?)"
		args = append(args, cursorMetric, cursorMetric, cursor.KeyID)
	}
	statement += " ORDER BY " + metric + " DESC,dimension_value ASC LIMIT ?"
	args = append(args, query.PageSize+1)
	rows, err := s.db.Conn().Query(ctx, statement, args...)
	if err != nil {
		return model.LeaderboardPage{}, err
	}
	defer func() { _ = rows.Close() }()
	result := model.LeaderboardPage{Meta: responseMeta(query), SortBy: query.SortBy}
	for rows.Next() {
		var dimension model.DimensionRow
		var row model.LeaderboardRow
		var quality string
		var cost, total int64
		var rank uint64
		scanArgs := append(r1DimensionArgs(&dimension, &quality, &cost), &rank, &total, &row.ShortKeyID)
		if err := rows.Scan(scanArgs...); err != nil {
			return model.LeaderboardPage{}, err
		}
		dimension.Tokens.Schema = "normalized-v1"
		dimension.Tokens.Quality = model.TokenQuality(quality)
		row.Rank = int(rank)
		row.KeyID = dimension.Value
		row.ProxyRequests = dimension.ProxyRequests
		row.UpstreamAttempts = dimension.UpstreamAttempts
		row.Tokens = dimension.Tokens
		row.KnownCost = model.NanoUSD(cost)
		row.UnpricedTokens = dimension.UnpricedTokens
		value := row.Tokens.Total
		if query.SortBy == model.LeaderboardSortCost {
			value = cost
		}
		row.PercentOfTotal = percentage(value, total)
		result.Rows = append(result.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return model.LeaderboardPage{}, err
	}
	if len(result.Rows) > query.PageSize {
		result.Rows = result.Rows[:query.PageSize]
		last := result.Rows[len(result.Rows)-1]
		value := strconv.FormatInt(last.Tokens.Total, 10)
		if query.SortBy == model.LeaderboardSortCost {
			value = last.KnownCost.String()
		}
		result.Meta.NextCursor, err = s.config.CursorCodec.Encode(model.Cursor{Version: 1, Operation: model.OperationLeaderboard, SortBy: query.SortBy, Selection: selection, Metric: value, KeyID: last.KeyID, Rank: last.Rank})
		if err != nil {
			return model.LeaderboardPage{}, err
		}
	}
	return result, nil
}
