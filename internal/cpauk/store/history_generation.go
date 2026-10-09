package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"
)

// History generations are sparse rows keyed by the same 15-minute boundary
// used by the compact aggregate. A row is present only after a mutation has
// touched its bucket. The generation value is durable, so a process restart
// cannot resurrect a cached result for an older version of the history.
const historyGenerationBucketNS = materialized15mDurationNS

// A wide maintenance operation may cover too many fine-grained buckets to
// enumerate safely. Advancing this reserved row invalidates all cached ranges.
const historyGenerationGlobalBucket int64 = -1 << 63

const createHistoryGenerationsSQL = `CREATE TABLE IF NOT EXISTS history_generations (
    bucket_start_ns INTEGER PRIMARY KEY,
    generation INTEGER NOT NULL CHECK(generation > 0)
);
CREATE INDEX IF NOT EXISTS history_generations_range_idx
    ON history_generations(bucket_start_ns);`

func ensureHistoryGenerations(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, createHistoryGenerationsSQL); err != nil {
		return fmt.Errorf("create history generations: %w", err)
	}
	return nil
}

func historyGenerationBucketStart(requestedNS int64) int64 {
	bucket := requestedNS / historyGenerationBucketNS
	if requestedNS < 0 && requestedNS%historyGenerationBucketNS != 0 {
		bucket--
	}
	return bucket * historyGenerationBucketNS
}

func historyGenerationBucketsForRange(startNS, endNS int64) []int64 {
	if startNS >= endNS {
		return nil
	}
	first := historyGenerationBucketStart(startNS)
	last := historyGenerationBucketStart(endNS - 1)
	count := (last-first)/historyGenerationBucketNS + 1
	if count <= 0 {
		return nil
	}
	// Avoid an accidental unbounded allocation for long-lived retained history.
	// A global generation is conservative but cannot leave stale middle ranges.
	if count > 1<<20 {
		return []int64{historyGenerationGlobalBucket}
	}
	buckets := make([]int64, 0, count)
	for bucket := first; bucket <= last; bucket += historyGenerationBucketNS {
		buckets = append(buckets, bucket)
	}
	return buckets
}

func touchHistoryGenerationsTx(ctx context.Context, tx *sql.Tx, buckets []int64) error {
	if len(buckets) == 0 {
		return nil
	}
	ordered := append([]int64(nil), buckets...)
	slices.Sort(ordered)
	statement, err := tx.PrepareContext(ctx, `INSERT INTO history_generations(bucket_start_ns,generation)
VALUES (?,1) ON CONFLICT(bucket_start_ns) DO UPDATE SET generation=history_generations.generation+1`)
	if err != nil {
		return fmt.Errorf("prepare history generation update: %w", err)
	}
	defer func() { _ = statement.Close() }()
	var previous int64
	havePrevious := false
	for _, bucket := range ordered {
		if havePrevious && bucket == previous {
			continue
		}
		if _, err := statement.ExecContext(ctx, bucket); err != nil {
			return fmt.Errorf("advance history generation for bucket %d: %w", bucket, err)
		}
		previous, havePrevious = bucket, true
	}
	return nil
}

func touchHistoryGenerationRangeTx(ctx context.Context, tx *sql.Tx, startNS, endNS int64) error {
	return touchHistoryGenerationsTx(ctx, tx, historyGenerationBucketsForRange(startNS, endNS))
}

// historyMutationBucketsTx collects every persisted representation that a
// destructive operation may remove. Retained history no longer has an events
// row, so looking at events alone would leave a stale cache after purging an
// already rolled-up key or import batch.
func historyMutationBucketsTx(ctx context.Context, tx *sql.Tx, column, value string) ([]int64, error) {
	if column != "key_id" && column != "import_batch_id" {
		return nil, fmt.Errorf("unsupported history mutation column %q", column)
	}
	query := `SELECT requested_at_ns,requested_at_ns+1 FROM events WHERE ` + column + `=?
UNION ALL SELECT bucket_start_ns,bucket_end_ns FROM rollups WHERE ` + column + `=?`
	rows, err := tx.QueryContext(ctx, query, value, value)
	if err != nil {
		return nil, fmt.Errorf("read history mutation ranges: %w", err)
	}
	defer func() { _ = rows.Close() }()
	seen := make(map[int64]struct{})
	for rows.Next() {
		var startNS, endNS int64
		if err := rows.Scan(&startNS, &endNS); err != nil {
			return nil, fmt.Errorf("scan history mutation range: %w", err)
		}
		for _, bucket := range historyGenerationBucketsForRange(startNS, endNS) {
			seen[bucket] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read history mutation ranges: %w", err)
	}
	buckets := make([]int64, 0, len(seen))
	for bucket := range seen {
		buckets = append(buckets, bucket)
	}
	return buckets, nil
}

// historyGenerationLocked returns a canonical digest for [startNS,endNS).
// The caller holds s.mu.RLock or s.mu.Lock. Fixed-width encoding avoids any
// ambiguity between adjacent integer values and keeps the digest independent
// of SQLite's textual formatting.
func (s *SQLiteStore) historyGenerationLocked(ctx context.Context, startNS, endNS int64) (string, error) {
	hash := sha256.New()
	_, _ = hash.Write([]byte("cpauk-history-generation-v1"))
	var encoded [16]byte
	binary.BigEndian.PutUint64(encoded[:8], uint64(startNS))
	binary.BigEndian.PutUint64(encoded[8:], uint64(endNS))
	_, _ = hash.Write(encoded[:])
	// Retention changes the valid query domain even when a retention pass has
	// no rows left to delete. Bind the in-memory cutoff so a cached empty or
	// historical result cannot bypass a newly established retained-range error.
	cutoffNS := int64(0)
	if !s.retentionCutoff.IsZero() {
		cutoffNS = s.retentionCutoff.UnixNano()
	}
	binary.BigEndian.PutUint64(encoded[:8], uint64(cutoffNS))
	binary.BigEndian.PutUint64(encoded[8:], 0)
	_, _ = hash.Write(encoded[:])
	rows, err := s.db.QueryContext(ctx, `SELECT bucket_start_ns,generation
FROM history_generations WHERE bucket_start_ns = ? OR
(bucket_start_ns < ? AND bucket_start_ns + ? > ?)
	ORDER BY bucket_start_ns`, historyGenerationGlobalBucket, endNS, historyGenerationBucketNS, startNS)
	if err != nil {
		return "", fmt.Errorf("read history generations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var bucket, generation int64
		if err := rows.Scan(&bucket, &generation); err != nil {
			return "", fmt.Errorf("scan history generation: %w", err)
		}
		binary.BigEndian.PutUint64(encoded[:8], uint64(bucket))
		binary.BigEndian.PutUint64(encoded[8:], uint64(generation))
		_, _ = hash.Write(encoded[:])
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read history generations: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
