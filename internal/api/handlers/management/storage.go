package management

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/managementasset"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/storageusage"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// GetStorageUsage reports only local CPA-managed locations. It never walks the
// host drive to identify third-party files or accepts client-supplied paths.
func (h *Handler) GetStorageUsage(c *gin.Context) {
	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	authDir, errAuth := util.ResolveAuthDir(h.cfg.AuthDir)
	pluginDir, errPlugins := config.ResolvePluginsDir(h.cfg.Plugins.Dir)
	logDir := h.logDir
	if logDir == "" {
		logDir = "logs"
		if writable := util.WritablePath(); writable != "" {
			logDir = filepath.Join(writable, "logs")
		}
	}
	analyticsPath := h.cfg.Analytics.Path
	if analyticsPath == "" {
		analyticsPath = filepath.Join(authDir, "state", "analytics", "analytics.db")
	}
	if service, ok := h.analytics.(interface{ LocalStoragePath() string }); ok {
		// A changed analytics path may require restart. Measure the active path.
		if active := service.LocalStoragePath(); active != "" {
			analyticsPath = active
		}
	}
	locations := []storageusage.Location{
		{Path: logDir, Category: "logs"},
		{Path: authDir, Category: "data"},
		{Path: analyticsPath, Category: "data"},
		{Path: filepath.Join(filepath.Dir(analyticsPath), "identity.key"), Category: "data"},
		{Path: filepath.Join(filepath.Dir(analyticsPath), "viewers.json"), Category: "data"},
		{Path: h.configFilePath, Category: "data"},
		{Path: pluginDir, Category: "data"},
		{Path: managementasset.StaticDir(h.configFilePath), Category: "data"},
	}
	// A custom database can share a directory with unrelated applications.
	// Include its sidecars and recovery files without claiming the entire parent.
	entries, errEntries := os.ReadDir(filepath.Dir(analyticsPath))
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && (strings.HasPrefix(name, filepath.Base(analyticsPath)+"-") ||
			strings.HasPrefix(name, filepath.Base(analyticsPath)+".") || strings.HasPrefix(name, "identity.key.")) {
			locations = append(locations, storageusage.Location{Path: filepath.Join(filepath.Dir(analyticsPath), name), Category: "data"})
		}
	}
	if store, ok := h.tokenStore.(interface{ WorkDir() string }); ok {
		locations = append(locations, storageusage.Location{Path: store.WorkDir(), Category: "data"})
	} else if store, ok := h.tokenStore.(interface {
		AuthDir() string
		ConfigPath() string
	}); ok {
		// Git and object stores keep their local mirrors beside auths and config.
		locations = append(locations, storageusage.Location{Path: filepath.Dir(store.AuthDir()), Category: "data"})
	}
	h.mu.Unlock()
	snapshot := h.storageUsage.Measure(c.Request.Context(), locations)
	snapshot.Partial = snapshot.Partial || errAuth != nil || errPlugins != nil || (errEntries != nil && !os.IsNotExist(errEntries))
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, snapshot)
}
