package chstore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	store "github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/backend"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

const MaxPricingRules = store.MaxPricingRules
const ModelsDevSource = store.ModelsDevSource
const PricingCatalogTTL = store.PricingCatalogTTL
const PricingRetrySuppression = store.PricingRetrySuppression

func (s *Store) loadPricing(ctx context.Context) (PricingSnapshot, map[string]json.RawMessage, error) {
	values, err := s.loadControl(ctx, "pricing")
	if err != nil {
		return PricingSnapshot{}, nil, err
	}
	var snapshot PricingSnapshot
	if raw, ok := values["snapshot"]; ok {
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			return snapshot, nil, err
		}
	} else {
		snapshot = pricingSnapshot(clonePriceBook(s.config.PriceBook), PricingProvenance{})
	}
	return snapshot, values, nil
}
func (s *Store) publishPricing(ctx context.Context, snapshot PricingSnapshot, values map[string]json.RawMessage) error {
	book := aggregate.PriceBook{Rules: clonePricingRules(snapshot.Rules), KiroUSDPerCredit: snapshot.KiroUSDPerCredit}
	snapshot = pricingSnapshot(book, snapshot.Provenance)
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	values["snapshot"] = raw
	if err = s.replaceControl(ctx, "pricing", values); err != nil {
		return err
	}
	s.config.PriceBook = clonePriceBook(book)
	return nil
}
func (s *Store) PricingSnapshot(ctx context.Context) (PricingSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return PricingSnapshot{}, ErrClosed
	}
	snapshot, _, err := s.loadPricing(ctx)
	return snapshot, err
}
func (s *Store) PricingRules(ctx context.Context) (aggregate.PriceBook, PricingProvenance, error) {
	snapshot, err := s.PricingSnapshot(ctx)
	return aggregate.PriceBook{Rules: snapshot.Rules, KiroUSDPerCredit: snapshot.KiroUSDPerCredit}, snapshot.Provenance, err
}
func (s *Store) PriceBook(ctx context.Context) (aggregate.PriceBook, error) {
	book, _, err := s.PricingRules(ctx)
	return book, err
}
func (s *Store) UpdatePriceBook(ctx context.Context, book aggregate.PriceBook) (aggregate.PriceBook, error) {
	manual := clonePricingRules(book.Rules)
	for i := range manual {
		manual[i].Catalog = false
		if manual[i].Source == ModelsDevSource {
			manual[i].Source = "management-api"
		}
	}
	if book.KiroUSDPerCredit != nil && *book.KiroUSDPerCredit < 0 {
		return aggregate.PriceBook{}, fmt.Errorf("negative credit rate")
	}
	if err := validatePricingRules(manual); err != nil {
		return aggregate.PriceBook{}, err
	}
	encoded, err := json.Marshal(manual)
	if err != nil {
		return aggregate.PriceBook{}, err
	}
	digest := sha256.Sum256(encoded)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return aggregate.PriceBook{}, ErrClosed
	}
	snapshot, values, err := s.loadPricing(ctx)
	if err != nil {
		return aggregate.PriceBook{}, err
	}
	p := snapshot.Provenance
	p.Source = "management-api"
	p.SourceDigest = fmt.Sprintf("%x", digest[:])
	p.SyncedAt = time.Now().UTC()
	sort.Slice(manual, func(i, j int) bool { return manual[i].ID < manual[j].ID })
	snapshot.Rules = append(manual, snapshot.Catalog...)
	if book.KiroUSDPerCredit != nil {
		snapshot.KiroUSDPerCredit = book.KiroUSDPerCredit
	}
	snapshot.Provenance = p
	if err := s.publishPricing(ctx, snapshot, values); err != nil {
		return aggregate.PriceBook{}, err
	}
	return clonePriceBook(s.config.PriceBook), nil
}
func (s *Store) ReplacePricingRules(ctx context.Context, rules []aggregate.PricingRule, p PricingProvenance) error {
	if err := validatePricingCatalog(rules, p); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	snapshot, values, err := s.loadPricing(ctx)
	if err != nil {
		return err
	}
	snapshot.Rules = clonePricingRules(rules)
	sort.SliceStable(snapshot.Rules, func(i, j int) bool {
		if snapshot.Rules[i].Catalog != snapshot.Rules[j].Catalog {
			return !snapshot.Rules[i].Catalog
		}
		return snapshot.Rules[i].ID < snapshot.Rules[j].ID
	})
	snapshot.Provenance = p
	return s.publishPricing(ctx, snapshot, values)
}
func (s *Store) CatalogProviders(ctx context.Context) ([]CatalogProvider, time.Time, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, time.Time{}, ErrClosed
	}
	snapshot, values, err := s.loadPricing(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	providers := []CatalogProvider{}
	if raw, ok := values["providers"]; ok {
		if err := json.Unmarshal(raw, &providers); err != nil {
			return nil, time.Time{}, err
		}
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })
	return providers, snapshot.Provenance.CatalogSyncedAt, nil
}

