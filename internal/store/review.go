package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"chokominto/internal/names"
)

// Review: open merge suggestions and received text that isn't linked,
// biggest first, with answers for many rows at once. Every bulk answer is
// one edit. See docs/architecture.md, "Review and fixing".

// Suggestion is an open pair, with how much listening it covers.
type Suggestion struct {
	ID      int64
	Kind    string
	A, B    int64
	Reason  string
	Weak    bool // a guess, shown as "Possibly"
	Listens int  // of both together
}

// OpenSuggestions returns up to limit open suggestions whose two sides are
// both still there, biggest first, and how many there are in all.
func (db *DB) OpenSuggestions(ctx context.Context, userID int64, limit int) ([]Suggestion, int, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT s.id, s.kind, s.a_id, s.b_id, s.reason, s.score FROM suggestions s
		WHERE s.user_id = ? AND s.status = 'open'
		AND NOT EXISTS (SELECT 1 FROM artists WHERE s.kind = 'artist' AND id IN (s.a_id, s.b_id) AND merged_into IS NOT NULL)
		AND NOT EXISTS (SELECT 1 FROM recordings WHERE s.kind = 'recording' AND id IN (s.a_id, s.b_id) AND merged_into IS NOT NULL)
		AND NOT EXISTS (SELECT 1 FROM releases WHERE s.kind = 'release' AND id IN (s.a_id, s.b_id) AND merged_into IS NOT NULL)`, userID)
	if err != nil {
		return nil, 0, err
	}
	type scored struct {
		Suggestion
		score float64
	}
	var all []scored
	for rows.Next() {
		var s scored
		if err := rows.Scan(&s.ID, &s.Kind, &s.A, &s.B, &s.Reason, &s.score); err != nil {
			rows.Close()
			return nil, 0, err
		}
		s.Weak = s.Reason == ReasonGuess
		all = append(all, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	ids := map[string][]int64{}
	for _, s := range all {
		ids[s.Kind] = append(ids[s.Kind], s.A, s.B)
	}
	counts, err := db.listenCountsOf(ctx, userID, ids)
	if err != nil {
		return nil, 0, err
	}
	for i := range all {
		c := counts[all[i].Kind]
		all[i].Listens = c[all[i].A] + c[all[i].B]
	}
	slices.SortStableFunc(all, func(x, y scored) int {
		if x.Listens != y.Listens {
			return y.Listens - x.Listens
		}
		if x.score != y.score {
			if x.score > y.score {
				return -1
			}
			return 1
		}
		return int(x.ID - y.ID)
	})
	out := make([]Suggestion, 0, min(limit, len(all)))
	for _, s := range all[:min(limit, len(all))] {
		out = append(out, s.Suggestion)
	}
	return out, len(all), nil
}

// listenCountsOf returns all-time listens of the given artists,
// recordings and releases, by kind.
func (db *DB) listenCountsOf(ctx context.Context, userID int64, ids map[string][]int64) (map[string]map[int64]int, error) {
	out := map[string]map[int64]int{}
	for kind, q := range map[string]string{
		"recording": `SELECT recording_id, sum(n) FROM listen_totals WHERE user_id = ? AND recording_id IN (SELECT value FROM json_each(?)) GROUP BY recording_id`,
		"release":   `SELECT release_id, sum(n) FROM listen_totals WHERE user_id = ? AND release_id IN (SELECT value FROM json_each(?)) GROUP BY release_id`,
		// CROSS JOIN keeps this order: the few artists asked about, then
		// their recordings, then those recordings' totals.
		"artist": `SELECT ra.artist_id, sum(t.n) FROM (SELECT DISTINCT value AS id FROM json_each(?2)) j
			CROSS JOIN recording_artists ra ON ra.artist_id = j.id
			CROSS JOIN listen_totals t ON t.user_id = ?1 AND t.recording_id = ra.recording_id
			GROUP BY ra.artist_id`,
	} {
		list, _ := json.Marshal(ids[kind])
		m, err := db.idMap(ctx, q, userID, string(list))
		if err != nil {
			return nil, err
		}
		out[kind] = map[int64]int{}
		for k, v := range m {
			out[kind][k] = int(v)
		}
	}
	return out, nil
}

// Answers to a suggestion.
const (
	AnswerSame      = "same"      // merge the two
	AnswerVersion   = "version"   // recordings: same song, different version
	AnswerDifferent = "different" // never suggest again
)

var ErrAnswer = errors.New("that answer doesn't fit")

// AnswerSuggestions answers several suggestions the same way, as one edit.
// The side with more listens is kept. Pairs that earlier answers in the
// same batch already joined are skipped. It returns the edit and how many
// were answered.
func (db *DB) AnswerSuggestions(ctx context.Context, userID int64, ids []int64, answer string) (int64, int, error) {
	if answer != AnswerSame && answer != AnswerVersion && answer != AnswerDifferent {
		return 0, 0, ErrAnswer
	}
	var editID int64
	var n int
	err := db.Write(ctx, func(tx *sql.Tx) error {
		// The counts only choose which side is kept, so the reader's view
		// is good enough.
		counts, err := db.listenCountsOf(ctx, userID, suggestionIDs(ctx, tx, ids))
		if err != nil {
			return err
		}
		e, err := OpenEditTx(ctx, tx, userID, EditMeta{Kind: "review"})
		if err != nil {
			return err
		}
		editID = e.ID
		for _, id := range slices.Compact(slices.Sorted(slices.Values(ids))) {
			var s Suggestion
			var status string
			err := tx.QueryRowContext(ctx, `SELECT id, kind, a_id, b_id, status FROM suggestions WHERE id = ? AND user_id = ?`, id, userID).
				Scan(&s.ID, &s.Kind, &s.A, &s.B, &status)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if status != "open" || mergedAway(ctx, tx, s.Kind, s.A) || mergedAway(ctx, tx, s.Kind, s.B) {
				continue
			}
			if answer == AnswerVersion && s.Kind != "recording" {
				return ErrAnswer
			}
			winner, loser := s.A, s.B
			if c := counts[s.Kind]; c[s.B] > c[s.A] {
				winner, loser = s.B, s.A
			}
			p := &plan{ctx: ctx, tx: tx, userID: userID}
			switch answer {
			case AnswerSame:
				if _, err := p.mergeInto(s.Kind, loser, winner); err != nil {
					return err
				}
			case AnswerVersion:
				if err := p.joinSongs(loser, winner); err != nil {
					return err
				}
				p.add(Change{Table: "suggestions", ID: s.ID, Before: map[string]any{"status": "open"}, After: map[string]any{"status": "accepted"}})
			case AnswerDifferent:
				p.add(Change{Table: "suggestions", ID: s.ID, Before: map[string]any{"status": "open"}, After: map[string]any{"status": "dismissed"}})
			}
			if err := p.applyTo(e); err != nil {
				return err
			}
			n++
		}
		if n == 0 {
			return ErrNotFound
		}
		summary := map[string]string{
			AnswerSame:      "Merged %s",
			AnswerVersion:   "Marked %s as versions of the same song",
			AnswerDifferent: "Marked %s as different",
		}[answer]
		if err := e.Summarize(fmt.Sprintf(summary, plural(n, "suggested pair"))); err != nil {
			return err
		}
		return e.Close()
	})
	return editID, n, err
}

// suggestionIDs lists the two sides of the given suggestions, by kind.
func suggestionIDs(ctx context.Context, tx *sql.Tx, sugs []int64) map[string][]int64 {
	out := map[string][]int64{}
	for _, id := range sugs {
		var kind string
		var a, b int64
		if tx.QueryRowContext(ctx, `SELECT kind, a_id, b_id FROM suggestions WHERE id = ?`, id).Scan(&kind, &a, &b) == nil {
			out[kind] = append(out[kind], a, b)
		}
	}
	return out
}

func mergedAway(ctx context.Context, tx *sql.Tx, kind string, id int64) bool {
	var merged sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT merged_into FROM `+MergeKinds[kind]+` WHERE id = ?`, id).Scan(&merged)
	return err != nil || merged.Valid
}

