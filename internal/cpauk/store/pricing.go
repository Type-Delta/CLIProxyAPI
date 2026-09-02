package store

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

const MaxPricingRules = 10_000

type PricingProvenance struct {
	Source       string
	SourceDigest string
	SyncedAt     time.Time
}

type PricingSnapshot struct {
	Rules      []aggregate.PricingRule
	Provenance PricingProvenance
}

// PricingCatalogStore is the persistence contract used by the service layer.
// Replacing a catalog updates pricing for subsequent writes immediately.
type PricingCatalogStore interface {
	PriceBook(context.Context) (aggregate.PriceBook, error)
	UpdatePriceBook(context.Context, aggregate.PriceBook) (aggregate.PriceBook, error)
	PricingRules(context.Context) (aggregate.PriceBook, PricingProvenance, error)
	PricingSnapshot(context.Context) (PricingSnapshot, error)
	ReplacePricingRules(context.Context, []aggregate.PricingRule, PricingProvenance) error
}

func (s *SQLiteStore) PriceBook(ctx context.Context) (aggregate.PriceBook, error) {
	book, _, err := s.PricingRules(ctx)
	return book, err
}

func (s *SQLiteStore) UpdatePriceBook(ctx context.Context, book aggregate.PriceBook) (aggregate.PriceBook, error) {
	canonical, err := json.Marshal(book.Rules)
	if err != nil {
		return aggregate.PriceBook{}, fmt.Errorf("encode pricing catalog: %w", err)
	}
	digest := sha256.Sum256(canonical)
	provenance := PricingProvenance{Source: "management-api", SourceDigest: hex.EncodeToString(digest[:]), SyncedAt: time.Now().UTC()}
	if err := s.ReplacePricingRules(ctx, book.Rules, provenance); err != nil {
		return aggregate.PriceBook{}, err
	}
	return s.PriceBook(ctx)
}

func (s *SQLiteStore) ReplacePricingRules(ctx context.Context, rules []aggregate.PricingRule, provenance PricingProvenance) error {
	if err := validatePricingCatalog(rules, provenance); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	if err := replacePricingRules(ctx, s.db, rules, provenance); err != nil {
		return err
	}
	s.config.PriceBook = aggregate.PriceBook{Rules: clonePricingRules(rules)}
	return nil
}

func (s *SQLiteStore) PricingRules(ctx context.Context) (aggregate.PriceBook, PricingProvenance, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return aggregate.PriceBook{}, PricingProvenance{}, ErrClosed
	}
	return loadPricingRules(ctx, s.db)
}

func (s *SQLiteStore) PricingSnapshot(ctx context.Context) (PricingSnapshot, error) {
	book, provenance, err := s.PricingRules(ctx)
	if err != nil {
		return PricingSnapshot{}, err
	}
	return PricingSnapshot{Rules: book.Rules, Provenance: provenance}, nil
}

