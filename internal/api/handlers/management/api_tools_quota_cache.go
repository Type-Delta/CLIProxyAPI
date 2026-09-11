package management

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	quotaResultCacheTTL = time.Minute
	quota429FallbackTTL = 3 * time.Minute
)

type quotaRequestKind uint8

const (
	quotaRequestNotCacheable quotaRequestKind = iota
	quotaRequestCacheable
	quotaRequestCodexConsume
)

// quotaCacheKey contains only digests. Raw credentials, headers, and proxy
// URLs never become map keys.
type quotaCacheKey struct {
	requestHash string
	authHash    string
	backoffHash string
}

type quotaCallOutcome struct {
	response           apiCallResponse
	cachedAt           time.Time
	hasResponse        bool
	outerStatus        int
	outerError         string
	rateLimited        bool
	retryAfter         time.Duration
	retryAfterProvided bool
	uncacheable        bool
	callerCanceled     bool
}

func (o quotaCallOutcome) clone() quotaCallOutcome {
	copyOutcome := o
	copyOutcome.response.Header = cloneResponseHeader(o.response.Header)
	return copyOutcome
}

func (o quotaCallOutcome) successfulResponse() bool {
	return o.hasResponse && o.response.StatusCode >= http.StatusOK && o.response.StatusCode < http.StatusMultipleChoices
}

type quotaCacheEntry struct {
	authHash  string
	expiresAt time.Time
	outcome   quotaCallOutcome
}

type quotaFlight struct {
	authHash    string
	done        chan struct{}
	outcome     quotaCallOutcome
	expiresAt   time.Time
	invalidated bool
}

type quotaBackoff struct {
	authHash  string
	expiresAt time.Time
	outcome   quotaCallOutcome
}

// apiCallQuotaCache deduplicates the provider-facing requests made by the
// management quota page. It is owned by one Handler and is safe for concurrent
// requests from multiple management clients.
type apiCallQuotaCache struct {
	mu       sync.Mutex
	now      func() time.Time
	entries  map[string]quotaCacheEntry
	flights  map[string]*quotaFlight
	backoffs map[string]quotaBackoff
}

func newAPICallQuotaCache(now func() time.Time) *apiCallQuotaCache {
	if now == nil {
		now = time.Now
	}
	return &apiCallQuotaCache{
		now:      now,
		entries:  make(map[string]quotaCacheEntry),
		flights:  make(map[string]*quotaFlight),
		backoffs: make(map[string]quotaBackoff),
	}
}

func (q *apiCallQuotaCache) currentTime() time.Time {
	if q == nil || q.now == nil {
		return time.Now()
	}
	return q.now()
}

