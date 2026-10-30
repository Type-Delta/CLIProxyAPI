package chstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	ch "github.com/ClickHouse/clickhouse-go/v2"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
)

// c4bLateArguments routes finalized, already priced arguments rather than
// repricing imported money or running payload validation a second time.
func (s *Store) c4bLateArguments(arguments [][]any) bool {
	if s.retentionCutoff.IsZero() {
		return false
	}
	for _, args := range arguments {
		if args[5].(int64) < s.retentionCutoff.UnixNano() {
			return true
		}
	}
	return false
}

// c4bInsertLate keeps pending rows below the raw floor until complete retained
// snapshots are staged. Both imported and live late rows are promoted, matching
// SQLite's next retention pass. The caller holds the writer lock.
func (s *Store) c4bInsertLate(ctx context.Context, arguments [][]any, estimated int64) (int64, error) {
	hourly, err := s.c4bBoundary(ctx, true)
	if err != nil {
		return 0, err
	}
	plans := map[string]c4bBucket{}
	generation := s.nextVersion()
	staged := make([][]any, 0, len(arguments))
	for _, args := range arguments {
		var retainedGeneration uint64
		var grain string
		var startNS int64
		if args[5].(int64) < s.retentionCutoff.UnixNano() {
			at := time.Unix(0, args[5].(int64)).UTC()
			_, dayEnd, errBounds := aggregate.BucketBounds(at, s.config.RetentionTimeZone, "1d")
			if errBounds != nil {
				return 0, errBounds
			}
			grain = "hourly"
			daily := hourly != 0 && dayEnd.UnixNano() <= hourly
			if daily {
				grain = "daily"
			}
			start, end, errBounds := retainedBucketBounds(at, s.config.RetentionTimeZone, daily)
			if errBounds != nil {
				return 0, errBounds
			}
			// Native retention may have clipped an overlapping DST interval.
			// Reuse its published bounds for every import owner.
			var publishedStart, publishedEnd int64
			err = s.db.Conn().QueryRow(ctx, "SELECT bucket_start_ns,bucket_end_ns FROM "+s.table("analytics_rollups")+" FINAL WHERE grain=? AND is_deleted=0 AND bucket_start_ns=? AND "+s.c4bPublishedPredicate()+" ORDER BY bucket_start_ns LIMIT 1", grain, start.UnixNano()).Scan(&publishedStart, &publishedEnd)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, err
			}
			if err == nil {
				start, end = time.Unix(0, publishedStart).UTC(), time.Unix(0, publishedEnd).UTC()
			}
			bucket := c4bBucket{Grain: grain, Start: start.UnixNano(), End: end.UnixNano()}

			if end.After(s.retentionCutoff) {
				return 0, fmt.Errorf("late event bucket crosses published raw boundary: %w", ErrRetainedRangePartial)
			}
			retainedGeneration = generation
			startNS = start.UnixNano()
			// eventArguments uses the stable eventColumns order.
			for i, column := range strings.Split(eventColumns, ",") {
				if strings.TrimSpace(column) == "import_batch_id" && args[i] != nil {
					bucket.Batch = args[i].(string)
				}
			}
			plans[fmt.Sprintf("%s/%d/%s", grain, startNS, bucket.Batch)] = bucket
		}
		staged = append(staged, append(args, retainedGeneration, grain, startNS))
	}
	controls, err := s.loadControl(ctx, "late_retention")
	if err != nil {
		return 0, err
	}
	planned := make([]c4bBucket, 0, len(plans))
	for _, bucket := range plans {
		planned = append(planned, bucket)
	}
	controls[strconv.FormatUint(generation, 10)], err = json.Marshal(planned)
	if err != nil {
		return 0, err
	}
	if err = s.replaceControl(ctx, "late_retention", controls); err != nil {
		return 0, err
	}
	batch, err := s.db.Conn().PrepareBatch(ctx, "INSERT INTO "+s.table("analytics_events")+" ("+eventColumns+",event_json,version,is_deleted,retained_generation,retained_grain,retained_bucket_start_ns)")
	if err != nil {
		return 0, err
	}
	defer func() { _ = batch.Abort() }()
	for _, row := range staged {
		if err = batch.Append(row...); err != nil {
			return 0, err
		}
	}
	if err = batch.Send(); err != nil {
		return 0, fmt.Errorf("stage late analytics events: %w", err)
	}
	s.reserveBudget(estimated)
	if err = c4bStep(ctx, "late:source"); err != nil {
		return 0, err
	}
	if err = s.c4bRecoverLate(ctx); err != nil {
		return 0, err
	}
	return int64(len(arguments)), nil
}

