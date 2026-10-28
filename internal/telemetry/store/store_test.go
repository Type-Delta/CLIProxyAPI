package store

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
)

func TestOpenSchedulesImportOnce(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	if _, err := Open(ctx, db, 30); err != nil {
		t.Fatal(err)
	}
	job, ok, err := db.Job(ctx, LegacyJob)
	if err != nil || !ok || job.State != "pending" {
		t.Fatalf("job = %+v, %v, %v", job, ok, err)
	}
	if err := db.SetJob(ctx, LegacyJob, "done", "finished"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, db, 30); err != nil {
		t.Fatal(err)
	}
	job, _, err = db.Job(ctx, LegacyJob)
	if err != nil || job.State != "done" {
		t.Fatalf("job = %+v, %v", job, err)
	}
}

func TestRetentionDoesNotAlterUnchangedTTL(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetRetention(ctx, 30); err != nil {
		t.Fatal(err)
	}
	// ClickHouse query logs expose whether startup issued expensive ALTERs.
	if err = db.Conn().Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatal(err)
	}
	var n uint64
	err = db.Conn().QueryRow(ctx, "SELECT count() FROM system.query_log WHERE type='QueryFinish' AND startsWith(query, ?)", "ALTER TABLE `"+db.Database()+"`.requests MODIFY TTL").Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("unchanged retention repeated ALTER: %d %v", n, err)
	}
	if err = s.SetRetention(ctx, 31); err != nil {
		t.Fatal(err)
	}
	var engine string
	if err = db.Conn().QueryRow(ctx, "SELECT engine_full FROM system.tables WHERE database=? AND name='requests'", db.Database()).Scan(&engine); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(engine, "31") {
		t.Fatal(engine)
	}
}
