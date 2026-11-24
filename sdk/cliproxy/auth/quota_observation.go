package auth

import (
	"context"
	"time"
)

type quotaObservationKey struct{}
type quotaObservation struct {
	manager           *Manager
	authID            string
	epoch             uint64
	registrationEpoch uint64
	credentialVersion uint64
}
type quotaResetBarsKey struct{}

// CaptureQuotaObservation marks the start of a provider quota request. Existing
// markers are retained so nested probes cannot refresh a stale observation.
func (m *Manager) CaptureQuotaObservation(ctx context.Context, authID string) context.Context {
	return m.captureQuotaObservation(ctx, authID, nil)
}

// CaptureQuotaObservationForAuth ties a probe to the credential snapshot
// supplied to its transport, even if a replacement is already registered.
func (m *Manager) CaptureQuotaObservationForAuth(ctx context.Context, auth *Auth) context.Context {
	if auth == nil {
		return m.CaptureQuotaObservation(ctx, "")
	}
	return m.captureQuotaObservation(ctx, auth.ID, auth)
}

func (m *Manager) captureQuotaObservation(ctx context.Context, authID string, source *Auth) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if m == nil || authID == "" {
		return ctx
	}
	if observation, ok := ctx.Value(quotaObservationKey{}).(quotaObservation); ok && observation.manager == m && observation.authID == authID {
		return ctx
	}
	m.mu.RLock()
	observation := quotaObservation{manager: m, authID: authID, epoch: m.quotaObservationEpochs[authID]}
	if source == nil {
		source = m.auths[authID]
	}
	if source != nil {
		observation.registrationEpoch = source.RegistrationEpoch
		observation.credentialVersion = source.CredentialVersion
	}
	m.mu.RUnlock()
	return context.WithValue(ctx, quotaObservationKey{}, observation)
}

// QuotaObservationEpoch identifies the current generation of cached quota reads.
func (m *Manager) QuotaObservationEpoch(authID string) uint64 {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.quotaObservationEpochs[authID]
}

func (m *Manager) quotaObservationCurrentLocked(ctx context.Context, authID string) bool {
	if ctx == nil {
		return true
	}
	observation, ok := ctx.Value(quotaObservationKey{}).(quotaObservation)
	if !ok || observation.manager != m || observation.authID != authID {
		return true
	}
	auth := m.auths[authID]
	return auth != nil && observation.epoch == m.quotaObservationEpochs[authID] &&
		observation.registrationEpoch == auth.RegistrationEpoch && observation.credentialVersion == auth.CredentialVersion
}

func (m *Manager) advanceQuotaObservationEpochLocked(authID string) {
	if m.quotaObservationEpochs == nil {
		m.quotaObservationEpochs = make(map[string]uint64)
	}
	m.quotaObservationEpochs[authID]++
}

func (m *Manager) beginQuotaResetLocked(ctx context.Context, authID string) {
	m.advanceQuotaObservationEpochLocked(authID)
	barIDs, _ := ctx.Value(quotaResetBarsKey{}).([]string)
	m.InvalidateQuotaReserveObservation(authID, barIDs)
}

// ApplyProviderQuotaReset reconciles a successful upstream reset and rejects
// observations that started before it. Nil modelIDs or barIDs target all pools.
func (m *Manager) ApplyProviderQuotaReset(ctx context.Context, authID string, modelIDs, barIDs []string) (bool, error) {
	return m.applyProviderQuotaReset(ctx, authID, modelIDs, barIDs, true)
}

// ApplyProviderQuotaResetForModels invalidates pre-reset observations and refilled
// windows without releasing an independently exhausted credential-wide pool.
// Pass empty, non-nil lists when the reset's affected pools are unknown.
func (m *Manager) ApplyProviderQuotaResetForModels(ctx context.Context, authID string, modelIDs, barIDs []string) (bool, error) {
	return m.applyProviderQuotaReset(ctx, authID, modelIDs, barIDs, false)
}

func (m *Manager) applyProviderQuotaReset(ctx context.Context, authID string, modelIDs, barIDs []string, recoverCredential bool) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, quotaResetBarsKey{}, barIDs)
	return m.applyProviderQuotaReport(ctx, authID, false, time.Time{}, modelIDs, recoverCredential, true)
}
