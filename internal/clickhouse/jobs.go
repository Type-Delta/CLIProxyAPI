package clickhouse

import (
	"context"
	"fmt"
	"time"
)

type Job struct {
	Name, State, Detail string
	UpdatedAt           time.Time
}

func (db *DB) EnsureJob(ctx context.Context, name string) error {
	db.jobsMu.Lock()
	defer db.jobsMu.Unlock()
	_, ok, err := db.Job(ctx, name)
	if err != nil || ok {
		return err
	}
	return db.setJob(ctx, name, "pending", "")
}
func (db *DB) Job(ctx context.Context, name string) (Job, bool, error) {
	var j Job
	rows, err := db.conn.Query(ctx, "SELECT name, state, detail, updated_at FROM "+db.ident()+".cpa_jobs FINAL WHERE name = ?", name)
	if err != nil {
		return j, false, fmt.Errorf("clickhouse: read job: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return j, false, rows.Err()
	}
	if err = rows.Scan(&j.Name, &j.State, &j.Detail, &j.UpdatedAt); err != nil {
		return j, false, err
	}
	return j, true, rows.Err()
}
func (db *DB) SetJob(ctx context.Context, name, state, detail string) error {
	db.jobsMu.Lock()
	defer db.jobsMu.Unlock()
	return db.setJob(ctx, name, state, detail)
}
func (db *DB) setJob(ctx context.Context, name, state, detail string) error {
	switch state {
	case "pending", "running", "done", "failed", "abandoned":
	default:
		return fmt.Errorf("clickhouse: invalid job state %q", state)
	}
	var persisted uint64
	if err := db.conn.QueryRow(ctx, "SELECT max(version) FROM "+db.ident()+".cpa_jobs").Scan(&persisted); err != nil {
		return fmt.Errorf("clickhouse: seed job version: %w", err)
	}
	if persisted > db.jobVersion {
		db.jobVersion = persisted
	}
	if db.jobVersion == ^uint64(0) {
		return fmt.Errorf("clickhouse: job version exhausted")
	}
	now := time.Now().UTC()
	version := uint64(now.UnixNano())
	if version <= db.jobVersion {
		version = db.jobVersion + 1
	}
	db.jobVersion = version
	if err := db.conn.Exec(ctx, "INSERT INTO "+db.ident()+".cpa_jobs (name,state,detail,updated_at,version) VALUES (?,?,?,?,?)", name, state, detail, now, version); err != nil {
		return fmt.Errorf("clickhouse: write job: %w", err)
	}
	return nil
}
