package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type dailyStatsKey struct {
	dayStart int64
	keyID    string
}

type dailyStatsRow struct {
	dayEnd        int64
	requests      int64
	succeeded     int64
	failed        int64
	input         int64
	output        int64
	reasoning     int64
	cached        int64
	cacheRead     int64
	cacheCreation int64
	total         int64
}

type dailyStatsQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func dailyStatsLocation(ctx context.Context, queryer dailyStatsQueryer, configuredZone string) (*time.Location, error) {
	var zone string
	err := queryer.QueryRowContext(ctx, "SELECT value FROM analytics_metadata WHERE key = ?", retentionTimeZoneMetadataKey).Scan(&zone)
	if errors.Is(err, sql.ErrNoRows) {
		var rollups int64
		if err := queryer.QueryRowContext(ctx, "SELECT COUNT(*) FROM rollups").Scan(&rollups); err != nil {
			return nil, fmt.Errorf("inspect retained analytics time zone for daily stats: %w", err)
		}
		zone = configuredZone
		if rollups != 0 {
			zone = "UTC"
		}
	} else if err != nil {
		return nil, fmt.Errorf("read retained analytics time zone for daily stats: %w", err)
	}
	if zone == "Local" {
		return time.Local, nil
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fmt.Errorf("load retained analytics time zone %q for daily stats: %w", zone, err)
	}
	return location, nil
}

func rebuildDailyStats(ctx context.Context, tx *sql.Tx, location *time.Location) (int64, error) {
	stats := make(map[dailyStatsKey]*dailyStatsRow)
	rows, err := tx.QueryContext(ctx, `SELECT bucket_start_ns,key_id,succeeded,upstream_attempts,
input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,
cache_creation_tokens,total_tokens FROM rollups`)
	if err != nil {
		return 0, fmt.Errorf("query retained rows for daily stats: %w", err)
	}
	for rows.Next() {
		var bucketStart, attempts, input, output, reasoning, cached, cacheRead, cacheCreation, total int64
		var keyID string
		var succeeded bool
		if err := rows.Scan(&bucketStart, &keyID, &succeeded, &attempts, &input, &output,
			&reasoning, &cached, &cacheRead, &cacheCreation, &total); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan retained row for daily stats: %w", err)
		}
		dayStart, dayEnd := retentionDayBounds(time.Unix(0, bucketStart).UTC(), location)
		key := dailyStatsKey{dayStart: dayStart.UnixNano(), keyID: keyID}
		row := stats[key]
		if row == nil {
			row = &dailyStatsRow{dayEnd: dayEnd.UnixNano()}
			stats[key] = row
		}
		if succeeded {
			row.succeeded += attempts
		} else {
			row.failed += attempts
		}
		row.input += input
		row.output += output
		row.reasoning += reasoning
		row.cached += cached
		row.cacheRead += cacheRead
		row.cacheCreation += cacheCreation
		row.total += total
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("read retained rows for daily stats: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close retained rows for daily stats: %w", err)
	}

	requestRows, err := tx.QueryContext(ctx, `SELECT bucket_start_ns,key_id,proxy_request_id
FROM request_rollups ORDER BY key_id,proxy_request_id,bucket_start_ns`)
	if err != nil {
		return 0, fmt.Errorf("query retained requests for daily stats: %w", err)
	}
	var lastKey dailyStatsKey
	var lastRequestID string
	haveLast := false
	for requestRows.Next() {
		var bucketStart int64
		var keyID, requestID string
		if err := requestRows.Scan(&bucketStart, &keyID, &requestID); err != nil {
			_ = requestRows.Close()
			return 0, fmt.Errorf("scan retained request for daily stats: %w", err)
		}
		dayStart, dayEnd := retentionDayBounds(time.Unix(0, bucketStart).UTC(), location)
		key := dailyStatsKey{dayStart: dayStart.UnixNano(), keyID: keyID}
		if haveLast && key == lastKey && requestID == lastRequestID {
			continue
		}
		row := stats[key]
		if row == nil {
			row = &dailyStatsRow{dayEnd: dayEnd.UnixNano()}
			stats[key] = row
		}
		row.requests++
		lastKey, lastRequestID, haveLast = key, requestID, true
	}
	if err := requestRows.Err(); err != nil {
		_ = requestRows.Close()
		return 0, fmt.Errorf("read retained requests for daily stats: %w", err)
	}
	if err := requestRows.Close(); err != nil {
		return 0, fmt.Errorf("close retained requests for daily stats: %w", err)
	}

	if _, err := tx.ExecContext(ctx, "DELETE FROM daily_stats"); err != nil {
		return 0, fmt.Errorf("clear daily stats: %w", err)
	}
	statement, err := tx.PrepareContext(ctx, `INSERT INTO daily_stats (
day_start_ns,day_end_ns,key_id,requests,succeeded,failed,input_tokens,output_tokens,
reasoning_tokens,cached_tokens,cache_read_tokens,cache_creation_tokens,total_tokens)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, fmt.Errorf("prepare daily stats: %w", err)
	}
	defer func() { _ = statement.Close() }()
	for key, row := range stats {
		if _, err := statement.ExecContext(ctx, key.dayStart, row.dayEnd, key.keyID, row.requests,
			row.succeeded, row.failed, row.input, row.output, row.reasoning, row.cached,
			row.cacheRead, row.cacheCreation, row.total); err != nil {
			return 0, fmt.Errorf("write daily stats: %w", err)
		}
	}
	return int64(len(stats)), nil
}

func (s *SQLiteStore) rebuildDailyStats(ctx context.Context) (int64, error) {
	location, err := s.retentionLocation(ctx)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin daily stats rebuild: %w", err)
	}
	count, err := rebuildDailyStats(ctx, tx, location)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit daily stats rebuild: %w", err)
	}
	return count, nil
}
