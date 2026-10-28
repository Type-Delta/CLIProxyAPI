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
	if s.db == nil {
		return model.ModelLeaderboard{}, ErrClosed
	}
	if !s.retentionCutoff.IsZero() && query.Start.Before(s.retentionCutoff) {
		return model.ModelLeaderboard{}, RetainedRangeError{Cutoff: s.retentionCutoff}
	}
	raw, err := s.rawSelection(ctx, query, "model", "provider", "succeeded", "session_digest", "output_tokens", "reasoning_tokens", "accounting_schema", "output_timing", "outcome")
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
 awards AS (
 SELECT 'most_used' AS metric,model,toFloat64(sessions) AS value,sessions AS tie,providers,CAST(NULL AS Nullable(Int64)) AS attempts,CAST(NULL AS Nullable(Int64)) AS failed,CAST(NULL AS Nullable(Int64)) AS samples,CAST(NULL AS Nullable(Float64)) AS share FROM stats WHERE sessions>0
 UNION ALL SELECT 'most_reliable',model,toFloat64(attempts-failed)/attempts,attempts,NULL,attempts,failed,NULL,NULL FROM stats WHERE attempts>=200
 UNION ALL SELECT 'speed',model,arrayElement(speeds,toUInt64(ceil(length(speeds)*0.70))),toInt64(length(speeds)),NULL,attempts-failed,NULL,toInt64(length(speeds)),NULL FROM stats WHERE length(speeds)>0 AND arrayElement(speeds,toUInt64(ceil(length(speeds)*0.70)))>=80
 UNION ALL SELECT 'most_talkative',model,toFloat64(arrayElement(talkative,toUInt64(ceil(length(talkative)*0.95)))),toInt64(length(talkative)),NULL,NULL,NULL,toInt64(length(talkative)),least(1.,toFloat64(reasoning)/generated_total) FROM stats WHERE length(talkative)>0
 ) SELECT metric,model,value,providers,attempts,failed,samples,share FROM awards ORDER BY metric,value DESC,tie DESC,model LIMIT 6 BY metric`
	rows, err := s.db.Conn().Query(ctx, statement, raw.Args...)
	if err != nil {
		return model.ModelLeaderboard{}, fmt.Errorf("query model leaderboard: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := model.ModelLeaderboard{Start: query.Start, End: query.End, Awards: []model.ModelLeaderboardAward{}}
	indices := map[string]int{}
	for _, metric := range []string{"most_used", "most_reliable", "speed", "most_talkative"} {
		indices[metric] = len(result.Awards)
		result.Awards = append(result.Awards, model.ModelLeaderboardAward{Metric: metric, Rows: []model.ModelLeaderboardRow{}})
	}
	for rows.Next() {
		var metric string
		var row model.ModelLeaderboardRow
		if err := rows.Scan(&metric, &row.Model, &row.Value, &row.Providers, &row.Attempts, &row.Failed, &row.Samples, &row.ReasoningShare); err != nil {
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
