package chstore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
)

func TestCoreWriteDedupePriceAndReplace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	events := loadFixtureEvents(t)
	raw := model.RawJSON(`{"usage":{"total_tokens":400},"telemetry":"` + strings.Repeat("x", 39000) + `"}`)
	events[1].UpstreamUsageRaw = &raw
	if err := s.WriteBatch(ctx, append(events, events[0])); err != nil {
		t.Fatal(err)
	}
	changed := events[0]
	changed.Model = "changed"
	changed.RequestedAt = changed.RequestedAt.AddDate(0, 1, 0)
	if err := s.WriteBatch(ctx, []model.Event{changed}); err != nil {
		t.Fatal(err)
	}
	got, err := s.loadEvents(ctx, "requested_at_ns>=?", events[0].RequestedAt.UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("dedupe count=%d", len(got))
	}
	var cost model.NanoUSD
	var unpriced int64
	for _, event := range got {
		if event.KnownCost != nil {
			cost += *event.KnownCost
		}
		unpriced += event.UnpricedTokens
		if event.AttemptID == events[0].AttemptID && (event.Model != events[0].Model || !event.RequestedAt.Equal(events[0].RequestedAt)) {
			t.Fatal("duplicate replaced original")
		}
		if event.AttemptID == events[1].AttemptID && (event.UpstreamUsageRaw == nil || *event.UpstreamUsageRaw != raw) {
			t.Fatal("diagnostic payload changed")
		}
	}
	if cost.String() != "0.002157525" || unpriced != 200 {
		t.Fatalf("cost=%s unpriced=%d", cost.String(), unpriced)
	}
	// A complete reprice version replaces the old row before background merges.
	replacement := got[0]
	newCost := model.NanoUSD(123)
	replacement.KnownCost = &newCost
	s.mu.Lock()
	_, err = s.insertEvents(ctx, []model.Event{replacement}, "", true)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.loadEvents(ctx, "attempt_id=?", replacement.AttemptID)
	if err != nil || len(got) != 1 || got[0].KnownCost == nil || *got[0].KnownCost != newCost {
		t.Fatalf("replacement=%+v err=%v", got, err)
	}
	var physical uint64
	if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM "+s.table("analytics_events")+" FINAL WHERE is_deleted=0").Scan(&physical); err != nil || physical != 3 {
		t.Fatalf("logical scalar count=%d err=%v", physical, err)
	}
}

