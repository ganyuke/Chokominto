// Package ingest is the only way listens get written. The ListenBrainz
// API, manual scrobbling and importers all go through Store.
package ingest

import (
	"context"
	"strings"
	"time"

	"chokominto/internal/store"
)

// Listen is one listen as a client sent it. Text fields are kept exactly as
// received, including odd spacing, so they can be reparsed later.
type Listen struct {
	ListenedAt int64
	HasTime    bool // false when the client sent no timestamp
	Artist     string
	Title      string
	Album      string
	// AlbumArtist is only set when the client sent the album's artist
	// separately from the track's.
	AlbumArtist string
	Payload     []byte // the listen object exactly as received
}

// ChunkSize is how many listens go in one transaction. API requests are at
// most 1000 listens and always fit in one. Big imports commit per chunk, so
// a failure part way through keeps what was done and a re-run skips it.
const ChunkSize = 5000

// Listens before this time (2002-01-01) or more than a day in the future
// are stored but flagged, since the client's clock or data is probably off.
const (
	earliest   = 1009843200
	futureSkew = 24 * time.Hour
)

var now = time.Now

// Incomplete reports whether a listen is missing what it needs to be
// shown and ranked normally.
func Incomplete(l Listen, t time.Time) bool {
	return !l.HasTime ||
		strings.TrimSpace(l.Artist) == "" ||
		strings.TrimSpace(l.Title) == "" ||
		l.ListenedAt < earliest ||
		l.ListenedAt > t.Add(futureSkew).Unix()
}

// Store saves listens. Nothing is rejected for its content: incomplete
// listens are stored with a flag so they can be fixed later instead of
// being lost or making a client retry forever. Duplicates are skipped.
func Store(ctx context.Context, db *store.DB, userID int64, origin string, tokenID *int64, ls []Listen) (store.InsertResult, error) {
	var total store.InsertResult
	t := now()
	for start := 0; start < len(ls); start += ChunkSize {
		chunk := ls[start:min(start+ChunkSize, len(ls))]
		nls := make([]store.NewListen, len(chunk))
		for i, l := range chunk {
			at := l.ListenedAt
			if !l.HasTime {
				at = t.Unix()
			}
			nls[i] = store.NewListen{
				ListenedAt:  at,
				Artist:      l.Artist,
				Title:       l.Title,
				Album:       l.Album,
				AlbumArtist: l.AlbumArtist,
				Payload:     l.Payload,
				Incomplete:  Incomplete(l, t),
			}
		}
		res, err := db.InsertListens(ctx, userID, origin, tokenID, nls)
		total.Stored += res.Stored
		total.Duplicates += res.Duplicates
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
