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
	return m.normalizeKeys(true)
}

func (m *RemoteManagement) normalizeKeys(hash bool) error {
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
			if strings.HasPrefix(key.SecretKey, "$2a$") || strings.HasPrefix(key.SecretKey, "$2b$") || strings.HasPrefix(key.SecretKey, "$2y$") {
				return fmt.Errorf("invalid management key hash at index %d", i)
			}
			if len(key.SecretKey) > 72 {
				return fmt.Errorf("hash management key at index %d: %w", i, bcrypt.ErrPasswordTooLong)
			}
			if !hash {
				continue
			}
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

// PrepareManagementKeysYAML validates and hashes scoped keys for raw config writes.
func PrepareManagementKeysYAML(data []byte) ([]byte, error) {
	docs, err := decodeConfigStream(data)
	if err != nil || len(docs) == 0 {
		return data, err
	}
	var cfg Config
	if errDecode := docs[0].Decode(&cfg); errDecode != nil {
		return nil, errDecode
	}
	original := append([]ManagementKey(nil), cfg.RemoteManagement.Keys...)
	if errKeys := cfg.RemoteManagement.NormalizeKeys(); errKeys != nil {
		return nil, errKeys
	}
	result, err := renderScopedManagementKeys(data, docs, original, cfg.RemoteManagement.Keys)
	if err != nil {
		return nil, err
	}
	if errSave := ValidateConfigSave(data, result); errSave != nil {
		return nil, errSave
	}
	return result, nil
}

func renderScopedManagementKeys(data []byte, docs []*yaml.Node, original, keys []ManagementKey) ([]byte, error) {
	changed := false
	for i, key := range keys {
		if original[i].SecretKey != key.SecretKey {
			changed = true
			break
		}
	}
	if !changed {
		return data, nil
	}
	doc := deepCopyNode(docs[0])
	root := doc.Content[0]
	section := "remote-management"
	if effectiveConfigValue(root, "management", "keys") != nil {
		section = "management"
	}
	effective := effectiveConfigValue(root, section, "keys")
	if effective == nil || effective.Kind != yaml.SequenceNode || len(effective.Content) != len(keys) {
		return nil, fmt.Errorf("management keys missing")
	}
	list := detachedConfigNode(effective)
	for i, key := range keys {
		if original[i].SecretKey != key.SecretKey {
			setConfigScalar(list.Content[i], []string{"secret-key"}, key.SecretKey)
		}
	}
	setConfigScalar(root, []string{section, "keys"}, "")
	target := effectiveConfigValue(root, section, "keys")
	target.Kind, target.Tag, target.Value, target.Content = list.Kind, list.Tag, list.Value, list.Content
	target.Style = list.Style
	return encodeConfigDocument(doc, data, docs)
}
