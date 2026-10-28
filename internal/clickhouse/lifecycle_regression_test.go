package clickhouse_test

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"testing"
	"testing/fstest"
	"time"
)

func TestCanceledGetPreservesBorrower(t *testing.T) {
	h := chtest.Handle(t)
	ctx := context.Background()
	db, err := h.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, _ = h.Get(canceled)
	if err := db.Ping(ctx); err != nil {
		t.Fatalf("borrowed pool closed: %v", err)
	}
}
func TestJobVersionSurvivesClockRollback(t *testing.T) {
	db := chtest.New(t)
	ctx := context.Background()
	err := db.Conn().Exec(ctx, "INSERT INTO "+"`"+db.Database()+"`.cpa_jobs (name,state,detail,updated_at,version) VALUES (?,?,?,?,?)", "clock", "running", "", time.Now().UTC(), uint64(time.Now().Add(time.Hour).UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetJob(ctx, "clock", "done", ""); err != nil {
		t.Fatal(err)
	}
	job, _, err := db.Job(ctx, "clock")
	if err != nil || job.State != "done" {
		t.Fatalf("job=%+v err=%v", job, err)
	}
}
func TestRejectUnknownAppliedMigration(t *testing.T) {
	db := chtest.New(t)
	ctx := context.Background()
	files := fstest.MapFS{"0001_first.sql": {Data: []byte("SELECT 1;")}}
	if _, err := db.Migrate(ctx, "future", files); err != nil {
		t.Fatal(err)
	}
	if err := db.Conn().Exec(ctx, "INSERT INTO "+"`"+db.Database()+"`.schema_migrations VALUES (?,?,?,?,?)", "future", uint32(2), "0002_future.sql", "future", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(ctx, "future", files); err == nil {
		t.Fatal("accepted unknown schema")
	}
}
