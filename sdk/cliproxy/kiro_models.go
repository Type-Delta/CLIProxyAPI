package cliproxy

import (
	"context"
	"strings"
	"time"
)

// refreshKiroModels reuses bounded registration workers and their publication
// fences. Discovery retries after a minute; successful catalogs last three hours.
func (s *Service) refreshKiroModels(ctx context.Context) {
	if s == nil || s.coreManager == nil || ctx.Err() != nil {
		return
	}
	cfg := s.currentConfig()
	tasks := make([]modelRegistrationTask, 0)
	for _, auth := range s.coreManager.List() {
		if auth == nil || auth.Disabled || !strings.EqualFold(auth.Provider, "kiro") || pluginHostHasAuthModelProvider(s.pluginHost, auth.Provider) || !s.kiroModelCatalog.RefreshDueForAuth(cfg, auth) {
			continue
		}
		snapshot := auth.Clone()
		tasks = append(tasks, modelRegistrationTask{
			phase: modelRegistrationPhase(snapshot), category: modelRegistrationCategory(snapshot),
			run: func(cache *openAICompatibilityRegistrationCache) {
				s.completeModelRegistrationForAuthWithCache(ctx, snapshot, cache)
			},
		})
	}
	s.runModelRegistrationTasks(ctx, tasks)
}

func (s *Service) runKiroModelRefresh(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refreshKiroModels(ctx)
		}
	}
}
