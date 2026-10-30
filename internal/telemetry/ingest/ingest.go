// Package ingest drains captured requests into ClickHouse.
package ingest

import (
	"bytes"
	"container/list"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/outbox"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/store"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/transcript"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

var ErrPaused = record.ErrIngestPaused

type Status struct {
	Backlog      int
	LastError    string
	LastSuccess  time.Time
	OldestAge    time.Duration
	PausedReason string
}
type Ingester struct {
	store              *store.Store
	dir                string
	minFree            uint64
	mu                 sync.RWMutex
	status             Status
	retention          int
	processMu          sync.Mutex
	pending            map[string]*batchRows
	known              map[string]bool
	heads              map[string]batchHead
	cache              map[string]*list.Element
	lru                *list.List
	drainMu            sync.Mutex
	failedPath         string
	failedAttempts     int
	failedDataAttempts int
	nextIdleCheck      time.Time
	nowFunc            func() time.Time
}

func New(s *store.Store, dir string, minFree uint64, retentionDays int) *Ingester {
	return &Ingester{store: s, dir: dir, minFree: minFree, retention: retentionDays, cache: make(map[string]*list.Element), lru: list.New(), nowFunc: time.Now}
}

func (i *Ingester) Store() *store.Store { return i.store }

func (i *Ingester) Status() Status { i.mu.RLock(); defer i.mu.RUnlock(); return i.status }
func (i *Ingester) setStatus(n int, err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.status.Backlog = n
	if n == 0 {
		i.status.OldestAge = 0
	}
	if err != nil {
		i.status.LastError = err.Error()
		if errors.Is(err, ErrPaused) {
			i.status.PausedReason = err.Error()
		} else {
			i.status.PausedReason = ""
		}
	} else {
		i.status.LastError = ""
		i.status.PausedReason = ""
	}
}
func (i *Ingester) table(name string) string { return "`" + i.store.DB().Database() + "`." + name }

// Run retries pending files until cancelled and runs retention GC daily.
func (i *Ingester) Run(ctx context.Context) {
	i.collectGarbage(ctx)
	gc := time.NewTicker(24 * time.Hour)
	defer gc.Stop()
	delay := time.Second
	for {
		err := i.Drain(ctx)
		if err == nil {
			delay = time.Second
		} else if delay < time.Minute {
			delay *= 2
			if delay > time.Minute {
				delay = time.Minute
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-gc.C:
			timer.Stop()
			i.collectGarbage(ctx)
		}
	}
}

func (i *Ingester) collectGarbage(ctx context.Context) {
	i.processMu.Lock()
	err := i.store.GC(ctx, i.retention)
	i.cache = make(map[string]*list.Element)
	i.lru.Init()
	i.processMu.Unlock()
	if err != nil {
		i.setStatus(i.Status().Backlog, err)
	}
}

func (i *Ingester) SetMinFreeBytes(bytes uint64) {
	i.processMu.Lock()
	i.minFree = bytes
	i.processMu.Unlock()
}

// SetRetention applies a changed TTL and updates the GC cutoff.
func (i *Ingester) SetRetention(ctx context.Context, days int) error {
	i.processMu.Lock()
	defer i.processMu.Unlock()
	if err := i.store.SetRetention(ctx, days); err != nil {
		return err
	}
	i.retention = days
	return nil
}

// Drain processes complete files in filename order and quarantines rejected records.
func (i *Ingester) Drain(ctx context.Context) error {
	i.drainMu.Lock()
	defer i.drainMu.Unlock()
	paths, err := outbox.List(i.dir)
	if err != nil {
		i.setStatus(0, err)
		return err
	}
	i.mu.Lock()
	i.status.OldestAge = 0
	if len(paths) > 0 {
		if info, e := os.Stat(paths[0]); e == nil {
			i.status.OldestAge = time.Since(info.ModTime())
			if i.status.OldestAge < 0 {
				i.status.OldestAge = 0
			}
		}
	}
	i.mu.Unlock()
	remaining := len(paths)
	fail := func(err error) error { i.setStatus(remaining, err); return err }

	if len(paths) == 0 {
		return i.idleHealth(ctx)
	}
	// Try bounded batches. If a batch fails, retry the first file through the
	// normal per-file path so rejection and retry accounting stay precise.
	for len(paths) > 1 {
		deadline := time.Now().Add(50 * time.Millisecond)
		var records []*record.Request
		var bytes int64
		for _, path := range paths {
			if len(records) >= 100 || time.Now().After(deadline) {
				break
			}
			info, statErr := os.Stat(path)
			if statErr != nil || (len(records) > 0 && bytes+info.Size() > 8<<20) {
				break
			}
			r, readErr := outbox.Read(path)
			if readErr != nil {
				break
			}
			encoded, encodeErr := json.Marshal(r)
			if encodeErr != nil || bytes+int64(len(encoded)) > 8<<20 {
				break
			}
			records = append(records, r)
			bytes += int64(len(encoded))
		}
		if len(records) < 2 {
			break
		}
		if err = i.IngestRecords(ctx, records); err != nil {
			break
		}
		for _, path := range paths[:len(records)] {
			if err = os.Remove(path); err != nil {
				return fail(err)
			}
			remaining--
		}
		i.failedPath = ""
		i.failedAttempts = 0
		i.failedDataAttempts = 0
		paths = paths[len(records):]
		i.setStatus(remaining, nil)
	}
	for n, path := range paths {
		if err = ctx.Err(); err != nil {
			return fail(err)
		}
		if path != i.failedPath {
			i.failedPath = path
			i.failedAttempts = 0
			i.failedDataAttempts = 0
		}
		i.failedAttempts++
		r, readErr := outbox.Read(path)
		err = readErr
		if err == nil {
			err = i.IngestRecords(ctx, []*record.Request{r})
		}
		if err != nil {
			permanent := record.DataError(err)
			if permanent {
				i.failedDataAttempts++
			} else {
				i.failedDataAttempts = 0
			}
			log.WithError(err).Warnf("telemetry: outbox attempt %d for %s failed (deterministic_data=%t)", i.failedAttempts, path, permanent)
			if ctx.Err() != nil {
				return fail(ctx.Err())
			}
			if !permanent || readErr == nil && i.failedDataAttempts < 3 {
				return fail(fmt.Errorf("%w: attempt %d: %w", ErrPaused, i.failedAttempts, err))
			}
			if quarantineErr := outbox.Quarantine(path); quarantineErr != nil {
				return fail(errors.Join(err, quarantineErr))
			}
			log.WithError(err).Warnf("telemetry: quarantined rejected outbox record after %d attempt(s): %s", i.failedAttempts, path)
			i.failedPath = ""
			i.failedAttempts = 0
			i.failedDataAttempts = 0
			remaining = len(paths) - n - 1
			continue
		}
		if err = os.Remove(path); err != nil {
			return fail(err)
		}
		i.failedPath = ""
		i.failedAttempts = 0
		i.failedDataAttempts = 0
		remaining = len(paths) - n - 1
		i.setStatus(remaining, nil)
	}
	i.setStatus(0, nil)
	return nil
}

// IngestRecords synchronously imports records using the same availability checks as the outbox.
func (i *Ingester) IngestRecords(ctx context.Context, recs []*record.Request) error {
	i.processMu.Lock()
	defer i.processMu.Unlock()
	i.store.Lock()
	defer i.store.Unlock()

	if err := i.store.DB().Ping(ctx); err != nil {
		return fmt.Errorf("%w: clickhouse unavailable: %v", ErrPaused, err)
	}
	if i.minFree > 0 {
		var free uint64
		if err := i.store.DB().Conn().QueryRow(ctx, "SELECT min(free_space) FROM system.disks WHERE type != 'Web'").Scan(&free); err != nil {
			return fmt.Errorf("%w: disk check: %v", ErrPaused, err)
		}
		if free < i.minFree {
			return fmt.Errorf("%w: clickhouse free disk %d below minimum %d", ErrPaused, free, i.minFree)
		}
	}
	i.pending = make(map[string]*batchRows)
	i.known = make(map[string]bool)
	i.heads = make(map[string]batchHead)
	defer func() { i.pending = nil; i.known = nil; i.heads = nil }()
	for _, r := range recs {
		if err := i.process(ctx, r); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if record.DataError(err) {
				return err
			}
			return fmt.Errorf("%w: %w", ErrPaused, err)
		}
	}
	if err := i.flush(ctx); err != nil {
		i.cache = make(map[string]*list.Element)
		i.lru.Init()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if record.DataError(err) {
			return err
		}
		return fmt.Errorf("%w: %w", ErrPaused, err)
	}
	for key := range i.known {
		i.remember(key)
	}
	if len(recs) > 0 {
		i.mu.Lock()
		i.status.LastSuccess = i.nowFunc().UTC()
		i.mu.Unlock()
	}
	return nil
}

// idleHealth checks availability at most twice a minute without claiming a
// successful ingestion or querying disk capacity while no records are pending.
func (i *Ingester) idleHealth(ctx context.Context) error {
	i.processMu.Lock()
	defer i.processMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	now := i.nowFunc()
	if now.Before(i.nextIdleCheck) {
		if status := i.Status(); status.PausedReason != "" {
			return fmt.Errorf("%w: %s", ErrPaused, status.PausedReason)
		}
		return nil
	}
	err := i.store.DB().Ping(ctx)
	// A caller ending its request says nothing about database availability.
	// Do not postpone or poison the final drain under a fresh shutdown context.
	if canceled := ctx.Err(); canceled != nil {
		return canceled
	}
	i.nextIdleCheck = i.nowFunc().Add(30 * time.Second)
	if err != nil {
		err = fmt.Errorf("%w: idle health: %w", ErrPaused, err)
	}
	i.setStatus(0, err)
	return err
}

func jsonString(v any) string { b, _ := json.Marshal(v); return string(b) }
func nullable(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}
func actual(t, fallback time.Time) time.Time {
	if t.IsZero() {
		return fallback
	}
	return t.UTC()
}

type batchRows struct {
	cols string
	rows [][]any
}

func (i *Ingester) insert(ctx context.Context, table string, cols string, vals ...any) error {
	if i.pending == nil {
		return fmt.Errorf("telemetry: insertion outside batch")
	}
	b := i.pending[table]
	if b == nil {
		b = &batchRows{cols: cols}
		i.pending[table] = b
	}
	b.rows = append(b.rows, vals)
	return nil
}
func (i *Ingester) flush(ctx context.Context) error {
	for _, table := range []string{"transcript_content", "transcript_entries", "request_contexts", "attempts", "requests"} {
		b := i.pending[table]
		if b == nil {
			continue
		}
		if table == "transcript_content" || table == "transcript_entries" {
			col := "content_id"
			if table == "transcript_entries" {
				col = "entry_id"
			}
			ids := make([]string, len(b.rows))
			for n, row := range b.rows {
				ids[n] = row[0].(string)
			}
			rows, err := i.store.DB().Conn().Query(ctx, "SELECT "+col+" FROM "+i.table(table)+" WHERE "+col+" IN (?)", ids)
			if err != nil {
				return err
			}
			existing := make(map[string]bool)
			for rows.Next() {
				var id string
				if err = rows.Scan(&id); err != nil {
					_ = rows.Close()
					return err
				}
				existing[id] = true
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return err
			}
			filtered := b.rows[:0]
			for _, row := range b.rows {
				if !existing[row[0].(string)] {
					filtered = append(filtered, row)
				}
			}
			b.rows = filtered
			if len(b.rows) == 0 {
				continue
			}
		}
		batch, err := i.store.DB().Conn().PrepareBatch(ctx, "INSERT INTO "+i.table(table)+" ("+b.cols+")")
		if err != nil {
			return fmt.Errorf("prepare %s: %w", table, err)
		}
		for _, row := range b.rows {
			if err = batch.Append(row...); err != nil {
				_ = batch.Abort()
				return fmt.Errorf("append %s: %w", table, err)
			}
		}
		if err = batch.Send(); err != nil {
			return fmt.Errorf("write %s: %w", table, err)
		}
	}
	return nil
}
func (i *Ingester) remember(key string) {
	if e := i.cache[key]; e != nil {
		i.lru.MoveToFront(e)
		return
	}
	i.cache[key] = i.lru.PushFront(key)
	if i.lru.Len() > 16384 {
		e := i.lru.Back()
		delete(i.cache, e.Value.(string))
		i.lru.Remove(e)
	}
}
func (i *Ingester) exists(ctx context.Context, table, col, id string) (bool, error) {
	key := table + ":" + id
	if i.known[key] {
		return true, nil
	}
	if e := i.cache[key]; e != nil {
		i.lru.MoveToFront(e)
		return true, nil
	}
	if table == "transcript_content" || table == "transcript_entries" {
		return false, nil
	}
	var count uint64
	err := i.store.DB().Conn().QueryRow(ctx, "SELECT count() FROM "+i.table(table)+" WHERE "+col+" = ?", id).Scan(&count)
	if err == nil && count > 0 {
		i.known[key] = true
	}
	return count > 0, err
}
func (i *Ingester) rowExists(ctx context.Context, table, condition string, args ...any) (bool, error) {
	var n uint64
	err := i.store.DB().Conn().QueryRow(ctx, "SELECT count() FROM "+i.table(table)+" WHERE "+condition, args...).Scan(&n)
	return n > 0, err
}
func (i *Ingester) content(ctx context.Context, id, kind string, raw []byte, text string, at time.Time) error {
	exists, err := i.exists(ctx, "transcript_content", "content_id", id)
	if err != nil || exists {
		return err
	}
	err = i.insert(ctx, "transcript_content", "content_id,kind,raw,text,bytes,first_observed_at", id, kind, string(raw), text, uint64(len(raw)), at)
	if err == nil {
		i.known["transcript_content:"+id] = true
	}
	return err
}

// Process writes a captured request. ReplacingMergeTree keys make retries safe.
func (i *Ingester) Process(ctx context.Context, r *record.Request) error {
	return i.IngestRecords(ctx, []*record.Request{r})
}

func (i *Ingester) process(ctx context.Context, r *record.Request) error {
	if r == nil || r.RequestID == "" {
		return fmt.Errorf("%w: missing request ID", record.ErrInvalidRecord)
	}
	already, err := i.exists(ctx, "requests", "request_id", r.RequestID)
	if err != nil {
		return err
	}
	if already {
		return nil
	}
	at := actual(r.StartedAt, time.Now().UTC())
	path := r.URL
	// Response framing remains separate from the client's requested delivery mode.
	requestedStream := r.Transport == record.TransportWebsocket || len(r.WebsocketTimeline) > 0
	if u, e := url.Parse(r.URL); e == nil {
		path = u.Path
		requestedStream = requestedStream || strings.HasSuffix(path, ":streamGenerateContent") || u.Query().Get("alt") == "sse"
	}
	f := transcript.Format(r.ClientFormat)
	if f == "" {
		f = transcript.DetectFormat(path, r.Body)
	}
	clientBody, clientResponse, clientStream := timelinePayload(r.Body, r.Response, r.Streaming, r.WebsocketTimeline)
	requestedStream = requestedStream || gjson.GetBytes(clientBody, "stream").Bool()
	if f == "" {
		f = transcript.DetectFormat(path, clientBody)
	}
	view, err := i.processView(ctx, r, 0, f, clientBody, clientResponse, clientStream, at, actual(r.FinishedAt, at))
	if err != nil {
		return err
	}
	for n := range r.Attempts {
		a := &r.Attempts[n]
		idx := a.Index
		if idx <= 0 {
			idx = n + 1
		}
		attemptBody, attemptResponse, attemptStream := timelinePayload(a.Body, a.Response, r.Streaming, a.WebsocketTimeline)
		af := transcript.Format(a.UpstreamFormat)
		if af == "" {
			af = transcript.DetectFormat(a.URL, attemptBody)
		}
		av, e := i.processView(ctx, r, idx, af, attemptBody, attemptResponse, attemptStream, actual(a.StartedAt, at), actual(a.ResponseAt, at))
		if e != nil {
			return e
		}
		existsAttempt, e := i.rowExists(ctx, "attempts", "request_id = ? AND attempt_index = ?", r.RequestID, uint32(idx))
		if e != nil {
			return e
		}
		if !existsAttempt {
			e = i.insert(ctx, "attempts", "request_id,attempt_index,request_started_at,started_at,response_at,method,url,upstream_format,provider,auth_id,auth_label,auth_type,transport,request_headers,envelope_content_id,request_body_raw,status,response_headers,response_body,response_truncated,websocket_timeline,errors,response_id", r.RequestID, uint32(idx), at, actual(a.StartedAt, at), nullable(a.ResponseAt), a.Method, a.URL, string(af), a.Provider, a.AuthID, a.AuthLabel, a.AuthType, a.Transport, jsonString(a.Headers), av.envelope, string(av.raw), uint16(a.Status), jsonString(a.ResponseHeaders), string(a.Response), a.ResponseTruncated, string(a.WebsocketTimeline), a.Errors, av.responseID)
		}

		if e != nil {
			return e
		}
	}
	i.known["requests:"+r.RequestID] = true
	return i.insert(ctx, "requests", "request_id,proxy_request_id,started_at,first_chunk_at,finished_at,method,url,path,client_format,model,stream,status,actor_key_id,session_id,detail,source,transport,request_headers,response_headers,envelope_content_id,response_id,response_body,response_truncated,websocket_timeline,errors,request_body_raw,ingested_at", r.RequestID, r.ProxyRequestID, at, nullable(r.FirstChunkAt), nullable(r.FinishedAt), r.Method, r.URL, path, string(f), view.model, requestedStream, uint16(r.Status), r.ActorKeyID, r.SessionID, r.Detail, r.Source, r.Transport, jsonString(r.Headers), jsonString(r.ResponseHeaders), view.envelope, view.responseID, string(r.Response), r.ResponseTruncated, string(r.WebsocketTimeline), r.Errors, string(view.raw), time.Now().UTC())
}

// timelinePayload extracts complete JSON websocket frames when bodies were not captured.
func timelinePayload(body, response []byte, stream bool, timeline []byte) ([]byte, []byte, bool) {
	if len(timeline) == 0 {
		return body, response, stream
	}
	var replies [][]byte
	for _, line := range bytes.Split(timeline, []byte("\n")) {
		var event struct {
			Event   string `json:"event"`
			Payload string `json:"payload"`
		}
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		if event.Event == "request" && len(body) == 0 {
			body = []byte(event.Payload)
		}
		if event.Event == "response" && json.Valid([]byte(event.Payload)) {
			replies = append(replies, []byte(event.Payload))
		}
	}
	if len(replies) > 0 {
		response = bytes.Join(replies, []byte("\n"))
		stream = true
	}
	return body, response, stream
}

type viewResult struct {
	envelope, model, responseID string
	raw                         []byte
}

func (i *Ingester) processView(ctx context.Context, r *record.Request, index int, f transcript.Format, body, response []byte, stream bool, at, responseAt time.Time) (viewResult, error) {
	parsed, err := transcript.ParseRequest(f, body)
	if err != nil {
		return viewResult{raw: body}, nil
	}
	out := viewResult{envelope: parsed.EnvelopeID, model: parsed.Model}
	if err = i.content(ctx, parsed.EnvelopeID, "envelope", parsed.Envelope, "", at); err != nil {
		return out, err
	}
	parent := transcript.RootEntryID(r.ActorKeyID, f)
	depth := uint32(0)
	// A continuation starts at the head of the referenced response.
	if parsed.PreviousResponseID != "" {
		parent, depth, err = i.previousHead(ctx, r.ActorKeyID, f, parsed.PreviousResponseID, parent, index)
		if err != nil {
			return out, err
		}
	}
	ids := make([]string, 0, len(parsed.Elements))
	contentIDs := make([]string, 0, len(parsed.Elements))
	tail := transcript.TailStart(parsed.Elements)
	for n, e := range parsed.Elements {
		var occurred *time.Time
		if n >= tail {
			occurred = &at
		}
		id, errEntry := i.entry(ctx, r, index, f, parent, depth, e, "request", at, occurred)
		err = errEntry
		if err != nil {
			return out, err
		}
		if id != "" {
			parent = id
			depth++
		}
		ids = append(ids, id)
		contentIDs = append(contentIDs, e.ContentID)
	}
	view := "client"
	if index > 0 {
		view = "upstream"
	}
	resp, parseErr := transcript.ParseResponse(f, response, stream)
	if parseErr != nil {
		resp, parseErr = transcript.ParseResponse(f, response, !stream)
	}
	if parseErr == nil {
		out.responseID = resp.ResponseID
		for _, e := range resp.Elements {
			id, errEntry := i.entry(ctx, r, index, f, parent, depth, e, "response", at, &responseAt)
			err = errEntry
			if err != nil {
				return out, err
			}
			if id != "" {
				parent = id
				depth++
			}
		}
	}
	existsContext, err := i.rowExists(ctx, "request_contexts", "request_id = ? AND attempt_index = ? AND view = ?", r.RequestID, uint32(index), view)
	if err != nil {
		return out, err
	}
	if !existsContext {
		err = i.insert(ctx, "request_contexts", "request_id,attempt_index,view,session_id,started_at,entry_ids,content_ids,response_head_id,response_depth", r.RequestID, uint32(index), view, r.SessionID, actual(r.StartedAt, at), ids, contentIDs, parent, depth)
		if err != nil {
			return out, err
		}
	}

	if out.responseID != "" {
		key := headKey(r.ActorKeyID, f, out.responseID, index)
		old, ok := i.heads[key]
		started := actual(r.StartedAt, at)
		if !ok || started.After(old.started) {
			i.heads[key] = batchHead{parent, depth, started}
		}
	}
	return out, nil
}
func (i *Ingester) entry(ctx context.Context, r *record.Request, index int, f transcript.Format, parent string, depth uint32, e transcript.Element, origin string, at time.Time, occurred *time.Time) (string, error) {
	id := transcript.OccurrenceID(r.ActorKeyID, f, parent, e)
	if err := i.content(ctx, e.ContentID, "element", e.Raw, e.Text, at); err != nil {
		return "", err
	}
	if id == "" {
		return "", nil
	}
	exists, err := i.exists(ctx, "transcript_entries", "entry_id", id)
	if err != nil {
		return "", err
	}
	if exists {
		return id, nil
	}
	err = i.insert(ctx, "transcript_entries", "entry_id,parent_entry_id,depth,actor_key_id,session_id,format,role,item_type,block_types,tool_call_ids,tool_names,content_id,semantic_id,origin,first_observed_at,occurred_at,first_request_id,first_attempt_index", id, parent, depth, r.ActorKeyID, r.SessionID, string(f), e.Role, e.ItemType, e.BlockTypes, e.ToolCallIDs, e.ToolNames, e.ContentID, e.SemanticID, origin, at, occurred, r.RequestID, uint32(index))
	if err == nil {
		i.known["transcript_entries:"+id] = true
	}
	return id, err
}

type batchHead struct {
	parent  string
	depth   uint32
	started time.Time
}

func headKey(actor string, f transcript.Format, responseID string, index int) string {
	view := "client"
	if index > 0 {
		view = "upstream"
	}
	return jsonString([]string{actor, string(f), responseID, view})
}
func (i *Ingester) previousHead(ctx context.Context, actor string, f transcript.Format, responseID, root string, index int) (string, uint32, error) {
	var reqID, body string
	var attempt uint32
	var stream bool
	var started time.Time
	pending, hasPending := i.heads[headKey(actor, f, responseID, index)]
	query := "SELECT request_id, toUInt32(0), response_body, stream, started_at FROM " + i.table("requests") + " WHERE response_id = ? AND actor_key_id = ? AND client_format = ? ORDER BY started_at DESC LIMIT 1"
	args := []any{responseID, actor, string(f)}
	if index > 0 {
		// Restrict the joined requests to the matching attempts so both
		// request_id and response_id indexes prune instead of hashing all requests.
		query = "SELECT a.request_id, a.attempt_index, a.response_body, r.stream, a.request_started_at FROM " + i.table("attempts") + " a INNER JOIN (SELECT request_id, stream FROM " + i.table("requests") + " WHERE actor_key_id = ? AND request_id IN (SELECT request_id FROM " + i.table("attempts") + " WHERE response_id = ?)) r ON a.request_id=r.request_id WHERE a.response_id = ? AND a.upstream_format = ? ORDER BY a.request_started_at DESC LIMIT 1"
		args = []any{actor, responseID, responseID, string(f)}
	}
	err := i.store.DB().Conn().QueryRow(ctx, query, args...).Scan(&reqID, &attempt, &body, &stream, &started)
	if errors.Is(err, sql.ErrNoRows) {
		if hasPending {
			return pending.parent, pending.depth, nil
		}
		return root, 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	if hasPending && pending.started.After(started) {
		return pending.parent, pending.depth, nil
	}
	var ids []string
	var responseHead string
	var responseDepth uint32
	err = i.store.DB().Conn().QueryRow(ctx, "SELECT entry_ids,response_head_id,response_depth FROM "+i.table("request_contexts")+" WHERE request_id = ? AND attempt_index = ? LIMIT 1", reqID, attempt).Scan(&ids, &responseHead, &responseDepth)
	if errors.Is(err, sql.ErrNoRows) {
		return root, 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	if responseHead != "" {
		return responseHead, responseDepth, nil
	}
	parent := root
	depth := uint32(0)
	for len(ids) > 0 && ids[len(ids)-1] == "" {
		ids = ids[:len(ids)-1]
	}
	if len(ids) > 0 {
		parent = ids[len(ids)-1]
		var d uint32
		err = i.store.DB().Conn().QueryRow(ctx, "SELECT depth FROM "+i.table("transcript_entries")+" WHERE entry_id = ? LIMIT 1", parent).Scan(&d)
		if err != nil {
			return "", 0, err
		}
		depth = d + 1
	}
	parsed, err := transcript.ParseResponse(f, []byte(body), stream)
	if err != nil {
		parsed, err = transcript.ParseResponse(f, []byte(body), !stream)
	}
	if err != nil {
		return parent, depth, nil
	}
	for _, e := range parsed.Elements {
		if id := transcript.OccurrenceID(actor, f, parent, e); id != "" {
			parent = id
			depth++
		}
	}
	return parent, depth, nil
}
