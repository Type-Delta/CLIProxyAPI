package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

const (
	modelsDevURL          = "https://models.dev/api.json"
	maxModelsDevBodyBytes = 32 << 20
)

// RefreshPricing is demand driven. It never runs from Open or a timer. A
// successful catalog replaces only the remote table; management overrides are
// read and written independently and therefore survive every refresh.
func (s *SQLiteStore) RefreshPricing(ctx context.Context) (result PricingRefreshResult, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil {
		return PricingRefreshResult{State: "unavailable", Err: ErrClosed}, ErrClosed
	}
	now := s.nowPricing()
	book, provenance, err := s.PricingRules(ctx)
	if err != nil {
		return PricingRefreshResult{State: "unavailable", Err: err}, err
	}
	result = pricingRefreshResult(book, provenance, now)
	if !resultShouldRefresh(result, now) {
		return result, nil
	}

	s.pricingRefreshMu.Lock()
	if flight := s.pricingFlight; flight != nil {
		done := flight.done
		s.pricingRefreshMu.Unlock()
		select {
		case <-done:
			return flight.result, flight.result.Err
		case <-ctx.Done():
			return result, ctx.Err()
		}
	}
	// Re-check after acquiring the coalescing lock. Another caller may have
	// completed a successful update between the first read and this lock.
	now = s.nowPricing()
	book, provenance, err = s.PricingRules(ctx)
	if err != nil {
		s.pricingRefreshMu.Unlock()
		return PricingRefreshResult{State: "unavailable", Err: err}, err
	}
	result = pricingRefreshResult(book, provenance, now)
	if !resultShouldRefresh(result, now) {
		s.pricingRefreshMu.Unlock()
		return result, nil
	}
	flight := &pricingRefreshFlight{done: make(chan struct{})}
	s.pricingFlight = flight
	s.pricingRefreshMu.Unlock()

	defer func() {
		s.pricingRefreshMu.Lock()
		flight.result = result
		close(flight.done)
		s.pricingFlight = nil
		s.pricingRefreshMu.Unlock()
		err = result.Err
	}()

	catalog, fetchErr := s.pricingFetcher.Fetch(ctx)
	result.Attempted = true
	if fetchErr != nil {
		result.Err = s.recordPricingRefreshFailure(ctx, fetchErr)
		result.State = pricingState(book.Rules, provenance, now)
		return result, result.Err
	}
	if errValidate := validateFetchedCatalog(catalog.Rules, catalog.Digest); errValidate != nil {
		result.Err = s.recordPricingRefreshFailure(ctx, errValidate)
		result.State = pricingState(book.Rules, provenance, now)
		return result, result.Err
	}
	if errReplace := s.replacePricingCatalog(ctx, catalog, now); errReplace != nil {
		result.Err = errReplace
		result.State = pricingState(book.Rules, provenance, now)
		return result, result.Err
	}
	updated, updatedProvenance, loadErr := s.PricingRules(ctx)
	if loadErr != nil {
		result.Err = loadErr
		return result, result.Err
	}
	result.Snapshot = pricingSnapshot(updated, updatedProvenance)
	result.State = "fresh"
	return result, nil
}

func (s *SQLiteStore) nowPricing() time.Time {
	now := time.Now
	if s != nil && s.pricingNow != nil {
		now = s.pricingNow
	}
	return now().UTC()
}

func resultShouldRefresh(result PricingRefreshResult, now time.Time) bool {
	if result.State == "fresh" || (!result.Snapshot.Provenance.CatalogRetryAt.IsZero() && now.Before(result.Snapshot.Provenance.CatalogRetryAt)) {
		return false
	}
	return true
}

func pricingRefreshResult(book aggregate.PriceBook, provenance PricingProvenance, now time.Time) PricingRefreshResult {
	return PricingRefreshResult{Snapshot: pricingSnapshot(book, provenance), State: pricingState(book.Rules, provenance, now)}
}

func pricingSnapshot(book aggregate.PriceBook, provenance PricingProvenance) PricingSnapshot {
	snapshot := PricingSnapshot{Rules: clonePricingRules(book.Rules), Provenance: provenance}
	for _, rule := range book.Rules {
		if rule.Catalog {
			snapshot.Catalog = append(snapshot.Catalog, clonePricingRules([]aggregate.PricingRule{rule})[0])
		} else {
			snapshot.Overrides = append(snapshot.Overrides, clonePricingRules([]aggregate.PricingRule{rule})[0])
		}
	}
	return snapshot
}

func pricingState(rules []aggregate.PricingRule, provenance PricingProvenance, now time.Time) string {
	hasCatalog := !provenance.CatalogSyncedAt.IsZero()
	if hasCatalog {
		foundCatalog := false
		for _, rule := range rules {
			if rule.Catalog {
				foundCatalog = true
				break
			}
		}
		hasCatalog = foundCatalog
	}
	if hasCatalog && now.Before(provenance.CatalogExpiresAt) {
		return "fresh"
	}
	if hasCatalog {
		return "stale"
	}
	return "unavailable"
}

