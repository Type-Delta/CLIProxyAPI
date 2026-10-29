package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// A quota reserve keeps part of a credential's quota bars unused. A reserve
// names a bar (the same bar ID CPAMC renders, e.g. "five-hour", "weekly",
// "zai-hour-5") and the minimum remaining percent that bar may reach. When CPA
// observes a bar at or below its reserve, the credential is withheld from
// routing until that bar resets, except for client keys on its bypass list.
//
// OAuth credentials store reserves in auth-file metadata ("quota_reserve",
// "quota_reserve_bypass_keys"). Config API-key entries store them as "quota-reserve"
// and "quota-reserve-bypass-keys"; the synthesizer copies both into the attributes
// below as JSON so selection reads one representation.
const (
	// AttributeQuotaReserve carries the JSON-encoded reserve map (bar ID -> min remaining percent).
	AttributeQuotaReserve = "quota_reserve"
	// AttributeQuotaReserveBypassKeys carries the JSON-encoded list of bypass client key IDs.
	AttributeQuotaReserveBypassKeys = "quota_reserve_bypass_keys"

	quotaReserveCheckInterval = 5 * time.Minute
)

// QuotaReserveBar is one observed usage bar normalized to remaining percent.
type QuotaReserveBar struct {
	ID               string
	RemainingPercent float64
	ResetAt          time.Time
}

// QuotaReserveHit describes one bar whose reserve currently holds a credential.
type QuotaReserveHit struct {
	BarID            string    `json:"bar_id"`
	ReservePercent   float64   `json:"reserve_percent"`
	RemainingPercent float64   `json:"remaining_percent"`
	ResetAt          time.Time `json:"reset_at,omitempty"`
}

// QuotaReserveState is the in-memory held state for one credential.
type QuotaReserveState struct {
	// Until is when the earliest blocking bar resets. Zero means the reset is
	// unknown; the credential stays held until the next check clears it.
	Until     time.Time         `json:"until,omitempty"`
	Hits      []QuotaReserveHit `json:"hits"`
	CheckedAt time.Time         `json:"checked_at"`
}

// QuotaReserveProber fetches the current usage bars for a credential. Callers pass
// the shared quota cache through it, so a fresh cached read costs nothing.
type QuotaReserveProber func(ctx context.Context, auth *Auth) ([]QuotaReserveBar, error)

type quotaReserveTracker struct {
	mu     sync.Mutex
	prober QuotaReserveProber
	// observations holds the last bars seen per credential. Held state is
	// derived from them against the credential's current reserves, so editing or
	// removing a reserve takes effect immediately without another provider read.
	observations map[string]quotaReserveObservation
	lastRun      map[string]time.Time
	inFlight     map[string]struct{}
	now          func() time.Time
}

type quotaReserveObservation struct {
	bars      []QuotaReserveBar
	checkedAt time.Time
}

// stateLocked derives the held state for a credential from its last observation.
func (t *quotaReserveTracker) stateLocked(auth *Auth) (QuotaReserveState, bool) {
	observation, ok := t.observations[auth.ID]
	if !ok {
		return QuotaReserveState{}, false
	}
	now := t.currentTime()
	bars := make([]QuotaReserveBar, 0, len(observation.bars))
	for _, bar := range observation.bars {
		// A bar whose reset has passed no longer reflects current usage.
		if bar.ResetAt.IsZero() || bar.ResetAt.After(now) {
			bars = append(bars, bar)
		}
	}
	hits := EvaluateQuotaReserves(QuotaReservesForAuth(auth), bars)
	if len(hits) == 0 {
		return QuotaReserveState{}, false
	}
	return QuotaReserveState{Until: quotaReserveUntil(hits), Hits: hits, CheckedAt: observation.checkedAt}, true
}

func (t *quotaReserveTracker) currentTime() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// SetQuotaReserveProber configures the usage reader used by post-request reserve checks.
// A nil prober disables new checks; existing held states still expire.
func (m *Manager) SetQuotaReserveProber(prober QuotaReserveProber) {
	if m == nil {
		return
	}
	t := m.quotaReserves()
	t.mu.Lock()
	t.prober = prober
	t.mu.Unlock()
}

