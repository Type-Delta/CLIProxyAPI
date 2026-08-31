package management

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/store"
)

type analyticsPricingSnapshot struct {
	CurrencyUnit string                 `json:"currency_unit"`
	Rounding     string                 `json:"rounding"`
	Rules        []analyticsPricingRule `json:"rules"`
	SyncState    string                 `json:"sync_state"`
	UpdatedAt    *time.Time             `json:"updated_at"`
}

type analyticsPricingRule struct {
	RuleID                  string                `json:"rule_id"`
	Match                   analyticsPricingMatch `json:"match"`
	InputPerMillion         *model.NanoUSD        `json:"input_per_million_usd"`
	OutputPerMillion        *model.NanoUSD        `json:"output_per_million_usd"`
	CacheReadMultiplier     string                `json:"cache_read_multiplier,omitempty"`
	CacheCreationMultiplier string                `json:"cache_creation_multiplier,omitempty"`
	Source                  string                `json:"source"`
}

type analyticsPricingMatch struct {
	Model string `json:"model,omitempty"`
	Alias string `json:"alias,omitempty"`
}

type analyticsPricingProvider interface {
	PriceBook(context.Context) (aggregate.PriceBook, error)
	UpdatePriceBook(context.Context, aggregate.PriceBook) (aggregate.PriceBook, error)
}

type analyticsPricingSnapshotProvider interface {
	PricingSnapshot(context.Context) (store.PricingSnapshot, error)
}

func (h *Handler) GetAnalyticsPricing(c *gin.Context) {
	service, err := h.analyticsServiceForRead()
	if err != nil {
		writeAnalyticsError(c, err)
		return
	}
	provider, ok := service.(analyticsPricingProvider)
	if !ok {
		setAnalyticsNoStore(c)
		c.JSON(http.StatusOK, analyticsPricingSnapshot{
			CurrencyUnit: "nano_usd", Rounding: "half_away_from_zero_once_per_event",
			Rules: []analyticsPricingRule{}, SyncState: "not_configured",
		})
		return
	}
	book, err := provider.PriceBook(c.Request.Context())
	if err != nil {
		writeAnalyticsError(c, classifyAnalyticsReadError(err))
		return
	}
	response := pricingSnapshot(book, "ready")
	if snapshots, okSnapshots := service.(analyticsPricingSnapshotProvider); okSnapshots {
		durable, errSnapshot := snapshots.PricingSnapshot(c.Request.Context())
		if errSnapshot != nil {
			writeAnalyticsError(c, classifyAnalyticsReadError(errSnapshot))
			return
		}
		if !durable.Provenance.SyncedAt.IsZero() {
			updatedAt := durable.Provenance.SyncedAt.UTC()
			response.UpdatedAt = &updatedAt
		}
	}
	setAnalyticsNoStore(c)
	c.JSON(http.StatusOK, response)
}

func (h *Handler) PutAnalyticsPricing(c *gin.Context) {
	var request analyticsPricingSnapshot
	if err := decodeAnalyticsJSON(c, &request, model.MaxQueryBodyBytes); err != nil || len(request.Rules) > 10_000 {
		writeAnalyticsInvalid(c, err)
		return
	}
	book := aggregate.PriceBook{Rules: make([]aggregate.PricingRule, 0, len(request.Rules))}
	seenRuleIDs := make(map[string]struct{}, len(request.Rules))
	seenMatches := make(map[string]struct{}, len(request.Rules))
	for _, rule := range request.Rules {
		converted := aggregate.PricingRule{
			ID: rule.RuleID, Model: rule.Match.Model, Alias: rule.Match.Alias,
			InputPerMillion: rule.InputPerMillion, OutputPerMillion: rule.OutputPerMillion,
			CacheReadMultiplier: rule.CacheReadMultiplier, CacheCreationMultiplier: rule.CacheCreationMultiplier,
			Source: rule.Source,
		}
		if err := converted.Validate(); err != nil {
			writeAnalyticsInvalid(c, err)
			return
		}
		matchKey := "model\x00" + converted.Model
		if converted.Alias != "" {
			matchKey = "alias\x00" + converted.Alias
		}
		if _, duplicate := seenRuleIDs[converted.ID]; duplicate {
			writeAnalyticsInvalid(c, fmt.Errorf("duplicate pricing rule ID"))
			return
		}
		if _, duplicate := seenMatches[matchKey]; duplicate {
			writeAnalyticsInvalid(c, fmt.Errorf("duplicate pricing match"))
			return
		}
		seenRuleIDs[converted.ID] = struct{}{}
		seenMatches[matchKey] = struct{}{}
		book.Rules = append(book.Rules, converted)
	}
	service, err := h.analyticsServiceForRead()
	if err != nil {
		writeAnalyticsError(c, err)
		return
	}
	provider, ok := service.(analyticsPricingProvider)
	if !ok {
		writeAnalyticsError(c, cpauk.ErrUnavailable)
		return
	}
	result, err := provider.UpdatePriceBook(c.Request.Context(), book)
	if err != nil {
		writeAnalyticsError(c, classifyAnalyticsReadError(err))
		return
	}
	response := pricingSnapshot(result, "ready")
	if snapshots, okSnapshots := service.(analyticsPricingSnapshotProvider); okSnapshots {
		if durable, errSnapshot := snapshots.PricingSnapshot(c.Request.Context()); errSnapshot == nil && !durable.Provenance.SyncedAt.IsZero() {
			updatedAt := durable.Provenance.SyncedAt.UTC()
			response.UpdatedAt = &updatedAt
		}
	}
	setAnalyticsNoStore(c)
	c.JSON(http.StatusOK, response)
}

