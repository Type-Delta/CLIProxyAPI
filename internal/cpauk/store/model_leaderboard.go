package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

const modelLeaderboardReliabilityFloor = 200
const modelLeaderboardSpeedFloor = 80.0

type modelLeaderboardStats struct {
	sessions       map[string]struct{}
	providers      map[string]struct{}
	attempts       int64
	failed         int64
	speeds         []float64
	talkative      []int64
	reasoningTotal int64
}

type modelLeaderboardCandidate struct {
	row model.ModelLeaderboardRow
	tie int64
}

// ModelLeaderboard derives all four awards from one pass over raw events in
// the requested window. Historic events without a session digest still count
// toward reliability, speed, and talkative metrics.
func (s *SQLiteStore) ModelLeaderboard(ctx context.Context, query model.Query) (model.ModelLeaderboard, error) {
	if err := s.validateQuery(&query, model.OperationModelLeaderboard); err != nil {
		return model.ModelLeaderboard{}, err
	}
	where, arguments, err := buildWhere(query)
	if err != nil {
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
	rows, err := s.db.QueryContext(ctx, `SELECT model,provider,succeeded,session_digest,
output_tokens,reasoning_tokens,accounting_schema,output_timing FROM events `+where+` AND model <> ''`, arguments...)
	if err != nil {
		return model.ModelLeaderboard{}, fmt.Errorf("query model leaderboard events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	stats := map[string]*modelLeaderboardStats{}
	for rows.Next() {
		var modelName, provider, accountingSchema string
		var succeeded bool
		var sessionDigest, outputTiming sql.NullString
		var output, reasoning int64
		if err := rows.Scan(&modelName, &provider, &succeeded, &sessionDigest, &output, &reasoning,
			&accountingSchema, &outputTiming); err != nil {
			return model.ModelLeaderboard{}, fmt.Errorf("scan model leaderboard event: %w", err)
		}
		item := stats[modelName]
		if item == nil {
			item = &modelLeaderboardStats{sessions: map[string]struct{}{}, providers: map[string]struct{}{}}
			stats[modelName] = item
		}
		item.attempts++
		if !succeeded {
			item.failed++
		}
		item.providers[provider] = struct{}{}
		if sessionDigest.Valid && sessionDigest.String != "" {
			item.sessions[sessionDigest.String] = struct{}{}
		}
		if succeeded && outputTiming.Valid {
			var timing model.OutputTiming
			if err := json.Unmarshal([]byte(outputTiming.String), &timing); err != nil {
				return model.ModelLeaderboard{}, fmt.Errorf("decode leaderboard timing: %w", err)
			}
			info := (model.Event{OutputTiming: &timing}).ArrivalThroughput()
			if info.TokensPerSecond != nil {
				item.speeds = append(item.speeds, *info.TokensPerSecond)
			}
		}
		if succeeded {
			generated, counted := generatedTokens(accountingSchema, output, reasoning)
			if counted && generated > 0 {
				item.talkative = append(item.talkative, generated)
				item.reasoningTotal += reasoning
			}
		}
	}
	if err := rows.Err(); err != nil {
		return model.ModelLeaderboard{}, fmt.Errorf("read model leaderboard events: %w", err)
	}
	return model.ModelLeaderboard{
		Start: query.Start, End: query.End,
		Awards: []model.ModelLeaderboardAward{
			modelAwardMostUsed(stats),
			modelAwardReliable(stats),
			modelAwardSpeed(stats),
			modelAwardTalkative(stats),
		},
	}, nil
}

func modelAwardMostUsed(stats map[string]*modelLeaderboardStats) model.ModelLeaderboardAward {
	candidates := make([]modelLeaderboardCandidate, 0, len(stats))
	for name, item := range stats {
		if len(item.sessions) == 0 {
			continue
		}
		providers := int64(len(item.providers))
		candidates = append(candidates, modelLeaderboardCandidate{tie: int64(len(item.sessions)), row: model.ModelLeaderboardRow{
			Model: name, Value: float64(len(item.sessions)), Providers: &providers,
		}})
	}
	return finishModelAward("most_used", candidates)
}

func modelAwardReliable(stats map[string]*modelLeaderboardStats) model.ModelLeaderboardAward {
	candidates := make([]modelLeaderboardCandidate, 0, len(stats))
	for name, item := range stats {
		if item.attempts < modelLeaderboardReliabilityFloor {
			continue
		}
		attempts, failed := item.attempts, item.failed
		candidates = append(candidates, modelLeaderboardCandidate{tie: attempts, row: model.ModelLeaderboardRow{
			Model: name, Value: float64(attempts-failed) / float64(attempts), Attempts: &attempts, Failed: &failed,
		}})
	}
	return finishModelAward("most_reliable", candidates)
}

func modelAwardSpeed(stats map[string]*modelLeaderboardStats) model.ModelLeaderboardAward {
	candidates := make([]modelLeaderboardCandidate, 0, len(stats))
	for name, item := range stats {
		if len(item.speeds) == 0 {
			continue
		}
		p70 := percentileFloat(item.speeds, 0.70)
		if p70 < modelLeaderboardSpeedFloor {
			continue
		}
		samples := int64(len(item.speeds))
		successful := item.attempts - item.failed
		candidates = append(candidates, modelLeaderboardCandidate{tie: samples, row: model.ModelLeaderboardRow{
			Model: name, Value: p70, Samples: &samples, Attempts: &successful,
		}})
	}
	return finishModelAward("speed", candidates)
}

func modelAwardTalkative(stats map[string]*modelLeaderboardStats) model.ModelLeaderboardAward {
	candidates := make([]modelLeaderboardCandidate, 0, len(stats))
	for name, item := range stats {
		if len(item.talkative) == 0 {
			continue
		}
		p95 := aggregate.Percentile(item.talkative, 0.95)
		if p95 == nil {
			continue
		}
		samples := int64(len(item.talkative))
		share := 0.0
		if *p95 > 0 {
			total := int64(0)
			for _, value := range item.talkative {
				total += value
			}
			if total > 0 {
				share = float64(item.reasoningTotal) / float64(total)
				if share > 1 {
					share = 1
				}
			}
		}
		candidates = append(candidates, modelLeaderboardCandidate{tie: samples, row: model.ModelLeaderboardRow{
			Model: name, Value: float64(*p95), Samples: &samples, ReasoningShare: &share,
		}})
	}
	return finishModelAward("most_talkative", candidates)
}

func finishModelAward(metric string, candidates []modelLeaderboardCandidate) model.ModelLeaderboardAward {
	sort.Slice(candidates, func(left, right int) bool {
		if candidates[left].row.Value != candidates[right].row.Value {
			return candidates[left].row.Value > candidates[right].row.Value
		}
		if candidates[left].tie != candidates[right].tie {
			return candidates[left].tie > candidates[right].tie
		}
		return candidates[left].row.Model < candidates[right].row.Model
	})
	if len(candidates) > 6 {
		candidates = candidates[:6]
	}
	rows := make([]model.ModelLeaderboardRow, len(candidates))
	for index := range candidates {
		rows[index] = candidates[index].row
		rows[index].Rank = index + 1
	}
	return model.ModelLeaderboardAward{Metric: metric, Rows: rows}
}

// normalized-v1 stores the canonical inclusive output total. The intake
// adapter replaces raw output with TokenBreakdown.Output.TotalTokens, which
// includes reasoning for every provider accounting style. raw-fallback-v1 means
// that replacement was impossible, so output may or may not include reasoning;
// those attempts are excluded from the talkative award. Older or imported
// schemas retain separate buckets here, so add reasoning only for those.
func generatedTokens(accountingSchema string, output, reasoning int64) (int64, bool) {
	switch accountingSchema {
	case model.AccountingSchemaNormalizedV1:
		return output, true
	case model.AccountingSchemaRawFallbackV1:
		return 0, false
	default:
		return output + reasoning, true
	}
}

func percentileFloat(values []float64, percentile float64) float64 {
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	position := int(math.Ceil(percentile*float64(len(ordered)))) - 1
	if position < 0 {
		position = 0
	}
	if position >= len(ordered) {
		position = len(ordered) - 1
	}
	return ordered[position]
}

func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	result := value.String
	return &result
}

func nullableInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}
