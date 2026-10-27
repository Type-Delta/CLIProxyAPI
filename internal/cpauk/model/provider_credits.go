package model

import "strings"

// ValidProviderCredits checks the canonical decimal stored in an analytics event.
func ValidProviderCredits(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) > 2 || len(parts[0]) == 0 || len(parts[0]) > 64 || len(parts[0]) > 1 && parts[0][0] == '0' {
		return false
	}
	if len(parts) == 2 && (len(parts[1]) == 0 || len(parts[1]) > 18 || parts[1][len(parts[1])-1] == '0') {
		return false
	}
	for _, part := range parts {
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return false
			}
		}
	}
	return true
}
