package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/regexp"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
	log "github.com/sirupsen/logrus"
)

var quotaCooldownDisabled atomic.Bool

var transientErrorCooldownSeconds atomic.Int64

// quotaResetState tracks recovered pools independently from usage-read epochs.
// Only resets advance execution watermarks; all healthy observations fence disk reads.
type quotaResetState struct {
	sharedSeq             uint64
	allModelsSeq          uint64
	modelSeq              map[string]uint64
	quotaObservationSeq   uint64
	publications          int
	published             chan struct{}
	recoveryVersion       uint64
	sharedRecoveryVersion uint64
	allRecoveryVersion    uint64
	clearRecoveryVersion  uint64
	removedVersion        uint64
	modelRecoveryVersions map[string]uint64
}

func (m *Manager) beginQuotaRecoveryPublicationLocked(authID string, modelIDs []string, recoverCredential bool) *quotaResetState {
	if m.quotaResetStates == nil {
		m.quotaResetStates = make(map[string]*quotaResetState)
	}
	state := m.quotaResetStates[authID]
	if state == nil {
		state = &quotaResetState{modelSeq: make(map[string]uint64), modelRecoveryVersions: make(map[string]uint64)}
		m.quotaResetStates[authID] = state
	}
	state.recoveryVersion++
	if recoverCredential || modelIDs == nil {
		state.sharedRecoveryVersion = state.recoveryVersion
	}
	if modelIDs == nil {
		state.allRecoveryVersion = state.recoveryVersion
	} else {
		for _, modelID := range modelIDs {
			state.modelRecoveryVersions[canonicalModelKey(modelID)] = state.recoveryVersion
		}
	}
	if state.publications == 0 {
		state.published = make(chan struct{})
	}
	state.publications++
	return state
}

func (m *Manager) beginQuotaResetPublicationLocked(authID string, modelIDs []string, recoverCredential bool) {
	state := m.beginQuotaRecoveryPublicationLocked(authID, modelIDs, recoverCredential)
	seq := m.currentExecutionSeq()
	if recoverCredential || modelIDs == nil {
		state.sharedSeq = seq
	}
	if modelIDs == nil {
		state.allModelsSeq = seq
	} else {
		for _, modelID := range modelIDs {
			state.modelSeq[canonicalModelLookupKey(modelID)] = seq
		}
	}
}

func (m *Manager) finishQuotaResetPublication(authID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state := m.quotaResetStates[authID]; state != nil {
		state.publications--
		if state.publications == 0 {
			close(state.published)
		}
	}
}

func (m *Manager) isPreResetQuotaFailureLocked(result Result, modelKey string) bool {
	if result.ExecutionSeq == 0 || result.Success || statusCodeFromResult(result.Error) != http.StatusTooManyRequests ||
		isForceCooldownError(result.Error) || isTransientRateLimitResultError(result.Error) || isCloudflareChallengeResultError(result.Error) {
		return false
	}
	state := m.quotaResetStates[result.AuthID]
	if state == nil {
		return false
	}
	watermark := state.sharedSeq
	if !result.CredentialScope && modelKey != "" {
		watermark = state.allModelsSeq
		if selected := state.modelSeq[canonicalModelLookupKey(modelKey)]; selected > watermark {
			watermark = selected
		}
	}
	return result.ExecutionSeq <= watermark
}

// SetQuotaCooldownDisabled toggles auth/model cooldown scheduling globally.
func SetQuotaCooldownDisabled(disable bool) {
	quotaCooldownDisabled.Store(disable)
}

// SetTransientErrorCooldownSeconds configures cooldowns for 408/500/502/503/504.
// 0 keeps the legacy default; negative values disable transient error cooldowns.
func SetTransientErrorCooldownSeconds(seconds int) {
	transientErrorCooldownSeconds.Store(int64(seconds))
}

// QuotaCooldownDisabledForAuth returns whether cooling is disabled for the auth under global settings.
func QuotaCooldownDisabledForAuth(auth *Auth) bool {
	return quotaCooldownDisabledForAuth(auth)
}

// QuotaCooldownDisabledForAuthWithConfig returns whether cooling is disabled for the auth with the given config.
func QuotaCooldownDisabledForAuthWithConfig(auth *Auth, cfg *internalconfig.Config) bool {
	return quotaCooldownDisabledForAuthWithConfig(auth, cfg)
}

func quotaCooldownDisabledForAuth(auth *Auth) bool {
	return quotaCooldownDisabledForAuthWithConfig(auth, nil)
}

func quotaCooldownDisabledForAuthWithConfig(auth *Auth, cfg *internalconfig.Config) bool {
	// Home owns cooldown state, so downstream instances must not schedule local cooldowns.
	if cfg != nil && cfg.Home.Enabled {
		return true
	}
	if auth != nil {
		if override, ok := auth.DisableCoolingOverride(); ok {
			return override
		}
		if override, ok := providerCoolingOverrideForAuth(auth, cfg); ok {
			return override
		}
	}
	if cfg != nil && cfg.DisableCooling {
		return true
	}
	return quotaCooldownDisabled.Load()
}

func providerCoolingOverrideForAuth(auth *Auth, cfg *internalconfig.Config) (bool, bool) {
	if auth == nil || cfg == nil {
		return false, false
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider == "" {
		return false, false
	}
	providerKey := ""
	compatName := ""
	if auth.Attributes != nil {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	if providerKey == "" && compatName == "" && provider != "openai-compatibility" {
		return false, false
	}
	if providerKey == "" {
		providerKey = provider
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, provider)
	if entry == nil || entry.DisableCooling == nil {
		return false, false
	}
	return *entry.DisableCooling, true
}

func nextTransientErrorRetryAfter(now time.Time) time.Time {
	return recoverableFailureRetryAfterWithHint(now, nil, false)
}

func recoverableFailureRetryAfter(now time.Time, disableCooling bool) time.Time {
	return recoverableFailureRetryAfterWithResult(now, nil, disableCooling, nil)
}

// transientRateLimitRetryAfter resolves the model-scoped cooldown for a
// transient capacity 429. An explicit zero retry hint means the upstream
// accepts an immediate retry and suppresses the default transient cooldown;
// only a missing hint falls back to that default.
func transientRateLimitRetryAfter(now time.Time, retryAfter *time.Duration, disableCooling bool) time.Time {
	if retryAfter != nil && *retryAfter == 0 {
		return time.Time{}
	}
	return recoverableFailureRetryAfterWithHint(now, retryAfter, disableCooling)
}

func recoverableFailureRetryAfterWithHint(now time.Time, retryAfter *time.Duration, disableCooling bool) time.Time {
	return recoverableFailureRetryAfterWithResult(now, retryAfter, disableCooling, nil)
}

func recoverableFailureRetryAfterWithResult(now time.Time, retryAfter *time.Duration, disableCooling bool, resultErr *Error) time.Time {
	if disableCooling {
		return time.Time{}
	}
	seconds := transientErrorCooldownSeconds.Load()
	if seconds < 0 {
		return time.Time{}
	}
	if retryAfter != nil && *retryAfter > 0 {
		return now.Add(*retryAfter)
	}
	if seconds == 0 {
		if isServerOverloadedResultError(resultErr) {
			return now.Add(codexOverloadCooldown)
		}
		return now.Add(transientErrorCooldown)
	}
	return now.Add(time.Duration(seconds) * time.Second)
}

func isServerOverloadedResultError(resultErr *Error) bool {
	if resultErr == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(resultErr.Code), "server_is_overloaded") {
		return true
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(resultErr.Message), &payload) != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(payload.Error.Code), "server_is_overloaded")
}

// SetConfig updates the runtime config snapshot used by request-time helpers.
// Callers should provide the latest config on reload so per-credential alias mapping stays in sync.
func (m *Manager) SetConfig(cfg *internalconfig.Config) {
	if m == nil {
		return
	}
	m.configCooldownMu.Lock()
	defer m.configCooldownMu.Unlock()
	if m.setConfigSnapshotLocked(cfg) {
		m.persistCooldownStatesLocked(context.Background())
	}
}

// SetConfigSnapshot updates only in-memory configuration state. It reports whether
// a caller must persist cleared cooldown state after its commit critical section.
func (m *Manager) SetConfigSnapshot(cfg *internalconfig.Config) bool {
	if m == nil {
		return false
	}
	m.configCooldownMu.Lock()
	defer m.configCooldownMu.Unlock()
	return m.setConfigSnapshotLocked(cfg)
}

func (m *Manager) setConfigSnapshotLocked(cfg *internalconfig.Config) bool {
	if cfg == nil {
		cfg = &internalconfig.Config{}
	} else {
		cfg = cfg.CloneForRuntime()
	}
	rules, warnings, errCompile := routing.Compile(cfg)
	if errCompile != nil {
		log.WithError(errCompile).Warn("routing rules could not be compiled; retaining previous config")
		return false
	}
	m.mu.RLock()
	oldCooldownStore := m.cooldownStore
	m.mu.RUnlock()
	previousCfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if homeSessionAliasTTL(previousCfg) != homeSessionAliasTTL(cfg) {
		m.homeSessionAliases.clear()
	}
	m.routingRules.Store(&routingSnapshot{rules: rules, needsClient: routingRulesNeedClient(cfg.Routing.Rules)})
	for _, warning := range warnings {
		log.WithField("warning", warning).Warn("routing rule warning")
	}
	if cfg.Home.Enabled && rules.Len() > 0 {
		log.Warn("routing rules are ignored while Home mode is enabled")
	}
	m.runtimeConfig.Store(cfg)
	clearedCooldowns := m.clearDisabledCooldownStates(cfg)
	if clearedCooldowns && oldCooldownStore != nil {
		m.mu.Lock()
		if m.cooldownStore == oldCooldownStore {
			m.pendingCooldownStateStore = oldCooldownStore
		}
		m.mu.Unlock()
	}
	if !cfg.Home.Enabled {
		m.clearHomeRuntimeAuths()
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	return clearedCooldowns
}

// ApplyConfigWithCooldownStateStore serializes a config update with its cooldown
// store transition. It persists the resulting state to the captured old store before
// exposing the resolved replacement store.
func (m *Manager) ApplyConfigWithCooldownStateStore(ctx context.Context, cfg *internalconfig.Config, store CooldownStateStore) bool {
	if m == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errContext := ctx.Err(); errContext != nil {
		return false
	}

	m.configCooldownMu.Lock()
	defer m.configCooldownMu.Unlock()
	if _, _, errCompile := routing.Compile(cfg); errCompile != nil {
		log.WithError(errCompile).Warn("routing rules could not be compiled; retaining previous config")
		return false
	}
	m.mu.RLock()
	oldStore := m.cooldownStore
	m.mu.RUnlock()
	m.setConfigSnapshotLocked(cfg)
	if oldStore != nil && !m.persistCooldownStatesToLocked(ctx, oldStore) {
		return false
	}
	if errContext := ctx.Err(); errContext != nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cooldownStore != oldStore {
		return false
	}
	if m.pendingCooldownStateStore == oldStore {
		m.pendingCooldownStateStore = nil
	}
	m.cooldownStore = store
	return true
}

// PersistCooldownStates writes the current cooldown snapshot using ctx.
func (m *Manager) PersistCooldownStates(ctx context.Context) {
	m.persistCooldownStates(ctx)
}

// SwapCooldownStateStore persists cleared state to the old store before replacing it.
// Persistence is deliberately performed without holding the manager lock.
func (m *Manager) SwapCooldownStateStore(ctx context.Context, store CooldownStateStore, persistOld bool) bool {
	if m == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errContext := ctx.Err(); errContext != nil {
		return false
	}
	m.configCooldownMu.Lock()
	defer m.configCooldownMu.Unlock()
	m.mu.RLock()
	oldStore := m.cooldownStore
	pendingStore := m.pendingCooldownStateStore
	m.mu.RUnlock()
	storeToPersist := pendingStore
	if storeToPersist == nil && persistOld {
		storeToPersist = oldStore
	}
	if storeToPersist != nil && !m.persistCooldownStatesToLocked(ctx, storeToPersist) {
		return false
	}
	if errContext := ctx.Err(); errContext != nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cooldownStore != oldStore {
		return false
	}
	if m.pendingCooldownStateStore == storeToPersist {
		m.pendingCooldownStateStore = nil
	}
	m.cooldownStore = store
	return true
}

func (m *Manager) cooldownDisabledForAuth(auth *Auth) bool {
	if m == nil {
		return quotaCooldownDisabledForAuth(auth)
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return quotaCooldownDisabledForAuthWithConfig(auth, cfg)
}

func (m *Manager) clearDisabledCooldownStates(cfg *internalconfig.Config) bool {
	if m == nil {
		return false
	}
	now := time.Now()
	snapshots := make([]*Auth, 0)
	m.mu.Lock()
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		isDisabled := auth.Disabled || auth.Status == StatusDisabled
		if !isDisabled && !quotaCooldownDisabledForAuthWithConfig(auth, cfg) {
			continue
		}
		var cleared bool
		if isDisabled {
			cleared = clearCooldownStateForAuth(auth, now)
		} else {
			cleared = clearDisabledCooldownStateForAuth(auth, now)
		}
		if cleared {
			snapshots = append(snapshots, auth.Clone())
		}
	}
	m.mu.Unlock()

	if m.scheduler != nil {
		for _, snapshot := range snapshots {
			m.scheduler.upsertAuth(snapshot)
		}
	}
	return len(snapshots) > 0
}

// RestoreCooldownStates restores unexpired persisted cooldown records into registered auths.
func (m *Manager) RestoreCooldownStates(ctx context.Context) error {
	return m.restoreCooldownStates(ctx, true)
}

// prepareQuotaRecovery loads an unseen store before a recovery can write a full
// snapshot, retaining independent pools and other credentials already on disk.
func (m *Manager) prepareQuotaRecovery(ctx context.Context, authID string, reset bool) error {
	m.mu.RLock()
	needsRead := m.auths[authID] != nil && m.cooldownStore != nil && m.cooldownReadStore != m.cooldownStore &&
		(reset || m.quotaObservationCurrentLocked(ctx, authID))
	if !needsRead && m.auths[authID] != nil && m.cooldownReadStore == m.cooldownStore {
		for _, record := range m.cooldownUnregistered {
			if strings.TrimSpace(record.AuthID) == authID {
				needsRead = reset || m.quotaObservationCurrentLocked(ctx, authID)
				break
			}
		}
	}
	m.mu.RUnlock()
	if !needsRead {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Recovery is already authoritative. Match the existing background
	// cooldown persistence even if its management request was canceled.
	return m.restoreCooldownStates(context.WithoutCancel(ctx), false)
}

func (m *Manager) restoreCooldownStates(ctx context.Context, persist bool) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var store CooldownStateStore
	var recoveryVersions map[string]uint64
	var registrations map[string]uint64
	for {
		m.mu.RLock()
		store = m.cooldownStore
		recoveryVersions = make(map[string]uint64, len(m.quotaResetStates))
		registrations = make(map[string]uint64, len(m.auths))
		for id, auth := range m.auths {
			if auth != nil {
				registrations[id] = auth.RegistrationEpoch
			}
		}
		pending := make([]<-chan struct{}, 0)
		for authID, state := range m.quotaResetStates {
			recoveryVersions[authID] = state.recoveryVersion
			if state.publications > 0 {
				pending = append(pending, state.published)
			}
		}
		m.mu.RUnlock()
		if store == nil || len(pending) == 0 {
			break
		}
		// Recovery is published before disk persistence. Wait for publication
		// to finish before capturing a recovery version for a new disk read.
		for _, done := range pending {
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if store == nil {
		return nil
	}
	records, errLoad := store.Load(ctx)
	if errLoad != nil {
		return errLoad
	}
	if len(records) == 0 {
		m.mu.Lock()
		if m.cooldownStore == store {
			m.cooldownReadStore = store
			m.cooldownUnregistered = nil
		}
		m.mu.Unlock()
		return nil
	}

	now := time.Now()
	authLevelRecords := make([]CooldownStateRecord, 0)
	snapshotsByID := make(map[string]*Auth)

	m.mu.Lock()
	records = m.reconcileCooldownRecoveryRecordsLocked(records, recoveryVersions, registrations, now)
	for _, record := range records {
		if strings.TrimSpace(record.Model) == "" {
			authLevelRecords = append(authLevelRecords, record)
			continue
		}
		if m.restoreCooldownRecordLocked(record, now) {
			if auth := m.auths[strings.TrimSpace(record.AuthID)]; auth != nil {
				snapshotsByID[auth.ID] = auth.Clone()
			}
		}
	}
	for _, record := range authLevelRecords {
		if m.restoreCooldownRecordLocked(record, now) {
			if auth := m.auths[strings.TrimSpace(record.AuthID)]; auth != nil {
				snapshotsByID[auth.ID] = auth.Clone()
			}
		}
	}
	for authID := range snapshotsByID {
		if auth := m.auths[authID]; auth != nil && m.cooldownDisabledForAuth(auth) {
			if clearDisabledCooldownStateForAuth(auth, now) {
				snapshotsByID[authID] = auth.Clone()
			}
		}
	}
	if m.cooldownStore == store {
		m.cooldownReadStore = store
		m.cooldownUnregistered = nil
		for _, record := range records {
			id := strings.TrimSpace(record.AuthID)
			if m.auths[id] == nil && m.authEpochs[id] == 0 {
				m.cooldownUnregistered = append(m.cooldownUnregistered, record)
			}
		}
	}
	m.mu.Unlock()

	if m.scheduler != nil {
		for _, snapshot := range snapshotsByID {
			m.scheduler.upsertAuth(snapshot)
		}
	}
	if persist {
		m.persistCooldownStates(context.Background())
	}
	return nil
}

// Reconcile only the loaded snapshot before merging it with live state. Recovery
// versions are separate from quota-read epochs so one usage response can still
// reconcile its later model families after proving shared recovery.
func (m *Manager) reconcileCooldownRecoveryRecordsLocked(records []CooldownStateRecord, versions, registrations map[string]uint64, now time.Time) []CooldownStateRecord {
	byAuth := make(map[string][]CooldownStateRecord)
	for _, record := range records {
		id := strings.TrimSpace(record.AuthID)
		byAuth[id] = append(byAuth[id], record)
	}
	reconciled := make([]CooldownStateRecord, 0, len(records))
	for id, loaded := range byAuth {
		recovery := m.quotaResetStates[id]
		version := versions[id]
		auth := m.auths[id]
		if recovery != nil && recovery.removedVersion > version {
			continue
		}
		if registered, existed := registrations[id]; existed && (auth == nil || auth.RegistrationEpoch != registered) {
			continue
		}
		if recovery == nil || recovery.recoveryVersion <= version || auth == nil || auth.Disabled || auth.Status == StatusDisabled || hasUnauthorizedAuthFailure(auth) {
			reconciled = append(reconciled, loaded...)
			continue
		}
		candidate := auth.Clone()
		candidate.ModelStates = nil
		candidate.Quota = QuotaState{}
		candidate.Unavailable = false
		candidate.NextRetryAfter = time.Time{}
		candidate.LastError = nil
		candidate.StatusMessage = ""
		candidate.Status = StatusActive
		candidate.forceCooldownUntil = time.Time{}
		candidate.credQuotaForceUntil = time.Time{}
		candidate.forceCooldownSeeded = true
		candidate.independentCooldown = nil
		candidate.UpdatedAt = time.Time{}
		snapshot := &Manager{auths: map[string]*Auth{id: candidate}}
		if cfg := m.runtimeConfig.Load(); cfg != nil {
			snapshot.runtimeConfig.Store(cfg)
		}
		for _, record := range loaded {
			if strings.TrimSpace(record.Model) != "" {
				snapshot.restoreCooldownRecordLocked(record, now)
			}
		}
		for _, record := range loaded {
			if strings.TrimSpace(record.Model) == "" {
				snapshot.restoreCooldownRecordLocked(record, now)
			}
		}
		if recovery.clearRecoveryVersion > version {
			for _, state := range candidate.ModelStates {
				if state != nil && state.Status != StatusDisabled {
					resetModelState(state, now)
				}
			}
			clearCooldownStateForAuth(candidate, now)
		}
		if recovery.allRecoveryVersion > version {
			recoverProviderQuotaState(candidate, nil, true, true, now)
		}
		selected := make(map[string]struct{})
		for model, modelVersion := range recovery.modelRecoveryVersions {
			if modelVersion > version {
				selected[model] = struct{}{}
			}
		}
		if len(selected) > 0 {
			recoverProviderQuotaState(candidate, selected, false, false, now)
		}
		if recovery.sharedRecoveryVersion > version {
			recoverProviderQuotaState(candidate, nil, false, true, now)
		}
		reconciled = append(reconciled, snapshot.cooldownStateRecordsForAuthLocked(candidate, now)...)
	}
	return reconciled
}

// recordRetryAfter keeps an older auth snapshot alive through its independent
// cause when the recorded outer quota deadline was shorter.
func recordRetryAfter(record CooldownStateRecord) time.Time {
	deadline := record.NextRetryAfter
	if strings.TrimSpace(record.Model) == "" && record.IndependentState != nil && record.IndependentState.NextRetryAfter.After(deadline) {
		deadline = record.IndependentState.NextRetryAfter
	}
	return deadline
}

func (m *Manager) restoreCooldownRecordLocked(record CooldownStateRecord, now time.Time) bool {
	authID := strings.TrimSpace(record.AuthID)
	record.NextRetryAfter = recordRetryAfter(record)
	if authID == "" || record.NextRetryAfter.IsZero() || !record.NextRetryAfter.After(now) {
		return false
	}
	auth := m.auths[authID]
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled || (m.cooldownDisabledForAuth(auth) && !isForceCooldownRecordAt(record, now)) || hasUnauthorizedAuthFailure(auth) {
		return false
	}
	updatedAt := record.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = now
	}
	reason := strings.TrimSpace(record.Reason)
	model := strings.TrimSpace(record.Model)
	quota := record.Quota
	if quota.Exceeded && quota.NextRecoverAt.IsZero() {
		quota.NextRecoverAt = record.NextRetryAfter
	}
	quota.cooldownEvaluated = quota.Exceeded
	if executorKeyFromAuth(auth) == "claude" || strings.EqualFold(strings.TrimSpace(record.Provider), "claude") {
		quota.cooldownSignals = sanitizePersistedClaudeCooldownSignals(record.CooldownSignals)
	}

	seedInitialForceCooldownDeadlines(auth)
	outerForceUntil := recordOuterForceUntil(record)
	indepForceUntil := recordIndependentForceUntil(record)
	if model == "" {
		independentAt := updatedAt
		if record.IndependentState != nil {
			independentAt = record.IndependentState.UpdatedAt
		}
		liveCleared := auth.independentCooldown != nil && !auth.independentCooldown.Unavailable &&
			auth.independentCooldown.NextRetryAfter.IsZero() && auth.independentCooldown.UpdatedAt.After(independentAt)
		if liveCleared && !isRecoverableQuotaCooldown(quota, record.LastError) &&
			!recordAuthLevelForceUntil(record).After(now) && !recordCredQuotaForceUntil(record).After(now) {
			return false
		}
		if record.IndependentState != nil {
			independent := record.IndependentState.Clone()
			independent.quotaForceCooldownUntil = record.IndependentQuotaForceCooldownUntil
			independent.nonQuotaCooldownUntil = record.IndependentNonQuotaCooldownUntil
			independent.forceCooldownUntil = recordIndependentForceUntil(record)
			independent.forceCooldownSeeded = true
			independent.Quota.cooldownEvaluated = independent.Quota.Exceeded
			independent.Quota.cooldownSignals = sanitizePersistedClaudeCooldownSignals(record.IndependentCooldownSignals)
			if !liveCleared {
				if !independent.Unavailable && independent.NextRetryAfter.IsZero() &&
					(auth.independentCooldown == nil || independent.UpdatedAt.After(auth.independentCooldown.UpdatedAt)) {
					auth.independentCooldown = independent
				} else {
					auth.independentCooldown = mergeModelState(auth.independentCooldown, independent)
				}
			}
		}
		prevNextRetry := auth.NextRetryAfter
		prevQuota := auth.Quota.Clone()
		prevUpdatedAt := auth.UpdatedAt
		prevLastErr := cloneError(auth.LastError)
		prevStatusMsg := auth.StatusMessage
		hadLiveAuthBlock := auth.Unavailable && (prevNextRetry.After(now) || (prevQuota.Exceeded && prevQuota.NextRecoverAt.After(now)))
		liveNewer := hadLiveAuthBlock && prevUpdatedAt.After(updatedAt)

		auth.Unavailable = true
		auth.Status = StatusError
		if prevNextRetry.After(now) && prevNextRetry.After(record.NextRetryAfter) {
			auth.NextRetryAfter = prevNextRetry
		} else {
			auth.NextRetryAfter = record.NextRetryAfter
		}
		mergedQuota := quota
		prevCredQuota := prevQuota.Exceeded && strings.TrimSpace(prevQuota.Reason) == "credential_quota" && prevQuota.NextRecoverAt.After(now)
		recCredQuota := quota.Exceeded && strings.TrimSpace(quota.Reason) == "credential_quota" && quota.NextRecoverAt.After(now)
		if prevCredQuota || recCredQuota {
			mergedQuota.Exceeded = true
			mergedQuota.Reason = "credential_quota"
			var credRecover time.Time
			if prevCredQuota {
				credRecover = prevQuota.NextRecoverAt
			}
			if recCredQuota && quota.NextRecoverAt.After(credRecover) {
				credRecover = quota.NextRecoverAt
			}
			mergedQuota.NextRecoverAt = credRecover
			if prevQuota.BackoffLevel > mergedQuota.BackoffLevel {
				mergedQuota.BackoffLevel = prevQuota.BackoffLevel
			}
			if prevCredQuota && (!recCredQuota || prevQuota.NextRecoverAt.After(quota.NextRecoverAt) || len(mergedQuota.cooldownSignals) == 0) {
				mergedQuota.cooldownSignals = prevQuota.cooldownSignals
			}
		} else if prevQuota.Exceeded && prevQuota.NextRecoverAt.After(now) {
			mergedQuota.Exceeded = true
			if prevQuota.NextRecoverAt.After(mergedQuota.NextRecoverAt) {
				mergedQuota.NextRecoverAt = prevQuota.NextRecoverAt
			}
			if prevQuota.BackoffLevel > mergedQuota.BackoffLevel {
				mergedQuota.BackoffLevel = prevQuota.BackoffLevel
			}
			if liveNewer && prevQuota.Reason != "" {
				mergedQuota.Reason = prevQuota.Reason
			} else if mergedQuota.Reason == "" {
				mergedQuota.Reason = prevQuota.Reason
			}
			if prevQuota.NextRecoverAt.After(quota.NextRecoverAt) || len(mergedQuota.cooldownSignals) == 0 {
				mergedQuota.cooldownSignals = prevQuota.cooldownSignals
			}
		}
		applyCooldownFields(&auth.Quota, mergedQuota)
		auth.Quota = mergeQuotaObservation(auth.Quota, prevQuota)
		auth.Quota = mergeQuotaObservation(auth.Quota, quota)
		if credForce := recordCredQuotaForceUntil(record); credForce.After(now) && credForce.After(auth.credQuotaForceUntil) {
			auth.credQuotaForceUntil = credForce
		}
		authForce := recordAuthLevelForceUntil(record)
		if isLegacyAggregatedAuthRecord(record, auth.ModelStates, now) {
			authForce = time.Time{}
		}
		if authForce.After(now) && authForce.After(auth.forceCooldownUntil) {
			auth.forceCooldownUntil = authForce
		}
		if record.QuotaForceCooldownUntil.After(now) && record.QuotaForceCooldownUntil.After(auth.quotaForceCooldownUntil) {
			auth.quotaForceCooldownUntil = record.QuotaForceCooldownUntil
		}
		if record.CredentialRetryAfter.After(now) && record.CredentialRetryAfter.After(auth.credentialRetryAfter) {
			auth.credentialRetryAfter = record.CredentialRetryAfter
		}
		if record.NonQuotaCooldownUntil.After(now) && record.NonQuotaCooldownUntil.After(auth.nonQuotaCooldownUntil) {
			auth.nonQuotaCooldownUntil = record.NonQuotaCooldownUntil
		}
		auth.forceCooldownSeeded = true
		auth.Generation++
		if liveNewer {
			auth.UpdatedAt = prevUpdatedAt
			if prevStatusMsg != "" {
				auth.StatusMessage = prevStatusMsg
			} else if reason != "" {
				auth.StatusMessage = reason
			}
			if prevLastErr != nil {
				auth.LastError = prevLastErr
			} else {
				auth.LastError = cloneError(record.LastError)
			}
		} else {
			auth.UpdatedAt = updatedAt
			if reason != "" {
				auth.StatusMessage = reason
			}
			auth.LastError = cloneError(record.LastError)
		}
		return true
	}

	state := ensureModelState(auth, model)
	seedModelForceCooldownDeadline(state)
	wrappedRecord := record.IndependentState != nil || record.CredentialQuotaPropagated || quota.Reason == "credential_quota"
	restored := &ModelState{
		Unavailable: true, Status: StatusError, StatusMessage: reason,
		NextRetryAfter: record.NextRetryAfter, Quota: quota,
		LastError: cloneError(record.LastError), UpdatedAt: updatedAt,
		forceCooldownUntil: outerForceUntil, forceCooldownSeeded: true,
	}
	restored.quotaForceCooldownUntil = record.QuotaForceCooldownUntil
	restored.nonQuotaCooldownUntil = record.NonQuotaCooldownUntil
	if wrappedRecord {
		restored.quotaForceCooldownUntil = time.Time{}
		restored.credentialQuotaPropagated = true
		restored.forceCooldownUntil = time.Time{}
		if outerForceUntil.After(now) && outerForceUntil.After(auth.credQuotaForceUntil) {
			auth.credQuotaForceUntil = outerForceUntil
		}
		restored.credentialQuotaShadow = record.IndependentState.Clone()
		if shadow := restored.credentialQuotaShadow; shadow != nil {
			shadow.Quota.cooldownEvaluated = shadow.Quota.Exceeded
			shadow.quotaForceCooldownUntil = record.IndependentQuotaForceCooldownUntil
			shadow.nonQuotaCooldownUntil = record.IndependentNonQuotaCooldownUntil
			shadow.forceCooldownUntil = indepForceUntil
			shadow.forceCooldownSeeded = true
			shadow.Quota.cooldownSignals = sanitizePersistedClaudeCooldownSignals(record.IndependentCooldownSignals)
		}
	}
	if record.ScopedQuotaPropagated || record.ScopedQuotaIndependentState != nil {
		scoped := restored
		if wrappedRecord {
			if restored.credentialQuotaShadow == nil {
				restored.credentialQuotaShadow = &ModelState{}
			}
			scoped = restored.credentialQuotaShadow
		}
		scoped.scopedQuotaPropagated = record.ScopedQuotaPropagated
		scoped.scopedQuotaShadow = record.ScopedQuotaIndependentState.Clone()
		if shadow := scoped.scopedQuotaShadow; shadow != nil {
			shadow.quotaForceCooldownUntil = record.ScopedQuotaIndependentQuotaForceUntil
			shadow.nonQuotaCooldownUntil = record.ScopedQuotaIndependentNonQuotaUntil
			shadow.forceCooldownUntil = recordScopedIndependentForceUntil(record)
			shadow.forceCooldownSeeded = true
			shadow.Quota.cooldownEvaluated = shadow.Quota.Exceeded
			shadow.Quota.cooldownSignals = sanitizePersistedClaudeCooldownSignals(record.ScopedQuotaIndependentSignals)
		}
	}
	// Old Devin reporting states represented credential exhaustion without a
	// marker. They must not become independent cooldowns during restoration.
	if wrappedRecord && modelQuotaLayer(state) == 0 && isDevinQuotaExhaustionMessage(devinStateErrorText(state)) {
		resetModelState(state, now)
	}
	if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now) && modelQuotaLayer(state) != 2 {
		var independent *ModelState
		if isModelStateActiveCooldown(state, now) && !isDevinQuotaExhaustionMessage(devinStateErrorText(state)) {
			independent = state.Clone()
		}
		*state = ModelState{
			Unavailable: true, Status: StatusError, NextRetryAfter: auth.Quota.NextRecoverAt,
			Quota: cooldownFieldsOf(auth.Quota), UpdatedAt: auth.UpdatedAt,
			credentialQuotaPropagated: true, credentialQuotaShadow: independent,
			forceCooldownSeeded: true,
		}
	}
	if modelQuotaLayer(state) == 0 || wrappedRecord || record.ScopedQuotaPropagated || !isDevinQuotaExhaustionMessage(devinRecordErrorText(record)) {
		mergeModelState(state, restored)
	}
	auth.forceCooldownSeeded = true
	auth.Generation++
	if updatedAt.After(auth.UpdatedAt) {
		auth.UpdatedAt = updatedAt
	}
	updateAggregatedAvailability(auth, now)
	return true
}

func isForceCooldownError(err *Error) bool {
	return err != nil && err.Code == ErrorCodeForceCooldown
}

func recordOuterForceUntil(record CooldownStateRecord) time.Time {
	if record.ForceCooldownEvaluated {
		return record.ForceCooldownUntil
	}
	if isForceCooldownError(record.LastError) {
		return record.NextRetryAfter
	}
	return time.Time{}
}

func recordCredQuotaForceUntil(record CooldownStateRecord) time.Time {
	if record.ForceCooldownEvaluated {
		return record.CredQuotaForceCooldownUntil
	}
	if strings.TrimSpace(record.Quota.Reason) == "credential_quota" && isForceCooldownError(record.LastError) {
		if !record.Quota.NextRecoverAt.IsZero() {
			return record.Quota.NextRecoverAt
		}
		return record.NextRetryAfter
	}
	return time.Time{}
}

func recordAuthLevelForceUntil(record CooldownStateRecord) time.Time {
	if record.ForceCooldownEvaluated {
		return record.ForceCooldownUntil
	}
	if strings.TrimSpace(record.Quota.Reason) != "credential_quota" && isForceCooldownError(record.LastError) {
		return record.NextRetryAfter
	}
	return time.Time{}
}

// isLegacyAggregatedAuthRecord reports whether a pre-ForceCooldownEvaluated
// Model=="" record is merely the updateAggregatedAvailability copy of
// auth.ModelStates rather than an independent model-less cooldown.
func isLegacyAggregatedAuthRecord(record CooldownStateRecord, states map[string]*ModelState, now time.Time) bool {
	if record.ForceCooldownEvaluated || len(states) == 0 || strings.TrimSpace(record.Quota.Reason) == "credential_quota" {
		return false
	}
	var earliestRetry time.Time
	matchedOrigin := false
	hasState := false
	for _, st := range states {
		if st == nil {
			continue
		}
		hasState = true
		if st.Status == StatusDisabled {
			continue
		}
		if !st.Unavailable || st.NextRetryAfter.IsZero() || !st.NextRetryAfter.After(now) {
			return false
		}
		if earliestRetry.IsZero() || st.NextRetryAfter.Before(earliestRetry) {
			earliestRetry = st.NextRetryAfter
		}
		if st.UpdatedAt.Equal(record.UpdatedAt) && cooldownErrorEqual(st.LastError, record.LastError) {
			matchedOrigin = true
		}
	}
	return hasState && matchedOrigin && !earliestRetry.IsZero() && record.NextRetryAfter.Equal(earliestRetry)
}

func recordIndependentForceUntil(record CooldownStateRecord) time.Time {
	if record.IndependentState == nil {
		return time.Time{}
	}
	if record.ForceCooldownEvaluated {
		return record.IndependentForceCooldownUntil
	}
	if isForceCooldownError(record.IndependentState.LastError) {
		return record.IndependentState.NextRetryAfter
	}
	return time.Time{}
}

func isForceCooldownRecordAt(record CooldownStateRecord, now time.Time) bool {
	if strings.TrimSpace(record.Model) == "" {
		return recordAuthLevelForceUntil(record).After(now) || recordCredQuotaForceUntil(record).After(now) || recordIndependentForceUntil(record).After(now)
	}
	return recordOuterForceUntil(record).After(now) || recordIndependentForceUntil(record).After(now) || recordScopedIndependentForceUntil(record).After(now)
}

func recordScopedIndependentForceUntil(record CooldownStateRecord) time.Time {
	if record.ScopedQuotaIndependentState == nil {
		return time.Time{}
	}
	if record.ForceCooldownEvaluated {
		return record.ScopedQuotaIndependentForceUntil
	}
	if isForceCooldownError(record.ScopedQuotaIndependentState.LastError) {
		return record.ScopedQuotaIndependentState.NextRetryAfter
	}
	return time.Time{}
}

// Pure read-only force deadline accessors safe under RLock.
func effectiveCredQuotaForceUntil(auth *Auth) time.Time {
	if auth == nil {
		return time.Time{}
	}
	if auth.forceCooldownSeeded {
		return auth.credQuotaForceUntil
	}
	if isForceCooldownError(auth.LastError) && auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" {
		return auth.Quota.NextRecoverAt
	}
	return time.Time{}
}

