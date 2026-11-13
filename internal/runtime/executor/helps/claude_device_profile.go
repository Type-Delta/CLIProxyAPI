package helps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	homekv "github.com/router-for-me/CLIProxyAPI/v8/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	defaultClaudeFingerprintUserAgent      = "claude-cli/2.1.321 (external, cli)"
	defaultClaudeFingerprintPackageVersion = "0.128.0"
	defaultClaudeFingerprintRuntimeVersion = "v26.12.1"
	defaultClaudeFingerprintOS             = "MacOS"
	defaultClaudeFingerprintArch           = "arm64"
)

var (
	claudeCLIVersionPattern     = regexp.MustCompile(`(?i)^claude-cli/(\d+)\.(\d+)\.(\d+)`)
	claudePackageVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	claudeRuntimeVersionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

	claudeDeviceProfileCache   = make(map[string]claudeDeviceProfileCacheEntry)
	claudeDeviceProfileCacheMu sync.RWMutex

	ClaudeDeviceProfileBeforeCandidateStore func(ClaudeDeviceProfile)
)

type claudeDeviceProfileKVClient interface {
	KVGet(ctx context.Context, key string) ([]byte, bool, error)
}

var currentClaudeDeviceProfileKVClient = func() (claudeDeviceProfileKVClient, bool, error) {
	return kvstore.CurrentKVClient()
}

type claudeCLIVersion struct {
	major int
	minor int
	patch int
}

func (v claudeCLIVersion) Compare(other claudeCLIVersion) int {
	switch {
	case v.major != other.major:
		if v.major > other.major {
			return 1
		}
		return -1
	case v.minor != other.minor:
		if v.minor > other.minor {
			return 1
		}
		return -1
	case v.patch != other.patch:
		if v.patch > other.patch {
			return 1
		}
		return -1
	default:
		return 0
	}
}

type ClaudeDeviceProfile struct {
	DeviceID       string
	UserAgent      string
	PackageVersion string
	RuntimeVersion string
	OS             string
	Arch           string
	version        claudeCLIVersion
	hasVersion     bool
	fallback       bool
}

type claudeDeviceProfileCacheEntry struct {
	profile ClaudeDeviceProfile
}

