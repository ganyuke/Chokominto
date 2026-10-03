package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Pictures for albums and artists. See docs/architecture.md, "Artwork".

// Artwork is a stored picture.
type Artwork struct {
	ID     int64
	SHA256 string
	Format string
	Width  int
	Height int
	Origin string
}

var artworkTables = map[string]string{"artist": "artists", "release": "releases", "song": "songs"}

// AddArtworkTx records a stored picture and returns its id. A picture
// that's there already keeps its first row.
func AddArtworkTx(ctx context.Context, tx *sql.Tx, a Artwork, originURL string) (int64, error) {
	var url any
	if originURL != "" {
		url = originURL
	}
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO artwork (sha256, format, width, height, origin, origin_url, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (sha256) DO UPDATE SET sha256 = excluded.sha256 RETURNING id`,
		a.SHA256, a.Format, a.Width, a.Height, a.Origin, url, unix()).Scan(&id)
	return id, err
}

// ItemArtwork returns an item's picture, and whether the owner chose it.
func (db *DB) ItemArtwork(ctx context.Context, kind string, id int64) (*Artwork, bool, error) {
	table, ok := artworkTables[kind]
	if !ok {
		return nil, false, ErrNotFound
	}
	var a Artwork
	var pinned bool
	err := db.r.QueryRowContext(ctx, `SELECT a.id, a.sha256, a.format, a.width, a.height, a.origin, e.artwork_pinned
		FROM `+table+` e JOIN artwork a ON a.id = e.artwork_id WHERE e.id = ?`, id).
		Scan(&a.ID, &a.SHA256, &a.Format, &a.Width, &a.Height, &a.Origin, &pinned)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return &a, pinned, err
}

// ArtworkFor returns the pictures of many items of one kind at once, for
// table rows.
func (db *DB) ArtworkFor(ctx context.Context, kind string, ids []int64) (map[int64]Artwork, error) {
	out := map[int64]Artwork{}
	table, ok := artworkTables[kind]
	if !ok || len(ids) == 0 {
		return out, nil
	}
	list := jsonIDs(ids)
	rows, err := db.r.QueryContext(ctx, `SELECT e.id, a.id, a.sha256, a.format, a.width, a.height, a.origin
		FROM `+table+` e JOIN artwork a ON a.id = e.artwork_id WHERE e.id IN (SELECT value FROM json_each(?))`, list)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var a Artwork
		if err := rows.Scan(&id, &a.ID, &a.SHA256, &a.Format, &a.Width, &a.Height, &a.Origin); err != nil {
			return nil, err
		}
		out[id] = a
	}
	return out, rows.Err()
}

// SetArtworkTx shows a picture for an item as one edit. pinned means the
// owner chose it, so lookups leave it alone. Automatic edits are made by
// lookups, and never replace a pinned picture.
func SetArtworkTx(ctx context.Context, tx *sql.Tx, userID int64, kind string, id, artworkID int64, pinned, automatic bool) (int64, error) {
	table, ok := artworkTables[kind]
	if !ok {
		return 0, ErrNotFound
	}
	var name string
	var merged sql.NullInt64
	var cur sql.NullInt64
	var curPinned bool
	err := tx.QueryRowContext(ctx, `SELECT name, merged_into, artwork_id, artwork_pinned FROM `+table+` WHERE id = ? AND user_id = ?`, id, userID).
		Scan(&name, &merged, &cur, &curPinned)
	if errors.Is(err, sql.ErrNoRows) || merged.Valid {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if automatic && curPinned {
		return 0, nil
	}
	if cur.Valid && cur.Int64 == artworkID && curPinned == pinned {
		return 0, nil
	}
	summary := "New picture for " + name
	if automatic {
		summary = "Found a picture for " + name
	}
	return ApplyEditTx(ctx, tx, userID, EditMeta{Kind: "artwork", Summary: summary, Automatic: automatic}, func(int64) []Change {
		return []Change{{Table: table, ID: id,
			Before: map[string]any{"artwork_id": nullable(cur), "artwork_pinned": boolInt(curPinned)},
			After:  map[string]any{"artwork_id": artworkID, "artwork_pinned": boolInt(pinned)}}}
	})
}

// UnusedArtwork lists pictures nothing shows anymore. A picture that an
// edit in the log could bring back by Undo counts as used.
func (db *DB) UnusedArtwork(ctx context.Context) ([]Artwork, error) {
	// The pictures in use are gathered once. Checking each picture against
	// the edit log on its own reads the whole log once per picture.
	rows, err := db.r.QueryContext(ctx, `WITH used(id) AS (
		  SELECT artwork_id FROM artists WHERE artwork_id IS NOT NULL
		  UNION SELECT artwork_id FROM releases WHERE artwork_id IS NOT NULL
		  UNION SELECT artwork_id FROM songs WHERE artwork_id IS NOT NULL
		  UNION SELECT json_extract(before, '$.artwork_id') FROM edit_changes WHERE tbl IN ('artists', 'releases', 'songs')
		  UNION SELECT json_extract(after, '$.artwork_id') FROM edit_changes WHERE tbl IN ('artists', 'releases', 'songs'))
		SELECT id, sha256, format, width, height, origin FROM artwork
		WHERE id NOT IN (SELECT id FROM used WHERE id IS NOT NULL) ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Artwork
	for rows.Next() {
		var a Artwork
		if err := rows.Scan(&a.ID, &a.SHA256, &a.Format, &a.Width, &a.Height, &a.Origin); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteArtwork removes a picture's row, if nothing uses it.
func (db *DB) DeleteArtwork(ctx context.Context, id int64) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM artwork WHERE id = ?
			AND NOT EXISTS (SELECT 1 FROM artists WHERE artwork_id = ?1)
			AND NOT EXISTS (SELECT 1 FROM releases WHERE artwork_id = ?1)
			AND NOT EXISTS (SELECT 1 FROM songs WHERE artwork_id = ?1)`, id)
		return err
	})
}

func jsonIDs(ids []int64) string {
	b, _ := json.Marshal(ids)
	return string(b)
}

// ArtworkItem is what a lookup needs to know about an album or artist.
type ArtworkItem struct {
	ID, UserID int64
	Name       string // primary name, for searching
	Artist     string // albums: the first artist, for searching
	MBID       string
	Names      []string        // every name, for the caller to key
	NameKeys   map[string]bool // match and romaji keys of every name
	ArtistKeys map[string]bool // albums: match and romaji keys of every name of every artist
	HasArtwork bool
	Pinned     bool // the owner chose or uploaded the picture
	FindOnline bool
}

func (db *DB) ArtworkItem(ctx context.Context, kind string, id int64) (ArtworkItem, error) {
	table, ok := artworkTables[kind]
	if !ok {
		return ArtworkItem{}, ErrNotFound
	}
	it := ArtworkItem{ID: id, NameKeys: map[string]bool{}, ArtistKeys: map[string]bool{}}
	var mbid sql.NullString
	var merged sql.NullInt64
	err := db.r.QueryRowContext(ctx, `SELECT e.user_id, e.name, e.mbid, e.merged_into, e.artwork_id IS NOT NULL, e.artwork_pinned, u.find_artwork
		FROM `+table+` e JOIN users u ON u.id = e.user_id WHERE e.id = ?`, id).
		Scan(&it.UserID, &it.Name, &mbid, &merged, &it.HasArtwork, &it.Pinned, &it.FindOnline)
	if errors.Is(err, sql.ErrNoRows) || merged.Valid {
		return it, ErrNotFound
	}
	if err != nil {
		return it, err
	}
	it.MBID = mbid.String
	keys := func(q string, into map[string]bool) error {
		rows, err := db.r.QueryContext(ctx, q, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				return err
			}
			into[k] = true
		}
		return rows.Err()
	}
	nameRows := func(q string) error {
		rows, err := db.r.QueryContext(ctx, q, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n, rk string
			if err := rows.Scan(&n, &rk); err != nil {
				return err
			}
			it.Names = append(it.Names, n)
			if rk != "" {
				it.NameKeys[rk] = true // romaji, for "Kawakiwoameku"
			}
		}
		return rows.Err()
	}
	if kind == "artist" {
		return it, nameRows(`SELECT name, coalesce(romaji_key, '') FROM artist_aliases WHERE artist_id = ?`)
	}
	if err := nameRows(`SELECT name, coalesce(romaji_key, '') FROM release_aliases WHERE release_id = ?`); err != nil {
		return it, err
	}
	if err := keys(`SELECT aa.match_key FROM release_credits rc JOIN artist_aliases aa ON aa.artist_id = rc.artist_id WHERE rc.release_id = ?1
		UNION SELECT aa.romaji_key FROM release_credits rc JOIN artist_aliases aa ON aa.artist_id = rc.artist_id WHERE rc.release_id = ?1 AND aa.romaji_key IS NOT NULL`, it.ArtistKeys); err != nil {
		return it, err
	}
	db.r.QueryRowContext(ctx, `SELECT a.name FROM release_credits rc JOIN artists a ON a.id = rc.artist_id WHERE rc.release_id = ? ORDER BY rc.position LIMIT 1`, id).Scan(&it.Artist)
	return it, nil
}

// SetLookupTx records where a lookup stands.
func SetLookupTx(ctx context.Context, tx *sql.Tx, kind string, id int64, state string, tries int, lastError string) error {
	var errText any
	if lastError != "" {
		errText = lastError
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO artwork_lookups (entity_type, entity_id, state, tries, last_error, checked_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (entity_type, entity_id) DO UPDATE SET state = excluded.state, tries = excluded.tries,
		last_error = excluded.last_error, checked_at = excluded.checked_at`, kind, id, state, tries, errText, unix())
	return err
}

// Candidate is a picture to choose from.
type Candidate struct {
	ID     int64
	Origin string
	URL    string
	Thumb  string
	Title  string
	Artist string
}

// AddCandidates keeps pictures to choose from, besides the ones there.
func (db *DB) AddCandidates(ctx context.Context, kind string, id int64, cs []Candidate) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		for _, c := range cs {
			var thumb any
			if c.Thumb != "" {
				thumb = c.Thumb
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO artwork_candidates (entity_type, entity_id, origin, url, thumb_url, title, artist, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, kind, id, c.Origin, c.URL, thumb, c.Title, c.Artist, unix()); err != nil {
				return err
			}
		}
		return nil
	})
}

