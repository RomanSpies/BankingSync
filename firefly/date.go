package firefly

import (
	"fmt"
	"strings"
	"time"
)

const dateLayout = "2006-01-02"

// FormatDate renders a calendar day as a date-time without an offset, which is
// what makes the day survive the round trip.
//
// A value carrying an offset is an instant, and Firefly renders instants in the
// server's own timezone: "…T00:00:00+00:00" comes back as "…T02:00:00+02:00" in
// Berlin — same day — but as the *previous* day at "…T20:00:00-04:00" in New
// York. Every transaction would then land one day early for anyone running
// Firefly west of UTC, consistently enough that nothing ever looks broken.
//
// Without an offset the value is wall-clock time, Firefly stores it as local
// midnight, and the calendar day is preserved at any server offset. Verified
// against a live instance; a bare date works too but deviates from the
// date-time the API schema declares.
func FormatDate(d time.Time) string {
	y, m, day := d.Date()
	return fmt.Sprintf("%04d-%02d-%02dT00:00:00", y, int(m), day)
}

// ParseDate extracts the calendar day Firefly meant. Firefly serialises
// timestamps with the server's offset, so "2026-07-01T00:00:00+02:00" is the
// first of July — converting it to UTC would silently make it the thirtieth of
// June and shift every candidate window by a day.
func ParseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty date")
	}

	if t, err := time.Parse(time.RFC3339, s); err == nil {
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC), nil
	}
	if t, err := time.Parse(dateLayout, s); err == nil {
		return t, nil
	}
	if len(s) >= len(dateLayout) {
		if t, err := time.Parse(dateLayout, s[:len(dateLayout)]); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised date %q", s)
}
