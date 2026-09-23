package claudecapture

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPrivateSandboxHidesHomeAndPassesOAuthOnDescriptor(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap isolation is Linux-only")
	}
	if err := sandboxAvailable(); err != nil {
		t.Skip(err)
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := preparePrivateHome(home); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(root, "probe.sh")
	if err := os.WriteFile(filepath.Join(root, "active.json"), []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(probe, []byte("#!/bin/sh\n[ ! -e \"$1\" ] || exit 23\n[ \"$HOME\" = /cpa/home ] || exit 24\nif echo modified > /cpa/active.json 2>/dev/null; then exit 25; fi\necho writable > /cpa/home/sentinel\ncat /proc/self/fd/3\n"), 0700); err != nil {
		t.Fatal(err)
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	command, err := sandboxCommand(context.Background(), root, home, probe, []string{home}, realHome)
	if err != nil {
		t.Fatal(err)
	}
	command.Env = privateCommandEnvironment("/cpa/home", "", "")
	var output bytes.Buffer
	command.Stdout = &output
	token := "fake-oauth-test-token"
	if err := runWithOAuthDescriptor(command, token); err != nil {
		t.Fatal(err)
	}
	if output.String() != token+"\n" {
		t.Fatal("private child did not receive the OAuth descriptor")
	}
	active, err := os.ReadFile(filepath.Join(root, "active.json"))
	if err != nil || string(active) != "unchanged" {
		t.Fatalf("private child changed active reference: %q, %v", active, err)
	}
	if _, err := os.Stat(filepath.Join(home, "sentinel")); err != nil {
		t.Fatalf("private child could not write its own home: %v", err)
	}
	if strings.Contains(strings.Join(command.Args, " "), token) || strings.Contains(strings.Join(command.Env, " "), token) {
		t.Fatal("OAuth token leaked into child arguments or environment")
	}
}

func TestUpdateCheckCadence(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name string
		last time.Time
		want bool
	}{
		{"never checked", time.Time{}, true},
		{"recent", now.Add(-23 * time.Hour), false},
		{"boundary", now.Add(-24 * time.Hour), true},
		{"future clock", now.Add(time.Minute), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldCheck(now, test.last); got != test.want {
				t.Fatalf("shouldCheck = %t, want %t", got, test.want)
			}
		})
	}
}

func TestUpdateCheckStateIsPrivateAndRetryable(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	path := filepath.Join(root, "last-check.json")
	if err := writePrivateJSON(path, checkState{Version: "2.1.280", CheckedAt: now}); err != nil {
		t.Fatal(err)
	}
	state := readCheckState(root)
	if state.Version != "2.1.280" || shouldCheck(now.Add(time.Hour), state.CheckedAt) {
		t.Fatalf("valid recent check was not reused: %+v", state)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("check state permissions = %v, %v", info, err)
	}
	if err := os.WriteFile(path, []byte(`{"latest_version":"invalid"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if state := readCheckState(root); !shouldCheck(now, state.CheckedAt) {
		t.Fatal("invalid persisted check state blocked retry")
	}
}

func TestPruneOldVersionsKeepsActiveAndOneRollback(t *testing.T) {
	versions := t.TempDir()
	for _, version := range []string{"2.1.278", "2.1.279", "2.1.280"} {
		if err := os.Mkdir(filepath.Join(versions, version), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := pruneOldVersions(versions, "2.1.280", "2.1.279"); err != nil {
		t.Fatal(err)
	}
	for version, want := range map[string]bool{"2.1.278": false, "2.1.279": true, "2.1.280": true} {
		_, err := os.Stat(filepath.Join(versions, version))
		if (err == nil) != want {
			t.Fatalf("version %s retained = %t, want %t", version, err == nil, want)
		}
	}
}

func TestLoadCurrentUsesPrivateActiveVersion(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap isolation is Linux-only")
	}
	if err := sandboxAvailable(); err != nil {
		t.Skip(err)
	}
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	root, err := DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	version := "2.1.269"
	cli := filepath.Join(root, "versions", version, "node_modules", ".bin", "claude")
	if err := os.MkdirAll(filepath.Dir(cli), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cli, []byte("#!/bin/sh\necho '2.1.269 (Claude Code)'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeProfile(filepath.Join(root, "versions", version, "oauth-profile.json"), referenceFixture()); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateJSON(filepath.Join(root, "active.json"), activeState{Version: version}); err != nil {
		t.Fatal(err)
	}
	profile, err := LoadCurrent()
	if err != nil || profile.ClaudeVersion != version {
		t.Fatalf("private active profile = %v, %v", profile, err)
	}
	if err := writePrivateJSON(filepath.Join(root, "active.json"), activeState{Version: "2.1.270"}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCurrent(); err == nil {
		t.Fatal("missing private active executable was accepted")
	}
}
