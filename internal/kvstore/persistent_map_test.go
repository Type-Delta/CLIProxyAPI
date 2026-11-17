package kvstore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func TestPersistentMapAtomicChangesAndSupersededOwner(t *testing.T) {
	t.Cleanup(func() {
		if errShutdown := ShutdownLocal(); errShutdown != nil {
			t.Error(errShutdown)
		}
	})
	if errConfigure := ConfigureLocal(true, filepath.Join(t.TempDir(), "kv.db")); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	first, _, errOpen := OpenPersistentMap("test:affinity:", 10, 4096)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	expiry := time.Now().Add(time.Hour)
	first.Set("old", []byte("account-b"), expiry)
	first.Flush()

	first.store.runUpdate = func(fn func(*bbolt.Tx) error) error {
		return first.store.db.Update(func(tx *bbolt.Tx) error {
			if errUpdate := fn(tx); errUpdate != nil {
				return errUpdate
			}
			return errors.New("injected transaction failure")
		})
	}
	first.Set("old", nil, time.Time{})
	first.Set("new", []byte("account-c"), expiry)
	first.Flush()
	first.store.runUpdate = nil
	second, entries, errReopen := OpenPersistentMap("test:affinity:", 10, 4096)
	if errReopen != nil || len(entries) != 1 || entries[0].Key != "old" {
		t.Fatalf("failed transaction changed durable bindings: %v, %v", entries, errReopen)
	}
	second.Set("old", []byte("account-d"), expiry)
	second.Flush()
	first.Set("old", []byte("stale-account-b"), expiry)
	first.Flush()
	value, found, errGet := second.store.KVGet(context.Background(), "test:affinity:old")
	if errGet != nil || !found || string(value) != "account-d" {
		t.Fatalf("superseded owner overwrote current binding: %q, %v, %v", value, found, errGet)
	}
}