// Candidates lists the pictures to choose from for an item.
func (db *DB) Candidates(ctx context.Context, kind string, id int64) ([]Candidate, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT id, origin, url, coalesce(thumb_url, ''), title, artist FROM artwork_candidates
		WHERE entity_type = ? AND entity_id = ? ORDER BY id`, kind, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.ID, &c.Origin, &c.URL, &c.Thumb, &c.Title, &c.Artist); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Candidate returns one candidate of an item.
func (db *DB) Candidate(ctx context.Context, kind string, id, candidateID int64) (Candidate, error) {
	var c Candidate
	err := db.r.QueryRowContext(ctx, `SELECT id, origin, url, coalesce(thumb_url, ''), title, artist FROM artwork_candidates
		WHERE id = ? AND entity_type = ? AND entity_id = ?`, candidateID, kind, id).Scan(&c.ID, &c.Origin, &c.URL, &c.Thumb, &c.Title, &c.Artist)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// Lookup returns where an item's lookup stands: found, candidates,
// notfound, error, or "" when it hasn't run.
func (db *DB) Lookup(ctx context.Context, kind string, id int64) (string, error) {
	var state string
	err := db.r.QueryRowContext(ctx, `SELECT state FROM artwork_lookups WHERE entity_type = ? AND entity_id = ?`, kind, id).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return state, err
}

// SetFindArtwork switches looking for pictures online.
func (db *DB) SetFindArtwork(ctx context.Context, userID int64, on bool) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE users SET find_artwork = ? WHERE id = ?`, boolInt(on), userID)
		return err
	})
}

