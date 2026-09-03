package managementasset

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBundledArtifactIsValid(t *testing.T) {
	asset, errResolve := Resolve("")
	if errResolve != nil {
		t.Fatalf("Resolve() error = %v", errResolve)
	}
	if asset.Source != "bundled" || len(asset.HTML) == 0 {
		t.Fatalf("Resolve() = source %q bytes %d", asset.Source, len(asset.HTML))
	}
	if asset.Manifest.CPAMCCommit != "45b287302f7f727e59802c9c059b6760b12745f6" {
		t.Fatalf("CPAMC commit = %q", asset.Manifest.CPAMCCommit)
	}
}

func TestResolveUsesOnlyValidatedMutableArtifact(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	t.Setenv("MANAGEMENT_STATIC_PATH", "")

	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	staticDir := filepath.Join(root, "static")
	if errMkdir := os.MkdirAll(staticDir, 0o755); errMkdir != nil {
		t.Fatal(errMkdir)
	}

	html := []byte("<!doctype html><title>mutable</title>")
	writeTestArtifact(t, staticDir, html, hexDigest(html))
	asset, errResolve := Resolve(configPath)
	if errResolve != nil {
		t.Fatalf("Resolve(valid mutable) error = %v", errResolve)
	}
	if asset.Source != "mutable" || string(asset.HTML) != string(html) {
		t.Fatalf("Resolve(valid mutable) source=%q body=%q", asset.Source, asset.HTML)
	}

	writeTestArtifact(t, staticDir, html, hexDigest([]byte("different")))
	asset, errResolve = Resolve(configPath)
	if errResolve != nil {
		t.Fatalf("Resolve(invalid mutable) error = %v", errResolve)
	}
	if asset.Source != "bundled" {
		t.Fatalf("Resolve(invalid mutable) source = %q, want bundled", asset.Source)
	}
}

func TestResolveUsesWritablePathLayout(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WRITABLE_PATH", root)
	t.Setenv("writable_path", "")
	t.Setenv("MANAGEMENT_STATIC_PATH", "")

	html := []byte("<!doctype html><title>writable</title>")
	staticDir := filepath.Join(root, "static")
	if errMkdir := os.MkdirAll(staticDir, 0o755); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	writeTestArtifact(t, staticDir, html, hexDigest(html))
	asset, errResolve := Resolve(filepath.Join(t.TempDir(), "config.yaml"))
	if errResolve != nil {
		t.Fatalf("Resolve() error = %v", errResolve)
	}
	if asset.Source != "mutable" || string(asset.HTML) != string(html) {
		t.Fatalf("Resolve() source=%q body=%q", asset.Source, asset.HTML)
	}
}

func writeTestArtifact(t *testing.T, dir string, html []byte, digest string) {
	t.Helper()
	manifest := ArtifactManifest{
		SchemaVersion: 1,
		CPAMCCommit:   "1111111111111111111111111111111111111111",
		ManagementAPI: ManagementAPIRange{Min: 1, Max: 1},
		BuiltAt:       time.Unix(1, 0).UTC(),
		SHA256:        digest,
	}
	manifestData, errMarshal := json.Marshal(manifest)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if errWrite := os.WriteFile(filepath.Join(dir, managementAssetName), html, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errWrite := os.WriteFile(filepath.Join(dir, manifestAssetName), manifestData, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
}

func hexDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
