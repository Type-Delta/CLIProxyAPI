package cpauk

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestDefaultConfigMatchesBoundedCollectorContract(t *testing.T) {
	config := DefaultConfig()
	if config.Enabled {
		t.Fatal("analytics defaults to enabled")
	}
	if config.QueueCapacity != 8192 || config.BatchSize != 256 || config.FlushInterval != 250*time.Millisecond {
		t.Fatalf("collector defaults = %#v", config)
	}
	if int64(config.QueueCapacity)*4096 != MaxQueueBytes {
		t.Fatalf("queue byte budget = %d", int64(config.QueueCapacity)*4096)
	}
	if !config.Privacy.StoreCredentialID {
		t.Fatal("credential pseudonyms should be enabled in the default config")
	}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigValidationReturnsFieldWithoutPathValue(t *testing.T) {
	tests := []struct {
		field  string
		mutate func(*Config)
	}{
		{field: "path", mutate: func(config *Config) { config.Path = " /private/analytics.db " }},
		{field: "queue-capacity", mutate: func(config *Config) { config.QueueCapacity = 8193 }},
		{field: "queue-capacity", mutate: func(config *Config) { config.QueueCapacity = math.MaxInt }},
		{field: "batch-size", mutate: func(config *Config) { config.BatchSize = config.QueueCapacity + 1 }},
		{field: "flush-interval", mutate: func(config *Config) { config.FlushInterval = time.Hour }},
		{field: "hot-retention-days", mutate: func(config *Config) { config.HotRetentionDays = -1 }},
		{field: "circuit-failure-threshold", mutate: func(config *Config) { config.CircuitFailureThreshold = -1 }},
		{field: "storage-budget", mutate: func(config *Config) { config.MaxStorageBytes = -1 }},
		{field: "shutdown-drain", mutate: func(config *Config) { config.ShutdownDrain = time.Hour }},
	}
	for _, test := range tests {
		t.Run(test.field, func(t *testing.T) {
			config := DefaultConfig()
			test.mutate(&config)
			err := config.Validate()
			var configError *ConfigError
			if !errors.As(err, &configError) || configError.Field != test.field {
				t.Fatalf("Validate error = %#v", err)
			}
			if test.field == "path" && contains(configError.Error(), config.Path) {
				t.Fatalf("configuration error leaked path value: %v", configError)
			}
		})
	}
}

func contains(value, substring string) bool {
	for index := 0; index+len(substring) <= len(value); index++ {
		if value[index:index+len(substring)] == substring {
			return true
		}
	}
	return false
}