func validateFetchedCatalog(rules []aggregate.PricingRule, digest string) error {
	if len(rules) == 0 {
		return errors.New("models.dev returned an empty pricing catalog")
	}
	if !model.IsFullKeyID(digest) {
		return errors.New("models.dev returned an invalid catalog digest")
	}
	for index := range rules {
		rule := rules[index]
		if !rule.Catalog || rule.Source != ModelsDevSource || rule.Provider == "" {
			return fmt.Errorf("invalid models.dev rule %q", rule.ID)
		}
	}
	return validatePricingRules(rules)
}

func (s *SQLiteStore) recordPricingRefreshFailure(ctx context.Context, fetchErr error) error {
	now := s.nowPricing()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	provenance, err := loadPricingProvenance(ctx, s.db)
	if err != nil {
		return fetchErr
	}
	provenance.CatalogRetryAt = now.Add(PricingRetrySuppression)
	provenance.CatalogLastError = "fetch_failed"
	if provenance.Source == "" {
		provenance.Source = "management-api"
	}
	if provenance.SourceDigest == "" {
		digest := sha256.Sum256(nil)
		provenance.SourceDigest = fmt.Sprintf("%x", digest[:])
	}
	if provenance.SyncedAt.IsZero() {
		provenance.SyncedAt = now
	}
	tx, beginErr := s.db.BeginTx(ctx, nil)
	if beginErr != nil {
		return fetchErr
	}
	if writeErr := writePricingProvenance(ctx, tx, provenance); writeErr != nil {
		_ = tx.Rollback()
		return fetchErr
	}
	if commitErr := tx.Commit(); commitErr != nil {
		return fetchErr
	}
	return fetchErr
}

func (s *SQLiteStore) replacePricingCatalog(ctx context.Context, catalog PricingCatalog, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	manualBook, provenance, err := loadPricingRules(ctx, s.db)
	if err != nil {
		return err
	}
	manualCount := 0
	for _, rule := range manualBook.Rules {
		if !rule.Catalog {
			manualCount++
		}
	}
	if manualCount+len(catalog.Rules) > MaxPricingRules {
		return fmt.Errorf("pricing catalog exceeds %d rules", MaxPricingRules)
	}
	provenance.CatalogSource = ModelsDevSource
	provenance.CatalogDigest = catalog.Digest
	provenance.CatalogSyncedAt = now
	provenance.CatalogExpiresAt = now.Add(PricingCatalogTTL)
	provenance.CatalogRetryAt = time.Time{}
	provenance.CatalogLastError = ""
	if provenance.Source == "" {
		provenance.Source = "management-api"
	}
	if provenance.SourceDigest == "" {
		bytes, marshalErr := json.Marshal(manualBook.Rules)
		if marshalErr != nil {
			return marshalErr
		}
		digest := sha256.Sum256(bytes)
		provenance.SourceDigest = fmt.Sprintf("%x", digest[:])
	}
	if provenance.SyncedAt.IsZero() {
		provenance.SyncedAt = now
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin pricing catalog update: %w", err)
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM pricing_catalog_rules"); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("clear pricing catalog rules: %w", err)
	}
	for index := range catalog.Rules {
		if err := insertPricingRule(ctx, tx, catalog.Rules[index], true); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("insert pricing catalog rule %d: %w", index, err)
		}
	}
	if err := writePricingProvenance(ctx, tx, provenance); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("record pricing catalog provenance: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit pricing catalog: %w", err)
	}
	// Publish the same catalog to the writer before releasing the store lock.
	// Previously ingested events retain their stored price until repricing.
	rules := make([]aggregate.PricingRule, 0, manualCount+len(catalog.Rules))
	for _, rule := range manualBook.Rules {
		if !rule.Catalog {
			rules = append(rules, rule)
		}
	}
	rules = append(rules, catalog.Rules...)
	s.config.PriceBook = aggregate.PriceBook{Rules: clonePricingRules(rules)}
	return nil
}

type modelsDevFetcher struct {
	client *http.Client
}

func newModelsDevFetcher(client *http.Client) PricingFetcher {
	if client == nil {
		client = &http.Client{}
	}
	return modelsDevFetcher{client: client}
}

type modelsDevProvider struct {
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	ID   string        `json:"id"`
	Cost modelsDevCost `json:"cost"`
}

type modelsDevCost struct {
	Input      json.RawMessage `json:"input"`
	Output     json.RawMessage `json:"output"`
	CacheRead  json.RawMessage `json:"cache_read"`
	CacheWrite json.RawMessage `json:"cache_write"`
}