func effectiveAuthForceUntil(auth *Auth) time.Time {
	if auth == nil {
		return time.Time{}
	}
	if auth.forceCooldownSeeded {
		return auth.forceCooldownUntil
	}
	if len(auth.ModelStates) == 0 && isForceCooldownError(auth.LastError) {
		return auth.NextRetryAfter
	}
	return time.Time{}
}

func effectiveAuthCooldownUntil(auth *Auth) time.Time {
	until := effectiveAuthForceUntil(auth)
	if auth != nil && auth.independentCooldown != nil && auth.independentCooldown.Unavailable && auth.independentCooldown.NextRetryAfter.After(until) {
		until = auth.independentCooldown.NextRetryAfter
	}
	return until
}

func effectiveModelForceUntil(state *ModelState) time.Time {
	if state == nil {
		return time.Time{}
	}
	if state.forceCooldownSeeded {
		return state.forceCooldownUntil
	}
	isWrapped := state.credentialQuotaPropagated || state.Quota.Reason == "credential_quota"
	if !isWrapped && isForceCooldownError(state.LastError) {
		return state.NextRetryAfter
	}
	return time.Time{}
}

func seedModelForceCooldownDeadline(state *ModelState) {
	if state == nil {
		return
	}
	if state.credentialQuotaShadow != nil {
		seedModelForceCooldownDeadline(state.credentialQuotaShadow)
	}
	if state.scopedQuotaShadow != nil {
		seedModelForceCooldownDeadline(state.scopedQuotaShadow)
	}
	if state.forceCooldownSeeded {
		return
	}
	state.forceCooldownSeeded = true
	isWrapped := state.credentialQuotaPropagated || state.Quota.Reason == "credential_quota"
	if !isWrapped && isForceCooldownError(state.LastError) && !state.NextRetryAfter.IsZero() {
		state.forceCooldownUntil = state.NextRetryAfter
	}
}

// seedInitialForceCooldownDeadlines initializes explicit force-cooldown provenance
// once from pre-populated Auth/ModelState struct literals before runtime updates.
func seedInitialForceCooldownDeadlines(auth *Auth) {
	if auth == nil {
		return
	}
	seedModelForceCooldownDeadline(auth.independentCooldown)
	for _, state := range auth.ModelStates {
		seedModelForceCooldownDeadline(state)
	}
	if auth.forceCooldownSeeded {
		return
	}
	auth.forceCooldownSeeded = true
	if !isForceCooldownError(auth.LastError) {
		return
	}
	if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && !auth.Quota.NextRecoverAt.IsZero() {
		auth.credQuotaForceUntil = auth.Quota.NextRecoverAt
	} else if len(auth.ModelStates) == 0 && !auth.NextRetryAfter.IsZero() {
		auth.forceCooldownUntil = auth.NextRetryAfter
	}
}

func clampStateToForceDeadline(state *ModelState, now time.Time) (kept bool, mutated bool) {
	if state == nil {
		return false, false
	}
	seedModelForceCooldownDeadline(state)
	// Provider-reported scoped quota is ordinary cooling. Its independent
	// layer may carry an explicit force deadline, which must survive by itself.
	if state.scopedQuotaPropagated || state.scopedQuotaShadow != nil {
		if shadow := state.scopedQuotaShadow; shadow != nil {
			if keptShadow, _ := clampStateToForceDeadline(shadow, now); keptShadow {
				*state = *shadow.Clone()
				return true, true
			}
		}
		return false, false
	}
	if !state.forceCooldownUntil.After(now) || !state.NextRetryAfter.After(now) {
		return false, false
	}
	if state.NextRetryAfter.After(state.forceCooldownUntil) {
		state.NextRetryAfter = state.forceCooldownUntil
		mutated = true
	}
	if state.Quota.Exceeded && state.Quota.NextRecoverAt.After(state.forceCooldownUntil) {
		state.Quota.NextRecoverAt = state.forceCooldownUntil
		mutated = true
	}
	return true, mutated
}

// clearDisabledCooldownStateForAuth clears ordinary cooldown/quota state for an auth
// under disable-cooling while preserving active ErrorCodeForceCooldown deadlines.
func clearDisabledCooldownStateForAuth(auth *Auth, now time.Time) bool {
	if auth == nil || hasUnauthorizedAuthFailure(auth) {
		return false
	}
	seedInitialForceCooldownDeadlines(auth)
	independentChanged := false
	if state := auth.independentCooldown; state != nil {
		kept, mutated := clampStateToForceDeadline(state, now)
		independentChanged = mutated || !kept
		if !kept {
			auth.independentCooldown = nil
		}
	}

	keepCredQuotaForce := auth.Quota.Exceeded &&
		auth.Quota.Reason == "credential_quota" &&
		auth.credQuotaForceUntil.After(now) &&
		auth.Quota.NextRecoverAt.After(now)
	keepAuthLevelForce := auth.Unavailable &&
		auth.forceCooldownUntil.After(now) &&
		auth.NextRetryAfter.After(now)
	combinedAuthForceUntil := time.Time{}
	if keepCredQuotaForce {
		combinedAuthForceUntil = auth.credQuotaForceUntil
	}
	if keepAuthLevelForce && auth.forceCooldownUntil.After(combinedAuthForceUntil) {
		combinedAuthForceUntil = auth.forceCooldownUntil
	}

	if len(auth.ModelStates) == 0 {
		if keepCredQuotaForce || keepAuthLevelForce {
			changed := independentChanged
			if !keepCredQuotaForce && !auth.credQuotaForceUntil.IsZero() {
				auth.credQuotaForceUntil = time.Time{}
				changed = true
			}
			if !keepAuthLevelForce && !auth.forceCooldownUntil.IsZero() {
				auth.forceCooldownUntil = time.Time{}
				changed = true
			}
			if !auth.credentialRetryAfter.IsZero() {
				auth.credentialRetryAfter = time.Time{}
				changed = true
			}
			if keepCredQuotaForce {
				if !auth.Quota.NextRecoverAt.Equal(auth.credQuotaForceUntil) {
					auth.Quota.NextRecoverAt = auth.credQuotaForceUntil
					changed = true
				}
			} else if auth.Quota.Reason == "credential_quota" {
				applyCooldownFields(&auth.Quota, QuotaState{})
				changed = true
			} else if auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(combinedAuthForceUntil) {
				auth.Quota.NextRecoverAt = combinedAuthForceUntil
				changed = true
			}
			if !auth.NextRetryAfter.Equal(combinedAuthForceUntil) {
				auth.NextRetryAfter = combinedAuthForceUntil
				changed = true
			}
			if changed {
				auth.Generation++
				auth.UpdatedAt = now
			}
			return changed
		}
		return clearCooldownStateForAuth(auth, now) || independentChanged
	}

	savedAuthNextRetry := combinedAuthForceUntil
	savedAuthQuota := cooldownFieldsOf(auth.Quota)
	if !keepCredQuotaForce {
		if savedAuthQuota.Reason == "credential_quota" {
			savedAuthQuota = QuotaState{}
		} else if keepAuthLevelForce && savedAuthQuota.Exceeded && savedAuthQuota.NextRecoverAt.After(auth.forceCooldownUntil) {
			savedAuthQuota.NextRecoverAt = auth.forceCooldownUntil
		}
	}

	changed := independentChanged
	if keepCredQuotaForce {
		if !auth.Quota.NextRecoverAt.Equal(auth.credQuotaForceUntil) {
			auth.Quota.NextRecoverAt = auth.credQuotaForceUntil
			changed = true
		}
		if !auth.NextRetryAfter.Equal(combinedAuthForceUntil) {
			auth.NextRetryAfter = combinedAuthForceUntil
			changed = true
		}
	}

	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		isWrapped := state.credentialQuotaPropagated || state.Quota.Reason == "credential_quota"
		if isWrapped {
			if keepCredQuotaForce {
				var shadowDeadline time.Time
				if state.credentialQuotaShadow != nil {
					keptShadow, mutatedShadow := clampStateToForceDeadline(state.credentialQuotaShadow, now)
					if !keptShadow {
						state.credentialQuotaShadow = nil
						state.UpdatedAt = now
						changed = true
					} else {
						shadowDeadline = state.credentialQuotaShadow.NextRetryAfter
						if mutatedShadow {
							state.UpdatedAt = now
							changed = true
						}
					}
				}
				effectiveOuter := auth.credQuotaForceUntil
				if shadowDeadline.After(effectiveOuter) {
					effectiveOuter = shadowDeadline
				}
				if state.Quota.NextRecoverAt.After(auth.credQuotaForceUntil) {
					state.Quota.NextRecoverAt = auth.credQuotaForceUntil
					state.UpdatedAt = now
					changed = true
				}
				if state.NextRetryAfter.After(effectiveOuter) {
					state.NextRetryAfter = effectiveOuter
					state.UpdatedAt = now
					changed = true
				}
				continue
			}
			if shadow := state.credentialQuotaShadow; shadow != nil {
				if keptShadow, _ := clampStateToForceDeadline(shadow, now); keptShadow {
					preservedSeq := shadow.cooldownRecordedSeq
					*state = *shadow.cloneShallow()
					state.cooldownRecordedSeq = preservedSeq
					state.credentialQuotaPropagated = false
					state.credentialQuotaShadow = nil
					state.UpdatedAt = now
					changed = true
					continue
				}
			}
		} else if keptModel, mutatedModel := clampStateToForceDeadline(state, now); keptModel {
			if mutatedModel {
				state.UpdatedAt = now
				changed = true
			}
			continue
		}
		if state.Unavailable || !state.NextRetryAfter.IsZero() || state.Quota.Exceeded || !state.Quota.NextRecoverAt.IsZero() || state.credentialQuotaShadow != nil || state.credentialQuotaPropagated || !state.forceCooldownUntil.IsZero() {
			state.Unavailable = false
			state.NextRetryAfter = time.Time{}
			applyCooldownFields(&state.Quota, QuotaState{})
			state.cooldownRecordedSeq = 0
			state.forceCooldownUntil = time.Time{}
			state.quotaForceCooldownUntil = time.Time{}
			state.nonQuotaCooldownUntil = time.Time{}
			state.credentialQuotaShadow = nil
			state.credentialQuotaPropagated = false
			state.scopedQuotaShadow = nil
			state.scopedQuotaPropagated = false
			state.UpdatedAt = now
			changed = true
		}
	}

	if !keepCredQuotaForce {
		auth.credQuotaForceUntil = time.Time{}
	}
	if !keepAuthLevelForce {
		auth.forceCooldownUntil = time.Time{}
		auth.quotaForceCooldownUntil = time.Time{}
	}
	if !auth.credentialRetryAfter.IsZero() {
		auth.credentialRetryAfter = time.Time{}
		changed = true
	}
	if !keepAuthLevelForce && !auth.nonQuotaCooldownUntil.IsZero() {
		auth.nonQuotaCooldownUntil = time.Time{}
	}

	prevUnavailable := auth.Unavailable
	prevNextRetry := auth.NextRetryAfter
	prevQuota := cooldownFieldsOf(auth.Quota)
	if !keepCredQuotaForce {
		auth.Unavailable = false
		auth.NextRetryAfter = time.Time{}
		applyCooldownFields(&auth.Quota, QuotaState{})
		if !keepAuthLevelForce {
			auth.cooldownRecordedSeq = 0
		}
		updateAggregatedAvailability(auth, now)
	}
	if keepAuthLevelForce {
		auth.Unavailable = true
		if savedAuthNextRetry.After(auth.NextRetryAfter) {
			auth.NextRetryAfter = savedAuthNextRetry
		}
		if !keepCredQuotaForce && savedAuthQuota.Exceeded && savedAuthQuota.NextRecoverAt.After(now) && !auth.Quota.NextRecoverAt.After(savedAuthQuota.NextRecoverAt) {
			applyCooldownFields(&auth.Quota, savedAuthQuota)
		}
	}
	if auth.Unavailable != prevUnavailable || !auth.NextRetryAfter.Equal(prevNextRetry) || !cooldownQuotaEqual(cooldownFieldsOf(auth.Quota), prevQuota) {
		changed = true
	}
	if changed {
		auth.Generation++
		auth.UpdatedAt = now
	}
	return changed
}

func clearCooldownStateForAuth(auth *Auth, now time.Time) bool {
	if auth == nil {
		return false
	}
	if hasUnauthorizedAuthFailure(auth) {
		return false
	}
	changed := auth.independentCooldown != nil
	auth.independentCooldown = nil
	auth.forceCooldownUntil = time.Time{}
	auth.quotaForceCooldownUntil = time.Time{}
	auth.credQuotaForceUntil = time.Time{}
	auth.credentialRetryAfter = time.Time{}
	auth.nonQuotaCooldownUntil = time.Time{}
	auth.cooldownRecordedSeq = 0
	if auth.Unavailable || !auth.NextRetryAfter.IsZero() || auth.Quota.Exceeded || !auth.Quota.NextRecoverAt.IsZero() {
		auth.Unavailable = false
		auth.NextRetryAfter = time.Time{}
		applyCooldownFields(&auth.Quota, QuotaState{})
		auth.UpdatedAt = now
		changed = true
	}
	for _, state := range auth.ModelStates {
		if state == nil || state.Status == StatusDisabled {
			continue
		}
		state.forceCooldownUntil = time.Time{}
		state.quotaForceCooldownUntil = time.Time{}
		state.nonQuotaCooldownUntil = time.Time{}
		state.cooldownRecordedSeq = 0
		if state.Unavailable || !state.NextRetryAfter.IsZero() || state.Quota.Exceeded || !state.Quota.NextRecoverAt.IsZero() || state.credentialQuotaShadow != nil || state.credentialQuotaPropagated {
			state.Unavailable = false
			state.NextRetryAfter = time.Time{}
			applyCooldownFields(&state.Quota, QuotaState{})
			state.credentialQuotaShadow = nil
			state.credentialQuotaPropagated = false
			state.scopedQuotaShadow = nil
			state.scopedQuotaPropagated = false
			state.UpdatedAt = now
			changed = true
		}
	}
	if len(auth.ModelStates) > 0 {
		updateAggregatedAvailability(auth, now)
	}
	if changed {
		auth.Generation++
		auth.UpdatedAt = now
	}
	return changed
}

func dedupeStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	seen := make(map[string]struct{}, len(values))
	out := values[:0]
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// ResetQuota clears quota/cooldown state for an auth and resumes registry routing.
func (m *Manager) ResetQuota(ctx context.Context, authID string) (*Auth, []string, error) {
	if m == nil {
		return nil, nil, nil
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil, nil, fmt.Errorf("auth id is required")
	}
	if errRestore := m.prepareQuotaRecovery(ctx, authID, true); errRestore != nil {
		return nil, nil, fmt.Errorf("load cooldown state before reset: %w", errRestore)
	}

	now := time.Now()
	var snapshot *Auth
	models := make([]string, 0)
	registeredModels := modelsForRegisteredAuth(authID)
	cooldownStateChanged := false

	releaseMutation := m.lockAuthMutation(authID)
	defer releaseMutation()
	m.mu.Lock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil {
		m.mu.Unlock()
		return nil, nil, nil
	}
	m.advanceQuotaObservationEpochLocked(authID)
	m.InvalidateQuotaReserveObservation(authID, nil)
	m.beginQuotaResetPublicationLocked(authID, nil, true)
	m.quotaResetStates[authID].clearRecoveryVersion = m.quotaResetStates[authID].recoveryVersion
	defer m.finishQuotaResetPublication(authID)

	var cooldownRecordsBefore []CooldownStateRecord
	trackCooldownState := m.cooldownStore != nil
	if trackCooldownState {
		cooldownRecordsBefore = m.cooldownStateRecordsForAuthLocked(auth, now)
	}

	for modelKey, state := range auth.ModelStates {
		if strings.TrimSpace(modelKey) == "" {
			continue
		}
		models = append(models, modelKey)
		if state != nil && state.Status != StatusDisabled {
			resetModelState(state, now)
		}
	}
	if clearCooldownStateForAuth(auth, now) {
		if len(models) == 0 {
			models = append(models, registeredModels...)
		}
	} else if len(auth.ModelStates) > 0 {
		updateAggregatedAvailability(auth, now)
	}

	if len(models) == 0 {
		models = append(models, registeredModels...)
	}
	models = dedupeStrings(models)

	if !auth.Disabled && auth.Status != StatusDisabled && !hasModelError(auth, now) {
		if !hasUnauthorizedAuthFailure(auth) {
			auth.LastError = nil
			auth.StatusMessage = ""
			auth.Status = StatusActive
		}
	}
	auth.Generation++
	auth.UpdatedAt = now
	snapshot = auth.Clone()
	if trackCooldownState {
		cooldownRecordsAfter := m.cooldownStateRecordsForAuthLocked(auth, now)
		cooldownStateChanged = !cooldownStateRecordsEqual(cooldownRecordsBefore, cooldownRecordsAfter)
	}
	errPersist := m.persistLocked(ctx, auth)
	m.mu.Unlock()
	releaseMutation()

	defer func() {
		if cooldownStateChanged {
			m.persistCooldownStates(context.Background())
		}
	}()

	supportedModels, regEpoch := registry.GetGlobalRegistry().GetModelsAndEpochForClient(authID)
	projections := make([]registry.ClientModelProjection, 0, len(supportedModels))
	for _, sm := range supportedModels {
		if sm == nil || strings.TrimSpace(sm.ID) == "" {
			continue
		}
		projections = append(projections, m.clientModelProjectionForAuth(snapshot, sm.ID, now))
	}
	if snapshot != nil {
		registry.GetGlobalRegistry().ApplyClientModelProjections(authID, regEpoch, snapshot.Generation, projections)
	}
	if m.scheduler != nil && snapshot != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	if errPersist != nil {
		return nil, nil, errPersist
	}
	return snapshot, models, nil
}

func modelsForRegisteredAuth(authID string) []string {
	supportedModels := registry.GetGlobalRegistry().GetModelsForClient(authID)
	models := make([]string, 0, len(supportedModels))
	for _, supportedModel := range supportedModels {
		if supportedModel == nil || strings.TrimSpace(supportedModel.ID) == "" {
			continue
		}
		models = append(models, canonicalModelKey(supportedModel.ID))
	}
	return models
}

// ApplyProviderQuotaReport synchronizes persisted routing cooldowns with a
// freshly fetched provider usage report. A healthy report clears quota
// cooldowns so a manually reset credential can rejoin routing immediately.
func (m *Manager) ApplyProviderQuotaReport(ctx context.Context, authID string, exhausted bool, resetAt time.Time) (bool, error) {
	return m.ApplyProviderQuotaReportForModels(ctx, authID, exhausted, resetAt, nil)
}

// ApplyProviderQuotaReportForModels applies a provider quota report to the
// selected model states. A nil model list keeps the historical credential-wide
// behavior; a non-nil list lets providers with independent model allowances
// recover one family without clearing another family's cooldown.
func (m *Manager) ApplyProviderQuotaReportForModels(ctx context.Context, authID string, exhausted bool, resetAt time.Time, modelIDs []string) (bool, error) {
	return m.applyProviderQuotaReport(ctx, authID, exhausted, resetAt, modelIDs, false, false)
}

// ApplyProviderQuotaRecovery releases a recovered allowance, including an older
// credential-wide quota refusal. Independent limits on other models survive.
func (m *Manager) ApplyProviderQuotaRecovery(ctx context.Context, authID string, modelIDs []string) (bool, error) {
	return m.applyProviderQuotaReport(ctx, authID, false, time.Time{}, modelIDs, true, false)
}

// ApplyProviderQuotaBars reconciles credential-wide normalized quota windows.
// Empty, malformed or exhausted reports without a future reset are inconclusive.
func (m *Manager) ApplyProviderQuotaBars(ctx context.Context, authID string, bars []QuotaReserveBar) (bool, error) {
	return m.ApplyProviderQuotaBarsForModels(ctx, authID, bars, nil)
}

// ApplyProviderQuotaBarsForModels reconciles one independently reported model pool.
// A nil model list applies to the entire credential.
func (m *Manager) ApplyProviderQuotaBarsForModels(ctx context.Context, authID string, bars []QuotaReserveBar, modelIDs []string) (bool, error) {
	if len(bars) == 0 {
		return false, nil
	}
	var reset time.Time
	for _, bar := range bars {
		if math.IsNaN(bar.RemainingPercent) || math.IsInf(bar.RemainingPercent, 0) || bar.RemainingPercent < 0 || bar.RemainingPercent > 100 {
			return false, nil
		}
		if bar.RemainingPercent > 0 {
			continue
		}
		if !bar.ResetAt.After(time.Now()) {
			return false, nil
		}
		if reset.IsZero() || bar.ResetAt.Before(reset) {
			reset = bar.ResetAt
		}
	}
	if reset.IsZero() {
		return m.ApplyProviderQuotaRecovery(ctx, authID, modelIDs)
	}
	return m.ApplyProviderQuotaReportForModels(ctx, authID, true, reset, modelIDs)
}

func (m *Manager) applyProviderQuotaReport(ctx context.Context, authID string, exhausted bool, resetAt time.Time, modelIDs []string, recoverCredential bool, resetObservation bool) (bool, error) {
	if m == nil {
		return false, nil
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false, fmt.Errorf("auth id is required")
	}

	now := time.Now()
	if exhausted {
		resetAt = resetAt.Round(0)
		if !resetAt.After(now) {
			return false, nil
		}
	} else {
		resetAt = time.Time{}
		if errRestore := m.prepareQuotaRecovery(ctx, authID, resetObservation); errRestore != nil {
			return false, fmt.Errorf("load cooldown state before quota recovery: %w", errRestore)
		}
	}

	releaseMutation := m.lockAuthMutation(authID)
	defer releaseMutation()
	m.mu.Lock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil {
		m.mu.Unlock()
		return false, nil
	}
	if resetObservation {
		m.beginQuotaResetLocked(ctx, authID)
	} else if !m.quotaObservationCurrentLocked(ctx, authID) {
		m.mu.Unlock()
		return false, nil
	}
	if auth.Disabled || auth.Status == StatusDisabled || hasUnauthorizedAuthFailure(auth) || m.cooldownDisabledForAuth(auth) {
		m.mu.Unlock()
		return false, nil
	}
	if !exhausted {
		if resetObservation {
			m.beginQuotaResetPublicationLocked(authID, modelIDs, recoverCredential)
		} else {
			m.beginQuotaRecoveryPublicationLocked(authID, modelIDs, recoverCredential)
		}
		defer m.finishQuotaResetPublication(authID)
	}

	var cooldownRecordsBefore []CooldownStateRecord
	trackCooldownState := m.cooldownStore != nil
	if trackCooldownState {
		cooldownRecordsBefore = m.cooldownStateRecordsForAuthLocked(auth, now)
	}

	selectedModels := make(map[string]struct{}, len(modelIDs))
	for _, modelID := range modelIDs {
		if modelKey := canonicalModelKey(modelID); modelKey != "" {
			selectedModels[modelKey] = struct{}{}
		}
	}
	selectAllModels := modelIDs == nil
	changed := false

	if exhausted {
		if !selectAllModels {
			if auth.ModelStates == nil {
				auth.ModelStates = make(map[string]*ModelState)
			}
			for modelID := range selectedModels {
				if auth.ModelStates[modelID] == nil {
					auth.ModelStates[modelID] = &ModelState{}
				}
			}
		}
		for modelID, state := range auth.ModelStates {
			if !selectAllModels {
				if _, selected := selectedModels[canonicalModelKey(modelID)]; !selected {
					continue
				}
			}
			if state == nil || state.Status == StatusDisabled {
				continue
			}
			if !selectAllModels {
				normalizeModelQuotaLayers(auth, state, now)
			}
			if selectAllModels {
				if !state.credentialQuotaPropagated && isModelStateActiveCooldown(state, now) {
					state.credentialQuotaShadow = state.cloneShallow()
				}
				state.credentialQuotaPropagated = true
			} else {
				if state.credentialQuotaPropagated {
					if state.credentialQuotaShadow == nil {
						state.credentialQuotaShadow = &ModelState{}
					}
					state = state.credentialQuotaShadow
				}
				if !state.scopedQuotaPropagated && isModelStateActiveCooldown(state, now) {
					state.scopedQuotaShadow = state.cloneShallow()
				}
				state.scopedQuotaPropagated = true
			}
			state.Unavailable = true
			state.Status = StatusError
			state.StatusMessage = "quota exhausted"
			state.NextRetryAfter = resetAt
			if shadow := state.credentialQuotaShadow; shadow != nil && shadow.NextRetryAfter.After(state.NextRetryAfter) {
				state.NextRetryAfter = shadow.NextRetryAfter
			}
			if shadow := state.scopedQuotaShadow; shadow != nil && shadow.NextRetryAfter.After(state.NextRetryAfter) {
				state.NextRetryAfter = shadow.NextRetryAfter
			}
			applyCooldownFields(&state.Quota, QuotaState{
				Exceeded:      true,
				Reason:        "quota",
				NextRecoverAt: resetAt,
			})
			state.UpdatedAt = now
			if outer := auth.ModelStates[modelID]; outer != state {
				outer.NextRetryAfter = auth.Quota.NextRecoverAt
				if state.NextRetryAfter.After(outer.NextRetryAfter) {
					outer.NextRetryAfter = state.NextRetryAfter
				}
				if until := effectiveAuthForceUntil(auth); until.After(outer.NextRetryAfter) {
					outer.NextRetryAfter = until
				}
			}
			changed = true
		}
		if selectAllModels {
			auth.Unavailable = true
			auth.Status = StatusError
			auth.StatusMessage = "quota exhausted"
			auth.NextRetryAfter = resetAt
			if state := auth.independentCooldown; state != nil && state.NextRetryAfter.After(auth.NextRetryAfter) {
				auth.NextRetryAfter = state.NextRetryAfter
			}
			if until := effectiveAuthForceUntil(auth); until.After(auth.NextRetryAfter) {
				auth.NextRetryAfter = until
			}
			applyCooldownFields(&auth.Quota, QuotaState{
				Exceeded:      true,
				Reason:        "credential_quota",
				NextRecoverAt: resetAt,
			})
			changed = true
		} else {
			updateAggregatedAvailability(auth, now)
		}
	} else {
		changed = recoverProviderQuotaState(auth, selectedModels, selectAllModels, recoverCredential, now)
		if resetObservation {
			barIDs, _ := ctx.Value(quotaResetBarsKey{}).([]string)
			if (barIDs == nil || len(barIDs) > 0) && QuotaRecoveryProviderForAuth(auth) != "" {
				m.quotaResetStates[authID].quotaObservationSeq = m.currentExecutionSeq()
				changed = clearGSRQuotaObservation(auth) || changed
			}
		}
	}

	if !changed {
		m.mu.Unlock()
		return false, nil
	}

	auth.Generation++
	auth.UpdatedAt = now
	snapshot := auth.Clone()
	cooldownStateChanged := false
	if trackCooldownState {
		cooldownRecordsAfter := m.cooldownStateRecordsForAuthLocked(auth, now)
		cooldownStateChanged = !cooldownStateRecordsEqual(cooldownRecordsBefore, cooldownRecordsAfter)
	}
	errPersist := m.persistLocked(ctx, auth)
	m.mu.Unlock()
	releaseMutation()

	defer func() {
		if cooldownStateChanged {
			m.persistCooldownStates(context.Background())
		}
	}()

	supportedModels, regEpoch := registry.GetGlobalRegistry().GetModelsAndEpochForClient(authID)
	projections := make([]registry.ClientModelProjection, 0, len(supportedModels))
	for _, sm := range supportedModels {
		if sm == nil || strings.TrimSpace(sm.ID) == "" {
			continue
		}
		projections = append(projections, m.clientModelProjectionForAuth(snapshot, sm.ID, now))
	}
	if len(projections) > 0 {
		registry.GetGlobalRegistry().ApplyClientModelProjections(authID, regEpoch, snapshot.Generation, projections)
	}
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	if errPersist != nil {
		return true, errPersist
	}
	return true, nil
}

// recoverProviderQuotaState is the canonical healthy-quota transform used by
// live observations and by disk snapshots that predate a scoped recovery.
func recoverProviderQuotaState(auth *Auth, selectedModels map[string]struct{}, selectAllModels, recoverCredential bool, now time.Time) bool {
	changed := false
	var independentAuthRetry time.Time
	if auth.independentCooldown == nil && auth.Unavailable && auth.NextRetryAfter.After(now) && !isRecoverableQuotaCooldown(auth.Quota, auth.LastError) {
		independentAuthRetry = auth.NextRetryAfter
	}
	if (selectAllModels || recoverCredential) && auth.Quota.Reason == "credential_quota" {
		for _, state := range auth.ModelStates {
			if state == nil || state.Status == StatusDisabled || !state.credentialQuotaPropagated {
				continue
			}
			if shadow := state.credentialQuotaShadow; shadow != nil {
				*state = *shadow.cloneShallow()
			} else {
				resetModelState(state, now)
			}
		}
		auth.credQuotaForceUntil = time.Time{}
		changed = true
	}
	for modelID, state := range auth.ModelStates {
		if !selectAllModels {
			if _, selected := selectedModels[canonicalModelKey(modelID)]; !selected {
				continue
			}
		}
		if state == nil || state.Status == StatusDisabled {
			continue
		}
		if state.credentialQuotaPropagated {
			if shadow := state.credentialQuotaShadow; shadow != nil {
				*state = *shadow.Clone()
			} else {
				resetModelState(state, now)
			}
			changed = true
		}
		if recoverModelQuotaState(state, !selectAllModels, now) {
			changed = true
		}
	}
	if (selectAllModels || recoverCredential || auth.Quota.Reason != "credential_quota") &&
		(auth.Quota.Exceeded || auth.Quota.Reason != "" || !auth.Quota.NextRecoverAt.IsZero()) {
		applyCooldownFields(&auth.Quota, QuotaState{})
		auth.Unavailable = false
		auth.NextRetryAfter = time.Time{}
		changed = true
	}
	if (selectAllModels || recoverCredential) && isRecoverableQuotaCooldown(QuotaState{}, auth.LastError) {
		auth.LastError = nil
		auth.Unavailable = false
		auth.NextRetryAfter = time.Time{}
		if effectiveAuthForceUntil(auth).After(now) {
			auth.StatusMessage = ErrorCodeForceCooldown
		}
		changed = true
	}
	updateAggregatedAvailability(auth, now)
	if independentAuthRetry.After(auth.NextRetryAfter) {
		auth.Unavailable = true
		auth.NextRetryAfter = independentAuthRetry
	}
	if !auth.Disabled && auth.Status != StatusDisabled && !auth.Unavailable && !auth.NextRetryAfter.After(now) && !hasModelError(auth, now) &&
		!(auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now)) {
		if auth.LastError != nil || auth.StatusMessage != "" || auth.Status != StatusActive {
			changed = true
		}
		auth.LastError = nil
		auth.StatusMessage = ""
		auth.Status = StatusActive
	}
	return changed
}

func isRecoverableQuotaCooldown(quota QuotaState, lastError *Error) bool {
	if quota.Reason == "quota" || quota.Reason == "credential_quota" {
		return true
	}
	if lastError != nil && (isForceCooldownError(lastError) || isTransientRateLimitResultError(lastError)) {
		return false
	}
	return lastError != nil && lastError.HTTPStatus == http.StatusTooManyRequests
}

func recoverModelQuotaState(state *ModelState, includeScoped bool, now time.Time) bool {
	changed := false
	if state.scopedQuotaPropagated {
		if !includeScoped {
			return false
		}
		if shadow := state.scopedQuotaShadow; shadow != nil {
			*state = *shadow.Clone()
		} else {
			resetModelState(state, now)
		}
		changed = true
	}
	if !isRecoverableQuotaCooldown(state.Quota, state.LastError) {
		return changed
	}
	forceUntil := effectiveModelForceUntil(state)
	forceSeq := state.cooldownRecordedSeq
	if shadow := state.scopedQuotaShadow; shadow != nil {
		*state = *shadow.Clone()
	} else {
		resetModelState(state, now)
	}
	if forceUntil.After(now) {
		if forceSeq > state.cooldownRecordedSeq {
			state.cooldownRecordedSeq = forceSeq
		}
		if forceUntil.After(state.forceCooldownUntil) {
			state.forceCooldownUntil = forceUntil
		}
		state.forceCooldownSeeded = true
		if forceUntil.After(state.NextRetryAfter) {
			state.NextRetryAfter = forceUntil
		}
		state.Unavailable = true
		state.Status = StatusError
		if state.LastError == nil {
			state.StatusMessage = ErrorCodeForceCooldown
		}
	}
	return true
}

func (m *Manager) persistCooldownStates(ctx context.Context) {
	if m == nil {
		return
	}
	m.configCooldownMu.Lock()
	defer m.configCooldownMu.Unlock()
	m.persistCooldownStatesLocked(ctx)
}

func (m *Manager) persistCooldownStatesLocked(ctx context.Context) {
	m.mu.RLock()
	store := m.cooldownStore
	m.mu.RUnlock()
	if m.persistCooldownStatesToLocked(ctx, store) {
		m.mu.Lock()
		if m.pendingCooldownStateStore == store {
			m.pendingCooldownStateStore = nil
		}
		m.mu.Unlock()
	}
}

func (m *Manager) persistCooldownStatesToLocked(ctx context.Context, store CooldownStateStore) bool {
	if m == nil || store == nil {
		return true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errContext := ctx.Err(); errContext != nil {
		return false
	}
	records := m.cooldownStateRecordsSnapshot()
	m.mu.RLock()
	if m.cooldownReadStore == store {
		now := time.Now()
		for _, record := range m.cooldownUnregistered {
			id := strings.TrimSpace(record.AuthID)
			record.NextRetryAfter = recordRetryAfter(record)
			// Registration alone does not integrate an unread cooldown. Keep
			// it until restoration consumes it or credential removal discards it.
			if (m.auths[id] != nil || m.authEpochs[id] == 0) && record.NextRetryAfter.After(now) {
				records = append(records, record)
			}
		}
	}
	m.mu.RUnlock()
	if errSave := store.Save(ctx, records); errSave != nil {
		logEntryWithRequestID(ctx).Warnf("failed to persist cooldown state: %v", errSave)
		return false
	}
	return ctx.Err() == nil
}

func (m *Manager) cooldownStateRecordsSnapshot() []CooldownStateRecord {
	now := time.Now()
	records := make([]CooldownStateRecord, 0)

	m.mu.RLock()
	for _, auth := range m.auths {
		records = append(records, m.cooldownStateRecordsForAuthLocked(auth, now)...)
	}
	m.mu.RUnlock()

	sort.Slice(records, func(i, j int) bool {
		if records[i].Provider != records[j].Provider {
			return records[i].Provider < records[j].Provider
		}
		if records[i].AuthID != records[j].AuthID {
			return records[i].AuthID < records[j].AuthID
		}
		return records[i].Model < records[j].Model
	})
	return records
}

func (m *Manager) cooldownStateRecordsForAuthLocked(auth *Auth, now time.Time) []CooldownStateRecord {
	if auth == nil || auth.ID == "" || auth.Disabled || auth.Status == StatusDisabled {
		return nil
	}
	coolingDisabled := m.cooldownDisabledForAuth(auth)
	records := make([]CooldownStateRecord, 0, 1+len(auth.ModelStates))
	if record, ok := authCooldownStateRecord(auth, now); ok && (!coolingDisabled || isForceCooldownRecordAt(record, now)) {
		records = append(records, record)
	}
	for model, state := range auth.ModelStates {
		if record, ok := modelCooldownStateRecord(auth, model, state, now); ok && (!coolingDisabled || isForceCooldownRecordAt(record, now)) {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].Model < records[j].Model
	})
	return records
}

func cooldownStateRecordsEqual(a, b []CooldownStateRecord) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !cooldownStateRecordEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func cooldownStateRecordEqual(a, b CooldownStateRecord) bool {
	if a.CredentialQuotaPropagated != b.CredentialQuotaPropagated || a.ScopedQuotaPropagated != b.ScopedQuotaPropagated || !cooldownIndependentStateEqual(a.ScopedQuotaIndependentState, b.ScopedQuotaIndependentState) || !a.ScopedQuotaIndependentForceUntil.Equal(b.ScopedQuotaIndependentForceUntil) || !maps.Equal(a.ScopedQuotaIndependentSignals, b.ScopedQuotaIndependentSignals) {
		return false
	}
	if a.Provider != b.Provider ||
		a.AuthID != b.AuthID ||
		a.AuthFile != b.AuthFile ||
		a.Model != b.Model ||
		a.Status != b.Status ||
		a.Reason != b.Reason ||
		!a.NextRetryAfter.Equal(b.NextRetryAfter) ||
		!a.UpdatedAt.Equal(b.UpdatedAt) ||
		!cooldownQuotaEqual(a.Quota, b.Quota) {
		return false
	}
	if !cooldownErrorEqual(a.LastError, b.LastError) {
		return false
	}
	if !a.QuotaForceCooldownUntil.Equal(b.QuotaForceCooldownUntil) ||
		!a.NonQuotaCooldownUntil.Equal(b.NonQuotaCooldownUntil) ||
		!a.IndependentQuotaForceCooldownUntil.Equal(b.IndependentQuotaForceCooldownUntil) ||
		!a.IndependentNonQuotaCooldownUntil.Equal(b.IndependentNonQuotaCooldownUntil) ||
		!a.ScopedQuotaIndependentQuotaForceUntil.Equal(b.ScopedQuotaIndependentQuotaForceUntil) ||
		!a.ScopedQuotaIndependentNonQuotaUntil.Equal(b.ScopedQuotaIndependentNonQuotaUntil) ||
		a.ForceCooldownEvaluated != b.ForceCooldownEvaluated ||
		!a.ForceCooldownUntil.Equal(b.ForceCooldownUntil) ||
		!a.CredQuotaForceCooldownUntil.Equal(b.CredQuotaForceCooldownUntil) ||
		!a.IndependentForceCooldownUntil.Equal(b.IndependentForceCooldownUntil) ||
		!a.CredentialRetryAfter.Equal(b.CredentialRetryAfter) {
		return false
	}
	if !maps.Equal(a.CooldownSignals, b.CooldownSignals) || !maps.Equal(a.IndependentCooldownSignals, b.IndependentCooldownSignals) {
		return false
	}
	return cooldownIndependentStateEqual(a.IndependentState, b.IndependentState)
}

func cooldownIndependentStateEqual(a, b *ModelState) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Status == b.Status &&
		a.StatusMessage == b.StatusMessage &&
		a.Unavailable == b.Unavailable &&
		a.NextRetryAfter.Equal(b.NextRetryAfter) &&
		a.UpdatedAt.Equal(b.UpdatedAt) &&
		cooldownQuotaEqual(a.Quota, b.Quota) &&
		cooldownErrorEqual(a.LastError, b.LastError)
}

func cooldownQuotaEqual(a, b QuotaState) bool {
	return a.Exceeded == b.Exceeded &&
		a.Reason == b.Reason &&
		a.BackoffLevel == b.BackoffLevel &&
		a.NextRecoverAt.Equal(b.NextRecoverAt)
}

func cooldownErrorEqual(a, b *Error) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Code == b.Code &&
		a.Message == b.Message &&
		a.Retryable == b.Retryable &&
		a.HTTPStatus == b.HTTPStatus &&
		maps.Equal(a.Metadata, b.Metadata)
}

