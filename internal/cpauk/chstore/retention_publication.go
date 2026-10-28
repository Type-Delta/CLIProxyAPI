package chstore

import (
	"context"
	"fmt"
)

// c4bPublishedPredicate is shared by totals and exact request identities. The
// generation-zero fallback keeps pre-migration and legacy imported facts visible.
func (s *Store) c4bPublishedPredicate() string {
	heads := s.table("analytics_retention_heads") + " FINAL"
	return "((grain,bucket_start_ns,import_batch_id,generation) IN (SELECT grain,bucket_start_ns,import_batch_id,generation FROM " + heads + ") OR (generation=0 AND (grain,bucket_start_ns,import_batch_id) NOT IN (SELECT grain,bucket_start_ns,import_batch_id FROM " + heads + ")))"
}

type c4bBucket struct {
	Grain      string
	Start, End int64
	Batch      string
}

// c4bPublishHeads sends all heads in one native block after both fact batches.
// A crash while staging cannot change which snapshot readers select.
func (s *Store) c4bPublishHeads(ctx context.Context, buckets []c4bBucket, generation uint64) error {
	if len(buckets) == 0 {
		return nil
	}
	batch, err := s.db.Conn().PrepareBatch(ctx, "INSERT INTO "+s.table("analytics_retention_heads")+" (grain,bucket_start_ns,import_batch_id,generation,version)")
	if err != nil {
		return err
	}
	defer func() { _ = batch.Abort() }()
	version := s.nextVersion()
	for _, bucket := range buckets {
		if err = batch.Append(bucket.Grain, bucket.Start, bucket.Batch, generation, version); err != nil {
			return err
		}
	}
	if err = batch.Send(); err != nil {
		return fmt.Errorf("publish retention generations: %w", err)
	}
	return nil
}

// c4bTablePublicationFilter limits logical history scans. Physical mutations
// deliberately remove every generation so purged data cannot reappear later.
func (s *Store) c4bTablePublicationFilter(table string) string {
	if table == "analytics_rollups" || table == "analytics_request_rollups" {
		return " AND " + s.c4bPublishedPredicate()
	}
	return ""
}
