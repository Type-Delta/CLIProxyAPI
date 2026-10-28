package chstore

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
)

func TestC4RestoreRecoveryEveryExchange(t *testing.T) {
	t.Parallel()
	for stop := 0; stop <= len(analyticsTables)+1; stop++ {
		t.Run(fmt.Sprint(stop), func(t *testing.T) {
			ctx := context.Background()
			handle := chtest.Handle(t)
			cfg := Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PriceBook: fixturePriceBook()}
			s, err := Open(ctx, handle, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := RunLegacyImport(ctx, s, cfg.Path); err != nil {
				t.Fatal(err)
			}
			events := loadFixtureEvents(t)
			if err := s.WriteBatch(ctx, events); err != nil {
				t.Fatal(err)
			}
			oldKey, oldEpoch := s.IdentityKeyArray(), s.IdentityEpoch()
			path := filepath.Join(t.TempDir(), "backup")
			if _, err := s.Backup(ctx, path); err != nil {
				t.Fatal(err)
			}
			if _, err := s.StartNewIdentityEpoch(ctx); err != nil {
				t.Fatal(err)
			}
			ctxCrash := context.WithValue(ctx, restoreTestHookKey{}, func(count int) error {
				if count == stop {
					return fmt.Errorf("simulated crash")
				}
				return nil
			})
			if err := s.Restore(ctxCrash, path, path+".manifest.json"); err == nil {
				t.Fatal("restore did not stop")
			}
			reopened, err := Open(ctx, handle, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close(ctx) }()
			got, err := reopened.loadEvents(ctx, "")
			if err != nil || len(got) != len(events) {
				t.Fatalf("recovered events=%d err=%v", len(got), err)
			}
			if reopened.IdentityKeyArray() != oldKey || reopened.IdentityEpoch() != oldEpoch {
				t.Fatal("recovered identity differs from backup")
			}
			db, err := handle.Get(ctx)
			if err != nil {
				t.Fatal(err)
			}
			job, found, err := db.Job(ctx, restoreJob)
			if err != nil || !found || job.State != "done" {
				t.Fatalf("job=%+v found=%v err=%v", job, found, err)
			}
		})
	}
}

func TestC4ArchiveStreamingChunkBound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	if err := s.db.Conn().Exec(ctx, "INSERT INTO "+s.table("analytics_metadata")+" SELECT concat('chunk-',toString(number)),repeat('x',4096),toUInt64(number+1),toUInt8(0) FROM numbers(1000)"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "backup")
	if _, err := s.Backup(ctx, path); err != nil {
		t.Fatal(err)
	}
	peak, chunks := 0, 0
	ctx = context.WithValue(ctx, archiveChunkTestHookKey{}, func(size int) { peak = max(peak, size); chunks++ })
	if err := s.Restore(ctx, path, path+".manifest.json"); err != nil {
		t.Fatal(err)
	}
	if chunks < 4 || peak > archiveChunkBytes {
		t.Fatalf("chunks=%d peak=%d limit=%d", chunks, peak, archiveChunkBytes)
	}
	var count uint64
	if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM "+s.table("analytics_metadata")+" FINAL WHERE startsWith(key,'chunk-')").Scan(&count); err != nil || count != 1000 {
		t.Fatalf("rows=%d err=%v", count, err)
	}
}

func TestC4RestoreMissingStage(t *testing.T) {
	t.Parallel()
	for _, exchanged := range []int{0, 1} {
		t.Run(fmt.Sprint(exchanged), func(t *testing.T) {
			ctx := context.Background()
			handle := chtest.Handle(t)
			cfg := Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PriceBook: fixturePriceBook()}
			s, err := Open(ctx, handle, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := RunLegacyImport(ctx, s, cfg.Path); err != nil {
				t.Fatal(err)
			}
			events := loadFixtureEvents(t)
			if err := s.WriteBatch(ctx, events); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "backup")
			if _, err := s.Backup(ctx, path); err != nil {
				t.Fatal(err)
			}
			crash := context.WithValue(ctx, restoreTestHookKey{}, func(count int) error {
				if count == exchanged {
					return fmt.Errorf("interrupted")
				}
				return nil
			})
			if err := s.Restore(crash, path, path+".manifest.json"); err == nil {
				t.Fatal("expected interruption")
			}
			db, err := handle.Get(ctx)
			if err != nil {
				t.Fatal(err)
			}
			job, _, err := db.Job(ctx, restoreJob)
			if err != nil {
				t.Fatal(err)
			}
			var journal restoreJournal
			if err := json.Unmarshal([]byte(job.Detail), &journal); err != nil {
				t.Fatal(err)
			}
			tmpStore := &Store{db: db}
			if err := db.Conn().Exec(ctx, "DROP TABLE "+tmpStore.table(journal.Tables[1].Stage)+" SYNC"); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(ctx, handle, cfg)
			if exchanged == 1 {
				if err == nil {
					_ = reopened.Close(ctx)
					t.Fatal("mixed state must fail closed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close(ctx) }()
			job, _, err = db.Job(ctx, restoreJob)
			if err != nil || job.State != "failed" {
				t.Fatalf("job=%+v err=%v", job, err)
			}
			got, err := reopened.loadEvents(ctx, "")
			if err != nil || len(got) != len(events) {
				t.Fatalf("events=%d err=%v", len(got), err)
			}
		})
	}
}
