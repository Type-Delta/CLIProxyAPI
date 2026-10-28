package storageusage

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
)

func TestClickHouseStorageGroupsTablesAndPartitionsDisk(t *testing.T) {
	disk := clickHouseDiskRecord{name: "main", path: "/data/", capacity: 1000, free: 300, unreserved: 250, keepFree: 50}
	queries := clickHouseQueries{
		disks: func(context.Context) ([]clickHouseDiskRecord, error) { return []clickHouseDiskRecord{disk}, nil },
		parts: func(context.Context) ([]clickHousePartRecord, error) {
			return []clickHousePartRecord{
				{database: "cpa", table: "analytics_events", disk: "main", bytes: 70},
				{database: "cpa", table: "requests", disk: "main", bytes: 50},
				{database: "cpa", table: "transcript_content", disk: "main", bytes: 20},
				{database: "cpa", table: "schema_migrations", disk: "main", bytes: 10},
				{database: "other", table: "events", disk: "main", bytes: 100},
				{database: "other", table: "analytics_requests", disk: "main", bytes: 5},
				{database: "cpa", table: "new_table", disk: "missing", bytes: 1},
			}, nil
		},
	}
	result := measureClickHouse(context.Background(), "cpa", queries)
	if !result.Reachable || !result.Partial || len(result.Disks) != 1 {
		t.Fatalf("result = %+v", result)
	}
	got := result.Disks[0]
	want := ClickHouseDisk{
		Name: "main", Path: "/data/", CapacityBytes: 1000, AvailableBytes: 300,
		UsableBytes: 250, KeepFreeBytes: 50, AnalyticsBytes: 70,
		RequestLogsBytes: 70, OtherCPABytes: 10, OtherCHBytes: 105, OtherFilesBytes: 445,
	}
	if got != want {
		t.Fatalf("disk = %+v, want %+v", got, want)
	}
	if classifyClickHouseTable("cpa", "cpa", "analytics_other") != ClickHouseAnalytics {
		t.Fatal("analytics prefix was not classified as analytics")
	}
	if classifyClickHouseTable("cpa", "cpa", "unexpected") != ClickHouseOtherCPA {
		t.Fatal("unexpected CPA table was not classified as other CPA data")
	}
}

func TestClickHouseStorageHandlesInconsistentSnapshots(t *testing.T) {
	queries := clickHouseQueries{
		disks: func(context.Context) ([]clickHouseDiskRecord, error) {
			return []clickHouseDiskRecord{{name: "main", capacity: 100, free: 0, unreserved: 0}}, nil
		},
		parts: func(context.Context) ([]clickHousePartRecord, error) {
			return []clickHousePartRecord{{database: "cpa", table: "requests", disk: "main", bytes: 150}}, nil
		},
	}
	result := measureClickHouse(context.Background(), "cpa", queries)
	if !result.Partial || result.Disks[0].OtherFilesBytes != 0 || result.Disks[0].RequestLogsBytes != 100 {
		t.Fatalf("result = %+v", result)
	}
}

func TestClickHouseStorageReadsRealServer(t *testing.T) {
	db := chtest.New(t)
	ctx := context.Background()
	exec := func(query string) {
		t.Helper()
		if err := db.Conn().Exec(ctx, query); err != nil {
			t.Fatalf("query %s: %v", query, err)
		}
	}
	exec("CREATE TABLE `" + db.Database() + "`.`analytics_events` (id UInt32) ENGINE = MergeTree ORDER BY id")
	exec("CREATE TABLE `" + db.Database() + "`.`requests` (id UInt32) ENGINE = MergeTree ORDER BY id")
	exec("CREATE TABLE `" + db.Database() + "`.`schema_migrations_extra` (id UInt32) ENGINE = MergeTree ORDER BY id")
	exec("INSERT INTO `" + db.Database() + "`.`analytics_events` VALUES (1),(2),(3)")
	exec("INSERT INTO `" + db.Database() + "`.`requests` VALUES (4),(5)")
	exec("INSERT INTO `" + db.Database() + "`.`schema_migrations_extra` VALUES (6)")
	otherDatabase := db.Database() + "_other"
	exec("CREATE DATABASE `" + otherDatabase + "`")
	t.Cleanup(func() {
		if err := db.Conn().Exec(context.Background(), "DROP DATABASE IF EXISTS `"+otherDatabase+"` SYNC"); err != nil {
			t.Errorf("drop other test database: %v", err)
		}
	})
	exec("CREATE TABLE `" + otherDatabase + "`.`unrelated` (id UInt32) ENGINE = MergeTree ORDER BY id")
	exec("INSERT INTO `" + otherDatabase + "`.`unrelated` VALUES (7)")

	queries, err := readClickHouse(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	result := measureClickHouse(ctx, db.Database(), queries)
	if !result.Reachable || result.Partial || len(result.Disks) == 0 {
		t.Fatalf("result = %+v", result)
	}
	var sawData bool
	for _, disk := range result.Disks {
		allParts := disk.AnalyticsBytes + disk.RequestLogsBytes + disk.OtherCPABytes + disk.OtherCHBytes
		if disk.AnalyticsBytes == 0 || disk.RequestLogsBytes == 0 || disk.OtherCPABytes == 0 || disk.OtherCHBytes == 0 {
			t.Fatalf("disk did not classify all test families: %+v", disk)
		}
		if disk.AvailableBytes+allParts+disk.OtherFilesBytes != disk.CapacityBytes {
			t.Fatalf("inconsistent disk partition: %+v", disk)
		}
		if disk.CapacityBytes > 0 && allParts > 0 {
			sawData = true
		}
	}
	if !sawData {
		t.Fatal("ClickHouse reported no sized active parts")
	}
}
