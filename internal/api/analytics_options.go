package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/backend"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/chstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

func newAnalyticsService(ctx context.Context, cfg *config.Config, handle *clickhouse.Handle) cpauk.Service {
	if cfg == nil {
		return cpauk.NewDisabled()
	}
	moduleConfig := analyticsModuleConfig(cfg)
	factory := func(ctx context.Context, cfg cpauk.Config) (cpauk.Backend, [32]byte, error) {
		return openAnalyticsBackend(ctx, handle, cfg)
	}
	if problem := cfg.Analytics.Problem(); problem != nil {
		return cpauk.NewInvalid(problem.Category, problem.Field, moduleConfig, factory)
	}
	return cpauk.New(ctx, moduleConfig, factory)
}

// queueAnalyticsSync runs analytics configuration work in submission order
// without holding up the caller. The work can wait on ClickHouse mutations, so
// config reloads and the auth/model registration behind them must not block on it.
// The returned channel closes when the job has finished.
func (s *Server) queueAnalyticsSync(job func()) <-chan struct{} {
	done := make(chan struct{})
	s.analyticsSyncMu.Lock()
	previous := s.analyticsSyncTail
	s.analyticsSyncTail = done
	s.analyticsSyncMu.Unlock()
	go func() {
		defer close(done)
		if previous != nil {
			<-previous
		}
		job()
	}()
	return done
}

type analyticsKeyLifecycle interface {
	UpdateKeyLifecycle(context.Context, []string, []string) error
}

func syncAnalyticsKeyLifecycle(ctx context.Context, service cpauk.Service, previous, current []config.APIKeyEntry) error {
	lifecycle, ok := service.(analyticsKeyLifecycle)
	if !ok {
		return nil
	}
	configured := analyticsKeyIDs(current)
	rotated := make([]string, 0)
	if len(previous) == len(current) && len(previous) != 0 {
		currentSet := make(map[string]struct{}, len(configured))
		for _, keyID := range configured {
			currentSet[keyID] = struct{}{}
		}
		for _, keyID := range analyticsKeyIDs(previous) {
			if _, remainsConfigured := currentSet[keyID]; !remainsConfigured {
				rotated = append(rotated, keyID)
			}
		}
		slices.Sort(rotated)
	}
	return lifecycle.UpdateKeyLifecycle(ctx, configured, rotated)
}

func analyticsKeyIDs(entries []config.APIKeyEntry) []string {
	seen := make(map[string]struct{}, len(entries))
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		keyID := config.APIKeyID(entry.Key)
		if _, duplicate := seen[keyID]; duplicate {
			continue
		}
		seen[keyID] = struct{}{}
		ids = append(ids, keyID)
	}
	slices.Sort(ids)
	return ids
}

