package chstore

import (
	"fmt"
	"math"
	"math/big"
	"strings"
)

func percentage(value, total int64) string {
	if total <= 0 || value <= 0 {
		return "0"
	}
	numerator := new(big.Int).Mul(big.NewInt(value), big.NewInt(100))
	ratio := new(big.Rat).SetFrac(numerator, big.NewInt(total))
	text := ratio.FloatString(9)
	return strings.TrimRight(strings.TrimRight(text, "0"), ".")
}

func optionalPercentage(value, total int64) *string {
	if total == 0 {
		return nil
	}
	numerator := new(big.Int).Mul(big.NewInt(value), big.NewInt(100))
	result := new(big.Rat).SetFrac(numerator, big.NewInt(total)).FloatString(18)
	result = strings.TrimRight(strings.TrimRight(result, "0"), ".")
	if point := strings.IndexByte(result, '.'); point >= 0 && len(result)-point-1 > 2 {
		result = new(big.Rat).SetFrac(numerator, big.NewInt(total)).FloatString(2)
		result = strings.TrimRight(strings.TrimRight(result, "0"), ".")
	}
	return &result
}

// optionalCacheReadRate uses only rows with an exact token breakdown. This lets
// mixed-provider ranges retain cache data from providers that reported it.
func optionalCacheReadRate(cacheRead, input int64) *string {
	return optionalPercentage(cacheRead, input)
}

func optionalPercentageFixed(value, total int64, decimals int) *string {
	if total == 0 {
		return nil
	}
	numerator := new(big.Int).Mul(big.NewInt(value), big.NewInt(100))
	result := new(big.Rat).SetFrac(numerator, big.NewInt(total)).FloatString(decimals)
	return &result
}

func decimalRatio(numerator, denominator int64) string {
	if numerator == 0 || denominator == 0 {
		return "0"
	}
	result := new(big.Rat).SetFrac(big.NewInt(numerator), big.NewInt(denominator)).FloatString(18)
	return strings.TrimRight(strings.TrimRight(result, "0"), ".")
}

func decimalProductsRatio(numeratorA, numeratorB, denominatorA, denominatorB int64) string {
	if numeratorA == 0 || numeratorB == 0 || denominatorA == 0 || denominatorB == 0 {
		return "0"
	}
	numerator := new(big.Int).Mul(big.NewInt(numeratorA), big.NewInt(numeratorB))
	denominator := new(big.Int).Mul(big.NewInt(denominatorA), big.NewInt(denominatorB))
	result := new(big.Rat).SetFrac(numerator, denominator).FloatString(18)
	return strings.TrimRight(strings.TrimRight(result, "0"), ".")
}

func addMoney(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b || b < 0 && a < math.MinInt64-b {
		return 0, fmt.Errorf("integer overflow in analytics money total")
	}
	return a + b, nil
}
