package store

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

var (
	ErrIdentityKeyMissing    error = classifiedError{message: "analytics identity key is missing", category: "identity_key", permanent: true}
	ErrIdentityKeyMismatch   error = classifiedError{message: "analytics identity key fingerprint mismatch", category: "identity_key", permanent: true}
	ErrMigrationChecksum     error = classifiedError{message: "analytics migration checksum mismatch", category: "migration", permanent: true}
	ErrUnsupportedSchema     error = classifiedError{message: "analytics schema version is newer than this binary", category: "migration", permanent: true}
	ErrCorruptDatabase       error = classifiedError{message: "analytics database failed integrity check", category: "storage_corrupt", permanent: true}
	ErrStorageQuota          error = classifiedError{message: "analytics storage quota would be exceeded", category: "storage_quota", permanent: true}
	ErrInsufficientFreeSpace error = classifiedError{message: "analytics storage reserve would be crossed", category: "storage_quota", permanent: true}
	ErrClosed                      = errors.New("analytics store is closed")
	ErrRetainedRangePartial        = errors.New("query range cuts through a retained rollup bucket")
)

type classifiedError struct {
	message   string
	category  string
	permanent bool
}

func (e classifiedError) Error() string    { return e.message }
func (e classifiedError) Category() string { return e.category }
func (e classifiedError) Permanent() bool  { return e.permanent }

type Config struct {
	Path            string
	IdentityKeyPath string
	MaxStorageBytes int64
	MinFreeBytes    int64
	PriceBook       aggregate.PriceBook
	CursorCodec     *model.CursorCodec
}

func (c *Config) normalize() error {
	if c.Path == "" {
		return fmt.Errorf("analytics database path is required")
	}
	absolute, err := filepath.Abs(c.Path)
	if err != nil {
		return fmt.Errorf("resolve analytics database path: %w", err)
	}
	c.Path = filepath.Clean(absolute)
	if c.IdentityKeyPath == "" {
		c.IdentityKeyPath = filepath.Join(filepath.Dir(c.Path), "identity.key")
	} else {
		identityAbsolute, err := filepath.Abs(c.IdentityKeyPath)
		if err != nil {
			return fmt.Errorf("resolve analytics identity key path: %w", err)
		}
		c.IdentityKeyPath = filepath.Clean(identityAbsolute)
	}
	if c.MaxStorageBytes < 0 || c.MinFreeBytes < 0 || c.MaxStorageBytes == 0 && c.MinFreeBytes == 0 {
		return fmt.Errorf("analytics storage limits cannot both be disabled")
	}
	return nil
}