func authCooldownStateRecord(auth *Auth, now time.Time) (CooldownStateRecord, bool) {
	if auth == nil || !auth.Unavailable || auth.NextRetryAfter.IsZero() || !auth.NextRetryAfter.After(now) {
		return CooldownStateRecord{}, false
	}
	var cooldownSignals map[string]string
	if auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(now) && !auth.NextRetryAfter.After(auth.Quota.NextRecoverAt) {
		cooldownSignals = sanitizePersistedClaudeCooldownSignals(auth.Quota.cooldownSignals)
	}
	var authForceUntil time.Time
	if u := effectiveAuthForceUntil(auth); u.After(now) {
		authForceUntil = u
	}
	var credQuotaForceUntil time.Time
	if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now) {
		if u := effectiveCredQuotaForceUntil(auth); u.After(now) {
			credQuotaForceUntil = u
		}
	}
	var credentialRetryAfter time.Time
	if !auth.credentialRetryAfter.IsZero() && auth.credentialRetryAfter.After(now) {
		credentialRetryAfter = auth.credentialRetryAfter
	}
	var authQuotaForceUntil time.Time
	if auth.quotaForceCooldownUntil.After(now) {
		authQuotaForceUntil = auth.quotaForceCooldownUntil
	}
	var nonQuotaUntil time.Time
	if auth.nonQuotaCooldownUntil.After(now) {
		nonQuotaUntil = auth.nonQuotaCooldownUntil
	}
	record := CooldownStateRecord{
		Provider:                    strings.TrimSpace(auth.Provider),
		AuthID:                      auth.ID,
		AuthFile:                    cooldownAuthFile(auth),
		Status:                      "cooling",
		NextRetryAfter:              auth.NextRetryAfter,
		Reason:                      cooldownReason(auth.StatusMessage, auth.Quota, auth.LastError),
		Quota:                       cooldownFieldsOf(auth.Quota),
		LastError:                   cloneError(auth.LastError),
		UpdatedAt:                   auth.UpdatedAt,
		CooldownSignals:             cooldownSignals,
		ForceCooldownUntil:          authForceUntil,
		QuotaForceCooldownUntil:     authQuotaForceUntil,
		CredQuotaForceCooldownUntil: credQuotaForceUntil,
		ForceCooldownEvaluated:      true,
		CredentialRetryAfter:        credentialRetryAfter,
		NonQuotaCooldownUntil:       nonQuotaUntil,
	}
	if state := auth.independentCooldown; state != nil {
		record.IndependentState = state.Clone()
		record.IndependentQuotaForceCooldownUntil = state.quotaForceCooldownUntil
		record.IndependentNonQuotaCooldownUntil = state.nonQuotaCooldownUntil
		record.IndependentForceCooldownUntil = effectiveModelForceUntil(state)
		record.IndependentCooldownSignals = sanitizePersistedClaudeCooldownSignals(state.Quota.cooldownSignals)
	}
	return record, true
}

func modelCooldownStateRecord(auth *Auth, model string, state *ModelState, now time.Time) (CooldownStateRecord, bool) {
	model = strings.TrimSpace(model)
	if auth == nil || state == nil || model == "" || !state.Unavailable || state.NextRetryAfter.IsZero() || !state.NextRetryAfter.After(now) {
		return CooldownStateRecord{}, false
	}
	var cooldownSignals map[string]string
	liveCredentialWrap := auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now)
	expiredCredentialWrap := (state.credentialQuotaPropagated || state.Quota.Reason == "credential_quota") && !liveCredentialWrap
	if !expiredCredentialWrap && state.Quota.Exceeded && state.Quota.NextRecoverAt.After(now) &&
		(state.Quota.Reason == "credential_quota" || !state.NextRetryAfter.After(state.Quota.NextRecoverAt)) {
		cooldownSignals = sanitizePersistedClaudeCooldownSignals(state.Quota.cooldownSignals)
	}
	var outerForceUntil time.Time
	if liveCredentialWrap {
		if u := effectiveCredQuotaForceUntil(auth); u.After(now) {
			outerForceUntil = u
		}
	} else if u := effectiveModelForceUntil(state); u.After(now) {
		outerForceUntil = u
	}
	var modelQuotaForceUntil time.Time
	if !liveCredentialWrap && state.quotaForceCooldownUntil.After(now) {
		modelQuotaForceUntil = state.quotaForceCooldownUntil
	}
	var modelNonQuotaUntil time.Time
	if state.nonQuotaCooldownUntil.After(now) {
		modelNonQuotaUntil = state.nonQuotaCooldownUntil
	}
	record := CooldownStateRecord{
		Provider:                strings.TrimSpace(auth.Provider),
		AuthID:                  auth.ID,
		AuthFile:                cooldownAuthFile(auth),
		Model:                   model,
		Status:                  "cooling",
		NextRetryAfter:          state.NextRetryAfter,
		Reason:                  cooldownReason(state.StatusMessage, state.Quota, state.LastError),
		Quota:                   cooldownFieldsOf(state.Quota),
		LastError:               cloneError(state.LastError),
		UpdatedAt:               state.UpdatedAt,
		CooldownSignals:         cooldownSignals,
		ForceCooldownUntil:      outerForceUntil,
		QuotaForceCooldownUntil: modelQuotaForceUntil,
		ForceCooldownEvaluated:  true,
		NonQuotaCooldownUntil:   modelNonQuotaUntil,
	}
	if state.credentialQuotaShadow != nil {
		record.IndependentState = state.credentialQuotaShadow.cloneShallow()
		if u := effectiveModelForceUntil(state.credentialQuotaShadow); u.After(now) {
			record.IndependentForceCooldownUntil = u
		}
		if state.credentialQuotaShadow.quotaForceCooldownUntil.After(now) {
			record.IndependentQuotaForceCooldownUntil = state.credentialQuotaShadow.quotaForceCooldownUntil
		}
		if state.credentialQuotaShadow.nonQuotaCooldownUntil.After(now) {
			record.IndependentNonQuotaCooldownUntil = state.credentialQuotaShadow.nonQuotaCooldownUntil
		}
		if state.credentialQuotaShadow.Quota.Exceeded && state.credentialQuotaShadow.Quota.NextRecoverAt.After(now) &&
			!state.credentialQuotaShadow.NextRetryAfter.After(state.credentialQuotaShadow.Quota.NextRecoverAt) {
			record.IndependentCooldownSignals = sanitizePersistedClaudeCooldownSignals(state.credentialQuotaShadow.Quota.cooldownSignals)
		}
	}
	record.CredentialQuotaPropagated = state.credentialQuotaPropagated
	scoped := state
	if state.credentialQuotaPropagated && state.credentialQuotaShadow != nil {
		scoped = state.credentialQuotaShadow
	}
	record.ScopedQuotaPropagated = scoped.scopedQuotaPropagated
	record.ScopedQuotaIndependentState = scoped.scopedQuotaShadow.cloneShallow()
	if shadow := scoped.scopedQuotaShadow; shadow != nil {
		if until := effectiveModelForceUntil(shadow); until.After(now) {
			record.ScopedQuotaIndependentForceUntil = until
		}
		record.ScopedQuotaIndependentQuotaForceUntil = shadow.quotaForceCooldownUntil
		record.ScopedQuotaIndependentNonQuotaUntil = shadow.nonQuotaCooldownUntil
		record.ScopedQuotaIndependentSignals = sanitizePersistedClaudeCooldownSignals(shadow.Quota.cooldownSignals)
	}
	return record, true
}

func cooldownReason(statusMessage string, quota QuotaState, lastErr *Error) string {
	if reason := strings.TrimSpace(quota.Reason); reason != "" {
		return reason
	}
	if statusMessage = strings.TrimSpace(statusMessage); statusMessage != "" {
		return statusMessage
	}
	if lastErr != nil {
		if code := strings.TrimSpace(lastErr.Code); code != "" {
			return code
		}
		if message := strings.TrimSpace(lastErr.Message); message != "" {
			return message
		}
	}
	return ""
}

// nextExecutionSeq stamps a new upstream dispatch with the next monotonic
// execution sequence. The stamp travels on the execution's Result so that
// MarkResult can order the execution against cooldowns recorded meanwhile.
// NextExecutionSeq allocates the next monotonic execution sequence number for
// ordering concurrent MarkResult updates against newly recorded cooldowns.
func (m *Manager) NextExecutionSeq() uint64 {
	return m.nextExecutionSeq()
}

func (m *Manager) nextExecutionSeq() uint64 {
	if m == nil {
		return 0
	}
	return m.executionSeq.Add(1)
}

// currentExecutionSeq returns the latest execution sequence issued so far.
// Capturing it as the cooldown watermark when a cooldown is recorded marks
// every execution dispatched up to that moment as predating the cooldown.
func (m *Manager) currentExecutionSeq() uint64 {
	if m == nil {
		return 0
	}
	return m.executionSeq.Load()
}

// isStaleExecutionResult reports whether an execution result originated from a superseded
// credential version or an earlier registration epoch than the currently active auth.
func isStaleExecutionResult(result Result, current *Auth) bool {
	if current == nil {
		return false
	}
	staleEpoch := result.RegistrationEpoch > 0 && result.RegistrationEpoch < current.RegistrationEpoch
	if staleEpoch {
		return true
	}
	if result.AccessToken != "" && authMatchesTokenPartition(current, result.AccessToken) {
		return false
	}
	if isTOSViolationResultError(result.Error) {
		// A service-scoped TOS suspension applies to the account, so a plain OAuth
		// access-token refresh (same credential, refresh token retained) must not
		// discard the terminal signal. A genuine credential replacement under the
		// same auth ID is a different account: an old request's TOS result must
		// not disable the fresh credential. The access token alone cannot tell
		// these apart, so exempt only when the refresh credential proves the same
		// account (identical refresh token) or the result carries no account hint
		// (internal/token-less paths where replacement cannot be detected).
		if result.AccessToken == "" {
			return false
		}
		refreshNow := authRefreshToken(current)
		if refreshNow != "" && result.RefreshToken != "" && refreshNow == result.RefreshToken {
			return false
		}
	}
	staleVersion := result.CredentialVersion < current.CredentialVersion && (result.CredentialVersion > 0 || current.CredentialVersion > 1)
	return staleVersion
}

