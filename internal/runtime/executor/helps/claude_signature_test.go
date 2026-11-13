package helps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestClaudeSignatureDistinctCredentialDeviceIDs(t *testing.T) {
	profileStore(t, true)
	h := claudeDeviceHeaders("claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.220)")
	ids := map[string]bool{}
	var software ClaudeDeviceProfile
	for _, scope := range []string{"credential-a", "credential-b"} {
		p, err := ResolveClaudeDeviceProfileRequired(context.Background(), &cliproxyauth.Auth{ID: scope}, "same-api-key", h, nil)
		if err != nil {
			t.Fatal(err)
		}
		id := p.DeviceID
		if len(id) != 64 || ids[id] {
			t.Errorf("distinct credential %s lacks unique stable64hex device ID: %q", scope, id)
		}
		ids[id] = true
		p.DeviceID = ""
		if software.UserAgent == "" {
			software = p
		} else if p != software {
			t.Fatal("credential uniqueness fabricated a software/platform difference")
		}
	}
}

func TestClaudeSignatureDeviceIdentitySurvivesUpgradeAndRestart(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "persistent"}[persistent], func(t *testing.T) {
			profileStore(t, persistent)
			original := resolveProfile(t, claudeDeviceHeaders("claude-cli/2.1.318 (external, sdk-ts, agent-sdk/0.3.220)"), nil)
			otherAuth := &cliproxyauth.Auth{ID: "other-credential"}
			other, err := ResolveClaudeDeviceProfileRequired(context.Background(), otherAuth, "key", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			upgraded := resolveProfile(t, claudeDeviceHeaders("claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.220)"), nil)
			if original.DeviceID != upgraded.DeviceID || original.UserAgent == upgraded.UserAgent {
				t.Fatal("software upgrade rotated device or failed to upgrade")
			}
			unchanged, err := ResolveClaudeDeviceProfileRequired(context.Background(), otherAuth, "key", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if unchanged != other || unchanged.DeviceID == original.DeviceID {
				t.Fatal("one credential mutated another signature")
			}
			if persistent {
				ResetClaudeDeviceProfileCache()
				if got := resolveProfile(t, nil, nil); got != upgraded {
					t.Fatal("restart lost signature")
				}
			}
		})
	}
	if claudeSignatureDeviceID(nil, "key-a") == claudeSignatureDeviceID(nil, "key-b") {
		t.Fatal("API key fallback scopes collide")
	}
	if claudeSignatureDeviceID(&cliproxyauth.Auth{ID: "id"}, "key-a") != claudeSignatureDeviceID(&cliproxyauth.Auth{ID: "id"}, "key-b") {
		t.Fatal("API key change rotated established auth scope")
	}
}

func TestClaudeSignatureMigratesMissingInvalidAndRetainsValidDeviceID(t *testing.T) {
	for _, storedID := range []string{"", strings.Repeat("a", 64), "malformed"} {
		t.Run(storedID, func(t *testing.T) {
			store := profileStore(t, true)
			profile := resolveProfile(t, claudeDeviceHeaders("claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.220)"), nil)
			value := claudeDeviceProfileKVValueFromProfile(profile)
			value.DeviceID = storedID
			key := claudeDeviceProfileKVKey(&cliproxyauth.Auth{ID: "credential"}, "key", ClaudeDeviceProfile{})
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.KVSet(context.Background(), key, raw, kvstore.KVSetOptions{}); err != nil {
				t.Fatal(err)
			}
			ResetClaudeDeviceProfileCache()
			if validClaudeSignatureDeviceID(storedID) {
				profile.DeviceID = storedID
			}
			if migrated := resolveProfile(t, nil, nil); migrated != profile {
				t.Fatalf("ID migration reset tuple: %+v", migrated)
			}
			raw, found, err := store.KVGet(context.Background(), key)
			if err != nil || !found {
				t.Fatal(err)
			}
			var persisted claudeDeviceProfileKVValue
			if err := json.Unmarshal(raw, &persisted); err != nil {
				t.Fatal(err)
			}
			if persisted.DeviceID != profile.DeviceID {
				t.Fatal("corrected device ID not persisted")
			}
		})
	}
}

