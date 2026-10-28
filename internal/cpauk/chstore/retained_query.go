package chstore

import (
	"fmt"
	"time"
)

func validateRollupWidth(grain, width string) error {
	minimum := time.Hour
	if grain == "daily" {
		minimum = 24 * time.Hour
	}
	if width == "1d" || width == "1w" {
		return nil
	}
	duration, err := time.ParseDuration(width)
	if err != nil || duration < minimum {
		return fmt.Errorf("%s retained analytics cannot answer %s buckets exactly", grain, width)
	}
	return nil
}