type claudeDeviceProfileKVValue struct {
	DeviceID       string `json:"device_id,omitempty"`
	UserAgent      string `json:"user_agent"`
	PackageVersion string `json:"package_version"`
	RuntimeVersion string `json:"runtime_version"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	Fallback       bool   `json:"fallback,omitempty"`
}

func ClaudeSignatureStabilizationEnabled(cfg *config.Config) bool {
	if cfg == nil || cfg.ClaudeHeaderDefaults.StabilizeDeviceProfile == nil {
		return false
	}
	return *cfg.ClaudeHeaderDefaults.StabilizeDeviceProfile
}

// ClaudeDeviceProfileStabilizationEnabled is the deprecated name of
// ClaudeSignatureStabilizationEnabled.
func ClaudeDeviceProfileStabilizationEnabled(cfg *config.Config) bool {
	return ClaudeSignatureStabilizationEnabled(cfg)
}

func ResetClaudeDeviceProfileCache() {
	claudeDeviceProfileCacheMu.Lock()
	claudeDeviceProfileCache = make(map[string]claudeDeviceProfileCacheEntry)
	claudeSignatureMemoryOwners = make(map[string]string)
	claudeDeviceProfileCacheMu.Unlock()
}

func MapStainlessOS() string {
	return mapStainlessOS()
}

func MapStainlessArch() string {
	return mapStainlessArch()
}

func defaultClaudeDeviceProfile(cfg *config.Config) ClaudeDeviceProfile {
	hdrDefault := func(cfgVal, fallback string) string {
		if strings.TrimSpace(cfgVal) != "" {
			return strings.TrimSpace(cfgVal)
		}
		return fallback
	}

	var hd config.ClaudeHeaderDefaults
	if cfg != nil {
		hd = cfg.ClaudeHeaderDefaults
	}

	profile := ClaudeDeviceProfile{
		UserAgent:      hdrDefault(hd.UserAgent, defaultClaudeFingerprintUserAgent),
		PackageVersion: hdrDefault(hd.PackageVersion, defaultClaudeFingerprintPackageVersion),
		RuntimeVersion: hdrDefault(hd.RuntimeVersion, defaultClaudeFingerprintRuntimeVersion),
		OS:             hdrDefault(hd.OS, defaultClaudeFingerprintOS),
		Arch:           hdrDefault(hd.Arch, defaultClaudeFingerprintArch),
		fallback:       true,
	}
	if parsed, ok := parseClaudeCodeUserAgent(profile.UserAgent); ok {
		profile.UserAgent = parsed.profileUserAgent
		profile.version = parsed.version
		profile.hasVersion = true
	}
	return profile
}

// mapStainlessOS maps runtime.GOOS to Stainless SDK OS names.
func mapStainlessOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	case "freebsd":
		return "FreeBSD"
	default:
		return "Other::" + runtime.GOOS
	}
}

// mapStainlessArch maps runtime.GOARCH to Stainless SDK architecture names.
func mapStainlessArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	case "386":
		return "x86"
	default:
		return "other::" + runtime.GOARCH
	}
}

func parseClaudeCLIVersion(userAgent string) (claudeCLIVersion, bool) {
	matches := claudeCLIVersionPattern.FindStringSubmatch(strings.TrimSpace(userAgent))
	if len(matches) != 4 {
		return claudeCLIVersion{}, false
	}
	major, err := strconv.Atoi(matches[1])
	if err != nil {
		return claudeCLIVersion{}, false
	}
	minor, err := strconv.Atoi(matches[2])
	if err != nil {
		return claudeCLIVersion{}, false
	}
	patch, err := strconv.Atoi(matches[3])
	if err != nil {
		return claudeCLIVersion{}, false
	}
	return claudeCLIVersion{major: major, minor: minor, patch: patch}, true
}

func shouldUpgradeClaudeDeviceProfile(candidate, current ClaudeDeviceProfile) bool {
	next, okNext := parseClaudeCodeUserAgent(candidate.UserAgent)
	previous, okPrevious := parseClaudeCodeUserAgent(current.UserAgent)
	if !okNext {
		return false
	}
	if !okPrevious && !current.fallback {
		return false
	}
	if okPrevious && ((!current.fallback && next.entrypoint != previous.entrypoint) || next.version.Compare(previous.version) < 0) {
		return false
	}
	changed := !okPrevious || next.version.Compare(previous.version) > 0
	for _, pair := range [][2]string{{next.agentSDKVersion, previous.agentSDKVersion}, {candidate.PackageVersion, current.PackageVersion}, {strings.TrimPrefix(candidate.RuntimeVersion, "v"), strings.TrimPrefix(current.RuntimeVersion, "v")}} {
		if pair[0] == pair[1] {
			continue
		}
		// An SDK cannot disappear once the client's tuple includes it.
		if pair[0] == "" {
			return false
		}
		if pair[1] == "" {
			changed = true
			continue
		}
		v, okV := parseClaudeSemver(pair[0])
		old, okOld := parseClaudeSemver(pair[1])
		if !okV {
			return false
		}
		if !okOld {
			if !current.fallback {
				return false
			}
			changed = true
			continue
		}
		if v.Compare(old) < 0 {
			return false
		}
		changed = changed || v.Compare(old) > 0
	}
	return changed || current.fallback
}

func parseClaudeSemver(raw string) (claudeCLIVersion, bool) {
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) != 3 {
		return claudeCLIVersion{}, false
	}
	major, errMajor := strconv.Atoi(parts[0])
	if errMajor != nil {
		return claudeCLIVersion{}, false
	}
	minor, errMinor := strconv.Atoi(parts[1])
	if errMinor != nil {
		return claudeCLIVersion{}, false
	}
	patch, errPatch := strconv.Atoi(parts[2])
	if errPatch != nil {
		return claudeCLIVersion{}, false
	}
	return claudeCLIVersion{major: major, minor: minor, patch: patch}, true
}

func pinClaudeDeviceProfilePlatform(profile, baseline ClaudeDeviceProfile) ClaudeDeviceProfile {
	profile.DeviceID = baseline.DeviceID
	profile.OS = baseline.OS
	profile.Arch = baseline.Arch
	return profile
}

// Learned profiles retain their device and tuple when fallback defaults change.
// A profile still using defaults may adopt a coherent configuration upgrade.
func normalizeClaudeDeviceProfile(profile, baseline ClaudeDeviceProfile) ClaudeDeviceProfile {
	if profile.OS == "" {
		profile.OS = baseline.OS
	}
	if profile.Arch == "" {
		profile.Arch = baseline.Arch
	}
	if profile.fallback {
		current, validCurrent := parseClaudeCodeUserAgent(profile.UserAgent)
		next, validNext := parseClaudeCodeUserAgent(baseline.UserAgent)
		if validCurrent && validNext && current.entrypoint == next.entrypoint && shouldUpgradeClaudeDeviceProfile(baseline, profile) {
			return pinClaudeDeviceProfilePlatform(baseline, profile)
		}
	}
	return profile
}

func extractClaudeDeviceProfile(headers http.Header, cfg *config.Config) (ClaudeDeviceProfile, bool) {
	parsed, ok := parseClaudeCodeUserAgent(headerValue(headers, "User-Agent"))
	if !ok || !isNativeClaudeEntrypoint(parsed.entrypoint, cfg) {
		return ClaudeDeviceProfile{}, false
	}
	packageVersion := headerValue(headers, "X-Stainless-Package-Version")
	runtimeVersion := headerValue(headers, "X-Stainless-Runtime-Version")
	// A partial observation cannot establish a coherent software tuple.
	if !claudePackageVersionPattern.MatchString(packageVersion) || !claudeRuntimeVersionPattern.MatchString(runtimeVersion) {
		return ClaudeDeviceProfile{}, false
	}
	baseline := defaultClaudeDeviceProfile(cfg)
	return ClaudeDeviceProfile{UserAgent: parsed.profileUserAgent, PackageVersion: packageVersion, RuntimeVersion: runtimeVersion, OS: baseline.OS, Arch: baseline.Arch, version: parsed.version, hasVersion: true}, true
}

func claudeDeviceProfileScopeKey(auth *cliproxyauth.Auth, apiKey string) string {
	return claudeSignatureCanonicalScope(auth, apiKey)
}

func claudeDeviceProfileLegacyScopeKey(auth *cliproxyauth.Auth, apiKey string) string {
	switch {
	case auth != nil && strings.TrimSpace(auth.ID) != "":
		return "auth:" + strings.TrimSpace(auth.ID)
	case strings.TrimSpace(apiKey) != "":
		return "api_key:" + strings.TrimSpace(apiKey)
	default:
		return "global"
	}
}

// All native entrypoints on a credential share one stored client identity.
func claudeDeviceProfileScopedKey(auth *cliproxyauth.Auth, apiKey string, _ ClaudeDeviceProfile, _ ...*config.Config) string {
	return claudeDeviceProfileScopeKey(auth, apiKey)
}

func claudeDeviceProfileCacheKey(auth *cliproxyauth.Auth, apiKey string, profile ClaudeDeviceProfile, configs ...*config.Config) string {
	sum := sha256.Sum256([]byte(claudeDeviceProfileScopedKey(auth, apiKey, profile, configs...)))
	return hex.EncodeToString(sum[:])
}

func claudeDeviceProfileKVKey(auth *cliproxyauth.Auth, apiKey string, profile ClaudeDeviceProfile, configs ...*config.Config) string {
	return "cpa:claude:device-profile:" + homekv.HashKeyPart(claudeDeviceProfileScopedKey(auth, apiKey, profile, configs...))
}

// The bounded memory fallback lives for this process, without time expiration.
// Persistent local/Home KV should be enabled when identity must survive restarts
// or more than 4096 simultaneously retained credential profiles. At capacity,
// required resolution refuses new identities and retains existing profiles.
func ResolveClaudeDeviceProfile(auth *cliproxyauth.Auth, apiKey string, headers http.Header, cfg *config.Config) ClaudeDeviceProfile {
	profile, errProfile := ResolveClaudeDeviceProfileRequired(context.Background(), auth, apiKey, headers, cfg)
	if errProfile != nil {
		return defaultClaudeDeviceProfile(cfg)
	}
	return profile
}

// ResolveClaudeDeviceProfileRequired accepts headers only from a confirmed native
// caller. Required persistent storage failures must stop request construction.
func ResolveClaudeDeviceProfileRequired(ctx context.Context, auth *cliproxyauth.Auth, apiKey string, headers http.Header, cfg *config.Config) (ClaudeDeviceProfile, error) {
	client, persistent, errClient := currentClaudeDeviceProfileKVClient()
	if persistent {
		if errClient != nil {
			return ClaudeDeviceProfile{}, errClient
		}
		if client == nil {
			return ClaudeDeviceProfile{}, fmt.Errorf("device profile KV unavailable")
		}
		return resolveClaudeDeviceProfileHome(ctx, client, auth, apiKey, headers, cfg)
	}
	return resolveClaudeDeviceProfileLocal(auth, apiKey, headers, cfg)
}

func resolveClaudeDeviceProfileLocal(auth *cliproxyauth.Auth, apiKey string, headers http.Header, cfg *config.Config) (ClaudeDeviceProfile, error) {
	baseline := defaultClaudeDeviceProfile(cfg)
	candidate, hasCandidate := extractClaudeDeviceProfile(headers, cfg)
	if hasCandidate && ClaudeDeviceProfileBeforeCandidateStore != nil {
		ClaudeDeviceProfileBeforeCandidateStore(candidate)
	}
	key := claudeDeviceProfileCacheKey(auth, apiKey, ClaudeDeviceProfile{})
	claudeDeviceProfileCacheMu.Lock()
	defer claudeDeviceProfileCacheMu.Unlock()
	entry, found := claudeDeviceProfileCache[key]
	profile := baseline
	if found {
		profile = normalizeClaudeDeviceProfile(entry.profile, baseline)
	}
	if hasCandidate && (!found || shouldUpgradeClaudeDeviceProfile(candidate, profile)) {
		if found {
			candidate = pinClaudeDeviceProfilePlatform(candidate, profile)
		}
		profile = candidate
	}
	if !found && len(claudeDeviceProfileCache) >= 4096 {
		return ClaudeDeviceProfile{}, fmt.Errorf("device profile memory capacity reached; enable persistent KV storage")
	}
	deviceID, errDevice := claimClaudeSignatureDevice(context.Background(), nil, auth, apiKey, profile.DeviceID)
	if errDevice != nil {
		return ClaudeDeviceProfile{}, errDevice
	}
	profile.DeviceID = deviceID
	claudeDeviceProfileCache[key] = claudeDeviceProfileCacheEntry{profile: profile}
	return profile, nil
}

type claudeDeviceProfileCASClient interface {
	KVCompareAndSwap(context.Context, string, []byte, bool, []byte, time.Duration) (bool, error)
}

// Before consolidation, non-CLI clients used entrypoint-specific keys. When
// no primary profile exists, migrate one deterministically rather than replacing
// its valid observed tuple with the fallback baseline.
func readLegacyClaudeDeviceProfile(ctx context.Context, client claudeDeviceProfileKVClient, auth *cliproxyauth.Auth, apiKey string, cfg *config.Config) ([]byte, bool, error) {
	legacyScope := claudeDeviceProfileLegacyScopeKey(auth, apiKey)
	if legacyScope != claudeDeviceProfileScopeKey(auth, apiKey) {
		key := "cpa:claude:device-profile:" + homekv.HashKeyPart(legacyScope)
		raw, found, err := client.KVGet(ctx, key)
		if err != nil || found {
			return raw, found, err
		}
	}

	entrypoints := make(map[string]bool)
	for entrypoint := range nativeClaudeEntrypoints {
		if entrypoint != "cli" {
			entrypoints[entrypoint] = true
		}
	}
	if cfg != nil {
		for _, entrypoint := range cfg.ClaudeHeaderDefaults.NativeEntrypoints {
			entrypoints[strings.ToLower(strings.TrimSpace(entrypoint))] = true
		}
		if cfg.ClaudeHeaderDefaults.OAuthSafeguard {
			for _, entrypoint := range cfg.ClaudeHeaderDefaults.OAuthSafeguardClientIdentities {
				entrypoints[strings.ToLower(strings.TrimSpace(entrypoint))] = true
			}
		}
	}
	names := make([]string, 0, len(entrypoints))
	for name := range entrypoints {
		if name != "" && name != "cli" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		key := "cpa:claude:device-profile:" + homekv.HashKeyPart(legacyScope+"|subclient:"+name)
		raw, found, err := client.KVGet(ctx, key)
		if err != nil || found {
			return raw, found, err
		}
	}
	return nil, false, nil
}

func decodeClaudeDeviceProfile(raw []byte) (ClaudeDeviceProfile, error) {
	var value claudeDeviceProfileKVValue
	if err := json.Unmarshal(raw, &value); err != nil {
		return ClaudeDeviceProfile{}, fmt.Errorf("decode device profile: %w", err)
	}
	profile := value.ToProfile()
	_, validNative := parseClaudeCodeUserAgent(profile.UserAgent)
	validTuple := validNative && claudePackageVersionPattern.MatchString(profile.PackageVersion) && claudeRuntimeVersionPattern.MatchString(profile.RuntimeVersion)
	validFallback := profile.fallback && profile.UserAgent != "" && profile.PackageVersion != "" && profile.RuntimeVersion != ""
	if !validTuple && !validFallback {
		return ClaudeDeviceProfile{}, fmt.Errorf("invalid persisted device profile")
	}
	return profile, nil
}

func resolveClaudeDeviceProfileHome(ctx context.Context, client claudeDeviceProfileKVClient, auth *cliproxyauth.Auth, apiKey string, headers http.Header, cfg *config.Config) (ClaudeDeviceProfile, error) {
	cas, ok := client.(claudeDeviceProfileCASClient)
	if !ok {
		return ClaudeDeviceProfile{}, fmt.Errorf("device profile persistence requires atomic compare-and-swap: %w", kvstore.ErrCompareAndSwapUnsupported)
	}
	baseline := defaultClaudeDeviceProfile(cfg)
	candidate, hasCandidate := extractClaudeDeviceProfile(headers, cfg)
	if hasCandidate && ClaudeDeviceProfileBeforeCandidateStore != nil {
		ClaudeDeviceProfileBeforeCandidateStore(candidate)
	}
	key := claudeDeviceProfileKVKey(auth, apiKey, ClaudeDeviceProfile{})
	for {
		if errContext := ctx.Err(); errContext != nil {
			return ClaudeDeviceProfile{}, errContext
		}
		raw, found, errGet := client.KVGet(ctx, key)
		if errGet != nil {
			return ClaudeDeviceProfile{}, errGet
		}
		profile := baseline
		storedRaw, storedFound := raw, found
		if !found {
			var errLegacy error
			storedRaw, storedFound, errLegacy = readLegacyClaudeDeviceProfile(ctx, client, auth, apiKey, cfg)
			if errLegacy != nil {
				return ClaudeDeviceProfile{}, errLegacy
			}
		}
		if storedFound {
			var errDecode error
			profile, errDecode = decodeClaudeDeviceProfile(storedRaw)
			if errDecode != nil {
				return ClaudeDeviceProfile{}, errDecode
			}
			profile = normalizeClaudeDeviceProfile(profile, baseline)
		}
		// A copy can initialize the canonical scope before the original filename's
		// historical profile is encountered. Reconcile that known legacy primary on
		// subsequent requests too, without replacing the canonical client or device.
		legacyScope := claudeDeviceProfileLegacyScopeKey(auth, apiKey)
		if found && legacyScope != claudeDeviceProfileScopeKey(auth, apiKey) {
			var legacyRaw []byte
			var legacyFound bool
			var errLegacy error
			if migrating, _ := ctx.Value(claudeSignatureMigrationContextKey{}).(bool); migrating {
				legacyRaw, legacyFound, errLegacy = readLegacyClaudeDeviceProfile(ctx, client, auth, apiKey, cfg)
			} else {
				legacyKey := "cpa:claude:device-profile:" + homekv.HashKeyPart(legacyScope)
				legacyRaw, legacyFound, errLegacy = client.KVGet(ctx, legacyKey)
			}
			if errLegacy != nil {
				return ClaudeDeviceProfile{}, errLegacy
			}
			if legacyFound {
				legacy, errDecode := decodeClaudeDeviceProfile(legacyRaw)
				if errDecode != nil {
					return ClaudeDeviceProfile{}, errDecode
				}
				if shouldUpgradeClaudeDeviceProfile(legacy, profile) {
					profile = pinClaudeDeviceProfilePlatform(legacy, profile)
				}
			}
		}

		if hasCandidate && (!storedFound || shouldUpgradeClaudeDeviceProfile(candidate, profile)) {
			if storedFound {
				candidate = pinClaudeDeviceProfilePlatform(candidate, profile)
			}
			profile = candidate
		}
		deviceID, errDevice := claimClaudeSignatureDevice(ctx, client, auth, apiKey, profile.DeviceID)
		if errDevice != nil {
			return ClaudeDeviceProfile{}, errDevice
		}
		profile.DeviceID = deviceID
		encoded, errEncode := json.Marshal(claudeDeviceProfileKVValueFromProfile(profile))
		if errEncode != nil {
			return ClaudeDeviceProfile{}, errEncode
		}
		// CAS even an unchanged value: it removes legacy TTLs and verifies that a
		// concurrent winner did not supersede the snapshot being returned.
		swapped, errCAS := cas.KVCompareAndSwap(ctx, key, raw, found, encoded, 0)
		if errCAS != nil {
			return ClaudeDeviceProfile{}, fmt.Errorf("persist device profile: %w", errCAS)
		}
		if swapped {
			return profile, nil
		}
	}
}

func claudeDeviceProfileKVValueFromProfile(profile ClaudeDeviceProfile) claudeDeviceProfileKVValue {
	return claudeDeviceProfileKVValue{
		DeviceID:       profile.DeviceID,
		UserAgent:      profile.UserAgent,
		PackageVersion: profile.PackageVersion,
		RuntimeVersion: profile.RuntimeVersion,
		OS:             profile.OS,
		Arch:           profile.Arch,
		Fallback:       profile.fallback,
	}
}

func (value claudeDeviceProfileKVValue) ToProfile() ClaudeDeviceProfile {
	profile := ClaudeDeviceProfile{
		DeviceID:       strings.TrimSpace(value.DeviceID),
		UserAgent:      strings.TrimSpace(value.UserAgent),
		PackageVersion: strings.TrimSpace(value.PackageVersion),
		RuntimeVersion: strings.TrimSpace(value.RuntimeVersion),
		OS:             strings.TrimSpace(value.OS),
		Arch:           strings.TrimSpace(value.Arch),
		fallback:       value.Fallback,
	}
	if parsed, ok := parseClaudeCodeUserAgent(profile.UserAgent); ok {
		profile.UserAgent = parsed.profileUserAgent
		profile.version = parsed.version
		profile.hasVersion = true
	}
	return profile
}

// ApplyClaudeDeviceProfileHeaders emits the resolved software tuple. Body billing
// cannot select software versions; the confirmed caller supplies request fields only.
func ApplyClaudeDeviceProfileHeaders(r *http.Request, profile ClaudeDeviceProfile, body []byte, callerUserAgent string) {
	if r == nil {
		return
	}
	profile.UserAgent = ClaudeDeviceProfileUserAgent(profile, callerUserAgent)

	for _, headerName := range []string{
		"User-Agent",
		"X-Stainless-Package-Version",
		"X-Stainless-Runtime-Version",
		"X-Stainless-Os",
		"X-Stainless-Arch",
	} {
		r.Header.Del(headerName)
	}
	r.Header.Set("User-Agent", profile.UserAgent)
	r.Header.Set("X-Stainless-Package-Version", profile.PackageVersion)
	r.Header.Set("X-Stainless-Runtime-Version", profile.RuntimeVersion)
	r.Header.Set("X-Stainless-Os", profile.OS)
	r.Header.Set("X-Stainless-Arch", profile.Arch)
}

// DefaultClaudeVersion returns the version string (e.g. "2.1.321") from the
// current baseline device profile. It extracts the version from the User-Agent.
func DefaultClaudeVersion(cfg *config.Config) string {
	profile := defaultClaudeDeviceProfile(cfg)
	if version, ok := parseClaudeCLIVersion(profile.UserAgent); ok {
		return strconv.Itoa(version.major) + "." + strconv.Itoa(version.minor) + "." + strconv.Itoa(version.patch)
	}
	return "2.1.321"
}

func ApplyClaudeDefaultDeviceProfileHeaders(r *http.Request, cfg *config.Config, body []byte) {
	ApplyClaudeDeviceProfileHeaders(r, defaultClaudeDeviceProfile(cfg), body, "")
}

func ApplyClaudeLegacyDeviceHeaders(r *http.Request, ginHeaders http.Header, cfg *config.Config, confirmedClaudeCode bool, keepNewerVersions ...bool) {
	if r == nil {
		return
	}
	profile := defaultClaudeDeviceProfile(cfg)
	miscEnsure := func(name, fallback string, valid func(string) bool) {
		if incoming := strings.TrimSpace(headerValue(ginHeaders, name)); incoming != "" && (valid == nil || valid(incoming)) {
			r.Header.Set(name, incoming)
			return
		}
		if current := strings.TrimSpace(r.Header.Get(name)); current != "" && (valid == nil || valid(current)) {
			return
		}

		r.Header.Set(name, fallback)
	}

	if confirmedClaudeCode {
		validRuntime := func(value string) bool {
			return claudeRuntimeVersionPattern.MatchString(value)
		}
		validPackage := func(value string) bool {
			return claudePackageVersionPattern.MatchString(value)
		}
		miscEnsure("X-Stainless-Runtime-Version", profile.RuntimeVersion, validRuntime)
		miscEnsure("X-Stainless-Package-Version", profile.PackageVersion, validPackage)
		miscEnsure("X-Stainless-Os", mapStainlessOS(), nil)
		miscEnsure("X-Stainless-Arch", mapStainlessArch(), nil)
		if clientUA := strings.TrimSpace(ginHeaders.Get("User-Agent")); plausibleClaudeCodeUserAgent(clientUA, cfg) {
			r.Header.Set("User-Agent", clientUA)
			return
		}
	}

	// Unconfirmed clients must not leak a copied or third-party software profile
	// into the upstream Claude Code SDK fingerprint.
	r.Header.Set("X-Stainless-Runtime-Version", profile.RuntimeVersion)
	r.Header.Set("X-Stainless-Package-Version", profile.PackageVersion)
	r.Header.Set("X-Stainless-Os", profile.OS)
	r.Header.Set("X-Stainless-Arch", profile.Arch)
	r.Header.Set("User-Agent", profile.UserAgent)
}
