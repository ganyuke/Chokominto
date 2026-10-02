package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// RankQuery selects listens in [From, To) and hides entities carrying any
// of HideLabels.
type RankQuery struct {
	From, To   int64
	HideLabels []int64
	Limit      int
	Offset     int
}

// Ref is a named entity as shown in tables.
type Ref struct {
	ID         int64
	Name       string
	OtherNames string
}

type SongRank struct {
	Rank        int
	SongID      int64
	RecordingID int64 // 0 on a combined song line
	Shown       int64 // the recording whose name and artists are shown
	Name        string
	OtherNames  string
	Version     string
	Artists     []Ref
	Listens     int
	Of          int // how many are ranked in all, beyond this page
}

type ArtistRank struct {
	Rank      int
	Artist    Ref
	Total     int
	Credited  int
	ViaGroups int
	Of        int
}

type AlbumRank struct {
	Rank    int
	Album   Ref
	Context string
	Artists []Ref
	Listens int
	Of      int
}

// allTimeEnd is where "all time" ends: any To past it counts as no end.
var allTimeEnd = time.Date(9000, 1, 1, 0, 0, 0, 0, time.UTC).Unix()

func yearBounds(y int) (int64, int64) {
	return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC).Unix(), time.Date(y+1, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
}

// periodCounts builds a CTE "pc(recording_id, release_id, n)" with the
// number of listens of each recording (and release, 0 for none) in
// [from, to).
//
// Counting every listen in a long period is too slow on a Pi, so whole UTC
// years come from the listen_years totals. A year is used when at least half
// of it falls inside the period. The parts of the period outside those
// years are counted listen by listen and added, and the parts of those years
// outside the period are counted and taken away. For a year in the owner's
// time zone, that's a few hours at each end. Short periods are counted
// directly.
func periodCounts(userID, from, to int64) (string, []any) {
	const raw = `SELECT recording_id, coalesce(release_id, 0) AS release_id, %s AS n FROM listens
		WHERE user_id = ? AND deleted_by IS NULL AND recording_id IS NOT NULL AND listened_at >= ? AND listened_at < ?`
	// All time: one row per recording and release.
	if from <= 0 && to >= allTimeEnd {
		return `pc AS (SELECT recording_id, release_id, n FROM listen_totals WHERE user_id = ?)`, []any{userID}
	}
	first, last := 0, -1
	if to > from {
		y0 := max(time.Unix(from, 0).UTC().Year(), 1970)
		y1 := min(time.Unix(to-1, 0).UTC().Year(), 9998)
		for y := y0; y <= y1; y++ {
			ys, ye := yearBounds(y)
			if 2*(min(to, ye)-max(from, ys)) >= ye-ys {
				if last < 0 {
					first = y
				}
				last = y
			}
		}
	}
	if last < 0 {
		return `pc AS (` + fmt.Sprintf(raw, "count(*)") + ` GROUP BY 1, 2)`, []any{userID, from, to}
	}
	ys, _ := yearBounds(first)
	_, ye := yearBounds(last)
	parts := []string{`SELECT recording_id, release_id, n FROM listen_years WHERE user_id = ? AND year BETWEEN ? AND ?`}
	args := []any{userID, first, last}
	span := func(sign string, a, b int64) {
		if a < b {
			parts = append(parts, fmt.Sprintf(raw, sign))
			args = append(args, userID, a, b)
		}
	}
	span("1", from, min(to, ys))  // before the years
	span("1", max(from, ye), to)  // after the years
	span("-1", ys, min(from, ye)) // in the years but before the period
	span("-1", max(to, ys), ye)   // in the years but after the period
	return `pc AS (SELECT recording_id, release_id, sum(n) AS n FROM (` + strings.Join(parts, " UNION ALL ") +
		`) GROUP BY recording_id, release_id HAVING sum(n) > 0)`, args
}

func hidden(entityType string, labels []int64) (string, []any) {
	if len(labels) == 0 {
		return "", nil
	}
	args := []any{entityType}
	for _, l := range labels {
		args = append(args, l)
	}
	return ` AND c.id NOT IN (SELECT entity_id FROM entity_labels WHERE entity_type = ? AND label_id IN (` +
		placeholders(len(labels)) + `))`, args
}

