package helps

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func claudeDeviceHeaders(userAgent string) http.Header {
	return http.Header{"User-Agent": {userAgent}, "X-Stainless-Package-Version": {"0.128.0"}, "X-Stainless-Runtime-Version": {"v26.12.1"}, "X-Stainless-Os": {"Windows"}, "X-Stainless-Arch": {"x64"}}
}

func profileStore(t *testing.T, enabled bool) *kvstore.BoltStore {
	t.Helper()
	store, err := kvstore.OpenBoltStore(filepath.Join(t.TempDir(), "profiles.db"))
	if err != nil {
		t.Fatal(err)
	}
	previous := currentClaudeDeviceProfileKVClient
	currentClaudeDeviceProfileKVClient = func() (claudeDeviceProfileKVClient, bool, error) { return store, enabled, nil }
	ResetClaudeDeviceProfileCache()
	t.Cleanup(func() {
		currentClaudeDeviceProfileKVClient = previous
		ResetClaudeDeviceProfileCache()
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func resolveProfile(t *testing.T, h http.Header, cfg *config.Config) ClaudeDeviceProfile {
	t.Helper()
	p, err := ResolveClaudeDeviceProfileRequired(context.Background(), &cliproxyauth.Auth{ID: "credential"}, "key", h, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClaudeDeviceProfileMonotonicObservedTuples(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "persistent"}[persistent], func(t *testing.T) {
			profileStore(t, persistent)
			first := claudeDeviceHeaders("claude-cli/2.1.318 (external, sdk-ts, agent-sdk/0.3.220)")
			want := resolveProfile(t, first, nil)
			sequence := []struct {
				ua, pkg, node string
				upgrade       bool
			}{
				{"claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.220)", "0.128.0", "v26.12.1", true},
				{first.Get("User-Agent"), "0.128.0", "v26.12.1", false},
				{"claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.221)", "0.128.1", "v26.12.2", true},
				// A higher CLI cannot bring an older SDK or Node with it.
				{"claude-cli/2.1.323 (external, sdk-ts, agent-sdk/0.3.220)", "0.128.1", "v26.12.2", false},
				{"claude-cli/2.1.323 (external, sdk-ts, agent-sdk/0.3.221)", "0.128.0", "v26.12.2", false},
				{"claude-cli/2.1.323 (external, sdk-ts, agent-sdk/0.3.221)", "0.128.1", "v26.12.1", false},
				{"claude-cli/2.1.323 (external, sdk-ts)", "0.128.1", "v26.12.2", false},
				{"claude-cli/3.0.0 (external, sdk-ts, agent-sdk/0.4.0)", "0.129.0", "v27.0.0", true},
				{"claude-cli/3.0.1 (external, cli, agent-sdk/0.4.0)", "0.129.0", "v27.0.0", false},
			}
			for _, item := range sequence {
				h := claudeDeviceHeaders(item.ua)
				h.Set("X-Stainless-Package-Version", item.pkg)
				h.Set("X-Stainless-Runtime-Version", item.node)
				got := resolveProfile(t, h, nil)
				if item.upgrade {
					want.UserAgent = item.ua
					want.PackageVersion = item.pkg
					want.RuntimeVersion = item.node
					want.version, _ = parseClaudeCLIVersion(item.ua)
				}
				if got != want {
					t.Fatalf("candidate %s: got %+v, want %+v", item.ua, got, want)
				}
			}
			newerConfig := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{UserAgent: "claude-cli/4.0.0 (external, cli)", PackageVersion: "1.0.0", RuntimeVersion: "v30.0.0", OS: "Linux", Arch: "x64"}}
			if got := resolveProfile(t, nil, newerConfig); got != want {
				t.Fatalf("config changed stored tuple/device: %+v", got)
			}
		})
	}
}

func TestClaudeDeviceProfileFallbackAndIncompleteObservation(t *testing.T) {
	profileStore(t, true)
	fallback := resolveProfile(t, nil, nil)
	if !fallback.fallback {
		t.Fatal("missing fallback marker")
	}
	older := claudeDeviceHeaders("claude-cli/2.1.279 (external, sdk-ts)")
	if got := resolveProfile(t, older, nil); got != fallback {
		t.Fatalf("fallback downgraded: %+v", got)
	}
	first := claudeDeviceHeaders("claude-cli/2.1.322 (external, sdk-ts)")
	first.Del("X-Stainless-Package-Version")
	if got := resolveProfile(t, first, nil); got != fallback {
		t.Fatal("partial tuple learned")
	}
	first.Set("X-Stainless-Package-Version", "0.128.0")
	established := resolveProfile(t, first, nil)
	if established.UserAgent != first.Get("User-Agent") || established.fallback {
		t.Fatal("first eligible client did not establish identity")
	}
	ResetClaudeDeviceProfileCache()
	if got := resolveProfile(t, nil, nil); got != established {
		t.Fatal("persistent profile lost on memory reset")
	}
	profileStore(t, false)
	if got := resolveProfile(t, older, nil); got.UserAgent != older.Get("User-Agent") {
		t.Fatal("first-ever native caller was forced up to baseline")
	}
}

func TestClaudeDeviceProfileLegacyMigrationPreservesTupleAndRemovesTTL(t *testing.T) {
	store := profileStore(t, true)
	auth := &cliproxyauth.Auth{ID: "credential"}
	key := claudeDeviceProfileKVKey(auth, "key", ClaudeDeviceProfile{})
	legacy := claudeDeviceProfileKVValue{UserAgent: "claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.220)", PackageVersion: "0.128.0", RuntimeVersion: "v26.12.1", OS: "Linux", Arch: "x64"}
	raw, _ := json.Marshal(legacy)
	if _, err := store.KVSet(context.Background(), key, raw, kvstore.KVSetOptions{EX: 7 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	profile := resolveProfile(t, nil, nil)
	if profile.UserAgent != legacy.UserAgent || profile.OS != "Linux" {
		t.Fatalf("migration changed valid profile: %+v", profile)
	}
	ttl, found, err := store.KVTTL(context.Background(), key)
	if err != nil || !found || ttl != 0 {
		t.Fatalf("persistent profile TTL = %v, found %v, error %v", ttl, found, err)
	}
}

type profileFaultStore struct {
	*kvstore.BoltStore
	getErr, casErr error
	beforeCAS      func()
}

func (s *profileFaultStore) KVGet(ctx context.Context, key string) ([]byte, bool, error) {
	if s.getErr != nil {
		return nil, false, s.getErr
	}
	return s.BoltStore.KVGet(ctx, key)
}
func (s *profileFaultStore) KVCompareAndSwap(ctx context.Context, key string, expected []byte, exists bool, value []byte, ttl time.Duration) (bool, error) {
	if s.casErr != nil {
		return false, s.casErr
	}
	if s.beforeCAS != nil {
		s.beforeCAS()
	}
	return s.BoltStore.KVCompareAndSwap(ctx, key, expected, exists, value, ttl)
}

func TestClaudeDeviceProfileRequiredPersistenceFailures(t *testing.T) {
	for _, operation := range []string{"client", "get", "cas", "unsupported", "corrupt"} {
		t.Run(operation, func(t *testing.T) {
			store := profileStore(t, true)
			fault := &profileFaultStore{BoltStore: store}
			sentinel := errors.New("unavailable")
			switch operation {
			case "get":
				fault.getErr = sentinel
			case "cas":
				fault.casErr = sentinel
			case "corrupt":
				key := claudeDeviceProfileKVKey(&cliproxyauth.Auth{ID: "credential"}, "key", ClaudeDeviceProfile{})
				_, _ = store.KVSet(context.Background(), key, []byte(`{"user_agent":"invalid"}`), kvstore.KVSetOptions{})
			}
			currentClaudeDeviceProfileKVClient = func() (claudeDeviceProfileKVClient, bool, error) {
				if operation == "client" {
					return nil, true, sentinel
				}
				if operation == "unsupported" {
					return struct{ claudeDeviceProfileKVClient }{store}, true, nil
				}
				return fault, true, nil
			}
			if _, err := ResolveClaudeDeviceProfileRequired(context.Background(), &cliproxyauth.Auth{ID: "credential"}, "key", claudeDeviceHeaders(defaultClaudeFingerprintUserAgent), nil); err == nil {
				t.Fatal("required KV failure silently fell back")
			}
		})
	}
}

func TestClaudeDeviceProfileCASRereadsConcurrentWinner(t *testing.T) {
	store := profileStore(t, true)
	winnerHeaders := claudeDeviceHeaders("claude-cli/2.1.323 (external, sdk-ts, agent-sdk/0.3.221)")
	winner, _ := extractClaudeDeviceProfile(winnerHeaders, nil)
	winner.DeviceID = claudeSignatureDeviceID(&cliproxyauth.Auth{ID: "credential"}, "key")
	key := claudeDeviceProfileKVKey(&cliproxyauth.Auth{ID: "credential"}, "key", ClaudeDeviceProfile{})
	raw, _ := json.Marshal(claudeDeviceProfileKVValueFromProfile(winner))
	var once sync.Once
	fault := &profileFaultStore{BoltStore: store, beforeCAS: func() {
		once.Do(func() {
			_, err := store.KVSet(context.Background(), key, raw, kvstore.KVSetOptions{})
			if err != nil {
				t.Error(err)
			}
		})
	}}
	currentClaudeDeviceProfileKVClient = func() (claudeDeviceProfileKVClient, bool, error) { return fault, true, nil }
	got := resolveProfile(t, claudeDeviceHeaders("claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.220)"), nil)
	if got != winner {
		t.Fatalf("returned stale snapshot: %+v, winner %+v", got, winner)
	}
}

func TestClaudeDeviceProfileConcurrentCandidates(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "persistent"}[persistent], func(t *testing.T) {
			profileStore(t, persistent)
			// Establish client identity before scheduling reorderings of its software.
			resolveProfile(t, claudeDeviceHeaders("claude-cli/2.1.318 (external, sdk-ts, agent-sdk/0.3.220)"), nil)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for _, version := range []string{"2.1.319", "2.1.322", "2.1.318", "2.1.320"} {
				wg.Add(1)
				go func(v string) {
					defer wg.Done()
					<-start
					_, err := ResolveClaudeDeviceProfileRequired(context.Background(), &cliproxyauth.Auth{ID: "credential"}, "key", claudeDeviceHeaders("claude-cli/"+v+" (external, sdk-ts, agent-sdk/0.3.220)"), nil)
					if err != nil {
						t.Error(err)
					}
				}(version)
			}
			close(start)
			wg.Wait()
			if got := resolveProfile(t, nil, nil); got.UserAgent != "claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.220)" {
				t.Fatalf("concurrent updates downgraded: %+v", got)
			}
		})
	}
}

