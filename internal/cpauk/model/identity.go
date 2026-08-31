package model

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	KeyIDAlgorithm        = "sha256-v1"
	CredentialIDAlgorithm = "hmac-sha256-v1"
	ShortKeyIDMinLength   = 12
)

type RequestIDQuality string

const (
	RequestIDObserved  RequestIDQuality = "observed"
	RequestIDSynthetic RequestIDQuality = "synthetic"
)

// KeyID returns the canonical identifier for a configured inbound key.
func KeyID(raw string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return hex.EncodeToString(sum[:])
}

func IsFullKeyID(value string) bool {
	return isLowerHex(value, sha256.Size*2)
}

func IsCorrelationID(value string) bool {
	return isLowerHex(value, 16*2)
}

func NewCorrelationID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate correlation ID: %w", err)
	}
	return hex.EncodeToString(value), nil
}

// CredentialID derives the privacy-preserving credential identity. AuthIndex
// takes precedence over AuthID. A missing source identity returns nil.
func CredentialID(identityKey []byte, provider, authIndex, authID string) (*string, error) {
	if len(identityKey) != 32 {
		return nil, fmt.Errorf("identity key must contain 32 bytes")
	}
	selector := ""
	identity := ""
	if value := strings.TrimSpace(authIndex); value != "" {
		selector, identity = "auth_index", value
	} else if value := strings.TrimSpace(authID); value != "" {
		selector, identity = "auth_id", value
	} else {
		return nil, nil
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	message := "credential-id-v1\x00" + provider + "\x00" + selector + "\x00" + identity
	mac := hmac.New(sha256.New, identityKey)
	_, _ = mac.Write([]byte(message))
	value := hex.EncodeToString(mac.Sum(nil))
	return &value, nil
}

func IdentityKeyFingerprint(identityKey []byte) (string, error) {
	if len(identityKey) != 32 {
		return "", fmt.Errorf("identity key must contain 32 bytes")
	}
	sum := sha256.Sum256(identityKey)
	return hex.EncodeToString(sum[:]), nil
}

func isLowerHex(value string, length int) bool {
	if len(value) != length || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// ShortKeyIDs returns the shortest even-length unique prefix for every
// distinct full ID. Duplicate full IDs intentionally share one display ID.
func ShortKeyIDs(fullIDs []string) (map[string]string, error) {
	unique := make(map[string]struct{}, len(fullIDs))
	for _, fullID := range fullIDs {
		if !IsFullKeyID(fullID) {
			return nil, fmt.Errorf("invalid key ID %q", fullID)
		}
		unique[fullID] = struct{}{}
	}

	result := make(map[string]string, len(unique))
	for fullID := range unique {
		length := ShortKeyIDMinLength
		for length < len(fullID) {
			prefix := fullID[:length]
			collision := false
			for otherID := range unique {
				if otherID != fullID && strings.HasPrefix(otherID, prefix) {
					collision = true
					break
				}
			}
			if !collision {
				break
			}
			length += 2
		}
		if length > len(fullID) {
			length = len(fullID)
		}
		result[fullID] = fullID[:length]
	}
	return result, nil
}