func (m *Manager) quotaReserves() *quotaReserveTracker {
	m.quotaReserveOnce.Do(func() {
		m.quotaReserveTracker = &quotaReserveTracker{
			observations: make(map[string]quotaReserveObservation),
			lastRun:      make(map[string]time.Time),
			inFlight:     make(map[string]struct{}),
		}
	})
	return m.quotaReserveTracker
}

// QuotaReservesForAuth returns the configured reserves for a credential, keyed by bar ID.
func QuotaReservesForAuth(auth *Auth) map[string]float64 {
	if auth == nil {
		return nil
	}
	raw := ""
	if auth.Attributes != nil {
		raw = auth.Attributes[AttributeQuotaReserve]
	}
	var source any
	if strings.TrimSpace(raw) != "" {
		var decoded any
		if json.Unmarshal([]byte(raw), &decoded) != nil {
			return nil
		}
		source = decoded
	} else if auth.Metadata != nil {
		source = auth.Metadata[AttributeQuotaReserve]
	}
	reserves, _ := NormalizeQuotaReserves(source)
	return reserves
}

// QuotaReserveBypassKeysForAuth returns the client key IDs that ignore this credential's reserves.
func QuotaReserveBypassKeysForAuth(auth *Auth) []string {
	if auth == nil {
		return nil
	}
	raw := ""
	if auth.Attributes != nil {
		raw = auth.Attributes[AttributeQuotaReserveBypassKeys]
	}
	var source any
	if strings.TrimSpace(raw) != "" {
		var decoded any
		if json.Unmarshal([]byte(raw), &decoded) != nil {
			return nil
		}
		source = decoded
	} else if auth.Metadata != nil {
		source = auth.Metadata[AttributeQuotaReserveBypassKeys]
	}
	keys, _ := NormalizeQuotaReserveBypassKeys(source)
	return keys
}

// NormalizeQuotaReserves validates a decoded reserve map. Values are percents in [0,100);
// a reserve of 0 is accepted but never triggers before exhaustion handling does.
func NormalizeQuotaReserves(value any) (map[string]float64, error) {
	if value == nil {
		return nil, nil
	}
	raw, ok := value.(map[string]any)
	if !ok {
		if typed, okTyped := value.(map[string]float64); okTyped {
			raw = make(map[string]any, len(typed))
			for key, item := range typed {
				raw[key] = item
			}
		} else {
			return nil, fmt.Errorf("quota reserve must be an object of bar id to percent")
		}
	}
	reserves := make(map[string]float64, len(raw))
	for key, item := range raw {
		barID := strings.TrimSpace(key)
		if barID == "" {
			return nil, fmt.Errorf("quota reserve bar id is required")
		}
		percent, okNumber := quotaReserveNumber(item)
		if !okNumber || math.IsNaN(percent) || percent < 0 || percent >= 100 {
			return nil, fmt.Errorf("quota reserve for %q must be a number from 0 to 99", barID)
		}
		reserves[barID] = percent
	}
	if len(reserves) == 0 {
		return nil, nil
	}
	return reserves, nil
}

// NormalizeQuotaReserveBypassKeys validates a decoded bypass list of client key IDs.
func NormalizeQuotaReserveBypassKeys(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	var items []any
	switch typed := value.(type) {
	case []any:
		items = typed
	case []string:
		for _, item := range typed {
			items = append(items, item)
		}
	default:
		return nil, fmt.Errorf("quota reserve bypass keys must be a list of key ids")
	}
	seen := make(map[string]struct{}, len(items))
	keys := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		keyID := strings.ToLower(strings.TrimSpace(text))
		if !ok || !isQuotaReserveKeyID(keyID) {
			return nil, fmt.Errorf("quota reserve bypass keys must be API key ids")
		}
		if _, dup := seen[keyID]; dup {
			continue
		}
		seen[keyID] = struct{}{}
		keys = append(keys, keyID)
	}
	if len(keys) == 0 {
		return nil, nil
	}
	sort.Strings(keys)
	return keys, nil
}

// isQuotaReserveKeyID accepts the 64-hex SHA-256 client key ID used by the
// management API. Raw keys never belong in a bypass list.
func isQuotaReserveKeyID(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func quotaReserveNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	}
	return 0, false
}

