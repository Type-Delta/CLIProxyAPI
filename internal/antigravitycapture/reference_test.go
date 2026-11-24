package antigravitycapture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestReferenceRejectsSharedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owner-only modes are enforced with a Windows ACL")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivateDirectory(dir); err == nil {
		t.Fatal("reference accepted a directory writable by other users")
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "reference")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivateDirectory(link); err == nil {
		t.Fatal("reference accepted a symlinked directory")
	}
}

func TestReferenceUpdateAndRollback(t *testing.T) {
	root := t.TempDir()
	var checks, installs, probes int
	version := "1.5.16"
	lookup := func(context.Context) (release, error) {
		checks++
		return release{Version: version}, nil
	}
	install := func(_ context.Context, _ release, binary string) error {
		installs++
		return os.WriteFile(binary, []byte("test binary"), 0700)
	}
	probe := func(_ context.Context, _, version string, credential Credential) (Profile, error) {
		probes++
		if credential.AccessToken == "fail" || version == "1.5.17" {
			return Profile{}, errors.New("probe refused")
		}
		return Profile{
			Version: version, CapturedAt: time.Now().UTC(),
			UserAgent:   "antigravity/cli/" + version + " (aidev_client; os_type=linux; arch=amd64)",
			ContentType: "application/json", EnvelopeUserAgent: "antigravity",
		}, nil
	}
	credentials := []Credential{{AccessToken: "fail"}, {AccessToken: "fake-success-token", Project: "test-project"}}
	update := func(force bool) (*Profile, error) {
		return updateReference(t.Context(), root, credentials, force, lookup, install, probe)
	}
	if profile, err := update(false); err != nil || profile.Version != version || probes != 2 {
		t.Fatalf("initial capture: profile=%+v probes=%d error=%v", profile, probes, err)
	}
	initial, err := loadReference(root)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(root, "active.json"))
	if err != nil || strings.Contains(string(body), "fake-success-token") || strings.Contains(string(body), "test-project") {
		t.Fatal("active reference retained private credential data or could not be read")
	}
	if _, err := update(false); err != nil || checks != 1 || installs != 1 {
		t.Fatalf("fresh reference repeated update: checks=%d installs=%d error=%v", checks, installs, err)
	}
	version = "1.5.17"
	if _, err := update(true); err == nil {
		t.Fatal("expected failed probe")
	}
	current, err := loadReference(root)
	if err != nil || current.Generation != initial.Generation {
		t.Fatalf("failed update changed active reference: %+v error=%v", current, err)
	}
	if candidates, _ := filepath.Glob(filepath.Join(root, "release-*")); len(candidates) != 1 {
		t.Fatalf("failed candidate remained on disk: %v", candidates)
	}

	version = "1.5.18"
	statePath := filepath.Join(root, "last-check.json")
	if err := writePrivateJSON(statePath, checkState{Version: initial.Version, CheckedAt: time.Now().Add(-2 * UpdateInterval)}); err != nil {
		t.Fatal(err)
	}
	if profile, err := update(false); err != nil || profile.Version != version {
		t.Fatalf("scheduled update did not replace active reference: %+v error=%v", profile, err)
	}
	current, err = loadReference(root)
	if err != nil || current.PreviousGeneration != initial.Generation {
		t.Fatalf("last known good binary was not retained: %+v error=%v", current, err)
	}
}
