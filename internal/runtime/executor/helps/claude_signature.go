package helps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/MoeForge/moego/ojson"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Existing credential pool identity survives enabling stabilization and renaming
// exact credential copies. The account and organization distinguish copied pools
// belonging to separate accounts. Missing pools retain the historical scope.
func claudeSignaturePoolDevice(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	pool := claudeauth.ReadDeviceIDPool(&auth.Metadata)
	var first string
	switch values := pool.(type) {
	case []string:
		if len(values) > 0 {
			first = values[0]
		}
	case []any:
		if len(values) > 0 {
			first, _ = values[0].(string)
		}
	}
	if validClaudeSignatureDeviceID(first) {
		return first
	}
	return ""
}

func validClaudeSignatureDeviceID(id string) bool {
	if len(id) != 64 {
		return false
	}
	raw, err := hex.DecodeString(id)
	return err == nil && len(raw) == 32 && strings.ToLower(id) == id
}

func claudeSignatureCanonicalScope(auth *cliproxyauth.Auth, apiKey string) string {
	device := claudeSignaturePoolDevice(auth)
	if device != "" {
		account := ClaudeCredentialAccountUUID(auth)
		organization := strings.TrimSpace(claudeauth.ReadMetadataString(&auth.Metadata, "organization_uuid"))
		// An absent account has no additional identity evidence beyond its stored pool.
		sum := sha256.Sum256([]byte("cli-proxy-api\x00claude\x00credential-pool\x00" + account + "\x00" + organization + "\x00" + device))
		return "pool:" + hex.EncodeToString(sum[:])
	}
	return claudeDeviceProfileLegacyScopeKey(auth, apiKey)
}

func claudeSignatureOwner(scope string) string {
	sum := sha256.Sum256([]byte("cli-proxy-api\x00claude\x00device-owner\x00" + scope))
	return hex.EncodeToString(sum[:])
}

var claudeSignatureMemoryOwners = make(map[string]string)

// claimClaudeSignatureDevice prefers a valid stored profile ID, then the native
// pool, then a deterministic scope-specific replacement. Each candidate is claimed
// atomically; exact copies have one owner and can reuse the same device.
func claimClaudeSignatureDevice(ctx context.Context, client claudeDeviceProfileKVClient, auth *cliproxyauth.Auth, apiKey, stored string) (string, error) {
	scope := claudeDeviceProfileScopeKey(auth, apiKey)
	owner := claudeSignatureOwner(scope)
	seen := make(map[string]bool)
	for _, id := range []string{stored, claudeSignaturePoolDevice(auth), claudeSignatureDeviceID(auth, apiKey)} {
		if !validClaudeSignatureDeviceID(id) || seen[id] {
			continue
		}
		seen[id] = true
		if client == nil {
			if previous, exists := claudeSignatureMemoryOwners[id]; exists {
				if previous == owner {
					return id, nil
				}
				continue
			}
			if len(claudeSignatureMemoryOwners) >= 8192 {
				return "", fmt.Errorf("device signature owner memory capacity reached; enable persistent KV storage")
			}
			claudeSignatureMemoryOwners[id] = owner
			return id, nil
		}
		cas, ok := client.(claudeDeviceProfileCASClient)
		if !ok {
			return "", fmt.Errorf("device signature ownership requires compare-and-swap")
		}
		key := "cpa:claude:device-owner:" + id
		raw, found, err := client.KVGet(ctx, key)
		if err != nil {
			return "", err
		}
		if found {
			if string(raw) == owner {
				return id, nil
			}
			continue
		}
		claimed, err := cas.KVCompareAndSwap(ctx, key, nil, false, []byte(owner), 0)
		if err != nil {
			return "", err
		}
		if claimed {
			return id, nil
		}
		raw, found, err = client.KVGet(ctx, key)
		if err != nil {
			return "", err
		}
		if found && string(raw) == owner {
			return id, nil
		}
	}
	return "", fmt.Errorf("device signature identity candidates are owned by another credential")
}

// Credential-scoped IDs make otherwise identical software/device tuples distinct
// without inventing software versions or changing the source credential record.
func claudeSignatureDeviceID(auth *cliproxyauth.Auth, apiKey string) string {
	sum := sha256.Sum256([]byte("cli-proxy-api\x00claude\x00signature-device\x00" + claudeDeviceProfileScopeKey(auth, apiKey)))
	return hex.EncodeToString(sum[:])
}

