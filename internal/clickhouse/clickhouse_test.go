package clickhouse_test

import (
	"context"
	"errors"
	"testing"
	"testing/fstest"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
)

func TestHandleErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := clickhouse.NewHandle(clickhouse.Config{}).Get(ctx); !errors.Is(err, clickhouse.ErrNotConfigured) {
		t.Fatalf("got %v", err)
	}
	if _, err := clickhouse.NewHandle(clickhouse.Config{DSN: "://invalid"}).Get(ctx); err == nil {
		t.Fatal("invalid DSN accepted")
	}
}
func TestMigrateAndJobs(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	files := fstest.MapFS{"0001_first.sql": {Data: []byte("CREATE TABLE {database}.migration_test (x UInt8 COMMENT 'Test value') ENGINE = MergeTree ORDER BY x COMMENT 'Test table';\nINSERT INTO {database}.migration_test VALUES (42);\n")}}
	res, err := db.Migrate(ctx, "test", files)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.FromVersion != 0 || res.ToVersion != 1 {
		t.Fatalf("result: %+v", res)
	}
	var n uint8
	if err = db.Conn().QueryRow(ctx, "SELECT x FROM `"+db.Database()+"`.migration_test").Scan(&n); err != nil || n != 42 {
		t.Fatalf("query: %d %v", n, err)
	}
	res, err = db.Migrate(ctx, "test", files)
	if err != nil || res.Created || res.FromVersion != 1 || res.ToVersion != 1 {
		t.Fatalf("rerun: %+v %v", res, err)
	}
	files["0001_first.sql"].Data = append(files["0001_first.sql"].Data, ' ')
	if _, err = db.Migrate(ctx, "test", files); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	if err = db.EnsureJob(ctx, "legacy"); err != nil {
		t.Fatal(err)
	}
	if err = db.SetJob(ctx, "legacy", "done", "imported"); err != nil {
		t.Fatal(err)
	}
	if err = db.EnsureJob(ctx, "legacy"); err != nil {
		t.Fatal(err)
	}
	job, ok, err := db.Job(ctx, "legacy")
	if err != nil || !ok || job.State != "done" || job.Detail != "imported" || job.UpdatedAt.IsZero() {
		t.Fatalf("job: %+v %v %v", job, ok, err)
	}
}

func TestMigrateWithJobOnlyOnCreation(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	files := fstest.MapFS{"0001_first.sql": {Data: []byte("CREATE TABLE {database}.job_test (x UInt8 COMMENT 'Test value') ENGINE = MergeTree ORDER BY x COMMENT 'Test table';\n")}}
	if _, err := db.Migrate(ctx, "existing", files); err != nil {
		t.Fatal(err)
	}
	if _, err := db.MigrateWithJob(ctx, "existing", files, "existing-job"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := db.Job(ctx, "existing-job"); err != nil || ok {
		t.Fatalf("existing component got a job: ok=%v err=%v", ok, err)
	}
	broken := fstest.MapFS{"0001_broken.sql": {Data: []byte("THIS IS NOT SQL;\n")}}
	if _, err := db.MigrateWithJob(ctx, "fresh", broken, "fresh-job"); err == nil {
		t.Fatal("broken migration succeeded")
	}
	job, ok, err := db.Job(ctx, "fresh-job")
	if err != nil || !ok || job.State != "pending" {
		t.Fatalf("job not recorded before a failed creation: %+v ok=%v err=%v", job, ok, err)
	}
}
