package aggregate

import (
	"fmt"
	"time"
)

type RangeKind string

const (
	RangeToday         RangeKind = "today"
	RangeYesterday     RangeKind = "yesterday"
	RangeCalendarWeek  RangeKind = "calendar_week"
	RangePreviousWeek  RangeKind = "previous_week"
	RangeCalendarMonth RangeKind = "calendar_month"
	RangePreviousMonth RangeKind = "previous_month"
	RangeCalendarYear  RangeKind = "calendar_year"
	RangePreviousYear  RangeKind = "previous_year"
	RangeRolling       RangeKind = "rolling"
)

// Bucketer resolves a query's time zone and fixed width once, then reuses the
// parsed values for every event in that query. This keeps the hot aggregation
// loop free of repeated time.LoadLocation and time.ParseDuration calls.
type Bucketer struct {
	location *time.Location
	width    string
	duration time.Duration
}

func NewBucketer(zoneName, width string) (Bucketer, error) {
	location, err := time.LoadLocation(zoneName)
	if err != nil {
		return Bucketer{}, fmt.Errorf("load time zone %q: %w", zoneName, err)
	}
	bucketer := Bucketer{location: location, width: width}
	if width != "1d" && width != "1w" {
		duration, err := time.ParseDuration(width)
		if err != nil || duration <= 0 {
			return Bucketer{}, fmt.Errorf("invalid bucket width %q", width)
		}
		bucketer.duration = duration
	}
	return bucketer, nil
}

func (b Bucketer) Bounds(at time.Time) (time.Time, time.Time, error) {
	local := at.In(b.location)
	var start, end time.Time
	switch b.width {
	case "1d":
		start = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, b.location)
		end = start.AddDate(0, 0, 1)
	case "1w":
		daysSinceMonday := (int(local.Weekday()) + 6) % 7
		start = time.Date(local.Year(), local.Month(), local.Day()-daysSinceMonday, 0, 0, 0, 0, b.location)
		end = start.AddDate(0, 0, 7)
	default:
		_, offsetSeconds := local.Zone()
		offset := time.Duration(offsetSeconds) * time.Second
		start = at.UTC().Add(offset).Truncate(b.duration).Add(-offset)
		end = start.Add(b.duration)
	}
	return start.UTC(), end.UTC(), nil
}

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
	case RangePreviousWeek:
		daysSinceMonday := (int(localNow.Weekday()) + 6) % 7
		end = day.AddDate(0, 0, -daysSinceMonday)
		start = end.AddDate(0, 0, -7)
	case RangeCalendarMonth:
		start = time.Date(localNow.Year(), localNow.Month(), 1, 0, 0, 0, 0, location)
		end = start.AddDate(0, 1, 0)
	case RangePreviousMonth:
		end = time.Date(localNow.Year(), localNow.Month(), 1, 0, 0, 0, 0, location)
		start = end.AddDate(0, -1, 0)
	case RangeCalendarYear:
		start = time.Date(localNow.Year(), time.January, 1, 0, 0, 0, 0, location)
		end = start.AddDate(1, 0, 0)
	case RangePreviousYear:
		end = time.Date(localNow.Year(), time.January, 1, 0, 0, 0, 0, location)
		start = end.AddDate(-1, 0, 0)
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
	bucketer, err := NewBucketer(zoneName, width)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return bucketer.Bounds(at)
}