func TestCorePatchMatchesSQLite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	sqlite, err := store.Open(ctx, Config{Path: filepath.Join(t.TempDir(), "source.db"), MaxStorageBytes: 64 << 20, PriceBook: fixturePriceBook()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlite.Close(ctx) }()
	events := loadFixtureEvents(t)
	events[1].ProxyRequestID = events[0].ProxyRequestID
	// An orphan patch is ignored rather than inherited by future attempts.
	orphan := model.ProxyResponsePatch{ProxyRequestID: events[0].ProxyRequestID, StatusCode: 500, Error: "orphan"}
	for _, writer := range []interface {
		WriteBatchWithPatches(context.Context, []model.Event, []model.ProxyResponsePatch) error
	}{s, sqlite} {
		if err := writer.WriteBatchWithPatches(ctx, nil, []model.ProxyResponsePatch{orphan}); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 9, 9, 3, 0, 0, 1, time.UTC)
	patches := []model.ProxyResponsePatch{{ProxyRequestID: "invalid", StatusCode: 200}, {ProxyRequestID: events[0].ProxyRequestID, StatusCode: 99}, {ProxyRequestID: events[0].ProxyRequestID, StatusCode: 429, Error: "limited", RespondedAt: at}, {ProxyRequestID: events[0].ProxyRequestID, StatusCode: 200, RespondedAt: at.Add(time.Second)}}
	for _, writer := range []interface {
		WriteBatchWithPatches(context.Context, []model.Event, []model.ProxyResponsePatch) error
	}{s, sqlite} {
		if err := writer.WriteBatchWithPatches(ctx, events[:2], patches); err != nil {
			t.Fatal(err)
		}
		// A new retry does not inherit the old patch, but can receive a later one.
		retry := events[2]
		retry.ProxyRequestID = events[0].ProxyRequestID
		if err := writer.WriteBatchWithPatches(ctx, []model.Event{retry}, nil); err != nil {
			t.Fatal(err)
		}
	}
	before, err := s.loadEvents(ctx, "attempt_id=?", events[2].AttemptID)
	if err != nil || len(before) != 1 || before[0].ProxyStatusCode != nil {
		t.Fatalf("late retry inherited outcome: %+v %v", before, err)
	}
	for _, writer := range []interface {
		WriteBatchWithPatches(context.Context, []model.Event, []model.ProxyResponsePatch) error
	}{s, sqlite} {
		if err := writer.WriteBatchWithPatches(ctx, nil, patches[3:]); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.loadEvents(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range got {
		want, found, err := sqlite.EventByAttemptID(ctx, event.AttemptID, model.Query{SchemaVersion: 1, Operation: model.OperationEvents, Start: event.RequestedAt.Add(-time.Hour), End: event.RequestedAt.Add(time.Hour), TimeZone: "UTC"})
		if err != nil || !found {
			t.Fatalf("SQLite lookup: %t %v", found, err)
		}
		if !reflect.DeepEqual(event.ProxyStatusCode, want.ProxyStatusCode) || !reflect.DeepEqual(event.ProxyError, want.ProxyError) || !reflect.DeepEqual(event.RespondedAt, want.RespondedAt) {
			t.Fatalf("patch differs: got=%+v want=%+v", event, want)
		}
	}
}

func TestCoreValidationPrecedesWriteAndConcurrentDedupe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	events := loadFixtureEvents(t)
	invalid := events[1]
	invalid.Tokens.Total = -1
	if err := s.WriteBatch(ctx, []model.Event{events[0], invalid}); err == nil {
		t.Fatal("invalid batch accepted")
	}
	got, err := s.loadEvents(ctx, "")
	if err != nil || len(got) != 0 {
		t.Fatalf("partial write=%+v err=%v", got, err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := s.WriteBatch(ctx, events); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	got, err = s.loadEvents(ctx, "")
	if err != nil || len(got) != 3 {
		t.Fatalf("concurrent dedupe count=%d err=%v", len(got), err)
	}
}

func TestCoreIdentityLegacyAndSharedPool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handle := chtest.Handle(t)
	config := Config{Path: filepath.Join(t.TempDir(), "legacy.db"), MaxStorageBytes: 64 << 20}
	legacy, err := store.Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	key, epoch := legacy.IdentityKey(), legacy.IdentityEpoch()
	if err := legacy.Close(ctx); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, handle, config)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(key, s.IdentityKey()) || epoch != s.IdentityEpoch() {
		t.Fatal("legacy identity not preserved")
	}
	actual, err := os.ReadFile(config.Path)
	if err != nil || !reflect.DeepEqual(original, actual) {
		t.Fatal("legacy source changed")
	}
	gotKey := s.IdentityKey()
	gotKey[0] ^= 255
	if reflect.DeepEqual(gotKey, s.IdentityKey()) {
		t.Fatal("identity accessor leaked mutable key")
	}
	db := s.db
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(ctx); err != nil {
		t.Fatalf("store close killed shared pool: %v", err)
	}
	if err := s.WriteBatch(ctx, loadFixtureEvents(t)); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed write: %v", err)
	}
	s, err = Open(ctx, handle, config)
	if err != nil {
		t.Fatal(err)
	}
	if s.IdentityEpoch() != epoch {
		t.Fatal("epoch changed on reopen")
	}
	job, found, err := db.Job(ctx, "legacy-import:analytics")
	if err != nil || !found || job.State != "pending" {
		t.Fatalf("job=%+v found=%t err=%v", job, found, err)
	}
	if err := db.SetJob(ctx, job.Name, "done", "test"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close(ctx)
	identityPath := filepath.Join(filepath.Dir(config.Path), "identity.key")
	if err := os.WriteFile(identityPath, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, handle, config); !errors.Is(err, ErrIdentityKeyMismatch) {
		t.Fatalf("mismatched key: %v", err)
	}
	if err := os.Remove(identityPath); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, handle, config); !errors.Is(err, ErrIdentityKeyMissing) {
		t.Fatalf("missing key: %v", err)
	}
	job, found, err = db.Job(ctx, job.Name)
	if err != nil || !found || job.State != "done" {
		t.Fatalf("job reset on reopen: %+v %v", job, err)
	}
}

