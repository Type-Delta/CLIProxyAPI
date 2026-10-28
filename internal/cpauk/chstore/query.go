package chstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func (s *Store) validateQuery(q *model.Query, operation model.Operation) error {
	if q == nil || q.Operation != operation {
		return fmt.Errorf("analytics query operation must be %s", operation)
	}
	if err := q.Validate(); err != nil {
		return err
	}
	if q.Cursor != "" {
		return q.ValidateCursor(s.config.CursorCodec, model.CursorTransportBody)
	}
	return nil
}

func responseMeta(q model.Query) model.ResponseMeta {
	return model.ResponseMeta{SchemaVersion: q.SchemaVersion, Range: model.Range{Start: q.Start, End: q.End, TimeZone: q.TimeZone}}
}

// selectedEventPage decodes only the SQL-bound detail page. Response columns are
// already patched by rawSelection; JSON is never used for filtering.
func (s *Store) selectedEventPage(ctx context.Context, selection readSelection, suffix string, args ...any) ([]model.Event, error) {
	rows, err := s.db.Conn().Query(ctx, selection.SQL+suffix, append(selection.Args, args...)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var events []model.Event
	for rows.Next() {
		var raw string
		var status, at *int64
		var message, sessionID *string
		var captured bool
		if err := rows.Scan(&raw, &status, &message, &at, &captured, &sessionID); err != nil {
			return nil, err
		}
		var event model.Event
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			return nil, err
		}
		if string(event.RoutingTrace) == "null" {
			event.RoutingTrace = nil
		}
		event.ProxyStatusCode = nil
		if status != nil {
			v := int(*status)
			event.ProxyStatusCode = &v
		}
		event.ProxyError = message
		event.RespondedAt = nil
		if at != nil {
			v := time.Unix(0, *at).UTC()
			event.RespondedAt = &v
		}
		event.SessionCaptured = captured
		event.SessionID = sessionID
		event.SessionSource = nil
		if sessionID != nil {
			id, source := model.SplitSessionIdentity(*sessionID)
			if model.IsDerivedSessionIdentity(*sessionID) {
				source = "derived"
			}
			event.SessionID = &id
			if source != "" {
				event.SessionSource = &source
			}
		}
		if event.ImportBatchID == "" {
			event.Source = "native"
		} else {
			event.Source = "import"
		}
		model.ClassifyEvent(&event)
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) Events(ctx context.Context, query model.Query) (model.EventPage, error) {
	return s.events(ctx, query, false)
}

// EventsForExport includes the persisted routing trace, without resolving live labels.
func (s *Store) EventsForExport(ctx context.Context, query model.Query) (model.EventPage, error) {
	return s.events(ctx, query, true)
}

func (s *Store) events(ctx context.Context, query model.Query, includeTrace bool) (model.EventPage, error) {
	if err := s.validateQuery(&query, model.OperationEvents); err != nil {
		return model.EventPage{}, err
	}
	selection, err := query.SelectionDigest()
	if err != nil {
		return model.EventPage{}, err
	}
	var cursor model.Cursor
	if query.Cursor != "" {
		cursor, err = s.config.CursorCodec.Decode(query.Cursor)
		if err != nil {
			return model.EventPage{}, err
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return model.EventPage{}, ErrClosed
	}
	bounds, err := s.readCutoffs(ctx)
	if err != nil {
		return model.EventPage{}, err
	}
	if bounds.rawSet && query.Start.UnixNano() < bounds.raw {
		return model.EventPage{}, RetainedRangeError{Cutoff: time.Unix(0, bounds.raw).UTC()}
	}
	selectionSQL, err := s.rawSelection(ctx, query, "event_json", "proxy_status_code", "proxy_error", "responded_at_ns", "session_captured", "session_id", "requested_at_ns", "attempt_id")
	if err != nil {
		return model.EventPage{}, err
	}
	countSQL, err := s.rawSelection(ctx, query, "attempt_id")
	if err != nil {
		return model.EventPage{}, err
	}
	var count uint64
	if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM ("+countSQL.SQL+")", countSQL.Args...).Scan(&count); err != nil {
		return model.EventPage{}, err
	}
	// Keep the page projection in the same foundation, without ordering-only columns.
	pageSQL := selectionSQL
	// Replace the trace in SQL so list pages never transfer or decode its payload.
	eventJSON := "JSONMergePatch(event_json,?)"
	if includeTrace {
		eventJSON = "event_json"
	} else {
		pageSQL.Args = append([]any{`{"routing_trace":null}`}, pageSQL.Args...)
	}
	pageSQL.SQL = "SELECT " + eventJSON + ",proxy_status_code,proxy_error,responded_at_ns,session_captured,session_id FROM (" + selectionSQL.SQL + ")"
	suffix := ""
	var pageArgs []any
	if cursor.RequestedAt != nil {
		suffix = " WHERE requested_at_ns < ? OR (requested_at_ns = ? AND attempt_id > ?)"
		pageArgs = append(pageArgs, cursor.RequestedAt.UnixNano(), cursor.RequestedAt.UnixNano(), cursor.AttemptID)
	}
	suffix += " ORDER BY requested_at_ns DESC,attempt_id ASC LIMIT ?"
	pageArgs = append(pageArgs, query.PageSize+1)
	events, err := s.selectedEventPage(ctx, pageSQL, suffix, pageArgs...)
	if err != nil {
		return model.EventPage{}, err
	}
	result := model.EventPage{Meta: responseMeta(query), TotalCount: int64(count)}
	hasMore := len(events) > query.PageSize
	if hasMore {
		events = events[:query.PageSize]
	}
	result.Events = events
	if hasMore {
		last := events[len(events)-1]
		result.Meta.NextCursor, err = s.config.CursorCodec.Encode(model.Cursor{Version: 1, Operation: model.OperationEvents, Selection: selection, RequestedAt: &last.RequestedAt, AttemptID: last.AttemptID})
		if err != nil {
			return model.EventPage{}, err
		}
	}
	s.enrichEvents(ctx, result.Events)
	return result, nil
}

func (s *Store) EventByAttemptID(ctx context.Context, attemptID string, query model.Query) (model.Event, bool, error) {
	if !model.IsCorrelationID(attemptID) {
		return model.Event{}, false, fmt.Errorf("invalid analytics attempt ID")
	}
	query.Cursor = ""
	if err := s.validateQuery(&query, model.OperationEvents); err != nil {
		return model.Event{}, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return model.Event{}, false, ErrClosed
	}
	bounds, err := s.readCutoffs(ctx)
	if err != nil {
		return model.Event{}, false, err
	}
	if bounds.rawSet && query.Start.UnixNano() < bounds.raw {
		return model.Event{}, false, RetainedRangeError{Cutoff: time.Unix(0, bounds.raw).UTC()}
	}
	// Global identity lookup uses the bloom index first. Pinning the subsequent
	// detail read to its exact timestamp also prunes the time-leading primary key.
	var requestedNS int64
	err = s.db.Conn().QueryRow(ctx, "SELECT requested_at_ns FROM "+s.table("analytics_events")+" FINAL PREWHERE attempt_id=? WHERE is_deleted=0 LIMIT 1 SETTINGS use_skip_indexes_if_final=1", attemptID).Scan(&requestedNS)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Event{}, false, nil
	}
	if err != nil {
		return model.Event{}, false, err
	}
	if requestedNS < query.Start.UnixNano() || requestedNS >= query.End.UnixNano() {
		return model.Event{}, false, nil
	}
	selectedQuery := query
	selectedQuery.Start = time.Unix(0, requestedNS).UTC()
	selectedQuery.End = selectedQuery.Start.Add(time.Nanosecond)
	selectionSQL, err := s.rawSelection(ctx, selectedQuery, "event_json", "proxy_status_code", "proxy_error", "responded_at_ns", "session_captured", "session_id", "attempt_id")
	if err != nil {
		return model.Event{}, false, err
	}
	selectionSQL.SQL = "SELECT event_json,proxy_status_code,proxy_error,responded_at_ns,session_captured,session_id FROM (" + selectionSQL.SQL + ") WHERE attempt_id=? LIMIT 1"
	selectionSQL.Args = append(selectionSQL.Args, attemptID)
	events, err := s.selectedEventPage(ctx, selectionSQL, "")
	if err != nil {
		return model.Event{}, false, err
	}
	if len(events) == 0 {
		return model.Event{}, false, nil
	}
	s.enrichEvents(ctx, events)
	return events[0], true, nil
}

func (s *Store) enrichEvents(ctx context.Context, events []model.Event) {
	s.enrichArrivalEstimates(ctx, events)
}