// do returns a cached result, waits for an existing request, or runs fn as
// the single leader. The leader uses a context without the caller's
// cancellation so a disconnected caller cannot strand the shared flight or
// prevent a successful result from being cached for other clients.
func (q *apiCallQuotaCache) do(ctx context.Context, key quotaCacheKey, fn func(context.Context) quotaCallOutcome) quotaCallOutcome {
	if ctx == nil {
		ctx = context.Background()
	}
	if fn == nil {
		return quotaCallOutcome{
			outerStatus: http.StatusBadGateway,
			outerError:  "request failed",
		}
	}
	if q == nil || key.requestHash == "" {
		if errContext := ctx.Err(); errContext != nil {
			return quotaCallOutcome{callerCanceled: true}
		}
		return fn(ctx)
	}
	if errContext := ctx.Err(); errContext != nil {
		return quotaCallOutcome{callerCanceled: true}
	}

	q.mu.Lock()
	q.ensureMapsLocked()
	now := q.currentTime()
	q.purgeExpiredLocked(now)

	if backoff, ok := q.backoffs[key.backoffHash]; ok && now.Before(backoff.expiresAt) {
		outcome := backoff.outcome.clone()
		q.mu.Unlock()
		return q.replayOutcome(outcome, backoff.expiresAt, now)
	}

	if entry, ok := q.entries[key.requestHash]; ok && now.Before(entry.expiresAt) {
		outcome := entry.outcome.clone()
		q.mu.Unlock()
		return q.replayOutcome(outcome, entry.expiresAt, now)
	}

	if flight, ok := q.flights[key.requestHash]; ok {
		q.mu.Unlock()
		select {
		case <-flight.done:
			return q.replayOutcome(flight.outcome.clone(), flight.expiresAt, q.currentTime())
		case <-ctx.Done():
			return quotaCallOutcome{callerCanceled: true}
		}
	}

	flight := &quotaFlight{
		authHash: key.authHash,
		done:     make(chan struct{}),
	}
	q.flights[key.requestHash] = flight
	q.mu.Unlock()

	// Keep the provider operation alive after the leader's HTTP request is
	// canceled. The existing HTTP client behavior still governs its timeout.
	leaderCtx := context.WithoutCancel(ctx)
	outcome := quotaCallOutcome{}
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.WithField("panic", recovered).Error("management quota request panicked")
				outcome = quotaCallOutcome{
					outerStatus: http.StatusBadGateway,
					outerError:  "request failed",
					uncacheable: true,
				}
			}
		}()
		outcome = fn(leaderCtx)
	}()

	now = q.currentTime()
	outcome.cachedAt = now
	q.mu.Lock()
	if currentFlight, ok := q.flights[key.requestHash]; ok && currentFlight == flight {
		delete(q.flights, key.requestHash)
	}
	rateLimited := outcome.rateLimited || (outcome.hasResponse && outcome.response.StatusCode == http.StatusTooManyRequests)
	var retryTTL time.Duration
	if rateLimited {
		retryTTL, _ = q.retryAfterForOutcome(outcome, now)
		flight.expiresAt = now.Add(retryTTL)
	}
	if !flight.invalidated && !outcome.uncacheable && !outcome.callerCanceled {
		ttl := quotaResultCacheTTL
		if rateLimited {
			ttl = retryTTL
		}
		entry := quotaCacheEntry{
			authHash:  key.authHash,
			expiresAt: now.Add(ttl),
			outcome:   outcome.clone(),
		}
		q.entries[key.requestHash] = entry
		flight.expiresAt = entry.expiresAt
	}
	if rateLimited && !outcome.uncacheable && !outcome.callerCanceled {
		backoffTTL := retryTTL
		backoff := quotaBackoff{
			authHash:  key.authHash,
			expiresAt: now.Add(backoffTTL),
			outcome:   outcome.clone(),
		}
		if existing, ok := q.backoffs[key.backoffHash]; ok && existing.expiresAt.After(backoff.expiresAt) {
			backoff = existing
		}
		q.backoffs[key.backoffHash] = backoff
	}
	flight.outcome = outcome.clone()
	close(flight.done)
	q.mu.Unlock()
	return outcome
}

func (q *apiCallQuotaCache) ensureMapsLocked() {
	if q.entries == nil {
		q.entries = make(map[string]quotaCacheEntry)
	}
	if q.flights == nil {
		q.flights = make(map[string]*quotaFlight)
	}
	if q.backoffs == nil {
		q.backoffs = make(map[string]quotaBackoff)
	}
}

func (q *apiCallQuotaCache) purgeExpiredLocked(now time.Time) {
	for hash, entry := range q.entries {
		if !now.Before(entry.expiresAt) {
			delete(q.entries, hash)
		}
	}
	for hash, backoff := range q.backoffs {
		if !now.Before(backoff.expiresAt) {
			delete(q.backoffs, hash)
		}
	}
}

func (q *apiCallQuotaCache) retryAfterForOutcome(outcome quotaCallOutcome, now time.Time) (time.Duration, bool) {
	if outcome.retryAfterProvided {
		return nonNegativeDuration(outcome.retryAfter), true
	}
	if outcome.response.StatusCode == http.StatusTooManyRequests {
		if retryAfter, ok := parseRetryAfter(outcome.response.Header, now); ok {
			return retryAfter, true
		}
	}
	return quota429FallbackTTL, false
}

func nonNegativeDuration(value time.Duration) time.Duration {
	if value < 0 {
		return 0
	}
	return value
}