func TestCoreFreshIdentityAndGenerationSnapshots(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	info, err := os.Stat(s.config.IdentityKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 32 || info.Mode().Perm() != 0o600 {
		t.Fatalf("identity file: %v", info)
	}
	initial := map[string]json.RawMessage{"one": json.RawMessage(`{"status":"configured"}`), "two": json.RawMessage(`{"status":"rotated"}`)}
	if err := s.replaceControl(ctx, "key_lifecycle", initial); err != nil {
		t.Fatal(err)
	}
	values, err := s.loadControl(ctx, "key_lifecycle")
	if err != nil || !reflect.DeepEqual(values, initial) {
		t.Fatalf("snapshot=%s err=%v", values, err)
	}
	// A staged generation without a head cannot leak into readers.
	if err := s.db.Conn().Exec(ctx, "INSERT INTO "+s.table("analytics_control")+" (namespace,generation,key,value,version,is_deleted) VALUES (?,?,?,?,?,?)", "key_lifecycle", s.nextVersion(), "orphan", "{}", s.nextVersion(), uint8(0)); err != nil {
		t.Fatal(err)
	}
	values, err = s.loadControl(ctx, "key_lifecycle")
	if err != nil || len(values) != 2 {
		t.Fatalf("staged snapshot leaked: %s %v", values, err)
	}
	if err := s.replaceControl(ctx, "key_lifecycle", map[string]json.RawMessage{"bad": json.RawMessage(`invalid`)}); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	if err := s.replaceControl(ctx, "key_lifecycle", nil); err != nil {
		t.Fatal(err)
	}
	values, err = s.loadControl(ctx, "key_lifecycle")
	if err != nil || len(values) != 0 {
		t.Fatalf("empty replacement=%s err=%v", values, err)
	}
	var uncommented uint64
	if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM system.columns WHERE database=? AND startsWith(table,'analytics_') AND comment=''", s.db.Database()).Scan(&uncommented); err != nil || uncommented != 0 {
		t.Fatalf("uncommented columns=%d err=%v", uncommented, err)
	}
	if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM system.tables WHERE database=? AND startsWith(name,'analytics_') AND comment=''", s.db.Database()).Scan(&uncommented); err != nil || uncommented != 0 {
		t.Fatalf("uncommented tables=%d err=%v", uncommented, err)
	}
}

func TestCoreBudgetQueriesAndReconfigure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	events := loadFixtureEvents(t)
	if err := s.checkQuota(ctx, 0); err != nil {
		t.Fatalf("live parts/disk budget query: %v", err)
	}
	oldMax := s.config.MaxStorageBytes
	if err := s.ReconfigureStorageBudget(ctx, 1, 0); !errors.Is(err, ErrStorageQuota) {
		t.Fatalf("tiny budget: %v", err)
	}
	if s.config.MaxStorageBytes != oldMax {
		t.Fatal("failed budget update persisted")
	}
	if err := s.ReconfigureStorageBudget(ctx, 0, 1<<62); !errors.Is(err, ErrInsufficientFreeSpace) {
		t.Fatalf("free-space reserve: %v", err)
	}
	if err := s.ReconfigureStorageBudget(ctx, 128<<20, 1); err != nil {
		t.Fatal(err)
	}
	s.config.MaxStorageBytes = s.budgetUsed + 1
	if err := s.WriteBatch(ctx, events); !errors.Is(err, ErrStorageQuota) {
		t.Fatalf("write over quota: %v", err)
	}
	s.config.MaxStorageBytes = 128 << 20
	s.config.MinFreeBytes = 1 << 62
	if err := s.WriteBatch(ctx, events); !errors.Is(err, ErrInsufficientFreeSpace) {
		t.Fatalf("write below reserve: %v", err)
	}
	s.config.MinFreeBytes = 1
	if err := s.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	if s.budgetReserved == 0 {
		t.Fatal("successful writes did not reserve cached budget")
	}
}

