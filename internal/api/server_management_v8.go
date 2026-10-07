package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
)

// registerManagementV8Routes defines the v8 contract independently of v0.
// Configuration paths mirror the v8 YAML tree; operational routes use its groups.
func (s *Server) registerManagementV8Routes() {
	const prefix = "/v8/management"
	s.engine.GET(prefix+"/oauth/callback", s.managementAvailabilityMiddleware(), s.mgmt.GetOAuthCallback)
	s.engine.POST(prefix+"/oauth/callback", s.managementAvailabilityMiddleware(), s.mgmt.PostOAuthCallback)

	v8 := s.engine.Group(prefix)
	v8.Use(s.managementAvailabilityMiddleware(), s.mgmt.Middleware(), func(c *gin.Context) {
		c.Set(management.ConfigV8ContextKey, true)
	})
	v8.GET("/config", s.mgmt.ConfigV8)
	v8.PUT("/config", s.mgmt.ConfigV8)
	v8.PATCH("/config", s.mgmt.ConfigV8)
	v8.GET("/config.yaml", s.mgmt.ConfigV8)
	v8.PUT("/config.yaml", s.mgmt.ConfigV8)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		v8.Handle(method, "/config/*path", s.mgmt.ConfigV8)
	}

	// These fork capabilities are part of the v8 management contract. Keep the
	// public analytics viewer on /v0/analytics/viewer; only its management APIs
	// live under this authenticated v8 group.
	v8.GET("/capabilities", s.mgmt.GetCapabilities)
	analytics := v8.Group("/analytics")
	analytics.Use(analyticsRecoveryMiddleware(), rejectAnalyticsKeyIdentityInURL(), s.mgmt.AnalyticsRateLimitMiddleware())
	analytics.GET("/health", s.mgmt.GetAnalyticsHealth)
	analytics.GET("/summary", s.mgmt.GetAnalyticsSummary)
	analytics.GET("/timeseries", s.mgmt.GetAnalyticsTimeseries)
	analytics.GET("/dimensions", s.mgmt.GetAnalyticsDimensions)
	analytics.GET("/events", s.mgmt.GetAnalyticsEvents)
	analytics.GET("/events/:attempt_id", s.mgmt.GetAnalyticsEvent)
	analytics.GET("/pricing", s.mgmt.GetAnalyticsPricing)
	analytics.GET("/pricing/catalog-providers", s.mgmt.GetAnalyticsPricingCatalogProviders)
	analytics.PUT("/pricing", s.mgmt.PutAnalyticsPricing)
	analytics.POST("/pricing/reprice", s.mgmt.PostAnalyticsReprice)
	analytics.GET("/providers", s.mgmt.GetAnalyticsProviders)
	analytics.GET("/quotas", s.mgmt.GetAnalyticsQuotas)
	analytics.GET("/keys", s.mgmt.GetAnalyticsKeys)
	analytics.GET("/leaderboard", s.mgmt.GetAnalyticsLeaderboard)
	analytics.POST("/query", s.mgmt.PostAnalyticsQuery)
	analytics.POST("/exports", s.mgmt.CreateAnalyticsExport)
	analytics.POST("/backups", s.mgmt.CreateAnalyticsBackup)
	analytics.POST("/backups/:id/restore", s.mgmt.RestoreAnalyticsBackup)
	analytics.POST("/imports/cpauk", s.mgmt.ImportCPAUKAnalytics)
	analytics.POST("/imports/:batch_id/rollback", s.mgmt.RollbackAnalyticsImport)
	analytics.POST("/purges/key", s.mgmt.PurgeAnalyticsKey)
	analytics.POST("/repairs", s.mgmt.RepairAnalytics)
	analytics.GET("/jobs/:job_id", s.mgmt.GetAnalyticsJob)
	analytics.DELETE("/jobs/:job_id", s.mgmt.CancelAnalyticsJob)
	analytics.POST("/viewers", s.mgmt.CreateAnalyticsViewer)
	analytics.GET("/viewers", s.mgmt.ListAnalyticsViewers)
	analytics.DELETE("/viewers/:id", s.mgmt.DeleteAnalyticsViewer)

	v8.GET("/api-keys", s.mgmt.GetAPIKeys)
	v8.PUT("/api-keys", s.mgmt.PutAPIKeys)
	v8.PATCH("/api-keys", s.mgmt.PatchAPIKeys)
	v8.DELETE("/api-keys", s.mgmt.DeleteAPIKeys)
	v8.GET("/api-key-limits", s.mgmt.GetAPIKeyLimits)
	v8.POST("/api-key-limits/reset", s.mgmt.ResetAPIKeyLimits)

	v8.GET("/server/latest-version", s.mgmt.GetLatestVersion)
	v8.POST("/requests/api-call", s.mgmt.APICall)
	v8.POST("/routing/cooldown/reset", s.mgmt.ResetQuota)
	v8.GET("/routing/model-definitions/:channel", s.mgmt.GetStaticModelDefinitions)

	v8.GET("/observability/logs", s.mgmt.GetLogs)
	v8.DELETE("/observability/logs", s.mgmt.DeleteLogs)
	v8.GET("/observability/logs/errors", s.mgmt.GetRequestErrorLogs)
	v8.GET("/observability/logs/errors/:name", s.mgmt.DownloadRequestErrorLog)
	v8.GET("/observability/logs/requests/:id", s.mgmt.GetRequestLogByID)
	v8.GET("/observability/usage/api-keys", s.mgmt.GetAPIKeyUsage)
	v8.GET("/observability/usage/queue", s.mgmt.GetUsageQueue)

	v8.GET("/credentials", s.mgmt.ListAuthFiles)
	v8.POST("/credentials", s.mgmt.UploadAuthFile)
	v8.DELETE("/credentials", s.mgmt.DeleteAuthFile)
	v8.GET("/credentials/models", s.mgmt.GetAuthFileModels)
	v8.GET("/credentials/download", s.mgmt.DownloadAuthFile)
	v8.PATCH("/credentials/status", s.mgmt.PatchAuthFileStatus)
	v8.PATCH("/credentials/fields", s.mgmt.PatchAuthFileFields)
	v8.POST("/credentials/refresh", s.mgmt.RefreshAuthFiles)
	v8.POST("/oauth/import", s.mgmt.ImportOAuthV8)
	v8.GET("/oauth/auth-url", s.mgmt.StartOAuthV8)
	v8.GET("/oauth/status", s.mgmt.GetAuthStatus)
	v8.DELETE("/oauth/session", s.mgmt.CancelAuthSession)

	v8.GET("/plugins", s.mgmt.ListPlugins)
	v8.DELETE("/plugins/:id", s.mgmt.DeletePlugin)
	v8.GET("/plugins/store", s.mgmt.ListPluginStore)
	v8.POST("/plugins/store/:id/install", s.mgmt.InstallPluginFromStore)
	v8.GET("/plugins/:id/quota", s.mgmt.GetPluginQuota)
	v8.POST("/plugins/:id/quota", s.mgmt.FetchPluginQuota)
	v8.DELETE("/plugins/:id/quota", s.mgmt.ResetPluginQuota)
}
