package chstore

import (
	"context"
	"slices"
	"testing"
)

// TestAnalyticsTablesCoverMigrations keeps maintenance in sync with the
// migrations: every analytics_ table must be backed up, restored and rotated.
func TestAnalyticsTablesCoverMigrations(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	rows, err := s.db.Conn().Query(context.Background(),
		"SELECT name FROM system.tables WHERE database = ? AND name LIKE 'analytics\\_%' AND NOT is_temporary", s.db.Database())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var migrated []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		migrated = append(migrated, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, name := range migrated {
		if !slices.Contains(analyticsTables, name) {
			t.Errorf("migrated table %s is missing from analyticsTables", name)
		}
	}
	for _, name := range analyticsTables {
		if !slices.Contains(migrated, name) {
			t.Errorf("analyticsTables lists %s, which no migration creates", name)
		}
	}
}