func pricingSnapshot(book aggregate.PriceBook, syncState string) analyticsPricingSnapshot {
	snapshot := analyticsPricingSnapshot{
		CurrencyUnit: "nano_usd", Rounding: "half_away_from_zero_once_per_event",
		Rules: make([]analyticsPricingRule, 0, len(book.Rules)), SyncState: syncState,
	}
	for _, rule := range book.Rules {
		snapshot.Rules = append(snapshot.Rules, analyticsPricingRule{
			RuleID: rule.ID, Match: analyticsPricingMatch{Model: rule.Model, Alias: rule.Alias},
			InputPerMillion: rule.InputPerMillion, OutputPerMillion: rule.OutputPerMillion,
			CacheReadMultiplier: rule.CacheReadMultiplier, CacheCreationMultiplier: rule.CacheCreationMultiplier,
			Source: rule.Source,
		})
	}
	return snapshot
}

type analyticsProviderStatus struct {
	Provider             string     `json:"provider"`
	Credentials          int        `json:"credentials"`
	AvailableCredentials int        `json:"available_credentials"`
	Unavailable          int        `json:"unavailable_credentials"`
	LastObservedAt       *time.Time `json:"last_observed_at,omitempty"`
}

type analyticsQuotaStatus struct {
	Provider          string     `json:"provider"`
	Credentials       int        `json:"credentials"`
	QuotaExceeded     int        `json:"quota_exceeded"`
	NextResetAt       *time.Time `json:"next_reset_at"`
	LastObservedAt    *time.Time `json:"last_observed_at,omitempty"`
	ObservationScoped bool       `json:"observation_scoped"`
}

type analyticsProviderQuotaStore interface {
	CredentialID(provider, authIndex, authID string) (*string, error)
	ReplaceProviderQuotaSnapshots(context.Context, []store.ProviderQuotaSnapshot) error
	ProviderQuotaSnapshots(context.Context) ([]store.ProviderQuotaSnapshot, error)
}

func (h *Handler) providerQuotaSnapshots(ctx context.Context, service cpauk.Service) ([]store.ProviderQuotaSnapshot, bool, error) {
	provider, ok := service.(analyticsProviderQuotaStore)
	if !ok {
		return nil, false, nil
	}
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager != nil {
		now := time.Now().UTC()
		byIdentity := make(map[string]store.ProviderQuotaSnapshot)
		for _, credential := range manager.List() {
			if credential == nil {
				continue
			}
			credentialID, err := provider.CredentialID(credential.Provider, credential.Index, credential.ID)
			if err != nil {
				return nil, true, err
			}
			if credentialID == nil {
				continue
			}
			providerName := strings.ToLower(strings.TrimSpace(credential.Provider))
			if providerName == "" {
				providerName = "unknown"
			}
			var nextResetAt *time.Time
			if !credential.Quota.NextRecoverAt.IsZero() {
				value := credential.Quota.NextRecoverAt.UTC()
				nextResetAt = &value
			}
			observedAt := credential.UpdatedAt.UTC()
			if observedAt.IsZero() {
				observedAt = now
			}
			snapshot := store.ProviderQuotaSnapshot{
				Provider: providerName, CredentialID: *credentialID,
				Available: !credential.Disabled && !credential.Unavailable && !credential.Quota.Exceeded,
				Disabled:  credential.Disabled, QuotaExceeded: credential.Quota.Exceeded,
				NextResetAt: nextResetAt, ObservedAt: observedAt,
			}
			identity := snapshot.Provider + "\x00" + snapshot.CredentialID
			if previous, duplicate := byIdentity[identity]; duplicate {
				snapshot.Available = snapshot.Available && previous.Available
				snapshot.Disabled = snapshot.Disabled || previous.Disabled
				snapshot.QuotaExceeded = snapshot.QuotaExceeded || previous.QuotaExceeded
				if previous.NextResetAt != nil && (snapshot.NextResetAt == nil || previous.NextResetAt.Before(*snapshot.NextResetAt)) {
					snapshot.NextResetAt = previous.NextResetAt
				}
				if previous.ObservedAt.After(snapshot.ObservedAt) {
					snapshot.ObservedAt = previous.ObservedAt
				}
			}
			byIdentity[identity] = snapshot
		}
		snapshots := make([]store.ProviderQuotaSnapshot, 0, len(byIdentity))
		for _, snapshot := range byIdentity {
			snapshots = append(snapshots, snapshot)
		}
		if err := provider.ReplaceProviderQuotaSnapshots(ctx, snapshots); err != nil {
			return nil, true, err
		}
	}
	snapshots, err := provider.ProviderQuotaSnapshots(ctx)
	return snapshots, true, err
}

