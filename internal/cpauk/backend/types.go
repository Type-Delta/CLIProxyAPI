package backend

import (
	"context"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

type PricingProvenance struct {
	Source                string
	SourceDigest          string
	SyncedAt              time.Time
	CatalogSource         string
	CatalogDigest         string
	CatalogSyncedAt       time.Time
	CatalogExpiresAt      time.Time
	CatalogRetryAt        time.Time
	CatalogLastError      string
	CatalogBindingsDigest string
}
type PricingSnapshot struct {
	KiroUSDPerCredit *model.NanoUSD
	Rules            []aggregate.PricingRule
	Overrides        []aggregate.PricingRule
	Catalog          []aggregate.PricingRule
	Provenance       PricingProvenance
}
type PricingRefreshResult struct {
	Snapshot  PricingSnapshot
	State     string
	Attempted bool
	Err       error
}
type PricingFetcher interface {
	Fetch(context.Context) (PricingCatalog, error)
}
type PricingCatalog struct {
	Rules     []aggregate.PricingRule
	Digest    string
	Providers []CatalogProvider
	// BindingsDigest identifies the catalog bindings the rules were built
	// from so a reconfigure during an in-flight fetch still triggers a refetch.
	BindingsDigest string
}
type CatalogProvider struct {
	ID   string
	Name string
}
type PricingCatalogStore interface {
	PriceBook(context.Context) (aggregate.PriceBook, error)
	UpdatePriceBook(context.Context, aggregate.PriceBook) (aggregate.PriceBook, error)
	PricingRules(context.Context) (aggregate.PriceBook, PricingProvenance, error)
	PricingSnapshot(context.Context) (PricingSnapshot, error)
	ReplacePricingRules(context.Context, []aggregate.PricingRule, PricingProvenance) error
}
type ProviderQuotaSnapshot struct {
	Provider      string
	CredentialID  string
	Model         string
	Available     bool
	Disabled      bool
	QuotaExceeded bool
	NextResetAt   *time.Time
	ObservedAt    time.Time
}
type ProviderQuotaStore interface {
	ReplaceProviderQuotaSnapshots(context.Context, []ProviderQuotaSnapshot) error
	ProviderQuotaSnapshots(context.Context) ([]ProviderQuotaSnapshot, error)
	ProviderCredentials(context.Context) ([]model.ProviderCredential, error)
}
type QuotaHistorySample struct {
	Provider         string     `json:"provider"`
	CredentialID     string     `json:"credential_id"`
	WindowID         string     `json:"window_id"`
	ObservedAt       time.Time  `json:"observed_at"`
	RemainingPercent float64    `json:"remaining_percent"`
	ResetAt          *time.Time `json:"reset_at"`
	PeriodHours      *float64   `json:"period_hours"`
	PlanID           string     `json:"plan_id"`
	Capacity         *float64   `json:"capacity"`
	Source           string     `json:"source"`
}
type KeyCatalogPage struct {
	Meta model.ResponseMeta  `json:"meta"`
	Keys []model.KeyIdentity `json:"keys"`
}
type KeyCatalogStore interface {
	KeyCatalog(context.Context, model.Query) (KeyCatalogPage, error)
	UpdateKeyLifecycle(context.Context, []string, []string) error
}
type RepriceOptions struct {
	Range            model.Range
	DryRun           bool
	ChunkSize        int
	Resume           bool
	ResumeCheckpoint string
}
type RepriceResult struct {
	Matched         int64
	Updated         int64
	Checkpoint      string
	Completed       bool
	EffectiveStart  time.Time
	RetainedCutoff  *time.Time
	HistoryComplete bool
}
type BackupManifest struct {
	SchemaVersion          int    `json:"schema_version"`
	IdentityEpoch          string `json:"identity_epoch"`
	DatabaseFile           string `json:"database_file"`
	DatabaseSHA256         string `json:"database_sha256"`
	IdentityKeyFile        string `json:"identity_key_file"`
	IdentityKeySHA256      string `json:"identity_key_sha256"`
	IdentityKeyFingerprint string `json:"identity_key_fingerprint"`
	CreatedAt              string `json:"created_at"`
}
type RetentionResult struct {
	Cutoff            time.Time
	HourlyCutoff      time.Time
	RolledUpRows      int64
	DeletedRows       int64
	DailyRolledUpRows int64
	DeletedHourlyRows int64
	RawCheckpoint     *time.Time
	HourlyCheckpoint  *time.Time
}
