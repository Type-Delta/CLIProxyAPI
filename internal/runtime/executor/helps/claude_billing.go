package helps

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/MoeForge/moego/ojson"
)

// ClaudeBillingFingerprint derives the three-character cc_version suffix from
// the first user text and client version, using Claude Code's UTF-16 sampling.
func ClaudeBillingFingerprint(messageText, version string) string {
	const salt = "59cf53e54c78"
	units := utf16.Encode([]rune(messageText))
	var sampled [3]uint16
	for i, index := range [...]int{4, 7, 20} {
		sampled[i] = '0'
		if index < len(units) {
			sampled[i] = units[index]
		}
	}
	digest := sha256.Sum256([]byte(salt + string(utf16.Decode(sampled[:])) + version))
	return hex.EncodeToString(digest[:])[:3]
}

// ApplyClaudeDeviceProfileBillingDoc normalizes existing attribution before
// payload rules and signing. messageText excludes injected context reminders.
func ApplyClaudeDeviceProfileBillingDoc(doc *ojson.Doc, profile ClaudeDeviceProfile, messageText string) {
	if doc == nil {
		return
	}
	parsed, ok := parseClaudeCodeUserAgent(profile.UserAgent)
	if !ok {
		return
	}
	version := strconv.Itoa(parsed.version.major) + "." + strconv.Itoa(parsed.version.minor) + "." + strconv.Itoa(parsed.version.patch)
	system := doc.Get("system")
	blocks := system.Array()
	if system.Type == ojson.String {
		blocks = []ojson.Result{system}
	}
	for i, block := range blocks {
		text := block.Get("text").String()
		path := "system." + strconv.Itoa(i) + ".text"
		if system.Type == ojson.String {
			text, path = block.String(), "system"
		}
		content, found := strings.CutPrefix(text, "x-anthropic-billing-header:")
		if !found {
			continue
		}
		fields := strings.Split(content, ";")
		changed := false
		for j, field := range fields {
			key, value, hasValue := strings.Cut(field, "=")
			if !hasValue {
				continue
			}
			value = strings.TrimSpace(value)
			replacement := value
			switch strings.TrimSpace(key) {
			case "cc_version":
				parts := strings.Split(value, ".")
				if len(parts) >= 3 && strings.Join(parts[:3], ".") == version {
					continue
				}
				replacement = version
				if len(parts) == 4 {
					replacement += "." + ClaudeBillingFingerprint(messageText, version)
				}
			case "cc_entrypoint":
				replacement = parsed.entrypoint
			default:
				continue
			}
			if replacement != value {
				fields[j] = key + "=" + replacement
				changed = true
			}
		}
		if changed {
			_ = doc.Set(path, "x-anthropic-billing-header:"+strings.Join(fields, ";"))
		}
	}
}

// ValidateClaudeDeviceProfileBilling checks each final attribution field
// independently, without rewriting payload-rule results or depending on order.
func ValidateClaudeDeviceProfileBilling(body []byte, userAgent string) error {
	profile, valid := parseClaudeCodeUserAgent(userAgent)
	if !valid {
		// Explicit opaque fallback identities have no native billing identity
		// to normalize or compare. Their configured transport remains supported.
		return nil
	}
	system := ojson.GetBytesNoCopy(body, "system")
	blocks := system.Array()
	if system.Type == ojson.String {
		blocks = []ojson.Result{system}
	}
	for _, block := range blocks {
		text := block.Get("text").String()
		if system.Type == ojson.String {
			text = block.String()
		}
		content, found := strings.CutPrefix(text, "x-anthropic-billing-header:")
		if !found {
			continue
		}
		for _, field := range strings.Split(content, ";") {
			key, value, hasValue := strings.Cut(field, "=")
			if !hasValue {
				continue
			}
			value = strings.TrimSpace(value)
			switch strings.TrimSpace(key) {
			case "cc_version":
				parts := strings.Split(value, ".")
				if len(parts) < 3 || len(parts) > 4 {
					return fmt.Errorf("final Claude billing version is invalid")
				}
				version, ok := parseClaudeSemver(strings.Join(parts[:3], "."))
				if !ok || version.Compare(profile.version) != 0 {
					return fmt.Errorf("final Claude billing version conflicts with the stabilized client profile")
				}
			case "cc_entrypoint":
				if !strings.EqualFold(value, profile.entrypoint) {
					return fmt.Errorf("final Claude billing entrypoint %q conflicts with stabilized client %q", value, profile.entrypoint)
				}
			}
		}
	}
	return nil
}
