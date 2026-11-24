package api

import (
	"context"
	"sync"
)

// startQuotaBackgroundJobs runs the hourly quota history collector and the
// armed manual-reset checker until stopQuotaBackgroundJobs cancels them.
func (s *Server) startQuotaBackgroundJobs() {
	if s.mgmt == nil || s.quotaHistoryCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.quotaHistoryCancel = cancel
	s.quotaHistoryDone = make(chan struct{})
	go func() {
		defer close(s.quotaHistoryDone)
		var jobs sync.WaitGroup
		jobs.Go(func() { s.mgmt.RunManualResetArmer(ctx) })
		s.mgmt.RunQuotaHistoryCollector(ctx)
		jobs.Wait()
	}()
}

func (s *Server) stopQuotaBackgroundJobs(ctx context.Context) {
	if s.quotaHistoryCancel == nil {
		return
	}
	s.quotaHistoryCancel()
	select {
	case <-s.quotaHistoryDone:
	case <-ctx.Done():
	}
}
