package chstore

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const c4bRollupColumns = `grain,bucket_start_ns,bucket_end_ns,first_activity_ns,last_activity_ns,key_id,provider,model,credential_id,endpoint_class,auth_type,service_tier,succeeded,error_class,status_code,token_quality,latency_bucket,cache_class,import_batch_id,proxy_requests,upstream_attempts,input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,cache_creation_tokens,total_tokens,known_cost_nano,unpriced_tokens`
const c4bRequestColumns = `grain,bucket_start_ns,bucket_end_ns,proxy_request_id,key_id,provider,model,credential_id,endpoint_class,auth_type,service_tier,succeeded,error_class,status_code,token_quality,latency_bucket,cache_class,import_batch_id`
const c4bDimensions = `key_id,provider,model,credential_id,endpoint_class,auth_type,service_tier,succeeded,error_class,status_code,token_quality,latency_bucket,cache_class,import_batch_id`
const c4bRawDimensions = `key_id,provider,model,ifNull(credential_id,''),endpoint_class,ifNull(auth_type,''),coalesce(service_tier_used,service_tier_requested,''),toInt64(succeeded),ifNull(error_class,''),ifNull(upstream_status_code,0),token_quality,multiIf(latency_ms<100,'<100ms',latency_ms<500,'100-499ms',latency_ms<1000,'500-999ms','1000ms+'),if(cache_read_tokens>0,'cached','uncached'),ifNull(import_batch_id,'')`
const c4bSums = `sum(input_tokens),sum(output_tokens),sum(reasoning_tokens),sum(cached_tokens),sum(cache_read_tokens),sum(cache_creation_tokens),sum(total_tokens),accurateCast(sum(toInt128(ifNull(known_cost_nano,0))),'Int64'),sum(unpriced_tokens)`

// c4bStep permits deterministic interruption at durable boundaries in integration tests.
// Production callers do not attach this private context key.
type c4bStepKey struct{}

func c4bStep(ctx context.Context, name string) error {
	if hook, ok := ctx.Value(c4bStepKey{}).(func(string) error); ok {
		if err := hook(name); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (s *Store) ApplyRetention(ctx context.Context, cutoff time.Time, batchSize int) (RetentionResult, error) {
	return s.ApplyRetentionPolicy(ctx, cutoff, time.Time{}, batchSize)
}

// ApplyRetentionPolicy publishes complete buckets before deleting their sources.
// batchSize is validated for API compatibility; an atomic publication unit is a
// whole bucket, regardless of how many source rows that bucket contains.
func (s *Store) ApplyRetentionPolicy(ctx context.Context, rawCutoff, hourlyCutoff time.Time, batchSize int) (RetentionResult, error) {
	result := RetentionResult{Cutoff: rawCutoff, HourlyCutoff: hourlyCutoff}
	if rawCutoff.IsZero() || rawCutoff.Location() != time.UTC || !hourlyCutoff.IsZero() && hourlyCutoff.Location() != time.UTC || batchSize < 1 || batchSize > 10000 {
		return result, fmt.Errorf("invalid retention cutoff or batch size")
	}
	if !hourlyCutoff.IsZero() && hourlyCutoff.After(rawCutoff) {
		return result, fmt.Errorf("hourly retention cutoff must not follow raw cutoff")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return result, ErrClosed
	}
	if err := s.requireLegacyImportDone(ctx); err != nil {
		return result, err
	}
	if err := s.c4bRecoverLate(ctx); err != nil {
		return result, err
	}
	for _, daily := range []bool{false, true} {
		cutoff := rawCutoff
		if daily {
			cutoff = hourlyCutoff
			if cutoff.IsZero() {
				break
			}
		}
		if err := s.c4bPromote(ctx, cutoff, daily, &result); err != nil {
			return result, err
		}
	}
	// All eligible buckets are published before any physical source cleanup.
	rawBoundary, err := s.c4bBoundary(ctx, false)
	if err != nil {
		return result, err
	}
	if err = s.c4bCleanup(ctx, rawBoundary, false); err != nil {
		return result, err
	}
	if err = c4bStep(ctx, "raw:deleted"); err != nil {
		return result, err
	}
	hourlyBoundary, err := s.c4bBoundary(ctx, true)
	if err != nil {
		return result, err
	}
	if !hourlyCutoff.IsZero() {
		if err = s.c4bCleanup(ctx, hourlyBoundary, true); err != nil {
			return result, err
		}
		if err = c4bStep(ctx, "hourly:deleted"); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *Store) c4bBoundary(ctx context.Context, daily bool) (int64, error) {
	key := "retention_raw_cutoff_ns"
	if daily {
		key = "retention_hourly_cutoff_ns"
	}
	raw, found, err := s.metadata(ctx, key)
	if err != nil || !found {
		return 0, err
	}
	return strconv.ParseInt(raw, 10, 64)
}

func (s *Store) c4bDelete(ctx context.Context, table, where string, args ...any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// ClickHouse keeps submitted mutations after client cancellation. Wait for
	// completion before releasing the writer lock or issuing dependent work.
	return s.db.Conn().Exec(context.WithoutCancel(ctx), "ALTER TABLE "+s.table(table)+" DELETE WHERE "+where+" SETTINGS mutations_sync=2", args...)
}

func (s *Store) c4bPublishBoundary(ctx context.Context, boundary int64, daily bool, checkpoint *int64) error {
	key := "retention_raw_cutoff_ns"
	if daily {
		key = "retention_hourly_cutoff_ns"
	}
	values := map[string]string{key: strconv.FormatInt(boundary, 10)}
	if checkpoint != nil {
		values[strings.Replace(key, "cutoff", "checkpoint", 1)] = strconv.FormatInt(*checkpoint, 10)
	}
	if err := s.setMetadataBatch(ctx, values); err != nil {
		return err
	}
	if !daily {
		floor := time.Unix(0, boundary).UTC()
		if floor.After(s.retentionCutoff) {
			s.retentionCutoff = floor
		}
	}
	return nil
}

// RetentionCutoff reports the published raw history boundary.
func (s *Store) RetentionCutoff() *time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.retentionCutoff.IsZero() {
		return nil
	}
	value := s.retentionCutoff
	return &value
}
