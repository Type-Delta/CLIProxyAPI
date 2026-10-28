package api

import (
	"context"
	"errors"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/chstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
)

func TestR2AnalyticsHealthReportsAbandonedImport(t *testing.T) {
	h := chtest.Handle(t)
	db, err := h.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetJob(context.Background(), "legacy-import:analytics", "abandoned", `{"reason":"unrecoverable legacy source","abandoned_at":"2026-10-28T00:00:00Z"}`); err != nil {
		t.Fatal(err)
	}
	status := legacyImportJobStatus(context.Background(), h, "legacy-import:analytics")
	if status == nil || status.State != "abandoned" || status.Reason != "unrecoverable legacy source" {
		t.Fatalf("status=%+v", status)
	}
}

func TestR2AbandonFailedImportUnblocksMaintenance(t *testing.T) {
	ctx := context.Background()
	h := chtest.Handle(t)
	db, err := h.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg := cpauk.DefaultConfig()
	cfg.Enabled = true
	cfg.Path = filepath.Join(t.TempDir(), "analytics.db")
	st, err := chstore.Open(ctx, h, chstore.Config{Path: cfg.Path, MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetJob(ctx, "legacy-import:analytics", "failed", "corrupt source"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	b := &analyticsBackend{Store: st, handle: h, done: done, cancel: func() {}, importErr: errors.New("corrupt source")}
	service := cpauk.New(ctx, cfg, func(context.Context, cpauk.Config) (cpauk.Backend, [32]byte, error) {
		return b, st.IdentityKeyArray(), nil
	})
	defer service.Close(ctx)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !service.Capabilities().Available {
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("failed import incorrectly blocks readiness")
		}
	}
	job, err := service.Maintenance().Start(ctx, cpauk.MaintenanceRequest{Kind: "abandon_legacy_import", Options: map[string]any{"reason": "unrecoverable source acknowledged"}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		status, err := service.Maintenance().Status(ctx, job.JobID)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == model.JobSucceeded {
			break
		}
		if status.State == model.JobFailed {
			t.Fatalf("abandon failed: %+v", status)
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("abandon never completed")
		}
	}
	if err = b.WaitLegacyImport(ctx); err != nil {
		t.Fatalf("abandoned import still blocks maintenance: %v", err)
	}
	health := service.Health()
	if health.LegacyImport == nil || health.LegacyImport.State != "abandoned" || health.LegacyImport.Reason != "unrecoverable source acknowledged" {
		t.Fatalf("health=%+v", health.LegacyImport)
	}
	if _, err = service.Maintenance().Start(ctx, cpauk.MaintenanceRequest{Kind: "checkpoint"}); err != nil {
		t.Fatalf("maintenance still blocked: %v", err)
	}
}
