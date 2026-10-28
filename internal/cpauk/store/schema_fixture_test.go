package store

import "testing"

func latestSchemaVersion(t *testing.T) int {
	t.Helper()
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	return migrations[len(migrations)-1].version
}