func TestCorePatchPreservesRecordedFieldsAndDefaultsTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	events := loadFixtureEvents(t)
	at := time.Date(2026, 9, 9, 3, 0, 0, 0, time.UTC)
	status := 201
	for i := range events {
		events[i].ProxyRequestID = events[0].ProxyRequestID
	}
	events[0].ProxyStatusCode = &status
	events[1].RespondedAt = &at
	if err := s.WriteBatchWithPatches(ctx, events, []model.ProxyResponsePatch{{ProxyRequestID: events[0].ProxyRequestID, StatusCode: 204}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.loadEvents(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range got {
		switch event.AttemptID {
		case events[0].AttemptID:
			if event.ProxyStatusCode == nil || *event.ProxyStatusCode != 201 || event.RespondedAt != nil {
				t.Fatal("patch overwrote recorded status")
			}
		case events[1].AttemptID:
			if event.ProxyStatusCode != nil || event.RespondedAt == nil || !event.RespondedAt.Equal(at) {
				t.Fatal("patch overwrote recorded response time")
			}
		default:
			if event.ProxyStatusCode == nil || *event.ProxyStatusCode != 204 || event.ProxyError != nil || event.RespondedAt == nil || event.RespondedAt.IsZero() || event.RespondedAt.Location() != time.UTC {
				t.Fatalf("default patch=%+v", event)
			}
		}
	}
}

func TestCoreImportPreservesRecordedPricingAndOwnership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	if err := s.WriteBatch(ctx, loadFixtureEvents(t)); err != nil {
		t.Fatal(err)
	}
	recorded, err := s.loadEvents(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	// Change the current book. Imported valid historical snapshots retain their price.
	s.config.PriceBook.Rules = nil
	for i := range recorded {
		id, err := model.NewCorrelationID()
		if err != nil {
			t.Fatal(err)
		}
		recorded[i].AttemptID = id
	}
	count, err := s.WriteImportBatch(ctx, recorded, "legacy-test")
	if err != nil || count != 3 {
		t.Fatalf("import count=%d err=%v", count, err)
	}
	count, err = s.WriteImportBatch(ctx, recorded, "second-import")
	if err != nil || count != 0 {
		t.Fatalf("import replay count=%d err=%v", count, err)
	}
	got, err := s.loadEvents(ctx, "import_batch_id=?", "legacy-test")
	if err != nil || len(got) != 3 {
		t.Fatalf("import lookup count=%d err=%v", len(got), err)
	}
	for _, event := range got {
		for _, want := range recorded {
			if event.AttemptID == want.AttemptID && want.KnownCost != nil {
				if event.KnownCost == nil || *event.KnownCost != *want.KnownCost || event.PriceRuleID != want.PriceRuleID || !reflect.DeepEqual(event.PricingBreakdown, want.PricingBreakdown) {
					t.Fatalf("recorded price changed: got=%+v want=%+v", event, want)
				}
			}
		}
	}
	if _, err := s.WriteImportBatch(ctx, recorded, ""); err == nil {
		t.Fatal("blank batch accepted")
	}
}

func TestCoreReopenRestoresPricingAndRetentionFloor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handle := chtest.Handle(t)
	config := Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20}
	s, err := Open(ctx, handle, config)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := PricingSnapshot{Rules: fixturePriceBook().Rules}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.replaceControl(ctx, "pricing", map[string]json.RawMessage{"snapshot": raw}); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if err := s.setMetadata(ctx, "retention_raw_cutoff_ns", strconv.FormatInt(cutoff.UnixNano(), 10)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, handle, config)
	if err != nil {
		t.Fatal(err)
	}
	if !s.retentionCutoff.Equal(cutoff) {
		t.Fatalf("restored cutoff=%v want=%v", s.retentionCutoff, cutoff)
	}
	if err := s.WriteBatch(ctx, loadFixtureEvents(t)); err != nil {
		t.Fatal(err)
	}
	events, err := s.loadEvents(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var cost model.NanoUSD
	for _, event := range events {
		if event.KnownCost != nil {
			cost += *event.KnownCost
		}
	}
	if cost.String() != "0.002157525" {
		t.Fatalf("startup pricing=%s", cost.String())
	}
}
