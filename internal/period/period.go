// Package period computes ranking periods (a week, a month, a year, all
// time or custom days) in the owner's time zone.
package period

import (
	"fmt"
	"time"
)

type Kind string

const (
	Week   Kind = "week"
	Month  Kind = "month"
	Year   Kind = "year"
	All    Kind = "all"
	Custom Kind = "custom"
)

// Period is [Start, End) in a time zone.
type Period struct {
	Kind      Kind
	Start     time.Time
	End       time.Time
	weekStart time.Weekday
}

var (
	allStart = time.Unix(0, 0)
	allEnd   = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
)

func day(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// Containing returns the period of the given kind that contains t. For
// Custom, use Days.
func Containing(k Kind, t time.Time, loc *time.Location, weekStart time.Weekday) Period {
	t = t.In(loc)
	p := Period{Kind: k, weekStart: weekStart}
	switch k {
	case Week:
		d := day(t)
		back := (int(d.Weekday()) - int(weekStart) + 7) % 7
		p.Start = d.AddDate(0, 0, -back)
		p.End = p.Start.AddDate(0, 0, 7)
	case Month:
		p.Start = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
		p.End = p.Start.AddDate(0, 1, 0)
	case Year:
		p.Start = time.Date(t.Year(), 1, 1, 0, 0, 0, 0, loc)
		p.End = p.Start.AddDate(1, 0, 0)
	default:
		p.Kind = All
		p.Start, p.End = allStart.In(loc), allEnd.In(loc)
	}
	return p
}

// Days is the custom period from the start of first to the end of last.
func Days(first, last time.Time, loc *time.Location) Period {
	first, last = day(first.In(loc)), day(last.In(loc))
	if last.Before(first) {
		first, last = last, first
	}
	return Period{Kind: Custom, Start: first, End: last.AddDate(0, 0, 1)}
}

// Prev and Next step to the period of the same length before or after.
func (p Period) Prev() Period { return p.step(-1) }
func (p Period) Next() Period { return p.step(1) }

func (p Period) step(n int) Period {
	q := p
	switch p.Kind {
	case Week:
		q.Start, q.End = p.Start.AddDate(0, 0, 7*n), p.End.AddDate(0, 0, 7*n)
	case Month:
		q.Start, q.End = p.Start.AddDate(0, n, 0), p.End.AddDate(0, n, 0)
	case Year:
		q.Start, q.End = p.Start.AddDate(n, 0, 0), p.End.AddDate(n, 0, 0)
	case Custom:
		days := int(p.End.Sub(p.Start).Hours()/24 + 0.5)
		q.Start, q.End = p.Start.AddDate(0, 0, days*n), p.End.AddDate(0, 0, days*n)
	}
	return q
}

// Unix bounds for queries: listened_at >= From and < To.
func (p Period) From() int64 { return p.Start.Unix() }
func (p Period) To() int64   { return p.End.Unix() }

// Label is how the period is shown: "28 Sep – 4 Oct 2026", "September 2026",
// "2026", "All time".
func (p Period) Label() string {
	switch p.Kind {
	case Month:
		return p.Start.Format("January 2006")
	case Year:
		return p.Start.Format("2006")
	case All:
		return "All time"
	}
	last := p.End.AddDate(0, 0, -1)
	switch {
	case p.Start.Equal(last):
		return p.Start.Format("2 Jan 2006")
	case p.Start.Year() != last.Year():
		return fmt.Sprintf("%s – %s", p.Start.Format("2 Jan 2006"), last.Format("2 Jan 2006"))
	case p.Start.Month() != last.Month():
		return fmt.Sprintf("%s – %s", p.Start.Format("2 Jan"), last.Format("2 Jan 2006"))
	default:
		return fmt.Sprintf("%d – %s", p.Start.Day(), last.Format("2 Jan 2006"))
	}
}

// Contains reports whether t falls inside the period.
func (p Period) Contains(t time.Time) bool {
	return !t.Before(p.Start) && t.Before(p.End)
}
