package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

type ClickHouseConfig struct {
	DSN                    string                     `yaml:"dsn" json:"dsn"`
	Database               string                     `yaml:"database" json:"database"`
	TelemetryRetentionDays int                        `yaml:"telemetry-retention-days" json:"telemetry-retention-days"`
	AgentQuery             ClickHouseAgentQueryConfig `yaml:"agent-query" json:"agent-query"`
}
type ClickHouseAgentQueryConfig struct {
	Enabled          bool          `yaml:"enabled" json:"enabled"`
	MaxExecutionTime time.Duration `yaml:"max-execution-time" json:"max-execution-time"`
	MaxResultRows    int           `yaml:"max-result-rows" json:"max-result-rows"`
	MaxResultBytes   int64         `yaml:"max-result-bytes" json:"max-result-bytes"`
	MaxMemoryBytes   int64         `yaml:"max-memory-bytes" json:"max-memory-bytes"`
}

func DefaultClickHouseConfig() ClickHouseConfig {
	return ClickHouseConfig{Database: "cpa", TelemetryRetentionDays: 30, AgentQuery: ClickHouseAgentQueryConfig{Enabled: true, MaxExecutionTime: 30 * time.Second, MaxResultRows: 10000, MaxResultBytes: 64 << 20, MaxMemoryBytes: 2 << 30}}
}

var clickHouseDatabasePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (c ClickHouseConfig) Validate() error {
	if strings.TrimSpace(c.DSN) == "" {
		return nil
	}
	if !clickHouseDatabasePattern.MatchString(c.Database) {
		return fmt.Errorf("clickhouse.database must be a valid identifier")
	}
	if c.TelemetryRetentionDays <= 0 || c.AgentQuery.MaxExecutionTime <= 0 || c.AgentQuery.MaxResultRows <= 0 || c.AgentQuery.MaxMemoryBytes <= 0 || c.AgentQuery.MaxResultBytes < 0 {
		return fmt.Errorf("clickhouse retention and agent-query limits must be positive")
	}
	return nil
}
