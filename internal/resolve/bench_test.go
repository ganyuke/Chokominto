package resolve

import (
	"context"
	"fmt"
	"testing"

	"chokominto/internal/store"
)

// Rankings at the size budget in docs/architecture.md: 500k listens over
// ten years, 5,000 tracks by 800 artists on 1,200 albums, all linked.
func BenchmarkRankings500k(b *testing.B) {
	if testing.Short() {
		b.Skip("slow to seed")
	}
	ctx := context.Background()
	e := newEnv(&testing.T{})
	const n = 500_000
	step := int64(315_360_000 / n)
	var batch []store.NewListen
	for i := range n {
		track := i * 7919 % 5000
		batch = append(batch, store.NewListen{
			ListenedAt: 1_450_000_000 + int64(i)*step,
			Artist:     fmt.Sprintf("Artist %d", track%800),
			Title:      fmt.Sprintf("Song %d", track),
			Album:      fmt.Sprintf("Album %d", track%1200),
			Payload:    []byte(`{}`),
		})
		if len(batch) == 5000 {
			if _, err := e.db.InsertListens(ctx, e.user, "listenbrainz", nil, batch); err != nil {
				b.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	e.run.Drain(ctx)
	all := store.RankQuery{From: 0, To: 1 << 40, Limit: 50}
	year := store.RankQuery{From: 1_450_000_000 + 315_360_000 - 31_536_000, To: 1 << 40, Limit: 50}
	b.Run("songs all time", func(b *testing.B) {
		for b.Loop() {
			e.db.TopSongs(ctx, e.user, all, false)
		}
	})
	b.Run("songs combined all time", func(b *testing.B) {
		for b.Loop() {
			e.db.TopSongs(ctx, e.user, all, true)
		}
	})
	b.Run("artists all time", func(b *testing.B) {
		for b.Loop() {
			e.db.TopArtists(ctx, e.user, all, store.ByTotal)
		}
	})
	b.Run("albums all time", func(b *testing.B) {
		for b.Loop() {
			e.db.TopAlbums(ctx, e.user, all)
		}
	})
	b.Run("artists last year", func(b *testing.B) {
		for b.Loop() {
			e.db.TopArtists(ctx, e.user, year, store.ByTotal)
		}
	})
}
