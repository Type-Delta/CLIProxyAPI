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

// Usage caps let an operator keep a reserve on a credential's usage bars. A cap
// names a bar (the same bar ID CPAMC renders, e.g. "five-hour", "weekly",
// "zai-hour-5") and the minimum remaining percent that bar may reach. When CPA
// observes a capped bar at or below its cap, the credential is withheld from
// routing until that bar resets, except for client keys on its bypass list.
//
// OAuth credentials store caps in auth-file metadata ("usage_caps",
// "usage_cap_bypass_keys"). Config API-key entries store them as "usage-caps"
// and "usage-cap-bypass-keys"; the synthesizer copies both into the attributes
// below as JSON so selection reads one representation.
const (
	// AttributeUsageCaps carries the JSON-encoded cap map (bar ID -> min remaining percent).
	AttributeUsageCaps = "usage_caps"
	// AttributeUsageCapBypassKeys carries the JSON-encoded list of bypass client key IDs.
	AttributeUsageCapBypassKeys = "usage_cap_bypass_keys"

	usageCapCheckInterval = 5 * time.Minute
)

// UsageCapBar is one observed usage bar normalized to remaining percent.
type UsageCapBar struct {
	ID               string
	RemainingPercent float64
	ResetAt          time.Time
}

// UsageCapHit describes one capped bar that currently blocks a credential.
type UsageCapHit struct {
	BarID            string    `json:"bar_id"`
	CapPercent       float64   `json:"cap_percent"`
	RemainingPercent float64   `json:"remaining_percent"`
	ResetAt          time.Time `json:"reset_at,omitempty"`
}

// UsageCapState is the in-memory capped state for one credential.
type UsageCapState struct {
	// Until is when the earliest blocking bar resets. Zero means the reset is
	// unknown; the credential stays capped until the next check clears it.
	Until     time.Time     `json:"until,omitempty"`
	Hits      []UsageCapHit `json:"hits"`
	CheckedAt time.Time     `json:"checked_at"`
}

// UsageCapProber fetches the current usage bars for a credential. Callers pass
// the shared quota cache through it, so a fresh cached read costs nothing.
type UsageCapProber func(ctx context.Context, auth *Auth) ([]UsageCapBar, error)

type usageCapTracker struct {
	mu     sync.Mutex
	prober UsageCapProber
	// observations holds the last bars seen per credential. Capped state is
	// derived from them against the credential's current caps, so editing or
	// removing a cap takes effect immediately without another provider read.
	observations map[string]usageCapObservation
	lastRun      map[string]time.Time
	inFlight     map[string]struct{}
	now          func() time.Time
}

type usageCapObservation struct {
	bars      []UsageCapBar
	checkedAt time.Time
}

// stateLocked derives the capped state for a credential from its last observation.
func (t *usageCapTracker) stateLocked(auth *Auth) (UsageCapState, bool) {
	observation, ok := t.observations[auth.ID]
	if !ok {
		return UsageCapState{}, false
	}
	now := t.currentTime()
	bars := make([]UsageCapBar, 0, len(observation.bars))
	for _, bar := range observation.bars {
		// A bar whose reset has passed no longer reflects current usage.
		if bar.ResetAt.IsZero() || bar.ResetAt.After(now) {
			bars = append(bars, bar)
		}
	}
	hits := EvaluateUsageCaps(UsageCapsForAuth(auth), bars)
	if len(hits) == 0 {
		return UsageCapState{}, false
	}
	return UsageCapState{Until: usageCapUntil(hits), Hits: hits, CheckedAt: observation.checkedAt}, true
}

func (t *usageCapTracker) currentTime() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// SetUsageCapProber configures the usage reader used by post-request cap checks.
// A nil prober disables new checks; existing capped states still expire.
func (m *Manager) SetUsageCapProber(prober UsageCapProber) {
	if m == nil {
		return
	}
	t := m.usageCaps()
	t.mu.Lock()
	t.prober = prober
	t.mu.Unlock()
}

func (m *Manager) usageCaps() *usageCapTracker {
	m.usageCapOnce.Do(func() {
		m.usageCapTracker = &usageCapTracker{
			observations: make(map[string]usageCapObservation),
			lastRun:      make(map[string]time.Time),
			inFlight:     make(map[string]struct{}),
		}
	})
	return m.usageCapTracker
}

// UsageCapsForAuth returns the configured caps for a credential, keyed by bar ID.
func UsageCapsForAuth(auth *Auth) map[string]float64 {
	if auth == nil {
		return nil
	}
	raw := ""
	if auth.Attributes != nil {
		raw = auth.Attributes[AttributeUsageCaps]
	}
	var source any
	if strings.TrimSpace(raw) != "" {
		var decoded any
		if json.Unmarshal([]byte(raw), &decoded) != nil {
			return nil
		}
		source = decoded
	} else if auth.Metadata != nil {
		source = auth.Metadata[AttributeUsageCaps]
	}
	caps, _ := NormalizeUsageCaps(source)
	return caps
}