func (f modelsDevFetcher) Fetch(ctx context.Context) (PricingCatalog, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsDevURL, nil)
	if err != nil {
		return PricingCatalog{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "CLIProxyAPI/models.dev-pricing")
	response, err := f.client.Do(request)
	if err != nil {
		return PricingCatalog{}, fmt.Errorf("fetch models.dev catalog: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return PricingCatalog{}, fmt.Errorf("models.dev returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxModelsDevBodyBytes+1))
	if err != nil {
		return PricingCatalog{}, fmt.Errorf("read models.dev catalog: %w", err)
	}
	if len(body) > maxModelsDevBodyBytes {
		return PricingCatalog{}, fmt.Errorf("models.dev catalog exceeds %d bytes", maxModelsDevBodyBytes)
	}
	var providers map[string]modelsDevProvider
	if err := json.Unmarshal(body, &providers); err != nil {
		return PricingCatalog{}, fmt.Errorf("decode models.dev catalog: %w", err)
	}
	rules := make([]aggregate.PricingRule, 0)
	for _, mapping := range modelsDevProviderMappings {
		provider, ok := providers[mapping.modelsDev]
		if !ok {
			continue
		}
		for modelID, entry := range provider.Models {
			if entry.ID == "" {
				entry.ID = modelID
			}
			rule, err := modelsDevRule(mapping.cpa, entry)
			if err != nil {
				return PricingCatalog{}, err
			}
			rules = append(rules, rule)
		}
	}
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].Provider != rules[j].Provider {
			return rules[i].Provider < rules[j].Provider
		}
		return rules[i].Model < rules[j].Model
	})
	digest := sha256.Sum256(body)
	return PricingCatalog{Rules: rules, Digest: fmt.Sprintf("%x", digest[:])}, nil
}

type modelsDevProviderMapping struct {
	cpa       string
	modelsDev string
}

// Provider mappings are deliberately explicit. Custom OpenAI-compatible
// providers and Antigravity are not guessed into a vendor catalog.
var modelsDevProviderMappings = []modelsDevProviderMapping{
	{cpa: "openai", modelsDev: "openai"},
	{cpa: "codex", modelsDev: "openai"},
	{cpa: "claude", modelsDev: "anthropic"},
	{cpa: "anthropic", modelsDev: "anthropic"},
	{cpa: "gemini", modelsDev: "google"},
	{cpa: "gemini-cli", modelsDev: "google"},
	{cpa: "gemini-interactions", modelsDev: "google"},
	{cpa: "aistudio", modelsDev: "google"},
	{cpa: "vertex", modelsDev: "google-vertex"},
	{cpa: "xai", modelsDev: "xai"},
}

func modelsDevRule(provider string, entry modelsDevModel) (aggregate.PricingRule, error) {
	input, inputOK, err := parseModelsDevPrice(entry.Cost.Input)
	if err != nil {
		return aggregate.PricingRule{}, err
	}
	output, outputOK, err := parseModelsDevPrice(entry.Cost.Output)
	if err != nil {
		return aggregate.PricingRule{}, err
	}
	var inputPtr, outputPtr *model.NanoUSD
	if inputOK && outputOK {
		inputPtr, outputPtr = &input, &output
	}
	rule := aggregate.PricingRule{Provider: provider, ID: "models.dev:" + provider + ":" + entry.ID,
		Model: entry.ID, InputPerMillion: inputPtr, OutputPerMillion: outputPtr,
		CacheReadMultiplier:     cacheMultiplier(entry.Cost.CacheRead, input, inputOK),
		CacheCreationMultiplier: cacheMultiplier(entry.Cost.CacheWrite, input, inputOK),
		Source:                  ModelsDevSource, Catalog: true}
	return rule, nil
}

func parseModelsDevPrice(raw json.RawMessage) (model.NanoUSD, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false, nil
	}
	value := strings.TrimSpace(string(raw))
	rational, ok := new(big.Rat).SetString(value)
	if !ok || rational.Sign() < 0 {
		return 0, false, fmt.Errorf("parse models.dev price %q: invalid nonnegative decimal", value)
	}
	rational.Mul(rational, big.NewRat(1_000_000_000, 1))
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(rational.Num(), rational.Denom(), remainder)
	if new(big.Int).Lsh(new(big.Int).Abs(remainder), 1).Cmp(rational.Denom()) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() || quotient.Sign() < 0 {
		return 0, false, fmt.Errorf("models.dev price %q is out of range", value)
	}
	return model.NanoUSD(quotient.Int64()), true, nil
}

func cacheMultiplier(raw json.RawMessage, input model.NanoUSD, inputOK bool) string {
	cache, ok, err := parseModelsDevPrice(raw)
	if err != nil || !ok || !inputOK || input == 0 {
		return ""
	}
	ratio := new(big.Rat).SetFrac(big.NewInt(int64(cache)), big.NewInt(int64(input)))
	return strings.TrimRight(strings.TrimRight(ratio.FloatString(18), "0"), ".")
}