// TopSongs ranks recordings, or songs when combine is set (covers and
// versions counted together, except versions marked to rank on their own).
func (db *DB) TopSongs(ctx context.Context, userID int64, q RankQuery, combine bool) ([]SongRank, error) {
	pc, args := periodCounts(userID, q.From, q.To)
	// c has one row per ranked line: a recording (rec set) or, combining, a
	// whole song (rec NULL). A recording marked to rank on its own keeps
	// its own line even when combining.
	count := `SELECT recording_id AS rec, (SELECT song_id FROM recordings WHERE id = recording_id) AS song, sum(n) AS n
		FROM pc GROUP BY recording_id`
	if combine {
		count = `SELECT CASE WHEN r.rank_alone = 1 THEN r.id END AS rec, r.song_id AS song, sum(pc.n) AS n
			FROM pc JOIN recordings r ON r.id = pc.recording_id
			GROUP BY r.song_id, CASE WHEN r.rank_alone = 1 THEN r.id END`
	}
	hide := ""
	if len(q.HideLabels) > 0 {
		hide = ` AND c.song NOT IN (SELECT entity_id FROM entity_labels WHERE entity_type = 'song' AND label_id IN (` +
			placeholders(len(q.HideLabels)) + `))`
		for _, l := range q.HideLabels {
			args = append(args, l)
		}
	}
	query := `WITH ` + pc + `, c AS (` + count + `)
		SELECT c.rec, c.song, c.n FROM c WHERE 1 = 1` + hide + ` ORDER BY c.n DESC, c.song, c.rec`
	rows, err := db.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var out []SongRank
	var page pager
	for rows.Next() {
		var rec sql.NullInt64
		var s SongRank
		if err := rows.Scan(&rec, &s.SongID, &s.Listens); err != nil {
			rows.Close()
			return nil, err
		}
		if s.Rank = page.next(q, s.Listens); s.Rank > 0 {
			s.RecordingID = rec.Int64
			out = append(out, s)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// A combined song line shows its original recording.
	var whole []int64
	for _, s := range out {
		if s.RecordingID == 0 {
			whole = append(whole, s.SongID)
		}
	}
	originals, err := db.originalRecordings(ctx, whole)
	if err != nil {
		return nil, err
	}
	shown := make([]int64, len(out))
	for i := range out {
		out[i].Of = page.n
		out[i].Shown = out[i].RecordingID
		if out[i].RecordingID == 0 {
			out[i].Shown = originals[out[i].SongID]
		}
		shown[i] = out[i].Shown
	}
	infos, err := db.RecordingInfos(ctx, shown)
	if err != nil {
		return nil, err
	}
	for i := range out {
		s := &out[i]
		ri, ok := infos[s.Shown]
		if !ok {
			return nil, fmt.Errorf("recording %d of song %d: %w", s.Shown, s.SongID, sql.ErrNoRows)
		}
		s.SongID, s.Name, s.OtherNames, s.Artists = ri.Song.ID, ri.Song.Name, ri.Song.OtherNames, ri.Artists
		if s.RecordingID != 0 {
			s.Version = ri.Version
		}
	}
	return out, nil
}

// pager numbers a ranking read best first, the way RANK() does, and keeps
// the rows on the asked-for page. Numbering in Go rather than with window
// functions in the query is several times faster in SQLite, since those
// sort the whole ranking again.
type pager struct {
	n, rank, prev int // rows seen, rank of the last, its count
}

// next counts a row and returns its rank, or 0 when it's off the page.
func (p *pager) next(q RankQuery, count int) int {
	if p.n == 0 || count != p.prev {
		p.rank = p.n + 1
	}
	p.prev = count
	p.n++
	if p.n <= q.Offset || p.n > q.Offset+q.Limit {
		return 0
	}
	return p.rank
}

// originalRecordings is OriginalRecording for several songs at once.
func (db *DB) originalRecordings(ctx context.Context, songIDs []int64) (map[int64]int64, error) {
	out := map[int64]int64{}
	if len(songIDs) == 0 {
		return out, nil
	}
	list, _ := json.Marshal(songIDs)
	rows, err := db.r.QueryContext(ctx, `SELECT s.value, (`+originalOf("s.value")+`) FROM json_each(?) s`, string(list))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var song int64
		var rec sql.NullInt64
		if err := rows.Scan(&song, &rec); err != nil {
			return nil, err
		}
		if rec.Valid {
			out[song] = rec.Int64
		}
	}
	return out, rows.Err()
}

// originalOf is a subquery for the recording a combined song row shows: the
// one marked original, otherwise the one listened to first.
func originalOf(song string) string {
	return `SELECT r.id FROM recordings r WHERE r.song_id = ` + song + ` AND r.merged_into IS NULL
		 ORDER BY r.is_original DESC,
		   (SELECT min(listened_at) FROM listens WHERE recording_id = r.id AND deleted_by IS NULL) IS NULL,
		   (SELECT min(listened_at) FROM listens WHERE recording_id = r.id AND deleted_by IS NULL),
		   r.id LIMIT 1`
}

// OriginalRecording is the recording a combined song row shows: the one
// marked original, otherwise the one listened to first.
func (db *DB) OriginalRecording(ctx context.Context, songID int64) (int64, error) {
	var id int64
	err := db.r.QueryRowContext(ctx, originalOf("?"), songID).Scan(&id)
	return id, err
}

// RecordingArtists are the main and featured credits, in order.
func (db *DB) RecordingArtists(ctx context.Context, recordingID int64) ([]Ref, error) {
	return db.refs(ctx, `SELECT a.id, a.name, a.other_names FROM recording_credits rc JOIN artists a ON a.id = rc.artist_id
		WHERE rc.recording_id = ? AND rc.role IN ('main', 'featured') ORDER BY rc.role = 'featured', rc.position`, recordingID)
}

func (db *DB) refs(ctx context.Context, q string, args ...any) ([]Ref, error) {
	rows, err := db.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ref
	for rows.Next() {
		var r Ref
		if err := rows.Scan(&r.ID, &r.Name, &r.OtherNames); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ArtistSort is the column top artists are ranked by.
type ArtistSort string

const (
	ByTotal     ArtistSort = "total"
	ByCredited  ArtistSort = "credited"
	ByViaGroups ArtistSort = "groups"
)

func (db *DB) TopArtists(ctx context.Context, userID int64, q RankQuery, sort ArtistSort) ([]ArtistRank, error) {
	col := map[ArtistSort]string{ByTotal: "total", ByCredited: "credited", ByViaGroups: "groups"}[sort]
	if col == "" {
		col = "total"
	}
	pc, args := periodCounts(userID, q.From, q.To)
	hide, hideArgs := hidden("artist", q.HideLabels)
	query := fmt.Sprintf(`WITH %s, c AS (
		SELECT ra.artist_id AS id, sum(pc.n) AS total,
		       sum(CASE WHEN ra.via = 'credited' THEN pc.n ELSE 0 END) AS credited,
		       sum(CASE WHEN ra.via = 'group' THEN pc.n ELSE 0 END) AS groups
		FROM pc JOIN recording_artists ra ON ra.recording_id = pc.recording_id
		GROUP BY ra.artist_id)
		SELECT c.id, c.total, c.credited, c.groups FROM c WHERE c.%s > 0 %s
		ORDER BY c.%s DESC, c.total DESC, c.id`, pc, col, hide, col)
	args = append(args, hideArgs...)
	rows, err := db.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var out []ArtistRank
	var page pager
	for rows.Next() {
		var a ArtistRank
		if err := rows.Scan(&a.Artist.ID, &a.Total, &a.Credited, &a.ViaGroups); err != nil {
			rows.Close()
			return nil, err
		}
		by := map[ArtistSort]int{ByCredited: a.Credited, ByViaGroups: a.ViaGroups}[sort]
		if col == "total" {
			by = a.Total
		}
		if a.Rank = page.next(q, by); a.Rank > 0 {
			out = append(out, a)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]int64, len(out))
	for i := range out {
		out[i].Of = page.n
		ids[i] = out[i].Artist.ID
	}
	refs, err := db.artistRefs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Artist = refs[out[i].Artist.ID]
	}
	return out, nil
}

// artistRefs looks up several artists' names at once.
func (db *DB) artistRefs(ctx context.Context, ids []int64) (map[int64]Ref, error) {
	out := map[int64]Ref{}
	if len(ids) == 0 {
		return out, nil
	}
	list, _ := json.Marshal(ids)
	rs, err := db.refs(ctx, `SELECT id, name, other_names FROM artists WHERE id IN (SELECT value FROM json_each(?))`, string(list))
	for _, r := range rs {
		out[r.ID] = r
	}
	return out, err
}

func (db *DB) TopAlbums(ctx context.Context, userID int64, q RankQuery) ([]AlbumRank, error) {
	pc, args := periodCounts(userID, q.From, q.To)
	hide, hideArgs := hidden("release", q.HideLabels)
	query := `WITH ` + pc + `, c AS (SELECT release_id AS id, sum(n) AS n FROM pc WHERE release_id <> 0 GROUP BY release_id)
		SELECT c.id, c.n FROM c WHERE 1 = 1 ` + hide + ` ORDER BY c.n DESC, c.id`
	args = append(args, hideArgs...)
	rows, err := db.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var out []AlbumRank
	var page pager
	for rows.Next() {
		var a AlbumRank
		if err := rows.Scan(&a.Album.ID, &a.Listens); err != nil {
			rows.Close()
			return nil, err
		}
		if a.Rank = page.next(q, a.Listens); a.Rank > 0 {
			out = append(out, a)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]int64, len(out))
	for i := range out {
		out[i].Of = page.n
		ids[i] = out[i].Album.ID
	}
	if err := db.fillAlbums(ctx, out, ids); err != nil {
		return nil, err
	}
	return out, nil
}

// fillAlbums adds the names, context and artists of a page of albums,
// in two queries whatever the page's length.
func (db *DB) fillAlbums(ctx context.Context, out []AlbumRank, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	list, _ := json.Marshal(ids)
	rows, err := db.r.QueryContext(ctx, `SELECT id, name, other_names, context FROM releases WHERE id IN (SELECT value FROM json_each(?))`, string(list))
	if err != nil {
		return err
	}
	type info struct {
		ref     Ref
		context string
	}
	infos := map[int64]info{}
	for rows.Next() {
		var x info
		if err := rows.Scan(&x.ref.ID, &x.ref.Name, &x.ref.OtherNames, &x.context); err != nil {
			rows.Close()
			return err
		}
		infos[x.ref.ID] = x
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = db.r.QueryContext(ctx, `SELECT rc.release_id, a.id, a.name, a.other_names FROM release_credits rc
		JOIN artists a ON a.id = rc.artist_id WHERE rc.release_id IN (SELECT value FROM json_each(?))
		ORDER BY rc.release_id, rc.position`, string(list))
	if err != nil {
		return err
	}
	defer rows.Close()
	artists := map[int64][]Ref{}
	for rows.Next() {
		var rel int64
		var a Ref
		if err := rows.Scan(&rel, &a.ID, &a.Name, &a.OtherNames); err != nil {
			return err
		}
		artists[rel] = append(artists[rel], a)
	}
	for i := range out {
		x := infos[out[i].Album.ID]
		out[i].Album, out[i].Context, out[i].Artists = x.ref, x.context, artists[out[i].Album.ID]
	}
	return rows.Err()
}

type Label struct {
	ID          int64
	Name        string
	HideDefault bool
}

// LabelsInUse lists the owner's labels that are on at least one entity of
// the given type, for a ranking's Hide row.
func (db *DB) LabelsInUse(ctx context.Context, userID int64, entityType string) ([]Label, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT l.id, l.name, l.hide_default FROM labels l WHERE l.user_id = ?
		 AND EXISTS (SELECT 1 FROM entity_labels el WHERE el.label_id = l.id AND el.entity_type = ?)
		 ORDER BY l.position, l.name`, userID, entityType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Label
	for rows.Next() {
		var l Label
		if err := rows.Scan(&l.ID, &l.Name, &l.HideDefault); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// JoinNames is "A, B" for display.
func JoinNames(rs []Ref) string {
	n := make([]string, len(rs))
	for i, r := range rs {
		n[i] = r.Name
	}
	return strings.Join(n, ", ")
}

// ListenCountIn is the number of linked listens in [from, to), from the
// same totals the rankings use.
func (db *DB) ListenCountIn(ctx context.Context, userID, from, to int64) (int, error) {
	pc, args := periodCounts(userID, from, to)
	var n int
	err := db.r.QueryRowContext(ctx, `WITH `+pc+` SELECT coalesce(sum(n), 0) FROM pc`, args...).Scan(&n)
	return n, err
}
