package store

import (
	"slices"
	"testing"
	"time"
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

// A period count of one item matches counting its listens one by one, for
// short spans and for spans that use whole years from the totals.
func TestEntityListenCountIn(t *testing.T) {
	ctx := t.Context()
	db := openTest(t)
	m := newMusic(t, db)
	start := time.Date(2023, 11, 20, 0, 0, 0, 0, time.UTC).Unix()
	var ls []NewListen
	for i := range 120 {
		ls = append(ls, NewListen{ListenedAt: start + int64(i)*9*86400, Artist: "ＹＯＡＳＯＢＩ", Title: "アイドル", Album: "アイドル", Payload: []byte(`{}`)})
		ls = append(ls, NewListen{ListenedAt: start + int64(i)*9*86400 + 3600, Artist: "Ayase", Title: "アイドル", Payload: []byte(`{}`)})
	}
	if _, err := db.InsertListens(ctx, m.u, "listenbrainz", nil, ls); err != nil {
		t.Fatal(err)
	}
	song := int64(count(t, db, `SELECT song_id FROM recordings WHERE id = ?`, m.idolJP))
	direct := map[string]string{
		"artist":  `recording_id IN (SELECT recording_id FROM recording_artists WHERE artist_id = ?)`,
		"song":    `recording_id IN (SELECT id FROM recordings WHERE song_id = ?)`,
		"release": `release_id = ?`,
	}
	ids := map[string]int64{"artist": m.ayase, "song": song, "release": m.single}
	day := func(y int, mo time.Month, d int) int64 { return time.Date(y, mo, d, 0, 0, 0, 0, time.UTC).Unix() }
	for _, span := range [][2]int64{
		{day(2024, 3, 1), day(2024, 4, 1)},   // a month
		{day(2024, 1, 1), day(2025, 1, 1)},   // a whole year
		{day(2023, 12, 25), day(2026, 1, 3)}, // years plus bits either side
		{day(2024, 1, 2), day(2025, 12, 30)}, // years minus bits
		{0, allTimeEnd + 1},                  // all time
	} {
		for kind, id := range ids {
			got, err := db.EntityListenCountIn(ctx, m.u, kind, id, span[0], span[1])
			if err != nil {
				t.Fatal(err)
			}
			want := count(t, db, `SELECT count(*) FROM listens WHERE user_id = ? AND deleted_by IS NULL AND listened_at >= ? AND listened_at < ? AND `+direct[kind],
				m.u, span[0], span[1], id)
			if got != want || want == 0 {
				t.Errorf("%s %v: %d, want %d", kind, span, got, want)
			}
		}
	}
	if _, err := db.EntityListenCountIn(ctx, m.u, "label", 1, 0, 1); err != ErrNotFound {
		t.Fatal(err)
	}
}