func (q *apiCallQuotaCache) replayOutcome(outcome quotaCallOutcome, expiresAt, now time.Time) quotaCallOutcome {
	// CPAMC derives provider clock offset from Date. Advance a cached Date by
	// its residence time so refreshing cannot move quota countdowns backwards.
	if outcome.hasResponse && !outcome.cachedAt.IsZero() && now.After(outcome.cachedAt) {
		for key, values := range outcome.response.Header {
			if !strings.EqualFold(key, "Date") || len(values) == 0 {
				continue
			}
			if date, errParse := http.ParseTime(values[0]); errParse == nil {
				outcome.response.Header[key] = []string{date.Add(now.Sub(outcome.cachedAt)).UTC().Format(http.TimeFormat)}
			}
			break
		}
	}
	if !outcome.hasResponse || outcome.response.StatusCode != http.StatusTooManyRequests {
		return outcome
	}
	remaining := expiresAt.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	seconds := int64(remaining / time.Second)
	if remaining > 0 && remaining%time.Second != 0 {
		seconds++
	}
	outcome.response.Header = setRetryAfterHeader(outcome.response.Header, strconv.FormatInt(seconds, 10))
	return outcome
}

// invalidateAuth drops cached quota responses for one credential. In-flight
// calls are allowed to finish, but their result is not admitted after an
// invalidation (for example, after a Codex reset-credit consume succeeds).
func (q *apiCallQuotaCache) invalidateAuth(authHash string) {
	if q == nil || authHash == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ensureMapsLocked()
	for requestHash, entry := range q.entries {
		if entry.authHash == authHash {
			delete(q.entries, requestHash)
		}
	}
	for requestHash, flight := range q.flights {
		if flight.authHash == authHash {
			flight.invalidated = true
			delete(q.flights, requestHash)
		}
	}
}

type apiCallRateLimitError struct {
	retryAfter         time.Duration
	retryAfterProvided bool
}

func (e *apiCallRateLimitError) Error() string {
	return "upstream request rate limited"
}

func quotaRateLimitFromError(err error) (time.Duration, bool, bool) {
	var rateLimitErr *apiCallRateLimitError
	if !errors.As(err, &rateLimitErr) || rateLimitErr == nil {
		return 0, false, false
	}
	return nonNegativeDuration(rateLimitErr.retryAfter), rateLimitErr.retryAfterProvided, true
}