// SetShowOtherNames switches showing items' other names under their name.
func (db *DB) SetShowOtherNames(ctx context.Context, userID int64, on bool) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE users SET show_other_names = ? WHERE id = ?`, boolInt(on), userID)
		return err
	})
}

// Cover is the picture shown for a recording, and where it comes from.
type Cover struct {
	Artwork
	ReleaseID int64 // the album whose cover it is, 0 for the song's own picture
}

// RecordingCovers returns a cover for each recording: its song's own
// picture, or else the cover of the album it's most listened on that has
// one.
func (db *DB) RecordingCovers(ctx context.Context, ids []int64) (map[int64]Artwork, error) {
	covers, err := db.RecordingCoverSources(ctx, ids)
	out := make(map[int64]Artwork, len(covers))
	for id, c := range covers {
		out[id] = c.Artwork
	}
	return out, err
}

// RecordingCoverSources is RecordingCovers with where each cover is from.
func (db *DB) RecordingCoverSources(ctx context.Context, ids []int64) (map[int64]Cover, error) {
	out := map[int64]Cover{}
	if len(ids) == 0 {
		return out, nil
	}
	// The song's own picture sorts first (release 0), then albums by the
	// recording's listens there.
	rows, err := db.r.QueryContext(ctx, `SELECT rec.id, 0, a.id, a.sha256, a.format, a.width, a.height, a.origin, 1 AS own, 0 AS n
		FROM recordings rec JOIN songs s ON s.id = rec.song_id JOIN artwork a ON a.id = s.artwork_id
		WHERE rec.id IN (SELECT value FROM json_each(?1))
		UNION ALL
		SELECT rt.recording_id, r.id, a.id, a.sha256, a.format, a.width, a.height, a.origin, 0,
		  coalesce((SELECT n FROM listen_totals lt WHERE lt.user_id = r.user_id AND lt.recording_id = rt.recording_id AND lt.release_id = r.id), 0)
		FROM release_tracks rt JOIN releases r ON r.id = rt.release_id AND r.merged_into IS NULL
		JOIN artwork a ON a.id = r.artwork_id
		WHERE rt.recording_id IN (SELECT value FROM json_each(?1))
		ORDER BY 1, own DESC, n DESC, 2`, jsonIDs(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var c Cover
		var own, n int
		if err := rows.Scan(&id, &c.ReleaseID, &c.ID, &c.SHA256, &c.Format, &c.Width, &c.Height, &c.Origin, &own, &n); err != nil {
			return nil, err
		}
		if _, ok := out[id]; !ok {
			out[id] = c
		}
	}
	return out, rows.Err()
}

// ClearArtworkTx takes an item's picture off, as one edit. It returns 0
// when there was none.
func ClearArtworkTx(ctx context.Context, tx *sql.Tx, userID int64, kind string, id int64) (int64, error) {
	table, ok := artworkTables[kind]
	if !ok {
		return 0, ErrNotFound
	}
	var name string
	var cur sql.NullInt64
	var pinned bool
	err := tx.QueryRowContext(ctx, `SELECT name, artwork_id, artwork_pinned FROM `+table+` WHERE id = ? AND user_id = ? AND merged_into IS NULL`, id, userID).
		Scan(&name, &cur, &pinned)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil || !cur.Valid {
		return 0, err
	}
	return ApplyEditTx(ctx, tx, userID, EditMeta{Kind: "artwork", Summary: "Took the picture off " + name}, func(int64) []Change {
		return []Change{{Table: table, ID: id,
			Before: map[string]any{"artwork_id": cur.Int64, "artwork_pinned": boolInt(pinned)},
			After:  map[string]any{"artwork_id": nil, "artwork_pinned": int64(0)}}}
	})
}

// SetCandidateThumb remembers where a candidate's small picture is kept.
func (db *DB) SetCandidateThumb(ctx context.Context, id int64, sha, format string) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE artwork_candidates SET thumb_sha = ?, thumb_format = ? WHERE id = ?`, sha, format, id)
		return err
	})
}

