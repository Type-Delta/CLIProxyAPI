package chstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

const (
	modelsDevURL          = "https://models.dev/api.json"
	modelsDevV2URL        = "https://models.dev/experimental/v2.0/api.json"
	maxModelsDevBodyBytes = 32 << 20
)

func catalogBindingsDigest(bindings []CatalogBinding) string {
	canonical := canonicalBindings(bindings)
	encoded, _ := json.Marshal(canonical)
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", digest[:])
}

func pricingRefreshResult(book aggregate.PriceBook, provenance PricingProvenance, now time.Time) PricingRefreshResult {
	return PricingRefreshResult{Snapshot: pricingSnapshot(book, provenance), State: pricingState(book.Rules, provenance, now)}
}

func pricingSnapshot(book aggregate.PriceBook, provenance PricingProvenance) PricingSnapshot {
	snapshot := PricingSnapshot{KiroUSDPerCredit: book.KiroUSDPerCredit, Rules: clonePricingRules(book.Rules), Provenance: provenance}
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

type modelsDevFetcher struct {
	client   *http.Client
	store    *Store
	bindings []CatalogBinding
}

func newModelsDevFetcher(client *http.Client) PricingFetcher {
	if client == nil {
		client = &http.Client{}
	}
	return modelsDevFetcher{client: client}
}

type modelsDevProvider struct {
	Name   string                    `json:"name"`
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	ID           string        `json:"id"`
	Cost         modelsDevCost `json:"cost"`
	Experimental struct {
		Modes map[string]modelsDevMode `json:"modes"`
	} `json:"experimental"`
}

type modelsDevCost struct {
	Input      json.RawMessage `json:"input"`
	Output     json.RawMessage `json:"output"`
	CacheRead  json.RawMessage `json:"cache_read"`
	CacheWrite json.RawMessage `json:"cache_write"`
	Text       *struct {
		Input struct {
			Price json.RawMessage `json:"price"`
			Unit  string          `json:"unit"`
			Cache struct {
				Read     json.RawMessage `json:"read"`
				Write    json.RawMessage `json:"write"`
				Explicit struct {
					Write json.RawMessage `json:"write"`
				} `json:"explicit"`
			} `json:"cache"`
		} `json:"input"`
		Output struct {
			Price json.RawMessage `json:"price"`
			Unit  string          `json:"unit"`
		} `json:"output"`
	} `json:"text"`
	Multipliers []struct {
		When   map[string]json.RawMessage `json:"when"`
		Factor json.RawMessage            `json:"factor"`
	} `json:"multipliers"`
}

type modelsDevMode struct {
	ServiceTier string        `json:"service_tier"`
	Cost        modelsDevCost `json:"cost"`
	Body        struct {
		ServiceTier string `json:"service_tier"`
	} `json:"body"`
	Provider struct {
		Body struct {
			ServiceTier string `json:"service_tier"`
		} `json:"body"`
	} `json:"provider"`
}

func (f modelsDevFetcher) Fetch(ctx context.Context) (PricingCatalog, error) {
	providers, body, err := f.fetchProviders(ctx, modelsDevV2URL)
	if err != nil && ctx.Err() == nil {
		providers, body, err = f.fetchProviders(ctx, modelsDevURL)
	}
	if err != nil {
		return PricingCatalog{}, err
	}
	rules := make([]aggregate.PricingRule, 0)
	providersList := make([]CatalogProvider, 0, len(providers))
	for providerID, provider := range providers {
		name := strings.TrimSpace(provider.Name)
		if name == "" {
			name = providerID
		}
		providersList = append(providersList, CatalogProvider{ID: providerID, Name: name})
	}
	sort.Slice(providersList, func(i, j int) bool { return providersList[i].ID < providersList[j].ID })
	bindings := append([]CatalogBinding(nil), f.bindings...)
	if f.store != nil {
		f.store.mu.RLock()
		bindings = append(bindings, f.store.config.CatalogBindings...)
		f.store.mu.RUnlock()
	}
	bindings = canonicalBindings(bindings)
	explicitCatalogProviders := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		if binding.Catalog != "" {
			explicitCatalogProviders[binding.Provider] = struct{}{}
		}
	}
	for _, mapping := range modelsDevProviderMappings {
		if _, overridden := explicitCatalogProviders[mapping.cpa]; overridden {
			continue
		}
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
	for _, binding := range bindings {
		providerID := binding.Provider
		catalogID := binding.Catalog
		if catalogID == "" {
			catalogID = strings.TrimPrefix(providerID, "openai-compatible-")
		}
		provider, ok := providers[catalogID]
		if !ok || providerID == "" {
			continue
		}
		for modelID, entry := range provider.Models {
			if entry.ID == "" {
				entry.ID = modelID
			}
			rule, err := modelsDevRule(providerID, entry)
			if err != nil {
				return PricingCatalog{}, err
			}
			rules = append(rules, rule)
		}
	}
	kiroRules, err := kiroBedrockFallbackRules(providers[bedrockModelsDevProvider], rules)
	if err != nil {
		return PricingCatalog{}, err
	}
	rules = append(rules, kiroRules...)
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].Provider != rules[j].Provider {
			return rules[i].Provider < rules[j].Provider
		}
		return rules[i].Model < rules[j].Model
	})
	digestInput, err := json.Marshal(struct {
		Body     []byte
		Bindings []CatalogBinding
	}{Body: body, Bindings: bindings})
	if err != nil {
		return PricingCatalog{}, fmt.Errorf("encode models.dev catalog digest: %w", err)
	}
	digest := sha256.Sum256(digestInput)
	return PricingCatalog{Rules: rules, Digest: fmt.Sprintf("%x", digest[:]), Providers: providersList, BindingsDigest: catalogBindingsDigest(bindings)}, nil
}