// MarkResult records an execution result and notifies hooks.
func (m *Manager) MarkResult(ctx context.Context, result Result) {
	if result.AuthID == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if result.Provider == "antigravity" {
		result.Model = cliproxyexecutor.UpstreamAttemptModel(ctx, result.Model)
	}
	if m != nil {
		if policy := m.ResultPolicy(); policy != nil {
			result = policy.ApplyResultPolicy(ctx, result)
			if result.AuthID == "" {
				return
			}
		}
	}
	modelKey := canonicalModelKey(result.Model)

	var authSnapshot *Auth
	cooldownStateChanged := false
	scheduleUnauthorizedRefresh := false
	now := time.Now()
	// A 401 condemns the access token, except one a -and-cooldown rule matched: that only cools the credential.
	rejectsToken := !result.Success && (result.Error == nil || result.Error.Code != ErrorCodeForceCooldown) && !shouldSkipCredentialCooldown(result.Error) && (statusCodeFromResult(result.Error) == http.StatusUnauthorized || isUnauthorizedError(result.Error))

	releaseMutation := m.lockAuthMutation(result.AuthID)
	defer releaseMutation()
	m.mu.Lock()
	if auth, ok := m.auths[result.AuthID]; ok && auth != nil {
		if isStaleExecutionResult(result, auth) {
			staleEpoch := result.RegistrationEpoch > 0 && result.RegistrationEpoch < auth.RegistrationEpoch
			if !staleEpoch && rejectsToken && result.AccessToken != "" {
				if recordRejectedTokenOnAuth(auth, result.AccessToken) {
					auth.Generation++
					auth.UpdatedAt = now
				}
			}
			m.mu.Unlock()
			releaseMutation()
			m.hook.OnResult(ctx, result)
			m.publishErrorEvent(result, nil)
			return
		}
		if modelKey == "" && strings.TrimSpace(result.RouteModel) != "" {
			if m != nil {
				modelKey = m.selectionModelKeyForAuth(auth, result.RouteModel)
			}
			if modelKey == "" {
				modelKey = canonicalModelKey(result.RouteModel)
			}
		}
		now = time.Now()
		responseHeaders := internallogging.GetResponseHeaders(ctx)
		var cooldownRecordsBefore []CooldownStateRecord
		trackCooldownState := m.cooldownStore != nil
		if trackCooldownState {
			cooldownRecordsBefore = m.cooldownStateRecordsForAuthLocked(auth, now)
		}
		if m != nil {
			_ = m.migrateAuthAliasModelStatesLocked(auth, nil, now, false)
			if modelKey != "" {
				modelKey = m.resolveMarkResultModelKeyLocked(auth, modelKey, result.RouteModel)
			}
		}
		modelState := existingModelState(auth, modelKey)
		if normalizeModelQuotaLayers(auth, modelState, now) {
			updateAggregatedAvailability(auth, now)
		}
		auth.recordRecentRequest(now, result.Success)
		if result.Success {
			auth.Success++
		} else {
			auth.Failed++
		}
		wasTerminalUnauthorized := hasUnauthorizedAuthFailure(auth)

		// retainModelCooldown pins a still-live model cooldown against a success
		// from an execution dispatched before that cooldown was recorded. Such a
		// success predates the failure and proves nothing about recovery, so the
		// state transition it normally causes is skipped entirely.
		retainModelCooldown := modelKey != "" && !wasTerminalUnauthorized && result.ExecutionSeq > 0
		if retainModelCooldown {
			if existing := existingModelState(auth, modelKey); existing != nil && isModelStateActiveCooldown(existing, now) &&
				existing.cooldownRecordedSeq > 0 && result.ExecutionSeq <= existing.cooldownRecordedSeq {
				modelState = existing
			} else {
				retainModelCooldown = false
			}
		}
		retainAuthCooldown := (modelKey == "" || isModelStateActiveCooldown(auth.independentCooldown, now)) && !wasTerminalUnauthorized && result.ExecutionSeq > 0 &&
			isAuthActiveCooldown(auth, now) && auth.cooldownRecordedSeq > 0 && result.ExecutionSeq <= auth.cooldownRecordedSeq
		retainCredentialRetry := !wasTerminalUnauthorized && result.ExecutionSeq > 0 &&
			!auth.credentialRetryAfter.IsZero() && auth.credentialRetryAfter.After(now) &&
			auth.cooldownRecordedSeq > 0 && result.ExecutionSeq <= auth.cooldownRecordedSeq

		retainScopedQuota := modelState != nil && modelState.scopedQuotaPropagated && modelState.Quota.NextRecoverAt.After(now)

		if result.Success {
			if !wasTerminalUnauthorized && !retainAuthCooldown && !retainModelCooldown && !retainScopedQuota && !retainCredentialRetry &&
				!(auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now)) && !effectiveAuthForceUntil(auth).After(now) {
				auth.independentCooldown = &ModelState{Status: StatusActive, UpdatedAt: now, forceCooldownSeeded: true}
			}
			if result.CompletedResponse && !result.SkipQuotaObservation && !wasTerminalUnauthorized && !(auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now)) && !retainScopedQuota && !retainCredentialRetry && !retainModelCooldown && !retainAuthCooldown {
				auth.InferenceVerified = true
				auth.LastInferenceVerifiedAt = now
			}
			if wasTerminalUnauthorized {
				if modelKey != "" {
					state := ensureModelState(auth, modelKey)
					modelState = state
					resetModelState(state, now)
				}
			} else if auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now) {
				// Retain active credential-scoped cooldown
			} else if retainScopedQuota {
				// Retain the provider-reported model quota until it recovers.
			} else if retainModelCooldown || retainAuthCooldown || retainCredentialRetry {
				// Retain a cooldown recorded after this execution was dispatched.
			} else if modelKey != "" {
				state := ensureModelState(auth, modelKey)
				modelState = state
				resetModelState(state, now)
				auth.credentialRetryAfter = time.Time{}
				updateAggregatedAvailability(auth, now)
				if !auth.Disabled && !hasModelError(auth, now) && !effectiveAuthCooldownUntil(auth).After(now) {
					auth.LastError = nil
					auth.StatusMessage = ""
					auth.Status = StatusActive
				}
			} else if !auth.Disabled {
				clearAuthStateOnSuccess(auth, now)
			}
		} else if !m.isPreResetQuotaFailureLocked(result, modelKey) {
			if modelKey != "" {
				if auth.Disabled || auth.Status == StatusDisabled {
					// A disabled credential (operator action or terminal TOS) is
					// terminal until explicitly re-enabled; a late failure must not
					// overwrite its disabled state or arm a new cooldown.
				} else if !shouldSkipCredentialCooldown(result.Error) {
					disableCooling := m.cooldownDisabledForAuth(auth)
					if result.Error != nil && result.Error.Code == ErrorCodeForceCooldown {
						disableCooling = false
					}
					state := ensureModelState(auth, modelKey)
					normalizeModelQuotaLayers(auth, state, now)
					modelState = state
					// Two-layer handling (issue #6690): while an auth-level
					// credential quota wraps the credential, a non-credential
					// own failure is applied to the independent layer (shadow)
					// so its deadline/backoff never inherit the propagated
					// quota; the outer fields are then composed from both
					// layers. Credential-scoped failures refresh the outer
					// layer and are never independent.
					outerState := state
					savedOuterState := state.cloneShallow()
					ownLayerPending := !result.CredentialScope &&
						auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now)
					if ownLayerPending {
						if state.credentialQuotaShadow != nil {
							preservedSeq := state.credentialQuotaShadow.cooldownRecordedSeq
							*state = *state.credentialQuotaShadow.cloneShallow()
							state.cooldownRecordedSeq = preservedSeq
						} else {
							fresh := &ModelState{}
							*state = *fresh
						}
					}
					var savedScopedState *ModelState
					independentFailure := statusCodeFromResult(result.Error) != http.StatusTooManyRequests || isTransientRateLimitResultError(result.Error)
					if !result.CredentialScope && (state.scopedQuotaPropagated ||
						(independentFailure && (state.scopedQuotaShadow != nil || (state.Quota.Exceeded && state.Quota.Reason == "quota")))) {
						if state.Quota.NextRecoverAt.After(now) {
							savedScopedState = state.cloneShallow()
						}
						if state.scopedQuotaShadow != nil {
							*state = *state.scopedQuotaShadow.cloneShallow()
						} else if !state.scopedQuotaPropagated && effectiveModelForceUntil(state).After(now) {
							// An explicit force bound is independent of an ordinary
							// quota window, even if both were recorded by one result.
							state.NextRetryAfter = effectiveModelForceUntil(state)
							applyCooldownFields(&state.Quota, QuotaState{})
						} else {
							*state = ModelState{}
						}
						if savedScopedState != nil {
							savedScopedState.forceCooldownUntil = time.Time{}
							savedScopedState.forceCooldownSeeded = true
						}
					}
					seedInitialForceCooldownDeadlines(auth)
					seedModelForceCooldownDeadline(state)
					state.Unavailable = true
					state.Status = StatusError
					state.UpdatedAt = now
					prevModelRetryAfter := state.NextRetryAfter
					prevModelForceUntil := state.forceCooldownUntil
					prevModelQuota := cooldownFieldsOf(state.Quota)
					prevModelQuotaSignals := maps.Clone(state.Quota.cooldownSignals)
					var ownModelForceDeadline time.Time
					var ownModelNonQuotaDeadline time.Time
					isForceResult := isForceCooldownError(result.Error)
					appliedCredQuotaWrap := false
					if result.Error != nil {
						state.LastError = cloneError(result.Error)
						state.StatusMessage = result.Error.Message
						if !wasTerminalUnauthorized {
							auth.LastError = cloneError(result.Error)
							auth.StatusMessage = result.Error.Message
						}
					}

					statusCode := statusCodeFromResult(result.Error)
					if isModelSupportResultError(result.Error) {
						if disableCooling {
							state.NextRetryAfter = time.Time{}
						} else if result.RetryAfter != nil && *result.RetryAfter > 0 {
							state.NextRetryAfter = now.Add(*result.RetryAfter)
						} else {
							next := now.Add(12 * time.Hour)
							state.NextRetryAfter = next
						}
						ownModelNonQuotaDeadline = state.NextRetryAfter
					} else if isCloudflareChallengeResultError(result.Error) {
						next, backoffLevel := nextCloudflareCooldown(state.Quota.BackoffLevel, disableCooling, now)
						state.NextRetryAfter = next
						ownModelNonQuotaDeadline = next
						state.StatusMessage = "cloudflare challenge"
						if auth.LastError != nil && !wasTerminalUnauthorized {
							auth.StatusMessage = "cloudflare challenge"
						}
						applyCooldownFields(&state.Quota, QuotaState{
							Exceeded:      true,
							Reason:        "cloudflare challenge",
							NextRecoverAt: next,
							BackoffLevel:  backoffLevel,
						})
					} else if isInvalidGrantResultError(result.Error) || isCreditBalanceTooLowResultError(result.Error) {
						if disableCooling {
							state.NextRetryAfter = time.Time{}
						} else {
							state.NextRetryAfter = now.Add(30 * time.Minute)
						}
						ownModelNonQuotaDeadline = state.NextRetryAfter
					} else if !result.CredentialScope && isTransientRateLimitResultError(result.Error) && statusCode == http.StatusTooManyRequests {
						// Transient per-turn capacity rejection (issue 7017): cool
						// only the reporting model until the provider retry hint.
						// The credential quota is not exhausted, so no quota state
						// is written and the quota exponential backoff never moves;
						// a still-live genuine quota deadline is left untouched and
						// keeps blocking through the generic retry branches below.
						next := transientRateLimitRetryAfter(now, result.RetryAfter, disableCooling)
						ownModelNonQuotaDeadline = next
						if isForceResult {
							ownModelForceDeadline = next
						}
						if !next.IsZero() && state.Quota.Exceeded && state.Quota.NextRecoverAt.After(next) {
							next = state.Quota.NextRecoverAt
						}
						if next.IsZero() && state.Quota.Exceeded && state.Quota.NextRecoverAt.After(now) {
							next = state.Quota.NextRecoverAt
						}
						state.NextRetryAfter = next
						state.Unavailable = !next.IsZero()
					} else if isTOSViolationResultError(result.Error) {
						// Terminal TOS suspension: keep the model state blocked
						// without a retry deadline; the auth-level terminal state
						// is finalized below.
						state.NextRetryAfter = time.Time{}
						state.Unavailable = true
						state.Status = StatusDisabled
						state.StatusMessage = "tos_violation"
					} else {
						switch statusCode {
						case 401, 402, 403:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(30 * time.Minute)
								state.NextRetryAfter = next
							}
						case 404:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else if result.RetryAfter != nil && *result.RetryAfter > 0 {
								state.NextRetryAfter = now.Add(*result.RetryAfter)
							} else {
								next := now.Add(12 * time.Hour)
								state.NextRetryAfter = next
							}
						case 429:
							prevStateQuotaActive := state.Quota.Exceeded && state.Quota.NextRecoverAt.After(now)
							prevStateRecoverAt := state.Quota.NextRecoverAt
							prevStateCooldownSignals := maps.Clone(state.Quota.cooldownSignals)
							var next time.Time
							var credentialNext time.Time
							keptPriorStateQuota := false
							backoffLevel := state.Quota.BackoffLevel
							if result.CredentialScope {
								backoffLevel = 0
								if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" {
									backoffLevel = auth.Quota.BackoffLevel
								}
							}
							if !disableCooling {
								reusedOpenWindow := false
								var baseBackoffBefore int
								if result.RetryAfter != nil {
									cooldown := *result.RetryAfter
									if cooldown < minQuotaCooldownFloor {
										cooldown = minQuotaCooldownFloor
									}
									next = now.Add(cooldown).Round(0)
								} else {
									quotaForFailure := state.Quota
									if result.CredentialScope {
										if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" {
											quotaForFailure = auth.Quota
										} else {
											quotaForFailure.NextRecoverAt = time.Time{}
											quotaForFailure.BackoffLevel = 0
										}
									}
									reusedOpenWindow = quotaForFailure.NextRecoverAt.After(now)
									baseBackoffBefore = quotaForFailure.BackoffLevel
									next, backoffLevel = quotaCooldownAfterFailure(quotaForFailure, now)
								}
								credentialNext = next
								if isForceResult {
									if result.RetryAfter != nil || !reusedOpenWindow {
										ownModelForceDeadline = credentialNext
									} else if result.CredentialScope {
										if auth.credQuotaForceUntil.After(now) {
											ownModelForceDeadline = auth.credQuotaForceUntil
										} else {
											ownModelForceDeadline, _ = quotaCooldownAfterFailure(QuotaState{BackoffLevel: baseBackoffBefore}, now)
										}
									} else if prevModelForceUntil.After(now) {
										ownModelForceDeadline = prevModelForceUntil
									} else {
										ownModelForceDeadline, _ = quotaCooldownAfterFailure(QuotaState{BackoffLevel: baseBackoffBefore}, now)
									}
								}
								if prevStateQuotaActive && (prevStateRecoverAt.After(next) || (!result.CredentialScope && result.RetryAfter == nil)) {
									next = prevStateRecoverAt
									keptPriorStateQuota = true
								} else if result.CredentialScope && result.RetryAfter == nil && auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now) {
									// quotaCooldownAfterFailure reused the open credential_quota window.
									keptPriorStateQuota = prevStateQuotaActive && prevStateRecoverAt.Equal(next)
								}
							}
							var cooldownSignals map[string]string
							if !disableCooling {
								if keptPriorStateQuota {
									cooldownSignals = prevStateCooldownSignals
								} else {
									cooldownSignals = rejectedClaudeSignalsForCooldown(result.Provider, responseHeaders)
									if len(cooldownSignals) == 0 && prevStateQuotaActive && prevStateRecoverAt.Equal(next) {
										cooldownSignals = prevStateCooldownSignals
									}
								}
							}
							state.NextRetryAfter = next
							applyCooldownFields(&state.Quota, QuotaState{
								Exceeded:        true,
								Reason:          "quota",
								NextRecoverAt:   next,
								BackoffLevel:    backoffLevel,
								cooldownSignals: cooldownSignals,
							})
							// Non-credential own failures re-derive their independent layer
							// below, so a stale shadow is dropped. A credential-scoped
							// report must not erase an existing independent layer: it only
							// refreshes the propagated outer layer.
							if !result.CredentialScope {
								state.credentialQuotaShadow = nil
							} else if !disableCooling && !state.credentialQuotaPropagated && savedOuterState.Quota.Reason != "credential_quota" {
								// First wrap of the reporting model: preserve any live prior
								// own block (including an ordinary model-scoped 429, which
								// quota restoration does not supersede). An outer
								// credential_quota label instead proves a prior wrap whose
								// state must never be snapshotted as independence.
								priorIndependent := prevModelRetryAfter.After(now) && !isDevinQuotaExhaustionMessage(devinStateErrorText(savedOuterState))
								if priorIndependent {
									preserved := savedOuterState.cloneShallow()
									preserved.NextRetryAfter = prevModelRetryAfter
									preserved.Unavailable = true
									preserved.Status = StatusError
									state.credentialQuotaShadow = preserved
								} else {
									state.credentialQuotaShadow = nil
								}
							}
							if result.CredentialScope && !disableCooling {
								appliedCredQuotaWrap = true
								prevAuthCredActive := auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now)
								prevAuthCredRecoverAt := auth.Quota.NextRecoverAt
								prevAuthCredSignals := maps.Clone(auth.Quota.cooldownSignals)
								var credCooldownSignals map[string]string
								if prevAuthCredActive && (prevAuthCredRecoverAt.After(credentialNext) || result.RetryAfter == nil) {
									credCooldownSignals = prevAuthCredSignals
								} else {
									credCooldownSignals = rejectedClaudeSignalsForCooldown(result.Provider, responseHeaders)
									if len(credCooldownSignals) == 0 && prevAuthCredActive && prevAuthCredRecoverAt.Equal(credentialNext) {
										credCooldownSignals = prevAuthCredSignals
									}
								}
								propagateCredentialQuotaToModelStatesLocked(auth, state, credentialNext, backoffLevel, credCooldownSignals, prevAuthCredActive, prevAuthCredRecoverAt, now)
								if !wasTerminalUnauthorized {
									auth.Unavailable = true
									authNext := credentialNext
									if prevAuthCredActive && prevAuthCredRecoverAt.After(authNext) {
										authNext = prevAuthCredRecoverAt
									}
									auth.Quota.Exceeded = true
									auth.Quota.Reason = "credential_quota"
									auth.Quota.NextRecoverAt = authNext
									auth.Quota.BackoffLevel = backoffLevel
									auth.Quota.cooldownSignals = maps.Clone(credCooldownSignals)
									auth.Quota.cooldownEvaluated = true
									auth.NextRetryAfter = authNext
									if authNext.After(state.Quota.NextRecoverAt) || (prevAuthCredActive && result.RetryAfter == nil) {
										state.Quota.NextRecoverAt = authNext
										state.Quota.cooldownSignals = maps.Clone(credCooldownSignals)
										if state.NextRetryAfter.Before(authNext) {
											state.NextRetryAfter = authNext
										}
									}
									if isForceResult && ownModelForceDeadline.After(auth.credQuotaForceUntil) {
										auth.credQuotaForceUntil = ownModelForceDeadline
									}
									auth.forceCooldownSeeded = true
									// The reporting model is now part of the wrap too: mark it
									// so a later credential report from another model cannot
									// snapshot this credential-derived state as independent.
									state.credentialQuotaPropagated = true
								}
							}
						case 408, 500, 502, 503, 504, 520, 521, 522, 523, 524, 525, 526:
							state.NextRetryAfter = recoverableFailureRetryAfterWithResult(now, result.RetryAfter, disableCooling, result.Error)
							state.Unavailable = !state.NextRetryAfter.IsZero()
						default:
							state.NextRetryAfter = recoverableFailureRetryAfter(now, disableCooling)
							state.Unavailable = !state.NextRetryAfter.IsZero()
						}
						if !isQuotaCooldownResultError(result.Error) {
							ownModelNonQuotaDeadline = state.NextRetryAfter
						}
					}

					if disableCooling && state.NextRetryAfter.IsZero() && state.Quota.NextRecoverAt.IsZero() {
						state.Unavailable = false
						state.Quota.Exceeded = false
					}
					if isForceResult && state.NextRetryAfter.IsZero() {
						state.NextRetryAfter = now.Add(transientErrorCooldown)
						state.Unavailable = true
					}
					if isForceResult && !isQuotaCooldownResultError(result.Error) && state.NextRetryAfter.After(ownModelNonQuotaDeadline) {
						ownModelNonQuotaDeadline = state.NextRetryAfter
					}
					if ownModelNonQuotaDeadline.After(state.nonQuotaCooldownUntil) {
						state.nonQuotaCooldownUntil = ownModelNonQuotaDeadline
					}
					if !appliedCredQuotaWrap {
						if result.CredentialScope && auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.credQuotaForceUntil.After(now) && auth.Quota.NextRecoverAt.After(now) {
							lastErr := cloneError(state.LastError)
							statusMsg := state.StatusMessage
							*state = *savedOuterState
							state.LastError = lastErr
							state.StatusMessage = statusMsg
							state.Unavailable = true
							state.Status = StatusError
							if state.NextRetryAfter.Before(auth.credQuotaForceUntil) {
								state.NextRetryAfter = auth.credQuotaForceUntil
							}
							if ownModelNonQuotaDeadline.After(state.nonQuotaCooldownUntil) {
								state.nonQuotaCooldownUntil = ownModelNonQuotaDeadline
							}
							state.UpdatedAt = now
						} else {
							if isForceResult {
								if ownModelForceDeadline.IsZero() {
									ownModelForceDeadline = state.NextRetryAfter
								}
								if ownModelForceDeadline.After(prevModelForceUntil) {
									state.forceCooldownUntil = ownModelForceDeadline
								} else {
									state.forceCooldownUntil = prevModelForceUntil
								}
							} else {
								state.forceCooldownUntil = prevModelForceUntil
							}
							state.forceCooldownSeeded = true
							if isForceResult && isQuotaCooldownResultError(result.Error) && ownModelForceDeadline.After(state.quotaForceCooldownUntil) {
								state.quotaForceCooldownUntil = ownModelForceDeadline
							}
							if state.forceCooldownUntil.After(now) {
								if state.NextRetryAfter.Before(state.forceCooldownUntil) {
									state.NextRetryAfter = state.forceCooldownUntil
								}
								state.Unavailable = true
								state.Status = StatusError
								if (!state.Quota.Exceeded || state.Quota.NextRecoverAt.IsZero()) && prevModelQuota.Exceeded && prevModelQuota.NextRecoverAt.After(now) {
									restored := prevModelQuota
									if disableCooling && restored.NextRecoverAt.After(state.forceCooldownUntil) {
										restored.NextRecoverAt = state.forceCooldownUntil
									}
									restored.cooldownSignals = prevModelQuotaSignals
									applyCooldownFields(&state.Quota, restored)
								}
							}
						}
					}
					// A later failure only extends a still-live cooldown; it never
					// shortens one. A deliberate zero write (disableCooling) still
					// clears the deadline.
					if !state.NextRetryAfter.IsZero() && prevModelRetryAfter.After(state.NextRetryAfter) && prevModelRetryAfter.After(now) {
						state.NextRetryAfter = prevModelRetryAfter
					}
					// Capture the dispatch watermark before composing layers so the
					// independent shadow carries this failure's stale-success
					// protection (issue #6690).
					cooldownSeq := m.currentExecutionSeq()
					state.cooldownRecordedSeq = cooldownSeq
					if savedScopedState != nil {
						independent := state.cloneShallow()
						savedScopedState.scopedQuotaShadow = independent
						savedScopedState.NextRetryAfter = savedScopedState.Quota.NextRecoverAt
						if independent.NextRetryAfter.After(savedScopedState.NextRetryAfter) {
							savedScopedState.NextRetryAfter = independent.NextRetryAfter
						}
						savedScopedState.cooldownRecordedSeq = cooldownSeq
						*state = *savedScopedState
					}
					if ownLayerPending {
						// Compose the outer presentation from the saved propagated
						// layer and the independent layer just updated in place.
						independent := state.cloneShallow()
						outer := savedOuterState.cloneShallow()
						if isModelStateActiveCooldown(independent, now) {
							outer.credentialQuotaShadow = independent
						} else {
							outer.credentialQuotaShadow = nil
						}
						outer.cooldownRecordedSeq = cooldownSeq
						// A composed outer state is wrapped by definition; stamp it so
						// a later credential report cannot mistake it for unwrapped.
						outer.credentialQuotaPropagated = true
						outer.Unavailable = true
						outer.Status = StatusError
						outer.UpdatedAt = now
						outer.LastError = cloneError(state.LastError)
						outer.StatusMessage = state.StatusMessage
						if state.NextRetryAfter.After(outer.NextRetryAfter) {
							outer.NextRetryAfter = state.NextRetryAfter
						}
						if auth.Quota.NextRecoverAt.After(outer.NextRetryAfter) {
							outer.NextRetryAfter = auth.Quota.NextRecoverAt
						}
						if !(outer.Quota.Exceeded && outer.Quota.Reason == "credential_quota") {
							applyCooldownFields(&outer.Quota, QuotaState{
								Exceeded:        true,
								Reason:          "credential_quota",
								NextRecoverAt:   auth.Quota.NextRecoverAt,
								BackoffLevel:    auth.Quota.BackoffLevel,
								cooldownSignals: auth.Quota.cooldownSignals,
							})
						}
						*outerState = *outer
						state = outerState
						modelState = state
					}
					if result.CredentialScope && !disableCooling && statusCodeFromResult(result.Error) == 429 {
						auth.cooldownRecordedSeq = cooldownSeq
						for _, otherState := range auth.ModelStates {
							if otherState != nil && otherState != state {
								otherState.cooldownRecordedSeq = cooldownSeq
							}
						}
					}
					if isTOSViolationResultError(result.Error) {
						auth.StatusMessage = "tos_violation"
						auth.Status = StatusDisabled
						auth.Disabled = true
						auth.Unavailable = true
						auth.NextRetryAfter = time.Time{}
						persistTOSAppealMetadata(auth, result.Error)
					} else {
						auth.Status = StatusError
					}
					updateAggregatedAvailability(auth, now)
				}
			} else {
				disableCooling := m.cooldownDisabledForAuth(auth)
				if result.Error != nil && result.Error.Code == ErrorCodeForceCooldown {
					disableCooling = false
				}
				if auth.Disabled || auth.Status == StatusDisabled {
					// A disabled credential (operator action or terminal TOS) is
					// terminal until explicitly re-enabled; a late auth-level failure
					// must not overwrite its disabled state or arm a new cooldown.
				} else if !wasTerminalUnauthorized {
					prevAuthCredActive := auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now)
					prevAuthCredRecoverAt := auth.Quota.NextRecoverAt
					prevAuthCredSignals := maps.Clone(auth.Quota.cooldownSignals)
					isolatedModelAgg := result.CredentialScope && len(auth.ModelStates) > 0 && auth.Quota.Reason != "credential_quota"
					var savedModelAggQuota QuotaState
					if isolatedModelAgg {
						savedModelAggQuota = auth.Quota.Clone()
						authOwnRetry := auth.credentialRetryAfter
						if activeAuthForce := effectiveAuthForceUntil(auth); activeAuthForce.After(authOwnRetry) {
							authOwnRetry = activeAuthForce
						}
						auth.NextRetryAfter = authOwnRetry
						auth.Quota = QuotaState{
							ObservedAt: savedModelAggQuota.ObservedAt,
							Signals:    maps.Clone(savedModelAggQuota.Signals),
						}
					}
					prevAuthQuotaActive := auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(now)
					prevAuthRecoverAt := auth.Quota.NextRecoverAt
					prevAuthCooldownSignals := maps.Clone(auth.Quota.cooldownSignals)
					keptPriorAuthQuota := false
					if !disableCooling && statusCodeFromResult(result.Error) == 429 && prevAuthQuotaActive {
						if result.RetryAfter == nil {
							keptPriorAuthQuota = true
						} else {
							cooldown := *result.RetryAfter
							if cooldown < minQuotaCooldownFloor {
								cooldown = minQuotaCooldownFloor
							}
							if prevAuthRecoverAt.After(now.Add(cooldown).Round(0)) {
								keptPriorAuthQuota = true
							}
						}
					}
					ownAuthForceDeadline, ownAuthRetryDeadline := applyAuthFailureState(auth, result.Error, result.RetryAfter, now, disableCooling)
					cooldownSeq := m.currentExecutionSeq()
					isQuota429 := !disableCooling &&
						statusCodeFromResult(result.Error) == 429 &&
						!isTransientRateLimitResultError(result.Error) &&
						!isCloudflareChallengeResultError(result.Error) &&
						!isCreditBalanceTooLowResultError(result.Error) &&
						!isInvalidGrantResultError(result.Error) &&
						auth.Quota.Exceeded
					if isQuota429 {
						auth.Quota.cooldownEvaluated = true
						if keptPriorAuthQuota {
							auth.Quota.cooldownSignals = prevAuthCooldownSignals
						} else if rejectedSignals := rejectedClaudeSignalsForCooldown(result.Provider, responseHeaders); len(rejectedSignals) > 0 {
							auth.Quota.cooldownSignals = rejectedSignals
						} else if prevAuthQuotaActive && auth.Quota.NextRecoverAt.Equal(prevAuthRecoverAt) {
							auth.Quota.cooldownSignals = prevAuthCooldownSignals
						} else {
							auth.Quota.cooldownSignals = nil
						}
						if result.CredentialScope && !shouldSkipCredentialCooldown(result.Error) {
							auth.Quota.Reason = "credential_quota"
							if isForceCooldownError(result.Error) && ownAuthForceDeadline.After(auth.credQuotaForceUntil) {
								auth.credQuotaForceUntil = ownAuthForceDeadline
								auth.forceCooldownSeeded = true
							}
							credSignals := auth.Quota.cooldownSignals
							if prevAuthCredActive && (prevAuthCredRecoverAt.After(auth.Quota.NextRecoverAt) || result.RetryAfter == nil) {
								credSignals = prevAuthCredSignals
							}
							propagateCredentialQuotaToModelStatesLocked(auth, nil, auth.Quota.NextRecoverAt, auth.Quota.BackoffLevel, credSignals, prevAuthCredActive, prevAuthCredRecoverAt, now)
							for _, otherState := range auth.ModelStates {
								if otherState != nil {
									otherState.cooldownRecordedSeq = cooldownSeq
								}
							}
						}
					} else {
						if result.CredentialScope && !disableCooling && !shouldSkipCredentialCooldown(result.Error) && !ownAuthRetryDeadline.IsZero() && ownAuthRetryDeadline.After(now) {
							if ownAuthRetryDeadline.After(auth.credentialRetryAfter) {
								auth.credentialRetryAfter = ownAuthRetryDeadline
							}
						}
						if isolatedModelAgg && !isCloudflareChallengeResultError(result.Error) {
							savedModelAggQuota.Signals = mergeQuotaObservation(auth.Quota, savedModelAggQuota).Signals
							applyCooldownFields(&auth.Quota, savedModelAggQuota)
							updateAggregatedAvailability(auth, now)
						}
					}
					auth.cooldownRecordedSeq = cooldownSeq
					if auth.independentCooldown != nil {
						auth.independentCooldown.cooldownRecordedSeq = auth.cooldownRecordedSeq
					}
				}
			}
		}

		if rejectsToken {
			tokenToReject := result.AccessToken
			if tokenToReject == "" {
				tokenToReject = authAccessTokenForRequest(ctx, auth, result.Options.Headers)
			}
			_ = recordRejectedTokenOnAuth(auth, tokenToReject)
		}
		if wasTerminalUnauthorized {
			auth.Unavailable = true
			auth.Status = StatusError
			auth.NextRefreshAfter = time.Time{}
			auth.NextRetryAfter = time.Time{}
		} else if auth.Status == StatusDisabled {
			// A terminal TOS_VIOLATION already deactivated the credential inside
			// applyAuthFailureState; preserve that state instead of overwriting
			// it with a generic error status.
		} else if rejectsToken && authHasRefreshCredential(auth) {
			if auth.NextRefreshAfter.IsZero() {
				if !auth.LastRefreshedAt.IsZero() && now.Sub(auth.LastRefreshedAt) < refreshFailureBackoff {
					auth.NextRefreshAfter = now.Add(refreshFailureBackoff)
				} else {
					auth.NextRefreshAfter = now
				}
			}
			scheduleUnauthorizedRefresh = true
		}

		auth.Generation++
		auth.UpdatedAt = now

		// These providers' routing observations come from usage probes. Adapter
		// responses cannot replace them, including adapters without passive headers.
		quotaProvider := QuotaRecoveryProviderForAuth(auth)
		resetState := m.quotaResetStates[result.AuthID]
		preResetObservation := result.ExecutionSeq != 0 && resetState != nil && result.ExecutionSeq <= resetState.quotaObservationSeq
		if !result.SkipQuotaObservation && !preResetObservation && quotaProvider != quotaRecoveryProviderZai && quotaProvider != quotaRecoveryProviderOpenCodeGo {
			auth.Quota.ObserveResponseHeadersForProvider(result.Provider, responseHeaders, now)
			if modelState != nil {
				modelState.Quota.ObserveResponseHeadersForProvider(result.Provider, responseHeaders, now)
			}
		}

		_ = m.persistLocked(ctx, auth)
		authSnapshot = auth.Clone()
		if trackCooldownState {
			cooldownRecordsAfter := m.cooldownStateRecordsForAuthLocked(auth, now)
			cooldownStateChanged = !cooldownStateRecordsEqual(cooldownRecordsBefore, cooldownRecordsAfter)
		}
	}
	m.mu.Unlock()
	releaseMutation()
	if m.scheduler != nil && authSnapshot != nil {
		credentialScopedResult := result.CredentialScope || scheduleUnauthorizedRefresh
		var targetModels []string
		if !credentialScopedResult && modelKey != "" {
			targetModels = append(targetModels, modelKey)
			if routeKey := canonicalModelKey(result.RouteModel); routeKey != "" && !sameCanonicalModelKey(routeKey, modelKey) {
				targetModels = append(targetModels, routeKey)
			}
		}
		m.scheduler.upsertAuthResult(authSnapshot, targetModels, credentialScopedResult)
	}
	if authSnapshot != nil && cooldownStateChanged {
		m.persistCooldownStates(context.Background())
	}
	// A newly exhausted Devin credential must join the refresh schedule so
	// quota revalidation can detect early restoration (issue #6690).
	if authSnapshot != nil {
		if _, due := devinQuotaRevalidateAt(authSnapshot, now); due {
			m.queueRefreshReschedule(result.AuthID)
		} else if scheduleUnauthorizedRefresh {
			m.queueRefreshReschedule(result.AuthID)
		}
	}

	supportedModels, regEpoch := registry.GetGlobalRegistry().GetModelsAndEpochForClient(result.AuthID)
	reg := registry.GetGlobalRegistry()
	projections := make([]registry.ClientModelProjection, 0, len(supportedModels))
	for _, sm := range supportedModels {
		if sm == nil || strings.TrimSpace(sm.ID) == "" {
			continue
		}
		projections = append(projections, m.clientModelProjectionForAuth(authSnapshot, sm.ID, now))
	}
	if authSnapshot != nil && len(projections) > 0 {
		reg.ApplyClientModelProjections(result.AuthID, regEpoch, authSnapshot.Generation, projections)
	}

	m.hook.OnResult(ctx, result)
	m.publishErrorEvent(result, authSnapshot)
	m.updateSessionAffinity(result)
	if !result.SkipQuotaObservation {
		m.scheduleQuotaReserveCheck(result.AuthID)
	}
}

func (m *Manager) updateSessionAffinity(result Result) {
	if m == nil {
		return
	}
	sel := m.Selector()
	if affinity, ok := sel.(interface {
		OnResult(Result)
	}); ok && affinity != nil {
		affinity.OnResult(result)
	}
}

func (m *Manager) recordExecutionResult(ctx context.Context, result Result, auth *Auth, ephemeral bool) {
	if auth != nil {
		if result.CredentialVersion == 0 {
			result.CredentialVersion = auth.CredentialVersion
		}
		if result.RegistrationEpoch == 0 {
			result.RegistrationEpoch = auth.RegistrationEpoch
		}
		if result.AccessToken == "" {
			result.AccessToken = authAccessTokenForRequest(ctx, auth, result.Options.Headers)
		}
		if result.RefreshToken == "" {
			result.RefreshToken = authRefreshToken(auth)
		}
	}
	if !ephemeral {
		m.MarkResult(ctx, result)
		return
	}
	m.reportHomeResult(ctx, result, auth)
}

// reportHomeResult only observes a Home dispatch result and never updates local auth state.
func (m *Manager) reportHomeResult(ctx context.Context, result Result, auth *Auth) {
	if m == nil || result.AuthID == "" {
		return
	}
	var snapshot *Auth
	if auth != nil {
		snapshot = auth.Clone()
	}
	m.hook.OnResult(ctx, result)
	m.publishErrorEvent(result, snapshot)
	m.updateSessionAffinity(result)
}

func (m *Manager) recordAvailabilityNeutralResult(ctx context.Context, result Result) {
	if result.AuthID == "" {
		return
	}

	var authSnapshot *Auth
	releaseMutation := m.lockAuthMutation(result.AuthID)
	defer releaseMutation()
	m.mu.Lock()
	if auth, ok := m.auths[result.AuthID]; ok && auth != nil {
		if isStaleExecutionResult(result, auth) {
			m.mu.Unlock()
			releaseMutation()
			m.hook.OnResult(ctx, result)
			m.publishErrorEvent(result, nil)
			return
		}
		now := time.Now()
		auth.recordRecentRequest(now, result.Success)
		if result.Success {
			auth.Success++
		} else {
			auth.Failed++
		}
		auth.Generation++
		auth.UpdatedAt = now
		_ = m.persistLocked(ctx, auth)
		authSnapshot = auth.Clone()
	}
	m.mu.Unlock()
	releaseMutation()

	m.hook.OnResult(ctx, result)
	m.publishErrorEvent(result, authSnapshot)
}

func existingModelState(auth *Auth, model string) *ModelState {
	model = canonicalModelKey(model)
	if auth == nil || model == "" || len(auth.ModelStates) == 0 {
		return nil
	}
	if state, ok := auth.ModelStates[model]; ok && state != nil {
		return state
	}
	lookupKey := canonicalModelLookupKey(model)
	if lookupKey == "" {
		return nil
	}
	var matched *ModelState
	for stateModel, state := range auth.ModelStates {
		if state == nil || canonicalModelLookupKey(stateModel) != lookupKey {
			continue
		}
		if matched == nil {
			matched = state
		} else {
			matched = mergeModelState(matched.Clone(), state)
		}
	}
	return matched
}

func ensureModelState(auth *Auth, model string) *ModelState {
	model = canonicalModelKey(model)
	if auth == nil || model == "" {
		return nil
	}
	normalizeModelStates(auth)
	if auth.ModelStates == nil {
		auth.ModelStates = make(map[string]*ModelState)
	}
	if state, ok := auth.ModelStates[model]; ok && state != nil {
		return state
	}
	lookupKey := canonicalModelLookupKey(model)
	for stateModel, state := range auth.ModelStates {
		if canonicalModelLookupKey(stateModel) == lookupKey && state != nil {
			return state
		}
	}
	state := &ModelState{Status: StatusActive}
	auth.ModelStates[model] = state
	return state
}

func normalizeModelStates(auth *Auth) bool {
	if auth == nil || len(auth.ModelStates) == 0 {
		return false
	}
	keys := make([]string, 0, len(auth.ModelStates))
	for model := range auth.ModelStates {
		keys = append(keys, model)
	}
	if len(keys) > 1 {
		sort.Strings(keys)
	}
	normalized := make(map[string]*ModelState, len(auth.ModelStates))
	keyByLookup := make(map[string]string, len(auth.ModelStates))
	changed := false
	for _, model := range keys {
		state := auth.ModelStates[model]
		modelKey := canonicalModelKey(model)
		if modelKey == "" {
			modelKey = strings.TrimSpace(model)
		}
		if modelKey != model {
			changed = true
		}
		lookupKey := canonicalModelLookupKey(modelKey)
		if lookupKey == "" {
			lookupKey = strings.ToLower(modelKey)
		}
		if existingKey, ok := keyByLookup[lookupKey]; ok {
			var existingClone *ModelState
			if normalized[existingKey] != nil {
				existingClone = normalized[existingKey].Clone()
			}
			merged := mergeModelState(existingClone, state)
			targetKey := existingKey
			if modelKey == lookupKey && existingKey != lookupKey {
				delete(normalized, existingKey)
				targetKey = modelKey
				keyByLookup[lookupKey] = targetKey
			}
			normalized[targetKey] = merged
			changed = true
			continue
		}
		keyByLookup[lookupKey] = modelKey
		normalized[modelKey] = state
	}
	if changed {
		auth.ModelStates = normalized
	}
	return changed
}