func TestApplyClaudeDeviceProfileBillingDocExistingFieldsOnly(t *testing.T) {
	p, _ := extractClaudeDeviceProfile(claudeDeviceHeaders("claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.220)"), nil)
	original := `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.318.AbC; cc_entrypoint=cli; cc_is_subagent=true; cch=12345;","cache_control":{"type":"ephemeral"}},{"type":"text","text":"instructions"}]}`
	doc := ojson.NewDocString(original)
	ApplyClaudeDeviceProfileBillingDoc(doc, p, "")
	want := bytes.ReplaceAll([]byte(original), []byte("2.1.318.AbC"), []byte("2.1.322."+ClaudeBillingFingerprint("", "2.1.322")))
	want = bytes.ReplaceAll(want, []byte("cc_entrypoint=cli"), []byte("cc_entrypoint=sdk-ts"))
	if !bytes.Equal(doc.Bytes(), want) {
		t.Fatalf("billing changed other fields: %s", doc.Bytes())
	}
	for _, raw := range []string{`{"messages":[]}`, `{"system":[{"text":"instructions"}]}`, `{"system":[{"text":"x-anthropic-billing-header: cch=12345;"}]}`} {
		doc := ojson.NewDocString(raw)
		ApplyClaudeDeviceProfileBillingDoc(doc, p, "")
		if doc.String() != raw {
			t.Fatalf("restored absent billing: %s", doc.String())
		}
	}
}