func (f modelsDevFetcher) fetchProviders(ctx context.Context, url string) (map[string]modelsDevProvider, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "CLIProxyAPI/models.dev-pricing")
	response, err := f.client.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch models.dev catalog: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("models.dev returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxModelsDevBodyBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read models.dev catalog: %w", err)
	}
	if len(body) > maxModelsDevBodyBytes {
		return nil, nil, fmt.Errorf("models.dev catalog exceeds %d bytes", maxModelsDevBodyBytes)
	}
	var providers map[string]modelsDevProvider
	if err := json.Unmarshal(body, &providers); err != nil {
		return nil, nil, fmt.Errorf("decode models.dev catalog: %w", err)
	}
	if len(providers) == 0 {
		return nil, nil, fmt.Errorf("models.dev returned an empty catalog")
	}
	return providers, body, nil
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

const bedrockModelsDevProvider = "amazon-bedrock"

var bedrockVersionSuffix = regexp.MustCompile(`-v\d+(?::\d+)?$`)

// kiroBedrockFallbackRules prices Kiro's Claude models with Amazon Bedrock
// rates when no other catalog rule covers the model. Bedrock IDs such as
// "global.anthropic.claude-sonnet-4-5-20250929-v1:0" become the CPA model ID
// "claude-sonnet-4-5-20250929"; global pricing wins over the bare regional ID.
func kiroBedrockFallbackRules(bedrock modelsDevProvider, existing []aggregate.PricingRule) ([]aggregate.PricingRule, error) {
	covered := make(map[string]struct{})
	for _, rule := range existing {
		if rule.Provider == "kiro" && rule.Model != "" {
			covered[rule.Model] = struct{}{}
		}
	}
	type candidate struct {
		entry  modelsDevModel
		global bool
	}
	candidates := make(map[string]candidate)
	for modelID, entry := range bedrock.Models {
		if entry.ID == "" {
			entry.ID = modelID
		}
		bedrockID := entry.ID
		global := strings.HasPrefix(bedrockID, "global.")
		bedrockID = strings.TrimPrefix(bedrockID, "global.")
		if !strings.HasPrefix(bedrockID, "anthropic.") {
			continue
		}
		cpaModel := bedrockVersionSuffix.ReplaceAllString(strings.TrimPrefix(bedrockID, "anthropic."), "")
		if _, ok := covered[cpaModel]; ok {
			continue
		}
		if current, ok := candidates[cpaModel]; ok && (current.global || !global) {
			continue
		}
		candidates[cpaModel] = candidate{entry: entry, global: global}
	}
	rules := make([]aggregate.PricingRule, 0, len(candidates))
	for cpaModel, picked := range candidates {
		entry := picked.entry
		bedrockID := entry.ID
		entry.ID = cpaModel
		rule, err := modelsDevRule("kiro", entry)
		if err != nil {
			return nil, err
		}
		rule.ID = "models.dev:kiro:" + bedrockModelsDevProvider + ":" + bedrockID
		rules = append(rules, rule)
	}
	return rules, nil
}

