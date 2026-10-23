package managementasset

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBundledArtifactIsValid(t *testing.T) {
	asset, errResolve := Resolve("")
	if errors.Is(errResolve, ErrBundleMissing) {
		t.Skip("this build has no bundled panel; run scripts/build-management-center.sh before testing it")
	}
	if errResolve != nil {
		t.Fatalf("Resolve() error = %v", errResolve)
	}
	if asset.Source != "bundled" || len(asset.HTML) == 0 {
		t.Fatalf("Resolve() = source %q bytes %d", asset.Source, len(asset.HTML))
	}
	wantCommit := cpamcSourceCommit(t)
	if asset.Manifest.CPAMCCommit != wantCommit {
		t.Fatalf("CPAMC commit = %q, want submodule revision %q; rebuild with scripts/build-management-center.sh", asset.Manifest.CPAMCCommit, wantCommit)
	}
}

// cpamcSourceCommit returns the checked-out CPAMC revision so the bundled panel
// can be checked against its source without hardcoding a commit here. Tests run
// outside a Git checkout, or without the submodule, are skipped.
func cpamcSourceCommit(t *testing.T) string {
	t.Helper()
	root, errAbs := filepath.Abs("../..")
	if errAbs != nil {
		t.Skipf("resolve repository root: %v", errAbs)
	}
	panelRoot := filepath.Join(root, "web", "management-center")
	if _, errGit := exec.LookPath("git"); errGit != nil {
		t.Skipf("git unavailable: %v", errGit)
	}
	out, errRev := exec.Command("git", "-C", panelRoot, "rev-parse", "HEAD").Output()
	if errRev != nil {
		t.Skipf("CPAMC submodule revision unavailable: %v", errRev)
	}
	return strings.TrimSpace(string(out))
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

func TestResolveReportsMissingBundle(t *testing.T) {
	original := bundledReadFile
	bundledReadFile = func(string) ([]byte, error) { return nil, fs.ErrNotExist }
	t.Cleanup(func() { bundledReadFile = original })

	if _, err := readBundled(); !errors.Is(err, ErrBundleMissing) {
		t.Fatalf("readBundled() error = %v, want ErrBundleMissing", err)
	}
}

func TestPanelAssetRejectsPathsOutsideAssets(t *testing.T) {
	names := []string{
		"",
		"/",
		"..",
		"../management.html",
		"mascots/../../management.html",
		"mascots/../claude.webp",
		"mascots//claude.webp",
		"mascots/claude.webp/",
		"mascots" + string(rune(92)) + "claude.webp",
	}
	for _, name := range names {
		if _, err := PanelAsset(name); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("PanelAsset(%q) error = %v, want fs.ErrNotExist", name, err)
		}
	}
}

func TestPanelAssetReadsBundledMascot(t *testing.T) {
	data, errAsset := PanelAsset("mascots/claude.webp")
	if errors.Is(errAsset, fs.ErrNotExist) {
		t.Skip("mascots are not in the embedded bundle; run scripts/build-management-center.sh")
	}
	if errAsset != nil {
		t.Fatalf("PanelAsset() error = %v", errAsset)
	}
	want, errRead := bundledAssets.ReadFile(bundledRoot + bundledAssetsDir + "/mascots/claude.webp")
	if errRead != nil {
		t.Fatal(errRead)
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("PanelAsset() returned %d bytes, want %d", len(data), len(want))
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
