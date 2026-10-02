package resolve

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"
	"time"

	"chokominto/internal/store"
)

// The yearly totals must always equal a real count, and rankings built from
// them must match counting listen by listen, for any period.
func TestYearTotalsMatchRealCounts(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(1, 2))
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	span := int64(3 * 365 * 86400)
	var ls []store.NewListen
	for range 3000 {
		track := rng.IntN(25)
		album := ""
		if track%3 != 0 {
			album = fmt.Sprintf("Album %d", track%4)
		}
		ls = append(ls, store.NewListen{
			ListenedAt: start + rng.Int64N(span),
			Artist:     fmt.Sprintf("Artist %d", track%5),
			Title:      fmt.Sprintf("Song %d", track),
			Album:      album,
			Payload:    []byte(`{}`),
		})
	}
	e.db.InsertListens(ctx, e.user, "listenbrainz", nil, ls)
	e.run.Drain(ctx)

	check := func(when string) {
		t.Helper()
		bad := e.q(`SELECT 'mismatch' FROM (
			SELECT user_id, CAST(strftime('%Y', listened_at, 'unixepoch') AS INTEGER) y, recording_id, coalesce(release_id, 0) rel, count(*) n
			FROM listens WHERE deleted_by IS NULL AND recording_id IS NOT NULL GROUP BY 1, 2, 3, 4) real
			FULL OUTER JOIN listen_years ly ON ly.user_id = real.user_id AND ly.year = real.y AND ly.recording_id = real.recording_id AND ly.release_id = real.rel
			WHERE real.n IS NOT ly.n`)
		if len(bad) > 0 {
			t.Fatalf("%s: %d rows of yearly totals are wrong", when, len(bad))
		}
		bad = e.q(`SELECT 'mismatch' FROM (
			SELECT user_id, recording_id, coalesce(release_id, 0) rel, count(*) n
			FROM listens WHERE deleted_by IS NULL AND recording_id IS NOT NULL GROUP BY 1, 2, 3) real
			FULL OUTER JOIN listen_totals lt ON lt.user_id = real.user_id AND lt.recording_id = real.recording_id AND lt.release_id = real.rel
			WHERE real.n IS NOT lt.n`)
		if len(bad) > 0 {
			t.Fatalf("%s: %d rows of all-time totals are wrong", when, len(bad))
		}
	}
	check("after linking")

	// Delete some listens, undo one of the deletes.
	got, _ := e.db.Listens(ctx, e.user, store.ListenRange{Limit: 20})
	var lastDel int64
	for _, l := range got {
		lastDel, _ = e.db.DeleteListen(ctx, e.user, l)
	}
	check("after deletes")
	e.db.UndoEdit(ctx, e.user, lastDel)
	check("after undo")

	// Undo an automatic link: its listens drop out of the totals.
	link := e.one(`SELECT id FROM edits WHERE kind = 'link' ORDER BY id LIMIT 1`)
	var linkID int64
	fmt.Sscan(link, &linkID)
	if _, err := e.db.UndoEdit(ctx, e.user, linkID); err != nil {
		t.Fatal(err)
	}
	check("after unlinking")

	// Rankings for random periods, in a time zone far from UTC, against a
	// brute-force count.
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	periods := [][2]int64{{0, 1 << 40}}
	for y := 2024; y <= 2026; y++ {
		periods = append(periods, [2]int64{time.Date(y, 1, 1, 0, 0, 0, 0, tokyo).Unix(), time.Date(y+1, 1, 1, 0, 0, 0, 0, tokyo).Unix()})
	}
	for range 40 {
		a, b := start+rng.Int64N(span), start+rng.Int64N(span)
		if a > b {
			a, b = b, a
		}
		periods = append(periods, [2]int64{a, b})
	}
	for _, p := range periods {
		ranked, err := e.db.TopSongs(ctx, e.user, store.RankQuery{From: p[0], To: p[1], Limit: 1000}, false)
		if err != nil {
			t.Fatal(err)
		}
		var fromTotals []string
		for _, s := range ranked {
			fromTotals = append(fromTotals, fmt.Sprintf("%d:%d", s.RecordingID, s.Listens))
		}
		direct := e.q(`SELECT recording_id || ':' || count(*) FROM listens
			WHERE user_id = ? AND deleted_by IS NULL AND recording_id IS NOT NULL AND listened_at >= ? AND listened_at < ?
			GROUP BY recording_id`, e.user, p[0], p[1])
		sort.Strings(fromTotals)
		sort.Strings(direct)
		if strings.Join(fromTotals, ",") != strings.Join(direct, ",") {
			t.Fatalf("period %v: rankings %v, direct count %v", p, fromTotals, direct)
		}
	}
}