// CandidateThumb returns where a candidate's small picture is kept, or ""
// when it hasn't been fetched.
func (db *DB) CandidateThumb(ctx context.Context, id int64) (string, string, string, error) {
	var url, sha, format string
	err := db.r.QueryRowContext(ctx, `SELECT coalesce(thumb_url, url), coalesce(thumb_sha, ''), coalesce(thumb_format, '') FROM artwork_candidates WHERE id = ?`, id).
		Scan(&url, &sha, &format)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", ErrNotFound
	}
	return url, sha, format, err
}

// LookAgain starts an item's lookup over at the owner's request: its
// candidates and the last result are forgotten, and a lookup is queued for
// now. With no result on record, the lookup also runs for an item that has
// a picture, to offer others.
func (db *DB) LookAgain(ctx context.Context, kind string, id int64) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM artwork_candidates WHERE entity_type = ? AND entity_id = ?`, kind, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM artwork_lookups WHERE entity_type = ? AND entity_id = ?`, kind, id); err != nil {
			return err
		}
		return EnqueueTx(ctx, tx, "artwork", fmt.Sprintf("%s:%d:0", kind, id), "", 0)
	})
}

// ArtworkLooking reports whether a lookup for the item is queued for now
// or running. Retries waiting for later don't count.
func (db *DB) ArtworkLooking(ctx context.Context, kind string, id int64) (bool, error) {
	var looking bool
	err := db.r.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE kind = 'artwork' AND key = ?)`,
		fmt.Sprintf("%s:%d:0", kind, id)).Scan(&looking)
	return looking, err
}

// ClearCandidates forgets an item's candidates, once one is chosen.
func (db *DB) ClearCandidates(ctx context.Context, kind string, id int64) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM artwork_candidates WHERE entity_type = ? AND entity_id = ?`, kind, id)
		return err
	})
}
