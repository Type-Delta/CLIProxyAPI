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
		start = localCalendarBoundary(local, b.location)
		end = localCalendarAdd(start, 0, 0, 1, b.location)
	case "1w":
		daysSinceMonday := (int(local.Weekday()) + 6) % 7
		start = localCalendarAdd(local, 0, 0, -daysSinceMonday, b.location)
		end = localCalendarAdd(start, 0, 0, 7, b.location)
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
	day := localCalendarBoundary(localNow, location)
	var start, end time.Time
	switch kind {
	case RangeToday:
		start, end = day, localCalendarAdd(day, 0, 0, 1, location)
	case RangeYesterday:
		end = day
		start = localCalendarAdd(end, 0, 0, -1, location)
	case RangeCalendarWeek:
		daysSinceMonday := (int(localNow.Weekday()) + 6) % 7
		start = localCalendarAdd(day, 0, 0, -daysSinceMonday, location)
		end = localCalendarAdd(start, 0, 0, 7, location)
	case RangePreviousWeek:
		daysSinceMonday := (int(localNow.Weekday()) + 6) % 7
		end = localCalendarAdd(day, 0, 0, -daysSinceMonday, location)
		start = localCalendarAdd(end, 0, 0, -7, location)
	case RangeCalendarMonth:
		start = calendarBoundary(localNow.Year(), localNow.Month(), 1, location)
		end = localCalendarAdd(start, 0, 1, 0, location)
	case RangePreviousMonth:
		end = calendarBoundary(localNow.Year(), localNow.Month(), 1, location)
		start = localCalendarAdd(end, 0, -1, 0, location)
	case RangeCalendarYear:
		start = calendarBoundary(localNow.Year(), time.January, 1, location)
		end = localCalendarAdd(start, 1, 0, 0, location)
	case RangePreviousYear:
		end = calendarBoundary(localNow.Year(), time.January, 1, location)
		start = localCalendarAdd(end, -1, 0, 0, location)
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

func localCalendarBoundary(at time.Time, location *time.Location) time.Time {
	local := at.In(location)
	return calendarBoundary(local.Year(), local.Month(), local.Day(), location)
}

func localCalendarAdd(at time.Time, years, months, days int, location *time.Location) time.Time {
	local := at.In(location)
	probe := time.Date(local.Year()+years, local.Month()+time.Month(months), local.Day()+days, 12, 0, 0, 0, location)
	return localCalendarBoundary(probe, location)
}

func calendarBoundary(year int, month time.Month, day int, location *time.Location) time.Time {
	boundary := time.Date(year, month, day, 0, 0, 0, 0, location)
	resolved := boundary.In(location)
	if resolved.Year() == year && resolved.Month() == month && resolved.Day() == day {
		return boundary
	}
	// Some zones skip local midnight when daylight saving starts. Go resolves
	// that nonexistent wall time to the previous day; advance until the first
	// instant that belongs to the requested calendar day.
	for step := time.Hour; step <= 48*time.Hour; step += time.Hour {
		candidate := boundary.Add(step)
		resolved = candidate.In(location)
		if resolved.Year() == year && resolved.Month() == month && resolved.Day() == day {
			return candidate
		}
	}
	return boundary
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
