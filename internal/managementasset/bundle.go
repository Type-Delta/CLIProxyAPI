package managementasset

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	manifestAssetName          = "management-artifact.json"
	managementAPISchemaVersion = 1
)

//go:embed bundled/management.html bundled/management-artifact.json
var bundledAssets embed.FS

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
	html, errHTML := bundledAssets.ReadFile("bundled/" + managementAssetName)
	if errHTML != nil {
		return ResolvedAsset{}, fmt.Errorf("read bundled management panel: %w", errHTML)
	}
	manifestData, errManifest := bundledAssets.ReadFile("bundled/" + manifestAssetName)
	if errManifest != nil {
		return ResolvedAsset{}, fmt.Errorf("read bundled management manifest: %w", errManifest)
	}
	manifest, errValidate := validateArtifact(html, manifestData)
	if errValidate != nil {
		return ResolvedAsset{}, fmt.Errorf("validate bundled management panel: %w", errValidate)
	}
	return ResolvedAsset{HTML: html, Manifest: manifest, Source: "bundled"}, nil
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
