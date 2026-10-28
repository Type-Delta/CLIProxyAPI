package chstore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
)

func TestC3QuotaHistoryFreshSchemaAndRetrySafeImport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := chtest.Handle(t)
	config := Config{Path: filepath.Join(t.TempDir(), "a.db"), MaxStorageBytes: 64 << 20}
	s, err := Open(ctx, h, config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(ctx) }()
	if s.SchemaVersion() != 1 {
		t.Fatalf("initial analytics schema version=%d", s.SchemaVersion())
	}
	db := s.db
	job, found, err := db.Job(ctx, c4bLegacyJob)
	if err != nil || !found || job.State != "pending" {
		t.Fatalf("fresh schema import job=%+v found=%t err=%v", job, found, err)
	}
	prefix := "`" + db.Database() + "`."
	at := time.Date(2026, 10, 1, 12, 0, 0, 123456789, time.UTC)
	reset := at.Add(time.Hour)
	period := 24.0
	sample := QuotaHistorySample{Provider: "codex", CredentialID: strings.Repeat("a", 64), WindowID: "daily", ObservedAt: at, RemainingPercent: 45, ResetAt: &reset, PeriodHours: &period, Source: "cpamc"}
	var remaining uint64
	if err := db.Conn().QueryRow(ctx, "SELECT count() FROM "+prefix+"analytics_control WHERE namespace='quota_history'").Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("unexpected quota-history control rows %d %v", remaining, err)
	}
	s.mu.Lock()
	err = s.appendQuotaHistoryRows(ctx, []quotaHistoryRow{sample})
	if err == nil {
		err = s.appendQuotaHistoryRows(ctx, []quotaHistoryRow{sample})
	}
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.QuotaHistory(ctx, "codex", at.Add(-time.Second), at.Add(time.Second))
	if err != nil || len(got) != 1 || !got[0].ObservedAt.Equal(at) || got[0].ResetAt == nil || !got[0].ResetAt.Equal(reset) || got[0].PeriodHours == nil || *got[0].PeriodHours != period || got[0].Capacity != nil {
		t.Fatalf("retry-safe quota import %+v %v", got, err)
	}
}
