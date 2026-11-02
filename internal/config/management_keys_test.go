package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestManagementKeysLoadHashAndUpload(t *testing.T) {
	for _, section := range []string{"management", "remote-management"} {
		t.Run(section, func(t *testing.T) {
			raw := "# keep operator comment\nport: 8317\n" + section + ":\n  secret-key: legacy-secret\n  # keep key comment\n  keys:\n    - id: viewer\n      name: Dashboard\n      secret-key: viewer-secret\n      scope: read-only\n    - id: admin\n      name: Administrator\n      secret-key: admin-secret\n      scope: full-access\n"
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if err := PrepareConfigFileForUpload(path); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.RemoteManagement.HasKeys() || len(cfg.RemoteManagement.Keys) != 2 {
				t.Fatal("missing keys")
			}
			for i, plain := range []string{"viewer-secret", "admin-secret"} {
				if bcrypt.CompareHashAndPassword([]byte(cfg.RemoteManagement.Keys[i].SecretKey), []byte(plain)) != nil {
					t.Fatal("unhashed key")
				}
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range []string{"viewer-secret", "admin-secret", "legacy-secret"} {
				if strings.Contains(string(saved), text) {
					t.Fatal("plaintext persisted")
				}
			}
			for _, text := range []string{"# keep operator comment", "# keep key comment"} {
				if !strings.Contains(string(saved), text) {
					t.Fatalf("lost comment %s", text)
				}
			}
			if err := PrepareConfigFileForUpload(path); err != nil {
				t.Fatal(err)
			}
			again, _ := os.ReadFile(path)
			if string(saved) != string(again) {
				t.Fatal("non-idempotent hashing")
			}
			metadata, _ := json.Marshal(cfg.RemoteManagement.Keys)
			if strings.Contains(string(metadata), "$2") || strings.Contains(string(metadata), "secret") {
				t.Fatal("metadata leaked secrets")
			}
		})
	}
}

func TestManagementKeysValidationFailsClosed(t *testing.T) {
	for _, fields := range []string{
		"id: a, name: Viewer, secret-key: secret, scope: admin",
		"id: a, name: Viewer, secret-key: secret",
		"id: a, name: Viewer, secret-key: secret, scope: ''",
		"id: a, name: Viewer, secret-key: secret, scope: READ-ONLY",
		"name: Viewer, secret-key: secret, scope: read-only",
		"id: a, name: Viewer, secret-key: '', scope: read-only",
		"id: a, name: Viewer, secret-key: '$2a$invalid', scope: read-only",
	} {
		raw := []byte("management:\n  keys: [{" + fields + "}]\n")
		if _, err := ParseConfigBytes(raw); err == nil {
			t.Fatalf("accepted %s", fields)
		}
		if _, err := prepareConfigForUpload(raw); err == nil {
			t.Fatalf("upload accepted %s", fields)
		}
	}
	duplicate := []byte("management:\n  keys:\n  - {id: a, name: A, secret-key: first, scope: full-access}\n  - {id: a, name: B, secret-key: second, scope: read-only}\n")
	if _, err := ParseConfigBytes(duplicate); err == nil {
		t.Fatal("accepted duplicate IDs")
	}
}

func TestManagementKeysRejectPlaintextAliases(t *testing.T) {
	for _, raw := range []string{
		"password: &password sensitive-key\nmanagement:\n  keys: [{id: a, name: A, secret-key: *password, scope: read-only}]\n",
		"shared: &keys [{id: a, name: A, secret-key: sensitive-key, scope: read-only}]\nmanagement:\n  keys: *keys\n",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatal("startup accepted plaintext aliases")
		}
		if err := PrepareConfigFileForUpload(path); err == nil {
			t.Fatal("upload accepted plaintext aliases")
		}
		after, _ := os.ReadFile(path)
		if string(after) != raw {
			t.Fatal("failed preparation modified file")
		}
	}
}
