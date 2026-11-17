package auth

import (
	"sort"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	log "github.com/sirupsen/logrus"
)

type persistentSessionEntry struct {
	AuthID    string   `json:"auth_id"`
	Aliases   []string `json:"aliases"`
	SessionID string   `json:"session_id"`
	Provider  string   `json:"provider"`
	Sequence  uint64   `json:"sequence"`
	BoundAt   int64    `json:"bound_at"`
	RequestID string   `json:"request_id"`
	Recency   uint64   `json:"recency"`
}

func (c *SessionCache) preparePersistence() {
	if !c.persistent {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() { c.persistence.Flush() }()
	c.ensureInitializedLocked()
}

func (c *SessionCache) restorePersistenceLocked() {
	if !c.persistent || c.persistenceStopped {
		return
	}
	if c.persistence != nil {
		if c.persistence.Current() {
			return
		}
		c.persistence = nil
		c.entries = make(map[string]sessionEntry)
		c.groups = make(map[string]sessionEntry)
		c.sessionGroups = make(map[string]map[string]struct{})
		c.evictionOrder.Init()
		clear(c.evictionElements)
	}
	store, entries, errOpen := kvstore.OpenPersistentMap("routing:session-affinity:v1:", c.maxEntries, 64<<20)
	if errOpen != nil {
		log.WithError(errOpen).Warn("failed to restore session affinity")
		return
	}
	if store == nil {
		return
	}
	now := time.Now()
	var restored []sessionEntry
	for _, item := range entries {
		root := ojson.GetBytes(item.Value, "entry")
		entry := sessionEntry{authID: root.Get("auth_id").String(), expiresAt: item.ExpiresAt, recency: root.Get("recency").Uint()}
		for _, alias := range root.Get("aliases").Array() {
			if alias.Type == ojson.String && strings.TrimSpace(alias.String()) != "" {
				entry.aliases = append(entry.aliases, alias.String())
			}
		}
		if entry.authID == "" || len(entry.aliases) == 0 || len(entry.aliases) > maxStableSessionAliases+1 || kvstore.HashKeyPart(entry.aliases[0]) != item.Key || !now.Before(entry.expiresAt) {
			continue
		}
		if limit := now.Add(c.ttl); entry.expiresAt.After(limit) {
			entry.expiresAt = limit
		}
		entry.binding = sessionBinding{
			sessionID: root.Get("session_id").String(), provider: root.Get("provider").String(),
			sequence: root.Get("sequence").Uint(), requestID: root.Get("request_id").String(),
		}
		if boundAt := root.Get("bound_at").Int(); boundAt != 0 {
			entry.binding.boundAt = time.Unix(0, boundAt)
		}
		restored = append(restored, entry)
		if entry.recency > c.accessSequence {
			c.accessSequence = entry.recency
		}
	}
	sort.Slice(restored, func(i, j int) bool { return restored[i].recency < restored[j].recency })
	c.persistence = store
	for _, entry := range restored {
		c.replaceAliasGroupsLocked(entry.authID, entry.expiresAt, entry.aliases, entry.binding)
		if entry.binding.sequence > c.bindingSequence {
			c.bindingSequence = entry.binding.sequence
		}
	}
}

func (c *SessionCache) persistEntryLocked(entry sessionEntry) {
	if c.persistence == nil || !sameSessionEntryGroup(c.groups[entry.aliases[0]], entry) {
		return
	}
	value := persistentSessionEntry{
		AuthID: entry.authID, Aliases: entry.aliases, SessionID: entry.binding.sessionID,
		Provider: entry.binding.provider, Sequence: entry.binding.sequence, RequestID: entry.binding.requestID, Recency: entry.recency,
	}
	if !entry.binding.boundAt.IsZero() {
		value.BoundAt = entry.binding.boundAt.UnixNano()
	}
	doc := ojson.NewDocString(`{}`)
	if errSet := doc.Set("entry", value); errSet != nil {
		log.WithError(errSet).Warn("failed to encode session affinity")
		return
	}
	c.persistence.Set(kvstore.HashKeyPart(entry.aliases[0]), doc.Bytes(), entry.expiresAt)
}
