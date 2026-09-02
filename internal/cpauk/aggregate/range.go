package aggregate

import (
	"fmt"
	"time"
)

type RangeKind string

const (
	RangeToday        RangeKind = "today"
	RangeYesterday    RangeKind = "yesterday"
	RangeCalendarWeek RangeKind = "calendar_week"
	RangeRolling      RangeKind = "rolling"
)

func ResolveRange(kind RangeKind, now time.Time, zoneName string, rolling time.Duration) (time.Time, time.Time, error) {
	location, err := time.LoadLocation(zoneName)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("load time zone %q: %w", zoneName, err)
	}
	localNow := now.In(location)
	day := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, location)
	var start, end time.Time
	switch kind {
	case RangeToday:
		start, end = day, day.AddDate(0, 0, 1)
	case RangeYesterday:
		start, end = day.AddDate(0, 0, -1), day
	case RangeCalendarWeek:
		daysSinceMonday := (int(localNow.Weekday()) + 6) % 7
		start = day.AddDate(0, 0, -daysSinceMonday)
		end = start.AddDate(0, 0, 7)
	case RangeRolling:
		if rolling <= 0 {
			return time.Time{}, time.Time{}, fmt.Errorf("rolling duration must be positive")
		}
		end = now
		start = now.Add(-rolling)
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("unsupported range kind %q", kind)
	}
	return start.UTC(), end.UTC(), nil
}

// BucketBounds uses calendar arithmetic for day and week widths. That keeps
// bucket boundaries correct across daylight-saving changes.
func BucketBounds(at time.Time, zoneName, width string) (time.Time, time.Time, error) {
	location, err := time.LoadLocation(zoneName)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("load time zone %q: %w", zoneName, err)
	}
	local := at.In(location)
	var start, end time.Time
	switch width {
	case "1d":
		start = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
		end = start.AddDate(0, 0, 1)
	case "1w":
		daysSinceMonday := (int(local.Weekday()) + 6) % 7
		start = time.Date(local.Year(), local.Month(), local.Day()-daysSinceMonday, 0, 0, 0, 0, location)
		end = start.AddDate(0, 0, 7)
	default:
		duration, err := time.ParseDuration(width)
		if err != nil || duration <= 0 {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid bucket width %q", width)
		}
		_, offsetSeconds := local.Zone()
		offset := time.Duration(offsetSeconds) * time.Second
		start = at.UTC().Add(offset).Truncate(duration).Add(-offset)
		end = start.Add(duration)
	}
	return start.UTC(), end.UTC(), nil
}
