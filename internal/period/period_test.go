package period

import (
	"testing"
	"time"

	_ "time/tzdata"
)

func TestPeriods(t *testing.T) {
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	// Wednesday 30 September 2026, 01:30 in Tokyo (still the 29th in UTC).
	now := time.Date(2026, 9, 30, 1, 30, 0, 0, tokyo)

	cases := []struct {
		p     Period
		label string
		start string
	}{
		{Containing(Week, now, tokyo, time.Monday), "28 Sep – 4 Oct 2026", "2026-09-28"},
		{Containing(Week, now, tokyo, time.Sunday), "27 Sep – 3 Oct 2026", "2026-09-27"},
		{Containing(Month, now, tokyo, time.Monday), "September 2026", "2026-09-01"},
		{Containing(Year, now, tokyo, time.Monday), "2026", "2026-01-01"},
		{Containing(Week, now, tokyo, time.Monday).Prev(), "21 – 27 Sep 2026", "2026-09-21"},
		{Containing(Month, now, tokyo, time.Monday).Next(), "October 2026", "2026-10-01"},
		{Containing(Week, time.Date(2026, 1, 1, 12, 0, 0, 0, tokyo), tokyo, time.Monday), "29 Dec 2025 – 4 Jan 2026", "2025-12-29"},
		{Days(time.Date(2026, 3, 31, 0, 0, 0, 0, tokyo), time.Date(2026, 1, 1, 0, 0, 0, 0, tokyo), tokyo), "1 Jan – 31 Mar 2026", "2026-01-01"},
		{Days(now, now, tokyo), "30 Sep 2026", "2026-09-30"},
		{Containing(All, now, tokyo, time.Monday), "All time", "1970-01-01"},
	}
	for _, c := range cases {
		if c.p.Label() != c.label {
			t.Errorf("label %q, want %q", c.p.Label(), c.label)
		}
		if got := c.p.Start.Format("2006-01-02"); got != c.start {
			t.Errorf("%s: start %s, want %s", c.label, got, c.start)
		}
	}
	w := Containing(Week, now, tokyo, time.Monday)
	if !w.Contains(now) || w.Contains(w.End) || !w.Contains(w.Start) {
		t.Error("week bounds wrong")
	}
	// A custom range steps by its own length.
	c := Days(time.Date(2026, 1, 1, 0, 0, 0, 0, tokyo), time.Date(2026, 1, 10, 0, 0, 0, 0, tokyo), tokyo)
	if got := c.Next().Label(); got != "11 – 20 Jan 2026" {
		t.Errorf("custom next: %s", got)
	}
}

func TestMonthStepFromLongMonth(t *testing.T) {
	// 31 January + 1 month must not skip February.
	m := Containing(Month, time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC), time.UTC, time.Monday)
	if got := m.Next().Label(); got != "February 2026" {
		t.Error(got)
	}
}
