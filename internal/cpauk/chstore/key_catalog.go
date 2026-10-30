package chstore

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"

	store "github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/backend"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

type keyLifecycleEntry struct {
	Status    model.KeyStatus `json:"status"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func (s *Store) UpdateKeyLifecycle(ctx context.Context, configuredIDs, rotatedIDs []string) error {
	if len(configuredIDs)+len(rotatedIDs) > store.MaxKeyLifecycleRows {
		return fmt.Errorf("key lifecycle snapshot exceeds %d rows", store.MaxKeyLifecycleRows)
	}
	seen := map[string]bool{}
	for _, id := range append(append([]string(nil), configuredIDs...), rotatedIDs...) {
		if !model.IsFullKeyID(id) {
			return fmt.Errorf("key lifecycle contains an invalid key ID")
		}
		if seen[id] {
			return fmt.Errorf("key lifecycle contains a duplicate key ID")
		}
		seen[id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	values, err := s.loadControl(ctx, "key_lifecycle")
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for id, raw := range values {
		var entry keyLifecycleEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			return err
		}
		if entry.Status == model.KeyStatusConfigured {
			entry.Status = model.KeyStatusDeleted
			entry.UpdatedAt = now
			values[id], err = json.Marshal(entry)
			if err != nil {
				return err
			}
		}
	}
	for _, group := range []struct {
		ids    []string
		status model.KeyStatus
	}{{configuredIDs, model.KeyStatusConfigured}, {rotatedIDs, model.KeyStatusRotated}} {
		for _, id := range group.ids {
			raw, err := json.Marshal(keyLifecycleEntry{group.status, now})
			if err != nil {
				return err
			}
			values[id] = raw
		}
	}
	return s.replaceControl(ctx, "key_lifecycle", values)
}

// KeyCatalog joins lifecycle state with selected usage and lifetime activity.
func (s *Store) KeyCatalog(ctx context.Context, query model.Query) (KeyCatalogPage, error) {
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
		cursor, err = s.config.CursorCodec.Decode(query.Cursor)
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
	type modelUsage struct {
		tokens int64
		cost   model.NanoUSD
	}
	type entry struct {
		item   model.KeyIdentity
		models map[string]modelUsage
	}
	entries := map[string]*entry{}
	ensure := func(id string) *entry {
		if entries[id] == nil {
			entries[id] = &entry{item: model.KeyIdentity{KeyID: id, Status: model.KeyStatusHistorical}, models: map[string]modelUsage{}}
		}
		return entries[id]
	}
	raw, err := s.rawSelection(ctx, query, "key_id", "model", "requested_at_ns", "total_tokens", "known_cost_nano", "unpriced_tokens", "generation_time_ms")
	if err != nil {
		return KeyCatalogPage{}, err
	}
	retained, err := s.retainedSelection(ctx, query, false, "key_id", "model", "first_activity_ns", "last_activity_ns", "total_tokens", "known_cost_nano", "unpriced_tokens")
	if err != nil {
		return KeyCatalogPage{}, err
	}
	statement := "SELECT key_id,model,min(first),max(last),sum(total_tokens),toString(sum(toInt128(cost))),sum(unpriced_tokens),sum(generation_time_ms),count(generation_time_ms) FROM (SELECT key_id,model,requested_at_ns AS first,requested_at_ns AS last,total_tokens,coalesce(known_cost_nano,0) AS cost,unpriced_tokens,generation_time_ms FROM (" + raw.SQL + ") UNION ALL SELECT key_id,model,first_activity_ns,last_activity_ns,total_tokens,known_cost_nano,unpriced_tokens,CAST(NULL AS Nullable(Int64)) FROM (" + retained.SQL + ")) GROUP BY key_id,model"
	rows, err := s.db.Conn().Query(ctx, statement, append(raw.Args, retained.Args...)...)
	if err != nil {
		return KeyCatalogPage{}, err
	}
	for rows.Next() {
		var id, name string
		var first, last, tokens, unpriced int64
		var costText string
		var samples uint64
		var generation *int64
		if err := rows.Scan(&id, &name, &first, &last, &tokens, &costText, &unpriced, &generation, &samples); err != nil {
			_ = rows.Close()
			return KeyCatalogPage{}, err
		}
		cost, errCost := strconv.ParseInt(costText, 10, 64)
		if errCost != nil {
			_ = rows.Close()
			return KeyCatalogPage{}, fmt.Errorf("integer overflow in analytics money total")
		}
		e := ensure(id)
		e.item.TotalTokens += tokens
		combined, errSum := addMoney(int64(e.item.KnownCost), cost)
		if errSum != nil {
			_ = rows.Close()
			return KeyCatalogPage{}, errSum
		}
		e.item.KnownCost = model.NanoUSD(combined)
		e.item.UnpricedTokens += unpriced
		e.models[name] = modelUsage{tokens: tokens, cost: model.NanoUSD(cost)}
		c3UpdateBounds(&e.item.FirstActivityAt, &e.item.LastActivityAt, first, last)
		if generation != nil && samples > 0 {
			if e.item.GenerationTimeMS == nil {
				v := int64(0)
				e.item.GenerationTimeMS = &v
			}
			*e.item.GenerationTimeMS += *generation
			e.item.GenerationSampleCount += int64(samples)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return KeyCatalogPage{}, err
	}
	raw, err = s.rawSelection(ctx, query, "key_id", "proxy_request_id")
	if err != nil {
		return KeyCatalogPage{}, err
	}
	retained, err = s.retainedSelection(ctx, query, true, "key_id", "proxy_request_id")
	if err != nil {
		return KeyCatalogPage{}, err
	}
	rows, err = s.db.Conn().Query(ctx, "SELECT key_id,uniqExact(proxy_request_id) FROM ("+raw.SQL+" UNION ALL "+retained.SQL+") GROUP BY key_id", append(raw.Args, retained.Args...)...)
	if err != nil {
		return KeyCatalogPage{}, err
	}
	for rows.Next() {
		var id string
		var requests uint64
		if err := rows.Scan(&id, &requests); err != nil {
			_ = rows.Close()
			return KeyCatalogPage{}, err
		}
		ensure(id).item.Requests = int64(requests)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return KeyCatalogPage{}, err
	}
	lifetimeBounds := map[string][2]int64{}
	lifetime := query
	lifetime.Start = time.Unix(0, math.MinInt64).UTC()
	lifetime.End = time.Unix(0, math.MaxInt64).UTC()
	lifetime.KeyIDs = nil
	lifetime.Filters = nil
	raw, err = s.rawSelection(ctx, lifetime, "key_id", "requested_at_ns")
	if err != nil {
		return KeyCatalogPage{}, err
	}
	retained, err = s.retainedSelection(ctx, lifetime, false, "key_id", "first_activity_ns", "last_activity_ns")
	if err != nil {
		return KeyCatalogPage{}, err
	}
	rows, err = s.db.Conn().Query(ctx, "SELECT key_id,min(first),max(last) FROM (SELECT key_id,requested_at_ns AS first,requested_at_ns AS last FROM ("+raw.SQL+") UNION ALL SELECT key_id,first_activity_ns,last_activity_ns FROM ("+retained.SQL+")) GROUP BY key_id", append(raw.Args, retained.Args...)...)
	if err != nil {
		return KeyCatalogPage{}, err
	}
	for rows.Next() {
		var id string
		var first, last int64
		if err := rows.Scan(&id, &first, &last); err != nil {
			_ = rows.Close()
			return KeyCatalogPage{}, err
		}

		lifetimeBounds[id] = [2]int64{first, last}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return KeyCatalogPage{}, err
	}
	values, err := s.loadControl(ctx, "key_lifecycle")
	if err != nil {
		return KeyCatalogPage{}, err
	}
	for id, raw := range values {
		var value keyLifecycleEntry
		if err := json.Unmarshal(raw, &value); err != nil {
			return KeyCatalogPage{}, err
		}
		if len(query.KeyIDs) > 0 && !slices.Contains(query.KeyIDs, id) {
			continue
		}
		ensure(id).item.Status = value.Status
	}
	for id, e := range entries {
		if bounds, ok := lifetimeBounds[id]; ok {
			c3UpdateBounds(&e.item.LifetimeFirstActivityAt, &e.item.LifetimeLastActivityAt, bounds[0], bounds[1])
		}
	}
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	short, err := model.ShortKeyIDs(ids)
	if err != nil {
		return KeyCatalogPage{}, err
	}
	items := make([]model.KeyIdentity, 0, len(entries))
	for id, e := range entries {
		e.item.ShortKeyID = short[id]
		names := make([]string, 0, len(e.models))
		for name := range e.models {
			names = append(names, name)
		}
		slices.SortFunc(names, func(a, b string) int {
			if e.models[a].tokens != e.models[b].tokens {
				return cmp.Compare(e.models[b].tokens, e.models[a].tokens)
			}
			return cmp.Compare(a, b)
		})
		for _, name := range names {
			u := e.models[name]
			if e.item.TopModel == nil {
				n := name
				e.item.TopModel = &n
				e.item.TopModelTokens = u.tokens
			}
			if u.cost > e.item.TopCostModelUSD {
				n := name
				e.item.TopCostModel = &n
				e.item.TopCostModelUSD = u.cost
			}
		}
		items = append(items, e.item)
	}
	slices.SortFunc(items, func(a, b model.KeyIdentity) int {
		if a.TotalTokens != b.TotalTokens {
			return cmp.Compare(b.TotalTokens, a.TotalTokens)
		}
		return cmp.Compare(a.KeyID, b.KeyID)
	})
	start := 0
	if cursor.Rank > 0 {
		metric, err := strconv.ParseInt(cursor.Metric, 10, 64)
		if err != nil {
			return KeyCatalogPage{}, err
		}
		found := false
		for i, item := range items {
			if item.KeyID == cursor.Value && item.TotalTokens == metric {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			return KeyCatalogPage{}, fmt.Errorf("key catalog cursor is stale")
		}
	}
	end := min(start+query.PageSize, len(items))
	result := KeyCatalogPage{Meta: responseMeta(query), Keys: append([]model.KeyIdentity(nil), items[start:end]...)}
	if end < len(items) && len(result.Keys) > 0 {
		last := result.Keys[len(result.Keys)-1]
		result.Meta.NextCursor, err = s.config.CursorCodec.Encode(model.Cursor{Version: 1, Operation: model.OperationDimensions, Selection: selection, Metric: strconv.FormatInt(last.TotalTokens, 10), Value: last.KeyID, Rank: end})
		if err != nil {
			return KeyCatalogPage{}, err
		}
	}
	return result, nil
}
func c3UpdateBounds(first, last **time.Time, firstNS, lastNS int64) {
	a, b := time.Unix(0, firstNS).UTC(), time.Unix(0, lastNS).UTC()
	if *first == nil || a.Before(**first) {
		*first = &a
	}
	if *last == nil || b.After(**last) {
		*last = &b
	}
}
