package chstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	log "github.com/sirupsen/logrus"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func (s *Store) initializeIdentity(ctx context.Context) error {
	fingerprint, hasFingerprint, err := s.metadata(ctx, "identity_fingerprint")
	if err != nil {
		return err
	}
	epoch, hasEpoch, err := s.metadata(ctx, "identity_epoch")
	if err != nil {
		return err
	}
	if hasFingerprint != hasEpoch {
		return fmt.Errorf("%w: incomplete identity metadata", ErrIdentityKeyMismatch)
	}
	legacyExists := false
	if _, err := os.Stat(s.config.Path); err == nil {
		legacyExists = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect legacy analytics: %w", err)
	}
	if !hasFingerprint {
		var count uint64
		for _, name := range []string{"analytics_events", "analytics_rollups", "analytics_request_rollups", "analytics_control"} {
			if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM "+s.table(name)).Scan(&count); err != nil {
				return err
			}
			if count > 0 {
				return fmt.Errorf("%w: populated analytics without identity metadata", ErrIdentityKeyMissing)
			}
		}
	}
	for _, directory := range []string{filepath.Dir(s.config.Path), filepath.Dir(s.config.IdentityKeyPath)} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("create analytics identity directory: %w", err)
		}
	}
	if err := os.Chmod(filepath.Dir(s.config.Path), 0o700); err != nil {
		return fmt.Errorf("set analytics directory permissions: %w", err)
	}
	var legacyFingerprint, legacyEpoch string
	legacyReadable := false
	if !hasFingerprint && legacyExists {
		legacyFingerprint, legacyEpoch, err = legacyIdentity(ctx, s.config.Path)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var sqliteErr *sqlite.Error
			code := 0
			if errors.As(err, &sqliteErr) {
				code = sqliteErr.Code() & 0xff
			}
			if code != sqlite3.SQLITE_NOTADB && code != sqlite3.SQLITE_CORRUPT {
				return legacyIdentityReadError{cause: err, code: code}
			}
			// Only confirmed corruption permits a fresh identity. A lock or I/O
			// failure cannot prove that the original identity is unrecoverable.
			log.WithError(err).Warn("legacy analytics identity was not adopted; using local identity key")
		} else {
			legacyReadable = true
		}
	}
	key, err := loadIdentityKey(s.config.IdentityKeyPath, !hasFingerprint && !legacyReadable)
	if err != nil {
		return err
	}
	actual, err := model.IdentityKeyFingerprint(key)
	if err != nil {
		return err
	}
	if hasFingerprint && fingerprint != actual {
		return ErrIdentityKeyMismatch
	}
	if !hasFingerprint {
		if legacyReadable {
			if legacyFingerprint != "" && legacyFingerprint != actual {
				return ErrIdentityKeyMismatch
			}
			epoch = legacyEpoch
		}
		if epoch == "" {
			epoch, err = model.NewCorrelationID()
			if err != nil {
				return err
			}
		}
		if err := s.setMetadataBatch(ctx, map[string]string{"identity_fingerprint": actual, "identity_epoch": epoch}); err != nil {
			return fmt.Errorf("record analytics identity: %w", err)
		}
	}
	if !model.IsCorrelationID(epoch) {
		return fmt.Errorf("%w: invalid identity epoch", ErrIdentityKeyMismatch)
	}
	s.identityKey = key
	s.identityEpoch = epoch
	return nil
}

// legacyIdentityReadError exposes a retryable startup category without publishing
// filesystem paths in health. The wrapped SQLite reason remains available to logs.
type legacyIdentityReadError struct {
	cause error
	code  int
}

func (e legacyIdentityReadError) Error() string   { return e.cause.Error() }
func (e legacyIdentityReadError) Unwrap() error   { return e.cause }
func (e legacyIdentityReadError) Permanent() bool { return false }
func (e legacyIdentityReadError) Category() string {
	switch e.code {
	case sqlite3.SQLITE_BUSY:
		return "legacy_identity_busy"
	case sqlite3.SQLITE_LOCKED:
		return "legacy_identity_locked"
	case sqlite3.SQLITE_IOERR:
		return "legacy_identity_io"
	default:
		return "legacy_identity_read"
	}
}

// legacyIdentity reads existing identity metadata without migrating or writing the source.
func legacyIdentity(ctx context.Context, path string) (string, string, error) {
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String() + "?mode=ro&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return "", "", fmt.Errorf("open legacy analytics identity: %w", err)
	}
	defer func() { _ = db.Close() }()
	var fingerprint, epoch string
	if err := db.QueryRowContext(ctx, "SELECT value FROM analytics_metadata WHERE key='identity_fingerprint'").Scan(&fingerprint); err != nil {
		return "", "", fmt.Errorf("read legacy analytics fingerprint: %w", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT value FROM analytics_metadata WHERE key='identity_epoch'").Scan(&epoch); err != nil {
		return "", "", fmt.Errorf("read legacy analytics epoch: %w", err)
	}
	return fingerprint, epoch, nil
}

func loadIdentityKey(path string, create bool) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) != 32 {
			return nil, fmt.Errorf("analytics identity key has invalid length")
		}
		return data, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read analytics identity key: %w", err)
	}
	if !create {
		return nil, ErrIdentityKeyMissing
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate analytics identity key: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create analytics identity key: %w", err)
	}
	if _, err := file.Write(key); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("write analytics identity key: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("sync analytics identity key: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close analytics identity key: %w", err)
	}
	return key, nil
}