func validatePricingCatalog(rules []aggregate.PricingRule, provenance PricingProvenance) error {
	if len(rules) > MaxPricingRules {
		return fmt.Errorf("pricing catalog exceeds %d rules", MaxPricingRules)
	}
	if provenance.Source == "" || len(provenance.Source) > model.MaxStoredStringBytes || !model.IsFullKeyID(provenance.SourceDigest) || provenance.SyncedAt.IsZero() || provenance.SyncedAt.Location() != time.UTC {
		return fmt.Errorf("pricing provenance is invalid")
	}
	seen := make(map[string]struct{}, len(rules))
	for index := range rules {
		if err := rules[index].Validate(); err != nil {
			return err
		}
		if _, duplicate := seen[rules[index].ID]; duplicate {
			return fmt.Errorf("duplicate pricing rule %q", rules[index].ID)
		}
		seen[rules[index].ID] = struct{}{}
	}
	return nil
}

func validatePricingRules(rules []aggregate.PricingRule) error {
	if len(rules) > MaxPricingRules {
		return fmt.Errorf("pricing catalog exceeds %d rules", MaxPricingRules)
	}
	seen := make(map[string]struct{}, len(rules))
	ids := make(map[string]struct{}, len(rules))
	for index := range rules {
		if _, exists := ids[rules[index].ID]; exists {
			return fmt.Errorf("duplicate pricing rule %q", rules[index].ID)
		}
		ids[rules[index].ID] = struct{}{}
		if err := rules[index].Validate(); err != nil {
			return err
		}
		match := rules[index].Provider + "\x00"
		if rules[index].Model != "" {
			match += "model\x00" + rules[index].Model
		} else {
			match += "alias\x00" + rules[index].Alias
		}
		if _, duplicate := seen[match]; duplicate {
			return fmt.Errorf("duplicate pricing match %q", match)
		}
		seen[match] = struct{}{}
	}
	return nil
}

func clonePricingRules(rules []aggregate.PricingRule) []aggregate.PricingRule {
	result := append([]aggregate.PricingRule(nil), rules...)
	for index := range result {
		result[index].ServiceTierPrices = maps.Clone(result[index].ServiceTierPrices)
		if result[index].InputPerMillion != nil {
			input := *result[index].InputPerMillion
			result[index].InputPerMillion = &input
		}
		if result[index].OutputPerMillion != nil {
			output := *result[index].OutputPerMillion
			result[index].OutputPerMillion = &output
		}
	}
	return result
}

func clonePriceBook(book aggregate.PriceBook) aggregate.PriceBook {
	book.Rules = clonePricingRules(book.Rules)
	if book.KiroUSDPerCredit != nil {
		rate := *book.KiroUSDPerCredit
		book.KiroUSDPerCredit = &rate
	}
	return book
}

func (s *Store) PricingMissing(ctx context.Context, selected model.Range) ([]model.PricingMissing, error) {
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
