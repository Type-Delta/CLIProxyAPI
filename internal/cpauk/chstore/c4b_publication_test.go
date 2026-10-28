package chstore

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// Every exported retained-history reader must ignore staged and superseded
// generations. This also catches readers that bypass retainedSelection.
func TestC4bAllPublicReadersIgnoreUnpublishedGenerations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	event := loadFixtureEvents(t)[0]
	event.RequestedAt = time.Date(2026, 8, 31, 4, 5, 0, 0, time.UTC)
	credential := strings.Repeat("a", 64)
	auth := "oauth"
	event.CredentialID = &credential
	event.AuthType = &auth
	if err := s.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	raw := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.ApplyRetention(ctx, raw, 1); err != nil {
		t.Fatal(err)
	}
	base := model.Query{SchemaVersion: 2, Start: raw.AddDate(0, 0, -1), End: raw.AddDate(0, 0, 1), TimeZone: "UTC"}
	readers := map[string]func() (any, error){
		"Summary": func() (any, error) { q := base; q.Operation = model.OperationSummary; return s.Summary(ctx, q) },
		"Timeseries": func() (any, error) {
			q := base
			q.Operation = model.OperationTimeseries
			q.BucketWidth = "1d"
			return s.Timeseries(ctx, q)
		},
		"Dimensions": func() (any, error) {
			q := base
			q.Operation = model.OperationDimensions
			q.Dimension = "provider"
			q.PageSize = 100
			return s.Dimensions(ctx, q)
		},
		"Leaderboard": func() (any, error) {
			q := base
			q.Operation = model.OperationLeaderboard
			q.SortBy = model.LeaderboardSortCost
			q.PageSize = 100
			return s.Leaderboard(ctx, q)
		},
		"ModelLeaderboard": func() (any, error) {
			q := base
			q.Operation = model.OperationModelLeaderboard
			return s.ModelLeaderboard(ctx, q)
		},
		"ModelCostSeries": func() (any, error) {
			q := base
			q.Operation = model.OperationModelCostSeries
			q.BucketWidth = "1d"
			name, _ := json.Marshal([]string{event.Model})
			q.Filters = map[string]json.RawMessage{"model": name}
			return s.ModelCostSeries(ctx, q)
		},
		"Activity": func() (any, error) {
			q := base
			q.Operation = model.OperationActivity
			q.Window = "year"
			return s.Activity(ctx, q)
		},
		"Analysis": func() (any, error) {
			q := base
			q.Operation = model.OperationAnalysis
			q.BucketWidth = "1d"
			return s.Analysis(ctx, q)
		},
		"KeyCatalog": func() (any, error) {
			q := base
			q.Operation = model.OperationDimensions
			q.Dimension = "key"
			q.PageSize = 100
			return s.KeyCatalog(ctx, q)
		},
		"PricingMissing": func() (any, error) {
			return s.PricingMissing(ctx, model.Range{Start: base.Start, End: base.End, TimeZone: base.TimeZone})
		},
		"ProviderCredentials": func() (any, error) { return s.ProviderCredentials(ctx) },
	}
	baseline := map[string]any{}
	baselineErrors := map[string]string{}
	for name, read := range readers {
		value, err := read()
		if err != nil && !errors.Is(err, ErrRetainedRangePartial) {
			t.Fatalf("baseline %s: %v", name, err)
		}
		if err != nil {
			baselineErrors[name] = err.Error()
		}
		baseline[name] = value
	}
	start := event.RequestedAt.Truncate(time.Hour)
	bucket := c4bBucket{"hourly", start.UnixNano(), start.Add(time.Hour).UnixNano(), ""}
	generation := s.nextVersion()
	// Publish an equivalent replacement and retain its superseded physical rows.
	if err := s.c4bStageLateBucket(ctx, bucket, generation); err != nil {
		t.Fatal(err)
	}
	if err := s.c4bPublishHeads(ctx, []c4bBucket{bucket}, generation); err != nil {
		t.Fatal(err)
	}
	for name, read := range readers {
		value, err := read()
		errText := ""
		if err != nil {
			errText = err.Error()
		}
		if errText != baselineErrors[name] || !reflect.DeepEqual(value, baseline[name]) {
			t.Fatalf("superseded generation leaked into %s: err=%v\n got=%+v\nwant=%+v", name, err, value, baseline[name])
		}
	}
	activeGeneration := generation
	generation = s.nextVersion()
	columns := strings.Split(c4bRollupColumns, ",")
	projection := append([]string(nil), columns...)
	for i, column := range columns {
		if column == "upstream_attempts" || column == "total_tokens" || column == "unpriced_tokens" {
			projection[i] = "toInt64(99999) AS " + column
		}
	}
	query := "INSERT INTO " + s.table("analytics_rollups") + " (" + c4bRollupColumns + ",version,is_deleted,generation) SELECT " + strings.Join(projection, ",") + ",?,toUInt8(0),? FROM " + s.table("analytics_rollups") + " FINAL WHERE is_deleted=0 AND " + s.c4bPublishedPredicate()
	if err := s.db.Conn().Exec(ctx, query, s.nextVersion(), generation); err != nil {
		t.Fatal(err)
	}
	requestProjection := strings.Replace(c4bRequestColumns, "proxy_request_id", "repeat('f',32) AS proxy_request_id", 1)
	query = "INSERT INTO " + s.table("analytics_request_rollups") + " (" + c4bRequestColumns + ",version,is_deleted,generation) SELECT " + requestProjection + ",?,toUInt8(0),? FROM " + s.table("analytics_request_rollups") + " FINAL WHERE is_deleted=0 AND " + s.c4bPublishedPredicate()
	if err := s.db.Conn().Exec(ctx, query, s.nextVersion(), generation); err != nil {
		t.Fatal(err)
	}
	for name, read := range readers {
		t.Run(name, func(t *testing.T) {
			value, err := read()
			errText := ""
			if err != nil {
				errText = err.Error()
			}
			if errText != baselineErrors[name] || !reflect.DeepEqual(value, baseline[name]) {
				t.Fatalf("unpublished generation leaked: err=%v\n got=%+v\nwant=%+v", err, value, baseline[name])
			}
		})
	}
	// Previews report logical facts; physical deletion still removes every generation.
	count, err := s.PreviewPurgeByKeyID(ctx, event.KeyID)
	if err != nil || count != 2 {
		t.Fatalf("preview=%d err=%v", count, err)
	}
	path := filepath.Join(t.TempDir(), "published-backup")
	manifest, err := s.Backup(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	exported := 0
	if err = readArchive(ctx, path, manifest.DatabaseSHA256, func(table, schema string, raw json.RawMessage) error {
		if raw == nil || table != "analytics_rollups" && table != "analytics_request_rollups" {
			return nil
		}
		var row map[string]json.RawMessage
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		if strings.Trim(string(row["generation"]), "\"") != strconv.FormatUint(activeGeneration, 10) {
			return errors.New("backup exported an unpublished or superseded generation")
		}
		exported++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if exported != 2 {
		t.Fatalf("backup exported %d retained rows, want active total and request rows", exported)
	}
	if _, err = s.PurgeByKeyID(ctx, event.KeyID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"analytics_rollups", "analytics_request_rollups"} {
		var count uint64
		if err = s.db.Conn().QueryRow(ctx, "SELECT count() FROM "+s.table(table)+" WHERE key_id=?", event.KeyID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("purge left %d physical rows in %s: %v", count, table, err)
		}
	}

}