func analyticsModuleConfig(cfg *config.Config) cpauk.Config {
	value := cfg.Analytics
	path := value.Path
	if path == "" {
		path = filepath.Join(cfg.AuthDir, "state", "analytics", "analytics.db")
	}
	bindingCapacity := len(cfg.GeminiKey) +
		len(cfg.InteractionsKey) +
		len(cfg.ClaudeKey) +
		len(cfg.CodexKey) +
		len(cfg.XAIKey) +
		len(cfg.VertexCompatAPIKey) +
		len(cfg.OpenAICompatibility)
	bindings := make([]cpauk.CatalogBinding, 0, bindingCapacity)
	seenBindings := make(map[string]struct{}, bindingCapacity)
	addBinding := func(provider, catalog string) {
		provider = strings.ToLower(strings.TrimSpace(provider))
		catalog = strings.ToLower(strings.TrimSpace(catalog))
		if provider == "" || catalog == "" {
			return
		}
		if _, exists := seenBindings[provider]; exists {
			return
		}
		seenBindings[provider] = struct{}{}
		bindings = append(bindings, cpauk.CatalogBinding{Provider: provider, Catalog: catalog})
	}
	for _, entry := range cfg.GeminiKey {
		if strings.TrimSpace(entry.APIKey) == "" && strings.TrimSpace(entry.BaseURL) == "" {
			continue
		}
		addBinding("gemini", entry.PricingCatalog)
	}
	for _, entry := range cfg.InteractionsKey {
		if strings.TrimSpace(entry.APIKey) == "" && strings.TrimSpace(entry.BaseURL) == "" {
			continue
		}
		addBinding("gemini-interactions", entry.PricingCatalog)
	}
	for _, entry := range cfg.ClaudeKey {
		if strings.TrimSpace(entry.APIKey) == "" && strings.TrimSpace(entry.BaseURL) == "" {
			continue
		}
		addBinding("claude", entry.PricingCatalog)
	}
	for _, entry := range cfg.CodexKey {
		if strings.TrimSpace(entry.APIKey) == "" && strings.TrimSpace(entry.BaseURL) == "" {
			continue
		}
		addBinding("codex", entry.PricingCatalog)
	}
	for _, entry := range cfg.XAIKey {
		if strings.TrimSpace(entry.APIKey) == "" && strings.TrimSpace(entry.BaseURL) == "" {
			continue
		}
		addBinding("xai", entry.PricingCatalog)
	}
	for _, entry := range cfg.VertexCompatAPIKey {
		if strings.TrimSpace(entry.APIKey) == "" && strings.TrimSpace(entry.BaseURL) == "" {
			continue
		}
		addBinding("vertex", entry.PricingCatalog)
	}
	for _, entry := range cfg.OpenAICompatibility {
		name := strings.ToLower(strings.TrimSpace(entry.Name))
		if name == "" || entry.Disabled {
			continue
		}
		provider := util.OpenAICompatibleProviderKey(name)
		if _, exists := seenBindings[provider]; exists {
			continue
		}
		seenBindings[provider] = struct{}{}
		bindings = append(bindings, cpauk.CatalogBinding{Provider: provider, Catalog: strings.ToLower(strings.TrimSpace(entry.PricingCatalog))})
	}
	slices.SortFunc(bindings, func(left, right cpauk.CatalogBinding) int { return strings.Compare(left.Provider, right.Provider) })
	return cpauk.Config{
		Enabled:                 value.Enabled,
		Path:                    path,
		QueueCapacity:           value.QueueCapacity,
		BatchSize:               value.BatchSize,
		FlushInterval:           value.FlushInterval,
		HotRetentionDays:        value.HotRetentionDays,
		CircuitFailureThreshold: value.CircuitFailureThreshold,
		MaxStorageBytes:         value.MaxStorageBytes,
		MinFreeBytes:            value.MinFreeBytes,
		StorageTimeZone:         value.StorageTimeZone,
		Privacy: cpauk.PrivacyConfig{
			StoreCredentialID: value.Privacy.StoreCredentialID,
		},
		CatalogBindings: bindings,
	}
}

func openAnalyticsBackend(ctx context.Context, handle *clickhouse.Handle, cfg cpauk.Config) (cpauk.Backend, [32]byte, error) {
	if handle == nil || !handle.Configured() {
		return nil, [32]byte{}, backend.ErrClickHouseNotConfigured
	}
	var cursorKey [32]byte
	if _, err := rand.Read(cursorKey[:]); err != nil {
		return nil, [32]byte{}, fmt.Errorf("generate analytics cursor key: %w", err)
	}
	cursorCodec, err := model.NewCursorCodec(cursorKey[:])
	if err != nil {
		return nil, [32]byte{}, fmt.Errorf("create analytics cursor codec: %w", err)
	}

	database, err := chstore.Open(ctx, handle, backend.Config{
		Path:              cfg.Path,
		IdentityKeyPath:   filepath.Join(filepath.Dir(cfg.Path), "identity.key"),
		MaxStorageBytes:   cfg.MaxStorageBytes,
		MinFreeBytes:      cfg.MinFreeBytes,
		RetentionTimeZone: cfg.StorageTimeZone,
		PriceBook:         aggregate.PriceBook{},
		CursorCodec:       cursorCodec,
		CatalogBindings:   cfg.CatalogBindings,
	})
	if err != nil {
		return nil, [32]byte{}, err
	}
	identityKey := database.IdentityKeyArray()
	jobCtx, cancel := context.WithCancel(context.Background())
	wrapped := &analyticsBackend{Store: database, handle: handle, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(wrapped.done)
		if err := chstore.RunLegacyImport(jobCtx, database, cfg.Path); err != nil {
			wrapped.importErr = err
			log.WithError(err).Warn("legacy analytics import failed")
		}
		// Import restores historical lifecycle state; current configuration wins.
		wrapped.lifecycleMu.Lock()
		defer wrapped.lifecycleMu.Unlock()
		if wrapped.lifecycleSet && jobCtx.Err() == nil {
			if err := database.UpdateKeyLifecycle(jobCtx, wrapped.configuredKeys, wrapped.rotatedKeys); err != nil {
				log.WithError(err).Warn("failed to synchronize keys after legacy analytics import")
			}
		}
	}()
	return wrapped, identityKey, nil
}

