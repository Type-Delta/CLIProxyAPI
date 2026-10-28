package api

import (
	"context"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/ingest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/legacylog"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/outbox"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/store"
	log "github.com/sirupsen/logrus"
)

type telemetryRuntime struct {
	cancel       context.CancelFunc
	stopOnce     sync.Once
	stopIngest   chan struct{}
	ingestDone   chan struct{}
	done         chan struct{}
	mu           sync.RWMutex
	ingester     *ingest.Ingester
	startupError string
	dir          string
	shutdownCtx  context.Context
}

func (s *Server) startTelemetry(logDir string) {
	if s.clickHouse.handle == nil || !s.clickHouse.handle.Configured() {
		return
	}
	s.engine.GET("/v8/management/telemetry/health", s.managementAvailabilityMiddleware(), s.mgmt.Middleware(), s.telemetryHealth)
	ctx, cancel := context.WithCancel(context.Background())
	rt := &telemetryRuntime{cancel: cancel, done: make(chan struct{}), dir: logDir, stopIngest: make(chan struct{}), ingestDone: make(chan struct{})}
	s.telemetry = rt
	go func() {
		defer close(rt.done)
		var ing *ingest.Ingester
		appliedDays := 0
		for ctx.Err() == nil {
			db, err := s.clickHouse.handle.Get(ctx)
			s.cfgMu.RLock()
			days := s.cfg.ClickHouse.TelemetryRetentionDays
			minFree := s.cfg.Analytics.MinFreeBytes
			s.cfgMu.RUnlock()
			if err == nil {
				var st *store.Store
				st, err = store.Open(ctx, db, days)
				if err == nil {
					ing = ingest.New(st, logDir, uint64(max(minFree, 0)), days)
					appliedDays = days
					rt.mu.Lock()
					rt.ingester = ing
					rt.startupError = ""
					rt.mu.Unlock()
					break
				}
			}
			rt.mu.Lock()
			rt.startupError = err.Error()
			rt.mu.Unlock()
			log.WithError(err).Warn("telemetry startup delayed")
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
		if ing == nil {
			return
		}
		s.cfgMu.RLock()
		aq := s.cfg.ClickHouse.AgentQuery
		s.cfgMu.RUnlock()
		if aq.Enabled {
			if _, err := s.clickHouse.handle.Agent(ctx, clickhouse.AgentLimits{MaxExecutionTime: aq.MaxExecutionTime, MaxResultRows: aq.MaxResultRows, MaxResultBytes: aq.MaxResultBytes, MaxMemoryBytes: aq.MaxMemoryBytes}); err != nil {
				log.WithError(err).Warn("agent SQL provisioning failed")
			}
		}
		ingestCtx, cancelIngest := context.WithCancel(ctx)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer close(rt.ingestDone); ing.Run(ingestCtx) }()
		defer cancelIngest()
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				db := ing.Store().DB()
				err := legacylog.RunJob(ctx, db, ing, logDir)
				if err == nil || ctx.Err() != nil {
					return
				}
				log.WithError(err).Warn("legacy request log import delayed; retrying")
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}
		}()

		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		s.cfgMu.RLock()
		days := appliedDays
		s.cfgMu.RUnlock()
		for {
			select {
			case <-rt.stopIngest:
				cancel()
				cancelIngest()
				<-rt.ingestDone
				if err := ing.Drain(rt.shutdownCtx); err != nil {
					log.WithError(err).Warn("telemetry shutdown drain incomplete; outbox retained")
				}
				cancel()
				wg.Wait()
				return
			case <-ctx.Done():
				cancelIngest()
				<-rt.ingestDone
				wg.Wait()
				return
			case <-ticker.C:
				s.cfgMu.RLock()
				next := s.cfg.ClickHouse.TelemetryRetentionDays
				free := s.cfg.Analytics.MinFreeBytes
				s.cfgMu.RUnlock()
				ing.SetMinFreeBytes(uint64(max(free, 0)))
				if next != days {
					if err := ing.SetRetention(ctx, next); err != nil {
						log.WithError(err).Warn("failed to update telemetry retention")
					} else {
						days = next
					}
				}
			}
		}
	}()
}
func (s *Server) stopTelemetry(contexts ...context.Context) {
	ctx := context.Background()
	if len(contexts) > 0 && contexts[0] != nil {
		ctx = contexts[0]
	}
	if s.telemetry != nil {
		s.telemetry.stopOnce.Do(func() {
			s.telemetry.shutdownCtx = ctx
			close(s.telemetry.stopIngest)
			s.telemetry.mu.RLock()
			ready := s.telemetry.ingester != nil
			s.telemetry.mu.RUnlock()
			if !ready {
				s.telemetry.cancel()
			}
			select {
			case <-s.telemetry.done:
			case <-ctx.Done():
				s.telemetry.cancel()
				return
			}
			s.telemetry.mu.RLock()
			ready = s.telemetry.ingester != nil
			s.telemetry.mu.RUnlock()
			if files, err := outbox.List(s.telemetry.dir); !ready && err == nil && len(files) > 0 && ctx.Err() == nil {
				// Shutdown may race initial schema creation. Try a final synchronous drain;
				// on connection failure the atomic outbox files remain for the next start.
				db, err := s.clickHouse.handle.Get(ctx)
				if err == nil {
					s.cfgMu.RLock()
					days, free := s.cfg.ClickHouse.TelemetryRetentionDays, s.cfg.Analytics.MinFreeBytes
					s.cfgMu.RUnlock()
					var st *store.Store
					st, err = store.Open(ctx, db, days)
					if err == nil {
						err = ingest.New(st, s.telemetry.dir, uint64(max(free, 0)), days).Drain(ctx)
					}
				}
				if err != nil {
					log.WithError(err).Warn("telemetry shutdown drain incomplete; outbox retained")
				}
			}
		})
	}
}

