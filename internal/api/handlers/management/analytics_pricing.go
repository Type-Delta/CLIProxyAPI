package management

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
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
	Missing      []model.PricingMissing `json:"missing"`
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
	UpdatedAt               *time.Time            `json:"updated_at"`
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

type analyticsPricingMissingProvider interface {
	PricingMissing(context.Context, model.Range) ([]model.PricingMissing, error)
}

type analyticsProviderCredentialsProvider interface {
	ProviderCredentials(context.Context) ([]model.ProviderCredential, error)
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
			Rules: []analyticsPricingRule{}, Missing: []model.PricingMissing{}, SyncState: "not_configured",
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
			for index := range response.Rules {
				response.Rules[index].UpdatedAt = &updatedAt
			}
		}
	}
	if missingProvider, okMissing := service.(analyticsPricingMissingProvider); okMissing {
		now := time.Now().UTC()
		missing, errMissing := missingProvider.PricingMissing(c.Request.Context(), model.Range{Start: now.Add(-model.MaxQueryRangeDays * 24 * time.Hour), End: now, TimeZone: "UTC"})
		if errMissing != nil {
			writeAnalyticsError(c, classifyAnalyticsReadError(errMissing))
			return
		}
		response.Missing = missing
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
	if request.CurrencyUnit != "" && request.CurrencyUnit != "nano_usd" {
		writeAnalyticsInvalid(c, fmt.Errorf("unsupported currency_unit %q", request.CurrencyUnit))
		return
	}
	if request.Rounding != "" && request.Rounding != "half_away_from_zero_once_per_event" {
		writeAnalyticsInvalid(c, fmt.Errorf("unsupported rounding %q", request.Rounding))
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
			for index := range response.Rules {
				response.Rules[index].UpdatedAt = &updatedAt
			}
		}
	}
	setAnalyticsNoStore(c)
	c.JSON(http.StatusOK, response)
}