// UsageCapBypassKeysForAuth returns the client key IDs that ignore this credential's caps.
func UsageCapBypassKeysForAuth(auth *Auth) []string {
	if auth == nil {
		return nil
	}
	raw := ""
	if auth.Attributes != nil {
		raw = auth.Attributes[AttributeUsageCapBypassKeys]
	}
	var source any
	if strings.TrimSpace(raw) != "" {
		var decoded any
		if json.Unmarshal([]byte(raw), &decoded) != nil {
			return nil
		}
		source = decoded
	} else if auth.Metadata != nil {
		source = auth.Metadata[AttributeUsageCapBypassKeys]
	}
	keys, _ := NormalizeUsageCapBypassKeys(source)
	return keys
}

// NormalizeUsageCaps validates a decoded cap map. Values are percents in [0,100);
// a cap of 0 is accepted but never triggers before exhaustion handling does.
func NormalizeUsageCaps(value any) (map[string]float64, error) {
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
			return nil, fmt.Errorf("usage caps must be an object of bar id to percent")
		}
	}
	caps := make(map[string]float64, len(raw))
	for key, item := range raw {
		barID := strings.TrimSpace(key)
		if barID == "" {
			return nil, fmt.Errorf("usage cap bar id is required")
		}
		percent, okNumber := usageCapNumber(item)
		if !okNumber || math.IsNaN(percent) || percent < 0 || percent >= 100 {
			return nil, fmt.Errorf("usage cap for %q must be a number from 0 to 99", barID)
		}
		caps[barID] = percent
	}
	if len(caps) == 0 {
		return nil, nil
	}
	return caps, nil
}

