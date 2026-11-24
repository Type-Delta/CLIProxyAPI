package management

import (
	"context"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// An armed manual reset spends one Claude reset grant or Codex reset credit on
// the operator's behalf. CPAMC arms it by setting the auth-file metadata flag
// below through PATCH /v8/management/credentials/fields. It fires when the
// normal weekly limit (manualResetTriggerBars) is spent or held by a reserve no
// configured client key can bypass, provided the reset refills that bar, or when
// a usable grant or credit expires within manualResetExpiryLead. Shorter,
// monthly and model-specific limits never trigger it. It is one-shot: CPA durably clears the flag before it
// sends the claim and never re-arms, whatever the provider answers.
const (
	manualResetArmedKey   = "manual_reset_armed"
	manualResetArmTick    = time.Minute
	manualResetExpiryLead = 15 * time.Minute
)

// manualResetTriggerBars are the normal weekly bars: Codex "weekly" and Claude
// "seven-day". Only these can trigger an armed reset.
var manualResetTriggerBars = map[string]bool{"weekly": true, "seven-day": true}

// codexManualResetBars are the standard-pool bars a Codex reset credit refills.
var codexManualResetBars = []string{"five-hour", "weekly", "monthly"}

// claudeManualResetBars maps a grant's "clears" windows to reserve bar IDs.
var claudeManualResetBars = map[string]string{
	"five_hour":                  "five-hour",
	"seven_day":                  "seven-day",
	"seven_day_overage_included": "seven-day-fable",
}

type manualResetOffer struct {
	grantID   string // Claude only; Codex consumes whichever credit it chooses.
	expiresAt time.Time
	refills   []string
}

func manualResetArmed(auth *coreauth.Auth) bool {
	if auth == nil || auth.Metadata == nil {
		return false
	}
	armed, _ := auth.Metadata[manualResetArmedKey].(bool)
	return armed
}

func manualResetSupported(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	return provider == "claude" || provider == "codex"
}

// lockManualResetClaim serializes reset spending per credential: panel claims
// through the API-call proxy and the armed check, which holds it from its usage
// read to its claim, so neither acts on a reading the other has already spent.
func (h *Handler) lockManualResetClaim(authID string) func() {
	value, _ := h.manualResetClaims.LoadOrStore(authID, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// quotaReserveBypassable reports whether a configured client key can still
// bypass the credential's reserves. Stale key IDs on the list do not count.
func (h *Handler) quotaReserveBypassable(auth *coreauth.Auth) bool {
	bypass := coreauth.QuotaReserveBypassKeysForAuth(auth)
	if h == nil || len(bypass) == 0 {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		return false
	}
	for _, entry := range h.cfg.APIKeys {
		if slices.Contains(bypass, config.APIKeyID(entry.Key)) {
			return true
		}
	}
	return false
}

// RunManualResetArmer checks armed credentials every minute until ctx ends.
// Reads go through the shared quota cache, so each armed credential costs at
// most one upstream read per cached URL per cache lifetime. Home mode is
// skipped (and refuses arming): several CPA nodes sharing a credential would
// each spend a reset.
func (h *Handler) RunManualResetArmer(ctx context.Context) {
	if h == nil || h.authManager == nil {
		return
	}
	ticker := time.NewTicker(manualResetArmTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if h.authManager.HomeEnabled() {
			continue
		}
		for _, auth := range h.authManager.List() {
			if ctx.Err() != nil {
				return
			}
			if manualResetArmed(auth) && manualResetSupported(auth) && !auth.Disabled && auth.Status != coreauth.StatusDisabled {
				h.checkArmedManualReset(ctx, auth)
			}
		}
	}
}

// checkArmedManualReset reads current usage and reset offers, then claims one
// offer when a trigger holds.
func (h *Handler) checkArmedManualReset(ctx context.Context, auth *coreauth.Auth) {
	defer h.lockManualResetClaim(auth.ID)()
	now := time.Now()
	var (
		bars   []coreauth.QuotaReserveBar
		offers []manualResetOffer
	)
	codexHeaders := cloneStringMap(quotaReserveCodexHeaders)
	if accountID := codexQuotaReserveAccountID(auth); accountID != "" {
		codexHeaders["Chatgpt-Account-Id"] = accountID
	}
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		// The grant status read carries the usage windows too.
		body, errRead := h.quotaReserveFetch(ctx, auth, claudeResetGrantsQuotaURL, quotaReserveClaudeHeaders)
		if errRead != nil {
			log.WithError(errRead).WithField("auth_id", auth.ID).Debug("armed manual reset: Claude status read failed")
			return
		}
		bars, _ = claudeQuotaReserveBars(body)
		offers = claudeManualResetOffers(body, now)
	case "codex":
		var errRead error
		bars, errRead = h.QuotaReserveBars(ctx, auth)
		if errRead != nil {
			log.WithError(errRead).WithField("auth_id", auth.ID).Debug("armed manual reset: Codex usage read failed")
			return
		}
		body, errCredits := h.quotaReserveFetch(ctx, auth, codexResetCreditsURL, codexHeaders)
		if errCredits != nil {
			log.WithError(errCredits).WithField("auth_id", auth.ID).Debug("armed manual reset: Codex credit read failed")
			return
		}
		offers = codexManualResetOffers(body, now)
	}
	var hits []coreauth.QuotaReserveHit
	if state, held := h.authManager.QuotaReserveState(auth.ID); held && !h.quotaReserveBypassable(auth) {
		hits = state.Hits
	}
	blocked := manualResetBlockedBars(bars, hits, now)
	offer, ok := pickManualResetOffer(offers, blocked, now)
	if !ok {
		// Explains a wait, e.g. a Claude grant that needs the account at its limit.
		log.WithFields(log.Fields{"auth_id": auth.ID, "offers": len(offers), "blocked_bars": len(blocked)}).Debug("armed manual reset: waiting")
		return
	}

	var send func() string
	if offer.grantID != "" {
		profile, errProfile := h.quotaReserveFetch(ctx, auth, claudeProfileQuotaURL, quotaReserveClaudeHeaders)
		organization := strings.ToLower(ojson.GetBytes(profile, "organization.uuid").String())
		// Only the canonical 36-character form; uuid.Validate also accepts URN and brace forms.
		if errProfile != nil || len(organization) != 36 || uuid.Validate(organization) != nil {
			log.WithField("auth_id", auth.ID).Debug("armed manual reset: Claude organization unavailable")
			return
		}
		send = func() string { return h.claimClaudeResetGrant(ctx, auth, organization, offer.grantID) }
	} else {
		send = func() string { return h.consumeCodexResetCredit(ctx, auth, codexHeaders) }
	}
	// Consume the arm durably before spending: the save fails if a disarm or a
	// disable landed during the reads, or if the store cannot record it, so a
	// restart can never reload an arm that already spent.
	stillArmed := func(current *coreauth.Auth) bool {
		return manualResetArmed(current) && !current.Disabled && current.Status != coreauth.StatusDisabled
	}
	disarmed, _ := h.authManager.PersistMetadataEntry(ctx, auth.ID, auth.RegistrationEpoch, manualResetArmedKey, false, stillArmed)
	if !disarmed {
		log.WithField("auth_id", auth.ID).Warn("armed manual reset: could not consume the arm; nothing was sent")
		return
	}
	log.WithFields(log.Fields{"auth_id": auth.ID, "provider": auth.Provider, "result": send()}).Info("armed manual reset sent; disarmed")
}

// claimClaudeResetGrant sends the cedar_ember claim through executeAPICall, so
// the same reconciliation as a panel claim refreshes routing afterwards. It
// returns the upstream answer for the log.
func (h *Handler) claimClaudeResetGrant(ctx context.Context, auth *coreauth.Auth, organization, grantID string) string {
	doc := ojson.NewDocString(`{}`)
	_ = doc.Set("program", "cedar_ember")
	_ = doc.Set("grant_id", grantID)
	_ = doc.Set("request_id", uuid.NewString())
	target := "https://api.anthropic.com/api/organizations/" + organization + "/reset_rate_limits"
	outcome := h.executeAPICall(ctx, http.MethodPost, target, "", auth, "", cloneStringMap(quotaReserveClaudeHeaders), doc.String())
	return manualResetResult(outcome, "result")
}

func (h *Handler) consumeCodexResetCredit(ctx context.Context, auth *coreauth.Auth, headers map[string]string) string {
	doc := ojson.NewDocString(`{}`)
	_ = doc.Set("redeem_request_id", uuid.NewString())
	outcome := h.executeAPICall(ctx, http.MethodPost, codexConsumeURL, "", auth, "", headers, doc.String())
	return manualResetResult(outcome, "code")
}

// manualResetResult names a claim answer for logs: the provider's result code,
// or the HTTP status when it gave none.
func manualResetResult(outcome quotaCallOutcome, field string) string {
	if !outcome.hasResponse {
		return "no response: " + outcome.outerError
	}
	if code := ojson.Get(outcome.response.Body, field).String(); code != "" && outcome.successfulResponse() {
		return code
	}
	return "HTTP " + strconv.Itoa(outcome.response.StatusCode)
}

// manualResetBlockedBars lists the weekly trigger bars no client can use: spent
// ones, plus the given reserve hits (callers pass hits only when nobody can
// bypass them).
func manualResetBlockedBars(bars []coreauth.QuotaReserveBar, hits []coreauth.QuotaReserveHit, now time.Time) map[string]bool {
	blocked := make(map[string]bool)
	for _, bar := range bars {
		if manualResetTriggerBars[bar.ID] && bar.RemainingPercent <= 0 && (bar.ResetAt.IsZero() || bar.ResetAt.After(now)) {
			blocked[bar.ID] = true
		}
	}
	for _, hit := range hits {
		if manualResetTriggerBars[hit.BarID] {
			blocked[hit.BarID] = true
		}
	}
	return blocked
}

// pickManualResetOffer returns the soonest-expiring offer that refills a
// blocked bar or would otherwise expire within manualResetExpiryLead.
func pickManualResetOffer(offers []manualResetOffer, blocked map[string]bool, now time.Time) (manualResetOffer, bool) {
	sorted := slices.Clone(offers)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i].expiresAt, sorted[j].expiresAt
		return !a.IsZero() && (b.IsZero() || a.Before(b))
	})
	for _, offer := range sorted {
		if !offer.expiresAt.IsZero() && offer.expiresAt.Sub(now) <= manualResetExpiryLead {
			return offer, true
		}
		for _, bar := range offer.refills {
			if blocked[bar] {
				return offer, true
			}
		}
	}
	return manualResetOffer{}, false
}