func TestApplyClaudeSignatureMetadataDocPreservesOtherIdentityFields(t *testing.T) {
	profileStore(t, false)
	profile := resolveProfile(t, nil, nil)
	identity := `{"device_id":"` + strings.Repeat("a", 64) + `","account_uuid":"selected-account","session_id":"native-session","parent_session_id":"native-parent","unknown":{"keep":true}}`
	doc := ojson.NewDocString(`{"metadata":{"user_id":"","other":"retain"},"messages":[]}`)
	if err := doc.Set("metadata.user_id", identity); err != nil {
		t.Fatal(err)
	}
	ApplyClaudeSignatureMetadataDoc(doc, profile)
	got := doc.Get("metadata.user_id").String()
	want := strings.Replace(identity, strings.Repeat("a", 64), profile.DeviceID, 1)
	if got != want || doc.Get("metadata.other").String() != "retain" {
		t.Fatalf("signature normalization altered selected metadata: %s", doc.String())
	}
	for _, raw := range []string{`{"messages":[]}`, `{"metadata":{"other":true}}`, `{"metadata":{"user_id":"{\"account_uuid\":\"account\"}"}}`, `{"metadata":{"user_id":"not-json"}}`, `{"metadata":{"user_id":42}}`} {
		doc := ojson.NewDocString(raw)
		ApplyClaudeSignatureMetadataDoc(doc, profile)
		if doc.String() != raw {
			t.Fatalf("created absent identity: %s", doc.String())
		}
	}
}

func TestClaudeSignatureDeviceIdentitySurvivesStoreReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signature.db")
	store, err := kvstore.OpenBoltStore(path)
	if err != nil {
		t.Fatal(err)
	}
	previous := currentClaudeDeviceProfileKVClient
	currentClaudeDeviceProfileKVClient = func() (claudeDeviceProfileKVClient, bool, error) { return store, true, nil }
	t.Cleanup(func() {
		currentClaudeDeviceProfileKVClient = previous
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	profile := resolveProfile(t, claudeDeviceHeaders("claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.220)"), nil)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = kvstore.OpenBoltStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolveProfile(t, nil, nil); got != profile {
		t.Fatalf("store reopen changed stable signature: %+v", got)
	}
}

func TestClaudeSignatureRejectsDuplicateFinalIdentity(t *testing.T) {
	p := ClaudeDeviceProfile{DeviceID: strings.Repeat("a", 64)}
	valid := `{"device_id":"` + p.DeviceID + `"}`
	duplicated := `{"device_id":"` + p.DeviceID + `","device_id":"` + strings.Repeat("b", 64) + `"}`
	for _, body := range []string{
		fmt.Sprintf(`{"metadata":{"user_id":%q}}`, duplicated),
		fmt.Sprintf(`{"metadata":{"user_id":%q,"user_id":%q}}`, valid, valid),
		fmt.Sprintf(`{"metadata":{"user_id":%q},"metadata":{"user_id":%q}}`, valid, valid),
	} {
		if err := ValidateClaudeSignatureMetadata([]byte(body), p); err == nil {
			t.Errorf("ambiguous final identity accepted: %s", body)
		}
	}
}

func signatureNativeAuth(id, account, device string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: id, Metadata: map[string]any{"account_uuid": account, "organization_uuid": "organization", "claude_device_ids": []string{device}}}
}

func TestClaudeSignatureExactCopiesShareDeviceAndUpgrade(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprint(persistent), func(t *testing.T) {
			profileStore(t, persistent)
			device := strings.Repeat("b", 64)
			original := signatureNativeAuth("a.json", "account-a", device)
			renamed := signatureNativeAuth("renamed-a.json", "account-a", device)
			headers := claudeDeviceHeaders("claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.220)")
			first, err := ResolveClaudeDeviceProfileRequired(context.Background(), original, "key", headers, nil)
			if err != nil {
				t.Fatal(err)
			}
			copy, err := ResolveClaudeDeviceProfileRequired(context.Background(), renamed, "key", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if first.DeviceID != device || copy != first {
				t.Fatalf("exact copy changed device/profile: first %+v renamed %+v", first, copy)
			}
		})
	}
}

