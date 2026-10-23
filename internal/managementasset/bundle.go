package managementasset

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	manifestAssetName          = "management-artifact.json"
	managementAPISchemaVersion = 1
	bundledRoot                = "bundled/"
	bundledAssetsDir           = "management-assets"
)

// ErrBundleMissing reports a CPA build whose embedded bundle holds no panel. A
// plain go build keeps this state until scripts/build-management-center.sh runs.
var ErrBundleMissing = errors.New("management panel bundle missing from build")

// bundledAssets holds the output of scripts/build-management-center.sh. Only
// bundled/.gitkeep exists in a fresh checkout, so all: is required to compile.
//
//go:embed all:bundled
var bundledAssets embed.FS

// bundledReadFile reads one file from the embedded bundle. Tests replace it to
// simulate a build without the panel bundle.
var bundledReadFile = func(name string) ([]byte, error) {
	return bundledAssets.ReadFile(name)
}

// ArtifactManifest binds a CPAMC build to its compatible CPA Management API range.
type ArtifactManifest struct {
	SchemaVersion int                `json:"schema_version"`
	CPAMCCommit   string             `json:"cpamc_commit"`
	ManagementAPI ManagementAPIRange `json:"management_api"`
	BuiltAt       time.Time          `json:"built_at"`
	SHA256        string             `json:"sha256"`
}

// ManagementAPIRange is the inclusive Management API schema range accepted by CPAMC.
type ManagementAPIRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// ResolvedAsset is a validated management panel and its provenance.
type ResolvedAsset struct {
	HTML     []byte
	Manifest ArtifactManifest
	Source   string
}

// Resolve returns a validated mutable panel or the immutable artifact bundled in CPA.
func Resolve(configFilePath string) (ResolvedAsset, error) {
	if mutable, errMutable := readMutable(configFilePath); errMutable == nil {
		return mutable, nil
	}
	return readBundled()
}

func readBundled() (ResolvedAsset, error) {
	html, errHTML := bundledReadFile(bundledRoot + managementAssetName)
	if errors.Is(errHTML, fs.ErrNotExist) {
		return ResolvedAsset{}, fmt.Errorf("%w: %s%s is absent; run scripts/build-management-center.sh and rebuild CPA", ErrBundleMissing, bundledRoot, managementAssetName)
	}
	if errHTML != nil {
		return ResolvedAsset{}, fmt.Errorf("read bundled management panel: %w", errHTML)
	}
	manifestData, errManifest := bundledReadFile(bundledRoot + manifestAssetName)
	if errManifest != nil {
		return ResolvedAsset{}, fmt.Errorf("read bundled management manifest: %w", errManifest)
	}
	manifest, errValidate := validateArtifact(html, manifestData)
	if errValidate != nil {
		return ResolvedAsset{}, fmt.Errorf("validate bundled management panel: %w", errValidate)
	}
	return ResolvedAsset{HTML: html, Manifest: manifest, Source: "bundled"}, nil
}

// PanelAsset returns one immutable panel asset from the embedded bundle, named
// relative to bundled/management-assets (for example "mascots/claude.webp").
// Missing files, directories, and names that are not clean relative paths report
// fs.ErrNotExist so callers can answer 404 without leaking the bundle layout.
func PanelAsset(name string) ([]byte, error) {
	name = strings.TrimPrefix(name, "/")
	if name == "" || name == ".." || strings.HasPrefix(name, "../") ||
		strings.Contains(name, "\\") || path.Clean(name) != name {
		return nil, fs.ErrNotExist
	}
	data, err := bundledReadFile(bundledRoot + bundledAssetsDir + "/" + name)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func readMutable(configFilePath string) (ResolvedAsset, error) {
	path := FilePath(configFilePath)
	if strings.TrimSpace(path) == "" {
		return ResolvedAsset{}, fmt.Errorf("empty management panel path")
	}
	html, errHTML := os.ReadFile(path)
	if errHTML != nil {
		return ResolvedAsset{}, errHTML
	}
	manifestData, errManifest := os.ReadFile(filepath.Join(filepath.Dir(path), manifestAssetName))
	if errManifest != nil {
		return ResolvedAsset{}, errManifest
	}
	manifest, errValidate := validateArtifact(html, manifestData)
	if errValidate != nil {
		return ResolvedAsset{}, errValidate
	}
	return ResolvedAsset{HTML: html, Manifest: manifest, Source: "mutable"}, nil
}

func validateArtifact(html, manifestData []byte) (ArtifactManifest, error) {
	var manifest ArtifactManifest
	decoder := json.NewDecoder(strings.NewReader(string(manifestData)))
	decoder.DisallowUnknownFields()
	if errDecode := decoder.Decode(&manifest); errDecode != nil {
		return ArtifactManifest{}, fmt.Errorf("decode manifest: %w", errDecode)
	}
	if errEOF := decoder.Decode(&struct{}{}); errEOF != io.EOF {
		return ArtifactManifest{}, fmt.Errorf("manifest contains trailing data")
	}
	if manifest.SchemaVersion != 1 {
		return ArtifactManifest{}, fmt.Errorf("unsupported manifest schema %d", manifest.SchemaVersion)
	}
	if len(manifest.CPAMCCommit) != 40 {
		return ArtifactManifest{}, fmt.Errorf("invalid CPAMC commit")
	}
	if _, errCommit := hex.DecodeString(manifest.CPAMCCommit); errCommit != nil {
		return ArtifactManifest{}, fmt.Errorf("invalid CPAMC commit")
	}
	if manifest.ManagementAPI.Min <= 0 || manifest.ManagementAPI.Max < manifest.ManagementAPI.Min ||
		managementAPISchemaVersion < manifest.ManagementAPI.Min || managementAPISchemaVersion > manifest.ManagementAPI.Max {
		return ArtifactManifest{}, fmt.Errorf("incompatible Management API range")
	}
	sum := sha256.Sum256(html)
	actual := hex.EncodeToString(sum[:])
	if !strings.EqualFold(strings.TrimSpace(manifest.SHA256), actual) {
		return ArtifactManifest{}, fmt.Errorf("management panel digest mismatch")
	}
	return manifest, nil
}
