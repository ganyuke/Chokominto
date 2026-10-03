package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Read queries for the artist, song and album pages, and for showing linked
// names on listens. Every lookup is scoped to the owner's own data.

// RecordingInfo is how a recording is shown in a row.
type RecordingInfo struct {
	RecordingID int64
	Song        Ref
	Version     string
	Artists     []Ref
}

func (db *DB) RecordingInfos(ctx context.Context, ids []int64) (map[int64]RecordingInfo, error) {
	out := map[int64]RecordingInfo{}
	if len(ids) == 0 {
		return out, nil
	}
	// Two queries for the whole page, whatever its length.
	list, _ := json.Marshal(ids)
	rows, err := db.r.QueryContext(ctx,
		`SELECT r.id, s.id, s.name, s.other_names, r.version FROM recordings r JOIN songs s ON s.id = r.song_id
		 WHERE r.id IN (SELECT value FROM json_each(?))`, string(list))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ri RecordingInfo
		if err := rows.Scan(&ri.RecordingID, &ri.Song.ID, &ri.Song.Name, &ri.Song.OtherNames, &ri.Version); err != nil {
			rows.Close()
			return nil, err
		}
		out[ri.RecordingID] = ri
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = db.r.QueryContext(ctx,
		`SELECT rc.recording_id, a.id, a.name, a.other_names FROM recording_credits rc JOIN artists a ON a.id = rc.artist_id
		 WHERE rc.recording_id IN (SELECT value FROM json_each(?)) AND rc.role IN ('main', 'featured')
		 ORDER BY rc.recording_id, rc.role = 'featured', rc.position`, string(list))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rec int64
		var a Ref
		if err := rows.Scan(&rec, &a.ID, &a.Name, &a.OtherNames); err != nil {
			return nil, err
		}
		if ri, ok := out[rec]; ok {
			ri.Artists = append(ri.Artists, a)
			out[rec] = ri
		}
	}
	return out, rows.Err()
}

func (db *DB) ReleaseRefs(ctx context.Context, ids []int64) (map[int64]Ref, error) {
	out := map[int64]Ref{}
	if len(ids) == 0 {
		return out, nil
	}
	list, _ := json.Marshal(ids)
	rs, err := db.refs(ctx, `SELECT id, name, other_names FROM releases WHERE id IN (SELECT value FROM json_each(?))`, string(list))
	for _, r := range rs {
		out[r.ID] = r
	}
	return out, err
}

// Entity looks up a named entity the user owns, for its own page: its
// OtherNames are the names listed there. When it was merged into another,
// MergedInto is that one's id.
type Entity struct {
	Ref
	MergedInto int64
}

var entityTables = map[string]string{"artist": "artists", "song": "songs", "release": "releases"}

func (db *DB) Entity(ctx context.Context, userID int64, kind string, id int64) (Entity, error) {
	var e Entity
	var merged sql.NullInt64
	err := db.r.QueryRowContext(ctx,
		`SELECT id, name, byline, merged_into FROM `+entityTables[kind]+` WHERE id = ? AND user_id = ?`, id, userID).
		Scan(&e.ID, &e.Name, &e.OtherNames, &merged)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	e.MergedInto = merged.Int64
	return e, err
}

