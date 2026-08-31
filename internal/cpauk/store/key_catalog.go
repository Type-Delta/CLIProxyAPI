package store

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

const MaxKeyLifecycleRows = 10_000

type KeyCatalogPage struct {
	Meta model.ResponseMeta  `json:"meta"`
	Keys []model.KeyIdentity `json:"keys"`
}

type KeyCatalogStore interface {
	KeyCatalog(context.Context, model.Query) (KeyCatalogPage, error)
	UpdateKeyLifecycle(context.Context, []string, []string) error
}

func (s *SQLiteStore) UpdateKeyLifecycle(ctx context.Context, configuredIDs, rotatedIDs []string) error {
	if len(configuredIDs)+len(rotatedIDs) > MaxKeyLifecycleRows {
		return fmt.Errorf("key lifecycle snapshot exceeds %d rows", MaxKeyLifecycleRows)
	}
	seen := make(map[string]struct{}, len(configuredIDs)+len(rotatedIDs))
	for _, keyID := range append(append([]string(nil), configuredIDs...), rotatedIDs...) {
		if !model.IsFullKeyID(keyID) {
			return fmt.Errorf("key lifecycle contains an invalid key ID")
		}
		if _, exists := seen[keyID]; exists {
			return fmt.Errorf("key lifecycle contains a duplicate key ID")
		}
		seen[keyID] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin key lifecycle update: %w", err)
	}
	now := time.Now().UTC().UnixNano()
	if _, err := tx.ExecContext(ctx, "UPDATE key_lifecycle SET status='deleted',updated_at_ns=? WHERE status='configured'", now); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("mark removed analytics keys: %w", err)
	}
	upsert := func(keyID string, status model.KeyStatus) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO key_lifecycle(key_id,status,updated_at_ns) VALUES (?,?,?)
