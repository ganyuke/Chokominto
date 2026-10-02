package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// seedBench builds a database with n listens over about ten years, spread
// across 5,000 distinct tracks. Budget from docs/architecture.md: every
// page under 150 ms on a Pi 4 with 500k listens.
func seedBench(b *testing.B, n int) (*DB, int64) {
	b.Helper()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(b.TempDir(), "bench.db"), "")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	u, _ := db.CreateUser(ctx, "elaina", "h")
	const start = 1_450_000_000
	step := int64(315_360_000 / n)
	var batch []NewListen
	for i := range n {
		track := i * 7919 % 5000
		batch = append(batch, NewListen{
			ListenedAt: start + int64(i)*step,
			Artist:     fmt.Sprintf("Artist %d", track%800),
			Title:      fmt.Sprintf("Song %d", track),
			Album:      fmt.Sprintf("Album %d", track%1200),
			Payload:    []byte(`{"track_metadata":{"additional_info":{"submission_client":"Pano Scrobbler"}}}`),
		})
		if len(batch) == 5000 {
			if _, err := db.InsertListens(ctx, u, "listenbrainz", nil, batch); err != nil {
				b.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	return db, u
}

func BenchmarkHistory500k(b *testing.B) {
	if testing.Short() {
		b.Skip("slow to seed")
	}
	ctx := context.Background()
	db, u := seedBench(b, 500_000)
	mid, _ := db.Listens(ctx, u, ListenRange{Before: ptr(CursorAt(1_450_000_000 + 315_360_000/2)), Limit: 1})
	midCursor := Cursor{mid[0].ListenedAt, mid[0].ID}

	// One History page view: the page of 100 plus the two "is there more"
	// probes the handler makes.
	page := func(r ListenRange) {
		ls, err := db.Listens(ctx, u, r)
		if err != nil || len(ls) != 100 {
			b.Fatal(err, len(ls))
		}
		first, last := ls[0], ls[len(ls)-1]
		db.Listens(ctx, u, ListenRange{After: &Cursor{first.ListenedAt, first.ID}, Limit: 1})
		db.Listens(ctx, u, ListenRange{Before: &Cursor{last.ListenedAt, last.ID}, Limit: 1})
	}
	b.Run("first page", func(b *testing.B) {
		for b.Loop() {
			page(ListenRange{Limit: 100})
		}
	})
	b.Run("middle page", func(b *testing.B) {
		for b.Loop() {
			page(ListenRange{Before: &midCursor, Limit: 100})
		}
	})
	b.Run("newer from middle", func(b *testing.B) {
		for b.Loop() {
			page(ListenRange{After: &midCursor, Oldest: true, Limit: 100})
		}
	})
	b.Run("listen stats", func(b *testing.B) {
		for b.Loop() {
			if _, _, _, err := db.ListenStats(ctx, u); err != nil {
				b.Fatal(err)
			}
		}
	})
}