// EvaluateQuotaReserves returns the observed bars that reached their reserve. A bar
// blocks once its remaining percent is at or below its reserve.
func EvaluateQuotaReserves(reserves map[string]float64, bars []QuotaReserveBar) []QuotaReserveHit {
	if len(reserves) == 0 {
		return nil
	}
	hits := make([]QuotaReserveHit, 0)
	for _, bar := range bars {
		reservePercent, ok := reserves[bar.ID]
		if !ok || bar.RemainingPercent > reservePercent {
			continue
		}
		hits = append(hits, QuotaReserveHit{
			BarID:            bar.ID,
			ReservePercent:   reservePercent,
			RemainingPercent: bar.RemainingPercent,
			ResetAt:          bar.ResetAt,
		})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].BarID < hits[j].BarID })
	return hits
}

// quotaReserveUntil is the earliest instant the credential may serve again: every
// blocking bar must reset, so it is the latest known reset. An unknown reset
// keeps the credential held until a later check observes it above the reserve.
func quotaReserveUntil(hits []QuotaReserveHit) time.Time {
	until := time.Time{}
	for _, hit := range hits {
		if hit.ResetAt.IsZero() {
			return time.Time{}
		}
		if hit.ResetAt.After(until) {
			until = hit.ResetAt
		}
	}
	return until
}

// QuotaReserveState returns the active held state for a credential, if any.
func (m *Manager) QuotaReserveState(authID string) (QuotaReserveState, bool) {
	if m == nil || m.quotaReserveTracker == nil {
		return QuotaReserveState{}, false
	}
	auth, ok := m.GetByID(authID)
	if !ok {
		return QuotaReserveState{}, false
	}
	t := m.quotaReserveTracker
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stateLocked(auth)
}

// quotaReserveHolds reports whether the credential is held for a request from
// the given client key ID.
func (m *Manager) quotaReserveHolds(auth *Auth, keyID string) bool {
	_, held := m.quotaReserveHeldUntil(auth, keyID)
	return held
}

// quotaReserveHeldUntil is quotaReserveHolds plus when the hold releases
// (see quotaReserveUntil); the time is zero when a blocking reset is unknown.
func (m *Manager) quotaReserveHeldUntil(auth *Auth, keyID string) (time.Time, bool) {
	if m == nil || auth == nil || m.quotaReserveTracker == nil {
		return time.Time{}, false
	}
	t := m.quotaReserveTracker
	t.mu.Lock()
	state, held := t.stateLocked(auth)
	t.mu.Unlock()
	if !held {
		return time.Time{}, false
	}
	if keyID == "" {
		return state.Until, true
	}
	for _, bypass := range QuotaReserveBypassKeysForAuth(auth) {
		if bypass == keyID {
			return time.Time{}, false
		}
	}
	return state.Until, true
}

// hasHeldQuotaReserve reports whether any credential is currently held, so the
// scheduler fast path (which cannot apply per-request bypass) can be skipped.
func (m *Manager) hasHeldQuotaReserve() bool {
	if m == nil || m.quotaReserveTracker == nil {
		return false
	}
	t := m.quotaReserveTracker
	t.mu.Lock()
	ids := make([]string, 0, len(t.observations))
	for id := range t.observations {
		ids = append(ids, id)
	}
	t.mu.Unlock()
	for _, id := range ids {
		if _, held := m.QuotaReserveState(id); held {
			return true
		}
	}
	return false
}

// quotaReserveKeyIDFromMetadata reads the requesting client key ID from execution metadata.
func quotaReserveKeyIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	value, _ := meta[cliproxyexecutor.ClientKeyIDMetadataKey].(string)
	return strings.ToLower(strings.TrimSpace(value))
}

