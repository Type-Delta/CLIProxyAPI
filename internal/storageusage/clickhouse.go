package storageusage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse"
)

// ClickHouseTableFamily distinguishes the business data shown in the panel.
type ClickHouseTableFamily string

const (
	ClickHouseAnalytics   ClickHouseTableFamily = "analytics"
	ClickHouseRequestLogs ClickHouseTableFamily = "request_logs"
	ClickHouseOtherCPA    ClickHouseTableFamily = "cpa_other"
	ClickHouseOtherDBs    ClickHouseTableFamily = "clickhouse_other"
	ClickHouseOtherFiles  ClickHouseTableFamily = "other_files"
)

var requestLogTables = map[string]struct{}{
	"requests":           {},
	"attempts":           {},
	"request_contexts":   {},
	"transcript_entries": {},
	"transcript_content": {},
}

// ClickHouseDisk is one storage device reported by ClickHouse, partitioned the
// same way as a local filesystem snapshot.
type ClickHouseDisk struct {
	Name             string `json:"name"`
	Path             string `json:"path"`
	CapacityBytes    uint64 `json:"capacity_bytes"`
	AvailableBytes   uint64 `json:"available_bytes"`
	UsableBytes      uint64 `json:"usable_bytes"`
	KeepFreeBytes    uint64 `json:"keep_free_bytes"`
	AnalyticsBytes   uint64 `json:"analytics_bytes"`
	RequestLogsBytes uint64 `json:"request_logs_bytes"`
	OtherCPABytes    uint64 `json:"cpa_other_bytes"`
	OtherCHBytes     uint64 `json:"clickhouse_other_bytes"`
	OtherFilesBytes  uint64 `json:"other_files_bytes"`
}

// ClickHouseStorage is omitted in plain mode. An unreachable server still keeps
// the local filesystem readings and explains why ClickHouse has no readings.
type ClickHouseStorage struct {
	Database   string           `json:"database"`
	Reachable  bool             `json:"reachable"`
	Message    string           `json:"message,omitempty"`
	MeasuredAt time.Time        `json:"measured_at"`
	Partial    bool             `json:"partial"`
	Disks      []ClickHouseDisk `json:"disks"`
}

type clickHouseDiskRecord struct {
	name, path string
	capacity   uint64
	free       uint64
	unreserved uint64
	keepFree   uint64
}

type clickHousePartRecord struct {
	database, table, disk string
	bytes                 uint64
}

type clickHouseQueries struct {
	disks func(context.Context) ([]clickHouseDiskRecord, error)
	parts func(context.Context) ([]clickHousePartRecord, error)
}

func classifyClickHouseTable(database, cpaDatabase, table string) ClickHouseTableFamily {
	if database == "" || database != cpaDatabase {
		return ClickHouseOtherDBs
	}
	if strings.HasPrefix(table, "analytics_") {
		return ClickHouseAnalytics
	}
	if _, ok := requestLogTables[table]; ok {
		return ClickHouseRequestLogs
	}
	return ClickHouseOtherCPA
}

// measureClickHouse reads ClickHouse's own accounting instead of scanning its
// host filesystem. Only successful snapshots are cached by Meter.
func measureClickHouse(ctx context.Context, database string, queries clickHouseQueries) ClickHouseStorage {
	result := ClickHouseStorage{Database: database, Disks: []ClickHouseDisk{}}
	diskRows, err := queries.disks(ctx)
	if err != nil {
		result.Message = "could not read ClickHouse disks: " + err.Error()
		result.MeasuredAt = time.Now().UTC()
		return result
	}
	partRows, err := queries.parts(ctx)
	if err != nil {
		result.Message = "could not read ClickHouse table sizes: " + err.Error()
		result.MeasuredAt = time.Now().UTC()
		return result
	}

	index := make(map[string]int, len(diskRows))
	for _, row := range diskRows {
		index[row.name] = len(result.Disks)
		result.Disks = append(result.Disks, ClickHouseDisk{
			Name: row.name, Path: row.path, CapacityBytes: row.capacity,
			AvailableBytes: row.free, UsableBytes: row.unreserved, KeepFreeBytes: row.keepFree,
		})
	}
	for _, part := range partRows {
		diskIndex, ok := index[part.disk]
		if !ok {
			result.Partial = true
			continue
		}
		disk := &result.Disks[diskIndex]
		switch classifyClickHouseTable(part.database, database, part.table) {
		case ClickHouseAnalytics:
			disk.AnalyticsBytes += part.bytes
		case ClickHouseRequestLogs:
			disk.RequestLogsBytes += part.bytes
		case ClickHouseOtherCPA:
			disk.OtherCPABytes += part.bytes
		default:
			disk.OtherCHBytes += part.bytes
		}
	}
	for i := range result.Disks {
		disk := &result.Disks[i]
		free := min(disk.AvailableBytes, disk.CapacityBytes)
		allParts := disk.AnalyticsBytes + disk.RequestLogsBytes + disk.OtherCPABytes + disk.OtherCHBytes
		if allParts > disk.CapacityBytes-free {
			// system.parts and system.disks are separate snapshots and can race.
			// Keep the displayed partition bounded without erasing CPA categories first.
			result.Partial = true
			overflow := allParts - (disk.CapacityBytes - free)
			for _, amount := range []*uint64{&disk.OtherCHBytes, &disk.OtherCPABytes, &disk.RequestLogsBytes, &disk.AnalyticsBytes} {
				reduced := min(*amount, overflow)
				*amount -= reduced
				overflow -= reduced
			}
			allParts = disk.AnalyticsBytes + disk.RequestLogsBytes + disk.OtherCPABytes + disk.OtherCHBytes
		}
		disk.AvailableBytes = free
		disk.OtherFilesBytes = disk.CapacityBytes - free - allParts
	}
	result.Reachable = true
	result.MeasuredAt = time.Now().UTC()
	return result
}

