package cpauk

import (
	"context"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
)

type quotaHistoryBackend interface {
	RecordQuotaHistory(context.Context, []store.QuotaHistorySample, time.Time) error
	QuotaHistory(context.Context, string, time.Time, time.Time) ([]store.QuotaHistorySample, error)
}

func (s *service) RecordQuotaHistory(ctx context.Context, sample []store.QuotaHistorySample) (err error) {
	s.mu.RLock()
	enabled := s.config.Privacy.StoreCredentialID
	s.mu.RUnlock()
	if !enabled {
		return ErrUnavailable
	}
	backend, err := s.backendForRead()
	if err != nil {
		return err
	}
	history, ok := backend.(quotaHistoryBackend)
	if !ok {
		return ErrUnavailable
	}
	return recoverQuery(func() error { return history.RecordQuotaHistory(ctx, sample, time.Now().UTC()) })
}

func (s *service) QuotaHistory(ctx context.Context, provider string, start, end time.Time) (result []store.QuotaHistorySample, err error) {
	s.mu.RLock()
	enabled := s.config.Privacy.StoreCredentialID
	s.mu.RUnlock()
	if !enabled {
		return nil, ErrUnavailable
	}
	backend, err := s.backendForRead()
	if err != nil {
		return nil, err
	}
	history, ok := backend.(quotaHistoryBackend)
	if !ok {
		return nil, ErrUnavailable
	}
	err = recoverQuery(func() error { result, err = history.QuotaHistory(ctx, provider, start, end); return err })
	return
}
