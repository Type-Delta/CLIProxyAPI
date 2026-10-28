// Package chstore implements CPAUK persistence on the shared ClickHouse database.
package chstore

import (
	"slices"
	"strings"

	store "github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/backend"
)

// Aliases share the neutral backend contract with legacy import and service callers.
type Config = store.Config
type CatalogBinding = store.CatalogBinding
type CatalogProvider = store.CatalogProvider
type PricingProvenance = store.PricingProvenance
type PricingSnapshot = store.PricingSnapshot
type PricingRefreshResult = store.PricingRefreshResult
type PricingFetcher = store.PricingFetcher
type PricingCatalog = store.PricingCatalog
type ProviderQuotaSnapshot = store.ProviderQuotaSnapshot
type QuotaHistorySample = store.QuotaHistorySample
type KeyCatalogPage = store.KeyCatalogPage
type RepriceOptions = store.RepriceOptions
type RepriceResult = store.RepriceResult
type RetentionResult = store.RetentionResult
type BackupManifest = store.BackupManifest
type RetainedRangeError = store.RetainedRangeError
type RetainedTimeZoneError = store.RetainedTimeZoneError
type ZoneMismatchError = store.ZoneMismatchError

var (
	ErrIdentityKeyMissing      = store.ErrIdentityKeyMissing
	ErrIdentityKeyMismatch     = store.ErrIdentityKeyMismatch
	ErrClosed                  = store.ErrClosed
	ErrStorageQuota            = store.ErrStorageQuota
	ErrInsufficientFreeSpace   = store.ErrInsufficientFreeSpace
	ErrRetainedRangePartial    = store.ErrRetainedRangePartial
	ErrRetainedTimeZonePartial = store.ErrRetainedTimeZonePartial
	ErrUnsupportedSchema       = store.ErrUnsupportedSchema
	ErrCorruptDatabase         = store.ErrCorruptDatabase
	ErrMigrationChecksum       = store.ErrMigrationChecksum
	ErrRepriceCatalogChanged   = store.ErrRepriceCatalogChanged
)

func normalizeConfig(c *Config) error {
	if err := c.Normalize(); err != nil {
		return err
	}
	c.CatalogBindings = canonicalBindings(c.CatalogBindings)
	return nil
}

func canonicalBindings(bindings []CatalogBinding) []CatalogBinding {
	result := make([]CatalogBinding, 0, len(bindings))
	seen := make(map[string]bool, len(bindings))
	for _, binding := range bindings {
		binding.Provider = strings.ToLower(strings.TrimSpace(binding.Provider))
		binding.Catalog = strings.ToLower(strings.TrimSpace(binding.Catalog))
		if binding.Provider == "" || seen[binding.Provider] {
			continue
		}
		seen[binding.Provider] = true
		result = append(result, binding)
	}
	slices.SortFunc(result, func(a, b CatalogBinding) int { return strings.Compare(a.Provider, b.Provider) })
	return result
}
