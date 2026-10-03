package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"chokominto/internal/names"
)

// The received text behind songs and albums: listing it with how it was
// linked, moving it between albums, and merging several albums at once.

// LinkedSource is received text with what it's linked to and how.
type LinkedSource struct {
	UnlinkedSource
	OnAlbum Ref // the album it's on, ID 0 for none
	// How the link was made: "" for not linked, "auto" for read
	// automatically, "rule" for one of the owner's rules, or the kind of
	// the edit that set it by hand (link, merge, review, edit).
	SetBy  string
	RuleID int64
	Agent  string // the agent that set it, if one did
	SetAt  int64
}

// Fixed reports whether the link was set by a person or an agent, so
// reading the text again never moves it.
func (s LinkedSource) Fixed() bool {
	return s.SetBy != "" && s.SetBy != "auto" && s.SetBy != "rule"
}

const linkedSourceCols = `s.id, s.user_id, s.artist_text, s.title_text, s.album_text, s.album_artist_text,
	s.recording_id, s.release_id, s.msid,
	(SELECT count(*) FROM listens l WHERE l.source_id = s.id AND l.deleted_by IS NULL AND l.fixed_by IS NULL),
	coalesce((SELECT max(id) FROM listens l WHERE l.source_id = s.id AND l.deleted_by IS NULL AND l.fixed_by IS NULL), 0),
	coalesce(rl.id, 0), coalesce(rl.name, ''), coalesce(rl.other_names, ''),
	e.kind, e.automatic, e.rule_id, e.created_at, t.label`

const linkedSourceJoins = ` LEFT JOIN releases rl ON rl.id = s.release_id
	LEFT JOIN edits e ON e.id = s.linked_by LEFT JOIN api_tokens t ON t.id = e.agent_token_id`