// Labels on an entity, by name.
func (db *DB) EntityLabels(ctx context.Context, kind string, id int64) ([]string, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT l.name FROM entity_labels el JOIN labels l ON l.id = el.label_id
		 WHERE el.entity_type = ? AND el.entity_id = ? ORDER BY l.position, l.name`, kind, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ArtistRelations are the links shown in an artist's infobox.
type ArtistRelations struct {
	CountsFor   []Ref // this artist's listens also count for these
	CountedFrom []Ref // these artists' listens also count for this one
	Members     []Ref
	MemberOf    []Ref
}

func (db *DB) ArtistRelations(ctx context.Context, id int64) (ArtistRelations, error) {
	var r ArtistRelations
	var err error
	const sel = `SELECT a.id, a.name, a.other_names FROM artists a `
	if r.CountsFor, err = db.refs(ctx, sel+`JOIN artist_counts_for c ON c.target_id = a.id WHERE c.artist_id = ? ORDER BY a.name`, id); err != nil {
		return r, err
	}
	if r.CountedFrom, err = db.refs(ctx, sel+`JOIN artist_counts_for c ON c.artist_id = a.id WHERE c.target_id = ? ORDER BY a.name`, id); err != nil {
		return r, err
	}
	if r.Members, err = db.refs(ctx, sel+`JOIN group_members g ON g.member_id = a.id WHERE g.group_id = ? ORDER BY a.name`, id); err != nil {
		return r, err
	}
	r.MemberOf, err = db.refs(ctx, sel+`JOIN group_members g ON g.group_id = a.id WHERE g.member_id = ? ORDER BY a.name`, id)
	return r, err
}

// RecordingCount is a recording with its all-time listens.
type RecordingCount struct {
	RecordingInfo
	Listens int
}

func (db *DB) recordingCounts(ctx context.Context, q string, args ...any) ([]RecordingCount, error) {
	rows, err := db.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var ids []int64
	var counts []int
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
		counts = append(counts, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	infos, err := db.RecordingInfos(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]RecordingCount, 0, len(ids))
	for i, id := range ids {
		out = append(out, RecordingCount{infos[id], counts[i]})
	}
	return out, nil
}

// ArtistTopRecordings are the artist's most played recordings of all time,
// counting every way the artist gets credit.
func (db *DB) ArtistTopRecordings(ctx context.Context, userID, artistID int64, limit int) ([]RecordingCount, error) {
	return db.recordingCounts(ctx, `SELECT lt.recording_id, sum(lt.n) AS n FROM listen_totals lt
		JOIN recording_artists ra ON ra.recording_id = lt.recording_id
		WHERE lt.user_id = ? AND ra.artist_id = ? GROUP BY lt.recording_id ORDER BY n DESC, lt.recording_id LIMIT ?`,
		userID, artistID, limit)
}

// SongRecordings are all recordings of a song (original, covers, versions)
// with their listens, most played first.
func (db *DB) SongRecordings(ctx context.Context, userID, songID int64) ([]RecordingCount, error) {
	return db.recordingCounts(ctx, `SELECT r.id, coalesce((SELECT sum(n) FROM listen_totals WHERE user_id = ? AND recording_id = r.id), 0) AS n
		FROM recordings r WHERE r.song_id = ? AND r.merged_into IS NULL ORDER BY n DESC, r.id`, userID, songID)
}

// AlbumTracks are the recordings on a release with listens of them from
// that release.
func (db *DB) AlbumTracks(ctx context.Context, userID, releaseID int64) ([]RecordingCount, error) {
	return db.recordingCounts(ctx, `SELECT rt.recording_id,
		coalesce((SELECT n FROM listen_totals WHERE user_id = ? AND recording_id = rt.recording_id AND release_id = rt.release_id), 0) AS n
		FROM release_tracks rt WHERE rt.release_id = ? ORDER BY rt.disc, rt.position, n DESC, rt.recording_id`, userID, releaseID)
}

func (db *DB) ReleaseArtists(ctx context.Context, releaseID int64) ([]Ref, error) {
	return db.refs(ctx, `SELECT a.id, a.name, a.other_names FROM release_credits rc JOIN artists a ON a.id = rc.artist_id
		WHERE rc.release_id = ? ORDER BY rc.position`, releaseID)
}

// SongReleases are the releases any recording of the song is on.
func (db *DB) SongReleases(ctx context.Context, songID int64) ([]Ref, error) {
	return db.refs(ctx, `SELECT DISTINCT rl.id, rl.name, rl.other_names FROM releases rl
		JOIN release_tracks rt ON rt.release_id = rl.id JOIN recordings r ON r.id = rt.recording_id
		WHERE r.song_id = ? AND rl.merged_into IS NULL ORDER BY rl.name`, songID)
}

// EntityListens are recent listens of an artist, song or release.
func (db *DB) EntityListens(ctx context.Context, userID int64, kind string, id int64, limit int) ([]Listen, error) {
	// Start from the item's own recordings. Left to itself, SQLite walks the
	// whole history newest first, which for a rarely played artist means
	// reading every listen.
	var from, where string
	switch kind {
	case "artist":
		from = `recording_artists x CROSS JOIN listens l INDEXED BY listens_by_recording ON l.recording_id = x.recording_id`
		where = `x.artist_id = ?2`
	case "song":
		from = `recordings x CROSS JOIN listens l INDEXED BY listens_by_recording ON l.recording_id = x.id`
		where = `x.song_id = ?2`
	case "release":
		from = `listens l INDEXED BY listens_by_release`
		where = `l.release_id = ?2`
	default:
		return nil, ErrNotFound
	}
	return db.scanListens(ctx, `SELECT `+listenCols+` FROM `+from+` JOIN sources s ON s.id = l.source_id
		WHERE `+where+` AND l.user_id = ?1 AND l.deleted_by IS NULL
		ORDER BY l.listened_at DESC, l.id DESC LIMIT ?3`, userID, id, limit)
}

// EntityListenCount is the all-time listen count of an artist, song or
// release, from the totals.
func (db *DB) EntityListenCount(ctx context.Context, userID int64, kind string, id int64) (int, error) {
	var q string
	switch kind {
	case "artist":
		q = `SELECT coalesce(sum(lt.n), 0) FROM listen_totals lt JOIN recording_artists ra ON ra.recording_id = lt.recording_id
			WHERE lt.user_id = ? AND ra.artist_id = ?`
	case "song":
		q = `SELECT coalesce(sum(lt.n), 0) FROM listen_totals lt JOIN recordings r ON r.id = lt.recording_id
			WHERE lt.user_id = ? AND r.song_id = ?`
	case "release":
		q = `SELECT coalesce(sum(n), 0) FROM listen_totals WHERE user_id = ? AND release_id = ?`
	}
	var n int
	err := db.r.QueryRowContext(ctx, q, userID, id).Scan(&n)
	return n, err
}

// EntityListenCountIn is the number of listens of an artist (counting their
// groups' too), song or release in [from, to), from the same totals the
// rankings use.
func (db *DB) EntityListenCountIn(ctx context.Context, userID int64, kind string, id, from, to int64) (int, error) {
	var where string
	switch kind {
	case "artist":
		where = `recording_id IN (SELECT recording_id FROM recording_artists WHERE artist_id = ?)`
	case "song":
		where = `recording_id IN (SELECT id FROM recordings WHERE song_id = ?)`
	case "release":
		where = `release_id = ?`
	default:
		return 0, ErrNotFound
	}
	pc, args := periodCounts(userID, from, to)
	var n int
	err := db.r.QueryRowContext(ctx, `WITH `+pc+` SELECT coalesce(sum(n), 0) FROM pc WHERE `+where, append(args, id)...).Scan(&n)
	return n, err
}

// ArtistAlbums lists the albums credited to an artist, including
// soundtracks shared with other composers, most listened first.
func (db *DB) ArtistAlbums(ctx context.Context, userID, artistID int64) ([]AlbumRank, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT rl.id, rl.name, rl.other_names, rl.context,
		coalesce((SELECT sum(n) FROM listen_totals WHERE user_id = ? AND release_id = rl.id), 0) AS n
		FROM release_credits rc JOIN releases rl ON rl.id = rc.release_id AND rl.merged_into IS NULL
		WHERE rc.artist_id = ? ORDER BY n DESC, rl.id`, userID, artistID)
	if err != nil {
		return nil, err
	}
	var out []AlbumRank
	for rows.Next() {
		var a AlbumRank
		if err := rows.Scan(&a.Album.ID, &a.Album.Name, &a.Album.OtherNames, &a.Context, &a.Listens); err != nil {
			rows.Close()
			return nil, err
		}
		a.Rank = len(out) + 1
		out = append(out, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]int64, len(out))
	for i := range out {
		ids[i] = out[i].Album.ID
	}
	return out, db.fillAlbums(ctx, out, ids)
}

