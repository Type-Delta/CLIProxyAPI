package config

import (
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

const ManagementScopeFullAccess = "full-access"
const ManagementScopeReadOnly = "read-only"

// ManagementKey stores a named management credential. SecretKey is never JSON metadata.
type ManagementKey struct {
	ID        string `yaml:"id" json:"id"`
	Name      string `yaml:"name" json:"name"`
	SecretKey string `yaml:"secret-key" json:"-"`
	Scope     string `yaml:"scope" json:"scope"`
}

func ValidManagementScope(scope string) bool {
	return scope == ManagementScopeFullAccess || scope == ManagementScopeReadOnly
}

func (m RemoteManagement) HasKeys() bool {
	if m.SecretKey != "" {
		return true
	}
	for _, key := range m.Keys {
		if key.SecretKey != "" && ValidManagementScope(key.Scope) {
			return true
		}
	}
	return false
}

// NormalizeKeys validates before hashing so invalid scopes cannot gain access.
func (m *RemoteManagement) NormalizeKeys() error {
	ids := make(map[string]bool, len(m.Keys))
	for i := range m.Keys {
		key := &m.Keys[i]
		key.ID = strings.TrimSpace(key.ID)
		key.Name = strings.TrimSpace(key.Name)
		if key.ID == "" || ids[key.ID] || key.Name == "" || key.SecretKey == "" || !ValidManagementScope(key.Scope) {
			return fmt.Errorf("invalid management key metadata or scope at index %d", i)
		}
		ids[key.ID] = true
		if !looksLikeBcrypt(key.SecretKey) {
			hashed, err := hashSecret(key.SecretKey)
			if err != nil {
				return fmt.Errorf("hash management key at index %d: %w", i, err)
			}
			key.SecretKey = hashed
		} else if _, err := bcrypt.Cost([]byte(key.SecretKey)); err != nil {
			return fmt.Errorf("invalid management key hash at index %d", i)
		}
	}
	return nil
}

// prepareManagementKeysYAML replaces only the effective list, retaining surrounding
// comments and refusing aliases that would leave plaintext credentials elsewhere.
func prepareManagementKeysYAML(data []byte, keys []ManagementKey) ([]byte, error) {
	if len(keys) == 0 {
		return data, nil
	}
	var source Config
	if err := yaml.Unmarshal(data, &source); err != nil {
		return nil, err
	}
	changed := false
	for i := range keys {
		if source.RemoteManagement.Keys[i].SecretKey != keys[i].SecretKey {
			changed = true
		}
	}
	if !changed {
		return data, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	root := expandConfigAliases(doc.Content[0])
	section := "remote-management"
	if yamlPath(root, "management.keys") != nil {
		section = "management"
	}
	node := yamlPath(root, section+".keys")
	if node == nil {
		return nil, fmt.Errorf("management keys missing")
	}
	for i, key := range keys {
		secret := yamlPath(node.Content[i], "secret-key")
		if secret == nil {
			return nil, fmt.Errorf("management key secret missing")
		}
		secret.Value = key.SecretKey
		secret.Tag = "!!str"
	}
	doc.Content[0] = root
	result, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, err
	}
	for _, key := range source.RemoteManagement.Keys {
		if !looksLikeBcrypt(key.SecretKey) && yamlScalarExists(&doc, key.SecretKey) {
			return nil, fmt.Errorf("plaintext management key remains elsewhere in config")
		}
	}
	return result, nil
}

// PrepareManagementKeysYAML validates and hashes scoped keys for raw config writes.
func PrepareManagementKeysYAML(data []byte) ([]byte, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if err := cfg.RemoteManagement.NormalizeKeys(); err != nil {
		return nil, err
	}
	return prepareManagementKeysYAML(data, cfg.RemoteManagement.Keys)
}
