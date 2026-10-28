package store

import "github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/backend"

type RetainedTimeZoneError = backend.RetainedTimeZoneError
type RetainedRangeError = backend.RetainedRangeError
type ZoneMismatchError = backend.ZoneMismatchError
type Config = backend.Config
type CatalogBinding = backend.CatalogBinding

var (
	ErrIdentityKeyMissing      = backend.ErrIdentityKeyMissing
	ErrIdentityKeyMismatch     = backend.ErrIdentityKeyMismatch
	ErrMigrationChecksum       = backend.ErrMigrationChecksum
	ErrUnsupportedSchema       = backend.ErrUnsupportedSchema
	ErrCorruptDatabase         = backend.ErrCorruptDatabase
	ErrStorageQuota            = backend.ErrStorageQuota
	ErrInsufficientFreeSpace   = backend.ErrInsufficientFreeSpace
	ErrClosed                  = backend.ErrClosed
	ErrRetainedRangePartial    = backend.ErrRetainedRangePartial
	ErrRetainedTimeZonePartial = backend.ErrRetainedTimeZonePartial
)
