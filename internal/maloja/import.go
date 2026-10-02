// Package maloja imports listens and scrobbler keys from a Maloja
// installation.
//
// It reads Maloja's database file directly, because Maloja's JSON export
// leaves out the original scrobbles. Where Maloja kept the original
// (its rawscrobble column), that text is used. Otherwise Maloja's cleaned-up
// artists, title and album stand in for it, and the payload says so.
package maloja

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"chokominto/internal/auth"
	"chokominto/internal/ingest"
	"chokominto/internal/store"
)

type Result struct {
	Total         int
	Stored        int
	Duplicates    int
	Reconstructed int // no original scrobble in Maloja, rebuilt from its cleaned data
}

// Progress is called after each chunk.
type Progress func(done, total int)

type source struct {
	db       *sql.DB
	hasRaw   bool
	hasAlbum bool
}

func open(ctx context.Context, path string) (*source, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(ON)")
	q.Add("_pragma", "busy_timeout(5000)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &source{db: db}
	cols, err := columns(ctx, db, "scrobbles")
	if err != nil {
		db.Close()
		return nil, err
	}
	if !cols["timestamp"] || !cols["track_id"] {
		db.Close()
		return nil, errors.New("this doesn't look like a Maloja database (no scrobbles table)")
	}
	s.hasRaw = cols["rawscrobble"]
	trackCols, err := columns(ctx, db, "tracks")
	if err != nil {
		db.Close()
		return nil, err
	}
	albumCols, err := columns(ctx, db, "albums")
	if err != nil {
		db.Close()
		return nil, err
	}
	// Older Maloja versions had no albums.
	s.hasAlbum = trackCols["album_id"] && albumCols["albtitle"]
	return s, nil
}

func columns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		cols[n] = true
	}
	return cols, rows.Err()
}

// trackArtists maps Maloja track ids to their artist names, sorted.
func (s *source) trackArtists(ctx context.Context) (map[int64][]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ta.track_id, a.name FROM trackartists ta JOIN artists a ON a.id = ta.artist_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64][]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		m[id] = append(m[id], name)
	}
	for _, names := range m {
		slices.Sort(names)
	}
	return m, rows.Err()
}

type malojaRow struct {
	timestamp int64
	raw       sql.NullString
	origin    sql.NullString
	duration  sql.NullInt64
	trackID   sql.NullInt64
	title     sql.NullString
	album     sql.NullString
}

// Import copies every Maloja scrobble into Chokominto for userID. It's safe
// to run again: listens already imported are skipped.
func Import(ctx context.Context, db *store.DB, userID int64, path string, progress Progress) (Result, error) {
	var res Result
	src, err := open(ctx, path)
	if err != nil {
		return res, err
	}
	defer src.db.Close()

	artists, err := src.trackArtists(ctx)
	if err != nil {
		return res, err
	}
	if err := src.db.QueryRowContext(ctx, `SELECT count(*) FROM scrobbles`).Scan(&res.Total); err != nil {
		return res, err
	}

	raw := "NULL"
	if src.hasRaw {
		raw = "s.rawscrobble"
	}
	album, albumJoin := "NULL", ""
	if src.hasAlbum {
		album, albumJoin = "al.albtitle", "LEFT JOIN albums al ON al.id = t.album_id"
	}
	rows, err := src.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT s.timestamp, %s, s.origin, s.duration, s.track_id, t.title, %s
		 FROM scrobbles s LEFT JOIN tracks t ON t.id = s.track_id %s
		 ORDER BY s.timestamp`, raw, album, albumJoin))
	if err != nil {
		return res, err
	}
	defer rows.Close()

	var batch []ingest.Listen
	flush := func() error {
		r, err := ingest.Store(ctx, db, userID, "import:maloja", nil, batch)
		res.Stored += r.Stored
		res.Duplicates += r.Duplicates
		batch = batch[:0]
		if progress != nil {
			progress(res.Stored+res.Duplicates, res.Total)
		}
		return err
	}
	for rows.Next() {
		var r malojaRow
		if err := rows.Scan(&r.timestamp, &r.raw, &r.origin, &r.duration, &r.trackID, &r.title, &r.album); err != nil {
			return res, err
		}
		l, reconstructed := toListen(r, artists[r.trackID.Int64])
		if reconstructed {
			res.Reconstructed++
		}
		batch = append(batch, l)
		if len(batch) == ingest.ChunkSize {
			if err := flush(); err != nil {
				return res, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	if len(batch) > 0 {
		if err := flush(); err != nil {
			return res, err
		}
	}
	return res, nil
}

// toListen prefers Maloja's copy of the original scrobble.
func toListen(r malojaRow, cleanedArtists []string) (ingest.Listen, bool) {
	l := ingest.Listen{ListenedAt: r.timestamp, HasTime: true}
	if r.raw.Valid && r.raw.String != "" {
		var m map[string]any
		if json.Unmarshal([]byte(r.raw.String), &m) == nil {
			artist := joinArtists(m["track_artists"])
			title, _ := m["track_title"].(string)
			if artist != "" && title != "" {
				l.Artist, l.Title = artist, title
				l.Album, _ = m["album_title"].(string)
				l.Payload = []byte(r.raw.String)
				return l, false
			}
		}
	}
	l.Artist = strings.Join(cleanedArtists, ", ")
	l.Title = r.title.String
	l.Album = r.album.String
	p := map[string]any{
		"reconstructed_from": "maloja",
		"artists":            cleanedArtists,
		"title":              r.title.String,
	}
	if r.album.Valid {
		p["album"] = r.album.String
	}
	if r.origin.Valid {
		p["origin"] = r.origin.String
	}
	if r.duration.Valid {
		p["duration"] = r.duration.Int64
	}
	l.Payload, _ = json.Marshal(p)
	return l, true
}

// joinArtists accepts Maloja's track_artists as a list or a single string.
func joinArtists(v any) string {
	switch a := v.(type) {
	case string:
		return a
	case []any:
		var names []string
		for _, x := range a {
			if s, ok := x.(string); ok && s != "" {
				names = append(names, s)
			}
		}
		return strings.Join(names, ", ")
	}
	return ""
}

type KeysResult struct {
	Added   int
	Skipped int // already imported
}

// ImportAPIKeys turns Maloja's apikeys.yml (name: key) into scrobbler
// tokens, so scrobblers keep working without new tokens.
func ImportAPIKeys(ctx context.Context, db *store.DB, userID int64, path string) (KeysResult, error) {
	var res KeysResult
	b, err := os.ReadFile(path)
	if err != nil {
		return res, err
	}
	var keys map[string]string
	if err := yaml.Unmarshal(b, &keys); err != nil {
		return res, fmt.Errorf("%s: %w", path, err)
	}
	names := make([]string, 0, len(keys))
	for n := range keys {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, name := range names {
		key := strings.TrimSpace(keys[name])
		if key == "" {
			continue
		}
		_, err := db.CreateToken(ctx, userID, name+" (from Maloja)", auth.HashSecret(key))
		if errors.Is(err, store.ErrTokenExists) {
			res.Skipped++
			continue
		}
		if err != nil {
			return res, err
		}
		res.Added++
	}
	return res, nil
}