func (h *Handler) GetAnalyticsProviders(c *gin.Context) {
	service, err := h.analyticsServiceForRead()
	if err != nil {
		writeAnalyticsError(c, err)
		return
	}
	if snapshots, durable, errSnapshots := h.providerQuotaSnapshots(c.Request.Context(), service); durable {
		if errSnapshots != nil {
			writeAnalyticsError(c, classifyAnalyticsReadError(errSnapshots))
			return
		}
		byProvider := map[string]*analyticsProviderStatus{}
		for _, snapshot := range snapshots {
			entry := byProvider[snapshot.Provider]
			if entry == nil {
				entry = &analyticsProviderStatus{Provider: snapshot.Provider}
				byProvider[snapshot.Provider] = entry
			}
			entry.Credentials++
			if snapshot.Available {
				entry.AvailableCredentials++
			} else {
				entry.Unavailable++
			}
			if entry.LastObservedAt == nil || snapshot.ObservedAt.After(*entry.LastObservedAt) {
				observed := snapshot.ObservedAt
				entry.LastObservedAt = &observed
			}
		}
		providers := make([]analyticsProviderStatus, 0, len(byProvider))
		for _, entry := range byProvider {
			providers = append(providers, *entry)
		}
		slices.SortFunc(providers, func(a, b analyticsProviderStatus) int { return strings.Compare(a.Provider, b.Provider) })
		setAnalyticsNoStore(c)
		c.JSON(http.StatusOK, gin.H{"providers": providers, "storage_scope": "instance", "durable": true})
		return
	}
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	byProvider := map[string]*analyticsProviderStatus{}
	if manager != nil {
		for _, credential := range manager.List() {
			if credential == nil {
				continue
			}
			name := strings.ToLower(strings.TrimSpace(credential.Provider))
			if name == "" {
				name = "unknown"
			}
			entry := byProvider[name]
			if entry == nil {
				entry = &analyticsProviderStatus{Provider: name}
				byProvider[name] = entry
			}
			entry.Credentials++
			if credential.Disabled || credential.Unavailable || credential.Quota.Exceeded {
				entry.Unavailable++
			} else {
				entry.AvailableCredentials++
			}
		}
	}
	providers := make([]analyticsProviderStatus, 0, len(byProvider))
	for _, entry := range byProvider {
		providers = append(providers, *entry)
	}
	slices.SortFunc(providers, func(a, b analyticsProviderStatus) int { return strings.Compare(a.Provider, b.Provider) })
	setAnalyticsNoStore(c)
	c.JSON(http.StatusOK, gin.H{"providers": providers, "storage_scope": "instance"})
}

