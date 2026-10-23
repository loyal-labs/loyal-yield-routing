package webapp

import (
	"strconv"
	"time"
)

// Zoned-time helpers porting the Intl.DateTimeFormat arithmetic of
// earnings-calculator.server.ts (getZonedParts, zonedDateTimeToUtc,
// createEarningsBuckets). Buckets and labels are part of the served JSON, so
// the arithmetic must agree with the TS hour by hour.

type zonedParts struct {
	year   int
	month  int
	day    int
	hour   int
	minute int
	second int
}

// normalizeEarningsTimezone mirrors normalizeEarningsTimezone
// (earnings-calculator.server.ts:119): an unset or unparseable zone falls back
// to UTC.
func normalizeEarningsTimezone(timezone string) string {
	if timezone == "" {
		return "UTC"
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return "UTC"
	}
	return timezone
}

func zonedLocation(timezone string) *time.Location {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

func getZonedParts(t time.Time, timezone string) zonedParts {
	local := t.In(zonedLocation(timezone))
	return zonedParts{
		year:   local.Year(),
		month:  int(local.Month()),
		day:    local.Day(),
		hour:   local.Hour(),
		minute: local.Minute(),
		second: local.Second(),
	}
}

// asUTCMillis is JS Date.UTC: the wall clock read as if it were UTC.
func (p zonedParts) asUTCMillis() int64 {
	return time.Date(p.year, time.Month(p.month), p.day, p.hour, p.minute, p.second, 0, time.UTC).UnixMilli()
}

func getTimezoneOffsetMs(t time.Time, timezone string) int64 {
	return getZonedParts(t, timezone).asUTCMillis() - t.UnixMilli()
}

func zonedDateTimeToUtc(timezone string, p zonedParts) time.Time {
	localAsUtc := p.asUTCMillis()
	utcMs := localAsUtc - getTimezoneOffsetMs(msTimeMillis(localAsUtc), timezone)
	for i := 0; i < 2; i++ {
		utcMs = localAsUtc - getTimezoneOffsetMs(msTimeMillis(utcMs), timezone)
	}
	return msTimeMillis(utcMs)
}

func (p zonedParts) addLocalDays(days int) zonedParts {
	date := time.Date(p.year, time.Month(p.month), p.day+days, 0, 0, 0, 0, time.UTC)
	return zonedParts{year: date.Year(), month: int(date.Month()), day: date.Day()}
}

func (p zonedParts) addLocalMonths(months int) zonedParts {
	date := time.Date(p.year, time.Month(p.month+months), 1, 0, 0, 0, 0, time.UTC)
	return zonedParts{year: date.Year(), month: int(date.Month()), day: 1}
}

func startOfLocalDay(t time.Time, timezone string) time.Time {
	p := getZonedParts(t, timezone)
	return zonedDateTimeToUtc(timezone, zonedParts{year: p.year, month: p.month, day: p.day})
}

func startOfLocalMonth(t time.Time, timezone string) time.Time {
	p := getZonedParts(t, timezone)
	return zonedDateTimeToUtc(timezone, zonedParts{year: p.year, month: p.month, day: 1})
}

// englishMonthShort is Intl.DateTimeFormat("en-US", {month: "short"}).
func englishMonthShort(month int) string {
	return time.Month(month).String()[:3]
}

func formatBucketLabel(startAt time.Time, rangeID, timezone string) string {
	local := startAt.In(zonedLocation(timezone))
	if rangeID == "1Y" || rangeID == "ALL" {
		return englishMonthShort(int(local.Month())) + " " + strconv.Itoa(local.Year())
	}
	return englishMonthShort(int(local.Month())) + " " + strconv.Itoa(local.Day())
}

type earningsBucket struct {
	endAt     time.Time
	isCurrent bool
	label     string
	startAt   time.Time
}

func createEarningsBuckets(firstDepositAt *time.Time, now time.Time, rangeID, timezone string) []earningsBucket {
	todayStart := startOfLocalDay(now, timezone)
	currentMonthStart := startOfLocalMonth(now, timezone)
	todayParts := getZonedParts(todayStart, timezone)
	currentMonthParts := getZonedParts(currentMonthStart, timezone)

	var starts []time.Time
	switch rangeID {
	case "7D", "30D":
		count := 30
		if rangeID == "7D" {
			count = 7
		}
		for i := count - 1; i >= 0; i-- {
			starts = append(starts, zonedDateTimeToUtc(timezone, todayParts.addLocalDays(-i)))
		}
	default:
		firstMonthStart := currentMonthStart
		if firstDepositAt != nil {
			firstMonthStart = startOfLocalMonth(*firstDepositAt, timezone)
		}
		firstMonthParts := getZonedParts(firstMonthStart, timezone)
		monthCount := 1
		if rangeID == "1Y" {
			monthCount = 12
		} else {
			span := (currentMonthParts.year-firstMonthParts.year)*12 + currentMonthParts.month - firstMonthParts.month + 1
			if span > monthCount {
				monthCount = span
			}
		}
		firstOffset := 0
		baseParts := firstMonthParts
		if rangeID == "1Y" {
			firstOffset = -(monthCount - 1)
			baseParts = currentMonthParts
		}
		for i := 0; i < monthCount; i++ {
			starts = append(starts, zonedDateTimeToUtc(timezone, baseParts.addLocalMonths(firstOffset+i)))
		}
	}

	buckets := make([]earningsBucket, 0, len(starts))
	for _, startAt := range starts {
		var nextStart time.Time
		if rangeID == "7D" || rangeID == "30D" {
			nextStart = zonedDateTimeToUtc(timezone, getZonedParts(startAt, timezone).addLocalDays(1))
		} else {
			nextStart = zonedDateTimeToUtc(timezone, getZonedParts(startAt, timezone).addLocalMonths(1))
		}
		endAt := nextStart
		if now.Before(nextStart) {
			endAt = now
		}
		buckets = append(buckets, earningsBucket{
			endAt:     endAt,
			isCurrent: !startAt.After(now) && now.Before(nextStart),
			label:     formatBucketLabel(startAt, rangeID, timezone),
			startAt:   startAt,
		})
	}
	return buckets
}
