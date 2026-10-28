package chstore

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// timingMetrics computes exact integer order statistics. Cancellation follows
// SQLite's explicit outcome column rather than the event compatibility classifier.
// Callers hold the store read lock.
func (s *Store) timingMetrics(ctx context.Context, q model.Query, percentiles bool) (map[string]model.TimingMetric, error) {
	columns := []string{"latency_ms", "first_token_latency_ms", "provider_latency_ms", "time_to_first_token_ms", "generation_time_ms"}
	names := []string{"e2e", "latency", "provider_latency", "ttft", "generation"}
	sources := []string{"observed", "observed_dispatch_to_first_token", "observed_dispatch_to_response", "observed", "observed_first_to_last_arrival"}
	selection, err := s.rawSelection(ctx, q, append(columns, "outcome")...)
	if err != nil {
		return nil, err
	}
	selects := make([]string, 0, 25)
	type metricValues struct {
		count          int64
		total, maximum *int64
		median, p95    *float64
	}
	values := make([]metricValues, len(columns))
	args := []any{}
	for i, c := range columns {
		selects = append(selects, "toInt64(count("+c+"))", "sumOrNull("+c+")", "maxOrNull("+c+")")
		args = append(args, &values[i].count, &values[i].total, &values[i].maximum)
		if percentiles {
			// The median averages the central rows. P95 uses SQLite's
			// ceil(.95*n)-1 index, which differs from ClickHouse's default quantile.
			sorted := "arraySort(groupArray(" + c + "))"
			selects = append(selects, "if(count("+c+")=0,NULL,arrayReduce('avg',arraySlice("+sorted+",intDiv(count("+c+")-1,2)+1,2-count("+c+")%2)))", "if(count("+c+")=0,NULL,toFloat64(arrayElement("+sorted+",intDiv(95*count("+c+")+99,100))))")
			args = append(args, &values[i].median, &values[i].p95)
		}
	}
	err = s.db.Conn().QueryRow(ctx, "SELECT "+strings.Join(selects, ",")+" FROM ("+selection.SQL+") WHERE outcome IS NULL OR outcome<>'canceled'", selection.Args...).Scan(args...)
	if err != nil {
		return nil, err
	}
	result := make(map[string]model.TimingMetric, len(columns))
	for i, name := range names {
		v := values[i]
		result[name] = model.TimingMetric{Source: sources[i], SampleCount: v.count, TotalMS: v.total, MaxMS: v.maximum, MedianMS: v.median, P95MS: v.p95}
	}
	return result, nil
}