func modelsDevRule(provider string, entry modelsDevModel) (aggregate.PricingRule, error) {
	cost, err := entry.Cost.tokenPrices()
	if err != nil {
		return aggregate.PricingRule{}, err
	}
	base, known, err := modelsDevTierPrice(cost)
	if err != nil {
		return aggregate.PricingRule{}, err
	}
	rule := aggregate.PricingRule{Provider: provider, ID: "models.dev:" + provider + ":" + entry.ID,
		Model:                   entry.ID,
		CacheReadMultiplier:     base.CacheReadMultiplier,
		CacheCreationMultiplier: base.CacheCreationMultiplier,
		Source:                  ModelsDevSource, Catalog: true}
	if known {
		rule.InputPerMillion, rule.OutputPerMillion = &base.InputPerMillion, &base.OutputPerMillion
	}
	tierCosts := make(map[string]modelsDevCost)
	for _, adjustment := range entry.Cost.Multipliers {
		// Other conditions need request metadata that token pricing does not have.
		if len(adjustment.When) != 1 {
			continue
		}
		var condition map[string]json.RawMessage
		if json.Unmarshal(adjustment.When["service_tier"], &condition) != nil || len(condition) != 1 {
			continue
		}
		var tier string
		if json.Unmarshal(condition["equals"], &tier) != nil || tier == "" {
			continue
		}
		previous, ok := tierCosts[tier]
		if !ok {
			previous = cost
		}
		next := previous
		for _, component := range []struct {
			price *json.RawMessage
			path  []string
		}{
			{&next.Input, []string{"text", "input", "price"}},
			{&next.Output, []string{"text", "output", "price"}},
			{&next.CacheRead, []string{"text", "input", "cache", "read"}},
			{&next.CacheWrite, []string{"text", "input", "cache", "write"}},
		} {
			if len(*component.price) == 0 || string(*component.price) == "null" {
				continue
			}
			factor, err := modelsDevFactor(adjustment.Factor, component.path)
			if err != nil {
				return aggregate.PricingRule{}, fmt.Errorf("models.dev model %s service tier %s: %w", entry.ID, tier, err)
			}
			value, ok := new(big.Rat).SetString(string(*component.price))
			if !ok {
				return aggregate.PricingRule{}, fmt.Errorf("models.dev model %s service tier %s has invalid token price %q", entry.ID, tier, *component.price)
			}
			*component.price = json.RawMessage(value.Mul(value, factor).FloatString(30))
		}
		tierCosts[tier] = next
	}
	for name, mode := range entry.Experimental.Modes {
		tier := mode.ServiceTier
		if tier == "" {
			tier = mode.Body.ServiceTier
		}
		if tier == "" {
			tier = mode.Provider.Body.ServiceTier
		}
		if tier == "" {
			tier = name
		}
		modeCost, err := mode.Cost.tokenPrices()
		if err != nil {
			return aggregate.PricingRule{}, err
		}
		if _, known, err := modelsDevTierPrice(modeCost); err != nil {
			return aggregate.PricingRule{}, err
		} else if known {
			tierCosts[tier] = modeCost
		}
	}
	for tier, cost := range tierCosts {
		price, known, err := modelsDevTierPrice(cost)
		if err != nil {
			return aggregate.PricingRule{}, err
		}
		if known {
			if rule.ServiceTierPrices == nil {
				rule.ServiceTierPrices = make(map[string]aggregate.ServiceTierPrice)
			}
			rule.ServiceTierPrices[strings.ToLower(strings.TrimSpace(tier))] = price
		}
	}
	return rule, nil
}

