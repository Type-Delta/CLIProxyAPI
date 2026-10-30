package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/transcript"
)

var ErrNotFound = errors.New("telemetry request not found")

type ErrorLog struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"`
}

func table(db *clickhouse.DB, name string) string { return "`" + db.Database() + "`." + name }
func ErrorLogs(ctx context.Context, db *clickhouse.DB) ([]ErrorLog, error) {
	// Report uncompressed payload bytes in one query. The downloaded legacy
	// rendering adds section framing, so its final byte length can differ.
	query := "WITH error_requests AS (SELECT * FROM " + table(db, "requests") + " FINAL WHERE detail='error') " +
		"SELECT r.request_id,r.started_at,length(r.response_body)+length(r.request_body_raw)+length(r.request_headers)+length(r.response_headers)+length(r.websocket_timeline)+ifNull(c.bytes,0)+ifNull(a.bytes,0) " +
		"FROM error_requests r LEFT JOIN (" +
		"SELECT x.request_id,sum(c.bytes) AS bytes FROM (SELECT request_id,arrayJoin(content_ids) AS content_id FROM " + table(db, "request_contexts") + " FINAL WHERE request_id IN (SELECT request_id FROM error_requests)) x " +
		"INNER JOIN (SELECT content_id,bytes FROM " + table(db, "transcript_content") + " FINAL) c ON x.content_id=c.content_id GROUP BY x.request_id) c ON r.request_id=c.request_id " +
		"LEFT JOIN (SELECT request_id,sum(length(response_body)+length(request_body_raw)+length(request_headers)+length(response_headers)+length(websocket_timeline)) AS bytes FROM " + table(db, "attempts") + " FINAL WHERE request_id IN (SELECT request_id FROM error_requests) GROUP BY request_id) a ON r.request_id=a.request_id ORDER BY r.started_at DESC"
	rows, err := db.Conn().Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ErrorLog{}
	for rows.Next() {
		var id string
		var at time.Time
		var size uint64
		if err = rows.Scan(&id, &at, &size); err != nil {
			return nil, err
		}
		out = append(out, ErrorLog{Name: "error-" + id + ".log", Size: int64(size), Modified: at.Unix()})
	}
	return out, rows.Err()
}
func headers(raw string) map[string][]string {
	var h map[string][]string
	_ = json.Unmarshal([]byte(raw), &h)
	return h
}
func value(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// Request retrieves a record and reconstructs each request body from its exact context content.
func Request(ctx context.Context, db *clickhouse.DB, id string) (*record.Request, error) {
	var r record.Request
	var first, finished *time.Time
	var reqHeaders, respHeaders, envelope, raw, response, timeline string
	var status uint16
	q := "SELECT request_id,proxy_request_id,started_at,first_chunk_at,finished_at,method,url,client_format,transport,actor_key_id,session_id,detail,source,request_headers,response_headers,envelope_content_id,request_body_raw,status,response_body,response_truncated,websocket_timeline,errors FROM " + table(db, "requests") + " FINAL WHERE "
	scan := func(query string, args ...any) error {
		return db.Conn().QueryRow(ctx, q+query, args...).Scan(&r.RequestID, &r.ProxyRequestID, &r.StartedAt, &first, &finished, &r.Method, &r.URL, &r.ClientFormat, &r.Transport, &r.ActorKeyID, &r.SessionID, &r.Detail, &r.Source, &reqHeaders, &respHeaders, &envelope, &raw, &status, &response, &r.ResponseTruncated, &timeline, &r.Errors)
	}
	err := scan("request_id = ? ORDER BY started_at DESC LIMIT 1 SETTINGS use_skip_indexes_if_final=1", id)
	if errors.Is(err, sql.ErrNoRows) {
		err = scan("request_id = ? OR endsWith(request_id, ?) OR proxy_request_id = ? ORDER BY (request_id = ?) DESC, started_at DESC LIMIT 1", id, id, id, id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.FirstChunkAt = value(first)
	r.FinishedAt = value(finished)
	r.Headers = headers(reqHeaders)
	r.ResponseHeaders = headers(respHeaders)
	r.Status = int(status)
	r.Response = []byte(response)
	r.WebsocketTimeline = []byte(timeline)
	if envelope == "" {
		r.Body = []byte(raw)
	} else {
		r.Body, err = rebuild(ctx, db, r.RequestID, 0, transcript.Format(r.ClientFormat), envelope)
		if err != nil {
			return nil, err
		}
	}
	rows, err := db.Conn().Query(ctx, "SELECT attempt_index,started_at,response_at,method,url,upstream_format,transport,provider,auth_id,auth_label,auth_type,request_headers,envelope_content_id,request_body_raw,status,response_headers,response_body,response_truncated,websocket_timeline,errors FROM "+table(db, "attempts")+" FINAL WHERE request_id=? ORDER BY attempt_index", r.RequestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type pending struct {
		index         int
		envelope, raw string
	}
	var pendingBodies []pending
	for rows.Next() {
		var a record.Attempt
		var idx uint32
		var at *time.Time
		var h, rh, env, body, resp, ws string
		var code uint16
		if err = rows.Scan(&idx, &a.StartedAt, &at, &a.Method, &a.URL, &a.UpstreamFormat, &a.Transport, &a.Provider, &a.AuthID, &a.AuthLabel, &a.AuthType, &h, &env, &body, &code, &rh, &resp, &a.ResponseTruncated, &ws, &a.Errors); err != nil {
			return nil, err
		}
		a.Index = int(idx)
		a.ResponseAt = value(at)
		a.Headers = headers(h)
		a.ResponseHeaders = headers(rh)
		a.Status = int(code)
		a.Response = []byte(resp)
		a.WebsocketTimeline = []byte(ws)
		pendingBodies = append(pendingBodies, pending{len(r.Attempts), env, body})
		r.Attempts = append(r.Attempts, a)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	_ = rows.Close()
	for _, p := range pendingBodies {
		a := &r.Attempts[p.index]
		if p.envelope == "" {
			a.Body = []byte(p.raw)
		} else {
			a.Body, err = rebuild(ctx, db, r.RequestID, a.Index, transcript.Format(a.UpstreamFormat), p.envelope)
			if err != nil {
				return nil, err
			}
		}
	}
	return &r, nil
}
func rebuild(ctx context.Context, db *clickhouse.DB, id string, attempt int, f transcript.Format, envelopeID string) ([]byte, error) {
	var ids []string
	err := db.Conn().QueryRow(ctx, "SELECT content_ids FROM "+table(db, "request_contexts")+" FINAL WHERE request_id=? AND attempt_index=? LIMIT 1 SETTINGS use_skip_indexes_if_final=1", id, uint32(attempt)).Scan(&ids)
	if err != nil {
		return nil, fmt.Errorf("read transcript context: %w", err)
	}
	wanted := append(append([]string{}, ids...), envelopeID)
	rows, err := db.Conn().Query(ctx, "SELECT content_id,raw FROM "+table(db, "transcript_content")+" FINAL WHERE content_id IN (?)", wanted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	content := make(map[string][]byte, len(wanted))
	for rows.Next() {
		var contentID, raw string
		if err = rows.Scan(&contentID, &raw); err != nil {
			return nil, err
		}
		content[contentID] = []byte(raw)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	envelope, ok := content[envelopeID]
	if !ok {
		return nil, fmt.Errorf("read transcript envelope %s: %w", envelopeID, ErrNotFound)
	}
	raws := make([][]byte, len(ids))
	for n, contentID := range ids {
		var ok bool
		raws[n], ok = content[contentID]
		if !ok {
			return nil, fmt.Errorf("read transcript content %s: %w", contentID, ErrNotFound)
		}
	}
	out, err := transcript.Rebuild(f, envelope, raws)
	if err != nil {
		return nil, fmt.Errorf("rebuild %s: %w", strings.TrimSpace(id), err)
	}
	return out, nil
}