func (h *Handler) GetAnalyticsQuotas(c *gin.Context) {
	service, err := h.analyticsServiceForRead()
	if err != nil {
		writeAnalyticsError(c, err)
		return
	}
	if snapshots, durable, errSnapshots := h.providerQuotaSnapshots(c.Request.Context(), service); durable {
		if errSnapshots != nil {
			writeAnalyticsError(c, classifyAnalyticsReadError(errSnapshots))
			return
		}
		byProvider := map[string]*analyticsQuotaStatus{}
		for _, snapshot := range snapshots {
			entry := byProvider[snapshot.Provider]
			if entry == nil {
				entry = &analyticsQuotaStatus{Provider: snapshot.Provider, ObservationScoped: true}
				byProvider[snapshot.Provider] = entry
			}
			entry.Credentials++
			if snapshot.QuotaExceeded {
				entry.QuotaExceeded++
			}
			if snapshot.NextResetAt != nil && (entry.NextResetAt == nil || snapshot.NextResetAt.Before(*entry.NextResetAt)) {
				next := *snapshot.NextResetAt
				entry.NextResetAt = &next
			}
			if entry.LastObservedAt == nil || snapshot.ObservedAt.After(*entry.LastObservedAt) {
				observed := snapshot.ObservedAt
				entry.LastObservedAt = &observed
			}
		}
		quotas := make([]analyticsQuotaStatus, 0, len(byProvider))
		for _, entry := range byProvider {
			quotas = append(quotas, *entry)
		}
		slices.SortFunc(quotas, func(a, b analyticsQuotaStatus) int { return strings.Compare(a.Provider, b.Provider) })
		setAnalyticsNoStore(c)
		c.JSON(http.StatusOK, gin.H{"quotas": quotas, "shared_enforcement": false, "durable": true})
		return
	}
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	byProvider := map[string]*analyticsQuotaStatus{}
	if manager != nil {
		for _, credential := range manager.List() {
			if credential == nil {
				continue
			}
			name := strings.ToLower(strings.TrimSpace(credential.Provider))
			if name == "" {
				name = "unknown"
			}
			entry := byProvider[name]
			if entry == nil {
				entry = &analyticsQuotaStatus{Provider: name, ObservationScoped: true}
				byProvider[name] = entry
			}
			entry.Credentials++
			if credential.Quota.Exceeded {
				entry.QuotaExceeded++
			}
			if !credential.Quota.NextRecoverAt.IsZero() && (entry.NextResetAt == nil || credential.Quota.NextRecoverAt.Before(*entry.NextResetAt)) {
				next := credential.Quota.NextRecoverAt.UTC()
				entry.NextResetAt = &next
			}
		}
	}
	quotas := make([]analyticsQuotaStatus, 0, len(byProvider))
	for _, entry := range byProvider {
		quotas = append(quotas, *entry)
	}
	slices.SortFunc(quotas, func(a, b analyticsQuotaStatus) int { return strings.Compare(a.Provider, b.Provider) })
	setAnalyticsNoStore(c)
	c.JSON(http.StatusOK, gin.H{"quotas": quotas, "shared_enforcement": false})
}

func (h *Handler) GetAnalyticsKeys(c *gin.Context) {
	values := c.Request.URL.Query()
	if dimension := values.Get("dimension"); dimension != "" && dimension != "key" {
		writeAnalyticsInvalid(c, fmt.Errorf("keys catalog dimension must be key"))
		return
	}
	values.Set("dimension", "key")
	if values.Get("cursor") != "" {
		writeAnalyticsInvalid(c, fmt.Errorf("key catalog cursor must use X-Analytics-Cursor"))
		return
	}
	query, err := analyticsGETQuery(values, model.OperationDimensions)
	if err != nil {
		writeAnalyticsInvalid(c, err)
		return
	}
	query.Cursor = strings.TrimSpace(c.GetHeader("X-Analytics-Cursor"))
	if len(query.Cursor) > model.MaxCursorBytes {
		writeAnalyticsInvalid(c, fmt.Errorf("key catalog cursor exceeds its bounds"))
		return
	}
	service := h.analyticsService()
	if service == nil {
		writeAnalyticsError(c, cpauk.ErrUnavailable)
		return
	}
	provider, ok := service.(interface {
		KeyCatalog(context.Context, model.Query) (store.KeyCatalogPage, error)
	})
	if !ok {
		writeAnalyticsError(c, cpauk.ErrUnavailable)
		return
	}
	page, err := provider.KeyCatalog(c.Request.Context(), query)
	if err != nil {
		writeAnalyticsError(c, classifyAnalyticsReadError(err))
		return
	}
	configured := map[string]apiKeyIdentityEntry{}
	h.mu.Lock()
	if h.cfg != nil {
		catalog, _ := buildAPIKeyIdentityCatalog(h.cfg.APIKeys, config.APIKeyID)
		for _, identity := range catalog {
			configured[identity.KeyID] = identity
		}
	}
	h.mu.Unlock()
	keys := page.Keys
	for index := range keys {
		identity, exists := configured[keys[index].KeyID]
		if !exists {
			continue
		}
		keys[index].Status = model.KeyStatusConfigured
		if identity.Status == "identity_conflict" {
			keys[index].Status = model.KeyStatusConflict
		}
		keys[index].ConfigIndexes = slices.Clone(identity.ConfigIndexes)
	}
	setAnalyticsNoStore(c)
	if page.Meta.NextCursor != "" {
		c.Header("X-Analytics-Next-Cursor", page.Meta.NextCursor)
	}
	c.JSON(http.StatusOK, gin.H{"meta": page.Meta, "keys": keys})
}