func (s *SQLiteStore) PricingMissing(ctx context.Context, selected model.Range) ([]model.PricingMissing, error) {
	query := model.Query{SchemaVersion: model.QuerySchemaVersion, Operation: model.OperationSummary,
		Start: selected.Start, End: selected.End, TimeZone: selected.TimeZone}
	if err := query.Validate(); err != nil {
		return nil, fmt.Errorf("invalid pricing missing range: %w", err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrClosed
	}
	if err := s.validateRetainedRange(ctx, query); err != nil {
		return nil, err
	}
	type missingTotals struct {
		firstSeen time.Time
		requests  int64
		unpriced  int64
	}
	grouped := map[string]missingTotals{}
	addRows := func(statement string, arguments ...any) error {
		rows, err := s.db.QueryContext(ctx, statement, arguments...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var modelName, provider string
			var firstNS, requests, unpriced int64
			if err := rows.Scan(&modelName, &provider, &firstNS, &requests, &unpriced); err != nil {
				return err
			}
			key := provider + "\x00" + modelName
			current := grouped[key]
			first := time.Unix(0, firstNS).UTC()
			if current.firstSeen.IsZero() || first.Before(current.firstSeen) {
				current.firstSeen = first
			}
			current.requests += requests
			current.unpriced += unpriced
			grouped[key] = current
		}
		return rows.Err()
	}
	if err := addRows(`SELECT model,provider,MIN(requested_at_ns),COUNT(DISTINCT proxy_request_id),SUM(unpriced_tokens)
FROM events WHERE requested_at_ns >= ? AND requested_at_ns < ? AND unpriced_tokens > 0
GROUP BY model,provider`, selected.Start.UnixNano(), selected.End.UnixNano()); err != nil {
		return nil, fmt.Errorf("query missing raw pricing: %w", err)
	}
	if err := addRows(`SELECT model,provider,MIN(first_activity_ns),SUM(proxy_requests),SUM(unpriced_tokens)
FROM rollups WHERE bucket_start_ns >= ? AND bucket_end_ns <= ? AND unpriced_tokens > 0
GROUP BY model,provider`, selected.Start.UnixNano(), selected.End.UnixNano()); err != nil {
		return nil, fmt.Errorf("query missing retained pricing: %w", err)
	}
	result := make([]model.PricingMissing, 0, len(grouped))
	for key, totals := range grouped {
		separator := 0
		for separator < len(key) && key[separator] != 0 {
			separator++
		}
		result = append(result, model.PricingMissing{Provider: key[:separator], Model: key[separator+1:],
			FirstSeen: totals.firstSeen, Requests: totals.requests, UnpricedTokens: totals.unpriced})
	}
	slices.SortFunc(result, func(left, right model.PricingMissing) int {
		if order := cmp.Compare(right.UnpricedTokens, left.UnpricedTokens); order != 0 {
			return order
		}
		if order := cmp.Compare(left.Provider, right.Provider); order != 0 {
			return order
		}
		return cmp.Compare(left.Model, right.Model)
	})
	return result, nil
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

func replacePricingRules(ctx context.Context, database *sql.DB, rules []aggregate.PricingRule, provenance PricingProvenance) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin pricing catalog update: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM pricing_rules"); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("clear pricing rules: %w", err)
	}
	for index := range rules {
		var input, output any
		if rules[index].InputPerMillion != nil {
			input, output = int64(*rules[index].InputPerMillion), int64(*rules[index].OutputPerMillion)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO pricing_rules
(rule_id,model,alias,input_per_million_nano,output_per_million_nano,cache_read_multiplier,cache_creation_multiplier,source)
VALUES (?,?,?,?,?,?,?,?)`, rules[index].ID, rules[index].Model, rules[index].Alias, input, output,
			rules[index].CacheReadMultiplier, rules[index].CacheCreationMultiplier, rules[index].Source); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("insert pricing rule %d: %w", index, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pricing_provenance(singleton,source,source_digest,synced_at_ns) VALUES (1,?,?,?)
ON CONFLICT(singleton) DO UPDATE SET source=excluded.source,source_digest=excluded.source_digest,synced_at_ns=excluded.synced_at_ns`,
		provenance.Source, provenance.SourceDigest, provenance.SyncedAt.UnixNano()); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("record pricing provenance: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit pricing catalog: %w", err)
	}
	return nil
}

func loadPricingRules(ctx context.Context, database *sql.DB) (aggregate.PriceBook, PricingProvenance, error) {
	rows, err := database.QueryContext(ctx, `SELECT rule_id,model,alias,input_per_million_nano,output_per_million_nano,
cache_read_multiplier,cache_creation_multiplier,source FROM pricing_rules ORDER BY rule_id`)
	if err != nil {
		return aggregate.PriceBook{}, PricingProvenance{}, fmt.Errorf("load pricing rules: %w", err)
	}
	defer func() { _ = rows.Close() }()
	book := aggregate.PriceBook{}
	for rows.Next() {
		var rule aggregate.PricingRule
		var input, output sql.NullInt64
		if err := rows.Scan(&rule.ID, &rule.Model, &rule.Alias, &input, &output, &rule.CacheReadMultiplier, &rule.CacheCreationMultiplier, &rule.Source); err != nil {
			return aggregate.PriceBook{}, PricingProvenance{}, fmt.Errorf("scan pricing rule: %w", err)
		}
		if input.Valid {
			inputValue, outputValue := model.NanoUSD(input.Int64), model.NanoUSD(output.Int64)
			rule.InputPerMillion, rule.OutputPerMillion = &inputValue, &outputValue
		}
		book.Rules = append(book.Rules, rule)
	}
	if err := rows.Err(); err != nil {
		return aggregate.PriceBook{}, PricingProvenance{}, err
	}
	var provenance PricingProvenance
	var syncedAt int64
	err = database.QueryRowContext(ctx, "SELECT source,source_digest,synced_at_ns FROM pricing_provenance WHERE singleton=1").Scan(&provenance.Source, &provenance.SourceDigest, &syncedAt)
	if err == sql.ErrNoRows {
		return book, PricingProvenance{}, nil
	}
	if err != nil {
		return aggregate.PriceBook{}, PricingProvenance{}, fmt.Errorf("load pricing provenance: %w", err)
	}
	provenance.SyncedAt = time.Unix(0, syncedAt).UTC()
	return book, provenance, nil
}

func clonePricingRules(rules []aggregate.PricingRule) []aggregate.PricingRule {
	result := append([]aggregate.PricingRule(nil), rules...)
	for index := range result {
		if result[index].InputPerMillion != nil {
			input := *result[index].InputPerMillion
			output := *result[index].OutputPerMillion
			result[index].InputPerMillion = &input
			result[index].OutputPerMillion = &output
		}
	}
	return result
}
