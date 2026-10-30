package chstore

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// Frozen row-streaming implementations serve as parity and benchmark oracles.
func (s *Store) s1LegacyKeyCatalog(ctx context.Context, query model.Query) (KeyCatalogPage, error) {
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
		item     model.KeyIdentity
		models   map[string]modelUsage
		requests map[string]bool
	}
	entries := map[string]*entry{}
	ensure := func(id string) *entry {
		if entries[id] == nil {
			entries[id] = &entry{item: model.KeyIdentity{KeyID: id, Status: model.KeyStatusHistorical}, models: map[string]modelUsage{}, requests: map[string]bool{}}
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
	statement := "SELECT key_id,model,requested_at_ns,requested_at_ns,total_tokens,coalesce(known_cost_nano,0),unpriced_tokens,generation_time_ms FROM (" + raw.SQL + ") UNION ALL SELECT key_id,model,first_activity_ns,last_activity_ns,total_tokens,known_cost_nano,unpriced_tokens,CAST(NULL AS Nullable(Int64)) FROM (" + retained.SQL + ")"
	rows, err := s.db.Conn().Query(ctx, statement, append(raw.Args, retained.Args...)...)
	if err != nil {
		return KeyCatalogPage{}, err
	}
	for rows.Next() {
		var id, name string
		var first, last, tokens, cost, unpriced int64
		var generation *int64
		if err := rows.Scan(&id, &name, &first, &last, &tokens, &cost, &unpriced, &generation); err != nil {
			_ = rows.Close()
			return KeyCatalogPage{}, err
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
		usage := e.models[name]
		usage.tokens += tokens
		combined, errSum = addMoney(int64(usage.cost), cost)
		if errSum != nil {
			_ = rows.Close()
			return KeyCatalogPage{}, errSum
		}
		usage.cost = model.NanoUSD(combined)
		e.models[name] = usage
		c3UpdateBounds(&e.item.FirstActivityAt, &e.item.LastActivityAt, first, last)
		if generation != nil {
			if e.item.GenerationTimeMS == nil {
				v := int64(0)
				e.item.GenerationTimeMS = &v
			}
			*e.item.GenerationTimeMS += *generation
			e.item.GenerationSampleCount++
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
	rows, err = s.db.Conn().Query(ctx, raw.SQL+" UNION ALL "+retained.SQL, append(raw.Args, retained.Args...)...)
	if err != nil {
		return KeyCatalogPage{}, err
	}
	for rows.Next() {
		var id, request string
		if err := rows.Scan(&id, &request); err != nil {
			_ = rows.Close()
			return KeyCatalogPage{}, err
		}
		ensure(id).requests[request] = true
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
	rows, err = s.db.Conn().Query(ctx, "SELECT key_id,requested_at_ns,requested_at_ns FROM ("+raw.SQL+") UNION ALL SELECT key_id,first_activity_ns,last_activity_ns FROM ("+retained.SQL+")", append(raw.Args, retained.Args...)...)
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
		if previous, ok := lifetimeBounds[id]; ok {
			first = min(first, previous[0])
			last = max(last, previous[1])
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
		e.item.Requests = int64(len(e.requests))
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

func (s *Store) s1LegacyProviderCredentials(ctx context.Context) ([]model.ProviderCredential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrClosed
	}
	byID := map[string]*model.ProviderCredential{}
	requests := map[string]map[string]bool{}
	ensure := func(provider, id string) *model.ProviderCredential {
		key := provider + "\x00" + id
		if byID[key] == nil {
			byID[key] = &model.ProviderCredential{Provider: provider, CredentialID: id, Status: "available"}
		}
		return byID[key]
	}
	addRequest := func(provider, id, request string) {
		key := provider + "\x00" + id
		if requests[key] == nil {
			requests[key] = map[string]bool{}
		}
		requests[key][request] = true
	}
	bounds, err := s.readCutoffs(ctx)
	if err != nil {
		return nil, err
	}
	where := "1=1"
	args := []any{}
	if bounds.raw != 0 {
		where = "requested_at_ns >= ?"
		args = append(args, bounds.raw)
	}

	events, err := s.loadEvents(ctx, where, args...)
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		if e.CredentialID == nil || *e.CredentialID == "" {
			continue
		}
		c := ensure(e.Provider, *e.CredentialID)
		addRequest(e.Provider, *e.CredentialID, e.ProxyRequestID)
		if !e.Succeeded {
			c.Failed++
			if e.ErrorClass != nil && (c.LastErrorAt == nil || e.RequestedAt.After(*c.LastErrorAt)) {
				v := *e.ErrorClass
				t := e.RequestedAt
				c.LastErrorClass = &v
				c.LastErrorAt = &t
			}
		}
		if e.RequestedAt.After(c.ObservedAt) {
			c.ObservedAt = e.RequestedAt
			if e.AuthType != nil {
				c.AuthType = *e.AuthType
			} else {
				c.AuthType = ""
			}
		}
	}
	hourly, cutoff := bounds.hourly, bounds.raw
	visible := "is_deleted=0 AND credential_id != '' AND ((grain='hourly' AND bucket_end_ns<=? AND bucket_start_ns>=?) OR (grain='daily' AND bucket_end_ns<=?))"
	visible += s.c4bTablePublicationFilter("analytics_rollups")
	rows, err := s.db.Conn().Query(ctx, "SELECT provider,credential_id,auth_type,succeeded,error_class,upstream_attempts,last_activity_ns FROM "+s.table("analytics_rollups")+" FINAL WHERE "+visible, cutoff, hourly, hourly)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var provider, id, auth, class string
		var succeeded, attempts, ns int64
		if err := rows.Scan(&provider, &id, &auth, &succeeded, &class, &attempts, &ns); err != nil {
			_ = rows.Close()
			return nil, err
		}
		c := ensure(provider, id)
		at := time.Unix(0, ns).UTC()
		if succeeded == 0 {
			c.Failed += attempts
			if class != "" && (c.LastErrorAt == nil || at.After(*c.LastErrorAt)) {
				c.LastErrorClass = &class
				c.LastErrorAt = &at
			}
		}
		if at.After(c.ObservedAt) {
			c.ObservedAt = at
			c.AuthType = auth
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = s.db.Conn().Query(ctx, "SELECT provider,credential_id,proxy_request_id FROM "+s.table("analytics_request_rollups")+" FINAL WHERE "+visible, cutoff, hourly, hourly)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var provider, id, request string
		if err := rows.Scan(&provider, &id, &request); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ensure(provider, id)
		addRequest(provider, id, request)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	snapshots, err := s.providerQuotaSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	for _, item := range snapshots {
		c := ensure(item.Provider, item.CredentialID)
		status := "available"
		if item.Disabled {
			status = "disabled"
		} else if item.QuotaExceeded {
			status = "quota_exceeded"
		} else if !item.Available {
			status = "unavailable"
		}
		if providerStatusSeverity(status) > providerStatusSeverity(c.Status) {
			c.Status = status
		}
		if c.Quota == nil {
			c.Quota = &model.ProviderQuota{}
		}
		if item.NextResetAt != nil && (c.Quota.ResetsAt == nil || item.NextResetAt.Before(*c.Quota.ResetsAt)) {
			v := *item.NextResetAt
			c.Quota.ResetsAt = &v
		}
		if item.ObservedAt.After(c.ObservedAt) {
			c.ObservedAt = item.ObservedAt
		}
	}
	result := make([]model.ProviderCredential, 0, len(byID))
	for key, c := range byID {
		c.Requests = int64(len(requests[key]))
		result = append(result, *c)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Provider != result[j].Provider {
			return result[i].Provider < result[j].Provider
		}
		return result[i].CredentialID < result[j].CredentialID
	})
	return result, nil
}

func (s *Store) s1LegacyPricingMissing(ctx context.Context, selected model.Range) ([]model.PricingMissing, error) {
	query := model.Query{SchemaVersion: model.QuerySchemaVersion, Operation: model.OperationSummary, Start: selected.Start, End: selected.End, TimeZone: selected.TimeZone}
	if err := query.Validate(); err != nil {
		return nil, fmt.Errorf("invalid pricing missing range: %w", err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrClosed
	}
	bounds, err := s.readCutoffs(ctx)
	if err != nil {
		return nil, err
	}
	hourly, cutoff := bounds.hourly, bounds.raw
	visible := "is_deleted=0 AND ((grain='hourly' AND bucket_end_ns<=? AND bucket_start_ns>=?) OR (grain='daily' AND bucket_end_ns<=?))"
	visible += s.c4bTablePublicationFilter("analytics_rollups")
	args := []any{cutoff, hourly, hourly}
	var overlaps, partial uint64
	if err := s.db.Conn().QueryRow(ctx, "SELECT count(),countIf(bucket_start_ns<? OR bucket_end_ns>?) FROM "+s.table("analytics_rollups")+" FINAL WHERE "+visible+" AND bucket_start_ns<? AND bucket_end_ns>?", append([]any{selected.Start.UnixNano(), selected.End.UnixNano()}, append(args, selected.End.UnixNano(), selected.Start.UnixNano())...)...).Scan(&overlaps, &partial); err != nil {
		return nil, err
	}
	if partial > 0 {
		return nil, ErrRetainedRangePartial
	}
	if overlaps > 0 && selected.TimeZone != s.config.RetentionTimeZone {
		return nil, RetainedTimeZoneError{StorageTimeZone: s.config.RetentionTimeZone, QueryTimeZone: selected.TimeZone}
	}
	type accum struct {
		item             model.PricingMissing
		requests         map[string]bool
		retainedRequests int64
	}
	grouped := map[string]*accum{}
	get := func(provider, name string) *accum {
		key := provider + "\x00" + name
		if grouped[key] == nil {
			grouped[key] = &accum{item: model.PricingMissing{Provider: provider, Model: name}, requests: map[string]bool{}}
		}
		return grouped[key]
	}
	addTime := func(a *accum, t time.Time) {
		if a.item.FirstSeen.IsZero() || t.Before(a.item.FirstSeen) {
			a.item.FirstSeen = t
		}
	}
	start := selected.Start.UnixNano()
	if cutoff != 0 && start < cutoff {
		start = cutoff
	}
	events, err := s.loadEvents(ctx, "requested_at_ns>=? AND requested_at_ns<? AND unpriced_tokens>0", start, selected.End.UnixNano())
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		a := get(e.Provider, e.Model)
		addTime(a, e.RequestedAt)
		a.item.UnpricedTokens += e.UnpricedTokens
		a.requests[e.ProxyRequestID] = true
	}
	rows, err := s.db.Conn().Query(ctx, "SELECT provider,model,first_activity_ns,proxy_requests,unpriced_tokens FROM "+s.table("analytics_rollups")+" FINAL WHERE "+visible+" AND bucket_start_ns>=? AND bucket_end_ns<=? AND unpriced_tokens>0", append(args, selected.Start.UnixNano(), selected.End.UnixNano())...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var provider, name string
		var first, requests, unpriced int64
		if err := rows.Scan(&provider, &name, &first, &requests, &unpriced); err != nil {
			_ = rows.Close()
			return nil, err
		}
		a := get(provider, name)
		addTime(a, time.Unix(0, first).UTC())
		a.item.UnpricedTokens += unpriced
		a.retainedRequests += requests
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}

	// SQLite sums each retained dimension bucket's request count when it contains
	// unpriced tokens. This can include priced requests from the same bucket.

	result := make([]model.PricingMissing, 0, len(grouped))
	for _, a := range grouped {
		a.item.Requests = int64(len(a.requests)) + a.retainedRequests
		result = append(result, a.item)
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.UnpricedTokens != b.UnpricedTokens {
			return a.UnpricedTokens > b.UnpricedTokens
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Model < b.Model
	})
	return result, nil
}