func TestApplyClaudeLegacyDeviceHeadersConfirmedPassthrough(t *testing.T) {
	cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{UserAgent: "claude-cli/3.0.0 (external, cli)", PackageVersion: "0.200.0", RuntimeVersion: "v30.0.0"}}
	h := claudeDeviceHeaders("claude-cli/2.1.318 (external, sdk-ts, workload/cron)")
	r := httptest.NewRequest("POST", "/messages", nil)
	ApplyClaudeLegacyDeviceHeaders(r, h, cfg, true)
	for _, name := range []string{"User-Agent", "X-Stainless-Package-Version", "X-Stainless-Runtime-Version", "X-Stainless-Os", "X-Stainless-Arch"} {
		if r.Header.Get(name) != h.Get(name) {
			t.Fatalf("native %s changed", name)
		}
	}
	r = httptest.NewRequest("POST", "/messages", nil)
	ApplyClaudeLegacyDeviceHeaders(r, h, cfg, false)
	if r.Header.Get("User-Agent") != cfg.ClaudeHeaderDefaults.UserAgent {
		t.Fatal("unconfirmed caller leaked tuple")
	}
}

func TestClaudeDeviceProfileMigratesLegacySubclient(t *testing.T) {
	store := profileStore(t, true)
	legacy := claudeDeviceProfileKVValue{UserAgent: "claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.220)", PackageVersion: "0.128.0", RuntimeVersion: "v26.12.1", OS: "Linux", Arch: "x64"}
	oldKey := "cpa:claude:device-profile:" + kvstore.HashKeyPart("auth:credential|subclient:sdk-ts")
	raw, _ := json.Marshal(legacy)
	_, err := store.KVSet(context.Background(), oldKey, raw, kvstore.KVSetOptions{EX: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	got := resolveProfile(t, claudeDeviceHeaders("claude-cli/2.1.318 (external, sdk-ts, agent-sdk/0.3.220)"), nil)
	if got.UserAgent != legacy.UserAgent || got.OS != legacy.OS {
		t.Fatalf("legacy subclient tuple downgraded: %+v", got)
	}
	key := claudeDeviceProfileKVKey(&cliproxyauth.Auth{ID: "credential"}, "key", ClaudeDeviceProfile{})
	if _, found, err := store.KVGet(context.Background(), key); err != nil || !found {
		t.Fatalf("consolidated profile missing: %v", err)
	}
}

func TestClaudeDeviceProfileNoDowngradeAfterSevenDays(t *testing.T) {
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	store, err := kvstore.OpenBoltStore(filepath.Join(t.TempDir(), "clock.db"), kvstore.WithNowFunc(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	previous := currentClaudeDeviceProfileKVClient
	currentClaudeDeviceProfileKVClient = func() (claudeDeviceProfileKVClient, bool, error) { return store, true, nil }
	t.Cleanup(func() { currentClaudeDeviceProfileKVClient = previous; _ = store.Close() })
	want := resolveProfile(t, claudeDeviceHeaders("claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.220)"), nil)
	now = now.Add(365 * 24 * time.Hour)
	if got := resolveProfile(t, nil, nil); got != want {
		t.Fatalf("persistent tuple expired to baseline: %+v", got)
	}
}

func TestClaudeDeviceProfileMemoryCapacityPreservesExistingIdentity(t *testing.T) {
	profileStore(t, false)
	established := resolveProfile(t, claudeDeviceHeaders("claude-cli/2.1.322 (external, sdk-ts)"), nil)
	claudeDeviceProfileCacheMu.Lock()
	for i := 0; len(claudeDeviceProfileCache) < 4096; i++ {
		claudeDeviceProfileCache[string(rune(i))] = claudeDeviceProfileCacheEntry{profile: established}
	}
	claudeDeviceProfileCacheMu.Unlock()
	if _, err := ResolveClaudeDeviceProfileRequired(context.Background(), &cliproxyauth.Auth{ID: "new-credential"}, "new-key", nil, nil); err == nil {
		t.Fatal("capacity silently evicted a stable identity")
	}
	if got := resolveProfile(t, nil, nil); got != established {
		t.Fatalf("capacity changed existing identity: %+v", got)
	}
}
