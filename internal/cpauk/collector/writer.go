package collector

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

type Writer interface {
	WriteBatch(context.Context, []model.Event) error
}

type classifiedError interface {
	error
	Permanent() bool
	Category() string
}

func classifyWriteError(err error) (bool, string) {
	if value, ok := err.(classifiedError); ok {
		category := safeCategory(value.Category())
		return value.Permanent(), category
	}
	return false, "storage_write"
}

func safeCategory(category string) string {
	if category == "" {
		return ""
	}
	switch category {
	case "storage_write", "storage_busy", "storage_io", "storage_quota",
		"storage_corrupt", "unsupported_schema", "migration", "identity_key",
		"worker_panic", "worker_panic_loop", "worker_restart":
		return category
	}
	return "storage_write"
}
