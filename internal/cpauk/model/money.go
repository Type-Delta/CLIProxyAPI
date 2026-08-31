package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// NanoUSD stores one billionth of a US dollar. JSON uses a decimal string so
// JavaScript and Go never pass currency through binary floating point.
type NanoUSD int64

const nanosPerUSD int64 = 1_000_000_000

func (n NanoUSD) String() string {
	negative := n < 0
	value := int64(n)
	var magnitude uint64
	if negative {
		magnitude = uint64(-(value + 1)) + 1
	} else {
		magnitude = uint64(value)
	}
	whole := magnitude / uint64(nanosPerUSD)
	fraction := magnitude % uint64(nanosPerUSD)
	result := strconv.FormatUint(whole, 10)
	if fraction != 0 {
		result += "." + strings.TrimRight(fmt.Sprintf("%09d", fraction), "0")
	}
	if negative {
		return "-" + result
	}
	return result
}

func ParseNanoUSD(value string) (NanoUSD, error) {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "eE+") {
		return 0, fmt.Errorf("invalid nano-USD decimal %q", value)
	}
	negative := strings.HasPrefix(value, "-")
	if negative {
		value = strings.TrimPrefix(value, "-")
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" || len(parts) == 2 && (parts[1] == "" || len(parts[1]) > 9) {
		return 0, fmt.Errorf("invalid nano-USD decimal")
	}
	for _, part := range parts {
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return 0, fmt.Errorf("invalid nano-USD decimal")
			}
		}
	}
	whole := new(big.Int)
	if _, ok := whole.SetString(parts[0], 10); !ok {
		return 0, fmt.Errorf("invalid nano-USD whole units")
	}
	whole.Mul(whole, big.NewInt(nanosPerUSD))
	if len(parts) == 2 {
		fraction := parts[1] + strings.Repeat("0", 9-len(parts[1]))
		fractionValue := new(big.Int)
		fractionValue.SetString(fraction, 10)
		whole.Add(whole, fractionValue)
	}
	if negative {
		whole.Neg(whole)
	}
	if !whole.IsInt64() {
		return 0, fmt.Errorf("nano-USD value out of range")
	}
	return NanoUSD(whole.Int64()), nil
}

func (n NanoUSD) MarshalJSON() ([]byte, error) {
	return json.Marshal(n.String())
}

func (n *NanoUSD) UnmarshalJSON(data []byte) error {
	if n == nil {
		return fmt.Errorf("unmarshal nano-USD into nil receiver")
	}
	if bytes.Equal(data, []byte("null")) {
		return fmt.Errorf("nano-USD cannot be null")
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("nano-USD must be a decimal string: %w", err)
	}
	parsed, err := ParseNanoUSD(value)
	if err != nil {
		return err
	}
	*n = parsed
	return nil
}

// CostForTokens applies a USD-per-million-token price. It rounds once per
// event to the nearest nano-USD, with exact halves rounded away from zero.
func CostForTokens(tokens int64, pricePerMillion NanoUSD) (NanoUSD, error) {
	if tokens < 0 {
		return 0, fmt.Errorf("tokens must not be negative")
	}
	numerator := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(int64(pricePerMillion)))
	denominator := big.NewInt(1_000_000)
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, denominator, remainder)
	if new(big.Int).Lsh(new(big.Int).Abs(remainder), 1).Cmp(denominator) >= 0 {
		if numerator.Sign() < 0 {
			quotient.Sub(quotient, big.NewInt(1))
		} else {
			quotient.Add(quotient, big.NewInt(1))
		}
	}
	if !quotient.IsInt64() {
		return 0, fmt.Errorf("event cost out of range")
	}
	return NanoUSD(quotient.Int64()), nil
}