// joinSongs puts the loser recording under the winner's song, keeping it as
// its own recording. A song left with nothing else in it is merged into the
// winner's, so its names come along.
func (p *plan) joinSongs(loser, winner int64) error {
	var ls, ws int64
	if err := p.tx.QueryRowContext(p.ctx, `SELECT song_id FROM recordings WHERE id = ?`, loser).Scan(&ls); err != nil {
		return err
	}
	if err := p.tx.QueryRowContext(p.ctx, `SELECT song_id FROM recordings WHERE id = ?`, winner).Scan(&ws); err != nil {
		return err
	}
	if ls == ws {
		return nil
	}
	var others int
	if err := p.tx.QueryRowContext(p.ctx, `SELECT count(*) FROM recordings WHERE song_id = ? AND id <> ? AND merged_into IS NULL`, ls, loser).
		Scan(&others); err != nil {
		return err
	}
	if others == 0 {
		if err := p.mergeSong(ls, ws); err != nil {
			return err
		}
		if err := p.fillBlanks("songs", ls, ws); err != nil {
			return err
		}
		return p.update("songs", ls, map[string]any{"merged_into": ws})
	}
	return p.update("recordings", loser, map[string]any{"song_id": ws, "is_original": int64(0)})
}

// Unlinked text

// UnlinkedSource is received text that isn't linked, or has listens
// stored as incomplete.
type UnlinkedSource struct {
	SourceInfo
	LatestListen int64 // for its Fix link
	Incomplete   bool  // a listen was stored as incomplete
}