// manualResetTime reads an RFC 3339 string or a Unix seconds/milliseconds number.
func manualResetTime(value ojson.Result) time.Time {
	if value.Type == ojson.Number {
		number := value.Float()
		if number <= 0 {
			return time.Time{}
		}
		if number < 1e11 {
			return time.Unix(0, int64(number*float64(time.Second))).UTC()
		}
		return time.UnixMilli(int64(number)).UTC()
	}
	parsed, errParse := time.Parse(time.RFC3339, strings.TrimSpace(value.String()))
	if errParse != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

// claudeManualResetOffers returns the grants the claim endpoint would accept
// now, mirroring CPAMC's anthropicResetGrantBlocker.
func claudeManualResetOffers(body []byte, now time.Time) []manualResetOffer {
	root := ojson.GetBytes(body, "cedar_ember")
	if !root.Get("eligible").Bool() {
		return nil
	}
	if cooldown := manualResetTime(root.Get("cooldown_until")); cooldown.After(now) {
		return nil
	}
	atLimit := root.Get("at_limit").Bool()
	var offers []manualResetOffer
	for _, grant := range root.Get("grants").Array() {
		id := grant.Get("id").String()
		requiresLimit := !grant.Get("use_requires_limit").Exists() || grant.Get("use_requires_limit").Bool()
		if !validClaudeResetID(id, 40, false) || grant.Get("resets_left").Int() <= 0 || grant.Get("paused").Bool() ||
			!grant.Get("usable_now").Bool() || (requiresLimit && !atLimit) || claudeGrantHasNoUsage(grant) {
			continue
		}
		if startsAt := manualResetTime(grant.Get("starts_at")); startsAt.After(now) {
			continue
		}
		expiresAt := manualResetTime(grant.Get("ends_at"))
		if !expiresAt.IsZero() && !expiresAt.After(now) {
			continue
		}
		offer := manualResetOffer{grantID: id, expiresAt: expiresAt}
		for _, window := range grant.Get("clears").Array() {
			if bar, ok := claudeManualResetBars[window.String()]; ok {
				offer.refills = append(offer.refills, bar)
			}
		}
		offers = append(offers, offer)
	}
	return offers
}

// claudeGrantHasNoUsage matches CPAMC's resetGrantHasNoUsage: a weekly
// give-back grant with every reported window at 0% would refill nothing.
func claudeGrantHasNoUsage(grant ojson.Result) bool {
	if !grant.Get("gives_back_weekly").Bool() {
		return false
	}
	percents := grant.Get("percent_used")
	if !percents.IsObject() {
		return false
	}
	seen := false
	allZero := true
	percents.ForEach(func(_, value ojson.Result) bool {
		seen = true
		if value.Float() != 0 {
			allZero = false
		}
		return true
	})
	return seen && allZero
}

// codexManualResetOffers returns available Codex rate-limit reset credits.
func codexManualResetOffers(body []byte, now time.Time) []manualResetOffer {
	var offers []manualResetOffer
	for _, credit := range ojson.GetBytes(body, "credits").Array() {
		resetType := credit.Get("reset_type").String()
		if resetType == "" {
			resetType = credit.Get("resetType").String()
		}
		if resetType != "codex_rate_limits" || credit.Get("status").String() != "available" {
			continue
		}
		expires := credit.Get("expires_at")
		if !expires.Exists() {
			expires = credit.Get("expiresAt")
		}
		expiresAt := manualResetTime(expires)
		if !expiresAt.IsZero() && !expiresAt.After(now) {
			continue
		}
		offers = append(offers, manualResetOffer{expiresAt: expiresAt, refills: codexManualResetBars})
	}
	return offers
}