func TestClaudeSignatureCopiedPoolsAcrossAccountsClaimDistinctIDs(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprint(persistent), func(t *testing.T) {
			profileStore(t, persistent)
			device := strings.Repeat("b", 64)
			accounts := []*cliproxyauth.Auth{signatureNativeAuth("a.json", "account-a", device), signatureNativeAuth("b.json", "account-b", device)}
			var wg sync.WaitGroup
			profiles := make([]ClaudeDeviceProfile, 2)
			for i := range accounts {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					p, err := ResolveClaudeDeviceProfileRequired(context.Background(), accounts[i], "key", nil, nil)
					if err != nil {
						t.Error(err)
					}
					profiles[i] = p
				}(i)
			}
			wg.Wait()
			if profiles[0].DeviceID == profiles[1].DeviceID {
				t.Fatal("different account scopes claimed same copied pool ID")
			}
			if profiles[0].DeviceID != device && profiles[1].DeviceID != device {
				t.Fatal("neither credential retained original pool")
			}
			if persistent {
				ResetClaudeDeviceProfileCache()
			}
			for i, auth := range accounts {
				p, err := ResolveClaudeDeviceProfileRequired(context.Background(), auth, "key", nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				if p != profiles[i] {
					t.Fatal("collision allocation rotated on reread")
				}
				if claudeSignaturePoolDevice(auth) != device {
					t.Fatal("claim mutated source auth pool")
				}
			}
		})
	}
}

