package session

import (
	"sort"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	log "github.com/sirupsen/logrus"
)

type persistentLCPGroup struct {
	Namespace         string   `json:"namespace"`
	AuthID            string   `json:"auth_id"`
	SessionID         string   `json:"session_id"`
	ParentSessionID   string   `json:"parent_session_id"`
	MinPrefixLength   int      `json:"min_prefix_length"`
	IsFork            bool     `json:"is_fork"`
	IsCompaction      bool     `json:"is_compaction"`
	NodeKind          string   `json:"node_kind"`
	EnvironmentDigest string   `json:"environment_digest"`
	Fingerprints      []string `json:"fingerprints"`
	TailFingerprints  []string `json:"tail_fingerprints"`
	AccessNumber      uint64   `json:"access_number"`
}

func lcpPersistenceKey(group *lcpGroup) string {
	return kvstore.HashKeyPart(group.namespace + "\x00" + group.key)
}

func (m *MerklePrefixMatcher) restorePersistenceLocked() {
	if !m.persistent || m.persistenceStopped {
		return
	}
	if m.persistence != nil {
		if m.persistence.Current() {
			return
		}
		m.persistence = nil
		m.resetIndexLocked()
	}
	store, entries, errOpen := kvstore.OpenPersistentMap("routing:lcp-affinity:v1:", m.maxGroups, 64<<20)
	if errOpen != nil {
		log.WithError(errOpen).Warn("failed to restore history-based session affinity")
		return
	}
	if store == nil {
		return
	}
	now := m.now()
	var restored []*lcpGroup
	for _, item := range entries {
		root := ojson.GetBytes(item.Value, "entry")
		group := &lcpGroup{
			namespace: root.Get("namespace").String(), authID: root.Get("auth_id").String(),
			sessionID: root.Get("session_id").String(), parentSessionID: root.Get("parent_session_id").String(),
			minPrefixLength: int(root.Get("min_prefix_length").Int()),
			isFork:          root.Get("is_fork").Bool(), isCompaction: root.Get("is_compaction").Bool(),
			nodeKind: root.Get("node_kind").String(), environmentDigest: root.Get("environment_digest").String(),
			expiresAt: item.ExpiresAt, lastAccessNumber: root.Get("access_number").Uint(),
		}
		for _, fp := range root.Get("fingerprints").Array() {
			group.fingerprints = append(group.fingerprints, fp.String())
		}
		for _, fp := range root.Get("tail_fingerprints").Array() {
			group.tailFingerprints = append(group.tailFingerprints, fp.String())
		}
		if group.namespace == "" || group.authID == "" || group.sessionID == "" || len(group.fingerprints) > m.maxTurns || len(group.tailFingerprints) > maxCompactionProbeWindow || !now.Before(group.expiresAt) {
			continue
		}
		if _, _, valid := m.sanitizeFingerprints(group.fingerprints, group.minPrefixLength); !valid {
			continue
		}
		group.key = sequenceKey(group.fingerprints)
		if lcpPersistenceKey(group) != item.Key {
			continue
		}
		if limit := now.Add(m.ttl); group.expiresAt.After(limit) {
			group.expiresAt = limit
		}
		if group.lastAccessNumber > m.accessCounter {
			m.accessCounter = group.lastAccessNumber
		}
		restored = append(restored, group)
	}
	sort.Slice(restored, func(i, j int) bool { return restored[i].lastAccessNumber < restored[j].lastAccessNumber })
	m.persistence = store
	for _, group := range restored {
		generation := group.lastAccessNumber
		m.addGroupLocked(group)
		group.lastAccessNumber = generation
		m.persistGroupLocked(group)
	}
}

func (m *MerklePrefixMatcher) persistGroupLocked(group *lcpGroup) {
	if m.persistence == nil || m.groups[group.namespace] == nil || m.groups[group.namespace].groups[group.key] != group {
		return
	}
	value := persistentLCPGroup{
		Namespace: group.namespace, AuthID: group.authID, SessionID: group.sessionID,
		ParentSessionID: group.parentSessionID, MinPrefixLength: group.minPrefixLength,
		IsFork: group.isFork, IsCompaction: group.isCompaction, NodeKind: group.nodeKind,
		EnvironmentDigest: group.environmentDigest, Fingerprints: group.fingerprints,
		TailFingerprints: group.tailFingerprints, AccessNumber: group.lastAccessNumber,
	}
	doc := ojson.NewDocString(`{}`)
	if errSet := doc.Set("entry", value); errSet != nil {
		log.WithError(errSet).Warn("failed to encode history-based session affinity")
		return
	}
	m.persistence.Set(lcpPersistenceKey(group), doc.Bytes(), group.expiresAt)
}