func mergeModelState(target, source *ModelState) *ModelState {
	if target == nil {
		return source
	}
	if source == nil {
		return target
	}
	// Align provenance before merging fields. An unwrapped cooldown belongs
	// beneath a quota layer, including when that layer is itself shared's shadow.
	targetLayer, sourceLayer := modelQuotaLayer(target), modelQuotaLayer(source)
	// Ordinary quota snapshots describe the same layer even when only one
	// has observed a subsequent independent failure.
	if targetLayer < 2 && sourceLayer < 2 && !target.scopedQuotaPropagated && !source.scopedQuotaPropagated &&
		target.Quota.Reason == "quota" && source.Quota.Reason == "quota" {
		targetLayer, sourceLayer = 1, 1
	}
	if targetLayer != sourceLayer {
		wrapped, independent := target, source
		if sourceLayer > targetLayer {
			wrapped, independent = source, target
		}
		merged := wrapped.Clone()
		shadow := &merged.scopedQuotaShadow
		if modelQuotaLayer(wrapped) == 2 {
			shadow = &merged.credentialQuotaShadow
		}
		if !modelStateIsClean(independent) {
			*shadow = mergeModelState(*shadow, independent.Clone())
		}
		if *shadow != nil && (*shadow).NextRetryAfter.After(merged.NextRetryAfter) {
			merged.NextRetryAfter = (*shadow).NextRetryAfter
		}
		merged.Quota = mergeQuotaObservation(merged.Quota, independent.Quota)
		if independent.Status == StatusDisabled {
			merged.Status = StatusDisabled
		}
		*target = *merged
		return target
	}

	preferred := target
	fallback := source
	if source.UpdatedAt.After(target.UpdatedAt) {
		preferred = source
		fallback = target
	}
	merged := ModelState{
		Status:         preferred.Status,
		StatusMessage:  preferred.StatusMessage,
		Unavailable:    target.Unavailable || source.Unavailable,
		NextRetryAfter: target.NextRetryAfter,
		LastError:      cloneError(preferred.LastError),
		Quota: QuotaState{
			Exceeded:          target.Quota.Exceeded || source.Quota.Exceeded,
			Reason:            preferred.Quota.Reason,
			NextRecoverAt:     target.Quota.NextRecoverAt,
			BackoffLevel:      target.Quota.BackoffLevel,
			cooldownEvaluated: target.Quota.cooldownEvaluated || source.Quota.cooldownEvaluated,
		},
		UpdatedAt: target.UpdatedAt,
	}
	merged.Quota = mergeQuotaObservation(merged.Quota, fallback.Quota)
	merged.Quota = mergeQuotaObservation(merged.Quota, preferred.Quota)
	if source.Quota.NextRecoverAt.After(target.Quota.NextRecoverAt) {
		merged.Quota.cooldownSignals = maps.Clone(source.Quota.cooldownSignals)
	} else if target.Quota.NextRecoverAt.After(source.Quota.NextRecoverAt) {
		merged.Quota.cooldownSignals = maps.Clone(target.Quota.cooldownSignals)
	} else if len(preferred.Quota.cooldownSignals) > 0 {
		merged.Quota.cooldownSignals = maps.Clone(preferred.Quota.cooldownSignals)
	} else if len(fallback.Quota.cooldownSignals) > 0 {
		merged.Quota.cooldownSignals = maps.Clone(fallback.Quota.cooldownSignals)
	}
	if source.NextRetryAfter.After(merged.NextRetryAfter) {
		merged.NextRetryAfter = source.NextRetryAfter
	}
	if source.Quota.NextRecoverAt.After(merged.Quota.NextRecoverAt) {
		merged.Quota.NextRecoverAt = source.Quota.NextRecoverAt
	}
	if source.Quota.BackoffLevel > merged.Quota.BackoffLevel {
		merged.Quota.BackoffLevel = source.Quota.BackoffLevel
	}
	if source.UpdatedAt.After(merged.UpdatedAt) {
		merged.UpdatedAt = source.UpdatedAt
	}
	// Keep the strongest cooldown watermark from either side so merging a
	// restored or alias-migrated state into a live auth cannot reopen the
	// stale-success race. Both sides stay zero across a process restart,
	// where restored cooldowns must remain clearable by any success.
	merged.cooldownRecordedSeq = target.cooldownRecordedSeq
	if source.cooldownRecordedSeq > merged.cooldownRecordedSeq {
		merged.cooldownRecordedSeq = source.cooldownRecordedSeq
	}
	merged.forceCooldownUntil = target.forceCooldownUntil
	if source.forceCooldownUntil.After(merged.forceCooldownUntil) {
		merged.forceCooldownUntil = source.forceCooldownUntil
	}
	merged.quotaForceCooldownUntil = target.quotaForceCooldownUntil
	if source.quotaForceCooldownUntil.After(merged.quotaForceCooldownUntil) {
		merged.quotaForceCooldownUntil = source.quotaForceCooldownUntil
	}
	merged.nonQuotaCooldownUntil = target.nonQuotaCooldownUntil
	if source.nonQuotaCooldownUntil.After(merged.nonQuotaCooldownUntil) {
		merged.nonQuotaCooldownUntil = source.nonQuotaCooldownUntil
	}
	merged.forceCooldownSeeded = target.forceCooldownSeeded || source.forceCooldownSeeded
	if merged.StatusMessage == "" {
		merged.StatusMessage = fallback.StatusMessage
	}
	if merged.LastError == nil {
		merged.LastError = cloneError(fallback.LastError)
	}
	if merged.Quota.Reason == "" {
		merged.Quota.Reason = fallback.Quota.Reason
	}
	if target.Status == StatusDisabled || source.Status == StatusDisabled {
		merged.Status = StatusDisabled
	} else if merged.Unavailable || merged.Quota.Exceeded {
		merged.Status = StatusError
	}
	// Keep the independent layer and wrap provenance from either side
	// (newer wins) while the merged state is wrapped. The reporting model
	// carries outer reason "quota", so wrap detection must include the
	// marker and shadow presence, not only the propagated label.
	if merged.Quota.Reason == "credential_quota" || merged.Quota.Reason == "quota" ||
		target.credentialQuotaPropagated || source.credentialQuotaPropagated ||
		target.credentialQuotaShadow != nil || source.credentialQuotaShadow != nil {
		if target.credentialQuotaShadow != nil && source.credentialQuotaShadow != nil {
			merged.credentialQuotaShadow = mergeModelState(target.credentialQuotaShadow.cloneShallow(), source.credentialQuotaShadow.cloneShallow())
		} else if preferred.credentialQuotaShadow != nil {
			merged.credentialQuotaShadow = preferred.credentialQuotaShadow.cloneShallow()
		} else if fallback.credentialQuotaShadow != nil {
			merged.credentialQuotaShadow = fallback.credentialQuotaShadow.cloneShallow()
		}
		merged.credentialQuotaPropagated = target.credentialQuotaPropagated || source.credentialQuotaPropagated
	}
	if target.scopedQuotaPropagated || source.scopedQuotaPropagated || target.scopedQuotaShadow != nil || source.scopedQuotaShadow != nil {
		merged.scopedQuotaPropagated = target.scopedQuotaPropagated || source.scopedQuotaPropagated
		if target.scopedQuotaShadow != nil && source.scopedQuotaShadow != nil {
			merged.scopedQuotaShadow = mergeModelState(target.scopedQuotaShadow.cloneShallow(), source.scopedQuotaShadow.cloneShallow())
		} else if preferred.scopedQuotaShadow != nil {
			merged.scopedQuotaShadow = preferred.scopedQuotaShadow.cloneShallow()
		} else if fallback.scopedQuotaShadow != nil {
			merged.scopedQuotaShadow = fallback.scopedQuotaShadow.cloneShallow()
		}
	}
	*target = merged
	return target
}

// modelQuotaLayer orders the persisted layers from independent to scoped to shared.
func modelQuotaLayer(state *ModelState) int {
	if state == nil {
		return 0
	}
	if state.credentialQuotaPropagated || state.Quota.Reason == "credential_quota" {
		return 2
	}
	if state.scopedQuotaPropagated || state.scopedQuotaShadow != nil {
		return 1
	}
	return 0
}

// normalizeModelQuotaLayers wraps live shared quota and exposes expired quota
// before result ordering or recovery inspects an independent deadline or watermark.
func normalizeModelQuotaLayers(auth *Auth, state *ModelState, now time.Time) bool {
	changed := false
	if state != nil && state.Status != StatusDisabled && modelQuotaLayer(state) != 2 &&
		auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now) {
		seedModelForceCooldownDeadline(state)
		var independent *ModelState
		if isModelStateActiveCooldown(state, now) && !isDevinQuotaExhaustionMessage(devinStateErrorText(state)) {
			independent = state.Clone()
		}
		quota := mergeQuotaObservation(cooldownFieldsOf(auth.Quota), state.Quota)
		*state = ModelState{
			Unavailable: true, Status: StatusError, NextRetryAfter: auth.Quota.NextRecoverAt,
			Quota: quota, UpdatedAt: auth.UpdatedAt, forceCooldownSeeded: true,
			credentialQuotaPropagated: true, credentialQuotaShadow: independent,
		}
		if independent != nil && independent.NextRetryAfter.After(state.NextRetryAfter) {
			state.NextRetryAfter = independent.NextRetryAfter
		}
		changed = true
	}
	for state != nil && state.Status != StatusDisabled {
		var shadow *ModelState
		switch modelQuotaLayer(state) {
		case 2:
			if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now) {
				return changed
			}
			shadow = state.credentialQuotaShadow
		case 1:
			if state.Quota.NextRecoverAt.After(now) {
				return changed
			}
			shadow = state.scopedQuotaShadow
		default:
			return changed
		}
		if shadow != nil {
			*state = *shadow.Clone()
		} else {
			resetModelState(state, now)
		}
		changed = true
	}
	return changed
}

func resetModelState(state *ModelState, now time.Time) {
	if state == nil {
		return
	}
	state.Unavailable = false
	state.Status = StatusActive
	state.StatusMessage = ""
	state.NextRetryAfter = time.Time{}
	state.LastError = nil
	applyCooldownFields(&state.Quota, QuotaState{})
	state.cooldownRecordedSeq = 0
	state.forceCooldownUntil = time.Time{}
	state.quotaForceCooldownUntil = time.Time{}
	state.nonQuotaCooldownUntil = time.Time{}
	state.credentialQuotaShadow = nil
	state.credentialQuotaPropagated = false
	state.scopedQuotaShadow = nil
	state.scopedQuotaPropagated = false
	state.UpdatedAt = now
}

func isModelStateActiveCooldown(state *ModelState, now time.Time) bool {
	if state == nil {
		return false
	}
	if state.Status == StatusDisabled {
		return true
	}
	if !state.NextRetryAfter.IsZero() && state.NextRetryAfter.After(now) {
		return true
	}
	if !state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.After(now) {
		return true
	}
	if state.Quota.Exceeded && state.Quota.NextRecoverAt.IsZero() {
		return true
	}
	return false
}

func (m *Manager) registryModelsForAuthAndModel(auth *Auth, authID, modelKey string) []string {
	authID = strings.TrimSpace(authID)
	modelKey = strings.TrimSpace(modelKey)
	if authID == "" && auth != nil {
		authID = auth.ID
	}
	if auth == nil && m != nil && authID != "" {
		m.mu.RLock()
		auth = m.auths[authID]
		m.mu.RUnlock()
	}
	if authID == "" || modelKey == "" {
		return nil
	}

	targetCanonical := canonicalModelLookupKey(modelKey)
	supportedModels := registry.GetGlobalRegistry().GetModelsForClient(authID)
	if len(supportedModels) == 0 {
		return nil
	}

	out := make([]string, 0, len(supportedModels))
	seen := make(map[string]struct{}, len(supportedModels))

	for _, model := range supportedModels {
		if model == nil || strings.TrimSpace(model.ID) == "" {
			continue
		}
		regModelID := strings.TrimSpace(model.ID)
		stateKey := ""
		if m != nil {
			stateKey = m.selectionModelKeyForAuth(auth, regModelID)
		}
		if stateKey == "" {
			stateKey = canonicalModelKey(regModelID)
		}
		if canonicalModelLookupKey(stateKey) == targetCanonical {
			if _, exists := seen[regModelID]; !exists {
				seen[regModelID] = struct{}{}
				out = append(out, regModelID)
			}
		}
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

func modelStateIsClean(state *ModelState) bool {
	if state == nil {
		return true
	}
	if state.Status != StatusActive {
		return false
	}
	if state.Unavailable || state.StatusMessage != "" || !state.NextRetryAfter.IsZero() || state.LastError != nil {
		return false
	}
	if state.Quota.Exceeded || state.Quota.Reason != "" || !state.Quota.NextRecoverAt.IsZero() || state.Quota.BackoffLevel != 0 {
		return false
	}
	return true
}

func updateAggregatedAvailability(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	// A terminal unauthorized credential stays blocked until its tokens change.
	// Model-level results must not make it selectable again.
	if hasUnauthorizedAuthFailure(auth) {
		auth.Unavailable = true
		return
	}
	defer func() {
		state := auth.independentCooldown
		if state == nil || !state.Unavailable || !state.NextRetryAfter.After(now) {
			return
		}
		auth.Unavailable = true
		if state.NextRetryAfter.After(auth.NextRetryAfter) {
			auth.NextRetryAfter = state.NextRetryAfter
		}
		if !auth.Quota.Exceeded {
			auth.LastError = cloneError(state.LastError)
			auth.StatusMessage = state.StatusMessage
			auth.Status = StatusError
			if state.Quota.Exceeded {
				applyCooldownFields(&auth.Quota, state.Quota)
			}
		}
	}()
	if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now) {
		auth.Unavailable = true
		if activeAuthForce := effectiveAuthForceUntil(auth); activeAuthForce.After(now) && auth.NextRetryAfter.Before(activeAuthForce) {
			auth.NextRetryAfter = activeAuthForce
		}
		return
	}
	activeAuthForce := effectiveAuthForceUntil(auth)
	if !activeAuthForce.After(now) && !auth.forceCooldownUntil.IsZero() {
		auth.forceCooldownUntil = time.Time{}
	}
	if !auth.credentialRetryAfter.IsZero() && !auth.credentialRetryAfter.After(now) {
		auth.credentialRetryAfter = time.Time{}
	}
	if len(auth.ModelStates) == 0 {
		if activeAuthForce.After(now) {
			auth.Unavailable = true
			if auth.NextRetryAfter.Before(activeAuthForce) {
				auth.NextRetryAfter = activeAuthForce
			}
			return
		}
		if auth.credentialRetryAfter.After(now) {
			auth.Unavailable = true
			if auth.NextRetryAfter.Before(auth.credentialRetryAfter) {
				auth.NextRetryAfter = auth.credentialRetryAfter
			}
			return
		}
		clearAggregatedAvailability(auth)
		return
	}
	allUnavailable := true
	earliestRetry := time.Time{}
	quotaExceeded := false
	quotaRecover := time.Time{}
	maxBackoffLevel := 0
	hasState := false
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		hasState = true
		stateUnavailable := false
		if state.Status == StatusDisabled {
			stateUnavailable = true
		} else if state.Unavailable {
			if state.NextRetryAfter.IsZero() {
				stateUnavailable = false
			} else if state.NextRetryAfter.After(now) {
				stateUnavailable = true
				if earliestRetry.IsZero() || state.NextRetryAfter.Before(earliestRetry) {
					earliestRetry = state.NextRetryAfter
				}
			} else {
				state.Unavailable = false
				state.NextRetryAfter = time.Time{}
			}
		}
		if !stateUnavailable {
			allUnavailable = false
		}
		if state.Quota.Exceeded {
			quotaExceeded = true
			if quotaRecover.IsZero() || (!state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.Before(quotaRecover)) {
				quotaRecover = state.Quota.NextRecoverAt
			}
			if state.Quota.BackoffLevel > maxBackoffLevel {
				maxBackoffLevel = state.Quota.BackoffLevel
			}
		}
	}
	if !hasState {
		if activeAuthForce.After(now) {
			auth.Unavailable = true
			if auth.NextRetryAfter.Before(activeAuthForce) {
				auth.NextRetryAfter = activeAuthForce
			}
			return
		}
		if auth.credentialRetryAfter.After(now) {
			auth.Unavailable = true
			if auth.NextRetryAfter.Before(auth.credentialRetryAfter) {
				auth.NextRetryAfter = auth.credentialRetryAfter
			}
			return
		}
		clearAggregatedAvailability(auth)
		return
	}
	auth.Unavailable = allUnavailable
	if allUnavailable {
		auth.NextRetryAfter = earliestRetry
	} else {
		auth.NextRetryAfter = time.Time{}
	}
	if activeAuthForce.After(now) {
		auth.Unavailable = true
		if auth.NextRetryAfter.Before(activeAuthForce) {
			auth.NextRetryAfter = activeAuthForce
		}
	}
	if auth.credentialRetryAfter.After(now) {
		auth.Unavailable = true
		if auth.NextRetryAfter.Before(auth.credentialRetryAfter) {
			auth.NextRetryAfter = auth.credentialRetryAfter
		}
	}
	if quotaExceeded {
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		if auth.Quota.NextRecoverAt.After(quotaRecover) {
			quotaRecover = auth.Quota.NextRecoverAt
		}
		auth.Quota.NextRecoverAt = quotaRecover
		auth.Quota.BackoffLevel = maxBackoffLevel
	} else if auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(now) {
		// Retain active auth-level quota cooldown
	} else {
		applyCooldownFields(&auth.Quota, QuotaState{})
	}
}

func clearAggregatedAvailability(auth *Auth) {
	if auth == nil {
		return
	}
	auth.Unavailable = false
	auth.NextRetryAfter = time.Time{}
	applyCooldownFields(&auth.Quota, QuotaState{})
}

func hasModelError(auth *Auth, now time.Time) bool {
	if auth == nil || len(auth.ModelStates) == 0 {
		return false
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.LastError != nil {
			return true
		}
		if state.Status == StatusError {
			if state.Unavailable && (state.NextRetryAfter.IsZero() || state.NextRetryAfter.After(now)) {
				return true
			}
		}
	}
	return false
}

func propagateCredentialQuotaToModelStatesLocked(auth *Auth, exclude *ModelState, credentialNext time.Time, backoffLevel int, credCooldownSignals map[string]string, prevAuthCredActive bool, prevAuthCredRecoverAt time.Time, now time.Time) {
	if auth == nil {
		return
	}
	for _, otherState := range auth.ModelStates {
		if otherState == nil || otherState == exclude {
			continue
		}
		// Normalize a naturally expired wrap first (issue #6690) so a
		// stale marker cannot suppress snapshotting a fresh block.
		if (otherState.credentialQuotaPropagated || otherState.Quota.Reason == "credential_quota") &&
			!prevAuthCredActive {
			if shadow := otherState.credentialQuotaShadow; shadow != nil && isModelStateActiveCooldown(shadow, now) {
				preservedSeq := shadow.cooldownRecordedSeq
				*otherState = *shadow.cloneShallow()
				otherState.cooldownRecordedSeq = preservedSeq
			} else {
				resetModelState(otherState, now)
			}
		}
		// On first wrap, preserve the sibling's independent layer
		// beneath the propagated credential quota (issue #6690):
		// a live own block is snapshotted; healthy/propagated-only
		// states get none. The marker keeps repeated propagation
		// from mistaking its own earlier layer for independence.
		alreadyPropagated := otherState.credentialQuotaPropagated ||
			otherState.Quota.Reason == "credential_quota"
		if !alreadyPropagated {
			if isModelStateActiveCooldown(otherState, now) && !isDevinQuotaExhaustionMessage(devinStateErrorText(otherState)) {
				otherState.credentialQuotaShadow = otherState.cloneShallow()
			} else {
				otherState.credentialQuotaShadow = nil
			}
		}
		otherState.credentialQuotaPropagated = true
		otherState.Unavailable = true
		otherState.Status = StatusError
		otherQuotaNext := credentialNext
		otherSignals := credCooldownSignals
		if prevAuthCredActive && prevAuthCredRecoverAt.After(otherQuotaNext) {
			otherQuotaNext = prevAuthCredRecoverAt
		}
		if otherState.Quota.Exceeded && otherState.Quota.NextRecoverAt.After(otherQuotaNext) {
			otherQuotaNext = otherState.Quota.NextRecoverAt
			otherSignals = maps.Clone(otherState.Quota.cooldownSignals)
		}
		otherRetryAfter := otherQuotaNext
		// Propagation only extends a sibling's still-live
		// per-model deadline; it never shortens one.
		if !otherState.NextRetryAfter.IsZero() && otherState.NextRetryAfter.After(otherRetryAfter) {
			otherRetryAfter = otherState.NextRetryAfter
		}
		otherState.NextRetryAfter = otherRetryAfter
		applyCooldownFields(&otherState.Quota, QuotaState{
			Exceeded:        true,
			Reason:          "credential_quota",
			NextRecoverAt:   otherQuotaNext,
			BackoffLevel:    backoffLevel,
			cooldownSignals: otherSignals,
		})
	}
}

func clearAuthStateOnSuccess(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	if hasUnauthorizedAuthFailure(auth) {
		auth.Unavailable = true
		return
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.credentialQuotaPropagated || state.Quota.Reason == "credential_quota" {
			if shadow := state.credentialQuotaShadow; shadow != nil && isModelStateActiveCooldown(shadow, now) {
				preservedSeq := shadow.cooldownRecordedSeq
				*state = *shadow.cloneShallow()
				state.cooldownRecordedSeq = preservedSeq
			} else {
				resetModelState(state, now)
			}
		}
	}
	auth.Unavailable = false
	auth.Status = StatusActive
	auth.StatusMessage = ""
	applyCooldownFields(&auth.Quota, QuotaState{})
	auth.LastError = nil
	auth.NextRetryAfter = time.Time{}
	auth.cooldownRecordedSeq = 0
	auth.forceCooldownUntil = time.Time{}
	auth.quotaForceCooldownUntil = time.Time{}
	auth.credQuotaForceUntil = time.Time{}
	auth.independentCooldown = &ModelState{Status: StatusActive, UpdatedAt: now, forceCooldownSeeded: true}
	auth.credentialRetryAfter = time.Time{}
	auth.nonQuotaCooldownUntil = time.Time{}
	auth.UpdatedAt = now
}

// isAuthActiveCooldown reports whether the auth carries a still-live
// auth-level cooldown recorded without a model key.
func isAuthActiveCooldown(auth *Auth, now time.Time) bool {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
		return false
	}
	if !auth.NextRetryAfter.IsZero() && auth.NextRetryAfter.After(now) {
		return true
	}
	if !auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now) {
		return true
	}
	if auth.Quota.Exceeded && auth.Quota.NextRecoverAt.IsZero() {
		return true
	}
	return false
}

func cloneError(err *Error) *Error {
	if err == nil {
		return nil
	}
	return err.Clone()
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func statusCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	type statusCoder interface {
		StatusCode() int
	}
	var sc statusCoder
	if errors.As(err, &sc) && sc != nil {
		return sc.StatusCode()
	}
	return 0
}