func TestClaudeSignatureCopiedProfileCollisionRetainsSoftware(t *testing.T) {
	store := profileStore(t, true)
	original := resolveProfile(t, claudeDeviceHeaders("claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.220)"), nil)
	other := signatureNativeAuth("other.json", "other-account", strings.Repeat("b", 64))
	key := claudeDeviceProfileKVKey(other, "key", ClaudeDeviceProfile{})
	raw, err := json.Marshal(claudeDeviceProfileKVValueFromProfile(original))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.KVSet(context.Background(), key, raw, kvstore.KVSetOptions{}); err != nil {
		t.Fatal(err)
	}
	migrated, err := ResolveClaudeDeviceProfileRequired(context.Background(), other, "key", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.DeviceID == original.DeviceID || migrated.DeviceID != claudeSignaturePoolDevice(other) {
		t.Fatal("cross-credential copied profile retained conflicting ID")
	}
	migrated.DeviceID = original.DeviceID
	if migrated != original {
		t.Fatal("copied profile correction changed software/platform")
	}
}

func TestClaudeSignatureLegacyAuthKeyMigrationRetainsDeviceAndTuple(t *testing.T) {
	store := profileStore(t, true)
	auth := signatureNativeAuth("legacy.json", "account-a", strings.Repeat("b", 64))
	profile, _ := extractClaudeDeviceProfile(claudeDeviceHeaders("claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.220)"), nil)
	profile.DeviceID = strings.Repeat("c", 64)
	raw, err := json.Marshal(claudeDeviceProfileKVValueFromProfile(profile))
	if err != nil {
		t.Fatal(err)
	}
	oldKey := "cpa:claude:device-profile:" + kvstore.HashKeyPart("auth:legacy.json")
	if _, err := store.KVSet(context.Background(), oldKey, raw, kvstore.KVSetOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveClaudeDeviceProfileRequired(context.Background(), auth, "key", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != profile {
		t.Fatalf("legacy authkey migration changed established identity: %+v", got)
	}
	renamed := signatureNativeAuth("renamed.json", "account-a", strings.Repeat("b", 64))
	got, err = ResolveClaudeDeviceProfileRequired(context.Background(), renamed, "key", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != profile {
		t.Fatal("rename after migration lost tuple")
	}
}

func TestClaudeSignatureConcurrentExactCopyClaimsShareOwner(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprint(persistent), func(t *testing.T) {
			profileStore(t, persistent)
			device := strings.Repeat("d", 64)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for _, name := range []string{"a.json", "renamed.json", "copy.json"} {
				wg.Add(1)
				go func(name string) {
					defer wg.Done()
					<-start
					p, err := ResolveClaudeDeviceProfileRequired(context.Background(), signatureNativeAuth(name, "account-a", device), "key", claudeDeviceHeaders("claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.220)"), nil)
					if err != nil {
						t.Error(err)
						return
					}
					if p.DeviceID != device {
						t.Errorf("exact copy allocated different owner device: %s", p.DeviceID)
					}
				}(name)
			}
			close(start)
			wg.Wait()
		})
	}
}

func TestClaudeSignatureAccountUUIDAliasDistinguishesCopiedPools(t *testing.T) {
	profileStore(t, true)
	device := strings.Repeat("b", 64)
	a := signatureNativeAuth("a.json", "account-a", device)
	b := signatureNativeAuth("b.json", "account-b", device)
	delete(a.Metadata, "account_uuid")
	a.Metadata["accountUuid"] = "account-a"
	delete(b.Metadata, "account_uuid")
	b.Metadata["accountUuid"] = "account-b"
	pa, err := ResolveClaudeDeviceProfileRequired(context.Background(), a, "key", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := ResolveClaudeDeviceProfileRequired(context.Background(), b, "key", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pa.DeviceID == pb.DeviceID {
		t.Fatal("supported accountUuid identities shared copied device/profile")
	}
	snake := signatureNativeAuth("renamed.json", "account-a", device)
	if claudeDeviceProfileScopeKey(snake, "key") != claudeDeviceProfileScopeKey(a, "key") {
		t.Fatal("equivalent account spelling changed scope")
	}
}

func TestClaudeSignatureAliasFirstReconcilesOriginalLegacyUpgrade(t *testing.T) {
	store := profileStore(t, true)
	device := strings.Repeat("b", 64)
	original := signatureNativeAuth("a.json", "account-a", device)
	copy := signatureNativeAuth("copy-a.json", "account-a", device)
	newer := claudeDeviceHeaders("claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.220)")
	legacy, _ := extractClaudeDeviceProfile(newer, nil)
	legacy.DeviceID = device
	raw, _ := json.Marshal(claudeDeviceProfileKVValueFromProfile(legacy))
	oldKey := "cpa:claude:device-profile:" + kvstore.HashKeyPart("auth:a.json")
	if _, err := store.KVSet(context.Background(), oldKey, raw, kvstore.KVSetOptions{}); err != nil {
		t.Fatal(err)
	}
	older := claudeDeviceHeaders("claude-cli/2.1.318 (external, sdk-ts, agent-sdk/0.3.220)")
	first, err := ResolveClaudeDeviceProfileRequired(context.Background(), copy, "key", older, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The unknown original filename cannot be discovered from hashed keys. Once
	// the original credential is available its established state must be reconciled.
	restored, err := ResolveClaudeDeviceProfileRequired(context.Background(), original, "key", older, nil)
	if err != nil {
		t.Fatal(err)
	}
	if restored.UserAgent != legacy.UserAgent || restored.DeviceID != first.DeviceID {
		t.Fatalf("alias initialization suppressed known legacy state: %+v", restored)
	}
	reread, err := ResolveClaudeDeviceProfileRequired(context.Background(), copy, "key", older, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reread != restored {
		t.Fatal("alias replay downgraded reconciled profile")
	}
}

func TestMigrateClaudeSignatureProfilesBeforeAliasRequest(t *testing.T) {
	store := profileStore(t, true)
	device := strings.Repeat("b", 64)
	original := signatureNativeAuth("z-original.json", "account-a", device)
	original.Provider = "claude"
	copy := signatureNativeAuth("a-copy.json", "account-a", device)
	copy.Provider = "claude"
	legacy, _ := extractClaudeDeviceProfile(claudeDeviceHeaders("claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.220)"), nil)
	legacy.DeviceID = device
	raw, _ := json.Marshal(claudeDeviceProfileKVValueFromProfile(legacy))
	oldKey := "cpa:claude:device-profile:" + kvstore.HashKeyPart("auth:z-original.json")
	if _, err := store.KVSet(context.Background(), oldKey, raw, kvstore.KVSetOptions{}); err != nil {
		t.Fatal(err)
	}
	enabled := true
	cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{StabilizeDeviceProfile: &enabled}}
	if err := MigrateClaudeSignatureProfiles(context.Background(), []*cliproxyauth.Auth{copy, original}, cfg); err != nil {
		t.Fatal(err)
	}
	first, err := ResolveClaudeDeviceProfileRequired(context.Background(), copy, "key", claudeDeviceHeaders("claude-cli/2.1.318 (external, sdk-ts, agent-sdk/0.3.220)"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first != legacy {
		t.Fatalf("preflight allowed first copy to downgrade migrated profile: %+v", first)
	}
}

func TestMigrateClaudeSignatureProfilesSkipsAbsentLegacyState(t *testing.T) {
	store := profileStore(t, true)
	auth := signatureNativeAuth("new.json", "account-a", strings.Repeat("b", 64))
	auth.Provider = "claude"
	enabled := true
	cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{StabilizeDeviceProfile: &enabled}}
	if err := MigrateClaudeSignatureProfiles(context.Background(), []*cliproxyauth.Auth{auth}, cfg); err != nil {
		t.Fatal(err)
	}
	key := claudeDeviceProfileKVKey(auth, "", ClaudeDeviceProfile{})
	if _, found, err := store.KVGet(context.Background(), key); err != nil || found {
		t.Fatalf("preflight seeded new fallback: found %v error %v", found, err)
	}
}

func TestMigrateClaudeSignatureProfilesReconcilesLegacySubclient(t *testing.T) {
	store := profileStore(t, true)
	device := strings.Repeat("b", 64)
	a := signatureNativeAuth("a.json", "account-a", device)
	a.Provider = "claude"
	b := signatureNativeAuth("b.json", "account-a", device)
	b.Provider = "claude"
	for _, item := range []struct{ key, version string }{{"auth:a.json", "2.1.318"}, {"auth:b.json|subclient:sdk-ts", "2.1.322"}} {
		profile, _ := extractClaudeDeviceProfile(claudeDeviceHeaders("claude-cli/"+item.version+" (external, sdk-ts, agent-sdk/0.3.220)"), nil)
		profile.DeviceID = device
		raw, _ := json.Marshal(claudeDeviceProfileKVValueFromProfile(profile))
		if _, err := store.KVSet(context.Background(), "cpa:claude:device-profile:"+kvstore.HashKeyPart(item.key), raw, kvstore.KVSetOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	enabled := true
	cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{StabilizeDeviceProfile: &enabled}}
	if err := MigrateClaudeSignatureProfiles(context.Background(), []*cliproxyauth.Auth{b, a}, cfg); err != nil {
		t.Fatal(err)
	}
	p, err := ResolveClaudeDeviceProfileRequired(context.Background(), a, "key", nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.UserAgent != "claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.220)" {
		t.Fatalf("legacy subclient suppressed after another alias preflight: %+v", p)
	}
}

func TestMigrateClaudeSignatureProfilesGatesAndErrors(t *testing.T) {
	store := profileStore(t, true)
	auth := signatureNativeAuth("a.json", "account-a", strings.Repeat("b", 64))
	auth.Provider = "claude"
	enabled := true
	cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{StabilizeDeviceProfile: &enabled}}
	sentinel := errors.New("KV unavailable")
	currentClaudeDeviceProfileKVClient = func() (claudeDeviceProfileKVClient, bool, error) { return nil, true, sentinel }
	if err := MigrateClaudeSignatureProfiles(context.Background(), []*cliproxyauth.Auth{auth}, nil); err != nil {
		t.Fatal("disabled preflight queried storage")
	}
	if err := MigrateClaudeSignatureProfiles(context.Background(), []*cliproxyauth.Auth{auth}, cfg); !errors.Is(err, sentinel) {
		t.Fatal("required KV failure suppressed")
	}
	currentClaudeDeviceProfileKVClient = func() (claudeDeviceProfileKVClient, bool, error) { return nil, false, nil }
	if err := MigrateClaudeSignatureProfiles(context.Background(), []*cliproxyauth.Auth{auth}, cfg); err != nil {
		t.Fatal("memory-only preflight should skip")
	}
	fault := &profileFaultStore{BoltStore: store, getErr: sentinel}
	currentClaudeDeviceProfileKVClient = func() (claudeDeviceProfileKVClient, bool, error) { return fault, true, nil }
	if err := MigrateClaudeSignatureProfiles(context.Background(), []*cliproxyauth.Auth{auth}, cfg); !errors.Is(err, sentinel) {
		t.Fatal("legacy read failure suppressed")
	}
	auth.Provider = "other"
	if err := MigrateClaudeSignatureProfiles(context.Background(), []*cliproxyauth.Auth{nil, auth}, cfg); err != nil {
		t.Fatal("non-Claude credential queried storage")
	}
}

type signatureCountingStore struct {
	*kvstore.BoltStore
	gets int
}

func (s *signatureCountingStore) KVGet(ctx context.Context, key string) ([]byte, bool, error) {
	s.gets++
	return s.BoltStore.KVGet(ctx, key)
}

func TestClaudeSignatureCanonicalRequestBoundsLegacyReads(t *testing.T) {
	store := profileStore(t, true)
	auth := signatureNativeAuth("a.json", "account-a", strings.Repeat("b", 64))
	if _, err := ResolveClaudeDeviceProfileRequired(context.Background(), auth, "key", nil, nil); err != nil {
		t.Fatal(err)
	}
	counted := &signatureCountingStore{BoltStore: store}
	currentClaudeDeviceProfileKVClient = func() (claudeDeviceProfileKVClient, bool, error) { return counted, true, nil }
	if _, err := ResolveClaudeDeviceProfileRequired(context.Background(), auth, "key", nil, nil); err != nil {
		t.Fatal(err)
	}
	// Canonical profile, one historical primary, and its established owner claim.
	if counted.gets != 3 {
		t.Fatalf("normal request scanned legacy subclients: %d GETs, want3", counted.gets)
	}
}
