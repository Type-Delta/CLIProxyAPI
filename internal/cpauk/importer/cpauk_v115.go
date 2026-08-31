package importer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/collector"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	_ "modernc.org/sqlite"
)

const CPAUKV115SourceKind = "cpauk-v1.15.0"

type CPAUKV115Row struct {
	Origin              string
	ID                  int64
	EventKey            string
	RawAPIKey           string
	Provider            string
	Endpoint            string
	AuthType            string
	RequestID           string
	Model               string
	ModelAlias          *string
	ServiceTier         string
	ResponseServiceTier string
	ExecutorType        string
	RequestedAt         time.Time
	AuthIndex           string
	Failed              bool
	Generate            *bool
	LatencyMS           int64
	TTFTMS              *int64
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
}

func OpenCPAUKV115Source(ctx context.Context, path string) (*SQLiteSource, error) {
	hasHot, err := cpaukSourceHasTable(ctx, path, "usage_events")
	if err != nil {
		return nil, err
	}
	if !hasHot {
		return nil, fmt.Errorf("CPAUK v1.15 source is missing usage_events")
	}
	hasArchive, err := cpaukSourceHasTable(ctx, path, "usage_events_archive")
	if err != nil {
		return nil, err
	}
	hotSelect := cpaukV115Select("hot", "usage_events", 0)
	combined := hotSelect
	if hasArchive {
		combined += " UNION ALL " + cpaukV115Select("archive", "usage_events_archive", 1)
	}
	query := `WITH combined AS (` + combined + `), ranked AS (
SELECT *, ROW_NUMBER() OVER (PARTITION BY CASE WHEN event_key<>'' THEN event_key ELSE printf('id:%d',id) END
ORDER BY origin_order,id) AS duplicate_rank FROM combined)
SELECT origin,id,event_key,api_group_key,provider,endpoint,auth_type,request_id,model,model_alias,
service_tier,response_service_tier,executor_type,timestamp,auth_index,failed,generate,latency_ms,ttft_ms,
input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,cache_creation_tokens,total_tokens
FROM ranked WHERE duplicate_rank=1 ORDER BY timestamp,id,origin_order`
	return OpenSQLiteSource(ctx, path, CPAUKV115SourceKind, query, decodeCPAUKV115Row)
}

func cpaukV115Select(origin, table string, order int) string {
	return fmt.Sprintf(`SELECT '%s' AS origin,%d AS origin_order,id,event_key,api_group_key,provider,endpoint,
auth_type,request_id,model,model_alias,service_tier,response_service_tier,executor_type,timestamp,
auth_index,failed,generate,latency_ms,ttft_ms,input_tokens,output_tokens,reasoning_tokens,cached_tokens,
cache_read_tokens,cache_creation_tokens,total_tokens FROM %s`, origin, order, table)
}

func decodeCPAUKV115Row(rows *sql.Rows) (any, error) {
	var row CPAUKV115Row
	var alias, timestamp sql.NullString
	var generate sql.NullBool
	var ttft sql.NullInt64
	if err := rows.Scan(&row.Origin, &row.ID, &row.EventKey, &row.RawAPIKey, &row.Provider, &row.Endpoint,
		&row.AuthType, &row.RequestID, &row.Model, &alias, &row.ServiceTier, &row.ResponseServiceTier,
		&row.ExecutorType, &timestamp, &row.AuthIndex, &row.Failed, &generate, &row.LatencyMS, &ttft,
		&row.InputTokens, &row.OutputTokens, &row.ReasoningTokens, &row.CachedTokens, &row.CacheReadTokens,
		&row.CacheCreationTokens, &row.TotalTokens); err != nil {
		return nil, err
	}
	if alias.Valid {
		row.ModelAlias = &alias.String
	}
	if generate.Valid {
		row.Generate = &generate.Bool
	}
	if ttft.Valid {
		row.TTFTMS = &ttft.Int64
	}
	requestedAt, err := parseCPAUKTime(timestamp.String)
	if err != nil {
		return nil, err
	}
	row.RequestedAt = requestedAt
	return row, nil
}

func NewCPAUKV115Transformer(identityKey [32]byte, storeCredential bool) Transformer {
	return func(_ context.Context, source SourceRow) (model.Event, bool, error) {
		row, ok := source.Value.(CPAUKV115Row)
		if !ok {
			return model.Event{}, false, fmt.Errorf("unexpected CPAUK v1.15 source row")
		}
		attemptID := deterministicImportID("attempt", row)
		proxyID := deterministicImportID("request", row)
		ids := []string{attemptID, proxyID}
		index := 0
		sanitizer := collector.NewSanitizer(collector.SanitizerOptions{
			IdentityKey: identityKey, StoreCredential: storeCredential,
			NewID: func() (string, error) {
				if index >= len(ids) {
					return "", fmt.Errorf("deterministic import ID exhausted")
				}
				value := ids[index]
				index++
				return value, nil
			},
		})
		ttft := time.Duration(0)
		if row.TTFTMS != nil {
			ttft = time.Duration(*row.TTFTMS) * time.Millisecond
		}
		record := collector.Source{
			ProxyRequestID: row.RequestID, EndpointClass: row.Endpoint, Provider: row.Provider,
			ExecutorType: row.ExecutorType, Model: row.Model, APIKey: row.RawAPIKey,
			AuthIndex: row.AuthIndex, AuthType: row.AuthType, Alias: valueOrEmpty(row.ModelAlias),
			ServiceTier: row.ServiceTier, ResponseTier: row.ResponseServiceTier, Generated: row.Generate,
			RequestedAt: row.RequestedAt, Latency: time.Duration(row.LatencyMS) * time.Millisecond,
			TTFT: ttft, Failed: row.Failed,
			Tokens: collector.SourceTokens{Input: row.InputTokens, Output: row.OutputTokens,
				Reasoning: row.ReasoningTokens, Cached: row.CachedTokens,
				CacheRead: row.CacheReadTokens, CacheCreation: row.CacheCreationTokens,
				Total: row.TotalTokens, Quality: model.TokenQualityExact},
		}
		result, err := sanitizer.Sanitize(record)
		if err != nil {
			return model.Event{}, false, err
		}
		return result.Event, false, nil
	}
}

func deterministicImportID(kind string, row CPAUKV115Row) string {
	digest := sha256.Sum256([]byte(CPAUKV115SourceKind + "\x00" + kind + "\x00" + row.Origin + "\x00" +
		row.EventKey + "\x00" + fmt.Sprintf("%d", row.ID)))
	return hex.EncodeToString(digest[:16])
}

func parseCPAUKTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid CPAUK storage timestamp")
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func cpaukSourceHasTable(ctx context.Context, path, table string) (bool, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}).String() + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)"
	database, err := sql.Open("sqlite", uri)
	if err != nil {
		return false, err
	}
	defer func() { _ = database.Close() }()
	var count int
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&count); err != nil {
		return false, err
	}
	return count != 0, nil
}
