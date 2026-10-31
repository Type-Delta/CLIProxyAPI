package chstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/ext"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

type c4bSourceHour struct{ First, Last int64 }
type c4bSourceBucket struct{ From, End, Start int64 }

// c4bPlanRetention simulates SQLite's oldest-source bucket selection, including
// fractional DST overlaps, using only one min/max pair per canonical source hour.
// Data-free hours are skipped in Go; no per-bucket database round trips occur.
func c4bPlanRetention(groups []c4bSourceHour, cutoff time.Time, zone string, daily bool) ([]c4bSourceBucket, int64, error) {
	var plan []c4bSourceBucket
	var cursor int64
	haveCursor := false
	for i := 0; i < len(groups); {
		group := groups[i]
		if haveCursor && group.Last < cursor {
			i++
			continue
		}
		at := group.First
		if haveCursor {
			at = max(at, cursor)
		}
		start, end, errBounds := retainedBucketBounds(time.Unix(0, at).UTC(), zone, daily)
		if errBounds != nil {
			return nil, 0, errBounds
		}

		if end.After(cutoff) {
			floor := min(start.UnixNano(), cutoff.UnixNano())
			if haveCursor {
				floor = max(floor, cursor)
			}
			return plan, floor, nil
		}
		from := start.UnixNano()
		if haveCursor {
			from = max(from, cursor)
		}
		if end.UnixNano() <= from {
			return nil, 0, fmt.Errorf("retention bucket does not advance at %s", time.Unix(0, from))
		}
		plan = append(plan, c4bSourceBucket{from, end.UnixNano(), start.UnixNano()})
		cursor = end.UnixNano()
		haveCursor = true
	}
	width := "1h"
	if daily {
		width = "1d"
	}
	start, _, err := aggregate.BucketBounds(cutoff, zone, width)
	if err != nil {
		return nil, 0, err
	}
	floor := start.UnixNano()
	if haveCursor {
		floor = max(floor, cursor)
	}
	return plan, floor, nil
}

