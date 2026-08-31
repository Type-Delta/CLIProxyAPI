package importer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type RowDecoder func(*sql.Rows) (any, error)

// SQLiteSource opens an upstream CPAUK database in read-only and query-only
// mode. Query must be a fixed adapter-owned SELECT, not administrator input.
type SQLiteSource struct {
	db          *sql.DB
	tx          *sql.Tx
	path        string
	sourceKind  string
	query       string
	decode      RowDecoder
	fingerprint string
}

func OpenSQLiteSource(ctx context.Context, path, sourceKind, query string, decode RowDecoder) (*SQLiteSource, error) {
	if path == "" || sourceKind == "" || query == "" || decode == nil {
		return nil, fmt.Errorf("SQLite import source configuration is incomplete")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve import source: %w", err)
	}
	fingerprint, err := sourceFileFingerprint(absolute)
	if err != nil {
		return nil, err
	}
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}).String() + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)"
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, fmt.Errorf("open CPAUK import source: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect CPAUK import source: %w", err)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("begin stable CPAUK import snapshot: %w", err)
	}
	return &SQLiteSource{db: db, tx: tx, path: absolute, sourceKind: sourceKind, query: query, decode: decode, fingerprint: fingerprint}, nil
}

func (s *SQLiteSource) Kind() string                                { return s.sourceKind }
func (s *SQLiteSource) Fingerprint(context.Context) (string, error) { return s.fingerprint, nil }

func (s *SQLiteSource) Read(ctx context.Context, offset int64, limit int) ([]SourceRow, bool, error) {
	rows, err := s.tx.QueryContext(ctx, "SELECT * FROM ("+s.query+") LIMIT ? OFFSET ?", limit+1, offset)
	if err != nil {
		return nil, false, fmt.Errorf("query CPAUK import source: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]SourceRow, 0, limit)
	for rows.Next() {
		if len(result) == limit {
			return result, false, nil
		}
		value, err := s.decode(rows)
		if err != nil {
			return nil, false, fmt.Errorf("decode CPAUK import source row: %w", err)
		}
		result = append(result, SourceRow{Offset: offset + int64(len(result)), Value: value})
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("read CPAUK import source: %w", err)
	}
	return result, true, nil
}

func (s *SQLiteSource) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	if s.tx != nil {
		_ = s.tx.Rollback()
		s.tx = nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func sourceFileFingerprint(path string) (string, error) {
	digest := sha256.New()
	for _, candidate := range []string{path, path + "-wal"} {
		file, err := os.Open(candidate)
		if err != nil {
			if candidate != path && os.IsNotExist(err) {
				continue
			}
			return "", fmt.Errorf("open CPAUK source for fingerprint: %w", err)
		}
		if _, err := digest.Write([]byte(filepath.Base(candidate) + "\x00")); err != nil {
			_ = file.Close()
			return "", err
		}
		if _, err := io.Copy(digest, file); err != nil {
			_ = file.Close()
			return "", fmt.Errorf("fingerprint CPAUK source: %w", err)
		}
		if err := file.Close(); err != nil {
			return "", fmt.Errorf("close CPAUK source fingerprint: %w", err)
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
