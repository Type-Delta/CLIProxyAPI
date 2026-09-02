package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

func TestV2SummaryRatesOutcomesAveragesAndCoverage(t *testing.T) {
	database, events := openV2FixtureStore(t)
	query := v2Query(model.OperationSummary, events[0].RequestedAt, events[0].RequestedAt.Add(15*time.Minute))

	summary, err := database.Summary(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Succeeded != 2 || summary.Failed != 1 || summary.SuccessRate == nil || *summary.SuccessRate != "66.67" {
		t.Fatalf("summary outcomes = succeeded %d failed %d rate %v", summary.Succeeded, summary.Failed, summary.SuccessRate)
	}
	if summary.RequestsPerMinute != "0.133333333333333333" || summary.TokensPerMinute != "40" || summary.CacheReadRate == nil || *summary.CacheReadRate != "20" {
		t.Fatalf("summary rates = rpm %q tpm %q cache %v", summary.RequestsPerMinute, summary.TokensPerMinute, summary.CacheReadRate)
	}
	if summary.RangeDays != "0.010416666666666667" || summary.AvgRequestsPerDay != "192" || summary.AvgTokensPerDay != "57600" || summary.AvgKnownCostUSDPerDay != "0.0576" {
		t.Fatalf("summary averages = range %q requests %q tokens %q cost %q", summary.RangeDays, summary.AvgRequestsPerDay, summary.AvgTokensPerDay, summary.AvgKnownCostUSDPerDay)
	}
	if !summary.PriceCoverageComplete || summary.UnpricedTokens != 0 {
		t.Fatalf("summary coverage = complete %t unpriced %d", summary.PriceCoverageComplete, summary.UnpricedTokens)
	}
}

func TestV2SummaryKeepsTwoDecimalSuccessRateAndAvoidsRateOverflow(t *testing.T) {
	database, events := openV2FixtureStore(t)
	large := v2Event(strings.Repeat("4", 32), strings.Repeat("c", 32), strings.Repeat("f", 64), events[0].RequestedAt.Add(20*time.Minute), true, nil, 0, 0, 1, 1)
	large.Tokens = model.TokenUsage{Input: 200_000, Total: 200_000, Schema: "normalized-v1", Quality: model.TokenQualityExact}
	if err := database.WriteBatch(context.Background(), []model.Event{large}); err != nil {
		t.Fatal(err)
	}
	query := v2Query(model.OperationSummary, events[0].RequestedAt, events[0].RequestedAt.Add(24*time.Hour))
	summary, err := database.Summary(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if summary.SuccessRate == nil || *summary.SuccessRate != "75.00" {
		t.Fatalf("success rate = %v, want 75.00", summary.SuccessRate)
	}
	if summary.AvgTokensPerDay != "200600" || summary.TokensPerMinute != "139.305555555555555556" {
		t.Fatalf("overflowed summary rates: avg=%q tpm=%q", summary.AvgTokensPerDay, summary.TokensPerMinute)
	}
}

func TestOptionalPercentageAvoidsOverflow(t *testing.T) {
	got := optionalPercentage(500_000_000_000_000_000, 1_000_000_000_000_000_000)
	if got == nil || *got != "50" {
		t.Fatalf("large percentage = %v, want 50", got)
	}
}

func TestV2ActivityReturnsOrderedBucketsAndCanonicalCacheCategories(t *testing.T) {
	database, events := openV2FixtureStore(t)
	query := v2Query(model.OperationActivity, events[0].RequestedAt, events[0].RequestedAt.Add(15*time.Minute))
	query.Window = "day"

	activity, err := database.Activity(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if activity.Grain != "5m" || activity.Zone != "UTC" {
		t.Fatalf("activity grain/zone = %q/%q", activity.Grain, activity.Zone)
	}
	var populated []model.ActivityBucket
	for index, bucket := range activity.Buckets {
		if index > 0 && !activity.Buckets[index-1].Start.Before(bucket.Start) {
			t.Fatalf("activity buckets are not strictly ordered: %+v", activity.Buckets)
		}
		if bucket.Requests != 0 {
			populated = append(populated, bucket)
		}
	}
	if len(populated) != 3 || populated[0].Succeeded != 1 || populated[1].Failed != 1 || populated[2].Succeeded != 1 {
		t.Fatalf("populated activity buckets = %+v", populated)
	}
	if populated[0].CachedTokens != 99 || populated[0].CacheReadTokens != 0 || populated[1].CacheReadTokens != 20 || populated[2].CacheReadTokens != 40 {
		t.Fatalf("activity cache categories = %+v", populated)
	}
}

func TestV2ActivityAndAnalysisMaterializeEmptyBuckets(t *testing.T) {
	database, events := openV2FixtureStore(t)
	query := v2Query(model.OperationActivity, events[0].RequestedAt, events[0].RequestedAt.Add(20*time.Minute))
	query.Window = "day"

	activity, err := database.Activity(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if len(activity.Buckets) != 5 {
		t.Fatalf("activity returned %d buckets, want 5: %+v", len(activity.Buckets), activity.Buckets)
	}
	if empty := activity.Buckets[3]; empty.Requests != 0 || empty.TotalTokens != 0 {
		t.Fatalf("activity gap bucket = %+v, want zero values", empty)
	}

	query.Operation = model.OperationAnalysis
	query.Window = ""
	query.BucketWidth = "5m"
	analysis, err := database.Analysis(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if len(analysis.SeriesByCategory.Buckets) != 5 || len(analysis.ModelByTime.Buckets) != 5 {
		t.Fatalf("analysis bucket counts = category %d model %d, want 5 each", len(analysis.SeriesByCategory.Buckets), len(analysis.ModelByTime.Buckets))
	}
	if empty := analysis.SeriesByCategory.Buckets[3]; empty.Requests != 0 || empty.TotalTokens != 0 {
		t.Fatalf("analysis gap bucket = %+v, want zero values", empty)
	}
}

func TestV2AnalysisLatencyStatisticsCoverRowsBeyondScatterLimit(t *testing.T) {
	ctx := context.Background()
	database, events := openV2FixtureStore(t)
	start := events[0].RequestedAt.Truncate(time.Hour)
	additional := make([]model.Event, 1100)
	for index := range additional {
		latency := int64(10)
		if index >= 1000 {
			latency = 10_000
		}
		additional[index] = v2Event(
			fmt.Sprintf("%032x", index+10), fmt.Sprintf("%032x", index+10_000), strings.Repeat("f", 64),
			start.Add(time.Duration(index+1)*time.Second), true, nil, 0, 0, latency, latency,
		)
	}
	if err := database.WriteBatch(ctx, additional); err != nil {
		t.Fatal(err)
	}
	query := v2Query(model.OperationAnalysis, start, start.Add(time.Hour))
	analysis, err := database.Analysis(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	latency := analysis.Latency
	if latency.SampleCount != 1103 || len(latency.Samples) != analysisLatencySampleLimit || !latency.Sampled {
		t.Fatalf("latency sampling = count %d samples %d sampled %t", latency.SampleCount, len(latency.Samples), latency.Sampled)
	}
	if latency.P95LatencyMS == nil || *latency.P95LatencyMS != 10_000 || latency.MaxLatencyMS == nil || *latency.MaxLatencyMS != 10_000 ||
		latency.P95TTFTMS == nil || *latency.P95TTFTMS != 10_000 || latency.MaxTTFTMS == nil || *latency.MaxTTFTMS != 10_000 {
		t.Fatalf("latency statistics excluded capped rows: %+v", latency)
	}
	if !latency.Samples[len(latency.Samples)-1].RequestedAt.After(start.Add(1000 * time.Second)) {
		t.Fatalf("scatter sample remains earliest-biased; final sample at %v", latency.Samples[len(latency.Samples)-1].RequestedAt)
	}
}

func TestV2AnalysisCostComponentsReconcileOncePerEventRounding(t *testing.T) {
	ctx := context.Background()
	rate := model.NanoUSD(400_000)
	book := aggregate.PriceBook{Rules: []aggregate.PricingRule{{
		ID: "fractional", Model: "model-v2", InputPerMillion: &rate, OutputPerMillion: &rate,
		CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "test",
	}}}
	database, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PriceBook: book})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(context.Background()) })
	start := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	event := v2Event(strings.Repeat("7", 32), strings.Repeat("8", 32), strings.Repeat("9", 64), start.Add(time.Minute), true, nil, 0, 0, 1, 1)
	event.Tokens = model.TokenUsage{Input: 1, Output: 1, Total: 2, Schema: "normalized-v1", Quality: model.TokenQualityExact}
	if err := database.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	query := v2Query(model.OperationAnalysis, start, start.Add(time.Hour))
	analysis, err := database.Analysis(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	components := analysis.CostComponents
	componentTotal, err := sumComponentUSD(components.UncachedInputUSD, components.CacheReadUSD, components.CacheCreationUSD, components.OutputUSD)
	if err != nil {
		t.Fatal(err)
	}
	if componentTotal != 1 || components.UncachedInputUSD != "0.000000001" || components.OutputUSD != "0" {
		t.Fatalf("cost components = %+v total nano-USD %d, want deterministic reconciled total 1", components, componentTotal)
	}
}

func TestReconcileCostComponentsUsesStableCategoryOrder(t *testing.T) {
	tokens := []model.TokenUsage{{Total: 1}, {Total: 1}, {Total: 1}, {Total: 1}}
	components := []model.NanoUSD{1, 1, 1, 1}
	reconcileCostComponents(components, tokens, 2)
	if components[0] != 0 || components[1] != 0 || components[2] != 1 || components[3] != 1 {
		t.Fatalf("negative residual allocation = %v, want [0 0 1 1] nano-USD", components)
	}

	components = []model.NanoUSD{0, 0, 0, 0}
	tokens[0].Total = 0
	reconcileCostComponents(components, tokens, 1)
	if components[0] != 0 || components[1] != 1 || components[2] != 0 || components[3] != 0 {
		t.Fatalf("positive residual allocation = %v, want [0 1 0 0] nano-USD", components)
	}
}

func sumComponentUSD(values ...string) (model.NanoUSD, error) {
	var total model.NanoUSD
	for _, value := range values {
		parsed, err := model.ParseNanoUSD(value)
		if err != nil {
			return 0, err
		}
		total += parsed
	}
	return total, nil
}

func TestV2AnalysisSectionsAreIndependentAndLongLatencyIsUnsupported(t *testing.T) {
	database, events := openV2FixtureStore(t)
	query := v2Query(model.OperationAnalysis, events[0].RequestedAt, events[0].RequestedAt.Add(15*time.Minute))
	query.BucketWidth = "5m"

	analysis, err := database.Analysis(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	assertAllAnalysisSectionsPresent(t, analysis)
	if analysis.SeriesByCategory.Meta.Partial || analysis.ModelByTime.Meta.Partial || analysis.Latency.Meta.Partial || analysis.CostComponents.Meta.Partial || analysis.KeyModelMatrix.Meta.Partial {
		t.Fatalf("complete analysis marked partial: %+v", analysis)
	}
	if len(analysis.SeriesByCategory.Buckets) == 0 || len(analysis.ModelByTime.Models) == 0 || len(analysis.Latency.Samples) != 3 || len(analysis.KeyModelMatrix.Cells) == 0 {
		t.Fatalf("analysis sections lost fixture data: %+v", analysis)
	}
	if analysis.SeriesByCategory.Buckets[0].KnownCost.String() != "0.0002" {
		t.Fatalf("analysis series known cost = %s", analysis.SeriesByCategory.Buckets[0].KnownCost.String())
	}
	if len(analysis.ModelByTime.Models) != 1 || len(analysis.ModelByTime.Buckets) != 4 || analysis.ModelByTime.Models[0].CacheReadTokens != 60 || analysis.ModelByTime.Models[0].KnownCost.String() != "0.0006" {
		t.Fatalf("analysis model by time = %+v", analysis.ModelByTime)
	}
	if analysis.Latency.P95TTFTMS == nil || *analysis.Latency.P95TTFTMS != 30 || analysis.Latency.P95LatencyMS == nil || *analysis.Latency.P95LatencyMS != 300 ||
		analysis.Latency.MaxTTFTMS == nil || *analysis.Latency.MaxTTFTMS != 30 || analysis.Latency.MaxLatencyMS == nil || *analysis.Latency.MaxLatencyMS != 300 ||
		analysis.Latency.Samples[1].Model != "model-v2" || analysis.Latency.Samples[1].Succeeded {
		t.Fatalf("analysis latency = %+v", analysis.Latency)
	}
	if analysis.CostComponents.UncachedInputUSD != "0.00024" || analysis.CostComponents.CacheReadUSD != "0.00006" ||
		analysis.CostComponents.CacheCreationUSD != "0" || analysis.CostComponents.OutputUSD != "0.0003" || analysis.CostComponents.BlendedUSDPerMillion != "1" {
		t.Fatalf("analysis cost components = %+v", analysis.CostComponents)
	}
	if len(analysis.KeyModelMatrix.Keys) != 2 || len(analysis.KeyModelMatrix.Models) != 1 || analysis.KeyModelMatrix.Cells[0].InputTokens == 0 ||
		analysis.KeyModelMatrix.Cells[0].KnownCost == 0 {
		t.Fatalf("analysis key/model matrix = %+v", analysis.KeyModelMatrix)
	}

	query.Start = events[0].RequestedAt.Add(-31 * 24 * time.Hour)
	longRange, err := database.Analysis(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	assertAllAnalysisSectionsPresent(t, longRange)
	if longRange.Latency.UnsupportedReason == "" || len(longRange.Latency.Samples) != 0 {
		t.Fatalf("long-range latency = %+v", longRange.Latency)
	}
	if longRange.SeriesByCategory == nil || longRange.ModelByTime == nil || longRange.CostComponents == nil || longRange.KeyModelMatrix == nil {
		t.Fatalf("unsupported latency hid another analysis section: %+v", longRange)
	}
}

func TestV2AnalysisMarksUnpricedCostsPartialAndPropagatesTotalFailure(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	event := v2Event(strings.Repeat("5", 32), strings.Repeat("d", 32), strings.Repeat("a", 64), start.Add(time.Minute), true, nil, 0, 0, 1, 10)
	if err := database.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	query := v2Query(model.OperationAnalysis, start, start.Add(time.Hour))
	analysis, err := database.Analysis(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if analysis.CostComponents == nil || !analysis.CostComponents.Meta.Partial {
		t.Fatalf("unpriced cost section = %+v, want partial", analysis.CostComponents)
	}
	if err := database.Close(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = database.Analysis(ctx, query)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("analysis after store close error = %v, want ErrClosed", err)
	}
}

func TestV2AnalysisHistoricalCostsWaitForExplicitReprice(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	rateA := model.NanoUSD(1_000_000_000)
	bookA := aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "price", Model: "model-v2", InputPerMillion: &rateA, OutputPerMillion: &rateA, CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "catalog"}}}
	database, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PriceBook: bookA})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(context.Background()) })
	event := v2Event(strings.Repeat("6", 32), strings.Repeat("e", 32), strings.Repeat("b", 64), start.Add(time.Minute), true, nil, 0, 0, 1, 10)
	if err := database.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	rateB := model.NanoUSD(2_000_000_000)
	bookB := aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "price", Model: "model-v2", InputPerMillion: &rateB, OutputPerMillion: &rateB, CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "catalog"}}}
	if _, err := database.UpdatePriceBook(ctx, bookB); err != nil {
		t.Fatal(err)
	}
	query := v2Query(model.OperationAnalysis, start, start.Add(time.Hour))
	before, err := database.Analysis(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if before.CostComponents == nil || !before.CostComponents.Meta.Partial || before.CostComponents.UncachedInputUSD == "0.0002" {
		t.Fatalf("analysis changed historical cost before reprice: %+v", before.CostComponents)
	}
	if _, err := database.Reprice(ctx, RepriceOptions{Range: model.Range{Start: query.Start, End: query.End, TimeZone: query.TimeZone}, ChunkSize: 100}, nil); err != nil {
		t.Fatal(err)
	}
	after, err := database.Analysis(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if after.CostComponents == nil || after.CostComponents.Meta.Partial || after.CostComponents.UncachedInputUSD != "0.0002" || after.CostComponents.OutputUSD != "0.0002" {
		t.Fatalf("analysis cost after reprice: %+v", after.CostComponents)
	}
}

func TestV2ActivityDayBucketsUseRequestedTimeZone(t *testing.T) {
	database, events := openV2FixtureStore(t)
	query := v2Query(model.OperationActivity, events[0].RequestedAt.Add(-time.Hour), events[0].RequestedAt.Add(24*time.Hour))
	query.Window = "year"
	query.TimeZone = "Asia/Kathmandu"

	activity, err := database.Activity(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if len(activity.Buckets) != 2 {
		t.Fatalf("activity buckets = %+v", activity.Buckets)
	}
	wantStart := time.Date(2026, 8, 31, 18, 15, 0, 0, time.UTC)
	if !activity.Buckets[0].Start.Equal(wantStart) || !activity.Buckets[0].End.Equal(wantStart.Add(24*time.Hour)) {
		t.Fatalf("Kathmandu day bucket = %s..%s", activity.Buckets[0].Start, activity.Buckets[0].End)
	}
	if activity.Buckets[1].Requests != 0 || activity.Buckets[1].TotalTokens != 0 {
		t.Fatalf("Kathmandu trailing bucket = %+v, want zero values", activity.Buckets[1])
	}
}

func TestV2RawCacheDimensionUsesCacheReadTokens(t *testing.T) {
	database, events := openV2FixtureStore(t)
	query := v2Query(model.OperationDimensions, events[0].RequestedAt, events[0].RequestedAt.Add(15*time.Minute))
	query.Dimension = "cache"
	query.PageSize = 10

	page, err := database.Dimensions(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]model.DimensionRow{}
	for _, row := range page.Rows {
		rows[row.Value] = row
	}
	if rows["cached"].UpstreamAttempts != 2 || rows["uncached"].UpstreamAttempts != 1 {
		t.Fatalf("cache dimension = %+v", page.Rows)
	}
}

func TestV2EventsFilterResultErrorAndSourceAndExposeProvenance(t *testing.T) {
	database, events := openV2FixtureStore(t)
	query := v2Query(model.OperationEvents, events[0].RequestedAt, events[0].RequestedAt.Add(15*time.Minute))
	query.PageSize = 10
	query.Filters = map[string]json.RawMessage{
		"result":      json.RawMessage(`"failure"`),
		"error_class": json.RawMessage(`["rate_limit"]`),
		"source":      json.RawMessage(`["import"]`),
	}

	page, err := database.Events(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.TotalCount != 1 || page.Events[0].AttemptID != events[1].AttemptID {
		t.Fatalf("filtered events = %+v", page.Events)
	}
	event := page.Events[0]
	if event.PriceRuleID != "v2-price" || event.PriceSource != "v2-fixture" || event.ImportBatchID != "batch-v2" || event.Source != "import" {
		t.Fatalf("event provenance = rule %q price source %q batch %q source %q", event.PriceRuleID, event.PriceSource, event.ImportBatchID, event.Source)
	}
}

func TestV2SourceDimensionDistinguishesNativeAndImportedEvents(t *testing.T) {
	database, events := openV2FixtureStore(t)
	query := v2Query(model.OperationDimensions, events[0].RequestedAt, events[0].RequestedAt.Add(15*time.Minute))
	query.Dimension = "source"
	query.PageSize = 10

	page, err := database.Dimensions(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	rows := make(map[string]model.DimensionRow, len(page.Rows))
	for _, row := range page.Rows {
		rows[row.Value] = row
	}
	if rows["native"].UpstreamAttempts != 2 || rows["import"].UpstreamAttempts != 1 {
		t.Fatalf("source dimension = %+v", page.Rows)
	}

	if _, err := database.ApplyRetentionPolicy(context.Background(), query.End, time.Time{}, 100); err != nil {
		t.Fatal(err)
	}
	retained, err := database.Dimensions(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	retainedRows := make(map[string]model.DimensionRow, len(retained.Rows))
	for _, row := range retained.Rows {
		retainedRows[row.Value] = row
	}
	if retainedRows["native"].UpstreamAttempts != 2 || retainedRows["import"].UpstreamAttempts != 1 {
		t.Fatalf("retained source dimension = %+v", retained.Rows)
	}
}

func assertAllAnalysisSectionsPresent(t *testing.T, analysis model.Analysis) {
	t.Helper()
	if analysis.SeriesByCategory == nil || analysis.ModelByTime == nil || analysis.Latency == nil || analysis.CostComponents == nil || analysis.KeyModelMatrix == nil {
		t.Fatalf("analysis has nil section: %+v", analysis)
	}
}

func openV2FixtureStore(t *testing.T) (*SQLiteStore, []model.Event) {
	t.Helper()
	ctx := context.Background()
	input, output := model.NanoUSD(1_000_000_000), model.NanoUSD(1_000_000_000)
	book := aggregate.PriceBook{Rules: []aggregate.PricingRule{{
		ID: "v2-price", Model: "model-v2", InputPerMillion: &input, OutputPerMillion: &output,
		CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "v2-fixture",
	}}}
	database, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PriceBook: book})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(context.Background()) })
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	errorClass := "rate_limit"
	ttft := int64(10)
	events := []model.Event{
		v2Event(strings.Repeat("1", 32), strings.Repeat("a", 32), strings.Repeat("d", 64), start.Add(time.Minute), true, nil, 99, 0, ttft, 100),
		v2Event(strings.Repeat("2", 32), strings.Repeat("a", 32), strings.Repeat("d", 64), start.Add(6*time.Minute), false, &errorClass, 0, 20, ttft*2, 200),
		v2Event(strings.Repeat("3", 32), strings.Repeat("b", 32), strings.Repeat("e", 64), start.Add(11*time.Minute), true, nil, 0, 40, ttft*3, 300),
	}
	if err := database.WriteBatch(ctx, []model.Event{events[0], events[2]}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.WriteImportBatch(ctx, []model.Event{events[1]}, "batch-v2"); err != nil {
		t.Fatal(err)
	}
	return database, events
}

func v2Event(attemptID, requestID, keyID string, requestedAt time.Time, succeeded bool, errorClass *string, cached, cacheRead, ttft, latency int64) model.Event {
	status := 200
	if !succeeded {
		status = 429
	}
	return model.Event{
		SchemaVersion: model.EventSchemaVersion, AttemptID: attemptID, ProxyRequestID: requestID,
		RequestIDQuality: model.RequestIDObserved, KeyID: keyID, RequestedAt: requestedAt,
		Provider: "provider-v2", ExecutorType: "executor-v2", Model: "model-v2", EndpointClass: "responses",
		Succeeded: succeeded, UpstreamStatusCode: &status, ErrorClass: errorClass, LatencyMS: latency,
		TimeToFirstTokenMS: &ttft, Tokens: model.TokenUsage{Input: 100, Output: 100, Cached: cached,
			CacheRead: cacheRead, Total: 200, Schema: "normalized-v1", Quality: model.TokenQualityExact},
	}
}

func v2Query(operation model.Operation, start, end time.Time) model.Query {
	return model.Query{SchemaVersion: 2, Operation: operation, Start: start, End: end, TimeZone: "UTC"}
}