// SearchKey is how received text is searched: the match keys of its
// artist, title and album, joined with a unit separator. Migration 0005
// computes the same for older text.
func SearchKey(t SourceText) string {
	return names.MatchKey(t.Artist) + "\x1f" + names.MatchKey(t.Title) + "\x1f" + names.MatchKey(t.Album)
}

// UnlinkedSources lists text with listens that isn't linked to a song, and
// text with listens stored as incomplete, most listens first.
func (db *DB) UnlinkedSources(ctx context.Context, userID int64) ([]UnlinkedSource, error) {
	// Unlinked text, and text with incomplete listens, each found by an
	// index, so this never reads every listen.
	rows, err := db.r.QueryContext(ctx, `WITH picked AS (
		  SELECT id FROM sources WHERE user_id = ?1 AND recording_id IS NULL
		  UNION SELECT l.source_id FROM listens l JOIN sources s ON s.id = l.source_id
		    WHERE l.incomplete = 1 AND l.deleted_by IS NULL AND s.user_id = ?1)
		SELECT s.id, s.user_id, s.artist_text, s.title_text, s.album_text, s.album_artist_text,
		s.recording_id, s.release_id, s.msid, count(l.id), max(l.id), max(l.incomplete)
		FROM picked p JOIN sources s ON s.id = p.id JOIN listens l ON l.source_id = s.id AND l.deleted_by IS NULL
		GROUP BY s.id ORDER BY count(l.id) DESC, s.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnlinkedSource
	for rows.Next() {
		var u UnlinkedSource
		if err := rows.Scan(&u.ID, &u.UserID, &u.Artist, &u.Title, &u.Album, &u.AlbumArtist, &u.RecordingID, &u.ReleaseID, &u.MSID,
			&u.Listens, &u.LatestListen, &u.Incomplete); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ReadTx runs fn in a read-only transaction, for lookups that share code
// with writers.
func (db *DB) ReadTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := db.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(tx)
}

// FindArtistsTx finds artists by name without creating any. Names with no
// artist are left out.
func FindArtistsTx(ctx context.Context, tx *sql.Tx, userID int64, names []string) ([]int64, error) {
	var out []int64
	for _, n := range names {
		if strings.TrimSpace(n) == "" {
			continue
		}
		id, err := FindArtistTx(ctx, tx, userID, n)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

// RecordingReleases lists the albums a recording is on.
func (db *DB) RecordingReleases(ctx context.Context, recordingID int64) ([]Ref, error) {
	return db.refs(ctx, `SELECT rl.id, rl.name, rl.other_names FROM release_tracks rt JOIN releases rl ON rl.id = rt.release_id
		WHERE rt.recording_id = ? AND rl.merged_into IS NULL ORDER BY rl.id`, recordingID)
}

// SearchSources finds received text whose artist, title or album contains
// q, ignoring case, width and spacing, most listens first.
func (db *DB) SearchSources(ctx context.Context, userID int64, q string, limit int) ([]UnlinkedSource, error) {
	key := names.MatchKey(q)
	if key == "" {
		return nil, nil
	}
	// Each hit's listens are counted through the source index, and only the
	// top ones get their other details, so a short query that matches
	// nearly everything stays quick.
	rows, err := db.r.QueryContext(ctx, `WITH hit AS (
		  SELECT id, (SELECT count(*) FROM listens l WHERE l.source_id = s.id AND l.deleted_by IS NULL) AS n
		  FROM sources s WHERE user_id = ?1 AND instr(search_key, ?2) > 0),
		top AS (SELECT id, n FROM hit WHERE n > 0 ORDER BY n DESC, id LIMIT ?3)
		SELECT s.id, s.user_id, s.artist_text, s.title_text, s.album_text, s.album_artist_text,
		s.recording_id, s.release_id, s.msid, t.n,
		(SELECT max(id) FROM listens WHERE source_id = s.id AND deleted_by IS NULL),
		(SELECT max(incomplete) FROM listens WHERE source_id = s.id AND deleted_by IS NULL)
		FROM top t JOIN sources s ON s.id = t.id ORDER BY t.n DESC, s.id`, userID, key, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnlinkedSource
	for rows.Next() {
		var u UnlinkedSource
		if err := rows.Scan(&u.ID, &u.UserID, &u.Artist, &u.Title, &u.Album, &u.AlbumArtist, &u.RecordingID, &u.ReleaseID, &u.MSID,
			&u.Listens, &u.LatestListen, &u.Incomplete); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