// c4bRecoverLate resumes durable pending rows before another write or retention
// cleanup. Publication is determined by heads, not by whether a source exists.
// After publication only compact tombstone receipts remain, preserving replay
// deduplication without retaining diagnostics or token payloads indefinitely.
func (s *Store) c4bRecoverLate(ctx context.Context) error {
	controls, err := s.loadControl(ctx, "late_retention")
	if err != nil {
		return err
	}
	statement := "SELECT DISTINCT retained_generation FROM " + s.table("analytics_events") + " FINAL PREWHERE _partition_id!='0'"
	var args []any
	if len(controls) == 0 {
		// Pending sources are staged strictly below the published raw floor.
		// Keep detecting old sources without plans, while pruning healthy raw
		// history by its leading sort key. Partition zero contains only deleted compact receipts.
		var floor int64
		if !s.retentionCutoff.IsZero() {
			floor = s.retentionCutoff.UnixNano()
		}
		statement += " AND requested_at_ns<?"
		args = append(args, floor)
	}
	statement += " WHERE is_deleted=0 AND retained_generation>0 ORDER BY retained_generation"
	rows, err := s.db.Conn().Query(ctx, statement, args...)
	if err != nil {
		return err
	}
	var generations []uint64
	for rows.Next() {
		var generation uint64
		if err = rows.Scan(&generation); err != nil {
			_ = rows.Close()
			return err
		}
		generations = append(generations, generation)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	pending := make(map[string]bool, len(generations))
	for _, generation := range generations {
		pending[strconv.FormatUint(generation, 10)] = true
	}
	reconciled := false
	for generation := range controls {
		if !pending[generation] {
			delete(controls, generation)
			reconciled = true
		}
	}
	// The caller holds the mutation lock, so no writer can be between publishing
	// a plan and inserting its pending sources while this reconciliation runs.
	if reconciled {
		if err = s.replaceControlMode(ctx, "late_retention", controls, false); err != nil {
			return err
		}
	}
	for _, generation := range generations {
		rows, err = s.db.Conn().Query(ctx, "SELECT DISTINCT retained_grain,retained_bucket_start_ns,ifNull(import_batch_id,'') FROM "+s.table("analytics_events")+" FINAL WHERE is_deleted=0 AND retained_generation=?", generation)
		if err != nil {
			return err
		}
		buckets := []c4bBucket{}
		for rows.Next() {
			var bucket c4bBucket
			if err = rows.Scan(&bucket.Grain, &bucket.Start, &bucket.Batch); err != nil {
				_ = rows.Close()
				return err
			}
			buckets = append(buckets, bucket)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		var planned []c4bBucket
		rawPlan := controls[strconv.FormatUint(generation, 10)]
		if len(rawPlan) == 0 {
			return fmt.Errorf("missing late bucket plan: pre-release databases must be recreated")
		}
		if err = json.Unmarshal(rawPlan, &planned); err != nil {
			return fmt.Errorf("invalid late bucket plan; pre-release databases must be recreated: %w", err)
		}
		publish := []c4bBucket{}
		for _, bucket := range buckets {
			var head uint64
			err = s.db.Conn().QueryRow(ctx, "SELECT generation FROM "+s.table("analytics_retention_heads")+" FINAL WHERE grain=? AND bucket_start_ns=? AND import_batch_id=?", bucket.Grain, bucket.Start, bucket.Batch).Scan(&head)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if head >= generation {
				continue
			}
			found := false
			for _, plan := range planned {
				if plan.Grain == bucket.Grain && plan.Start == bucket.Start && plan.Batch == bucket.Batch {
					bucket.End = plan.End
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("missing persisted late bucket bounds for %s/%d: pre-release databases must be recreated", bucket.Grain, bucket.Start)
			}

			if err = s.c4bStageLateBucket(ctx, bucket, generation); err != nil {
				var exception *ch.Exception
				if errors.As(err, &exception) && exception.Code == 70 {
					// These rows were staged but never acknowledged or published.
					// Roll them back as a unit; keep every committed baseline intact.
					return errors.Join(err, s.c4bRejectLateGeneration(context.WithoutCancel(ctx), generation, controls))
				}
				return err
			}
			publish = append(publish, bucket)
		}
		if err = c4bStep(ctx, "late:staged"); err != nil {
			return err
		}
		if err = s.c4bPublishHeads(ctx, publish, generation); err != nil {
			return err
		}
		if err = c4bStep(ctx, "late:published"); err != nil {
			return err
		}
		// Generation is part of the sorting key, so merges cannot reclaim an
		// old snapshot. Once the head is durable these rows are unreachable.
		for _, bucket := range buckets {
			for _, table := range []string{"analytics_rollups", "analytics_request_rollups"} {
				if err = s.c4bDelete(ctx, table, "grain=? AND bucket_start_ns=? AND import_batch_id=? AND generation<?", bucket.Grain, bucket.Start, bucket.Batch, generation); err != nil {
					return err
				}
			}
		}
		if err = s.c4bLateReceipts(ctx, generation); err != nil {
			return err
		}
		delete(controls, strconv.FormatUint(generation, 10))
		if err = s.replaceControl(ctx, "late_retention", controls); err != nil {
			return err
		}
		if err = c4bStep(ctx, "late:receipts"); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) c4bStageLateBucket(ctx context.Context, bucket c4bBucket, generation uint64) error {
	// A failed stage is unreachable. Replace it in full; never add a chunk to its
	// own previous result. Existing published native/import snapshots are inputs.
	for _, table := range []string{"analytics_rollups", "analytics_request_rollups"} {
		if err := s.c4bDelete(ctx, table, "grain=? AND bucket_start_ns=? AND import_batch_id=? AND generation=?", bucket.Grain, bucket.Start, bucket.Batch, generation); err != nil {
			return err
		}
	}
	source := "is_deleted=0 AND retained_generation=? AND retained_grain=? AND retained_bucket_start_ns=? AND ifNull(import_batch_id,'')=?"
	sourceArgs := []any{generation, bucket.Grain, bucket.Start, bucket.Batch}
	baseline := "grain=? AND bucket_start_ns=? AND import_batch_id=? AND is_deleted=0 AND " + s.c4bPublishedPredicate()
	baselineArgs := []any{bucket.Grain, bucket.Start, bucket.Batch}
	delta := "SELECT ?,?,?,min(requested_at_ns),max(requested_at_ns)," + c4bRawDimensions + ",toInt64(uniqExact(proxy_request_id)),toInt64(count())," + c4bSums + " FROM " + s.table("analytics_events") + " FINAL WHERE " + source + " GROUP BY " + c4bRawDimensions
	inputs := "SELECT " + c4bRollupColumns + " FROM " + s.table("analytics_rollups") + " FINAL WHERE " + baseline + " UNION ALL " + delta
	query := "INSERT INTO " + s.table("analytics_rollups") + " (" + c4bRollupColumns + ",version,is_deleted,generation) SELECT grain,bucket_start_ns,bucket_end_ns,min(first_activity_ns),max(last_activity_ns)," + c4bDimensions + ",sum(proxy_requests),sum(upstream_attempts)," + c4bSums + ",?,toUInt8(0),? FROM (" + inputs + ") GROUP BY grain,bucket_start_ns,bucket_end_ns," + c4bDimensions
	args := []any{s.nextVersion(), generation}
	args = append(args, baselineArgs...)
	args = append(args, bucket.Grain, bucket.Start, bucket.End)
	args = append(args, sourceArgs...)
	if err := s.db.Conn().Exec(ctx, query, args...); err != nil {
		return fmt.Errorf("stage additive late totals: %w", err)
	}
	if err := c4bStep(ctx, "late:totals"); err != nil {
		return err
	}
	inputs = "SELECT " + c4bRequestColumns + " FROM " + s.table("analytics_request_rollups") + " FINAL WHERE " + baseline + " UNION ALL SELECT ?,?,?,proxy_request_id," + c4bRawDimensions + " FROM " + s.table("analytics_events") + " FINAL WHERE " + source
	query = "INSERT INTO " + s.table("analytics_request_rollups") + " (" + c4bRequestColumns + ",version,is_deleted,generation) SELECT DISTINCT *,?,toUInt8(0),? FROM (" + inputs + ")"
	args = []any{s.nextVersion(), generation}
	args = append(args, baselineArgs...)
	args = append(args, bucket.Grain, bucket.Start, bucket.End)
	args = append(args, sourceArgs...)
	// Parameters are bound in SELECT-before-FROM order, including the outer
	// version/generation before the baseline and delta subquery parameters.
	if err := s.db.Conn().Exec(ctx, query, args...); err != nil {
		return fmt.Errorf("stage additive late request identities: %w", err)
	}
	return c4bStep(ctx, "late:requests")
}

func (s *Store) c4bLateReceipts(ctx context.Context, generation uint64) error {
	query := "INSERT INTO " + s.table("analytics_events") + " (attempt_id,key_id,import_batch_id,retained_generation,retained_grain,retained_bucket_start_ns,version,is_deleted) SELECT attempt_id,key_id,import_batch_id,retained_generation,retained_grain,retained_bucket_start_ns,?,toUInt8(1) FROM " + s.table("analytics_events") + " FINAL WHERE retained_generation=? AND is_deleted=0"
	if err := s.db.Conn().Exec(ctx, query, s.nextVersion(), generation); err != nil {
		return fmt.Errorf("compact late-write replay receipts for generation %s: %w", strconv.FormatUint(generation, 10), err)
	}
	return s.c4bDelete(ctx, "analytics_events", "retained_generation=? AND is_deleted=0", generation)
}

func (s *Store) c4bRejectLateGeneration(ctx context.Context, generation uint64, controls map[string]json.RawMessage) error {
	for _, table := range []string{"analytics_rollups", "analytics_request_rollups"} {
		if err := s.c4bDelete(ctx, table, "generation=?", generation); err != nil {
			return err
		}
	}
	if err := s.c4bDelete(ctx, "analytics_events", "retained_generation=?", generation); err != nil {
		return err
	}
	if err := c4bStep(ctx, "late:rejected:source-deleted"); err != nil {
		return err
	}
	delete(controls, strconv.FormatUint(generation, 10))
	return s.replaceControlMode(ctx, "late_retention", controls, false)
}
