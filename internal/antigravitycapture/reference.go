package antigravitycapture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/regexp"
)

const UpdateInterval = 24 * time.Hour

var (
	updateLock        = make(chan struct{}, 1)
	versionPattern    = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	generationPattern = regexp.MustCompile(`^release-[0-9]+$`)
	referenceAgent    = regexp.MustCompile(`^antigravity/cli/[0-9]+\.[0-9]+\.[0-9]+ \(aidev_client; os_type=(?:darwin|linux|windows); arch=(?:amd64|arm64)(?:; cl=[0-9]+)?(?:; auth_method=consumer)?\)$`)
)

// Credential supplies one current CPA-managed access token for a private probe.
type Credential struct {
	AccessToken string
	ExpiresAt   time.Time
	Project     string
	ProxyURL    string
}

// Profile stores only the stable wire properties observed on a successful probe.
type Profile struct {
	Version           string    `json:"version"`
	CapturedAt        time.Time `json:"captured_at"`
	UserAgent         string    `json:"user_agent"`
	ContentType       string    `json:"content_type"`
	EnvelopeUserAgent string    `json:"envelope_user_agent"`
}

type activeReference struct {
	Profile
	Generation         string `json:"generation"`
	PreviousGeneration string `json:"previous_generation,omitempty"`
}

type checkState struct {
	Version   string    `json:"version"`
	CheckedAt time.Time `json:"checked_at"`
}

func defaultRoot() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cli-proxy-api", "antigravity-reference"), nil
}

// LoadCurrent reads the active capture without starting the CLI or checking for updates.
func LoadCurrent() (*Profile, error) {
	root, err := defaultRoot()
	if err != nil {
		return nil, err
	}
	active, err := loadReference(root)
	if err != nil {
		return nil, err
	}
	return &active.Profile, nil
}

func loadReference(root string) (activeReference, error) {
	var active activeReference
	if err := checkPrivateDirectory(root); err != nil {
		return activeReference{}, err
	}
	if err := readPrivateJSON(filepath.Join(root, "active.json"), &active); err != nil {
		return activeReference{}, fmt.Errorf("read active Antigravity reference: %w", err)
	}
	if !generationPattern.MatchString(active.Generation) || !active.valid() {
		return activeReference{}, errors.New("active Antigravity reference is invalid")
	}
	generation := filepath.Join(root, active.Generation)
	if err := checkPrivateDirectory(generation); err != nil {
		return activeReference{}, err
	}
	binary, errStat := os.Lstat(filepath.Join(generation, executableName()))
	if errStat != nil || !binary.Mode().IsRegular() {
		return activeReference{}, errors.New("active Antigravity reference is invalid")
	}
	return active, nil
}

func (p Profile) valid() bool {
	return versionPattern.MatchString(p.Version) && !p.CapturedAt.IsZero() &&
		!p.CapturedAt.After(time.Now().Add(time.Minute)) && len(p.UserAgent) <= 256 &&
		strings.HasPrefix(p.UserAgent, "antigravity/cli/"+p.Version+" (") &&
		referenceAgent.MatchString(p.UserAgent) && p.ContentType == "application/json" && p.EnvelopeUserAgent == "antigravity"
}

// MatchesContentType compares the media type with the one recorded by the reference.
func (p *Profile) MatchesContentType(headers http.Header) bool {
	mediaType, _, err := mime.ParseMediaType(headers.Get("Content-Type"))
	return p != nil && err == nil && len(headers.Values("Content-Type")) == 1 && mediaType == p.ContentType
}

// EnsureCurrent installs and probes the official release when its version changes.
// A failed candidate leaves the last complete reference active.
func EnsureCurrent(ctx context.Context, credentials []Credential, force bool) (*Profile, error) {
	root, err := defaultRoot()
	if err != nil {
		return nil, err
	}
	select {
	case updateLock <- struct{}{}:
		defer func() { <-updateLock }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return updateReference(ctx, root, credentials, force, fetchRelease, installRelease, capture)
}

func updateReference(ctx context.Context, root string, credentials []Credential, force bool,
	lookup func(context.Context) (release, error),
	install func(context.Context, release, string) error,
	probe func(context.Context, string, string, Credential) (Profile, error),
) (*Profile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	if err := checkPrivateDirectory(root); err != nil {
		return nil, err
	}
	if err := securePrivateDirectory(root); err != nil {
		return nil, err
	}
	current, errCurrent := loadReference(root)
	statePath := filepath.Join(root, "last-check.json")
	var state checkState
	_ = readPrivateJSON(statePath, &state)
	now := time.Now()
	recent := versionPattern.MatchString(state.Version) && !state.CheckedAt.After(now) && now.Sub(state.CheckedAt) < UpdateInterval
	if !force && recent && errCurrent == nil && current.Version == state.Version {
		return &current.Profile, nil
	}
	if len(credentials) == 0 {
		return nil, errors.New("Antigravity reference capture needs a current CPA Antigravity OAuth credential")
	}
	latest, err := lookup(ctx)
	if err != nil {
		return nil, err
	}
	if !force && errCurrent == nil && current.Version == latest.Version {
		return &current.Profile, writePrivateJSON(statePath, checkState{Version: latest.Version, CheckedAt: now.UTC()})
	}

	candidate, err := os.MkdirTemp(root, "release-")
	if err != nil {
		return nil, err
	}
	promoted := false
	defer func() {
		if !promoted {
			_ = os.RemoveAll(candidate)
		}
	}()
	if err := securePrivateDirectory(candidate); err != nil {
		return nil, err
	}
	binary := filepath.Join(candidate, executableName())
	if err := install(ctx, latest, binary); err != nil {
		return nil, err
	}
	var failures []error
	for _, credential := range credentials {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		profile, err := probe(ctx, binary, latest.Version, credential)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if !profile.valid() || profile.Version != latest.Version {
			return nil, errors.New("candidate Antigravity capture does not match its release")
		}
		active := activeReference{Profile: profile, Generation: filepath.Base(candidate)}
		if errCurrent == nil {
			active.PreviousGeneration = current.Generation
		}
		if err := writePrivateJSON(filepath.Join(root, "active.json"), active); err != nil {
			return nil, err
		}
		promoted = true
		if errCurrent == nil && generationPattern.MatchString(current.PreviousGeneration) && current.PreviousGeneration != current.Generation {
			_ = os.RemoveAll(filepath.Join(root, current.PreviousGeneration))
		}
		return &profile, writePrivateJSON(statePath, checkState{Version: latest.Version, CheckedAt: now.UTC()})
	}
	return nil, fmt.Errorf("private Antigravity reference capture failed: %w", errors.Join(failures...))
}

func checkPrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || !ownsPrivateDirectory(path, info) || (runtime.GOOS != "windows" && info.Mode().Perm()&0022 != 0) {
		return errors.New("Antigravity reference directory must be writable only by its owner")
	}
	return nil
}

func executableName() string {
	if runtime.GOOS == "windows" {
		return "agy.exe"
	}
	return "agy"
}

func readPrivateJSON(path string, dest any) error {
	const maxBytes = 64 << 10
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return errors.New("reference state must be a private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxBytes {
		return errors.New("reference state exceeds size limit")
	}
	return json.Unmarshal(body, dest)
}

func writePrivateJSON(path string, value any) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".reference-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	errWrite := json.NewEncoder(file).Encode(value)
	errClose := file.Close()
	if err := errors.Join(errWrite, errClose); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
