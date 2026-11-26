package chstore

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// ModelLeaderboard ranks raw attempts using the SQLite award thresholds and
// nearest-rank percentiles. Only the six winning rows per award leave ClickHouse.
func (s *Store) ModelLeaderboard(ctx context.Context, query model.Query) (model.ModelLeaderboard, error) {
	if err := s.validateQuery(&query, model.OperationModelLeaderboard); err != nil {
		return model.ModelLeaderboard{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ctx = withReadCutoffs(ctx)
	if s.db == nil {
		return model.ModelLeaderboard{}, ErrClosed
	}
	if !s.retentionCutoff.IsZero() && query.Start.Before(s.retentionCutoff) {
		return model.ModelLeaderboard{}, RetainedRangeError{Cutoff: s.retentionCutoff}
	}
	raw, err := s.rawSelection(ctx, query, "model", "provider", "succeeded", "session_digest", "key_id", "latency_ms", "output_tokens", "reasoning_tokens", "accounting_schema", "output_timing", "outcome")
	if err != nil {
		return model.ModelLeaderboard{}, err
	}
	statement := `WITH evidence AS (SELECT *, ` + c2r2SpeedSQL() + ` AS speed,
 if(accounting_schema='normalized-v1',output_tokens,output_tokens+reasoning_tokens) AS generated,
 succeeded=1 AND coalesce(outcome,'')!='canceled' AS eligible FROM (` + raw.SQL + `) WHERE model!=''),
 stats AS (SELECT model, toInt64(count()) AS attempts, toInt64(countIf(succeeded=0)) AS failed,
 toInt64(uniqExact(provider)) AS providers, toInt64(uniqExactIf(session_digest,coalesce(session_digest,'')!='')) AS sessions,
 arraySort(groupArrayIf(assumeNotNull(speed),eligible AND speed IS NOT NULL)) AS speeds,
 arraySort(groupArrayIf(generated,eligible AND accounting_schema!='raw-fallback-v1' AND generated>0)) AS talkative,
 sumIf(reasoning_tokens,eligible AND accounting_schema!='raw-fallback-v1' AND generated>0) AS reasoning,
 sumIf(generated,eligible AND accounting_schema!='raw-fallback-v1' AND generated>0) AS generated_total FROM evidence GROUP BY model),
 session_keys AS (
 SELECT model,assumeNotNull(session_digest) AS session_digest,key_id,sum(latency_ms) AS key_latency
 FROM evidence WHERE session_digest IS NOT NULL AND session_digest!=''
 GROUP BY model,session_digest,key_id),
 session_totals AS (
 SELECT model,assumeNotNull(session_digest) AS session_digest,sum(latency_ms) AS latency,toInt64(count()) AS samples
 FROM evidence WHERE session_digest IS NOT NULL AND session_digest!=''
 GROUP BY model,session_digest),
 marathon AS (
 SELECT totals.model,
 tupleElement(arrayElement(arraySort(x -> tuple(-tupleElement(x,1),tupleElement(x,2)),groupArray((totals.latency,totals.session_digest,totals.samples,keys.key_id))),1),1) AS latency,
 tupleElement(arrayElement(arraySort(x -> tuple(-tupleElement(x,1),tupleElement(x,2)),groupArray((totals.latency,totals.session_digest,totals.samples,keys.key_id))),1),3) AS samples,
 tupleElement(arrayElement(arraySort(x -> tuple(-tupleElement(x,1),tupleElement(x,2)),groupArray((totals.latency,totals.session_digest,totals.samples,keys.key_id))),1),4) AS key_id,
 toUInt8(1) AS has_marathon
 FROM session_totals AS totals
 INNER JOIN (SELECT model,session_digest,argMin(key_id,tuple(-key_latency,key_id)) AS key_id FROM session_keys GROUP BY model,session_digest) AS keys
 ON totals.model=keys.model AND totals.session_digest=keys.session_digest
 GROUP BY totals.model),
 awards AS (
 SELECT tupleElement(award,1) AS metric,model,tupleElement(award,2) AS value,
 tupleElement(award,3) AS tie,tupleElement(award,4) AS providers,
 tupleElement(award,5) AS attempts,tupleElement(award,6) AS failed,
 tupleElement(award,7) AS samples,tupleElement(award,8) AS share,
 tupleElement(award,9) AS key_id
 FROM (SELECT stats.model,arrayJoin(arrayFilter(x -> tupleElement(x,10),[
 tuple('most_used',toFloat64(sessions),sessions,toNullable(providers),CAST(NULL AS Nullable(Int64)),CAST(NULL AS Nullable(Int64)),CAST(NULL AS Nullable(Int64)),CAST(NULL AS Nullable(Float64)),CAST(NULL AS Nullable(String)),sessions>0),
 tuple('most_reliable',toFloat64(attempts-failed)/attempts,attempts,NULL,toNullable(attempts),toNullable(failed),NULL,NULL,CAST(NULL AS Nullable(String)),attempts>=200),
 tuple('speed',toFloat64(arrayElement(speeds,toUInt64(ceil(length(speeds)*0.70)))),toInt64(length(speeds)),NULL,toNullable(attempts-failed),NULL,toNullable(toInt64(length(speeds))),NULL,CAST(NULL AS Nullable(String)),length(speeds)>0 AND arrayElement(speeds,toUInt64(ceil(length(speeds)*0.70)))>=80),
 tuple('most_talkative',toFloat64(arrayElement(talkative,toUInt64(ceil(length(talkative)*0.95)))),toInt64(length(talkative)),NULL,NULL,NULL,toNullable(toInt64(length(talkative))),toNullable(least(1.,toFloat64(reasoning)/generated_total)),CAST(NULL AS Nullable(String)),length(talkative)>0),
 tuple('marathon',toFloat64(marathon.latency),toInt64(marathon.samples),NULL,NULL,NULL,toNullable(toInt64(marathon.samples)),CAST(NULL AS Nullable(Float64)),toNullable(marathon.key_id),marathon.has_marathon=1)
 ])) AS award FROM stats LEFT JOIN marathon ON stats.model=marathon.model)
 ) SELECT metric,model,value,providers,attempts,failed,samples,share,key_id FROM awards ORDER BY metric,value DESC,tie DESC,model LIMIT 6 BY metric`
	rows, err := s.db.Conn().Query(ctx, statement, raw.Args...)
	if err != nil {
		return model.ModelLeaderboard{}, fmt.Errorf("query model leaderboard: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := model.ModelLeaderboard{Start: query.Start, End: query.End, Awards: []model.ModelLeaderboardAward{}}
	indices := map[string]int{}
	for _, metric := range []string{"most_used", "most_reliable", "speed", "most_talkative", "marathon"} {
		indices[metric] = len(result.Awards)
		result.Awards = append(result.Awards, model.ModelLeaderboardAward{Metric: metric, Rows: []model.ModelLeaderboardRow{}})
	}
	for rows.Next() {
		var metric string
		var row model.ModelLeaderboardRow
		if err := rows.Scan(&metric, &row.Model, &row.Value, &row.Providers, &row.Attempts, &row.Failed, &row.Samples, &row.ReasoningShare, &row.KeyID); err != nil {
			return model.ModelLeaderboard{}, err
		}
		award := &result.Awards[indices[metric]]
		row.Rank = len(award.Rows) + 1
		award.Rows = append(award.Rows, row)
	}
	return result, rows.Err()
}

// c2r2SpeedSQL mirrors OutputTiming.Validate and ArrivalThroughput for observed
// windows. Unavailable timing and legacy generation_time_ms never produce TPS.
func c2r2SpeedSQL() string {
	return `if(
 JSONExtractInt(output_timing,'version')=1
 AND JSONExtractString(output_timing,'protocol') IN ('chat','claude','responses','gemini','kiro')
 AND JSONExtractString(output_timing,'source') IN ('source_reader','websocket_reader')
 AND JSONExtractString(output_timing,'status')='observed'
 AND JSONExtractString(output_timing,'reason')=''
 AND JSONExtractString(output_timing,'token_quality') IN ('exact','estimated')
 AND JSONExtractString(output_timing,'coverage') IN ('visible_deltas','wire_deltas')
 AND JSONExtractInt(output_timing,'first_arrival_ns')>=0
 AND JSONExtractInt(output_timing,'last_arrival_ns')-JSONExtractInt(output_timing,'first_arrival_ns')>=100000000
 AND JSONExtractInt(output_timing,'arrival_count')>=3
 AND JSONExtractInt(output_timing,'window_tokens')>=16
 AND JSONExtractInt(output_timing,'first_batch_bytes')>=0
 AND JSONExtractInt(output_timing,'total_bytes')>=0
 AND JSONExtractInt(output_timing,'largest_batch_bytes')>=0
 AND JSONExtractInt(output_timing,'max_interarrival_ns')>=0
 AND JSONExtractInt(output_timing,'first_batch_bytes')<=JSONExtractInt(output_timing,'largest_batch_bytes')
 AND JSONExtractInt(output_timing,'largest_batch_bytes')<=JSONExtractInt(output_timing,'total_bytes')
 AND JSONExtractInt(output_timing,'max_interarrival_ns')<=JSONExtractInt(output_timing,'last_arrival_ns')-JSONExtractInt(output_timing,'first_arrival_ns'),
 toFloat64(JSONExtractInt(output_timing,'window_tokens'))*1000000000/toFloat64(JSONExtractInt(output_timing,'last_arrival_ns')-JSONExtractInt(output_timing,'first_arrival_ns')),CAST(NULL AS Nullable(Float64)))`
}