func errorCodeFromError(err error) string {
	if err == nil {
		return ""
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil && strings.TrimSpace(authErr.Code) != "" {
		return strings.TrimSpace(authErr.Code)
	}
	type errorCoder interface {
		ErrorCode() string
	}
	var ec errorCoder
	if errors.As(err, &ec) && ec != nil {
		return strings.TrimSpace(ec.ErrorCode())
	}
	return ""
}

func isRequestScopedError(err error) bool {
	if err == nil || isCredentialScopedError(err) {
		return false
	}
	// A preserved terminal response belongs to this request unless the executor
	// explicitly classifies the rejection as safe to retry on another credential.
	if isRequestTerminatedError(err) {
		return true
	}
	requestErr, ok := errors.AsType[cliproxyexecutor.RequestScopedError](err)
	return ok && requestErr != nil && requestErr.IsRequestScoped()
}

// isStreamTruncatedError reports locally synthesized failures for accepted
// upstream streams that ended without a terminal frame. Unlike a received
// HTTP refusal, the generation was already submitted upstream.
func isStreamTruncatedError(err error) bool {
	if err == nil || isCredentialScopedError(err) {
		return false
	}
	truncatedErr, ok := errors.AsType[cliproxyexecutor.StreamTruncatedError](err)
	if ok && truncatedErr != nil && truncatedErr.IsStreamTruncated() {
		return true
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil && authErr.Code == streamTruncatedErrorCode {
		return true
	}
	return false
}

func isStreamTruncatedResultError(err *Error) bool {
	return err != nil && err.Code == streamTruncatedErrorCode
}

// ResultErrorFromError classifies an execution or transport error into an auth.Error
// while preserving lifecycle and transient transport classifications so MarkResult
// does not cool credentials on client cancellations or network drops.
func ResultErrorFromError(err error) *Error {
	return resultErrorFromError(err)
}

func resultErrorFromError(err error) *Error {
	if err == nil {
		return nil
	}
	var sourceErr *Error
	var resultErr *Error
	if errors.As(err, &sourceErr) && sourceErr != nil {
		resultErr = cloneError(sourceErr)
	} else {
		resultErr = &Error{Message: err.Error()}
	}
	if resultErr.Code == "" {
		resultErr.Code = errorCodeFromError(err)
	}
	if resultErr.HTTPStatus == 0 {
		resultErr.HTTPStatus = statusCodeFromError(err)
	}
	type tosMetadataProvider interface {
		TOSAppealMetadata() map[string]string
	}
	var tmp tosMetadataProvider
	if errors.As(err, &tmp) && tmp != nil {
		if meta := tmp.TOSAppealMetadata(); len(meta) > 0 {
			resultErr.Metadata = meta
		}
	}
	switch {
	case isCredentialScopedError(err):
		// A credential-scoped failure always stays on the credential quota path;
		// normalize conflicting model-only or cooldown-skipping codes so MarkResult
		// cannot downgrade whole-credential exhaustion.
		switch strings.ToLower(strings.TrimSpace(resultErr.Code)) {
		case transientRateLimitErrorCode,
			requestScopedErrorCode,
			streamTruncatedErrorCode,
			connectionLifecycleErrorCode,
			transientTransportErrorCode:
			resultErr.Code = credentialQuotaErrorCode
		}
	case isStreamTruncatedError(err):
		// Preserve the local truncation classification alongside the public
		// 502 status so MarkResult can skip credential cooldown.
		resultErr.Code = streamTruncatedErrorCode
	case isTransientRateLimitError(err):
		// Preserve the transient capacity classification alongside the public
		// 429 status so MarkResult cools only the reporting model until the
		// provider retry hint instead of recording quota exhaustion.
		resultErr.Code = transientRateLimitErrorCode
	case isExplicitModelNotFoundError(err, ""):
		if resultErr.Code == "" || resultErr.Code == requestScopedErrorCode {
			resultErr.Code = "model_not_found"
		}
	case isRequestScopedError(err) || isRequestInvalidError(err):
		// Prefer true request-scoped faults (including Claude OAuth cancellation)
		// over the broader connection-lifecycle classification.
		resultErr.Code = requestScopedErrorCode
	case isConfiguredTLSHandshakeTimeoutError(err):
		// An internal TLS establishment deadline is a transient transport
		// failure, not a caller-owned context lifecycle event.
		if resultErr.Code == "" || resultErr.Code == transientTransportErrorCode {
			resultErr.Code = transientTransportErrorCode
		}
	case isConnectionLifecycleError(err):
		// Preserve lifecycle classification for MarkResult without making the error
		// request-scoped (which would also stop credential fallback).
		if resultErr.Code == "" || resultErr.Code == connectionLifecycleErrorCode {
			resultErr.Code = connectionLifecycleErrorCode
		}
	case isTransientTransportError(err):
		if resultErr.Code == "" || resultErr.Code == transientTransportErrorCode {
			resultErr.Code = transientTransportErrorCode
		}
	}
	return resultErr
}

// shouldSkipCredentialCooldown reports failures that must not mark auth/model cooling.
// Connection lifecycle is intentionally separate from request_scoped so transport
// drops do not also stop credential rotation via isRequestInvalidError.
func shouldSkipCredentialCooldown(err *Error) bool {
	if err != nil && err.Code == ErrorCodeForceCooldown {
		return false
	}
	if err != nil && (err.Code == ErrorCodeAntigravityTargetUnavailable || err.Code == ErrorCodeAntigravityReferenceUnavailable) {
		return true
	}
	return isRequestScopedResultError(err) || isConnectionLifecycleResultError(err) || isTransientTransportResultError(err) || isStreamTruncatedResultError(err)
}

// CheckAntigravityTargetModel checks model state when a grouped route resolves
// to a different wire model on the selected credential.
func CheckAntigravityTargetModel(auth *Auth, model string, now time.Time, useCredits bool) error {
	state := existingModelState(auth, model)
	if state == nil {
		return nil
	}
	blocked, _, next := availabilityBlock(state.Unavailable, state.Quota.Exceeded, state.NextRetryAfter, state.Quota.NextRecoverAt, now)
	if state.Status != StatusDisabled && (!blocked || (useCredits && state.Quota.Exceeded &&
		state.Quota.Reason == "quota" && state.Quota.NextRecoverAt.After(now) &&
		!state.NextRetryAfter.After(state.Quota.NextRecoverAt) && !state.nonQuotaCooldownUntil.After(now) &&
		!effectiveModelForceUntil(state).After(now))) {
		return nil
	}
	err := &Error{Code: ErrorCodeAntigravityTargetUnavailable, HTTPStatus: http.StatusServiceUnavailable, Message: "selected Antigravity effort variant is unavailable"}
	if state.Status != StatusDisabled && next.After(now) {
		err.HTTPStatus = http.StatusTooManyRequests
		return &trustedRetryAfterError{cause: err, deadline: next}
	}
	return err
}

// isConnectionLifecycleError reports transport/session lifecycle failures that must
// not cool credentials: client cancellation and WebSocket/HTTP/2 close/EOF disconnects.
func isConnectionLifecycleError(err error) bool {
	if err == nil {
		return false
	}
	// Typed WebSocket close codes are an unambiguous connection lifecycle signal.
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) && closeErr != nil {
		switch closeErr.Code {
		case websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseAbnormalClosure:
			return true
		}
	}
	// Credential/auth/quota statuses must never be reclassified from response text.
	if statusCodeFromError(err) != 0 {
		return false
	}
	if isConfiguredTLSHandshakeTimeoutError(err) {
		return false
	}
	// Client abort and request-scoped timeouts are not credential faults.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return isConnectionLifecycleMessage(err.Error())
}

func isConnectionLifecycleResultError(err *Error) bool {
	if err == nil {
		return false
	}
	if err.Code == connectionLifecycleErrorCode {
		return true
	}
	if err.Code == transientTransportErrorCode {
		return false
	}
	// Message fallback only when no HTTP status is attached, so 401/429/5xx
	// response bodies cannot suppress credential cooldown.
	if statusCodeFromResult(err) != 0 {
		return false
	}
	return isConnectionLifecycleMessage(err.Message)
}

func isConnectionLifecycleMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	switch lower {
	case "context canceled", "context deadline exceeded", "eof", "unexpected eof":
		return true
	}
	// gorilla/websocket CloseError.Error() and common wrappers.
	if strings.Contains(lower, "websocket: close 1000") ||
		strings.Contains(lower, "websocket: close 1001") ||
		strings.Contains(lower, "websocket: close 1006") {
		return true
	}
	// Wrapped transport EOF phrasing (e.g. "read tcp ...: unexpected EOF").
	// "http2: response body closed" means CPA closed the upstream body itself,
	// usually after the downstream client went away.
	if strings.Contains(lower, "unexpected eof") || strings.Contains(lower, "http2: response body closed") {
		return true
	}
	if isHTTP2ClosedConnectionMessage(lower) {
		return true
	}
	return false
}

func isHTTP2ClosedConnectionMessage(lower string) bool {
	// x/net/http2 keeps these errors unexported; allow common wrappers such as url.Error.
	return strings.Contains(lower, "http2: client connection force closed via clientconn.close") ||
		strings.Contains(lower, "http2: client conn is closed")
}

// isConfiguredTLSHandshakeTimeoutError identifies the typed internal
// establishment deadline produced by the configured TLS handshake bound.
// Delegating to the shared executor predicate ensures classification is
// based on error identity rather than message text and that any
// caller-owned context error outside a configured-timeout subtree wins.
func isConfiguredTLSHandshakeTimeoutError(err error) bool {
	if err == nil || statusCodeFromError(err) != 0 {
		return false
	}
	return cliproxyexecutor.IsConfiguredTLSHandshakeTimeoutError(err)
}

// isTransientTransportError reports pre-HTTP dial/TLS/DNS/reset failures that
// should retry under request-retry without cooling the selected credential.
func isTransientTransportError(err error) bool {
	if err == nil {
		return false
	}
	// HTTP-status failures stay on the credential/status retry path.
	if statusCodeFromError(err) != 0 {
		return false
	}
	if isConfiguredTLSHandshakeTimeoutError(err) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr != nil && (dnsErr.IsTimeout || dnsErr.IsTemporary || dnsErr.Timeout()) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr != nil && netErr.Timeout() {
		return true
	}
	if isTransientSyscallError(err) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr != nil {
		return true
	}
	return isTransientTransportMessage(err.Error())
}

func isTransientTransportResultError(err *Error) bool {
	if err == nil {
		return false
	}
	if err.Code == transientTransportErrorCode {
		return true
	}
	if statusCodeFromResult(err) != 0 {
		return false
	}
	// Once an error has been reduced to a Result message, typed identity is
	// no longer available. An explicit transient code assigned by
	// resultErrorFromError is authoritative for configured handshake
	// timeouts; matching text alone must not promote an external or
	// caller-owned deadline that happens to use the same phrase.
	if strings.Contains(strings.ToLower(err.Message), "tls handshake timeout") {
		return false
	}
	return isTransientTransportMessage(err.Message)
}

func isTransientSyscallError(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ECONNABORTED,
		syscall.ETIMEDOUT, syscall.EHOSTUNREACH, syscall.ENETUNREACH, syscall.EPIPE:
		return true
	default:
		return false
	}
}

func isTransientTransportMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	switch {
	case strings.Contains(lower, "tls: tls handshake"),
		strings.Contains(lower, "tls handshake timeout"),
		strings.Contains(lower, "wsarecv"),
		strings.Contains(lower, "wsasend"),
		strings.Contains(lower, "a connection attempt failed"),
		strings.Contains(lower, "connection refused"),
		strings.Contains(lower, "connection reset"),
		strings.Contains(lower, "i/o timeout"),
		strings.Contains(lower, "no such host"),
		strings.Contains(lower, "server misbehaving"),
		strings.Contains(lower, "network is unreachable"),
		strings.Contains(lower, "no route to host"),
		strings.Contains(lower, "broken pipe"),
		strings.Contains(lower, "connection aborted"),
		strings.Contains(lower, "use of closed network connection"),
		strings.Contains(lower, "unexpected eof"),
		isHTTP2ClosedConnectionMessage(lower):
		return true
	default:
		return false
	}
}

func isUnauthorizedError(err error) bool {
	if err == nil || codex.IsRefreshPreSendError(err) {
		return false
	}
	if statusCodeFromError(err) == http.StatusUnauthorized {
		return true
	}
	raw := strings.ToLower(err.Error())
	return strings.Contains(raw, "status 401") || strings.Contains(raw, "401 unauthorized") || strings.Contains(raw, "(401)")
}

func hasUnauthorizedAuthFailure(auth *Auth) bool {
	if auth == nil || auth.LastError == nil {
		return false
	}
	if auth.Unavailable && auth.Status == StatusError && auth.NextRefreshAfter.IsZero() && auth.NextRetryAfter.IsZero() &&
		(auth.LastError.StatusCode() == http.StatusUnauthorized || strings.EqualFold(auth.LastError.Code, "unauthorized")) {
		return true
	}
	return false
}

// HasUnauthorizedAuthFailure reports whether the auth has a terminal unauthorized error
// with no pending refresh scheduled.
func HasUnauthorizedAuthFailure(auth *Auth) bool {
	return hasUnauthorizedAuthFailure(auth)
}

func hasDisabledInvalidGrantFailure(auth *Auth) bool {
	if auth == nil {
		return false
	}
	isDisabled := auth.Disabled || auth.Status == StatusDisabled
	if !isDisabled {
		return false
	}
	if auth.LastError != nil && (isInvalidGrantResultError(auth.LastError) || isInvalidGrantErrorMessage(auth.LastError.Message) || isInvalidGrantErrorMessage(auth.LastError.Code)) {
		return true
	}
	return false
}

// HasDisabledInvalidGrantFailure reports whether the auth is disabled and has encountered an invalid_grant error.
func HasDisabledInvalidGrantFailure(auth *Auth) bool {
	return hasDisabledInvalidGrantFailure(auth)
}

var (
	renewalURLPattern          = regexp.MustCompile(`(?i)https?://[^\s"'<>\\]+`)
	renewalJWTPattern          = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)?\b`)
	renewalSecretKVPattern     = regexp.MustCompile(`(?i)\b(access_token|refresh_token|id_token|client_secret|code_verifier|code_challenge|authorization)\s*([=:]|"\s*:\s*")\s*(?:(?:bearer|basic|token)\s+)?([^\s"&',}\\]+)`)
	renewalOAuthParamPatten    = regexp.MustCompile(`(?i)(?:[?&]|\b)(code|state)=([^\s"&',}\\]+)`)
	renewalEscapedParamPattern = regexp.MustCompile(`(?i)(code|state)%3[dD]([^\s"&',}\\%]+)`)
	renewalJSONStatePattern    = regexp.MustCompile(`(?i)("state"\s*:\s*)"([^"]*)(?:"|$)`)
	renewalJSONCodePattern     = regexp.MustCompile(`(?i)("code"\s*:\s*)"([^"]*)(?:"|$)`)
	renewalBearerPattern       = regexp.MustCompile(`(?i)\bBearer\s+[^\s"',}\\]+`)
)

func isKnownSafeOAuthErrorCode(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "refresh_outcome_unknown",
		"refresh_token_reused",
		"refresh_token_expired",
		"refresh_token_invalidated",
		"token_revoked",
		"invalid_grant",
		"invalid_client",
		"invalid_request",
		"invalid_request_error",
		"unauthorized",
		"rate_limit_exceeded",
		"insufficient_quota",
		"billing_hard_limit_reached",
		"account_deactivated",
		"context_length_exceeded",
		"server_error",
		"service_unavailable",
		"bad_gateway",
		"model_not_found":
		return true
	default:
		return false
	}
}

// SanitizeRenewalMessage strips URLs, bearer tokens, JWTs, OAuth codes/states, and secret fields from diagnostic messages.
func SanitizeRenewalMessage(raw string) string {
	return sanitizeRenewalMessage(raw)
}

func redactRenewalPatterns(in string) string {
	out := renewalURLPattern.ReplaceAllString(in, "[redacted-url]")
	out = renewalJWTPattern.ReplaceAllString(out, "[redacted-token]")
	out = renewalBearerPattern.ReplaceAllString(out, "Bearer [redacted]")
	out = renewalSecretKVPattern.ReplaceAllString(out, "${1}=[redacted]")
	out = renewalOAuthParamPatten.ReplaceAllString(out, "${1}=[redacted]")
	out = renewalEscapedParamPattern.ReplaceAllString(out, "${1}=[redacted]")
	out = renewalJSONStatePattern.ReplaceAllString(out, `${1}"[redacted]"`)
	out = renewalJSONCodePattern.ReplaceAllStringFunc(out, func(match string) string {
		sub := renewalJSONCodePattern.FindStringSubmatch(match)
		if len(sub) == 3 && isKnownSafeOAuthErrorCode(sub[2]) && strings.HasSuffix(match, `"`) {
			return match
		}
		if len(sub) >= 2 {
			return sub[1] + `"[redacted]"`
		}
		return `"code":"[redacted]"`
	})
	return out
}

func sanitizeRenewalMessage(raw string) string {
	// Redact secrets on the full input BEFORE truncation so secrets straddling the truncation boundary cannot leak.
	preRedacted := redactRenewalPatterns(raw)
	out := internallogging.SafeDiagnosticForLog(preRedacted)
	return redactRenewalPatterns(out)
}

func isTerminalRefreshGrantMessage(message string) bool {
	lower := strings.ToLower(message)
	for _, token := range []string{
		"refresh_outcome_unknown",
		"refresh_token_reused",
		"refresh_token_expired",
		// Anthropic returns {"error": "invalid_grant", "error_description": "Refresh token expired"}.
		"refresh token expired",
		"refresh_token_invalidated",
		"token_revoked",
		"invalid_client",
	} {
		if strings.Contains(lower, token) {
			return true
		}
	}
	return false
}

func isTerminalRefreshError(auth *Auth, err error) bool {
	if err == nil || codex.IsRefreshPreSendError(err) {
		return false
	}
	var partErr interface {
		OuterRefreshError() error
		SIWCRefreshError() error
	}
	if errors.As(err, &partErr) && partErr != nil {
		return isTerminalRefreshError(auth, partErr.OuterRefreshError()) ||
			isTerminalRefreshError(auth, partErr.SIWCRefreshError())
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil && isTerminalRefreshGrantMessage(authErr.Code) {
		return true
	}
	if isUnauthorizedError(err) || isTerminalRefreshGrantMessage(err.Error()) {
		return true
	}
	if auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") && isInvalidGrantError(err) {
		return true
	}
	return false
}

func terminalRefreshErrorCode(message string, statusCode int) string {
	lower := strings.ToLower(message)
	for _, code := range []string{
		"refresh_outcome_unknown",
		"refresh_token_reused",
		"refresh_token_expired",
		"refresh_token_invalidated",
		"token_revoked",
		"invalid_client",
	} {
		if strings.Contains(lower, code) {
			return code
		}
	}
	if strings.Contains(lower, "refresh token expired") {
		return "refresh_token_expired"
	}
	if isInvalidGrantErrorMessage(lower) {
		return "invalid_grant"
	}
	if statusCode == http.StatusUnauthorized {
		return "unauthorized"
	}
	return "refresh_failed"
}

func sanitizeRenewalError(auth *Auth, err error) *Error {
	if err == nil {
		return nil
	}
	var partErr interface {
		OuterRefreshError() error
		SIWCRefreshError() error
	}
	if errors.As(err, &partErr) && partErr != nil {
		outerErr := partErr.OuterRefreshError()
		siwcErr := partErr.SIWCRefreshError()
		if outerErr != nil && isTerminalRefreshError(auth, outerErr) {
			err = outerErr
		} else if siwcErr != nil && isTerminalRefreshError(auth, siwcErr) {
			err = siwcErr
		}
	}
	raw := err.Error()
	statusCode := statusCodeFromError(err)
	if statusCode == 0 {
		if isUnauthorizedError(err) {
			statusCode = http.StatusUnauthorized
		} else if isInvalidGrantError(err) || isTerminalRefreshGrantMessage(raw) {
			statusCode = http.StatusBadRequest
		}
	}
	terminal := isTerminalRefreshError(auth, err)
	code := terminalRefreshErrorCode(raw, statusCode)
	var existingAuthErr *Error
	if errors.As(err, &existingAuthErr) && existingAuthErr != nil && isKnownSafeOAuthErrorCode(existingAuthErr.Code) {
		code = strings.ToLower(strings.TrimSpace(existingAuthErr.Code))
	}
	return &Error{
		Code:       code,
		Message:    sanitizeRenewalMessage(raw),
		HTTPStatus: statusCode,
		Retryable:  !terminal,
	}
}

func refreshErrorFromError(err error) *Error {
	if err == nil {
		return nil
	}
	statusCode := statusCodeFromError(err)
	if statusCode == 0 && isUnauthorizedError(err) {
		statusCode = http.StatusUnauthorized
	}
	authErr := &Error{Message: sanitizeRenewalMessage(err.Error()), HTTPStatus: statusCode}
	if statusCode == http.StatusUnauthorized {
		authErr.Code = "unauthorized"
		authErr.Retryable = false
	}
	return authErr
}

func retryAfterFromError(err error) *time.Duration {
	if err == nil {
		return nil
	}
	// An exhausted Home round owns the aggregated multi-credential deadline
	// for scheduling just as it does for response headers.
	var exhausted *homeRetryRoundExhaustedError
	if errors.As(err, &exhausted) && exhausted != nil {
		// The aggregate owns this error even when RetryAfter() is nil: nil
		// encodes an explicit zero (immediate) or absent aggregate and must
		// not be replaced by the last attempt's positive hint. Callers that
		// need the immediate signal consult retryNow / the aggregate first.
		return exhausted.RetryAfter()
	}
	// A locally trusted deadline outranks any hint on the wrapped error so a
	// re-recorded terminal error cannot reintroduce a stale upstream value.
	if trusted := trustedRetryAfterFromError(err); trusted != nil {
		return trusted
	}
	type retryAfterProvider interface {
		RetryAfter() *time.Duration
	}
	var rap retryAfterProvider
	if !errors.As(err, &rap) || rap == nil {
		return nil
	}
	// An expired trusted wrapper reports a negative remaining time; natural
	// expiry is not an invalid provider hint, so fall through to the wrapped
	// error's own hint instead of surfacing the negative value.
	switch rap.(type) {
	case *poolCooldownTerminalError, *trustedRetryAfterError:
		if inner := errors.Unwrap(err); inner != nil {
			return retryAfterFromError(inner)
		}
		return nil
	}
	retryAfter := rap.RetryAfter()
	if retryAfter == nil {
		return nil
	}
	value := *retryAfter
	return &value
}

// isTransientRateLimitError reports a transient per-model capacity/admission
// 429 whose quota is not exhausted. The classification travels through the
// shared executor contract, so only executors that explicitly vouch for the
// failure opt out of quota cooldown handling.
func isTransientRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	// A credential-scoped failure always stays on the quota path; a transient
	// classification must never downgrade whole-credential exhaustion.
	if isCredentialScopedError(err) {
		return false
	}
	transientErr, ok := errors.AsType[cliproxyexecutor.TransientRateLimitError](err)
	if ok && transientErr != nil && transientErr.IsTransientRateLimit() {
		return true
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil && authErr.Code == transientRateLimitErrorCode {
		return true
	}
	return false
}

func isTransientRateLimitResultError(err *Error) bool {
	return err != nil && err.Code == transientRateLimitErrorCode
}

func isCredentialScopedError(err error) bool {
	if err == nil {
		return false
	}
	type credentialScopedProvider interface {
		IsCredentialScoped() bool
	}
	var csp credentialScopedProvider
	if errors.As(err, &csp) && csp != nil && csp.IsCredentialScoped() {
		return true
	}
	return strings.EqualFold(errorCodeFromError(err), credentialQuotaErrorCode)
}

func statusCodeFromResult(err *Error) int {
	if err == nil {
		return 0
	}
	return err.StatusCode()
}

func isModelSupportErrorMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	patterns := [...]string{
		"model_not_supported",
		"requested model is not supported",
		"requested model is unsupported",
		"requested model is unavailable",
		"model is not supported",
		"model not supported",
		"unsupported model",
		"model unavailable",
		"not available for your plan",
		"not available for your account",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func isModelSupportError(err error) bool {
	if err == nil {
		return false
	}
	if isExplicitModelNotFoundError(err, "") {
		return true
	}
	status := statusCodeFromError(err)
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity && status != http.StatusNotFound {
		return false
	}
	return isModelSupportErrorMessage(err.Error())
}

func isInvalidGrantErrorMessage(message string) bool {
	return strings.Contains(strings.ToLower(message), "invalid_grant")
}

func isInvalidGrantError(err error) bool {
	if err == nil || codex.IsRefreshPreSendError(err) {
		return false
	}
	if !isInvalidGrantErrorMessage(err.Error()) {
		return false
	}
	status := statusCodeFromError(err)
	if status == http.StatusBadRequest || status == http.StatusUnauthorized || status == 0 {
		return true
	}
	return false
}

func isInvalidGrantResultError(err *Error) bool {
	if err == nil {
		return false
	}
	if !isInvalidGrantErrorMessage(err.Code) && !isInvalidGrantErrorMessage(err.Message) {
		return false
	}
	status := statusCodeFromResult(err)
	if status == http.StatusBadRequest || status == http.StatusUnauthorized || status == 0 {
		return true
	}
	return false
}

func isCreditBalanceTooLowError(err error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromError(err)
	if clienterror.IsCreditBalanceTooLow(status, err) {
		return true
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil && authErr.Message != "" {
		return clienterror.IsCreditBalanceTooLow(status, errors.New(authErr.Message))
	}
	return false
}

func isCreditBalanceTooLowResultError(err *Error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromResult(err)
	if clienterror.IsCreditBalanceTooLow(status, err) {
		return true
	}
	if err.Message != "" {
		return clienterror.IsCreditBalanceTooLow(status, errors.New(err.Message))
	}
	return false
}

func isModelSupportResultError(err *Error) bool {
	if err == nil {
		return false
	}
	if isExplicitModelNotFoundError(err, "") {
		return true
	}
	status := statusCodeFromResult(err)
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity && status != http.StatusNotFound {
		return false
	}
	return isModelSupportErrorMessage(err.Message)
}

func isCloudflareChallengeErrorMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(lower, "challenge-platform") ||
		strings.Contains(lower, "cf-mitigated") ||
		strings.Contains(lower, "cloudflare challenge") ||
		(strings.Contains(lower, "just a moment") && strings.Contains(lower, "cloudflare"))
}

// isCloudflareChallengeError checks whether err is a Cloudflare bot/waf challenge.
// Cloudflare challenges are served with HTTP 403. HTTP status >= 500 indicates an
// upstream gateway/origin failure (such as 520-526 origin errors) and takes precedence
// over challenge classification.
func isCloudflareChallengeError(err error) bool {
	if err == nil {
		return false
	}
	if status := statusCodeFromError(err); status >= 500 {
		return false
	}
	return isCloudflareChallengeErrorMessage(err.Error())
}

// isCloudflareChallengeResultError checks whether err is a Cloudflare bot/waf challenge.
// Responses with HTTP status >= 500 represent upstream gateway/origin failures
// (such as Cloudflare 520-526) and are excluded from challenge classification.
func isCloudflareChallengeResultError(err *Error) bool {
	if err == nil {
		return false
	}
	if status := statusCodeFromResult(err); status >= 500 {
		return false
	}
	return isCloudflareChallengeErrorMessage(err.Message)
}

// isTOSViolationResultError reports whether the failure is a terminal Antigravity
// Terms-of-Service suspension (403 PERMISSION_DENIED + ErrorInfo reason=
// TOS_VIOLATION). The executor marks these via the tos_violation error code so
// the credential is deactivated instead of cooled; sibling credentials are not
// hammered during an automated sweep.
func isTOSViolationResultError(err *Error) bool {
	if err == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(err.Code), "tos_violation")
}

// persistTOSAppealMetadata copies the appeal detail carried on a TOS_VIOLATION
// result error onto the credential record so the management API can surface it
// for the operator. It is a no-op for errors without appeal metadata.
func persistTOSAppealMetadata(auth *Auth, err *Error) {
	if auth == nil || err == nil || len(err.Metadata) == 0 {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	for key, value := range err.Metadata {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			auth.Metadata[key] = trimmed
		}
	}
}

func nextCloudflareCooldown(backoffLevel int, disableCooling bool, now time.Time) (time.Time, int) {
	var next time.Time
	if !disableCooling {
		cooldown, nextLevel := nextQuotaCooldown(backoffLevel, disableCooling)
		if cooldown < 10*time.Second {
			cooldown = 10 * time.Second
		}
		if cooldown > 0 {
			next = now.Add(cooldown)
		}
		backoffLevel = nextLevel
	}
	return next, backoffLevel
}

func isRequestScopedNotFoundResultError(err *Error) bool {
	if err == nil || statusCodeFromResult(err) != http.StatusNotFound {
		return false
	}
	return clienterror.IsItemNotPersisted(err.Message)
}

func isRequestScopedResultError(err *Error) bool {
	if err == nil {
		return false
	}
	if err.IsRequestScoped() || isRequestScopedNotFoundResultError(err) {
		return true
	}
	return isRequestInvalidError(err)
}

