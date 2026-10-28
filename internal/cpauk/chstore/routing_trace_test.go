package chstore

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestRoutingTraceReadContractAndRewrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	events := loadFixtureEvents(t)[:2]
	trace := json.RawMessage(`{"version":1,"mode":"local","retry_round":0,"selection":0,"decided_by":"strategy","candidates":[{"id":"` + strings.Repeat("a", 64) + `","provider":"openai"}],"pick":0}`)
	events[0].RoutingTrace = trace
	q := model.Query{SchemaVersion: 2, Operation: model.OperationEvents, Start: events[0].RequestedAt.Add(-time.Hour), End: events[1].RequestedAt.Add(time.Hour), TimeZone: "UTC", PageSize: 50}
	if err := s.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T) {
		t.Helper()
		for i, event := range events {
			detail, found, err := s.EventByAttemptID(ctx, event.AttemptID, q)
			if err != nil || !found || string(detail.RoutingTrace) != string(event.RoutingTrace) {
				t.Fatalf("detail %d trace=%s found=%t err=%v", i, detail.RoutingTrace, found, err)
			}
			var column *string
			if err := s.db.Conn().QueryRow(ctx, "SELECT routing_trace_json FROM "+s.table("analytics_events")+" FINAL WHERE is_deleted=0 AND attempt_id=?", event.AttemptID).Scan(&column); err != nil {
				t.Fatal(err)
			}
			if i == 0 && (column == nil || *column != string(trace)) || i == 1 && column != nil {
				t.Fatalf("scalar trace %d = %v", i, column)
			}
		}
	}
	t.Run("write and detail", check)
	t.Run("list omits trace", func(t *testing.T) {
		page, err := s.Events(ctx, q)
		if err != nil || len(page.Events) != 2 {
			t.Fatalf("list count=%d err=%v", len(page.Events), err)
		}
		for _, event := range page.Events {
			if len(event.RoutingTrace) != 0 {
				t.Fatalf("list returned trace: %s", event.RoutingTrace)
			}
		}
	})
	t.Run("export includes trace", func(t *testing.T) {
		page, err := s.EventsForExport(ctx, q)
		if err != nil || len(page.Events) != 2 {
			t.Fatalf("export count=%d err=%v", len(page.Events), err)
		}
		for _, event := range page.Events {
			if event.AttemptID == events[0].AttemptID && string(event.RoutingTrace) != string(trace) {
				t.Fatalf("export trace=%s", event.RoutingTrace)
			}
		}
	})
	t.Run("outcome patch", func(t *testing.T) {
		if err := s.WriteBatchWithPatches(ctx, nil, []model.ProxyResponsePatch{{ProxyRequestID: events[0].ProxyRequestID, StatusCode: 200, RespondedAt: events[0].RequestedAt.Add(time.Second)}}); err != nil {
			t.Fatal(err)
		}
		check(t)
		detail, _, err := s.EventByAttemptID(ctx, events[0].AttemptID, q)
		if err != nil || detail.ProxyStatusCode == nil || *detail.ProxyStatusCode != 200 {
			t.Fatalf("patch not applied: %+v err=%v", detail, err)
		}
	})
	t.Run("reprice", func(t *testing.T) {
		result, err := s.Reprice(ctx, RepriceOptions{Range: model.Range{Start: q.Start, End: q.End, TimeZone: q.TimeZone}}, nil)
		if err != nil || result.Updated != 2 {
			t.Fatalf("reprice=%+v err=%v", result, err)
		}
		check(t)
	})
	t.Run("backup restore publication", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "backup.json")
		if _, err := s.Backup(ctx, path); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PurgeByKeyID(ctx, events[0].KeyID); err != nil {
			t.Fatal(err)
		}
		if err := s.Restore(ctx, path, path+".manifest.json"); err != nil {
			t.Fatal(err)
		}
		check(t)
	})
	t.Run("identity epoch archive", func(t *testing.T) {
		result, err := s.StartNewIdentityEpoch(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Restore(ctx, result.ArchivedDB, result.ArchivedDB+".manifest.json"); err != nil {
			t.Fatal(err)
		}
		check(t)
	})
}

func TestRoutingTraceLateStagingAndRetention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	event := loadFixtureEvents(t)[0]
	event.RoutingTrace = json.RawMessage(`{"version":1,"mode":"local","candidates":[],"pick":-1}`)
	cutoff := event.RequestedAt.AddDate(0, 0, 2).Truncate(24 * time.Hour)
	if _, err := s.ApplyRetentionPolicy(ctx, cutoff, cutoff.AddDate(0, 0, -1), 1); err != nil {
		t.Fatal(err)
	}
	interrupted := errors.New("stop after staging")
	for _, step := range []string{"late:source", "late:published"} {
		t.Run(step, func(t *testing.T) {
			stopped := context.WithValue(ctx, c4bStepKey{}, func(name string) error {
				if name == step {
					return interrupted
				}
				return nil
			})
			var err error
			if step == "late:source" {
				err = s.WriteBatch(stopped, []model.Event{event})
			} else {
				err = s.c4bRecoverLate(stopped)
			}
			if !errors.Is(err, interrupted) {
				t.Fatalf("expected interruption: %v", err)
			}
			var scalar, raw string
			if err := s.db.Conn().QueryRow(ctx, "SELECT assumeNotNull(routing_trace_json),event_json FROM "+s.table("analytics_events")+" FINAL WHERE is_deleted=0 AND attempt_id=?", event.AttemptID).Scan(&scalar, &raw); err != nil {
				t.Fatal(err)
			}
			var saved model.Event
			if err := json.Unmarshal([]byte(raw), &saved); err != nil {
				t.Fatal(err)
			}
			if scalar != string(event.RoutingTrace) || string(saved.RoutingTrace) != scalar {
				t.Fatalf("staged trace lost: scalar=%s event=%s", scalar, saved.RoutingTrace)
			}
		})
	}
	if err := s.c4bRecoverLate(ctx); err != nil {
		t.Fatal(err)
	}
	var deleted uint8
	var scalar *string
	var raw string
	if err := s.db.Conn().QueryRow(ctx, "SELECT is_deleted,routing_trace_json,event_json FROM "+s.table("analytics_events")+" FINAL WHERE attempt_id=?", event.AttemptID).Scan(&deleted, &scalar, &raw); err != nil {
		t.Fatal(err)
	}
	if deleted != 1 || scalar != nil || raw != "" {
		t.Fatalf("expired diagnostics retained in receipt: deleted=%d trace=%v event=%s", deleted, scalar, raw)
	}
}
