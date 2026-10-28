package config

import (
	"testing"
	"time"
)

func TestClickHouseConfig(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("config-version: 8\nclickhouse:\n  dsn: clickhouse://localhost:9000\n  agent-query:\n    enabled: false\n    max-execution-time: 5s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClickHouse.Database != "cpa" || cfg.ClickHouse.TelemetryRetentionDays != 30 || cfg.ClickHouse.AgentQuery.Enabled || cfg.ClickHouse.AgentQuery.MaxExecutionTime != 5*time.Second || cfg.ClickHouse.AgentQuery.MaxResultRows != 10000 || cfg.ClickHouse.AgentQuery.MaxResultBytes != 64<<20 || cfg.ClickHouse.AgentQuery.MaxMemoryBytes != 2<<30 {
		t.Fatalf("config: %+v", cfg.ClickHouse)
	}
	for _, s := range []string{"database: bad-name", "telemetry-retention-days: 0", "agent-query: {max-result-rows: -1}"} {
		if _, err := ParseConfigBytes([]byte("config-version: 8\nclickhouse:\n  dsn: clickhouse://localhost:9000\n  " + s + "\n")); err == nil {
			t.Errorf("accepted %s", s)
		}
	}
}