// tokenPrices normalizes v2 text rates and v1 token rates at the catalog boundary.
func (cost modelsDevCost) tokenPrices() (modelsDevCost, error) {
	if cost.Text == nil {
		return cost, nil
	}
	text := cost.Text
	if (text.Input.Unit != "" && text.Input.Unit != "token") || (text.Output.Unit != "" && text.Output.Unit != "token") {
		return modelsDevCost{}, nil
	}
	write := text.Input.Cache.Write
	if len(write) == 0 {
		write = text.Input.Cache.Explicit.Write
	}
	if bytes.HasPrefix(bytes.TrimSpace(write), []byte("{")) {
		// Object order identifies the provider's default cache TTL in models.dev.
		decoder := json.NewDecoder(bytes.NewReader(write))
		_, _ = decoder.Token()
		if decoder.More() {
			_, _ = decoder.Token()
			var defaultPrice json.RawMessage
			if err := decoder.Decode(&defaultPrice); err != nil {
				return modelsDevCost{}, fmt.Errorf("decode models.dev cache write price: %w", err)
			}
			write = defaultPrice
		} else {
			write = nil
		}
	}
	return modelsDevCost{Input: text.Input.Price, Output: text.Output.Price, CacheRead: text.Input.Cache.Read, CacheWrite: write}, nil
}

func modelsDevTierPrice(cost modelsDevCost) (aggregate.ServiceTierPrice, bool, error) {
	input, inputOK, err := parseModelsDevPrice(cost.Input)
	if err != nil {
		return aggregate.ServiceTierPrice{}, false, err
	}
	output, outputOK, err := parseModelsDevPrice(cost.Output)
	if err != nil {
		return aggregate.ServiceTierPrice{}, false, err
	}
	return aggregate.ServiceTierPrice{InputPerMillion: input, OutputPerMillion: output,
		CacheReadMultiplier:     cacheMultiplier(cost.CacheRead, input, inputOK),
		CacheCreationMultiplier: cacheMultiplier(cost.CacheWrite, input, inputOK)}, inputOK && outputOK, nil
}