func (db *DB) linkedSources(ctx context.Context, q string, args ...any) ([]LinkedSource, error) {
	rows, err := db.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LinkedSource
	for rows.Next() {
		var s LinkedSource
		var kind, agent sql.NullString
		var auto sql.NullBool
		var rule, at sql.NullInt64
		if err := rows.Scan(&s.ID, &s.UserID, &s.Artist, &s.Title, &s.Album, &s.AlbumArtist, &s.RecordingID, &s.ReleaseID, &s.MSID,
			&s.Listens, &s.LatestListen, &s.OnAlbum.ID, &s.OnAlbum.Name, &s.OnAlbum.OtherNames, &kind, &auto, &rule, &at, &agent); err != nil {
			return nil, err
		}
		s.LinkedByOwner = auto.Valid && !auto.Bool
		s.RuleID, s.SetAt, s.Agent = rule.Int64, at.Int64, agent.String
		switch {
		case !s.RecordingID.Valid:
		case !auto.Valid:
			s.SetBy = "auto" // handed back to be read automatically
		case !auto.Bool:
			s.SetBy = kind.String
		case rule.Valid:
			s.SetBy = "rule"
		default:
			s.SetBy = "auto"
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SourcesOf lists the received text linked to a song (any of its versions)
// or an album, most listens first. Text none of whose listens are left is
// left out, and so are listens fixed alone (see FixedListens).
func (db *DB) SourcesOf(ctx context.Context, userID int64, kind string, id int64) ([]LinkedSource, error) {
	// Start from the item's own text. Left to itself, SQLite reads all of
	// the user's text.
	var index, where string
	switch kind {
	case "song":
		index, where = "sources_by_recording", `s.recording_id IN (SELECT id FROM recordings WHERE song_id = ?2 AND merged_into IS NULL)`
	case "release":
		index, where = "sources_by_release", `s.release_id = ?2 AND s.release_id IS NOT NULL`
	default:
		return nil, ErrNotFound
	}
	all, err := db.linkedSources(ctx, `SELECT `+linkedSourceCols+` FROM sources s INDEXED BY `+index+linkedSourceJoins+`
		WHERE s.user_id = ?1 AND `+where+` ORDER BY 10 DESC, s.id`, userID, id)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(s LinkedSource) bool { return s.LatestListen == 0 }), nil
}

// LinkedSourceInfo is one received text with how it was linked.
func (db *DB) LinkedSourceInfo(ctx context.Context, userID, sourceID int64) (LinkedSource, error) {
	found, err := db.linkedSources(ctx, `SELECT `+linkedSourceCols+` FROM sources s`+linkedSourceJoins+`
		WHERE s.user_id = ? AND s.id = ?`, userID, sourceID)
	if err != nil {
		return LinkedSource{}, err
	}
	if len(found) == 0 {
		return LinkedSource{}, ErrNotFound
	}
	return found[0], nil
}

// FixedSources lists received text whose link was set by a person or an
// agent, most listens first, with how many there are in all. q narrows it
// to text containing q.
func (db *DB) FixedSources(ctx context.Context, userID int64, q string, limit, offset int) ([]LinkedSource, int, error) {
	key := names.MatchKey(q)
	// Each text's edit is looked up by id. Joined the other way round,
	// SQLite walks every edit and then every text for each.
	const fixed = `SELECT s.id FROM sources s INDEXED BY sources_linked CROSS JOIN edits e ON e.id = s.linked_by
		WHERE s.user_id = ?1 AND s.recording_id IS NOT NULL AND s.linked_by IS NOT NULL AND e.automatic = 0
		AND (?2 = '' OR instr(s.search_key, ?2) > 0)`
	var total int
	if err := db.r.QueryRowContext(ctx, `SELECT count(*) FROM (`+fixed+`)`, userID, key).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit == 0 {
		return nil, total, nil
	}
	out, err := db.linkedSources(ctx, `WITH fixed AS MATERIALIZED (`+fixed+`)
		SELECT `+linkedSourceCols+` FROM fixed f CROSS JOIN sources s ON s.id = f.id`+linkedSourceJoins+`
		ORDER BY 10 DESC, s.id LIMIT ?3 OFFSET ?4`, userID, key, limit, offset)
	return out, total, err
}

// liveRelease checks an album belongs to the user and wasn't merged.
func liveRelease(ctx context.Context, tx *sql.Tx, userID, id int64) (string, error) {
	var name string
	var merged sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT name, merged_into FROM releases WHERE id = ? AND user_id = ?`, id, userID).Scan(&name, &merged)
	if errors.Is(err, sql.ErrNoRows) || merged.Valid {
		return "", ErrNotFound
	}
	return name, err
}

// SetSourcesRelease puts received text on an album, or on none when
// releaseID is 0, as one edit. Every listen with that text follows, and the
// song stays the same. The text counts as linked by hand from then on. Text
// that isn't linked to a song is refused.
func (db *DB) SetSourcesRelease(ctx context.Context, userID int64, sourceIDs []int64, releaseID int64) (LinkResult, error) {
	var res LinkResult
	err := db.Write(ctx, func(tx *sql.Tx) error {
		var release any
		if releaseID != 0 {
			name, err := liveRelease(ctx, tx, userID, releaseID)
			if err != nil {
				return err
			}
			res.Name, release = name, releaseID
		}
		p := &plan{ctx: ctx, tx: tx, userID: userID}
		planned := map[int64]bool{}
		ids := slices.Compact(slices.Sorted(slices.Values(sourceIDs)))
		for _, id := range ids {
			s, err := SourceTx(ctx, tx, id)
			if err != nil || s.UserID != userID || !s.RecordingID.Valid {
				return ErrNotFound
			}
			if s.ReleaseID.Valid == (releaseID != 0) && s.ReleaseID.Int64 == releaseID {
				continue // there already
			}
			rec := s.RecordingID.Int64
			if releaseID != 0 && !planned[rec] {
				planned[rec] = true
				track := map[string]any{"release_id": releaseID, "recording_id": rec, "disc": int64(1), "position": nil}
				there, err := p.exists("release_tracks", track)
				if err != nil {
					return err
				}
				if !there {
					p.add(Change{Op: OpInsert, Table: "release_tracks", After: track})
				}
			}
			var n int
			tx.QueryRowContext(ctx, `SELECT count(*) FROM listens WHERE source_id = ? AND deleted_by IS NULL AND fixed_by IS NULL`, id).Scan(&n)
			res.Listens += n
			before := map[string]any{"release_id": nullable(s.ReleaseID), "linked_by": currentLinkedBy(ctx, tx, s.ID)}
			p.changes = append(p.changes, func(editID int64) Change {
				return Change{Table: "sources", ID: s.ID, Before: before, After: map[string]any{"release_id": release, "linked_by": editID}}
			})
		}
		if len(ids) == 0 {
			return ErrNotFound
		}
		if len(p.changes) == 0 {
			return nil // all there already
		}
		summary := fmt.Sprintf("Put %s on the album %s", plural(res.Listens, "listen"), res.Name)
		if releaseID == 0 {
			summary = fmt.Sprintf("Took %s off their album", plural(res.Listens, "listen"))
		}
		var err error
		res.EditID, err = p.apply(EditMeta{Kind: "link", Summary: summary})
		return err
	})
	return res, err
}

// AlbumCount is an album with listens, and for related albums how many
// songs it shares.
type AlbumCount struct {
	Ref
	Context string
	Listens int
	Shared  int
}

func (db *DB) albumCounts(ctx context.Context, q string, args ...any) ([]AlbumCount, error) {
	rows, err := db.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlbumCount
	for rows.Next() {
		var a AlbumCount
		if err := rows.Scan(&a.ID, &a.Name, &a.OtherNames, &a.Context, &a.Listens, &a.Shared); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RelatedReleases lists other albums that hold a version of a song this
// album holds, the ones sharing most songs first, with all their listens.
func (db *DB) RelatedReleases(ctx context.Context, userID, releaseID int64, limit int) ([]AlbumCount, error) {
	return db.albumCounts(ctx, `WITH shared AS (
		  SELECT rt2.release_id AS id, count(DISTINCT r.song_id) AS songs
		  FROM release_tracks rt JOIN recordings r ON r.id = rt.recording_id
		  JOIN recordings r2 ON r2.song_id = r.song_id AND r2.merged_into IS NULL
		  JOIN release_tracks rt2 ON rt2.recording_id = r2.id AND rt2.release_id <> ?2
		  WHERE rt.release_id = ?2 GROUP BY rt2.release_id)
		SELECT rl.id, rl.name, rl.other_names, coalesce(rl.context, ''),
		  coalesce((SELECT sum(n) FROM listen_totals WHERE user_id = ?1 AND release_id = rl.id), 0), sh.songs
		FROM shared sh JOIN releases rl ON rl.id = sh.id AND rl.merged_into IS NULL AND rl.user_id = ?1
		ORDER BY sh.songs DESC, 5 DESC, rl.id LIMIT ?3`, userID, releaseID, limit)
}

// SongAlbums lists the albums a song is on, with the listens of the song
// from each, most listened first.
func (db *DB) SongAlbums(ctx context.Context, userID, songID int64) ([]AlbumCount, error) {
	return db.albumCounts(ctx, `SELECT rl.id, rl.name, rl.other_names, coalesce(rl.context, ''),
		  coalesce(sum((SELECT n FROM listen_totals WHERE user_id = ?1 AND recording_id = r.id AND release_id = rl.id)), 0), count(*)
		FROM recordings r JOIN release_tracks rt ON rt.recording_id = r.id
		JOIN releases rl ON rl.id = rt.release_id AND rl.merged_into IS NULL
		WHERE r.song_id = ?2 AND r.merged_into IS NULL GROUP BY rl.id ORDER BY 5 DESC, rl.name, rl.id`, userID, songID)
}

// RecordingAlbums lists the albums each of the given recordings is on.
func (db *DB) RecordingAlbums(ctx context.Context, ids []int64) (map[int64][]Ref, error) {
	out := map[int64][]Ref{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := db.r.QueryContext(ctx, `SELECT rt.recording_id, rl.id, rl.name, rl.other_names FROM release_tracks rt
		JOIN releases rl ON rl.id = rt.release_id AND rl.merged_into IS NULL
		WHERE rt.recording_id IN (SELECT value FROM json_each(?)) ORDER BY rt.recording_id, rl.name, rl.id`, jsonIDs(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rec int64
		var r Ref
		if err := rows.Scan(&rec, &r.ID, &r.Name, &r.OtherNames); err != nil {
			return nil, err
		}
		out[rec] = append(out[rec], r)
	}
	return out, rows.Err()
}

// MergeMany merges several items of one kind into winner, as one edit.
func (db *DB) MergeMany(ctx context.Context, userID int64, kind string, losers []int64, winner int64) (int64, error) {
	var editID int64
	err := db.Write(ctx, func(tx *sql.Tx) error {
		e, err := OpenEditTx(ctx, tx, userID, EditMeta{Kind: "merge"})
		if err != nil {
			return err
		}
		editID = e.ID
		var merged []string
		for _, loser := range slices.Compact(slices.Sorted(slices.Values(losers))) {
			if loser == winner {
				continue
			}
			p := &plan{ctx: ctx, tx: tx, userID: userID}
			name, err := p.mergeInto(kind, loser, winner)
			if err != nil {
				return err
			}
			if err := p.applyTo(e); err != nil {
				return err
			}
			merged = append(merged, name)
		}
		if len(merged) == 0 {
			return ErrMergeSelf
		}
		wname, err := entityName(ctx, tx, kind, winner)
		if err != nil {
			return err
		}
		if err := e.Summarize(fmt.Sprintf("Merged %s into %s", strings.Join(merged, ", "), wname)); err != nil {
			return err
		}
		return e.Close()
	})
	return editID, err
}

// Listens linked on their own

// AlbumChoice says which album a listen linked on its own goes on.
type AlbumChoice struct {
	Keep bool  // the album it's on now
	ID   int64 // else this one, 0 for none
}

// LinkListen links one listen to a recording by hand, apart from the other
// listens sent with the same text, as one edit. The listen stops following
// its text until UnfixListen.
func (db *DB) LinkListen(ctx context.Context, userID, listenID, recordingID int64, album AlbumChoice) (LinkResult, error) {
	res := LinkResult{Listens: 1}
	err := db.Write(ctx, func(tx *sql.Tx) error {
		var err error
		res, err = LinkListenTx(ctx, tx, userID, listenID, recordingID, album)
		return err
	})
	return res, err
}

func LinkListenTx(ctx context.Context, tx *sql.Tx, userID, listenID, recordingID int64, album AlbumChoice) (LinkResult, error) {
	res := LinkResult{Listens: 1}
	var rec, rel, fixed sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT recording_id, release_id, fixed_by FROM listens WHERE id = ? AND user_id = ? AND deleted_by IS NULL`,
		listenID, userID).Scan(&rec, &rel, &fixed)
	if errors.Is(err, sql.ErrNoRows) {
		return res, ErrNotFound
	}
	if err != nil {
		return res, err
	}
	if err := liveRecording(ctx, tx, userID, recordingID); err != nil {
		return res, err
	}
	if res.Name, err = entityName(ctx, tx, "recording", recordingID); err != nil {
		return res, err
	}
	p := &plan{ctx: ctx, tx: tx, userID: userID}
	release := nullable(rel)
	if !album.Keep {
		release = nullID(album.ID)
	}
	albumName := ""
	if id, ok := release.(int64); ok {
		if albumName, err = liveRelease(ctx, tx, userID, id); err != nil {
			return res, err
		}
		track := map[string]any{"release_id": id, "recording_id": recordingID, "disc": int64(1), "position": nil}
		there, err := p.exists("release_tracks", track)
		if err != nil {
			return res, err
		}
		if !there {
			p.add(Change{Op: OpInsert, Table: "release_tracks", After: track})
		}
	}
	before := map[string]any{"recording_id": nullable(rec), "release_id": nullable(rel), "fixed_by": nullable(fixed)}
	p.changes = append(p.changes, func(editID int64) Change {
		return Change{Table: "listens", ID: listenID, Before: before,
			After: map[string]any{"recording_id": recordingID, "release_id": release, "fixed_by": editID}}
	})
	res.From = fromName(ctx, tx, map[int64]bool{rec.Int64: true}, recordingID)
	summary := moveSummary(1, res.From, res.Name)
	if rec.Valid && rec.Int64 == recordingID {
		summary = "Put 1 listen of " + res.Name + " on the album " + albumName
		if albumName == "" {
			summary = "Took 1 listen of " + res.Name + " off its album"
		}
	}
	res.EditID, err = p.apply(EditMeta{Kind: "link", Summary: summary})
	return res, err
}

// UnfixListen makes a listen linked on its own follow its text again, as
// one edit. It returns 0 when the listen wasn't linked on its own.
func (db *DB) UnfixListen(ctx context.Context, userID, listenID int64) (int64, error) {
	var editID int64
	err := db.Write(ctx, func(tx *sql.Tx) error {
		var rec, rel, fixed, srcRec, srcRel sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT l.recording_id, l.release_id, l.fixed_by, s.recording_id, s.release_id
			FROM listens l JOIN sources s ON s.id = l.source_id WHERE l.id = ? AND l.user_id = ? AND l.deleted_by IS NULL`,
			listenID, userID).Scan(&rec, &rel, &fixed, &srcRec, &srcRel)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil || !fixed.Valid {
			return err
		}
		editID, err = ApplyEditTx(ctx, tx, userID, EditMeta{Kind: "link", Summary: "Linked 1 listen like the others sent with the same text again"},
			func(int64) []Change {
				return []Change{{Table: "listens", ID: listenID,
					Before: map[string]any{"recording_id": nullable(rec), "release_id": nullable(rel), "fixed_by": fixed.Int64},
					After:  map[string]any{"recording_id": nullable(srcRec), "release_id": nullable(srcRel), "fixed_by": nil}}}
			})
		return err
	})
	return editID, err
}

// FixedListens lists the listens linked on their own to a song (any of its
// versions) or an album, newest first.
func (db *DB) FixedListens(ctx context.Context, userID int64, kind string, id int64) ([]Listen, error) {
	var where string
	switch kind {
	case "song":
		where = `l.recording_id IN (SELECT id FROM recordings WHERE song_id = ?2 AND merged_into IS NULL)`
	case "release":
		where = `l.release_id = ?2`
	default:
		return nil, ErrNotFound
	}
	return db.scanListens(ctx, `SELECT `+listenCols+` FROM listens l INDEXED BY listens_fixed JOIN sources s ON s.id = l.source_id
		WHERE l.user_id = ?1 AND l.fixed_by IS NOT NULL AND l.deleted_by IS NULL AND `+where+`
		ORDER BY l.listened_at DESC, l.id DESC`, userID, id)
}

// SourceLink is where one received text should point.
type SourceLink struct {
	SourceID    int64
	RecordingID int64
	ReleaseID   int64 // 0 for no album
}

// LinkSourcesToTx points received texts at the given recordings and albums,
// as one edit by the owner. By hand, the texts stay there when text is read
// again. Otherwise they count as read automatically from then on, which is
// how text linked by hand is handed back. The recordings must already be on
// their albums. summary is used as given, or says what moved when it's "".
func LinkSourcesToTx(ctx context.Context, tx *sql.Tx, userID int64, links []SourceLink, byHand bool, summary string) (LinkResult, error) {
	var res LinkResult
	p := &plan{ctx: ctx, tx: tx, userID: userID}
	from, to := map[int64]bool{}, map[int64]bool{}
	for _, l := range links {
		s, err := SourceTx(ctx, tx, l.SourceID)
		if err != nil || s.UserID != userID {
			return res, ErrNotFound
		}
		if err := liveRecording(ctx, tx, userID, l.RecordingID); err != nil {
			return res, err
		}
		var n int
		tx.QueryRowContext(ctx, `SELECT count(*) FROM listens WHERE source_id = ? AND deleted_by IS NULL AND fixed_by IS NULL`, s.ID).Scan(&n)
		res.Listens += n
		from[s.RecordingID.Int64], to[l.RecordingID] = true, true
		before := map[string]any{"recording_id": nullable(s.RecordingID), "release_id": nullable(s.ReleaseID),
			"linked_by": currentLinkedBy(ctx, tx, s.ID)}
		rec, rel := l.RecordingID, nullID(l.ReleaseID)
		p.changes = append(p.changes, func(editID int64) Change {
			var by any
			if byHand {
				by = editID
			}
			return Change{Table: "sources", ID: s.ID, Before: before,
				After: map[string]any{"recording_id": rec, "release_id": rel, "linked_by": by}}
		})
	}
	if len(links) == 0 {
		return res, ErrNotFound
	}
	if len(to) == 1 {
		for id := range to {
			res.Name, _ = entityName(ctx, tx, "recording", id)
			res.From = fromName(ctx, tx, from, id)
		}
	}
	if summary == "" {
		summary = moveSummary(res.Listens, res.From, res.Name)
	}
	var err error
	res.EditID, err = p.apply(EditMeta{Kind: "link", Summary: summary})
	return res, err
}

// LinkScope says what a fix made from one listen applies to. The two parts
// are independent: which of the listens sent so far move, and whether later
// listens sent with the same text follow.
type LinkScope struct {
	All   bool // every listen sent with the text so far, not only the one
	Later bool // later listens sent with the same text too
}

// LinkTarget is where a fix sends listens.
type LinkTarget struct {
	RecordingID int64
	Album       AlbumChoice
}

// LinkScoped moves listens to a recording and album by hand, as one edit,
// starting from one listen. See linkScoped for what each scope does.
func (db *DB) LinkScoped(ctx context.Context, userID, listenID int64, t LinkTarget, sc LinkScope) (LinkResult, error) {
	var res LinkResult
	err := db.Write(ctx, func(tx *sql.Tx) error {
		var err error
		res, err = LinkScopedTx(ctx, tx, userID, listenID, t, sc)
		return err
	})
	return res, err
}

func LinkScopedTx(ctx context.Context, tx *sql.Tx, userID, listenID int64, t LinkTarget, sc LinkScope) (LinkResult, error) {
	if err := liveRecording(ctx, tx, userID, t.RecordingID); err != nil {
		return LinkResult{}, err
	}
	name, err := entityName(ctx, tx, "recording", t.RecordingID)
	if err != nil {
		return LinkResult{}, err
	}
	p := &plan{ctx: ctx, tx: tx, userID: userID}
	res, summary, err := p.linkScoped(listenID, t, sc, name)
	if err != nil {
		return res, err
	}
	res.EditID, err = p.apply(EditMeta{Kind: "link", Summary: summary})
	return res, err
}

// linkScoped plans a fix made from one listen, and says what it does.
//
//   - This listen, not later ones: the listen is linked on its own.
//   - Every listen so far, not later ones: each is linked on its own, and
//     the text keeps its link, so later listens go where they went.
//   - This listen and later ones: the text is linked, and the other listens
//     sent so far are each kept where they are.
//   - Every listen so far and later ones: the text is linked, and all its
//     listens follow.
//
// name is the target recording's name, which may be one the plan adds.
func (p *plan) linkScoped(listenID int64, t LinkTarget, sc LinkScope, name string) (LinkResult, string, error) {
	res := LinkResult{Name: name}
	type row struct {
		id              int64
		rec, rel, fixed sql.NullInt64
	}
	var this row
	var sourceID int64
	err := p.tx.QueryRowContext(p.ctx, `SELECT id, recording_id, release_id, fixed_by, source_id FROM listens WHERE id = ? AND user_id = ? AND deleted_by IS NULL`,
		listenID, p.userID).Scan(&this.id, &this.rec, &this.rel, &this.fixed, &sourceID)
	if errors.Is(err, sql.ErrNoRows) {
		return res, "", ErrNotFound
	}
	if err != nil {
		return res, "", err
	}
	s, err := SourceTx(p.ctx, p.tx, sourceID)
	if err != nil {
		return res, "", err
	}
	// The other listens sent with the text that still follow it.
	rows, err := p.tx.QueryContext(p.ctx, `SELECT id, recording_id, release_id, fixed_by FROM listens
		WHERE source_id = ? AND deleted_by IS NULL AND fixed_by IS NULL AND id <> ? ORDER BY id`, sourceID, listenID)
	if err != nil {
		return res, "", err
	}
	var others []row
	for rows.Next() {
		var o row
		if err := rows.Scan(&o.id, &o.rec, &o.rel, &o.fixed); err != nil {
			rows.Close()
			return res, "", err
		}
		others = append(others, o)
	}
	rows.Close()

	albumName := ""
	if !t.Album.Keep && t.Album.ID != 0 {
		if albumName, err = liveRelease(p.ctx, p.tx, p.userID, t.Album.ID); err != nil {
			return res, "", err
		}
	}
	tracks := map[[2]int64]bool{}
	// albumFor is the album a listen ends up on, which gets the recording
	// as a track when it hasn't got it.
	albumFor := func(cur sql.NullInt64) (any, error) {
		rel := t.Album.ID
		if t.Album.Keep {
			rel = cur.Int64
		}
		if rel == 0 {
			return nil, nil
		}
		track := map[string]any{"release_id": rel, "recording_id": t.RecordingID, "disc": int64(1), "position": nil}
		there, err := p.exists("release_tracks", track)
		if err != nil {
			return nil, err
		}
		if !there && !tracks[[2]int64{rel, t.RecordingID}] {
			p.add(Change{Op: OpInsert, Table: "release_tracks", After: track})
			tracks[[2]int64{rel, t.RecordingID}] = true
		}
		return rel, nil
	}
	alone := func(l row) error {
		release, err := albumFor(l.rel)
		if err != nil {
			return err
		}
		before := map[string]any{"recording_id": nullable(l.rec), "release_id": nullable(l.rel), "fixed_by": nullable(l.fixed)}
		p.changes = append(p.changes, func(editID int64) Change {
			return Change{Table: "listens", ID: l.id, Before: before,
				After: map[string]any{"recording_id": t.RecordingID, "release_id": release, "fixed_by": editID}}
		})
		return nil
	}

	from := this.rec
	switch {
	case sc.Later:
		if !sc.All {
			// The others stay where they are, each on its own.
			for _, o := range others {
				p.changes = append(p.changes, func(editID int64) Change {
					return Change{Table: "listens", ID: o.id, Before: map[string]any{"fixed_by": nil}, After: map[string]any{"fixed_by": editID}}
				})
			}
		} else {
			from = s.RecordingID
		}
		var release any
		if t.Album.Keep {
			release, err = p.releaseFor(s, t.RecordingID, tracks)
		} else {
			release, err = albumFor(sql.NullInt64{})
		}
		if err != nil {
			return res, "", err
		}
		p.linkSource(s, t.RecordingID, release)
		if this.fixed.Valid {
			// Linked on its own before, it follows its text again.
			p.add(Change{Table: "listens", ID: this.id,
				Before: map[string]any{"recording_id": nullable(this.rec), "release_id": nullable(this.rel), "fixed_by": this.fixed.Int64},
				After:  map[string]any{"recording_id": t.RecordingID, "release_id": release, "fixed_by": nil}})
		}
	case sc.All:
		from = s.RecordingID
		for _, l := range append([]row{this}, others...) {
			if err := alone(l); err != nil {
				return res, "", err
			}
		}
	default:
		if err := alone(this); err != nil {
			return res, "", err
		}
	}
	res.Listens = 1
	if sc.All {
		res.Listens += len(others)
	}
	res.From = fromName(p.ctx, p.tx, map[int64]bool{from.Int64: true}, t.RecordingID)
	summary := moveSummary(res.Listens, res.From, name)
	if from.Valid && from.Int64 == t.RecordingID && !t.Album.Keep {
		summary = fmt.Sprintf("Put %s of %s on the album %s", plural(res.Listens, "listen"), name, albumName)
		if t.Album.ID == 0 {
			summary = fmt.Sprintf("Took %s of %s off their album", plural(res.Listens, "listen"), name)
		}
	}
	switch {
	case sc.Later && !sc.All:
		summary += ", and later ones sent with the same text"
	case sc.All && !sc.Later:
		summary += ", but not later ones"
	}
	return res, summary, nil
}