// ApplyClaudeSignatureMetadataDoc normalizes only an existing JSON device_id in
// metadata.user_id. Call it before final payload rules and signing; account,
// session and other caller fields retain their selected values. It never creates
// metadata on count_tokens or restores absent identity fields.
func ApplyClaudeSignatureMetadataDoc(doc *ojson.Doc, profile ClaudeDeviceProfile) {
	if doc == nil || len(profile.DeviceID) != 64 {
		return
	}
	decoded, err := hex.DecodeString(profile.DeviceID)
	if err != nil || len(decoded) != 32 {
		return
	}
	identity := doc.Get("metadata.user_id")
	if identity.Type != ojson.String || !ojson.Valid(identity.String()) {
		return
	}
	user := ojson.NewDocString(identity.String())
	if !ojson.Parse(identity.String()).IsObject() || !user.Get("device_id").Exists() {
		return
	}
	if user.Get("device_id").String() == profile.DeviceID {
		return
	}
	if err := user.Set("device_id", profile.DeviceID); err != nil {
		return
	}
	_ = doc.Set("metadata.user_id", user.String())
}

// ValidateClaudeSignatureMetadata rejects a conflicting final device identity
// without restoring identity fields removed by payload rules or token counting.
func ValidateClaudeSignatureMetadata(body []byte, profile ClaudeDeviceProfile) error {
	metadata, hasMetadata, errMetadata := uniqueClaudeJSONObjectMember(body, "metadata")
	if errMetadata != nil {
		return fmt.Errorf("final Claude signature metadata: %w", errMetadata)
	}
	if !hasMetadata {
		return nil
	}
	identityRaw, hasIdentity, errIdentity := uniqueClaudeJSONObjectMember(metadata, "user_id")
	if errIdentity != nil {
		return fmt.Errorf("final Claude signature metadata: %w", errIdentity)
	}
	if !hasIdentity {
		return nil
	}
	identity := ojson.ParseBytes(identityRaw)
	if identity.Type != ojson.String || !ojson.Valid(identity.String()) {
		return fmt.Errorf("final Claude signature metadata is invalid")
	}
	deviceRaw, hasDevice, errDevice := uniqueClaudeJSONObjectMember([]byte(identity.String()), "device_id")
	if errDevice != nil {
		return fmt.Errorf("final Claude signature metadata: %w", errDevice)
	}
	device := ojson.ParseBytes(deviceRaw)
	if hasDevice && device.String() != profile.DeviceID {
		return fmt.Errorf("final Claude device identity conflicts with the stabilized signature")
	}
	return nil
}

type claudeSignatureMigrationContextKey struct{}

// MigrateClaudeSignatureProfiles reconciles legacy profiles for the known loaded
// Claude credentials before the listener starts. It neither enumerates opaque KV
// keys nor creates fallback profiles for credentials without stored legacy state.
func MigrateClaudeSignatureProfiles(ctx context.Context, auths []*cliproxyauth.Auth, cfg *config.Config) error {
	if len(auths) == 0 || !ClaudeSignatureStabilizationEnabled(cfg) {
		return nil
	}
	client, persistent, errClient := currentClaudeDeviceProfileKVClient()
	if !persistent {
		return nil
	}
	if errClient != nil {
		return errClient
	}
	if client == nil {
		return fmt.Errorf("device profile KV unavailable during signature migration")
	}
	known := make([]*cliproxyauth.Auth, 0, len(auths))
	for _, auth := range auths {
		if auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") && claudeSignaturePoolDevice(auth) != "" {
			known = append(known, auth)
		}
	}
	sort.SliceStable(known, func(i, j int) bool { return known[i].ID < known[j].ID })
	for _, auth := range known {
		if errContext := ctx.Err(); errContext != nil {
			return errContext
		}
		_, found, errLegacy := readLegacyClaudeDeviceProfile(ctx, client, auth, "", cfg)
		if errLegacy != nil {
			return fmt.Errorf("read legacy Claude signature profile: %w", errLegacy)
		}
		if !found {
			continue
		}
		if _, errResolve := ResolveClaudeDeviceProfileRequired(context.WithValue(ctx, claudeSignatureMigrationContextKey{}, true), auth, "", nil, cfg); errResolve != nil {
			return fmt.Errorf("migrate Claude signature profile: %w", errResolve)
		}
	}
	return nil
}