func isCountTokensEndpointNotFoundError(err error, requestedModel string) bool {
	if err == nil {
		return false
	}
	baseModel := thinking.ParseSuffix(requestedModel).ModelName
	if isExplicitModelNotFoundError(err, baseModel) {
		return false
	}
	status := statusCodeFromError(err)
	if status == http.StatusNotFound {
		return true
	}
	if isCredentialScopedError(err) || isCloudflareChallengeError(err) || isInvalidGrantError(err) || isCreditBalanceTooLowError(err) || isModelSupportErrorMessage(err.Error()) {
		return false
	}
	switch status {
	case http.StatusNotImplemented:
		return true
	case 0:
		return isCountTokensUnsupportedErrorCode(errorCodeFromError(err)) || errors.Is(err, errors.ErrUnsupported)
	default:
		return false
	}
}

func isCountTokensUnsupportedErrorCode(value string) bool {
	normalized := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(value)))
	switch normalized {
	case "not_implemented",
		"count_tokens_unsupported",
		"count_tokens_not_supported",
		"unsupported_operation",
		"unsupported_capability",
		"method_not_implemented":
		return true
	default:
		return false
	}
}

func isResponsesCompactRequest(opts cliproxyexecutor.Options) bool {
	return opts.Alt == "responses/compact"
}

func isResponsesCompactRequestFaultError(opts cliproxyexecutor.Options, err error) bool {
	if !isResponsesCompactRequest(opts) || err == nil {
		return false
	}
	if isCredentialScopedError(err) || isCloudflareChallengeError(err) || isInvalidGrantError(err) || isCreditBalanceTooLowError(err) {
		return false
	}
	status := statusCodeFromError(err)
	if clienterror.IsRequestFault(status, err) {
		return true
	}
	switch status {
	case http.StatusBadRequest,
		http.StatusNotFound,
		http.StatusMethodNotAllowed,
		http.StatusConflict,
		http.StatusRequestEntityTooLarge,
		http.StatusUnprocessableEntity,
		http.StatusNotImplemented:
		return true
	default:
		return false
	}
}

func isResponsesCompactAvailabilityNeutralError(opts cliproxyexecutor.Options, err error, resultErr *Error) bool {
	if !isResponsesCompactRequest(opts) {
		return false
	}
	if resultErr != nil && resultErr.Code == ErrorCodeForceCooldown {
		return false
	}
	if isCredentialScopedError(err) || isCloudflareChallengeError(err) || isInvalidGrantError(err) || isCreditBalanceTooLowError(err) {
		return false
	}
	if resultErr != nil && (isCloudflareChallengeResultError(resultErr) || isInvalidGrantResultError(resultErr) || isCreditBalanceTooLowResultError(resultErr)) {
		return false
	}
	status := statusCodeFromError(err)
	if status == 0 && resultErr != nil {
		status = statusCodeFromResult(resultErr)
	}
	if status == http.StatusUnauthorized || status == http.StatusPaymentRequired || status == http.StatusForbidden || status == http.StatusTooManyRequests {
		return false
	}
	return true
}

func isExplicitModelNotFoundError(err error, requestedModel string) bool {
	if err == nil {
		return false
	}
	if isModelNotFoundIdentifier(errorCodeFromError(err)) {
		return true
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		if isModelNotFoundIdentifier(authErr.Code) || isStructuredModelNotFoundError(authErr.Message, requestedModel) || isStructuredModelNotFoundError(authErr.Error(), requestedModel) {
			return true
		}
	} else if isStructuredModelNotFoundError(err.Error(), requestedModel) {
		return true
	}

	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, nested := range wrapped.Unwrap() {
			if isExplicitModelNotFoundError(nested, requestedModel) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return isExplicitModelNotFoundError(wrapped.Unwrap(), requestedModel)
	}
	return false
}

func isStructuredModelNotFoundError(message, requestedModel string) bool {
	var payload any
	if errJSON := json.Unmarshal([]byte(strings.TrimSpace(message)), &payload); errJSON != nil {
		return false
	}
	return containsStructuredModelNotFound(payload, requestedModel)
}

func containsStructuredModelNotFound(value any, requestedModel string) bool {
	switch typed := value.(type) {
	case map[string]any:
		notFoundType := false
		exactModelReference := false
		for key, item := range typed {
			text, isString := item.(string)
			if isString {
				switch strings.ToLower(strings.TrimSpace(key)) {
				case "code":
					if isModelNotFoundIdentifier(text) {
						return true
					}
				case "type":
					if isModelNotFoundIdentifier(text) {
						return true
					}
					notFoundType = notFoundType || isNotFoundErrorIdentifier(text)
				case "error", "message", "detail", "error_description", "title":
					if isExplicitModelNotFoundMessage(text, requestedModel) {
						return true
					}
					exactModelReference = exactModelReference || isExactRequestedModelReference(text, requestedModel)
				}
			}
			switch item.(type) {
			case map[string]any, []any:
				if containsStructuredModelNotFound(item, requestedModel) {
					return true
				}
			}
		}
		return notFoundType && exactModelReference
	case []any:
		for _, item := range typed {
			if text, isString := item.(string); isString && isExplicitModelNotFoundMessage(text, requestedModel) {
				return true
			}
			if containsStructuredModelNotFound(item, requestedModel) {
				return true
			}
		}
	}
	return false
}

func isModelNotFoundIdentifier(value string) bool {
	candidate := strings.ToLower(strings.TrimSpace(value))
	if fragment := strings.LastIndex(candidate, "#"); fragment >= 0 && fragment+1 < len(candidate) {
		candidate = candidate[fragment+1:]
	} else {
		if query := strings.Index(candidate, "?"); query >= 0 {
			candidate = candidate[:query]
		}
		candidate = strings.TrimRight(candidate, "/")
		if separator := strings.LastIndexAny(candidate, "/:"); separator >= 0 {
			candidate = candidate[separator+1:]
		}
	}
	normalized := strings.NewReplacer("-", "_", " ", "_").Replace(candidate)
	switch normalized {
	case "model_not_found", "model_not_found_error", "unknown_model", "model_does_not_exist", "model_not_exist":
		return true
	default:
		return false
	}
}

func isNotFoundErrorIdentifier(value string) bool {
	normalized := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(value)))
	return normalized == "not_found" || normalized == "not_found_error"
}

func isExplicitModelNotFoundMessage(message, requestedModel string) bool {
	lower := strings.Trim(strings.ToLower(strings.TrimSpace(message)), " .!;\t\r\n")
	if lower == "" {
		return false
	}
	if strings.Contains(lower, "in request") || strings.Contains(lower, "in body") || strings.Contains(lower, "request body") {
		return false
	}
	normalized := strings.NewReplacer("-", "_").Replace(lower)
	if strings.Contains(normalized, "model_not_found") || strings.Contains(normalized, "unknown_model") {
		return true
	}
	for _, prefix := range []string{"no such model", "unknown model"} {
		if lower != prefix && !strings.HasPrefix(lower, prefix+" ") && !strings.HasPrefix(lower, prefix+":") {
			continue
		}
		remainder := strings.TrimSpace(strings.TrimPrefix(lower, prefix))
		remainder = strings.TrimSpace(strings.TrimPrefix(remainder, ":"))
		if remainder == "" {
			return true
		}
		missingSuffix, matches := trimRequestedModelReference(remainder, requestedModel)
		return matches && missingSuffix == ""
	}
	for _, prefix := range []string{"the requested model", "requested model", "the model", "model"} {
		if lower != prefix && !strings.HasPrefix(lower, prefix+" ") && !strings.HasPrefix(lower, prefix+":") {
			continue
		}
		remainder := strings.TrimSpace(strings.TrimPrefix(lower, prefix))
		remainder = strings.TrimSpace(strings.TrimPrefix(remainder, ":"))
		if isMissingModelPhrase(remainder) {
			return true
		}
		missingSuffix, matches := trimRequestedModelReference(remainder, requestedModel)
		return matches && isMissingModelPhrase(missingSuffix)
	}
	return false
}

func isExactRequestedModelReference(message, requestedModel string) bool {
	lower := strings.Trim(strings.ToLower(strings.TrimSpace(message)), " .!;\t\r\n")
	for _, prefix := range []string{"the requested model", "requested model", "the model", "model"} {
		if lower != prefix && !strings.HasPrefix(lower, prefix+" ") && !strings.HasPrefix(lower, prefix+":") {
			continue
		}
		remainder := strings.TrimSpace(strings.TrimPrefix(lower, prefix))
		remainder = strings.TrimSpace(strings.TrimPrefix(remainder, ":"))
		suffix, matches := trimRequestedModelReference(remainder, requestedModel)
		return matches && suffix == ""
	}
	return false
}

func trimRequestedModelReference(value, requestedModel string) (string, bool) {
	model := strings.ToLower(strings.TrimSpace(requestedModel))
	if model == "" {
		return "", false
	}
	for _, candidate := range []string{model, "'" + model + "'", `"` + model + `"`, "`" + model + "`"} {
		if value == candidate {
			return "", true
		}
		if !strings.HasPrefix(value, candidate) {
			continue
		}
		remainder := value[len(candidate):]
		if remainder == "" || strings.ContainsRune(" :,", rune(remainder[0])) {
			return strings.TrimLeft(remainder, " :,"), true
		}
	}
	return "", false
}

func isMissingModelPhrase(value string) bool {
	switch strings.Trim(value, " .!;\t\r\n") {
	case "not found", "was not found", "could not be found", "does not exist", "doesn't exist", "not exist", "is unknown", "does not exist or you do not have access to it":
		return true
	default:
		return false
	}
}

// isRequestInvalidError returns true if the error represents a client request
// error that should neither rotate nor penalize credentials. Model-support
// errors remain eligible for alternate routing and keep their model-level state.
func isRequestInvalidError(err error) bool {
	if err == nil {
		return false
	}
	if isStreamTruncatedError(err) {
		// A truncated accepted stream must stop here: the generation was
		// already submitted and must not be replayed on another credential.
		return true
	}
	if isRequestScopedError(err) {
		return true
	}
	// Credential-scoped failures must rotate to another credential, even when
	// their HTTP status would otherwise look like a caller-owned request fault.
	if isCredentialScopedError(err) {
		return false
	}
	if isCloudflareChallengeError(err) {
		return false
	}
	if isInvalidGrantError(err) {
		return false
	}
	if isCreditBalanceTooLowError(err) {
		return false
	}
	if isModelSupportError(err) {
		return false
	}
	status := statusCodeFromError(err)
	if clienterror.IsRequestFault(status, err) {
		return true
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil && authErr.Message != "" {
		// When authErr.Code is non-empty, Error() formats as "Code: Message" which
		// breaks JSON parsing in clienterror. Re-evaluate against the raw Message body.
		if clienterror.IsRequestFault(status, errors.New(authErr.Message)) {
			return true
		}
	}
	return false
}

func applyAuthFailureState(auth *Auth, resultErr *Error, retryAfter *time.Duration, now time.Time, disableCooling bool) (time.Time, time.Time) {
	if auth == nil || shouldSkipCredentialCooldown(resultErr) {
		return time.Time{}, time.Time{}
	}
	seedInitialForceCooldownDeadlines(auth)
	// A legacy standalone record still identifies its cause before a new
	// quota failure can overwrite the only stored error and retry fields.
	if auth.independentCooldown == nil && len(auth.ModelStates) == 0 && auth.Unavailable && auth.NextRetryAfter.After(now) && auth.LastError != nil &&
		!auth.Quota.Exceeded && auth.Quota.Reason == "" && auth.Quota.NextRecoverAt.IsZero() && !isRecoverableQuotaCooldown(auth.Quota, auth.LastError) {
		auth.independentCooldown = &ModelState{
			Status: auth.Status, StatusMessage: auth.StatusMessage, Unavailable: true,
			NextRetryAfter: auth.NextRetryAfter, LastError: cloneError(auth.LastError), UpdatedAt: auth.UpdatedAt,
			forceCooldownUntil: effectiveAuthForceUntil(auth), forceCooldownSeeded: true,
			cooldownRecordedSeq: auth.cooldownRecordedSeq,
		}
	}
	quotaFailure := statusCodeFromResult(resultErr) == http.StatusTooManyRequests &&
		!isTransientRateLimitResultError(resultErr) && !isCloudflareChallengeResultError(resultErr) &&
		!isInvalidGrantResultError(resultErr) && !isCreditBalanceTooLowResultError(resultErr)
	if !quotaFailure {
		// Evaluate the cause without quota or model aggregates, preserving its
		// own retry hint even when a longer quota window is already active.
		independent := &Auth{forceCooldownSeeded: true}
		if state := auth.independentCooldown; state != nil {
			independent.Status = state.Status
			independent.StatusMessage = state.StatusMessage
			independent.Unavailable = state.Unavailable
			independent.NextRetryAfter = state.NextRetryAfter
			independent.LastError = cloneError(state.LastError)
			independent.Quota = state.Quota.Clone()
			independent.forceCooldownUntil = state.forceCooldownUntil
			independent.quotaForceCooldownUntil = state.quotaForceCooldownUntil
			independent.nonQuotaCooldownUntil = state.nonQuotaCooldownUntil
		}
		applyAuthFailureFields(independent, resultErr, retryAfter, now, disableCooling)
		auth.independentCooldown = &ModelState{
			Status: independent.Status, StatusMessage: independent.StatusMessage,
			Unavailable: independent.Unavailable, NextRetryAfter: independent.NextRetryAfter,
			LastError: cloneError(independent.LastError), Quota: independent.Quota.Clone(), UpdatedAt: now,
			forceCooldownUntil: independent.forceCooldownUntil, forceCooldownSeeded: true,
			quotaForceCooldownUntil: independent.quotaForceCooldownUntil, nonQuotaCooldownUntil: independent.nonQuotaCooldownUntil,
		}
	}
	return applyAuthFailureFields(auth, resultErr, retryAfter, now, disableCooling)
}

func applyAuthFailureFields(auth *Auth, resultErr *Error, retryAfter *time.Duration, now time.Time, disableCooling bool) (time.Time, time.Time) {
	if auth == nil {
		return time.Time{}, time.Time{}
	}
	seedInitialForceCooldownDeadlines(auth)
	prevAuthRetryAfter := auth.NextRetryAfter
	prevAuthForceUntil := auth.forceCooldownUntil
	prevAuthQuota := cooldownFieldsOf(auth.Quota)
	prevAuthQuotaSignals := maps.Clone(auth.Quota.cooldownSignals)
	var prevCredQuotaForceUntil time.Time
	if prevAuthQuota.Exceeded && prevAuthQuota.Reason == "credential_quota" && prevAuthQuota.NextRecoverAt.After(now) && auth.credQuotaForceUntil.After(now) {
		prevCredQuotaForceUntil = auth.credQuotaForceUntil
	}
	var ownAuthForceDeadline time.Time
	var ownAuthNonQuotaDeadline time.Time
	isForceAuth := isForceCooldownError(resultErr)
	if shouldSkipCredentialCooldown(resultErr) {
		return time.Time{}, time.Time{}
	}
	defer func() {
		if disableCooling && auth.NextRetryAfter.IsZero() && auth.Quota.NextRecoverAt.IsZero() {
			auth.Unavailable = false
			auth.Quota.Exceeded = false
		}
	}()
	auth.Unavailable = true
	auth.Status = StatusError
	auth.UpdatedAt = now
	if resultErr != nil {
		auth.LastError = cloneError(resultErr)
		if resultErr.Message != "" {
			auth.StatusMessage = resultErr.Message
		}
	}
	statusCode := statusCodeFromResult(resultErr)
	if isTransientRateLimitResultError(resultErr) && statusCode == http.StatusTooManyRequests {
		// Without a model key the transient capacity rejection degrades to an
		// auth-level transient cooldown (issue 7017), never quota exhaustion.
		next := transientRateLimitRetryAfter(now, retryAfter, disableCooling)
		ownAuthNonQuotaDeadline = next
		if isForceAuth {
			ownAuthForceDeadline = next
		}
		if !next.IsZero() && auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(next) {
			next = auth.Quota.NextRecoverAt
		}
		if next.IsZero() && auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(now) {
			next = auth.Quota.NextRecoverAt
		}
		auth.NextRetryAfter = next
		auth.Unavailable = !next.IsZero()
	} else if isCloudflareChallengeResultError(resultErr) {
		auth.StatusMessage = "cloudflare challenge"
		next, backoffLevel := nextCloudflareCooldown(auth.Quota.BackoffLevel, disableCooling, now)
		ownAuthNonQuotaDeadline = next
		applyCooldownFields(&auth.Quota, QuotaState{
			Exceeded:      true,
			Reason:        "cloudflare challenge",
			NextRecoverAt: next,
			BackoffLevel:  backoffLevel,
		})
		auth.NextRetryAfter = next
	} else if isInvalidGrantResultError(resultErr) {
		auth.StatusMessage = "invalid_grant"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(30 * time.Minute)
		}
	} else if isCreditBalanceTooLowResultError(resultErr) {
		auth.StatusMessage = "payment_required"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(30 * time.Minute)
		}
	} else {
		switch statusCode {
		case 401:
			auth.StatusMessage = "unauthorized"
			if disableCooling {
				auth.NextRetryAfter = time.Time{}
			} else {
				auth.NextRetryAfter = now.Add(30 * time.Minute)
			}
		case 402, 403:
			if isTOSViolationResultError(resultErr) {
				// Terminal credential suspension: deactivate so the credential is
				// excluded from selection/rotation, but keep OAuth refresh intact
				// (the suspension is service-scoped per issue #7515; the token and
				// model catalog keep working) so operator tooling can verify
				// account liveness. The appeal detail travels on the result error's
				// Metadata and is persisted onto the credential record below.
				auth.StatusMessage = "tos_violation"
				auth.Status = StatusDisabled
				auth.Disabled = true
				auth.Unavailable = true
				auth.NextRetryAfter = time.Time{}
				auth.LastError = cloneError(resultErr)
				persistTOSAppealMetadata(auth, resultErr)
			} else {
				auth.StatusMessage = "payment_required"
				if disableCooling {
					auth.NextRetryAfter = time.Time{}
				} else {
					auth.NextRetryAfter = now.Add(30 * time.Minute)
				}
			}
		case 404:
			auth.StatusMessage = "not_found"
			if disableCooling {
				auth.NextRetryAfter = time.Time{}
			} else if retryAfter != nil && *retryAfter > 0 {
				auth.NextRetryAfter = now.Add(*retryAfter)
			} else {
				auth.NextRetryAfter = now.Add(12 * time.Hour)
			}
		case 429:
			auth.StatusMessage = "quota exhausted"
			auth.Quota.Exceeded = true
			auth.Quota.Reason = "quota"
			var next time.Time
			if !disableCooling {
				if retryAfter != nil {
					cooldown := *retryAfter
					if cooldown < minQuotaCooldownFloor {
						cooldown = minQuotaCooldownFloor
					}
					next = now.Add(cooldown).Round(0)
					if isForceAuth {
						ownAuthForceDeadline = next
					}
				} else {
					reusedOpenAuthWindow := auth.Quota.NextRecoverAt.After(now)
					baseAuthBackoff := auth.Quota.BackoffLevel
					next, auth.Quota.BackoffLevel = quotaCooldownAfterFailure(auth.Quota, now)
					if isForceAuth {
						if !reusedOpenAuthWindow {
							ownAuthForceDeadline = next
						} else if prevAuthForceUntil.After(now) {
							ownAuthForceDeadline = prevAuthForceUntil
						} else {
							ownAuthForceDeadline, _ = quotaCooldownAfterFailure(QuotaState{BackoffLevel: baseAuthBackoff}, now)
						}
					}
				}
				if auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(next) {
					next = auth.Quota.NextRecoverAt
				}
			}
			auth.Quota.NextRecoverAt = next
			auth.NextRetryAfter = next
		case 408, 500, 502, 503, 504, 520, 521, 522, 523, 524, 525, 526:
			auth.StatusMessage = "transient upstream error"
			auth.NextRetryAfter = recoverableFailureRetryAfterWithResult(now, retryAfter, disableCooling, resultErr)
			auth.Unavailable = !auth.NextRetryAfter.IsZero()
		default:
			if auth.StatusMessage == "" {
				auth.StatusMessage = "request failed"
			}
			auth.NextRetryAfter = recoverableFailureRetryAfter(now, disableCooling)
			auth.Unavailable = !auth.NextRetryAfter.IsZero()
		}
	}
	if isForceAuth && auth.NextRetryAfter.IsZero() {
		auth.NextRetryAfter = now.Add(transientErrorCooldown)
		auth.Unavailable = true
	}
	ownAuthRetryDeadline := auth.NextRetryAfter
	if isForceAuth && ownAuthForceDeadline.After(ownAuthRetryDeadline) {
		ownAuthRetryDeadline = ownAuthForceDeadline
	}
	if !isQuotaCooldownResultError(resultErr) {
		if !ownAuthNonQuotaDeadline.IsZero() || isTransientRateLimitResultError(resultErr) {
			ownAuthRetryDeadline = ownAuthNonQuotaDeadline
		}
		if !isTransientRateLimitResultError(resultErr) && ownAuthNonQuotaDeadline.IsZero() {
			ownAuthNonQuotaDeadline = auth.NextRetryAfter
		}
		if isForceAuth && ownAuthForceDeadline.After(ownAuthNonQuotaDeadline) {
			ownAuthNonQuotaDeadline = ownAuthForceDeadline
		}
		if ownAuthNonQuotaDeadline.After(auth.nonQuotaCooldownUntil) {
			auth.nonQuotaCooldownUntil = ownAuthNonQuotaDeadline
		}
	}
	if isForceAuth {
		if ownAuthForceDeadline.IsZero() {
			ownAuthForceDeadline = auth.NextRetryAfter
		}
		if ownAuthForceDeadline.After(prevAuthForceUntil) {
			auth.forceCooldownUntil = ownAuthForceDeadline
		} else {
			auth.forceCooldownUntil = prevAuthForceUntil
		}
	} else {
		auth.forceCooldownUntil = prevAuthForceUntil
	}
	auth.forceCooldownSeeded = true
	if isForceAuth && isQuotaCooldownResultError(resultErr) && ownAuthForceDeadline.After(auth.quotaForceCooldownUntil) {
		auth.quotaForceCooldownUntil = ownAuthForceDeadline
	}
	if prevCredQuotaForceUntil.After(now) {
		if prevCredQuotaForceUntil.After(auth.credQuotaForceUntil) {
			auth.credQuotaForceUntil = prevCredQuotaForceUntil
		}
		if !auth.Quota.Exceeded || auth.Quota.Reason != "credential_quota" || auth.Quota.NextRecoverAt.Before(prevCredQuotaForceUntil) {
			restored := prevAuthQuota
			if disableCooling && restored.NextRecoverAt.After(prevCredQuotaForceUntil) {
				restored.NextRecoverAt = prevCredQuotaForceUntil
			} else if !disableCooling && auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(restored.NextRecoverAt) {
				restored.NextRecoverAt = auth.Quota.NextRecoverAt
			}
			restored.cooldownSignals = prevAuthQuotaSignals
			applyCooldownFields(&auth.Quota, restored)
		}
	}
	activeAuthForce := auth.forceCooldownUntil
	if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.credQuotaForceUntil.After(activeAuthForce) {
		activeAuthForce = auth.credQuotaForceUntil
	}
	if activeAuthForce.After(now) {
		if auth.NextRetryAfter.Before(activeAuthForce) {
			auth.NextRetryAfter = activeAuthForce
		}
		auth.Unavailable = true
		if (!auth.Quota.Exceeded || auth.Quota.NextRecoverAt.IsZero()) && prevAuthQuota.Exceeded && prevAuthQuota.NextRecoverAt.After(now) {
			restored := prevAuthQuota
			if disableCooling && restored.NextRecoverAt.After(activeAuthForce) {
				restored.NextRecoverAt = activeAuthForce
			}
			restored.cooldownSignals = prevAuthQuotaSignals
			applyCooldownFields(&auth.Quota, restored)
		}
	}
	// A later failure only extends a still-live credential cooldown; a
	// deliberate zero write (disableCooling) still clears it.
	if !auth.NextRetryAfter.IsZero() && prevAuthRetryAfter.After(auth.NextRetryAfter) && prevAuthRetryAfter.After(now) {
		auth.NextRetryAfter = prevAuthRetryAfter
	}
	if !isForceAuth {
		return time.Time{}, ownAuthRetryDeadline
	}
	return ownAuthForceDeadline, ownAuthRetryDeadline
}

// quotaCooldownAfterFailure returns the recovery deadline and backoff level for
// a quota failure observed at now. Failures that land while a previous quota
// window is still open reuse that window instead of escalating, so a burst of
// concurrent in-flight failures advances the backoff ladder at most once per
// window.
func quotaCooldownAfterFailure(quota QuotaState, now time.Time) (time.Time, int) {
	if quota.NextRecoverAt.After(now) {
		return quota.NextRecoverAt, quota.BackoffLevel
	}
	cooldown, nextLevel := nextQuotaCooldown(quota.BackoffLevel, false)
	var next time.Time
	if cooldown > 0 {
		next = now.Add(cooldown).Round(0)
	}
	return next, nextLevel
}

// nextQuotaCooldown returns the next cooldown duration and updated backoff level for repeated quota errors.
func nextQuotaCooldown(prevLevel int, disableCooling bool) (time.Duration, int) {
	if prevLevel < 0 {
		prevLevel = 0
	}
	if disableCooling {
		return 0, prevLevel
	}
	cooldown := quotaBackoffBase * time.Duration(1<<prevLevel)
	if cooldown < quotaBackoffBase {
		cooldown = quotaBackoffBase
	}
	if cooldown >= quotaBackoffMax {
		return quotaBackoffMax, prevLevel
	}
	return cooldown, prevLevel + 1
}