func readClickHouse(_ context.Context, db *clickhouse.DB) (clickHouseQueries, error) {
	conn := db.Conn()
	return clickHouseQueries{
		disks: func(ctx context.Context) ([]clickHouseDiskRecord, error) {
			rows, err := conn.Query(ctx, "SELECT name, path, total_space, free_space, unreserved_space, keep_free_space FROM system.disks ORDER BY name")
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			values := make([]clickHouseDiskRecord, 0, 4)
			for rows.Next() {
				var row clickHouseDiskRecord
				if err := rows.Scan(&row.name, &row.path, &row.capacity, &row.free, &row.unreserved, &row.keepFree); err != nil {
					return nil, err
				}
				values = append(values, row)
			}
			return values, rows.Err()
		},
		parts: func(ctx context.Context) ([]clickHousePartRecord, error) {
			rows, err := conn.Query(ctx, "SELECT database, table, disk_name, sum(bytes_on_disk) FROM system.parts WHERE active GROUP BY database, table, disk_name ORDER BY database, table, disk_name")
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			values := make([]clickHousePartRecord, 0, 64)
			for rows.Next() {
				var row clickHousePartRecord
				if err := rows.Scan(&row.database, &row.table, &row.disk, &row.bytes); err != nil {
					return nil, err
				}
				values = append(values, row)
			}
			return values, rows.Err()
		},
	}, nil
}

type clickHouseCacheEntry struct {
	key      string
	snapshot ClickHouseStorage
}

// MeasureClickHouse adds ClickHouse readings to a Meter's existing short-lived
// cache. Connection failures are retried on the next request; they are never
// allowed to hide local storage.
func (m *Meter) MeasureClickHouse(ctx context.Context, db *clickhouse.DB) ClickHouseStorage {
	if m == nil || db == nil {
		return unreachableClickHouse(ctx, errors.New("clickhouse storage is not active"))
	}
	key := fmt.Sprintf("%p:%s", db, db.Database())
	queries, err := readClickHouse(ctx, db)
	if err != nil {
		return unreachableClickHouse(ctx, err)
	}
	m.mu.Lock()
	cached := m.clickHouse.key == key && time.Since(m.clickHouse.snapshot.MeasuredAt) < 15*time.Second
	snapshot := m.clickHouse.snapshot
	m.mu.Unlock()
	if cached {
		return snapshot
	}

	// Internal ClickHouse queries are bounded independently of the HTTP request.
	measureCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	snapshot = measureClickHouse(measureCtx, db.Database(), queries)
	if ctx.Err() == nil && snapshot.Reachable {
		m.mu.Lock()
		m.clickHouse = clickHouseCacheEntry{key: key, snapshot: snapshot}
		m.mu.Unlock()
	}
	return snapshot
}

func unreachableClickHouse(ctx context.Context, cause error) ClickHouseStorage {
	message := cause.Error()
	if ctx.Err() != nil {
		message = "could not read ClickHouse storage: " + ctx.Err().Error()
	}
	return ClickHouseStorage{Reachable: false, Message: message, Disks: []ClickHouseDisk{}, MeasuredAt: time.Now().UTC()}
}
