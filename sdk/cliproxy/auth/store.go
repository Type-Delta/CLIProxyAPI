package auth

import (
	"context"
	"path/filepath"
	"strings"
)

// Store abstracts persistence of Auth state across restarts.
type Store interface {
	// List returns all auth records stored in the backend.
	List(ctx context.Context) ([]*Auth, error)
	// Save persists the provided auth record, replacing any existing one with same ID.
	// It may enrich exported fields, but must not change ID, RegistrationEpoch,
	// CredentialVersion or Generation. The manager merges these changes even on non-fatal save errors.
	// Do not retain auth or mutate shared nested values in place: replace metadata
	// values, pointers, Storage and Runtime instead. Auth.Clone shares these objects.
	// Save may call manager read APIs, but must not synchronously mutate the
	// manager or call Load (the reload barrier remains held until publication).
	Save(ctx context.Context, auth *Auth) (string, error)
	// Delete removes the auth record identified by id.
	Delete(ctx context.Context, id string) error
}

// IsRuntimeStatePath reports whether a path is reserved for local state rather
// than a credential. The default KV database and its sidecars stay local.
func IsRuntimeStatePath(authDir, path string) bool {
	name := strings.ToLower(filepath.Base(path))
	if name == "kv.db" || strings.HasPrefix(name, "kv.db.") || strings.HasPrefix(name, "kv.db-") {
		return true
	}
	if authDir == "" {
		return false
	}
	rel, err := filepath.Rel(authDir, path)
	return err == nil && (rel == "state" || strings.HasPrefix(rel, "state"+string(filepath.Separator)))
}