// EntityStats are the numbers under an item's name.
type EntityStats struct {
	Listens     int
	Rank        int   // all time, as the ranking page counts by default. 0 when it isn't ranked
	First, Last int64 // when it was first and last listened to, 0 for never
}

// hiddenByDefault leaves out items with a label hidden in rankings by
// default, like characters, so the rank matches the ranking page.
const hiddenByDefault = `NOT EXISTS (SELECT 1 FROM entity_labels el JOIN labels lb ON lb.id = el.label_id
	WHERE lb.hide_default = 1 AND el.entity_type = ?3 AND el.entity_id = %s)`

func (db *DB) EntityStats(ctx context.Context, userID int64, kind string, id int64) (EntityStats, error) {
	var st EntityStats
	var err error
	if st.Listens, err = db.EntityListenCount(ctx, userID, kind, id); err != nil || st.Listens == 0 {
		return st, err
	}
	var totals, recs string
	switch kind {
	case "artist":
		totals = `SELECT ra.artist_id AS id, NULL AS rec, sum(lt.n) AS n FROM listen_totals lt JOIN recording_artists ra ON ra.recording_id = lt.recording_id
			WHERE lt.user_id = ?1 GROUP BY ra.artist_id`
		recs = `SELECT recording_id AS id FROM recording_artists WHERE artist_id = ?2`
	case "song":
		// The rows of Top songs as it shows by default, with versions
		// combined except those marked to rank on their own row.
		totals = `SELECT r.song_id AS id, CASE WHEN r.rank_alone = 1 THEN r.id END AS rec, sum(lt.n) AS n
			FROM listen_totals lt JOIN recordings r ON r.id = lt.recording_id
			WHERE lt.user_id = ?1 GROUP BY r.song_id, CASE WHEN r.rank_alone = 1 THEN r.id END`
		recs = `SELECT id FROM recordings WHERE song_id = ?2`
	case "release":
		totals = `SELECT release_id AS id, NULL AS rec, sum(n) AS n FROM listen_totals WHERE user_id = ?1 AND release_id <> 0 GROUP BY release_id`
	default:
		return st, ErrNotFound
	}
	hidden := fmt.Sprintf(hiddenByDefault, "t.id")
	self := fmt.Sprintf(hiddenByDefault, "?2")
	// The item's own row is the one the ranking page shows for it. For a
	// song that's the combined row, or its best version when every version
	// ranks on its own.
	err = db.r.QueryRowContext(ctx, `WITH t AS (`+totals+`)
		SELECT CASE WHEN `+self+` THEN 1 + (SELECT count(*) FROM t WHERE
		  t.n > (SELECT n FROM t WHERE id = ?2 ORDER BY rec IS NOT NULL, n DESC LIMIT 1) AND `+hidden+`) ELSE 0 END`,
		userID, id, kind).Scan(&st.Rank)
	if err != nil {
		return st, err
	}
	// First and last per recording (or for the release), each straight from
	// an index, rather than going through every listen in between.
	edge := func(fn, by string) string {
		return `(SELECT ` + fn + `(listened_at) FROM listens WHERE ` + by + ` AND user_id = ?1 AND deleted_by IS NULL)`
	}
	q := `SELECT coalesce(min(` + edge("min", "recording_id = r.id") + `), 0), coalesce(max(` + edge("max", "recording_id = r.id") + `), 0)
		FROM (` + recs + `) r`
	if kind == "release" {
		q = `SELECT coalesce(` + edge("min", "release_id = ?2") + `, 0), coalesce(` + edge("max", "release_id = ?2") + `, 0)`
	}
	err = db.r.QueryRowContext(ctx, q, userID, id).Scan(&st.First, &st.Last)
	return st, err
}