// scheduleQuotaReserveCheck runs one background usage check for a credential with
// reserves, at most once per quotaReserveCheckInterval. Retries and concurrent requests
// for the same credential collapse into the single in-flight check.
func (m *Manager) scheduleQuotaReserveCheck(authID string) {
	if m == nil || strings.TrimSpace(authID) == "" {
		return
	}
	auth, ok := m.GetByID(authID)
	if !ok || auth == nil || len(QuotaReservesForAuth(auth)) == 0 {
		m.clearQuotaReserveState(authID)
		return
	}
	t := m.quotaReserves()
	t.mu.Lock()
	prober := t.prober
	now := t.currentTime()
	if prober == nil {
		t.mu.Unlock()
		return
	}
	if _, running := t.inFlight[authID]; running {
		t.mu.Unlock()
		return
	}
	if last, seen := t.lastRun[authID]; seen && now.Sub(last) < quotaReserveCheckInterval {
		t.mu.Unlock()
		return
	}
	t.inFlight[authID] = struct{}{}
	t.lastRun[authID] = now
	t.mu.Unlock()

	go func() {
		defer func() {
			t.mu.Lock()
			delete(t.inFlight, authID)
			t.mu.Unlock()
		}()
		m.runQuotaReserveCheck(context.Background(), auth, prober)
	}()
}

func (m *Manager) runQuotaReserveCheck(ctx context.Context, auth *Auth, prober QuotaReserveProber) {
	bars, errProbe := prober(ctx, auth)
	if errProbe != nil {
		log.WithError(errProbe).WithField("auth_id", auth.ID).Debug("quota reserve check failed")
		return
	}
	m.ApplyQuotaReserveBars(auth, bars)
}

// ApplyQuotaReserveBars records observed bars for a credential; its held state
// follows from them. Management quota reads call it too, so a manual refresh on
// the quota page applies reserves immediately.
func (m *Manager) ApplyQuotaReserveBars(auth *Auth, bars []QuotaReserveBar) {
	if m == nil || auth == nil || len(bars) == 0 {
		return
	}
	t := m.quotaReserves()
	t.mu.Lock()
	_, wasHeld := t.stateLocked(auth)
	t.observations[auth.ID] = quotaReserveObservation{bars: append([]QuotaReserveBar(nil), bars...), checkedAt: t.currentTime()}
	state, held := t.stateLocked(auth)
	t.mu.Unlock()
	if held && !wasHeld {
		log.WithFields(log.Fields{"auth_id": auth.ID, "bars": len(state.Hits)}).Info("credential reached its quota reserve")
	} else if !held && wasHeld {
		log.WithField("auth_id", auth.ID).Info("credential is back above its quota reserve")
	}
}

// clearQuotaReserveState forgets observations for a credential without reserves.
func (m *Manager) clearQuotaReserveState(authID string) {
	if m == nil || m.quotaReserveTracker == nil {
		return
	}
	t := m.quotaReserveTracker
	t.mu.Lock()
	delete(t.observations, authID)
	t.mu.Unlock()
}

// RecheckQuotaReserve forgets the throttle for a credential so its next request
// (or an explicit call) reads usage again; used after reserves are edited.
func (m *Manager) RecheckQuotaReserve(authID string) {
	if m == nil {
		return
	}
	t := m.quotaReserves()
	t.mu.Lock()
	delete(t.lastRun, authID)
	t.mu.Unlock()
	m.scheduleQuotaReserveCheck(authID)
}

// ApplyQuotaReserveMetadata mirrors auth-file quota-reserve metadata into attributes.
// Invalid metadata is dropped from attributes so routing never half-applies it.
func ApplyQuotaReserveMetadata(auth *Auth) {
	if auth == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	delete(auth.Attributes, AttributeQuotaReserve)
	delete(auth.Attributes, AttributeQuotaReserveBypassKeys)
	if auth.Metadata == nil {
		return
	}
	if reserves, err := NormalizeQuotaReserves(auth.Metadata[AttributeQuotaReserve]); err == nil && len(reserves) > 0 {
		if raw, errMarshal := json.Marshal(reserves); errMarshal == nil {
			auth.Attributes[AttributeQuotaReserve] = string(raw)
		}
	}
	if keys, err := NormalizeQuotaReserveBypassKeys(auth.Metadata[AttributeQuotaReserveBypassKeys]); err == nil && len(keys) > 0 {
		if raw, errMarshal := json.Marshal(keys); errMarshal == nil {
			auth.Attributes[AttributeQuotaReserveBypassKeys] = string(raw)
		}
	}
}
