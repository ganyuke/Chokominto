package store

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"chokominto/internal/names"
)

// Merge suggestions: pairs of artists, recordings or albums that look like
// the same thing, found in the background and answered in Review. Nothing is
// ever merged without the owner. Dismissed pairs are never suggested again.
// See docs/architecture.md, "Merge suggestions".

// Reasons, shown in Review, strongest first.
const (
	ReasonMBID     = "Same MBID"
	ReasonKey      = "Same name, different capitals, width or spacing"
	ReasonRomaji   = "One name is the romaji of the other"
	ReasonBrackets = "Same title apart from the part in brackets"
	ReasonCover    = "Same title, different artists"
	ReasonAlbum    = "Same album name, different artists"
	ReasonGuess    = "Possibly the reading of the Japanese name"
)

var reasonScore = map[string]float64{
	ReasonMBID: 1, ReasonKey: 0.9, ReasonRomaji: 0.8, ReasonBrackets: 0.7, ReasonAlbum: 0.6, ReasonCover: 0.4, ReasonGuess: 0.3,
}

// QueueSuggestTx asks for suggestions to be looked for again in a minute,
// after a burst of new artists, songs and albums has settled.
func QueueSuggestTx(ctx context.Context, tx *sql.Tx, userID int64) error {
	return EnqueueTx(ctx, tx, "suggest", fmt.Sprint("user:", userID), "", unix()+60)
}

// SuggestJob handles a "suggest" job with key "user:<id>".
func (db *DB) SuggestJob(ctx context.Context, j Job) error {
	idText, ok := strings.CutPrefix(j.Key, "user:")
	if !ok {
		return fmt.Errorf("suggest job with key %q", j.Key)
	}
	userID, err := strconv.ParseInt(idText, 10, 64)
	if err != nil {
		return err
	}
	return db.FindSuggestions(ctx, userID)
}

type aliasRow struct {
	owner                      int64
	name                       string
	lang, match, romaji, guess string
	stripped                   string // match key without a bracketed or dashed tail
}

type pair struct {
	kind string
	a, b int64
}

// FindSuggestions looks through all of a user's names for likely pairs and
// records new ones.
func (db *DB) FindSuggestions(ctx context.Context, userID int64) error {
	found := map[pair]string{}
	add := func(kind string, a, b int64, reason string) {
		if a == b {
			return
		}
		p := pair{kind, min(a, b), max(a, b)}
		if cur, ok := found[p]; !ok || reasonScore[reason] > reasonScore[cur] {
			found[p] = reason
		}
	}

	// Artists: any two names that match.
	artists, err := db.aliasRows(ctx, `SELECT a.artist_id, a.lang, a.match_key, coalesce(a.romaji_key, ''), coalesce(a.guess_key, ''), a.name
		FROM artist_aliases a JOIN artists e ON e.id = a.artist_id WHERE e.user_id = ? AND e.merged_into IS NULL`, userID)
	if err != nil {
		return err
	}
	namePairs(artists, func(a, b int64, reason string) {
		if reason != ReasonBrackets {
			add("artist", a, b, reason)
		}
	})

	// Recordings: names of their songs. The same title by the same artist is
	// a likely duplicate, by another artist a possible cover. Each song is
	// represented by its first recording.
	songs, err := db.aliasRows(ctx, `SELECT a.song_id, a.lang, a.match_key, coalesce(a.romaji_key, ''), coalesce(a.guess_key, ''), a.name
		FROM song_aliases a JOIN songs e ON e.id = a.song_id WHERE e.user_id = ? AND e.merged_into IS NULL`, userID)
	if err != nil {
		return err
	}
	first, err := db.idMap(ctx, `SELECT song_id, min(id) FROM recordings WHERE user_id = ? AND merged_into IS NULL GROUP BY song_id`, userID)
	if err != nil {
		return err
	}
	songArtists, err := db.idSets(ctx, `SELECT DISTINCT r.song_id, c.artist_id FROM recordings r
		JOIN recording_credits c ON c.recording_id = r.id AND c.role = 'main'
		WHERE r.user_id = ? AND r.merged_into IS NULL`, userID)
	if err != nil {
		return err
	}
	namePairs(songs, func(a, b int64, reason string) {
		ra, rb := first[a], first[b]
		if ra == 0 || rb == 0 {
			return
		}
		if !overlaps(songArtists[a], songArtists[b]) {
			if reason != ReasonKey {
				return
			}
			reason = ReasonCover
		}
		add("recording", ra, rb, reason)
	})

	// Albums: matching names and at least one artist in common, since many
	// albums share generic names.
	releases, err := db.aliasRows(ctx, `SELECT a.release_id, a.lang, a.match_key, coalesce(a.romaji_key, ''), coalesce(a.guess_key, ''), a.name
		FROM release_aliases a JOIN releases e ON e.id = a.release_id WHERE e.user_id = ? AND e.merged_into IS NULL`, userID)
	if err != nil {
		return err
	}
	releaseArtists, err := db.idSets(ctx, `SELECT c.release_id, c.artist_id FROM release_credits c
		JOIN releases e ON e.id = c.release_id WHERE e.user_id = ? AND e.merged_into IS NULL`, userID)
	if err != nil {
		return err
	}
	// A soundtrack split by composer: the same specific name, no artist
	// in common.
	releaseName := map[int64]string{}
	for _, r := range releases {
		releaseName[r.owner] = r.name
	}
	namePairs(releases, func(a, b int64, reason string) {
		switch {
		case overlaps(releaseArtists[a], releaseArtists[b]):
			add("release", a, b, reason)
		case reason == ReasonKey && names.CompilationName(releaseName[a]):
			add("release", a, b, ReasonAlbum)
		}
	})

	// The same MBID, set by hand or from MusicBrainz.
	for kind, table := range map[string]string{"artist": "artists", "recording": "recordings", "release": "releases"} {
		rows, err := db.r.QueryContext(ctx, `SELECT a.id, b.id FROM `+table+` a JOIN `+table+` b ON b.mbid = a.mbid AND b.id > a.id
			AND b.user_id = a.user_id AND b.merged_into IS NULL WHERE a.user_id = ? AND a.merged_into IS NULL AND a.mbid IS NOT NULL`, userID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var a, b int64
			if err := rows.Scan(&a, &b); err != nil {
				rows.Close()
				return err
			}
			add(kind, a, b, ReasonMBID)
		}
		rows.Close()
	}

	return db.Write(ctx, func(tx *sql.Tx) error {
		now := unix()
		for p, reason := range found {
			// A stronger reason replaces a weaker one on an open pair.
			// Answered pairs stay answered.
			if _, err := tx.ExecContext(ctx, `INSERT INTO suggestions (user_id, kind, a_id, b_id, reason, score, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (kind, a_id, b_id) DO UPDATE SET reason = excluded.reason, score = excluded.score
				WHERE status = 'open' AND excluded.score > score`,
				userID, p.kind, p.a, p.b, reason, reasonScore[reason], now); err != nil {
				return err
			}
		}
		return nil
	})
}

