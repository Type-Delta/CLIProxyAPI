package clickhouse

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

type MigrateResult struct {
	Component              string
	FromVersion, ToVersion int
	Created                bool
}
type migration struct {
	version  int
	name     string
	data     []byte
	checksum string
}

func (db *DB) Migrate(ctx context.Context, component string, files fs.FS) (MigrateResult, error) {
	return db.migrate(ctx, component, files, "")
}

// MigrateWithJob is Migrate for components that own a one-time job. When the
// component is created from version 0, the job is recorded as pending before
// the first migration statement runs, so a crash mid-creation cannot leave a
// created component without its job, and an existing component never gets one.
func (db *DB) MigrateWithJob(ctx context.Context, component string, files fs.FS, job string) (MigrateResult, error) {
	return db.migrate(ctx, component, files, job)
}

func (db *DB) migrate(ctx context.Context, component string, files fs.FS, job string) (MigrateResult, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	result := MigrateResult{Component: component}
	var migrations []migration
	err := fs.WalkDir(files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".sql") {
			return nil
		}
		if len(name) < 10 || name[4] != '_' {
			return fmt.Errorf("invalid migration filename %q", path)
		}
		v, e := strconv.Atoi(name[:4])
		if e != nil || v <= 0 {
			return fmt.Errorf("invalid migration filename %q", path)
		}
		data, e := fs.ReadFile(files, path)
		if e != nil {
			return e
		}
		sum := sha256.Sum256(data)
		migrations = append(migrations, migration{v, name, data, fmt.Sprintf("%x", sum)})
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("clickhouse: read migrations: %w", err)
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	for i := 1; i < len(migrations); i++ {
		if migrations[i].version == migrations[i-1].version {
			return result, fmt.Errorf("clickhouse: duplicate migration version %d", migrations[i].version)
		}
	}
	rows, err := db.conn.Query(ctx, "SELECT version, name, checksum FROM "+db.ident()+".schema_migrations WHERE component = ?", component)
	if err != nil {
		return result, fmt.Errorf("clickhouse: read migration history: %w", err)
	}
	applied := map[int]string{}
	for rows.Next() {
		var version uint32
		var name, checksum string
		if err = rows.Scan(&version, &name, &checksum); err != nil {
			_ = rows.Close()
			return result, err
		}
		applied[int(version)] = checksum
		if int(version) > result.FromVersion {
			result.FromVersion = int(version)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return result, err
	}
	inventory := make(map[int]bool, len(migrations))
	for _, m := range migrations {
		inventory[m.version] = true
	}
	for version := range applied {
		if !inventory[version] {
			return result, fmt.Errorf("clickhouse: applied migration %s/%d is missing from this binary", component, version)
		}
	}
	result.ToVersion = result.FromVersion
	result.Created = result.FromVersion == 0
	for _, m := range migrations {
		if sum, ok := applied[m.version]; ok && sum != m.checksum {
			return result, fmt.Errorf("clickhouse: migration checksum mismatch for %s/%s", component, m.name)
		}
	}
	if result.Created && job != "" && len(migrations) > 0 {
		if err = db.EnsureJob(ctx, job); err != nil {
			return result, fmt.Errorf("clickhouse: record job %s for %s: %w", job, component, err)
		}
	}
	for _, m := range migrations {
		if _, ok := applied[m.version]; ok {
			continue
		}
		if m.version <= result.FromVersion {
			return result, fmt.Errorf("clickhouse: missing historical migration %s/%s", component, m.name)
		}
		for _, statement := range splitStatements(strings.ReplaceAll(string(m.data), "{database}", db.ident())) {
			if err = db.conn.Exec(ctx, statement); err != nil {
				return result, fmt.Errorf("clickhouse: apply %s/%s: %w", component, m.name, err)
			}
		}
		if err = db.conn.Exec(ctx, "INSERT INTO "+db.ident()+".schema_migrations (component,version,name,checksum,applied_at) VALUES (?,?,?,?,?)", component, uint32(m.version), m.name, m.checksum, time.Now().UTC()); err != nil {
			return result, fmt.Errorf("clickhouse: record migration %s/%s: %w", component, m.name, err)
		}
		result.ToVersion = m.version
	}
	return result, nil
}
func splitStatements(sql string) []string {
	var out []string
	var b strings.Builder
	s := bufio.NewScanner(strings.NewReader(sql))
	s.Buffer(make([]byte, 4096), 16<<20)
	for s.Scan() {
		line := s.Text()
		if strings.HasSuffix(strings.TrimSpace(line), ";") {
			b.WriteString(strings.TrimSuffix(strings.TrimSpace(line), ";"))
			if stmt := strings.TrimSpace(b.String()); stmt != "" {
				out = append(out, stmt)
			}
			b.Reset()
		} else {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	if stmt := strings.TrimSpace(b.String()); stmt != "" {
		out = append(out, stmt)
	}
	return out
}
