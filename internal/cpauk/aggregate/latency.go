package aggregate

import (
	"cmp"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
)

const LatencySketchVersion = "relative-error-0.01-v1"

type LatencyBin struct {
	Index int64
	Count int64
}

func LatencyBins(values []int64) ([]LatencyBin, error) {
	counts := map[int64]int64{}
	for _, value := range values {
		if value <= 0 {
			return nil, fmt.Errorf("latency sketch values must be positive")
		}
		index := int64(math.Ceil(math.Log(float64(value)) / math.Log((1.0+0.01)/(1.0-0.01))))
		counts[index]++
	}
	result := make([]LatencyBin, 0, len(counts))
	for index, count := range counts {
		result = append(result, LatencyBin{Index: index, Count: count})
	}
	slices.SortFunc(result, func(left, right LatencyBin) int { return cmp.Compare(left.Index, right.Index) })
	return result, nil
}

func MarshalLatencyBins(bins []LatencyBin) ([]byte, error) {
	result := []byte{1}
	result = binary.AppendUvarint(result, uint64(len(bins)))
	previous := int64(0)
	for index, bin := range bins {
		if bin.Count <= 0 || index > 0 && bin.Index <= previous {
			return nil, fmt.Errorf("latency bins must be sorted, unique, and nonempty")
		}
		result = binary.AppendUvarint(result, zigzagEncode(bin.Index))
		result = binary.AppendUvarint(result, uint64(bin.Count))
		previous = bin.Index
	}
	return result, nil
}

func zigzagEncode(value int64) uint64 {
	return uint64(value<<1) ^ uint64(value>>63)
}

func Percentile(values []int64, percentile float64) *int64 {
	if len(values) == 0 {
		return nil
	}
	copyValues := slices.Clone(values)
	slices.Sort(copyValues)
	position := int(math.Ceil(percentile*float64(len(copyValues)))) - 1
	if position < 0 {
		position = 0
	}
	if position >= len(copyValues) {
		position = len(copyValues) - 1
	}
	return &copyValues[position]
}

func SampleAttemptIDs(ids []string, capacity int) ([]string, error) {
	if capacity < 0 {
		return nil, fmt.Errorf("capacity must not be negative")
	}
	result := slices.Clone(ids)
	for _, id := range result {
		if len(id) != 32 {
			return nil, fmt.Errorf("invalid attempt ID")
		}
		if _, err := hex.DecodeString(id); err != nil {
			return nil, fmt.Errorf("invalid attempt ID: %w", err)
		}
	}
	slices.Sort(result)
	if len(result) > capacity {
		result = result[:capacity]
	}
	return result, nil
}
