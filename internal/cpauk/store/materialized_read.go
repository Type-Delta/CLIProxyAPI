package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// materialized15mAggregate is one dimension row from analytics_15m. A caller
// must combine rows with the same bucket before publishing a point because
// each row retains the dimensions required by filters and later breakdowns.
type materialized15mAggregate struct {
	BucketStart      int64
	BucketEnd        int64
	FirstActivity    int64
	LastActivity     int64
	Succeeded        bool
	ProxyRequests    int64
	UpstreamAttempts int64
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

// materialized15mRead contains exact request identities for each bucket. The
// IDs are needed when rows with different dimensions share one proxy request;
// summing row proxy_requests would over-count that request.
type materialized15mRead struct {
	Eligible   bool
	Aggregates []materialized15mAggregate
	Requests   map[int64]map[string]struct{}
}

// ReadMaterialized15Minute reads complete UTC 15-minute buckets selected by a
// validated timeseries/activity query. Non-UTC queries and partial boundaries
// return Eligible=false so callers can preserve calendar and DST semantics by
// reading raw events instead.
func (s *SQLiteStore) ReadMaterialized15Minute(ctx context.Context, query model.Query) (materialized15mRead, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.readMaterialized15m(ctx, query)
}

// readMaterialized15m assumes the caller holds s.mu.RLock. Keeping this small
// no-lock variant lets query paths share their existing read transaction.
func (s *SQLiteStore) readMaterialized15m(ctx context.Context, query model.Query) (materialized15mRead, error) {
	result := materialized15mRead{Requests: map[int64]map[string]struct{}{}}
	if s.db == nil {
		return result, ErrClosed
	}
	if query.TimeZone != "UTC" || query.Start.UnixNano()%materialized15mDurationNS != 0 ||
		query.End.UnixNano()%materialized15mDurationNS != 0 {
		return result, nil
	}
	result.Eligible = true
	where, arguments, err := buildMaterialized15mWhere(query, "a")
	if err != nil {
		return materialized15mRead{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.bucket_start_ns,a.bucket_end_ns,
a.first_activity_ns,a.last_activity_ns,a.succeeded,a.proxy_requests,a.upstream_attempts,
a.input_tokens,a.output_tokens,a.reasoning_tokens,a.cached_tokens,a.cache_read_tokens,
a.cache_creation_tokens,a.total_tokens,a.generation_time_ms,a.generation_sample_count,
a.known_cost_nano,a.unpriced_tokens,a.token_quality FROM analytics_15m a `+where+
		" ORDER BY a.bucket_start_ns", arguments...)
	if err != nil {
		return materialized15mRead{}, fmt.Errorf("read materialized 15-minute aggregates: %w", err)
	}
	for rows.Next() {
		var aggregate materialized15mAggregate
		var quality string
		if err := rows.Scan(&aggregate.BucketStart, &aggregate.BucketEnd, &aggregate.FirstActivity,
			&aggregate.LastActivity, &aggregate.Succeeded, &aggregate.ProxyRequests,
			&aggregate.UpstreamAttempts, &aggregate.InputTokens, &aggregate.OutputTokens,
			&aggregate.ReasoningTokens, &aggregate.CachedTokens, &aggregate.CacheReadTokens,
			&aggregate.CacheCreate, &aggregate.TotalTokens, &aggregate.GenerationTime,
			&aggregate.GenerationSample, &aggregate.KnownCost, &aggregate.UnpricedTokens, &quality); err != nil {
			_ = rows.Close()
			return materialized15mRead{}, fmt.Errorf("scan materialized 15-minute aggregate: %w", err)
		}
		aggregate.TokenQuality = model.TokenQuality(quality)
		result.Aggregates = append(result.Aggregates, aggregate)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return materialized15mRead{}, fmt.Errorf("read materialized 15-minute aggregate rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return materialized15mRead{}, fmt.Errorf("close materialized 15-minute aggregate rows: %w", err)
	}

	requestWhere, requestArguments, err := buildMaterialized15mWhere(query, "r")
	if err != nil {
		return materialized15mRead{}, err
	}
	requestRows, err := s.db.QueryContext(ctx, `SELECT r.bucket_start_ns,r.proxy_request_id
FROM analytics_15m_requests r `+requestWhere+" ORDER BY r.bucket_start_ns", requestArguments...)
	if err != nil {
		return materialized15mRead{}, fmt.Errorf("read materialized 15-minute request identities: %w", err)
	}
	for requestRows.Next() {
		var bucketStart int64
		var requestID string
		if err := requestRows.Scan(&bucketStart, &requestID); err != nil {
			_ = requestRows.Close()
			return materialized15mRead{}, fmt.Errorf("scan materialized 15-minute request identity: %w", err)
		}
		if result.Requests[bucketStart] == nil {
			result.Requests[bucketStart] = map[string]struct{}{}
		}
		result.Requests[bucketStart][requestID] = struct{}{}
	}
	if err := requestRows.Err(); err != nil {
		_ = requestRows.Close()
		return materialized15mRead{}, fmt.Errorf("read materialized 15-minute request identities: %w", err)
	}
	if err := requestRows.Close(); err != nil {
		return materialized15mRead{}, fmt.Errorf("close materialized 15-minute request identities: %w", err)
	}
	return result, nil
}

func buildMaterialized15mWhere(query model.Query, prefix string) (string, []any, error) {
	column := func(name string) string { return prefix + "." + name }
	clauses := []string{column("bucket_start_ns") + " >= ?", column("bucket_end_ns") + " <= ?"}
	arguments := []any{query.Start.UnixNano(), query.End.UnixNano()}
	if len(query.KeyIDs) != 0 {
		clauses = append(clauses, inClause(column("key_id"), len(query.KeyIDs)))
		for _, keyID := range query.KeyIDs {
			arguments = append(arguments, keyID)
		}
	}
	columns := map[string]string{
		"provider": "provider", "model": "model", "credential_id": "credential_id",
		"endpoint_class": "endpoint_class", "auth_type": "auth_type", "service_tier": "service_tier",
		"error_class": "error_class", "status_code": "status_code", "token_quality": "token_quality",
	}
	for name, raw := range query.Filters {
		if name == "success" {
			var value bool
			if err := json.Unmarshal(raw, &value); err != nil {
				return "", nil, fmt.Errorf("decode success filter: %w", err)
			}
			clauses = append(clauses, column("succeeded")+" = ?")
			arguments = append(arguments, value)
			continue
		}
		if name == "source" {
			var values []string
			if err := json.Unmarshal(raw, &values); err != nil {
				return "", nil, fmt.Errorf("decode source filter: %w", err)
			}
			var sourceClauses []string
			var batchIDs []string
			for _, value := range values {
				switch value {
				case "native":
					sourceClauses = append(sourceClauses, column("import_batch_id")+" = ''")
				case "import":
					sourceClauses = append(sourceClauses, column("import_batch_id")+" <> ''")
				default:
					batchIDs = append(batchIDs, value)
				}
			}
			if len(batchIDs) > 0 {
				sourceClauses = append(sourceClauses, inClause(column("import_batch_id"), len(batchIDs)))
				for _, batchID := range batchIDs {
					arguments = append(arguments, batchID)
				}
			}
			if len(sourceClauses) == 0 {
				return "", nil, fmt.Errorf("source filter is empty")
			}
			clauses = append(clauses, "("+strings.Join(sourceClauses, " OR ")+")")
			continue
		}
		nameColumn, ok := columns[name]
		if !ok {
			return "", nil, fmt.Errorf("materialized 15-minute filter %q is unsupported", name)
		}
		if name == "status_code" {
			var values []int
			if err := json.Unmarshal(raw, &values); err != nil {
				return "", nil, fmt.Errorf("decode status filter: %w", err)
			}
			clauses = append(clauses, inClause(column(nameColumn), len(values)))
			for _, value := range values {
				arguments = append(arguments, value)
			}
			continue
		}
		var values []string
		if err := json.Unmarshal(raw, &values); err != nil {
			return "", nil, fmt.Errorf("decode %s filter: %w", name, err)
		}
		clauses = append(clauses, inClause(column(nameColumn), len(values)))
		for _, value := range values {
			arguments = append(arguments, value)
		}
	}
	return "WHERE " + strings.Join(clauses, " AND "), arguments, nil
}
