package budget

import (
	"strings"
	"time"
)

const (
	windowDaysBefore = 7
	windowDaysAfter  = 8
)

// WindowBounds returns the half-open candidate window [from, to) around d.
func WindowBounds(d time.Time) (time.Time, time.Time) {
	day := d.UTC().Truncate(24 * time.Hour)
	return day.AddDate(0, 0, -windowDaysBefore), day.AddDate(0, 0, windowDaysAfter)
}

// InWindow reports whether c falls inside the half-open window around target.
func InWindow(c, target time.Time) bool {
	from, to := WindowBounds(target)
	day := c.UTC().Truncate(24 * time.Hour)
	return !day.Before(from) && day.Before(to)
}

func dayDistance(a, b time.Time) int {
	d := int(a.UTC().Truncate(24*time.Hour).Sub(b.UTC().Truncate(24*time.Hour)) / (24 * time.Hour))
	if d < 0 {
		return -d
	}
	return d
}

// NormalisePayee strips a leading card-scheme prefix and collapses whitespace,
// for comparison only. What gets written to the backend is always what the bank
// sent — this exists so that the pending and booked spelling of one purchase
// compare equal.
func NormalisePayee(name string, prefixes []string) string {
	name = strings.Join(strings.Fields(name), " ")
	for _, p := range prefixes {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len(name) <= len(p) || !strings.EqualFold(name[:len(p)], p) {
			continue
		}
		// Only strip on a word boundary, so "VISABANK GmbH" keeps its name.
		switch name[len(p)] {
		case ' ', '-', '*', '/', ':':
			return strings.TrimSpace(name[len(p)+1:])
		}
	}
	return name
}