func pricingSnapshot(book aggregate.PriceBook, syncState string) analyticsPricingSnapshot {
	snapshot := analyticsPricingSnapshot{
		CurrencyUnit: "nano_usd", Rounding: "half_away_from_zero_once_per_event",
		Rules: make([]analyticsPricingRule, 0, len(book.Rules)), Missing: []model.PricingMissing{}, SyncState: syncState,
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

// PostAnalyticsReprice starts the resumable pricing maintenance operation.
// Range bounds are normalized here so a resumed job has stable selection
// semantics even if the named range would resolve differently later.
func (h *Handler) PostAnalyticsReprice(c *gin.Context) {
	var request struct {
		Range    *model.RangeRequest `json:"range,omitempty"`
		Start    time.Time           `json:"start,omitempty"`
		End      time.Time           `json:"end,omitempty"`
		TimeZone string              `json:"time_zone,omitempty"`
		DryRun   bool                `json:"dry_run"`
		Resume   bool                `json:"resume"`
	}
	if err := decodeAnalyticsJSON(c, &request, model.MaxQueryBodyBytes); err != nil {
		writeAnalyticsInvalid(c, err)
		return
	}
	query := model.Query{SchemaVersion: model.QuerySchemaVersionV2, Operation: model.OperationSummary,
		Range: request.Range, Start: request.Start, End: request.End, TimeZone: request.TimeZone}
	if query.Range != nil {
		if err := resolveAnalyticsRange(&query, time.Now().UTC()); err != nil {
			writeAnalyticsInvalid(c, err)
			return
		}
	}
	if err := query.Validate(); err != nil {
		writeAnalyticsInvalid(c, err)
		return
	}
	h.startAnalyticsJob(c, "reprice", map[string]any{
		"range":   model.Range{Start: query.Start.UTC(), End: query.End.UTC(), TimeZone: query.TimeZone},
		"dry_run": request.DryRun, "resume": request.Resume,
	})
}

type analyticsProviderStatus struct {
	Provider             string                     `json:"provider"`
	Credentials          int                        `json:"credentials"`
	CredentialRows       []model.ProviderCredential `json:"credential_rows,omitempty"`
	AvailableCredentials int                        `json:"available_credentials"`
	Unavailable          int                        `json:"unavailable_credentials"`
	LastObservedAt       *time.Time                 `json:"last_observed_at,omitempty"`
}

type analyticsQuotaStatus struct {
	Provider          string                     `json:"provider"`
	Credentials       int                        `json:"credentials"`
	QuotaExceeded     int                        `json:"quota_exceeded"`
	NextResetAt       *time.Time                 `json:"next_reset_at"`
	LastObservedAt    *time.Time                 `json:"last_observed_at,omitempty"`
	ObservationScoped bool                       `json:"observation_scoped"`
	CredentialRows    []model.ProviderCredential `json:"credential_rows,omitempty"`
}

func providerCredentialFromSnapshot(snapshot store.ProviderQuotaSnapshot) model.ProviderCredential {
	status := "unavailable"
	if snapshot.Available {
		status = "available"
	} else if snapshot.Disabled {
		status = "disabled"
	} else if snapshot.QuotaExceeded {
		status = "quota_exceeded"
	}
	credential := model.ProviderCredential{CredentialID: snapshot.CredentialID, Provider: snapshot.Provider, Status: status, ObservedAt: snapshot.ObservedAt}
	if snapshot.NextResetAt != nil {
		credential.Quota = &model.ProviderQuota{ResetsAt: snapshot.NextResetAt}
	}
	return credential
}

func (h *Handler) analyticsProviderRows(ctx context.Context, service cpauk.Service) ([]model.ProviderCredential, bool, error) {
	provider, ok := service.(analyticsProviderCredentialsProvider)
	if !ok {
		return nil, false, nil
	}
	rows, err := provider.ProviderCredentials(ctx)
	if err != nil {
		return nil, true, err
	}
	identityProvider, canHash := service.(analyticsProviderQuotaStore)
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil || !canHash {
		return rows, true, nil
	}
	byIdentity := make(map[string]model.ProviderCredential, len(rows))
	for _, row := range rows {
		byIdentity[row.Provider+"\x00"+row.CredentialID] = row
	}
	for _, credential := range manager.List() {
		if credential == nil {
			continue
		}
		credentialID, errID := identityProvider.CredentialID(credential.Provider, credential.Index, credential.ID)
		if errID != nil {
			return nil, true, errID
		}
		if credentialID == nil {
			continue
		}
		providerName := strings.ToLower(strings.TrimSpace(credential.Provider))
		if providerName == "" {
			providerName = "unknown"
		}
		key := providerName + "\x00" + *credentialID
		row := byIdentity[key]
		row.CredentialID, row.Provider, row.AuthType = *credentialID, providerName, credential.AuthKind()
		row.Status = string(credential.Status)
		switch {
		case credential.Disabled:
			row.Status = "disabled"
		case credential.Quota.Exceeded:
			row.Status = "quota_exceeded"
		case credential.Unavailable:
			row.Status = "unavailable"
		}
		if requests := credential.Success + credential.Failed; requests > row.Requests {
			row.Requests = requests
		}
		if credential.Failed > row.Failed {
			row.Failed = credential.Failed
		}
		if credential.LastError != nil && strings.TrimSpace(credential.LastError.Code) != "" {
			errorClass := strings.TrimSpace(credential.LastError.Code)
			if len(errorClass) > model.MaxStoredStringBytes {
				errorClass = errorClass[:model.MaxStoredStringBytes]
			}
			row.LastErrorClass = &errorClass
			if !credential.UpdatedAt.IsZero() {
				lastErrorAt := credential.UpdatedAt.UTC()
				row.LastErrorAt = &lastErrorAt
			}
		}
		observedAt := credential.Quota.ObservedAt.UTC()
		if observedAt.IsZero() {
			observedAt = credential.UpdatedAt.UTC()
		}
		if observedAt.After(row.ObservedAt) {
			row.ObservedAt = observedAt
		}
		row.Quota = observedProviderQuota(credential.Quota.Signals, credential.Quota.NextRecoverAt, row.Quota)
		byIdentity[key] = row
	}
	rows = rows[:0]
	for _, row := range byIdentity {
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(left, right model.ProviderCredential) int {
		if order := strings.Compare(left.Provider, right.Provider); order != 0 {
			return order
		}
		return strings.Compare(left.CredentialID, right.CredentialID)
	})
	return rows, true, nil
}

func observedProviderQuota(signals map[string]string, nextRecoverAt time.Time, existing *model.ProviderQuota) *model.ProviderQuota {
	var quota model.ProviderQuota
	if existing != nil {
		quota = *existing
	}
	if !nextRecoverAt.IsZero() {
		reset := nextRecoverAt.UTC()
		quota.ResetsAt = &reset
	}
	var limit, remaining *int64
	for name, raw := range signals {
		value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || value < 0 {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "x-ratelimit-limit-requests":
			parsed := value
			limit = &parsed
		case "x-ratelimit-remaining-requests":
			parsed := value
			remaining = &parsed
		}
	}
	if limit != nil {
		quota.Limit = limit
	}
	if remaining != nil {
		quota.Remaining = remaining
	}
	if limit != nil && remaining != nil {
		used := max(int64(0), *limit-*remaining)
		quota.Used = &used
	}
	if quota.Limit == nil && quota.Used == nil && quota.Remaining == nil && quota.ResetsAt == nil {
		return nil
	}
	return &quota
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
		providerRows, hasProviderRows, errRows := h.analyticsProviderRows(c.Request.Context(), service)
		if errRows != nil {
			writeAnalyticsError(c, classifyAnalyticsReadError(errRows))
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
			if !hasProviderRows {
				entry.CredentialRows = append(entry.CredentialRows, providerCredentialFromSnapshot(snapshot))
			}
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
		if hasProviderRows {
			for _, row := range providerRows {
				entry := byProvider[row.Provider]
				if entry == nil {
					entry = &analyticsProviderStatus{Provider: row.Provider}
					byProvider[row.Provider] = entry
				}
				entry.CredentialRows = append(entry.CredentialRows, row)
			}
			for _, entry := range byProvider {
				entry.Credentials = len(entry.CredentialRows)
				entry.AvailableCredentials, entry.Unavailable = 0, 0
				entry.LastObservedAt = nil
				for _, row := range entry.CredentialRows {
					if providerCredentialUnavailable(row.Status) {
						entry.Unavailable++
					} else {
						entry.AvailableCredentials++
					}
					if entry.LastObservedAt == nil || row.ObservedAt.After(*entry.LastObservedAt) {
						observed := row.ObservedAt
						entry.LastObservedAt = &observed
					}
				}
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
		providerRows, hasProviderRows, errRows := h.analyticsProviderRows(c.Request.Context(), service)
		if errRows != nil {
			writeAnalyticsError(c, classifyAnalyticsReadError(errRows))
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
			if !hasProviderRows {
				entry.CredentialRows = append(entry.CredentialRows, providerCredentialFromSnapshot(snapshot))
			}
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
		if hasProviderRows {
			for _, row := range providerRows {
				entry := byProvider[row.Provider]
				if entry == nil {
					entry = &analyticsQuotaStatus{Provider: row.Provider, ObservationScoped: true}
					byProvider[row.Provider] = entry
				}
				entry.CredentialRows = append(entry.CredentialRows, row)
			}
			for _, entry := range byProvider {
				entry.Credentials, entry.QuotaExceeded = len(entry.CredentialRows), 0
				entry.NextResetAt, entry.LastObservedAt = nil, nil
				for _, row := range entry.CredentialRows {
					if row.Status == "quota_exceeded" {
						entry.QuotaExceeded++
					}
					if row.Quota != nil && row.Quota.ResetsAt != nil && (entry.NextResetAt == nil || row.Quota.ResetsAt.Before(*entry.NextResetAt)) {
						reset := *row.Quota.ResetsAt
						entry.NextResetAt = &reset
					}
					if entry.LastObservedAt == nil || row.ObservedAt.After(*entry.LastObservedAt) {
						observed := row.ObservedAt
						entry.LastObservedAt = &observed
					}
				}
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

func providerCredentialUnavailable(status string) bool {
	switch status {
	case "disabled", "quota_exceeded", "unavailable", "error":
		return true
	default:
		return false
	}
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
	configuredLabels := map[string]string{}
	h.mu.Lock()
	if h.cfg != nil {
		catalog, _ := buildAPIKeyIdentityCatalog(h.cfg.APIKeys, config.APIKeyID)
		for _, identity := range catalog {
			configured[identity.KeyID] = identity
		}
		for _, entry := range h.cfg.APIKeys {
			labelNode, exists := entry.ExtensionFields["label"]
			if !exists {
				continue
			}
			var label string
			if err := labelNode.Decode(&label); err == nil {
				label = strings.TrimSpace(label)
				if label != "" && len(label) <= model.MaxStoredStringBytes {
					configuredLabels[config.APIKeyID(entry.Key)] = label
				}
			}
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
		keys[index].Label = configuredLabels[keys[index].KeyID]
	}
	setAnalyticsNoStore(c)
	if page.Meta.NextCursor != "" {
		c.Header("X-Analytics-Next-Cursor", page.Meta.NextCursor)
	}
	c.JSON(http.StatusOK, gin.H{"meta": page.Meta, "keys": keys})
}