// TelemetryStatus supplies ingestion backlog and errors to status consumers.
func (s *Server) TelemetryStatus() ingest.Status {
	if s.telemetry == nil {
		return ingest.Status{}
	}
	s.telemetry.mu.RLock()
	defer s.telemetry.mu.RUnlock()
	if s.telemetry.ingester == nil {
		status := ingest.Status{LastError: s.telemetry.startupError, PausedReason: s.telemetry.startupError}
		if files, err := outbox.List(s.telemetry.dir); err == nil {
			status.Backlog = len(files)
			if len(files) > 0 {
				if info, err := os.Stat(files[0]); err == nil {
					status.OldestAge = max(time.Since(info.ModTime()), 0)
				}
			}
		}
		return status
	}
	return s.telemetry.ingester.Status()
}

// requestLoggingMiddleware snapshots the logger so reloads do not change in-flight captures.
func (s *Server) requestLoggingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		s.requestLoggerMu.RLock()
		if s.requestLoggingStopped {
			s.requestLoggerMu.RUnlock()
			c.Next()
			return
		}
		logger := s.requestLogger
		s.requestLoggerWG.Add(1)
		s.requestLoggerMu.RUnlock()
		defer s.requestLoggerWG.Done()
		middleware.RequestLoggingMiddleware(logger)(c)
	}
}

// waitRequestLoggers bounds waits for hijacked HTTP handlers by the shutdown context.
func (s *Server) waitRequestLoggers(ctx context.Context) {
	done := make(chan struct{})
	go func() { s.requestLoggerWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (s *Server) telemetryHealth(c *gin.Context) {
	status := s.TelemetryStatus()
	response := gin.H{"backlog_files": status.Backlog, "oldest_age_seconds": status.OldestAge.Seconds(), "last_error": status.LastError, "paused_reason": status.PausedReason, "last_success": status.LastSuccess}
	if legacy := legacyImportJobStatus(c.Request.Context(), s.clickHouse.handle, store.LegacyJob); legacy != nil {
		response["legacy_import"] = legacy
	}
	c.JSON(http.StatusOK, response)
}
