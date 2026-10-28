package chstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	sqlstore "github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
)

func TestEventReadsMatchSQLite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	codec, err := model.NewCursorCodec([]byte("12345678901234567890123456789012"))
	if err != nil {
		t.Fatal(err)
	}
	ch := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "ch.db"), MaxStorageBytes: 64 << 20, PriceBook: fixturePriceBook(), CursorCodec: codec})
	sqlitePath := filepath.Join(t.TempDir(), "sqlite.db")
	sqlite, err := sqlstore.Open(ctx, Config{Path: sqlitePath, MaxStorageBytes: 64 << 20, PriceBook: fixturePriceBook(), CursorCodec: codec})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlite.Close(ctx) })
	legacy, err := sql.Open("sqlite", sqlitePath)
	if err != nil {
		t.Fatal(err)
	}
	var captureSince int64
	if err := legacy.QueryRow("SELECT applied_at_ns FROM schema_migrations WHERE version=18").Scan(&captureSince); err != nil {
		t.Fatal(err)
	}
	_ = legacy.Close()
	if err := ch.setMetadata(ctx, "session_capture_since_ns", strconv.FormatInt(captureSince, 10)); err != nil {
		t.Fatal(err)
	}

	events := loadFixtureEvents(t)
	if err := ch.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	if err := sqlite.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	patches := []model.ProxyResponsePatch{{ProxyRequestID: events[0].ProxyRequestID, StatusCode: 502, Error: "upstream failed", RespondedAt: events[0].RequestedAt.Add(time.Second)}, {ProxyRequestID: events[0].ProxyRequestID, StatusCode: 200, RespondedAt: events[0].RequestedAt.Add(2 * time.Second)}}
	if err := ch.WriteBatchWithPatches(ctx, nil, patches); err != nil {
		t.Fatal(err)
	}
	if err := sqlite.WriteBatchWithPatches(ctx, nil, patches); err != nil {
		t.Fatal(err)
	}
	start, end := events[0].RequestedAt, events[0].RequestedAt
	for _, event := range events {
		if event.RequestedAt.Before(start) {
			start = event.RequestedAt
		}
		if event.RequestedAt.After(end) {
			end = event.RequestedAt
		}
	}
	query := model.Query{SchemaVersion: 2, Operation: model.OperationEvents, Start: start.Add(-time.Second), End: end.Add(time.Second), TimeZone: "UTC", PageSize: 2}
	for _, filter := range []map[string]json.RawMessage{nil, {"provider": json.RawMessage(`["` + events[0].Provider + `"]`)}, {"success": json.RawMessage(`true`)}, {"result": json.RawMessage(`"failure"`)}} {
		q := query
		q.Filters = filter
		for {
			got, e1 := ch.Events(ctx, q)
			want, e2 := sqlite.Events(ctx, q)
			if e1 != nil || e2 != nil {
				t.Fatalf("events errors: CH %v SQLite %v", e1, e2)
			}
			gotCursor, wantCursor := got.Meta.NextCursor, want.Meta.NextCursor
			got.Meta.NextCursor, want.Meta.NextCursor = "", ""
			if (gotCursor == "") != (wantCursor == "") {
				t.Fatal("cursor presence mismatch")
			}
			if gotCursor != "" {
				a, err := codec.Decode(gotCursor)
				if err != nil {
					t.Fatal(err)
				}
				b, err := codec.Decode(wantCursor)
				if err != nil || !reflect.DeepEqual(a, b) {
					t.Fatal("cursor contents mismatch")
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("events mismatch filter %s\nCH: %+v\nSQLite: %+v", filter, got, want)
			}
			if gotCursor == "" {
				break
			}
			q.Cursor = gotCursor
		}
	}
	for _, e := range events {
		got, found, e1 := ch.EventByAttemptID(ctx, e.AttemptID, query)
		want, ok, e2 := sqlite.EventByAttemptID(ctx, e.AttemptID, query)
		if e1 != nil || e2 != nil || found != ok || !reflect.DeepEqual(got, want) {
			t.Fatalf("lookup %s mismatch\nCH: %+v %v %v\nSQLite: %+v %v %v", e.AttemptID, got, found, e1, want, ok, e2)
		}
	}
}