// parseRetryAfter supports both RFC 7231 forms: delta-seconds and HTTP-date.
// A valid date in the past means retry immediately; malformed or overflowing
// values use the caller's fallback.
func parseRetryAfter(headers map[string][]string, now time.Time) (time.Duration, bool) {
	var raw string
	for key, values := range headers {
		if !strings.EqualFold(key, "Retry-After") || len(values) == 0 {
			continue
		}
		raw = strings.TrimSpace(values[0])
		break
	}
	if raw == "" {
		return 0, false
	}
	if isRetryAfterDeltaSeconds(raw) {
		seconds, errParse := strconv.ParseInt(raw, 10, 64)
		if errParse != nil {
			return 0, false
		}
		if seconds > math.MaxInt64/int64(time.Second) {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	when, errParse := http.ParseTime(raw)
	if errParse != nil {
		return 0, false
	}
	if when.Before(now) {
		return 0, true
	}
	return when.Sub(now), true
}

func isRetryAfterDeltaSeconds(raw string) bool {
	if raw == "" {
		return false
	}
	for _, char := range raw {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func setRetryAfterHeader(headers map[string][]string, value string) map[string][]string {
	cloned := cloneResponseHeader(headers)
	if cloned == nil {
		cloned = make(map[string][]string)
	}
	for key := range cloned {
		if strings.EqualFold(key, "Retry-After") {
			cloned[key] = []string{value}
			return cloned
		}
	}
	cloned["Retry-After"] = []string{value}
	return cloned
}

func cloneResponseHeader(headers map[string][]string) map[string][]string {
	if len(headers) == 0 {
		return nil
	}
	cloned := make(map[string][]string, len(headers))
	for key, values := range headers {
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

func quotaAuthIdentityHash(auth *coreauth.Auth) string {
	if auth == nil {
		return digestQuotaKey("quota-auth:none")
	}
	index := strings.TrimSpace(auth.Index)
	if index == "" {
		index = strings.TrimSpace(auth.EnsureIndex())
	}
	var builder strings.Builder
	writeQuotaPart(&builder, "quota-auth-v1")
	writeQuotaPart(&builder, auth.ID)
	writeQuotaPart(&builder, auth.Provider)
	writeQuotaPart(&builder, auth.FileName)
	writeQuotaPart(&builder, index)
	writeQuotaPart(&builder, strconv.FormatUint(auth.RegistrationEpoch, 10))
	writeQuotaStringMap(&builder, auth.Attributes)
	return digestQuotaKey(builder.String())
}

func quotaBackoffHash(authHash, provider string) string {
	var builder strings.Builder
	writeQuotaPart(&builder, "quota-backoff-v1")
	writeQuotaPart(&builder, authHash)
	writeQuotaPart(&builder, strings.ToLower(strings.TrimSpace(provider)))
	return digestQuotaKey(builder.String())
}

func buildQuotaCacheKey(h *Handler, auth *coreauth.Auth, method string, rawURL *url.URL, requestProxyURL string, headers map[string]string, body string) quotaCacheKey {
	authHash := quotaAuthIdentityHash(auth)
	provider := ""
	if auth != nil {
		provider = strings.ToLower(strings.TrimSpace(auth.Provider))
	}
	var builder strings.Builder
	writeQuotaPart(&builder, "quota-request-v1")
	writeQuotaPart(&builder, authHash)
	writeQuotaPart(&builder, strings.ToUpper(strings.TrimSpace(method)))
	if rawURL != nil {
		writeQuotaPart(&builder, rawURL.String())
	}
	writeQuotaPart(&builder, body)
	writeQuotaPart(&builder, strings.TrimSpace(requestProxyURL))
	writeQuotaStringMap(&builder, headers)
	writeQuotaPart(&builder, quotaProxyFingerprint(h, auth, requestProxyURL))
	return quotaCacheKey{
		requestHash: digestQuotaKey(builder.String()),
		authHash:    authHash,
		backoffHash: quotaBackoffHash(authHash, provider),
	}
}

func quotaProxyFingerprint(h *Handler, auth *coreauth.Auth, requestProxyURL string) string {
	var builder strings.Builder
	writeQuotaPart(&builder, strings.TrimSpace(requestProxyURL))
	if auth != nil {
		writeQuotaPart(&builder, strings.TrimSpace(auth.ProxyURL))
	}
	if h != nil && h.cfg != nil {
		writeQuotaPart(&builder, strings.TrimSpace(h.cfg.ProxyURL))
		writeQuotaPart(&builder, strings.TrimSpace(proxyURLFromAPIKeyConfig(h.cfg, auth)))
	}
	return builder.String()
}

func writeQuotaStringMap(builder *strings.Builder, values map[string]string) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := strings.ToLower(keys[i]), strings.ToLower(keys[j])
		if left == right {
			return keys[i] < keys[j]
		}
		return left < right
	})
	for _, key := range keys {
		writeQuotaPart(builder, strings.ToLower(key))
		writeQuotaPart(builder, values[key])
	}
}

func writeQuotaPart(builder *strings.Builder, value string) {
	value = strings.TrimSpace(value)
	builder.WriteString(strconv.Itoa(len(value)))
	builder.WriteByte(':')
	builder.WriteString(value)
	builder.WriteByte('|')
}

func digestQuotaKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

const (
	claudeProfileQuotaURL = "https://api.anthropic.com/api/oauth/profile"
	claudeUsageQuotaURL   = "https://api.anthropic.com/api/oauth/usage"
	codexUsageQuotaURL    = "https://chatgpt.com/backend-api/wham/usage"
	codexResetCreditsURL  = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	codexConsumeURL       = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"
	kimiUsageQuotaURL     = "https://api.kimi.com/coding/v1/usages"
	xaiWeeklyQuotaURL     = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
	xaiMonthlyQuotaURL    = "https://cli-chat-proxy.grok.com/v1/billing"
	xaiProfileQuotaURL    = "https://api.x.ai/v1/me"
	xaiHealthURL          = "https://api.x.ai/v1/chat/completions"
	xaiHealthModel        = "grok-4.5"
	antigravityCodeAssist = "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
)

var antigravityQuotaHosts = map[string]struct{}{
	"daily-cloudcode-pa.googleapis.com":         {},
	"daily-cloudcode-pa.sandbox.googleapis.com": {},
	"cloudcode-pa.googleapis.com":               {},
}

func classifyQuotaRequest(auth *coreauth.Auth, method string, rawURL *url.URL, body string) quotaRequestKind {
	if auth == nil || rawURL == nil {
		return quotaRequestNotCacheable
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	method = strings.ToUpper(strings.TrimSpace(method))
	if strings.EqualFold(strings.TrimSpace(authAttribute(auth, "usage_probe")), "zai") && method == http.MethodGet && exactQuotaURL(rawURL, zaiUsageQuotaURL) {
		return quotaRequestCacheable
	}
	switch provider {
	case "claude":
		if method == http.MethodGet && exactQuotaURL(rawURL, claudeProfileQuotaURL, claudeUsageQuotaURL) {
			return quotaRequestCacheable
		}
	case "codex":
		if method == http.MethodGet && exactQuotaURL(rawURL, codexUsageQuotaURL, codexResetCreditsURL) {
			return quotaRequestCacheable
		}
		if method == http.MethodPost && exactQuotaURL(rawURL, codexConsumeURL) {
			return quotaRequestCodexConsume
		}
	case "kimi":
		if method == http.MethodGet && exactQuotaURL(rawURL, kimiUsageQuotaURL) {
			return quotaRequestCacheable
		}
	case "xai":
		if method == http.MethodGet && exactQuotaURL(rawURL, xaiWeeklyQuotaURL, xaiMonthlyQuotaURL, xaiProfileQuotaURL) {
			return quotaRequestCacheable
		}
		if method == http.MethodPost && exactQuotaURL(rawURL, xaiHealthURL) && isXAIHealthProbe(body) {
			return quotaRequestCacheable
		}
	case "antigravity":
		if method == http.MethodPost && exactQuotaURL(rawURL, antigravityCodeAssist) {
			return quotaRequestCacheable
		}
		if method == http.MethodPost && isAntigravityQuotaURL(rawURL) {
			return quotaRequestCacheable
		}
	}
	return quotaRequestNotCacheable
}

func exactQuotaURL(rawURL *url.URL, candidates ...string) bool {
	if rawURL == nil || rawURL.Fragment != "" {
		return false
	}
	for _, candidate := range candidates {
		parsed, errParse := url.Parse(candidate)
		if errParse != nil {
			continue
		}
		if strings.EqualFold(rawURL.Scheme, parsed.Scheme) &&
			strings.EqualFold(rawURL.Host, parsed.Host) &&
			rawURL.EscapedPath() == parsed.EscapedPath() &&
			rawURL.RawQuery == parsed.RawQuery {
			return true
		}
	}
	return false
}

func isAntigravityQuotaURL(rawURL *url.URL) bool {
	if rawURL == nil || !strings.EqualFold(rawURL.Scheme, "https") || rawURL.Fragment != "" {
		return false
	}
	if _, ok := antigravityQuotaHosts[strings.ToLower(rawURL.Host)]; !ok {
		return false
	}
	return rawURL.EscapedPath() == "/v1internal:retrieveUserQuotaSummary" && rawURL.RawQuery == ""
}

func isXAIHealthProbe(body string) bool {
	var payload map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal([]byte(body), &payload); errUnmarshal != nil || len(payload) != 4 {
		return false
	}
	var model string
	if errUnmarshal := json.Unmarshal(payload["model"], &model); errUnmarshal != nil || model != xaiHealthModel {
		return false
	}
	var messages []json.RawMessage
	if errUnmarshal := json.Unmarshal(payload["messages"], &messages); errUnmarshal != nil || len(messages) != 1 {
		return false
	}
	var message map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(messages[0], &message); errUnmarshal != nil || len(message) != 2 {
		return false
	}
	var role, content string
	if errUnmarshal := json.Unmarshal(message["role"], &role); errUnmarshal != nil || role != "user" {
		return false
	}
	if errUnmarshal := json.Unmarshal(message["content"], &content); errUnmarshal != nil || content != "ping" {
		return false
	}
	var maxTokens int
	if errUnmarshal := json.Unmarshal(payload["max_tokens"], &maxTokens); errUnmarshal != nil || maxTokens != 1 {
		return false
	}
	if !bytes.Equal(bytes.TrimSpace(payload["stream"]), []byte("false")) {
		return false
	}
	return true
}

// quotaCallOutcomeFromTokenError turns credential acquisition failures into
// the same generic management response as before while retaining a provider
// rate-limit deadline for the quota cache.
func quotaCallOutcomeFromTokenError(err error) quotaCallOutcome {
	retryAfter, retryAfterProvided, rateLimited := quotaRateLimitFromError(err)
	return quotaCallOutcome{
		outerStatus:        http.StatusBadRequest,
		outerError:         "auth token refresh failed",
		rateLimited:        rateLimited,
		retryAfter:         retryAfter,
		retryAfterProvided: retryAfterProvided,
	}
}