// c4bPromote stages an entire grain's eligible run in two INSERT SELECT passes.
// Go supplies canonical non-overlapping source intervals as an external table;
// ASOF JOIN assigns each source to the same bucket SQLite would have selected.
func (s *Store) c4bPromote(ctx context.Context, cutoff time.Time, daily bool, result *RetentionResult) error {
	bounds, err := s.readCutoffs(ctx)
	if err != nil {
		return err
	}
	table, column, grain, step := "analytics_events", "requested_at_ns", "hourly", "raw"
	sourceWhere := "is_deleted=0 AND requested_at_ns<?"
	args := []any{cutoff.UnixNano()}
	old, oldSet := bounds.raw, bounds.rawSet
	if daily {
		if !bounds.rawSet {
			return nil
		}
		if cutoff.UnixNano() > bounds.raw {
			cutoff = time.Unix(0, bounds.raw).UTC()
		}
		cutoff, _, err = aggregate.BucketBounds(cutoff, s.config.RetentionTimeZone, "1d")
		if err != nil {
			return err
		}
		table, column, grain, step = "analytics_rollups", "bucket_start_ns", "daily", "hourly"
		sourceWhere = "is_deleted=0 AND grain='hourly' AND bucket_end_ns<=? AND " + s.c4bPublishedPredicate()
		args = []any{cutoff.UnixNano()}
		old, oldSet = bounds.hourly, bounds.hourlySet
	}
	if oldSet {
		sourceWhere += " AND " + column + ">=?"
		args = append(args, old)
	}
	hourExpr := coreBucketExpression(column, model.Query{TimeZone: s.config.RetentionTimeZone, BucketWidth: "1h"})
	dayExpr := coreBucketExpression(column, model.Query{TimeZone: s.config.RetentionTimeZone, BucketWidth: "1d"})
	rows, err := s.db.Conn().Query(ctx, "SELECT min("+column+"),max("+column+") FROM "+s.table(table)+" FINAL WHERE "+sourceWhere+" GROUP BY "+hourExpr+","+dayExpr+" ORDER BY min("+column+")", args...)
	if err != nil {
		return err
	}
	groups := []c4bSourceHour{}
	for rows.Next() {
		var group c4bSourceHour
		if err = rows.Scan(&group.First, &group.Last); err != nil {
			_ = rows.Close()
			return err
		}
		groups = append(groups, group)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	plan, floor, err := c4bPlanRetention(groups, cutoff, s.config.RetentionTimeZone, daily)
	if err != nil {
		return err
	}
	operations, err := s.loadControl(ctx, "retention")
	if err != nil {
		return err
	}
	var previous struct {
		End     int64             `json:"bucket_end_ns"`
		Buckets []c4bSourceBucket `json:"buckets"`
	}
	if raw, ok := operations[step]; ok {
		if err = json.Unmarshal(raw, &previous); err != nil {
			return err
		}
		// An interrupted run keeps its exact plan until its boundary publishes.
		if len(previous.Buckets) > 0 && previous.End > old && previous.End <= cutoff.UnixNano() {
			plan, floor = previous.Buckets, previous.End
		}
	}
	if oldSet {
		floor = max(floor, old)
	}
	if len(plan) == 0 {
		return s.c4bPublishBoundary(ctx, floor, daily, nil)
	}
	// Limit the physical source to the complete consumed prefix. A prior crash's
	// already published sources stay excluded by old, even before cleanup runs.
	sourceWhere = "is_deleted=0 AND " + column + "<?"
	args = []any{floor}
	if daily {
		sourceWhere += " AND grain='hourly' AND " + s.c4bPublishedPredicate()
	}
	if oldSet {
		sourceWhere += " AND " + column + ">=?"
		args = append(args, old)
	}
	var sourceCount uint64
	var checkpoint int64
	if err = s.db.Conn().QueryRow(ctx, "SELECT count(),max("+column+") FROM "+s.table(table)+" FINAL WHERE "+sourceWhere, args...).Scan(&sourceCount, &checkpoint); err != nil {
		return err
	}
	generation := s.nextVersion()
	operations[step], err = json.Marshal(map[string]any{"source": step, "bucket_start_ns": plan[0].Start, "bucket_end_ns": floor, "generation": generation, "buckets": plan})
	if err != nil {
		return err
	}
	if err = s.replaceControl(ctx, "retention", operations); err != nil {
		return err
	}
	if err = c4bStep(ctx, step+":begin"); err != nil {
		return err
	}
	external, err := ext.NewTable("c4b_retention_bounds", ext.Column("c4b_key", "UInt8"), ext.Column("source_start_ns", "Int64"), ext.Column("source_end_ns", "Int64"), ext.Column("planned_start_ns", "Int64"), ext.Column("planned_end_ns", "Int64"))
	if err != nil {
		return err
	}
	for _, bucket := range plan {
		if err = external.Append(uint8(0), bucket.From, bucket.End, bucket.Start, bucket.End); err != nil {
			return err
		}
	}
	stageCtx := ch.Context(ctx, ch.WithExternalTable(external))
	joined := func(sourceTable, where, at string) string {
		return " FROM (SELECT *,toUInt8(0) AS c4b_key FROM " + s.table(sourceTable) + " FINAL WHERE " + where + ") AS e ASOF INNER JOIN c4b_retention_bounds AS b ON e.c4b_key=b.c4b_key AND e." + at + ">=b.source_start_ns WHERE e." + at + "<b.source_end_ns"
	}
	dimensions, activity, counts := c4bRawDimensions, "min(requested_at_ns),max(requested_at_ns)", "toInt64(uniqExact(proxy_request_id)),toInt64(count())"
	if daily {
		dimensions, activity, counts = c4bDimensions, "min(first_activity_ns),max(last_activity_ns)", "sum(proxy_requests),sum(upstream_attempts)"
	}
	groupBy := " GROUP BY b.planned_start_ns,b.planned_end_ns," + dimensions
	sql := "INSERT INTO " + s.table("analytics_rollups") + " (" + c4bRollupColumns + ",version,is_deleted,generation) SELECT ?,b.planned_start_ns,b.planned_end_ns," + activity + "," + dimensions + "," + counts + "," + c4bSums + ",?,toUInt8(0),?" + joined(table, sourceWhere, column) + groupBy
	insertArgs := []any{grain, s.nextVersion(), generation}
	insertArgs = append(insertArgs, args...)
	if err = s.db.Conn().Exec(synchronousWriteContext(stageCtx), sql, insertArgs...); err != nil {
		return fmt.Errorf("stage bulk %s retention totals: %w", grain, err)
	}
	if err = c4bStep(ctx, step+":totals"); err != nil {
		return err
	}
	requestTable := table
	if daily {
		requestTable = "analytics_request_rollups"
	}
	sql = "INSERT INTO " + s.table("analytics_request_rollups") + " (" + c4bRequestColumns + ",version,is_deleted,generation) SELECT DISTINCT ?,b.planned_start_ns,b.planned_end_ns,proxy_request_id," + dimensions + ",?,toUInt8(0),?" + joined(requestTable, sourceWhere, column)
	insertArgs = []any{grain, s.nextVersion(), generation}
	insertArgs = append(insertArgs, args...)
	if err = s.db.Conn().Exec(synchronousWriteContext(stageCtx), sql, insertArgs...); err != nil {
		return fmt.Errorf("stage bulk %s retention identities: %w", grain, err)
	}
	if err = c4bStep(ctx, step+":requests"); err != nil {
		return err
	}
	rows, err = s.db.Conn().Query(ctx, "SELECT bucket_start_ns,bucket_end_ns,import_batch_id,count() FROM "+s.table("analytics_rollups")+" FINAL WHERE grain=? AND generation=? AND is_deleted=0 GROUP BY bucket_start_ns,bucket_end_ns,import_batch_id", grain, generation)
	if err != nil {
		return err
	}
	buckets := []c4bBucket{}
	var rolled int64
	for rows.Next() {
		bucket := c4bBucket{Grain: grain}
		var count uint64
		if err = rows.Scan(&bucket.Start, &bucket.End, &bucket.Batch, &count); err != nil {
			_ = rows.Close()
			return err
		}
		buckets = append(buckets, bucket)
		rolled += int64(count)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	if err = s.c4bPublishHeads(ctx, buckets, generation); err != nil {
		return err
	}
	if err = s.c4bPublishBoundary(ctx, floor, daily, &checkpoint); err != nil {
		return err
	}
	if err = c4bStep(ctx, step+":published"); err != nil {
		return err
	}
	point := time.Unix(0, checkpoint).UTC()
	if daily {
		result.DailyRolledUpRows += rolled
		result.DeletedHourlyRows += int64(sourceCount)
		result.HourlyCheckpoint = &point
	} else {
		result.RolledUpRows += rolled
		result.DeletedRows += int64(sourceCount)
		result.RawCheckpoint = &point
	}
	return nil
}

func (s *Store) c4bCleanup(ctx context.Context, boundary int64, daily bool) error {
	if boundary == 0 {
		return nil
	}
	if daily {
		for _, table := range []string{"analytics_rollups", "analytics_request_rollups"} {
			if err := s.c4bDelete(ctx, table, "grain='hourly' AND bucket_end_ns<=?", boundary); err != nil {
				return err
			}
			if err := c4bStep(ctx, "hourly:source-"+table+"-deleted"); err != nil {
				return err
			}
		}
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	expired := "SELECT attempt_id FROM " + s.table("analytics_events") + " FINAL WHERE is_deleted=0 AND requested_at_ns<?"
	// One set-based mutation removes only retired attempt references, preserving
	// hot attempts in the same patch. It must finish before any event partition drops.
	sql := "ALTER TABLE " + s.table("analytics_outcomes") + " UPDATE attempt_ids=arrayFilter(id -> id NOT IN (" + expired + "),attempt_ids) WHERE arrayExists(id -> id IN (" + expired + "),attempt_ids) SETTINGS mutations_sync=2"
	if err := s.db.Conn().Exec(context.WithoutCancel(ctx), sql, boundary, boundary); err != nil {
		return fmt.Errorf("remove expired outcome references: %w", err)
	}
	if err := s.c4bDelete(ctx, "analytics_outcomes", "empty(attempt_ids)"); err != nil {
		return err
	}
	if err := c4bStep(ctx, "raw:outcomes-deleted"); err != nil {
		return err
	}
	cutoff := time.Unix(0, boundary).UTC()
	monthStart := time.Date(cutoff.Year(), cutoff.Month(), 1, 0, 0, 0, 0, time.UTC)
	month := cutoff.Year()*100 + int(cutoff.Month())
	rows, err := s.db.Conn().Query(ctx, "SELECT DISTINCT partition,partition_id FROM system.parts WHERE active AND database=? AND table='analytics_events'", s.db.Database())
	if err != nil {
		return err
	}
	var partitions []string
	for rows.Next() {
		var value, id string
		if err = rows.Scan(&value, &id); err != nil {
			_ = rows.Close()
			return err
		}
		number, errNumber := strconv.Atoi(value)
		if errNumber != nil {
			_ = rows.Close()
			return fmt.Errorf("decode analytics monthly partition %q: %w", value, errNumber)
		}
		if number != 0 && number < month {
			partitions = append(partitions, id)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, id := range partitions {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = s.db.Conn().Exec(context.WithoutCancel(ctx), "ALTER TABLE "+s.table("analytics_events")+" DROP PARTITION ID ?", id); err != nil {
			return fmt.Errorf("drop retained analytics month: %w", err)
		}
	}
	// Time-leading ordering limits the only remaining event mutation to the
	// boundary month. Compact receipts are in partition zero and are untouched.
	return s.c4bDelete(ctx, "analytics_events", "requested_at_ns>=? AND requested_at_ns<? AND is_deleted=0", monthStart.UnixNano(), boundary)
}

// retainedBucketBounds is the shared canonical source for retention and late plans.
func retainedBucketBounds(at time.Time, zone string, daily bool) (time.Time, time.Time, error) {
	dayStart, dayEnd, err := aggregate.BucketBounds(at, zone, "1d")
	if err != nil || daily {
		return dayStart, dayEnd, err
	}
	start, end, err := aggregate.BucketBounds(at, zone, "1h")
	if err != nil {
		return start, end, err
	}
	if start.Before(dayStart) {
		start = dayStart
	}
	if end.After(dayEnd) {
		end = dayEnd
	}
	return start, end, nil
}
