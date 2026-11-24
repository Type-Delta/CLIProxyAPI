package management

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Claim replay protection covers the panel's ambiguous-operation retry window.
const claudeResetRetryWindow = 10 * time.Minute

// Grant evidence lives only in the existing quota response cache. Missing,
// expired or ambiguous metadata never implies a credential-wide refill.
type claudeResetGrantScope struct {
	fiveHour bool
	weekly   bool
	fable    bool
}

type claudeResetGrantCatalog struct {
	epoch  uint64
	grants map[string]claudeResetGrantScope
}

type claudeResetReconcileKey struct{}

func parseClaudeResetGrantCatalog(body []byte, epoch uint64) *claudeResetGrantCatalog {
	if !ojson.ValidBytes(body) {
		return nil
	}
	grants := ojson.GetBytes(body, "cedar_ember.grants")
	if !grants.IsArray() || len(grants.Array()) > 64 {
		return nil
	}
	catalog := &claudeResetGrantCatalog{epoch: epoch, grants: make(map[string]claudeResetGrantScope)}
	for _, grant := range grants.Array() {
		id := grant.Get("id").String()
		if grant.Get("id").Type != ojson.String || !validClaudeResetID(id, 40, false) {
			return nil
		}
		if _, duplicate := catalog.grants[id]; duplicate {
			return nil
		}
		clears := grant.Get("clears")
		if !clears.IsArray() {
			return nil
		}
		scope := claudeResetGrantScope{}
		for _, window := range clears.Array() {
			switch window.String() {
			case "five_hour":
				scope.fiveHour = true
			case "seven_day":
				scope.weekly = true
			case "seven_day_overage_included":
				scope.fable = true
			}
		}
		catalog.grants[id] = scope
	}
	return catalog
}

func validClaudeResetID(value string, max int, uppercase bool) bool {
	if len(value) == 0 || len(value) > max {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '_' || char == '-' || uppercase && char >= 'A' && char <= 'Z' {
			continue
		}
		return false
	}
	return true
}

func (q *apiCallQuotaCache) claudeResetScope(authHash, data string, epoch uint64) claudeResetGrantScope {
	if !ojson.Valid(data) || ojson.Get(data, "program").String() != "cedar_ember" {
		return claudeResetGrantScope{}
	}
	id := ojson.Get(data, "grant_id").String()
	q.mu.Lock()
	defer q.mu.Unlock()
	q.purgeExpiredLocked(q.currentTime())
	var scope claudeResetGrantScope
	var observedAt time.Time
	found := false
	for _, entry := range q.entries {
		catalog := entry.outcome.claudeResetGrants
		if entry.authHash != authHash || catalog == nil || catalog.epoch != epoch {
			continue
		}
		next := catalog.grants[id]
		if !found || entry.outcome.cachedAt.After(observedAt) {
			scope, observedAt, found = next, entry.outcome.cachedAt, true
		} else if entry.outcome.cachedAt.Equal(observedAt) {
			// Equally fresh, conflicting catalogs prove only their overlap.
			scope.fiveHour = scope.fiveHour && next.fiveHour
			scope.weekly = scope.weekly && next.weekly
			scope.fable = scope.fable && next.fable
		}
	}
	return scope
}

// rememberClaudeReset bounds duplicate-claim suppression to 128 recent request
// digests and the panel retry window. It records no credentials or durable state.
func (q *apiCallQuotaCache) rememberClaudeReset(authID, data string) bool {
	id := ojson.Get(data, "request_id").String()
	if !validClaudeResetID(id, 64, true) {
		return true
	}
	key := digestQuotaKey(authID + ":" + id)
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.currentTime()
	if expiry := q.claudeResetRequests[key]; now.Before(expiry) {
		return false
	}
	for request, expiry := range q.claudeResetRequests {
		if !now.Before(expiry) {
			delete(q.claudeResetRequests, request)
		}
	}
	if q.claudeResetRequests == nil {
		q.claudeResetRequests = make(map[string]time.Time)
	}
	if len(q.claudeResetRequests) >= 128 {
		var oldest string
		for request, expiry := range q.claudeResetRequests {
			if oldest == "" || expiry.Before(q.claudeResetRequests[oldest]) {
				oldest = request
			}
		}
		delete(q.claudeResetRequests, oldest)
	}
	q.claudeResetRequests[key] = now.Add(claudeResetRetryWindow)
	return true
}

func (h *Handler) reconcileClaudeReset(ctx context.Context, auth *coreauth.Auth, scope claudeResetGrantScope, result, data, proxy string, headers map[string]string) {
	if h.authManager == nil {
		return
	}
	if result == "not_limited" {
		// No grant was spent. Only the selected grant's windows are known
		// healthy; a Fable-only grant says nothing about shared weekly quota.
		if scope.fiveHour && scope.weekly {
			_, errRecover := h.authManager.ApplyProviderQuotaRecovery(ctx, auth.ID, []string{})
			h.logClaudeResetError(auth, errRecover)
		}
		return
	}
	if result != "reset" && result != "already_used" {
		return
	}
	cache := h.getQuotaCache()
	if !cache.rememberClaudeReset(auth.ID, data) {
		return
	}
	if result == "reset" {
		bars := make([]string, 0, 3)
		if scope.fiveHour {
			bars = append(bars, "five-hour")
		}
		if scope.weekly {
			bars = append(bars, "seven-day")
		}
		if scope.fable {
			bars = append(bars, "seven-day-fable")
		}
		base := scope.fiveHour && scope.weekly
		models := h.quotaModelsForPool(auth, func(id string) bool {
			if strings.Contains(strings.ToLower(id), "fable") {
				return scope.fable
			}
			return base
		})
		var errReset error
		if base {
			_, errReset = h.authManager.ApplyProviderQuotaReset(ctx, auth.ID, models, bars)
		} else {
			_, errReset = h.authManager.ApplyProviderQuotaResetForModels(ctx, auth.ID, models, bars)
		}
		h.logClaudeResetError(auth, errReset)
	}
	cache.invalidateAuth(quotaAuthIdentityHash(auth))
	if result == "reset" && scope.fiveHour && scope.weekly {
		return
	}
	// A Fable refund may also make shared weekly quota usable. Fetch through
	// the common cache/backoff boundary with an observation captured after the
	// reset. The claim's context already carries the superseded observation.
	readCtx := context.WithValue(context.Background(), claudeResetReconcileKey{}, scope)
	target, _ := url.Parse(claudeUsageQuotaURL)
	key := buildQuotaCacheKey(h, auth, http.MethodGet, target, proxy, headers, "")
	// Reconciliation must lead its own observation rather than join a normal
	// refresh whose broader model recovery could clear an unrelated refusal.
	key.requestHash += ":claude-reset-reconcile"
	cache.do(readCtx, key, func(leaderCtx context.Context) quotaCallOutcome {
		return h.executeAPICall(leaderCtx, http.MethodGet, claudeUsageQuotaURL, auth.Index, auth, proxy, headers, "")
	})
}

func (h *Handler) logClaudeResetError(auth *coreauth.Auth, err error) {
	if err != nil {
		log.WithError(err).WithField("auth_id", auth.ID).Warn("failed to reconcile Claude quota after manual reset")
	}
}
