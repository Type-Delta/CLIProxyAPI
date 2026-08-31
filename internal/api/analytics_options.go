package api

import (
	"context"
	"crypto/rand"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/store"
)

func newAnalyticsService(ctx context.Context, cfg *config.Config) cpauk.Service {
	if cfg == nil {
		return cpauk.NewDisabled()
	}
	moduleConfig := analyticsModuleConfig(cfg)
	if problem := cfg.Analytics.Problem(); problem != nil {
		return cpauk.NewInvalid(problem.Category, problem.Field, moduleConfig, openAnalyticsBackend)
	}
	return cpauk.New(ctx, moduleConfig, openAnalyticsBackend)
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
		Privacy: cpauk.PrivacyConfig{
			StoreCredentialID: value.Privacy.StoreCredentialID,
		},
	}
}

func openAnalyticsBackend(ctx context.Context, cfg cpauk.Config) (cpauk.Backend, [32]byte, error) {
	var cursorKey [32]byte
	if _, err := rand.Read(cursorKey[:]); err != nil {
		return nil, [32]byte{}, fmt.Errorf("generate analytics cursor key: %w", err)
	}
	cursorCodec, err := model.NewCursorCodec(cursorKey[:])
	if err != nil {
		return nil, [32]byte{}, fmt.Errorf("create analytics cursor codec: %w", err)
	}
	database, err := store.Open(ctx, store.Config{
		Path:            cfg.Path,
		IdentityKeyPath: filepath.Join(filepath.Dir(cfg.Path), "identity.key"),
		MaxStorageBytes: cfg.MaxStorageBytes,
		MinFreeBytes:    cfg.MinFreeBytes,
		PriceBook:       aggregate.PriceBook{},
		CursorCodec:     cursorCodec,
	})
	if err != nil {
		return nil, [32]byte{}, err
	}
	return database, database.IdentityKeyArray(), nil
}