// analyticsBackend owns its background import, while borrowing the server pool.
type analyticsBackend struct {
	*chstore.Store
	handle                      *clickhouse.Handle
	cancel                      context.CancelFunc
	done                        chan struct{}
	importErr                   error
	closeOnce                   sync.Once
	lifecycleMu                 sync.Mutex
	lifecycleSet                bool
	configuredKeys, rotatedKeys []string
}

func (b *analyticsBackend) Close(ctx context.Context) error {
	b.StopBackground()
	// The server closes the shared pool next, so the worker must have exited.
	select {
	case <-b.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return b.Store.Close(ctx)
}
func (b *analyticsBackend) LegacyImportStatus() *model.LegacyImportStatus {
	return legacyImportJobStatus(context.Background(), b.handle, "legacy-import:analytics")
}

// legacyImportJobStatus projects the shared journal into the management health shape.
func legacyImportJobStatus(ctx context.Context, handle *clickhouse.Handle, name string) *model.LegacyImportStatus {
	if handle == nil || !handle.Configured() {
		return nil
	}
	db, err := handle.Get(ctx)
	if err != nil {
		return &model.LegacyImportStatus{State: "failed", Reason: err.Error()}
	}
	job, found, err := db.Job(ctx, name)
	if err != nil {
		return &model.LegacyImportStatus{State: "failed", Reason: err.Error()}
	}
	if !found {
		return nil
	}
	result := &model.LegacyImportStatus{State: job.State}
	var detail map[string]json.RawMessage
	if json.Unmarshal([]byte(job.Detail), &detail) == nil {
		for key, raw := range detail {
			var count int64
			if json.Unmarshal(raw, &count) == nil && string(raw) != "null" {
				if result.Counts == nil {
					result.Counts = map[string]int64{}
				}
				result.Counts[key] = count
			}
		}
		if job.State == "failed" {
			_ = json.Unmarshal(detail["error"], &result.Reason)
		}
		if job.State == "abandoned" {
			_ = json.Unmarshal(detail["reason"], &result.Reason)
		}
	}
	if job.State == "failed" && result.Reason == "" {
		result.Reason = job.Detail
	}
	return result
}

func (b *analyticsBackend) WaitLegacyImport(ctx context.Context) error {
	select {
	case <-b.done:
		if b.importErr != nil && b.handle != nil {
			db, err := b.handle.Get(ctx)
			if err != nil {
				return err
			}
			job, found, err := db.Job(ctx, "legacy-import:analytics")
			if err != nil {
				return err
			}
			if found && job.State == "abandoned" {
				return nil
			}
		}
		return b.importErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *analyticsBackend) UpdateKeyLifecycle(ctx context.Context, configured, rotated []string) error {
	b.lifecycleMu.Lock()
	defer b.lifecycleMu.Unlock()
	b.lifecycleSet = true
	b.configuredKeys = append([]string(nil), configured...)
	b.rotatedKeys = append([]string(nil), rotated...)
	return b.Store.UpdateKeyLifecycle(ctx, configured, rotated)
}

func (b *analyticsBackend) Restore(ctx context.Context, path, manifest string) error {
	err := b.Store.Restore(ctx, path, manifest)
	if err != nil {
		log.WithError(err).Warn("analytics restore failed")
	}
	return err
}

func (b *analyticsBackend) StopBackground() { b.closeOnce.Do(b.cancel) }

// CheckReady uses cached shared-pool health without taking the analytics lock.
func (b *analyticsBackend) CheckReady(ctx context.Context) error { return b.handle.CheckHealth(ctx) }

func (b *analyticsBackend) AbandonLegacyImport(ctx context.Context, reason string) error {
	db, err := b.handle.Get(ctx)
	if err != nil {
		return err
	}
	job, _, err := db.Job(ctx, "legacy-import:analytics")
	if err != nil {
		return err
	}
	if job.State == "running" {
		return fmt.Errorf("cannot abandon a running legacy analytics import")
	}
	select {
	case <-b.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	abandoner, ok := any(b.Store).(interface {
		AbandonLegacyImport(context.Context, string) error
	})
	if !ok {
		return fmt.Errorf("legacy import abandonment is unsupported by this backend")
	}
	return abandoner.AbandonLegacyImport(ctx, reason)
}