// A bracketed or dash-separated tail: "アイドル (TV size)", "アイドル - Idol".
var titleTail = regexp.MustCompile(`^(.+?)\s*(?:[(\[（［【〔].*|\s[-‐–—~〜]\s.*)$`)

func (db *DB) aliasRows(ctx context.Context, q string, userID int64) ([]aliasRow, error) {
	rows, err := db.r.QueryContext(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []aliasRow
	for rows.Next() {
		var a aliasRow
		var name string
		if err := rows.Scan(&a.owner, &a.lang, &a.match, &a.romaji, &a.guess, &name); err != nil {
			return nil, err
		}
		a.name = name
		if m := titleTail.FindStringSubmatch(name); m != nil {
			a.stripped = names.MatchKey(m[1])
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// namePairs calls fn for each pair of different owners whose names match,
// with the reason.
func namePairs(rows []aliasRow, fn func(a, b int64, reason string)) {
	byKey := map[string][]int64{}
	latin := map[string][]int64{} // Latin names, long vowels folded
	for _, r := range rows {
		if r.match == "" {
			continue
		}
		byKey[r.match] = append(byKey[r.match], r.owner)
		if r.lang != "original" {
			latin[names.FoldLongVowels(r.match)] = append(latin[names.FoldLongVowels(r.match)], r.owner)
		}
	}
	each := func(owners []int64, owner int64, reason string) {
		for _, o := range owners {
			if o != owner {
				fn(owner, o, reason)
			}
		}
	}
	for _, r := range rows {
		each(byKey[r.match], r.owner, ReasonKey)
		if r.romaji != "" {
			each(latin[r.romaji], r.owner, ReasonRomaji)
		}
		if r.guess != "" {
			each(latin[r.guess], r.owner, ReasonGuess)
		}
		if r.stripped != "" {
			each(byKey[r.stripped], r.owner, ReasonBrackets)
		}
	}
}

func overlaps(a, b map[int64]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}

func (db *DB) idMap(ctx context.Context, q string, args ...any) (map[int64]int64, error) {
	rows, err := db.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]int64{}
	for rows.Next() {
		var k, v int64
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

func (db *DB) idSets(ctx context.Context, q string, args ...any) (map[int64]map[int64]bool, error) {
	rows, err := db.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]map[int64]bool{}
	for rows.Next() {
		var k, v int64
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		if m[k] == nil {
			m[k] = map[int64]bool{}
		}
		m[k][v] = true
	}
	return m, rows.Err()
}
