package usage

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var providerCreditsPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$`)

// NormalizeProviderCredits validates and canonicalizes a nonnegative JSON decimal.
// Inputs are limited to 128 bytes, exponent magnitude 128, 64 integer digits,
// and 18 significant fractional places after trailing zero removal.
func NormalizeProviderCredits(value string) (string, error) {
	invalid := func() (string, error) { return "", fmt.Errorf("invalid provider credits") }
	if len(value) == 0 || len(value) > 128 {
		return invalid()
	}
	parts := providerCreditsPattern.FindStringSubmatch(value)
	if parts == nil {
		return invalid()
	}
	exponent := 0
	if parts[3] != "" {
		var err error
		exponent, err = strconv.Atoi(parts[3])
		if err != nil || exponent < -128 || exponent > 128 {
			return invalid()
		}
	}
	digits := parts[1] + parts[2]
	point := len(parts[1]) + exponent
	var integer, fraction string
	switch {
	case point <= 0:
		integer, fraction = "0", strings.Repeat("0", -point)+digits
	case point >= len(digits):
		integer = digits + strings.Repeat("0", point-len(digits))
	default:
		integer, fraction = digits[:point], digits[point:]
	}
	integer = strings.TrimLeft(integer, "0")
	if integer == "" {
		integer = "0"
	}
	fraction = strings.TrimRight(fraction, "0")
	if len(integer) > 64 || len(fraction) > 18 {
		return invalid()
	}
	if fraction != "" {
		return integer + "." + fraction, nil
	}
	return integer, nil
}