// NormalizeUsageCapBypassKeys validates a decoded bypass list of client key IDs.
func NormalizeUsageCapBypassKeys(value any) ([]string, error) {
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
		return nil, fmt.Errorf("usage cap bypass keys must be a list of key ids")
	}
	seen := make(map[string]struct{}, len(items))
	keys := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		keyID := strings.ToLower(strings.TrimSpace(text))
		if !ok || !isUsageCapKeyID(keyID) {
			return nil, fmt.Errorf("usage cap bypass keys must be API key ids")
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

// isUsageCapKeyID accepts the 64-hex SHA-256 client key ID used by the
// management API. Raw keys never belong in a bypass list.
func isUsageCapKeyID(value string) bool {
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

func usageCapNumber(value any) (float64, bool) {
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

// EvaluateUsageCaps returns the capped bars among the observed ones. A bar
// blocks once its remaining percent is at or below its cap.
func EvaluateUsageCaps(caps map[string]float64, bars []UsageCapBar) []UsageCapHit {
	if len(caps) == 0 {
		return nil
	}
	hits := make([]UsageCapHit, 0)
	for _, bar := range bars {
		capPercent, ok := caps[bar.ID]
		if !ok || bar.RemainingPercent > capPercent {
			continue
		}
		hits = append(hits, UsageCapHit{
			BarID:            bar.ID,
			CapPercent:       capPercent,
			RemainingPercent: bar.RemainingPercent,
			ResetAt:          bar.ResetAt,
		})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].BarID < hits[j].BarID })
	return hits
}

// usageCapUntil is the earliest instant the credential may serve again: every
// blocking bar must reset, so it is the latest known reset. An unknown reset
// keeps the credential capped until a later check observes it above the cap.
func usageCapUntil(hits []UsageCapHit) time.Time {
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

// UsageCapState returns the active capped state for a credential, if any.
func (m *Manager) UsageCapState(authID string) (UsageCapState, bool) {
	if m == nil || m.usageCapTracker == nil {
		return UsageCapState{}, false
	}
	auth, ok := m.GetByID(authID)
	if !ok {
		return UsageCapState{}, false
	}
	t := m.usageCapTracker
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stateLocked(auth)
}

// usageCappedFor reports whether the credential is capped for a request from
// the given client key ID.
func (m *Manager) usageCappedFor(auth *Auth, keyID string) bool {
	if m == nil || auth == nil || m.usageCapTracker == nil {
		return false
	}
	t := m.usageCapTracker
	t.mu.Lock()
	_, capped := t.stateLocked(auth)
	t.mu.Unlock()
	if !capped {
		return false
	}
	if keyID == "" {
		return true
	}
	for _, bypass := range UsageCapBypassKeysForAuth(auth) {
		if bypass == keyID {
			return false
		}
	}
	return true
}

// hasActiveUsageCaps reports whether any credential is currently capped, so the
// scheduler fast path (which cannot apply per-request bypass) can be skipped.
func (m *Manager) hasActiveUsageCaps() bool {
	if m == nil || m.usageCapTracker == nil {
		return false
	}
	t := m.usageCapTracker
	t.mu.Lock()
	ids := make([]string, 0, len(t.observations))
	for id := range t.observations {
		ids = append(ids, id)
	}
	t.mu.Unlock()
	for _, id := range ids {
		if _, capped := m.UsageCapState(id); capped {
			return true
		}
	}
	return false
}

// usageCapKeyIDFromMetadata reads the requesting client key ID from execution metadata.
func usageCapKeyIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	value, _ := meta[cliproxyexecutor.ClientKeyIDMetadataKey].(string)
	return strings.ToLower(strings.TrimSpace(value))
}

// scheduleUsageCapCheck runs one background usage check for a credential with
// caps, at most once per usageCapCheckInterval. Retries and concurrent requests
// for the same credential collapse into the single in-flight check.
func (m *Manager) scheduleUsageCapCheck(authID string) {
	if m == nil || strings.TrimSpace(authID) == "" {
		return
	}
	auth, ok := m.GetByID(authID)
	if !ok || auth == nil || len(UsageCapsForAuth(auth)) == 0 {
		m.clearUsageCapState(authID)
		return
	}
	t := m.usageCaps()
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
	if last, seen := t.lastRun[authID]; seen && now.Sub(last) < usageCapCheckInterval {
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
		m.runUsageCapCheck(context.Background(), auth, prober)
	}()
}

func (m *Manager) runUsageCapCheck(ctx context.Context, auth *Auth, prober UsageCapProber) {
	bars, errProbe := prober(ctx, auth)
	if errProbe != nil {
		log.WithError(errProbe).WithField("auth_id", auth.ID).Debug("usage cap check failed")
		return
	}
	m.ApplyUsageCapBars(auth, bars)
}

// ApplyUsageCapBars records observed bars for a credential; its capped state
// follows from them. Management quota reads call it too, so a manual refresh on
// the quota page applies caps immediately.
func (m *Manager) ApplyUsageCapBars(auth *Auth, bars []UsageCapBar) {
	if m == nil || auth == nil || len(bars) == 0 {
		return
	}
	t := m.usageCaps()
	t.mu.Lock()
	_, wasCapped := t.stateLocked(auth)
	t.observations[auth.ID] = usageCapObservation{bars: append([]UsageCapBar(nil), bars...), checkedAt: t.currentTime()}
	state, capped := t.stateLocked(auth)
	t.mu.Unlock()
	if capped && !wasCapped {
		log.WithFields(log.Fields{"auth_id": auth.ID, "bars": len(state.Hits)}).Info("credential reached a usage cap")
	} else if !capped && wasCapped {
		log.WithField("auth_id", auth.ID).Info("credential is back above its usage caps")
	}
}

// clearUsageCapState forgets observations for a credential without caps.
func (m *Manager) clearUsageCapState(authID string) {
	if m == nil || m.usageCapTracker == nil {
		return
	}
	t := m.usageCapTracker
	t.mu.Lock()
	delete(t.observations, authID)
	t.mu.Unlock()
}

// RecheckUsageCaps forgets the throttle for a credential so its next request
// (or an explicit call) reads usage again; used after caps are edited.
func (m *Manager) RecheckUsageCaps(authID string) {
	if m == nil {
		return
	}
	t := m.usageCaps()
	t.mu.Lock()
	delete(t.lastRun, authID)
	t.mu.Unlock()
	m.scheduleUsageCapCheck(authID)
}

// ApplyUsageCapMetadata mirrors auth-file usage-cap metadata into attributes.
// Invalid metadata is dropped from attributes so routing never half-applies it.
func ApplyUsageCapMetadata(auth *Auth) {
	if auth == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	delete(auth.Attributes, AttributeUsageCaps)
	delete(auth.Attributes, AttributeUsageCapBypassKeys)
	if auth.Metadata == nil {
		return
	}
	if caps, err := NormalizeUsageCaps(auth.Metadata[AttributeUsageCaps]); err == nil && len(caps) > 0 {
		if raw, errMarshal := json.Marshal(caps); errMarshal == nil {
			auth.Attributes[AttributeUsageCaps] = string(raw)
		}
	}
	if keys, err := NormalizeUsageCapBypassKeys(auth.Metadata[AttributeUsageCapBypassKeys]); err == nil && len(keys) > 0 {
		if raw, errMarshal := json.Marshal(keys); errMarshal == nil {
			auth.Attributes[AttributeUsageCapBypassKeys] = string(raw)
		}
	}
}