func modelsDevFactor(raw json.RawMessage, path []string) (*big.Rat, error) {
	for bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		if len(path) == 0 {
			return big.NewRat(1, 1), nil
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
		raw, path = fields[path[0]], path[1:]
		if len(raw) == 0 {
			return big.NewRat(1, 1), nil
		}
	}
	value, ok := new(big.Rat).SetString(strings.TrimSpace(string(raw)))
	if !ok || value.Sign() < 0 {
		return nil, fmt.Errorf("invalid nonnegative models.dev price factor %q", raw)
	}
	return value, nil
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

type pricingFlight struct {
	done   chan struct{}
	result PricingRefreshResult
}

var pricingFlights sync.Map

func (s *Store) RefreshPricing(ctx context.Context) (PricingRefreshResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil {
		return PricingRefreshResult{State: "unavailable", Err: ErrClosed}, ErrClosed
	}
	now := time.Now().UTC()
	if s.config.PricingNow != nil {
		now = s.config.PricingNow().UTC()
	}
	book, p, err := s.PricingRules(ctx)
	if err != nil {
		return PricingRefreshResult{State: "unavailable", Err: err}, err
	}
	result := pricingRefreshResult(book, p, now)
	should := func(r PricingRefreshResult) bool {
		return (r.Snapshot.Provenance.CatalogRetryAt.IsZero() || !now.Before(r.Snapshot.Provenance.CatalogRetryAt)) && (r.State != "fresh" || r.Snapshot.Provenance.CatalogBindingsDigest != s.catalogBindingsDigest())
	}
	if !should(result) {
		return result, nil
	}
	flight := &pricingFlight{done: make(chan struct{})}
	actual, loaded := pricingFlights.LoadOrStore(s, flight)
	if loaded {
		other := actual.(*pricingFlight)
		select {
		case <-other.done:
			return other.result, other.result.Err
		case <-ctx.Done():
			return result, ctx.Err()
		}
	}
	defer func() { flight.result = result; close(flight.done); pricingFlights.Delete(s) }()
	book, p, err = s.PricingRules(ctx)
	if err != nil {
		result.Err = err
		return result, err
	}
	result = pricingRefreshResult(book, p, now)
	if !should(result) {
		return result, nil
	}
	fetcher := s.config.PricingFetcher
	if fetcher == nil {
		fetcher = modelsDevFetcher{client: &http.Client{}, store: s}
	}
	catalog, fetchErr := fetcher.Fetch(ctx)
	result.Attempted = true
	if fetchErr == nil {
		fetchErr = validateFetchedCatalog(catalog.Rules, catalog.Digest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		result.Err = ErrClosed
		return result, result.Err
	}
	snapshot, values, err := s.loadPricing(ctx)
	if err != nil {
		result.Err = err
		return result, err
	}
	p = snapshot.Provenance
	if fetchErr != nil {
		p.CatalogRetryAt = now.Add(PricingRetrySuppression)
		p.CatalogLastError = "fetch_failed"
		if p.Source == "" {
			p.Source = "management-api"
		}
		if p.SourceDigest == "" {
			digest := sha256.Sum256(nil)
			p.SourceDigest = fmt.Sprintf("%x", digest[:])
		}
		if p.SyncedAt.IsZero() {
			p.SyncedAt = now
		}
		snapshot.Provenance = p
		_ = s.publishPricing(ctx, snapshot, values)
		result.Err = fetchErr
		return result, fetchErr
	}
	providersSeen := make(map[string]struct{}, len(catalog.Providers))
	for _, provider := range catalog.Providers {
		if _, duplicate := providersSeen[provider.ID]; duplicate {
			result.Err = fmt.Errorf("duplicate pricing catalog provider %q", provider.ID)
			return result, result.Err
		}
		providersSeen[provider.ID] = struct{}{}
	}
	if len(snapshot.Overrides)+len(catalog.Rules) > MaxPricingRules {
		result.Err = fmt.Errorf("pricing catalog exceeds %d rules", MaxPricingRules)
		return result, result.Err
	}
	p.CatalogSource = ModelsDevSource
	p.CatalogDigest = catalog.Digest
	p.CatalogSyncedAt = now
	p.CatalogExpiresAt = now.Add(PricingCatalogTTL)
	p.CatalogRetryAt = time.Time{}
	p.CatalogLastError = ""
	p.CatalogBindingsDigest = catalog.BindingsDigest
	if p.CatalogBindingsDigest == "" {
		p.CatalogBindingsDigest = catalogBindingsDigest(s.config.CatalogBindings)
	}
	if p.Source == "" {
		p.Source = "management-api"
	}
	if p.SourceDigest == "" {
		raw, _ := json.Marshal(snapshot.Overrides)
		digest := sha256.Sum256(raw)
		p.SourceDigest = fmt.Sprintf("%x", digest[:])
	}
	if p.SyncedAt.IsZero() {
		p.SyncedAt = now
	}
	snapshot.Rules = append(clonePricingRules(snapshot.Overrides), clonePricingRules(catalog.Rules)...)
	snapshot.Provenance = p
	raw, err := json.Marshal(catalog.Providers)
	if err != nil {
		result.Err = err
		return result, err
	}
	values["providers"] = raw
	if err = s.publishPricing(ctx, snapshot, values); err != nil {
		result.Err = err
		return result, err
	}
	result.Snapshot = pricingSnapshot(s.config.PriceBook, p)
	result.State = "fresh"
	return result, nil
}
func (s *Store) catalogBindingsDigest() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return catalogBindingsDigest(s.config.CatalogBindings)
}
