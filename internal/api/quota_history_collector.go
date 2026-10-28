package api

import "context"

func (s *Server) startQuotaHistoryCollector() {
	if s.mgmt == nil || s.quotaHistoryCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.quotaHistoryCancel = cancel
	s.quotaHistoryDone = make(chan struct{})
	go func() { defer close(s.quotaHistoryDone); s.mgmt.RunQuotaHistoryCollector(ctx) }()
}
func (s *Server) stopQuotaHistoryCollector(ctx context.Context) {
	if s.quotaHistoryCancel == nil {
		return
	}
	s.quotaHistoryCancel()
	select {
	case <-s.quotaHistoryDone:
	case <-ctx.Done():
	}
}
