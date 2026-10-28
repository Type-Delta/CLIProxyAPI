package chstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
)

const archiveChunkBytes = 1 << 20

type archiveChunkTestHookKey struct{}

// readArchive decodes one row at a time; a single row can exceed the chunk limit.
func readArchive(ctx context.Context, path, expected string, visit func(string, string, json.RawMessage) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	hash := sha256.New()
	d := json.NewDecoder(io.TeeReader(f, hash))
	token := func(want any) error {
		got, err := d.Token()
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("invalid archive token %v (want %v)", got, want)
		}
		return nil
	}
	if err := token(json.Delim('{')); err != nil {
		return err
	}
	if err := token("format"); err != nil {
		return err
	}
	if err := token("cpa-clickhouse-analytics-v1"); err != nil {
		return err
	}
	if err := token("tables"); err != nil {
		return err
	}
	if err := token(json.Delim('{')); err != nil {
		return err
	}
	seen := map[string]bool{}
	for d.More() {
		if err := ctx.Err(); err != nil {
			return err
		}
		nameToken, err := d.Token()
		if err != nil {
			return err
		}
		name, ok := nameToken.(string)
		if !ok || !slices.Contains(analyticsTables, name) || seen[name] {
			return fmt.Errorf("invalid archive table %v", nameToken)
		}
		seen[name] = true
		if err := token(json.Delim('{')); err != nil {
			return err
		}
		if err := token("schema"); err != nil {
			return err
		}
		var schema string
		if err := d.Decode(&schema); err != nil {
			return err
		}
		if err := token("rows"); err != nil {
			return err
		}
		if err := token(json.Delim('[')); err != nil {
			return err
		}
		if visit != nil {
			if err := visit(name, schema, nil); err != nil {
				return err
			}
		}
		for d.More() {
			if err := ctx.Err(); err != nil {
				return err
			}
			var row json.RawMessage
			if err := d.Decode(&row); err != nil {
				return err
			}
			if len(row) == 0 || row[0] != '{' {
				return fmt.Errorf("invalid archive row in %s", name)
			}
			if visit != nil {
				if err := visit(name, schema, row); err != nil {
					return err
				}
			}
		}
		if err := token(json.Delim(']')); err != nil {
			return err
		}
		if err := token(json.Delim('}')); err != nil {
			return err
		}
	}
	if len(seen) != len(analyticsTables) {
		return fmt.Errorf("incomplete archive inventory")
	}
	if err := token(json.Delim('}')); err != nil {
		return err
	}
	if err := token(json.Delim('}')); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("archive contains trailing data")
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return fmt.Errorf("backup data checksum mismatch")
	}
	return nil
}
