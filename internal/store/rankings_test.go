package store

import (
	"slices"
	"testing"
)

// Ranks must match RANK(): ties share a number and the next one skips, also
// when a tie spans two pages.
func TestPagerRanks(t *testing.T) {
	counts := []int{9, 7, 7, 7, 5, 5, 2}
	for _, c := range []struct {
		offset, limit int
		want          []int
	}{
		{0, 10, []int{1, 2, 2, 2, 5, 5, 7}},
		{0, 3, []int{1, 2, 2}},
		{3, 3, []int{2, 5, 5}},
		{6, 3, []int{7}},
		{9, 3, nil},
	} {
		var p pager
		var got []int
		for _, n := range counts {
			if r := p.next(RankQuery{Offset: c.offset, Limit: c.limit}, n); r > 0 {
				got = append(got, r)
			}
		}
		if !slices.Equal(got, c.want) || p.n != len(counts) {
			t.Errorf("offset %d limit %d: got %v of %d, want %v of %d", c.offset, c.limit, got, p.n, c.want, len(counts))
		}
	}
}
