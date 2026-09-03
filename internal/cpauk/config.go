package cpauk

import (
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

const (
	DefaultQueueCapacity           = 8192
	DefaultBatchSize               = 256
	DefaultFlushInterval           = 250 * time.Millisecond
	DefaultHotRetentionDays        = 90
	DefaultCircuitFailureThreshold = 5
	DefaultMaxStorageBytes         = int64(5 * 1024 * 1024 * 1024)
	DefaultMinFreeBytes            = int64(512 * 1024 * 1024)
	DefaultStorageTimeZone         = "UTC"
	DefaultShutdownDrain           = 5 * time.Second
	MaxQueueBytes                  = int64(32 * 1024 * 1024)
)

type PrivacyConfig struct {
	StoreCredentialID bool `yaml:"store-credential-id" json:"store_credential_id"`
}

// Config is the validated analytics configuration passed by CPA. The module
// does not read or decode CPA configuration files itself.
type Config struct {
	Enabled                 bool          `yaml:"enabled" json:"enabled"`
	Path                    string        `yaml:"path" json:"path"`
	QueueCapacity           int           `yaml:"queue-capacity" json:"queue_capacity"`
	BatchSize               int           `yaml:"batch-size" json:"batch_size"`
	FlushInterval           time.Duration `yaml:"flush-interval" json:"flush_interval"`
	HotRetentionDays        int           `yaml:"hot-retention-days" json:"hot_retention_days"`
	CircuitFailureThreshold int           `yaml:"circuit-failure-threshold" json:"circuit_failure_threshold"`
	MaxStorageBytes         int64         `yaml:"max-storage-bytes" json:"max_storage_bytes"`
	MinFreeBytes            int64         `yaml:"min-free-bytes" json:"min_free_bytes"`
	StorageTimeZone         string        `yaml:"storage-time-zone" json:"storage_time_zone"`
	Privacy                 PrivacyConfig `yaml:"privacy" json:"privacy"`
	ShutdownDrain           time.Duration `yaml:"-" json:"-"`
}

func DefaultConfig() Config {
	return Config{
		QueueCapacity:           DefaultQueueCapacity,
		BatchSize:               DefaultBatchSize,
		FlushInterval:           DefaultFlushInterval,
		HotRetentionDays:        DefaultHotRetentionDays,
		CircuitFailureThreshold: DefaultCircuitFailureThreshold,
		MaxStorageBytes:         DefaultMaxStorageBytes,
		MinFreeBytes:            DefaultMinFreeBytes,
		StorageTimeZone:         DefaultStorageTimeZone,
		Privacy:                 PrivacyConfig{StoreCredentialID: true},
		ShutdownDrain:           DefaultShutdownDrain,
	}
}

// WithDefaults fills zero-valued optional settings. Enabled remains opt-in.
func (c Config) WithDefaults() Config {
	defaults := DefaultConfig()
	if c.QueueCapacity == 0 {
		c.QueueCapacity = defaults.QueueCapacity
	}
	if c.BatchSize == 0 {
		c.BatchSize = defaults.BatchSize
	}
	if c.FlushInterval == 0 {
		c.FlushInterval = defaults.FlushInterval
	}
	if c.HotRetentionDays == 0 {
		c.HotRetentionDays = defaults.HotRetentionDays
	}
	if c.CircuitFailureThreshold == 0 {
		c.CircuitFailureThreshold = defaults.CircuitFailureThreshold
	}
	if c.MaxStorageBytes == 0 {
		c.MaxStorageBytes = defaults.MaxStorageBytes
	}
	if c.MinFreeBytes == 0 {
		c.MinFreeBytes = defaults.MinFreeBytes
	}
	if c.StorageTimeZone == "" {
		c.StorageTimeZone = defaults.StorageTimeZone
	}
	if c.ShutdownDrain == 0 {
		c.ShutdownDrain = defaults.ShutdownDrain
	}
	return c
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Path) != c.Path {
		return &ConfigError{Field: "path", Reason: "must not have surrounding whitespace"}
	}
	maxQueueCapacity := int(MaxQueueBytes / int64(model.MaxEventBytes))
	if c.QueueCapacity < 1 || c.QueueCapacity > maxQueueCapacity {
		return &ConfigError{Field: "queue-capacity", Reason: fmt.Sprintf("must fit within the %d-byte queue budget", MaxQueueBytes)}
	}
	if c.BatchSize < 1 || c.BatchSize > c.QueueCapacity {
		return &ConfigError{Field: "batch-size", Reason: "must be between 1 and queue-capacity"}
	}
	if c.FlushInterval < time.Millisecond || c.FlushInterval > time.Minute {
		return &ConfigError{Field: "flush-interval", Reason: "must be between 1ms and 1m"}
	}
	if c.HotRetentionDays < 1 {
		return &ConfigError{Field: "hot-retention-days", Reason: "must be positive"}
	}
	if c.CircuitFailureThreshold < 1 {
		return &ConfigError{Field: "circuit-failure-threshold", Reason: "must be positive"}
	}
	if c.MaxStorageBytes <= 0 && c.MinFreeBytes <= 0 {
		return &ConfigError{Field: "storage-budget", Reason: "max-storage-bytes and min-free-bytes cannot both be disabled"}
	}
	if c.MaxStorageBytes < 0 || c.MinFreeBytes < 0 {
		return &ConfigError{Field: "storage-budget", Reason: "byte limits must not be negative"}
	}
	if strings.TrimSpace(c.StorageTimeZone) != c.StorageTimeZone {
		return &ConfigError{Field: "storage-time-zone", Reason: "must not have surrounding whitespace"}
	}
	if _, err := time.LoadLocation(c.StorageTimeZone); err != nil {
		return &ConfigError{Field: "storage-time-zone", Reason: "must be an IANA time zone"}
	}
	if c.ShutdownDrain <= 0 || c.ShutdownDrain > time.Minute {
		return &ConfigError{Field: "shutdown-drain", Reason: "must be between 1ns and 1m"}
	}
	return nil
}
