package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

type materialized15mAggregate struct {
	BucketStart      int64
	BucketEnd        int64
	FirstActivity    int64
	LastActivity     int64
	ProxyRequests    int64
	UpstreamAttempts int64
	Succeeded        int64
	Failed           int64
	InputTokens      int64
	OutputTokens     int64
	ReasoningTokens  int64
	CachedTokens     int64
	CacheReadTokens  int64
	CacheCreate      int64
	TotalTokens      int64
	GenerationTime   int64
	GenerationSample int64
	KnownCost        model.NanoUSD
	UnpricedTokens   int64
	TokenQuality     model.TokenQuality
}

type materialized15mRead struct {
	Eligible      bool
	Aggregates    []materialized15mAggregate
	RequestCounts map[int64]int64
}

func (s *SQLiteStore) ReadMaterialized15Minute(ctx context.Context, query model.Query) (materialized15mRead, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.readMaterialized15m(ctx, query)
}

// readMaterialized15m only accepts selections represented by the compact
// dimensions. All other filters use the raw path, preserving exact semantics
// for uncommon combinations and partial/calendar boundaries.
func (s *SQLiteStore) readMaterialized15m(ctx context.Context, query model.Query) (materialized15mRead, error) {
	result := materialized15mRead{RequestCounts: map[int64]int64{}}
	if s.db == nil {
		return result, ErrClosed
	}
	kind, values, ok := compact15mSelection(query)
	if !ok || !compact15mWidthEligible(query.BucketWidth) || query.TimeZone != "UTC" || query.Start.UnixNano()%materialized15mDurationNS != 0 ||
		query.End.UnixNano()%materialized15mDurationNS != 0 || !compactQueryAligned(query.Start.UnixNano(), query.BucketWidth) ||
		!compactQueryAligned(query.End.UnixNano(), query.BucketWidth) {
		return result, nil
	}
	result.Eligible = true
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return materialized15mRead{}, err
	}
	defer func() { _ = tx.Rollback() }()
	clauses := []string{"bucket_start_ns >= ?", "bucket_end_ns <= ?", "dimension_kind = ?"}
	arguments := []any{query.Start.UnixNano(), query.End.UnixNano(), kind}
	if kind == "overall" {
		clauses = append(clauses, "dimension_value = ''")
	} else {
		clauses = append(clauses, inClause("dimension_value", len(values)))
		for _, value := range values {
			arguments = append(arguments, value)
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT bucket_start_ns,MAX(bucket_end_ns),MIN(first_activity_ns),MAX(last_activity_ns),
SUM(proxy_requests),SUM(upstream_attempts),SUM(succeeded_attempts),SUM(failed_attempts),
SUM(input_tokens),SUM(output_tokens),SUM(reasoning_tokens),SUM(cached_tokens),SUM(cache_read_tokens),
SUM(cache_creation_tokens),SUM(total_tokens),SUM(generation_time_ms),SUM(generation_sample_count),
SUM(known_cost_nano),SUM(unpriced_tokens),
CASE WHEN SUM(CASE WHEN token_quality='missing' THEN 1 ELSE 0 END)>0 THEN 'missing'
WHEN SUM(CASE WHEN token_quality='estimated' THEN 1 ELSE 0 END)>0 THEN 'estimated' ELSE 'exact' END
FROM analytics_15m_compact WHERE `+strings.Join(clauses, " AND ")+`
GROUP BY bucket_start_ns ORDER BY bucket_start_ns`, arguments...)
	if err != nil {
		return materialized15mRead{}, fmt.Errorf("read compact 15-minute aggregates: %w", err)
	}
	for rows.Next() {
		var aggregate materialized15mAggregate
		var quality string
		if err := rows.Scan(&aggregate.BucketStart, &aggregate.BucketEnd, &aggregate.FirstActivity,
			&aggregate.LastActivity, &aggregate.ProxyRequests, &aggregate.UpstreamAttempts,
			&aggregate.Succeeded, &aggregate.Failed, &aggregate.InputTokens, &aggregate.OutputTokens,
			&aggregate.ReasoningTokens, &aggregate.CachedTokens, &aggregate.CacheReadTokens,
			&aggregate.CacheCreate, &aggregate.TotalTokens, &aggregate.GenerationTime,
			&aggregate.GenerationSample, &aggregate.KnownCost, &aggregate.UnpricedTokens, &quality); err != nil {
			_ = rows.Close()
			return materialized15mRead{}, fmt.Errorf("scan compact 15-minute aggregate: %w", err)
		}
		aggregate.TokenQuality = model.TokenQuality(quality)
		result.Aggregates = append(result.Aggregates, aggregate)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return materialized15mRead{}, fmt.Errorf("read compact 15-minute aggregate rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return materialized15mRead{}, fmt.Errorf("close compact 15-minute aggregate rows: %w", err)
	}

	requestClauses := []string{"bucket_start_ns >= ?", "bucket_start_ns + ? <= ?"}
	requestArguments := []any{query.Start.UnixNano(), materialized15mDurationNS, query.End.UnixNano()}
	if kind != "overall" {
		column := map[string]string{"model": "model", "provider": "provider", "key": "key_id"}[kind]
		requestClauses = append(requestClauses, inClause(column, len(values)))
		for _, value := range values {
			requestArguments = append(requestArguments, value)
		}
	}
	requestExpression, requestBucketArguments, err := compactRequestBucketExpression(query.BucketWidth)
	if err != nil {
		return materialized15mRead{}, err
	}
	requestArguments = append(requestBucketArguments, requestArguments...)
	requestRows, err := tx.QueryContext(ctx, `SELECT compact_bucket,COUNT(DISTINCT proxy_request_id)
FROM (SELECT `+requestExpression+` AS compact_bucket,proxy_request_id
FROM analytics_15m_request_ids WHERE `+strings.Join(requestClauses, " AND ")+`)
GROUP BY compact_bucket ORDER BY compact_bucket`, requestArguments...)
	if err != nil {
		return materialized15mRead{}, fmt.Errorf("read compact 15-minute request counts: %w", err)
	}
	for requestRows.Next() {
		var bucketStart, count int64
		if err := requestRows.Scan(&bucketStart, &count); err != nil {
			_ = requestRows.Close()
			return materialized15mRead{}, fmt.Errorf("scan compact 15-minute request count: %w", err)
		}
		result.RequestCounts[bucketStart] = count
	}
	if err := requestRows.Err(); err != nil {
		_ = requestRows.Close()
		return materialized15mRead{}, fmt.Errorf("read compact 15-minute request counts: %w", err)
	}
	if err := requestRows.Close(); err != nil {
		return materialized15mRead{}, fmt.Errorf("close compact 15-minute request counts: %w", err)
	}
	return result, nil
}

func compactRequestBucketExpression(width string) (string, []any, error) {
	var duration time.Duration
	var anchor int64
	if width == "1w" {
		duration = 7 * 24 * time.Hour
		anchor = time.Date(1970, time.January, 5, 0, 0, 0, 0, time.UTC).UnixNano()
	} else if width == "1d" {
		duration = 24 * time.Hour
	} else {
		parsed, err := time.ParseDuration(width)
		if err != nil || parsed <= 0 {
			return "", nil, fmt.Errorf("invalid compact bucket width %q", width)
		}
		duration = parsed
	}
	// The positive-modulo form also handles dates before the Unix epoch.
	return "bucket_start_ns - ((bucket_start_ns - ? ) % ? + ? ) % ?", []any{anchor, int64(duration), int64(duration), int64(duration)}, nil
}

func compact15mSelection(query model.Query) (string, []string, bool) {
	if len(query.Filters) == 0 {
		if len(query.KeyIDs) != 0 {
			return "key", query.KeyIDs, true
		}
		return "overall", nil, true
	}
	if len(query.KeyIDs) != 0 || len(query.Filters) != 1 {
		return "", nil, false
	}
	for name, raw := range query.Filters {
		if name != "model" && name != "provider" {
			return "", nil, false
		}
		var values []string
		if err := json.Unmarshal(raw, &values); err != nil || len(values) == 0 {
			return "", nil, false
		}
		return name, values, true
	}
	return "", nil, false
}

func compactDimensionKind(dimension string) (string, bool) {
	switch dimension {
	case "provider", "model", "key":
		return dimension, true
	default:
		return "", false
	}
}
