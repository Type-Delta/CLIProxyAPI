package kvstore

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"go.etcd.io/bbolt"
)

// PersistentMapEntry is a value and its absolute expiry in a bounded local cache.
type PersistentMapEntry struct {
	Key       string
	Value     []byte
	ExpiresAt time.Time
}

// PersistentMap batches changes made while its owner's cache lock is held.
// Opening a replacement fences writes from the previous owner of the namespace.
// Home owns its routing state separately; this adapter only uses local bbolt.
type PersistentMap struct {
	store     *BoltStore
	prefix    string
	epoch     uint64
	pending   map[string]PersistentMapEntry
	lastError string
}

// Current reports whether the adapter still refers to the active local database.
// A path change must not leave a live cache writing to the closed previous store.
func (m *PersistentMap) Current() bool {
	if m == nil {
		return false
	}
	client, enabled, errClient := CurrentKVClient()
	return errClient == nil && enabled && client == m.store
}

// OpenPersistentMap reads a bounded namespace without extending entry lifetimes.
// A nil map means local persistence is disabled or Home owns the active KV store.
func OpenPersistentMap(prefix string, maxEntries, maxBytes int) (*PersistentMap, []PersistentMapEntry, error) {
	client, enabled, errClient := CurrentKVClient()
	if errClient != nil || !enabled {
		return nil, nil, errClient
	}
	store, local := client.(*BoltStore)
	if !local {
		return nil, nil, nil
	}
	if prefix == "" || maxEntries <= 0 || maxBytes <= 0 {
		return nil, nil, fmt.Errorf("invalid persistent cache bounds")
	}
	store.cacheMu.Lock()
	defer store.cacheMu.Unlock()
	var entries []PersistentMapEntry
	totalBytes := 0
	errRead := store.view(func(tx *bbolt.Tx, now time.Time) error {
		cursor := tx.Bucket(boltKVBucket).Cursor()
		for key, raw := cursor.Seek([]byte(prefix)); key != nil && bytes.HasPrefix(key, []byte(prefix)); key, raw = cursor.Next() {
			value, expiresAt, expired, errDecode := decodeEnvelope(raw, now.UnixNano())
			if errDecode != nil {
				return errDecode
			}
			if expired {
				continue
			}
			totalBytes += len(key) + len(value)
			if len(entries) >= maxEntries || totalBytes > maxBytes {
				return fmt.Errorf("persistent cache exceeds restore bounds")
			}
			entries = append(entries, PersistentMapEntry{
				Key: strings.TrimPrefix(string(key), prefix), Value: bytes.Clone(value),
				ExpiresAt: time.Unix(0, expiresAt),
			})
		}
		return nil
	})
	if errRead != nil {
		return nil, nil, errRead
	}
	if store.cacheEpochs == nil {
		store.cacheEpochs = make(map[string]uint64)
	}
	store.cacheEpochs[prefix]++
	return &PersistentMap{store: store, prefix: prefix, epoch: store.cacheEpochs[prefix]}, entries, nil
}

// Set stages an upsert, or a deletion when value is nil, until Flush.
func (m *PersistentMap) Set(key string, value []byte, expiresAt time.Time) {
	if m == nil {
		return
	}
	if m.pending == nil {
		m.pending = make(map[string]PersistentMapEntry)
	}
	m.pending[key] = PersistentMapEntry{Key: key, Value: value, ExpiresAt: expiresAt}
}

// Flush commits one cache operation atomically. Failures are logged without
// discarding the in-memory routing state or logging session/credential values.
func (m *PersistentMap) Flush() {
	if m == nil || len(m.pending) == 0 {
		return
	}
	defer func() { m.pending = nil }()
	m.store.cacheMu.Lock()
	defer m.store.cacheMu.Unlock()
	if m.store.cacheEpochs[m.prefix] != m.epoch {
		return
	}
	errWrite := m.store.update(func(tx *bbolt.Tx, now time.Time) error {
		bucket := tx.Bucket(boltKVBucket)
		for key, entry := range m.pending {
			if entry.Value == nil || !now.Before(entry.ExpiresAt) {
				if errDelete := bucket.Delete([]byte(m.prefix + key)); errDelete != nil {
					return errDelete
				}
				continue
			}
			if errPut := bucket.Put([]byte(m.prefix+key), encodeEnvelope(entry.Value, entry.ExpiresAt.UnixNano())); errPut != nil {
				return errPut
			}
		}
		return nil
	})
	if errWrite != nil {
		if message := errWrite.Error(); message != m.lastError {
			log.WithError(errWrite).WithField("namespace", m.prefix).Warn("failed to persist routing cache")
			m.lastError = message
		}
	} else if m.lastError != "" {
		log.WithField("namespace", m.prefix).Info("routing cache persistence recovered")
		m.lastError = ""
	}
}