ON CONFLICT(key_id) DO UPDATE SET status=excluded.status,updated_at_ns=excluded.updated_at_ns`, keyID, status, now)
		return err
	}
	for _, keyID := range configuredIDs {
		if err := upsert(keyID, model.KeyStatusConfigured); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record configured analytics key: %w", err)
		}
	}
	for _, keyID := range rotatedIDs {
		if err := upsert(keyID, model.KeyStatusRotated); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record rotated analytics key: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit key lifecycle update: %w", err)
	}
	return nil
}

func (s *SQLiteStore) KeyCatalog(ctx context.Context, query model.Query) (KeyCatalogPage, error) {
	if query.Dimension != "key" {
		return KeyCatalogPage{}, fmt.Errorf("key catalog requires the key dimension")
	}
	if err := s.validateQuery(&query, model.OperationDimensions); err != nil {
		return KeyCatalogPage{}, err
	}
	selection, err := query.SelectionDigest()
	if err != nil {
		return KeyCatalogPage{}, err
	}
	var cursor model.Cursor
	if query.Cursor != "" {
		cursor, err = s.decodeCursor(query.Cursor)
		if err != nil {
			return KeyCatalogPage{}, err
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return KeyCatalogPage{}, ErrClosed
	}
	if err := s.validateRetainedRange(ctx, query); err != nil {
		return KeyCatalogPage{}, err
	}
	grouped, err := s.dimensionTotalsFor(ctx, query, "key")
	if err != nil {
		return KeyCatalogPage{}, err
	}
	activity, err := s.keyActivity(ctx, query)
	if err != nil {
		return KeyCatalogPage{}, err
	}
	statuses, err := s.keyLifecycle(ctx)
	if err != nil {
		return KeyCatalogPage{}, err
	}
	selected := make(map[string]struct{}, len(query.KeyIDs))
	for _, keyID := range query.KeyIDs {
		selected[keyID] = struct{}{}
	}
	fullIDs := make([]string, 0, len(grouped)+len(statuses))
	for keyID := range grouped {
		fullIDs = append(fullIDs, keyID)
	}
	for keyID := range statuses {
		if _, hasUsage := grouped[keyID]; hasUsage {
			continue
		}
		if len(selected) != 0 {
			if _, included := selected[keyID]; !included {
				continue
			}
		}
		fullIDs = append(fullIDs, keyID)
	}
	shortIDs, err := model.ShortKeyIDs(fullIDs)
	if err != nil {
		return KeyCatalogPage{}, err
	}
	keys := make([]model.KeyIdentity, 0, len(fullIDs))
	for _, keyID := range fullIDs {
		totals := grouped[keyID]
		status := statuses[keyID]
		if status == "" {
			status = model.KeyStatusHistorical
		}
		item := model.KeyIdentity{KeyID: keyID, ShortKeyID: shortIDs[keyID], Status: status,
			TotalTokens: totals.tokens.Total, KnownCost: totals.knownCost, UnpricedTokens: totals.unpriced}
		if bounds, ok := activity[keyID]; ok {
			first, last := time.Unix(0, bounds[0]).UTC(), time.Unix(0, bounds[1]).UTC()
			item.FirstActivityAt, item.LastActivityAt = &first, &last
		}
		keys = append(keys, item)
	}
	slices.SortFunc(keys, func(left, right model.KeyIdentity) int {
		if metric := cmp.Compare(right.TotalTokens, left.TotalTokens); metric != 0 {
			return metric
		}
		return cmp.Compare(left.KeyID, right.KeyID)
	})
	start := 0
	if cursor.Rank > 0 {
		metric, err := strconv.ParseInt(cursor.Metric, 10, 64)
		if err != nil {
			return KeyCatalogPage{}, err
		}
		found := false
		for index := range keys {
			if keys[index].KeyID == cursor.Value && keys[index].TotalTokens == metric {
				start, found = index+1, true
				break
			}
		}
		if !found {
			return KeyCatalogPage{}, fmt.Errorf("key catalog cursor is stale")
		}
	}
	end := start + query.PageSize
	hasMore := end < len(keys)
	if end > len(keys) {
		end = len(keys)
	}
	result := KeyCatalogPage{Meta: responseMeta(query), Keys: append([]model.KeyIdentity(nil), keys[start:end]...)}
	if hasMore && len(result.Keys) != 0 {
		last := result.Keys[len(result.Keys)-1]
		next := model.Cursor{Version: 1, Operation: model.OperationDimensions, Selection: selection,
			Metric: strconv.FormatInt(last.TotalTokens, 10), Value: last.KeyID, Rank: start + len(result.Keys)}
		result.Meta.NextCursor, err = s.encodeCursor(next)
		if err != nil {
			return KeyCatalogPage{}, err
		}
	}
	return result, nil
}

func (s *SQLiteStore) keyActivity(ctx context.Context, query model.Query) (map[string][2]int64, error) {
	result := map[string][2]int64{}
	rawWhere, rawArguments, err := buildWhere(query)
	if err != nil {
		return nil, err
	}
	if err := scanKeyActivity(ctx, s.db, `SELECT key_id,MIN(requested_at_ns),MAX(requested_at_ns)
FROM events `+rawWhere+` GROUP BY key_id`, rawArguments, result); err != nil {
		return nil, fmt.Errorf("query raw key activity: %w", err)
	}
	rollupWhere, rollupArguments, err := buildRollupWhere(query, "bucket_start_ns", "bucket_end_ns")
	if err != nil {
		return nil, err
	}
	if err := scanKeyActivity(ctx, s.db, `SELECT key_id,MIN(first_activity_ns),MAX(last_activity_ns)
FROM rollups `+rollupWhere+` GROUP BY key_id`, rollupArguments, result); err != nil {
		return nil, fmt.Errorf("query retained key activity: %w", err)
	}
	return result, nil
}

func scanKeyActivity(ctx context.Context, database queryer, statement string, arguments []any, result map[string][2]int64) error {
	rows, err := database.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var keyID string
		var first, last int64
		if err := rows.Scan(&keyID, &first, &last); err != nil {
			return err
		}
		if current, exists := result[keyID]; exists {
			first = min(first, current[0])
			last = max(last, current[1])
		}
		result[keyID] = [2]int64{first, last}
	}
	return rows.Err()
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *SQLiteStore) keyLifecycle(ctx context.Context) (map[string]model.KeyStatus, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT key_id,status FROM key_lifecycle")
	if err != nil {
		return nil, fmt.Errorf("query key lifecycle: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := map[string]model.KeyStatus{}
	for rows.Next() {
		var keyID string
		var status model.KeyStatus
		if err := rows.Scan(&keyID, &status); err != nil {
			return nil, err
		}
		result[keyID] = status
	}
	return result, rows.Err()
}
