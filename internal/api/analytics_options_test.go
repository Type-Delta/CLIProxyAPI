package api

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestAnalyticsModuleConfigPropagatesStorageTimeZone(t *testing.T) {
	cfg := &config.Config{Analytics: config.DefaultAnalyticsConfig()}
	cfg.Analytics.StorageTimeZone = "Asia/Bangkok"
	if got := analyticsModuleConfig(cfg).StorageTimeZone; got != "Asia/Bangkok" {
		t.Fatalf("storage time zone=%q, want Asia/Bangkok", got)
	}
}
