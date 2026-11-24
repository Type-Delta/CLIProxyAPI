package antigravitycapture

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/MoeForge/moego/json"
)

const manifestHost = "antigravity-cli-auto-updater-974169037036.us-central1.run.app"
const maxReleaseBytes = 512 << 20

type release struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA512  string `json:"sha512"`
}

func fetchRelease(ctx context.Context) (release, error) {
	platform := runtime.GOOS + "_" + runtime.GOARCH
	switch platform {
	case "linux_amd64", "linux_arm64", "darwin_arm64", "darwin_amd64", "windows_amd64", "windows_arm64":
	default:
		return release{}, fmt.Errorf("private Antigravity reference does not support %s", platform)
	}
	if runtime.GOOS == "linux" {
		// The official installer uses the musl release when the loader is musl.
		if paths, _ := filepath.Glob("/lib/ld-musl-*.so.1"); len(paths) > 0 {
			platform += "_musl"
		}
	}
	res, err := getReleaseAsset(ctx, "https://"+manifestHost+"/manifests/"+platform+".json")
	if err != nil {
		return release{}, fmt.Errorf("fetch Antigravity release manifest: %w", err)
	}
	defer res.Body.Close()
	const limit = 32 << 10
	body, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil || len(body) > limit {
		return release{}, errors.New("could not read Antigravity release manifest")
	}
	var latest release
	if json.Unmarshal(body, &latest) != nil || !versionPattern.MatchString(latest.Version) ||
		!validReleaseURL(latest.URL) || len(latest.SHA512) != sha512.Size*2 {
		return release{}, errors.New("invalid Antigravity release manifest")
	}
	return latest, nil
}

func validReleaseURL(raw string) bool {
	address, err := url.Parse(raw)
	if err != nil || address.Scheme != "https" || address.User != nil || address.Port() != "" ||
		address.RawQuery != "" || address.Fragment != "" {
		return false
	}
	switch address.Host {
	case manifestHost:
		return strings.HasPrefix(address.Path, "/manifests/")
	case "storage.googleapis.com":
		return strings.HasPrefix(address.Path, "/antigravity-public/antigravity-cli/") &&
			!strings.Contains(address.Path, "..")
	default:
		return false
	}
}

func getReleaseAsset(ctx context.Context, raw string) (*http.Response, error) {
	if !validReleaseURL(raw) {
		return nil, errors.New("untrusted Antigravity release address")
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		_ = res.Body.Close()
		return nil, fmt.Errorf("Antigravity release server returned HTTP %d", res.StatusCode)
	}
	return res, nil
}

func installRelease(ctx context.Context, latest release, executable string) error {
	wantDigest, err := hex.DecodeString(latest.SHA512)
	if err != nil || len(wantDigest) != sha512.Size {
		return errors.New("invalid Antigravity release checksum")
	}
	res, err := getReleaseAsset(ctx, latest.URL)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	download, err := os.CreateTemp(filepath.Dir(executable), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(download.Name())
	defer download.Close()
	hash := sha512.New()
	size, err := io.Copy(io.MultiWriter(download, hash), io.LimitReader(res.Body, maxReleaseBytes+1))
	if err != nil || size == 0 || size > maxReleaseBytes {
		return errors.New("could not read Antigravity release binary within size limit")
	}
	if subtle.ConstantTimeCompare(hash.Sum(nil), wantDigest) != 1 {
		return errors.New("Antigravity release SHA-512 checksum mismatch")
	}
	if strings.HasSuffix(latest.URL, ".tar.gz") {
		if _, err := download.Seek(0, io.SeekStart); err != nil {
			return err
		}
		return unpackRelease(download, executable)
	}
	if err := download.Close(); err != nil {
		return err
	}
	if err := os.Chmod(download.Name(), 0700); err != nil {
		return err
	}
	return os.Rename(download.Name(), executable)
}

func unpackRelease(download io.Reader, executable string) error {
	compressed, err := gzip.NewReader(download)
	if err != nil {
		return err
	}
	defer compressed.Close()
	archive := tar.NewReader(io.LimitReader(compressed, maxReleaseBytes+1024))
	entry, err := archive.Next()
	if err != nil || entry.Typeflag != tar.TypeReg || entry.Size <= 0 || entry.Size > maxReleaseBytes ||
		(entry.Name != "antigravity" && entry.Name != "agy") {
		return errors.New("Antigravity release archive has an unexpected executable")
	}
	binary, err := os.OpenFile(executable, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		return err
	}
	_, errCopy := io.CopyN(binary, archive, entry.Size)
	errClose := binary.Close()
	return errors.Join(errCopy, errClose)
}
