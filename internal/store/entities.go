package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"chokominto/internal/names"
)

// Transaction-level building blocks for resolving received text into
// artists, songs, recordings and releases. internal/resolve decides what
// to call; these only read and write.

type Source struct {
	ID          int64
	UserID      int64
	Artist      string
	Title       string
	Album       string
	AlbumArtist string
	RecordingID sql.NullInt64
	ReleaseID   sql.NullInt64
}

func SourceTx(ctx context.Context, tx *sql.Tx, id int64) (Source, error) {
	var s Source
	err := tx.QueryRowContext(ctx,
		`SELECT id, user_id, artist_text, title_text, album_text, album_artist_text, recording_id, release_id FROM sources WHERE id = ?`, id).
		Scan(&s.ID, &s.UserID, &s.Artist, &s.Title, &s.Album, &s.AlbumArtist, &s.RecordingID, &s.ReleaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrNotFound
	}
	return s, err
}

type Rule struct {
	ID        int64
	Kind      string // clean, split, cv or link
	Field     string // clean and split: artist, title or album
	MatchMode string
	Pattern   string
	// Which received text a clean or link rule applies to. nil = any.
	ArtistMatch, TitleMatch, AlbumMatch *string
	RecordingID, ReleaseID              int64 // link: where it goes, 0 = none
	Priority                            int64
	Enabled                             bool
}

const ruleCols = `id, kind, coalesce(field, ''), match_mode, coalesce(pattern, ''), artist_match, title_match, album_match,
	coalesce(recording_id, 0), coalesce(release_id, 0), priority, enabled`

func scanRule(sc interface{ Scan(...any) error }) (Rule, error) {
	var r Rule
	var a, t, al sql.NullString
	err := sc.Scan(&r.ID, &r.Kind, &r.Field, &r.MatchMode, &r.Pattern, &a, &t, &al, &r.RecordingID, &r.ReleaseID, &r.Priority, &r.Enabled)
	for _, x := range []struct {
		ns  sql.NullString
		dst **string
	}{{a, &r.ArtistMatch}, {t, &r.TitleMatch}, {al, &r.AlbumMatch}} {
		if x.ns.Valid {
			v := x.ns.String
			*x.dst = &v
		}
	}
	return r, err
}

// RulesTx returns a user's enabled rules in the order they apply.
func RulesTx(ctx context.Context, tx *sql.Tx, userID int64) ([]Rule, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+ruleCols+` FROM rules WHERE user_id = ? AND enabled = 1 ORDER BY priority DESC, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rs []Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		rs = append(rs, r)
	}
	return rs, rows.Err()
}

// Names

// aliasLang guesses the kind of a new alias from its text. Latin text that
// is the romanization of one of the entity's kana names is romaji. Other
// Latin text is taken as English. The owner can correct it later.
func aliasLang(ctx context.Context, tx *sql.Tx, table, owner string, ownerID int64, name string) (string, error) {
	if names.IsJapanese(name) {
		return "original", nil
	}
	key := names.FoldLongVowels(names.MatchKey(name))
	var n int
	err := tx.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s = ? AND romaji_key = ?`, table, owner), ownerID, key).Scan(&n)
	if n > 0 {
		return "romaji", err
	}
	return "en", err
}

var aliasTables = map[string]struct{ entity, owner string }{
	"artist_aliases":  {"artists", "artist_id"},
	"song_aliases":    {"songs", "song_id"},
	"release_aliases": {"releases", "release_id"},
}

// AddAliasTx adds a name to an artist, song or release (ignoring one it
// already has) and refreshes the entity's display names.
func AddAliasTx(ctx context.Context, tx *sql.Tx, table string, ownerID int64, name string) error {
	t, ok := aliasTables[table]
	if !ok {
		return fmt.Errorf("no alias table %q", table)
	}
	lang, err := aliasLang(ctx, tx, table, t.owner, ownerID, name)
	if err != nil {
		return err
	}
	match, romaji, guess := aliasKeys(name)
	_, err = tx.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO %s (%s, name, lang, match_key, romaji_key, guess_key) VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, table, t.owner),
		ownerID, name, lang, match, romaji, guess)
	if err != nil {
		return err
	}
	return refreshNamesTx(ctx, tx, table, ownerID)
}

// refreshNamesTx recomputes an entity's cached primary and other names: the
// pinned alias first, then English, romaji, original script.
func refreshNamesTx(ctx context.Context, tx *sql.Tx, table string, ownerID int64) error {
	t := aliasTables[table]
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(
		`SELECT a.name FROM %s a JOIN %s e ON e.id = a.%s WHERE a.%s = ?
		 ORDER BY (a.id = e.pinned_alias) DESC,
		          CASE a.lang WHEN 'en' THEN 0 WHEN 'romaji' THEN 1 ELSE 2 END, a.id`,
		table, t.entity, t.owner, t.owner), ownerID)
	if err != nil {
		return err
	}
	var all []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		all = append(all, n)
	}
	rows.Close()
	if len(all) == 0 {
		return nil
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET name = ?, other_names = ? WHERE id = ?`, t.entity),
		all[0], strings.Join(all[1:], " "), ownerID)
	return err
}

// Artists

// FindArtistTx finds a user's artist by a name's match key. When several
// share it, the oldest wins, so results are stable.
func FindArtistTx(ctx context.Context, tx *sql.Tx, userID int64, name string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		`SELECT a.id FROM artist_aliases al JOIN artists a ON a.id = al.artist_id
		 WHERE al.match_key = ? AND a.user_id = ? AND a.merged_into IS NULL ORDER BY a.id LIMIT 1`,
		names.MatchKey(name), userID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

func CreateArtistTx(ctx context.Context, tx *sql.Tx, userID int64, name string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		`INSERT INTO artists (user_id, name, created_at) VALUES (?, ?, ?) RETURNING id`, userID, name, unix()).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, AddAliasTx(ctx, tx, "artist_aliases", id, name)
}

// CountsForTx lists who an artist's listens also count for.
func CountsForTx(ctx context.Context, tx *sql.Tx, artistID int64) ([]int64, error) {
	return ids(ctx, tx, `SELECT target_id FROM artist_counts_for WHERE artist_id = ? ORDER BY target_id`, artistID)
}

// AddCountsForTx makes from's listens also count for to. It does nothing if
// the link exists or would make a loop.
func AddCountsForTx(ctx context.Context, tx *sql.Tx, from, to int64, note string) error {
	if from == to {
		return nil
	}
	var loops int
	err := tx.QueryRowContext(ctx,
		`WITH RECURSIVE reach(id) AS (
		   SELECT target_id FROM artist_counts_for WHERE artist_id = ?
		   UNION SELECT c.target_id FROM artist_counts_for c JOIN reach r ON c.artist_id = r.id)
		 SELECT count(*) FROM reach WHERE id = ?`, to, from).Scan(&loops)
	if err != nil || loops > 0 {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO artist_counts_for (artist_id, target_id, note) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, from, to, note)
	return err
}

// Labels

func LabelIDTx(ctx context.Context, tx *sql.Tx, userID int64, name string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM labels WHERE user_id = ? AND name = ?`, userID, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

func AddLabelTx(ctx context.Context, tx *sql.Tx, labelID int64, entityType string, entityID int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO entity_labels (label_id, entity_type, entity_id) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
		labelID, entityType, entityID)
	return err
}

// Songs and recordings

type Credit struct {
	ArtistID int64
	Role     string // "main" or "featured"
}

// FindRecordingsTx finds recordings with this title credited (as main) to
// any of the given artists.
func FindRecordingsTx(ctx context.Context, tx *sql.Tx, userID int64, title string, mainArtists []int64) ([]int64, error) {
	if len(mainArtists) == 0 {
		return nil, nil
	}
	args := []any{userID, names.MatchKey(title)}
	for _, a := range mainArtists {
		args = append(args, a)
	}
	return ids(ctx, tx, `SELECT DISTINCT r.id FROM song_aliases sa
		JOIN songs s ON s.id = sa.song_id AND s.user_id = ? AND s.merged_into IS NULL
		JOIN recordings r ON r.song_id = s.id AND r.merged_into IS NULL
		JOIN recording_credits rc ON rc.recording_id = r.id AND rc.role = 'main'
		WHERE sa.match_key = ? AND rc.artist_id IN (`+placeholders(len(mainArtists))+`) ORDER BY r.id`, args...)
}

// OnReleaseTx keeps the recordings that are on a release with this title.
func OnReleaseTx(ctx context.Context, tx *sql.Tx, recordings []int64, album string) ([]int64, error) {
	if len(recordings) == 0 {
		return nil, nil
	}
	args := []any{names.MatchKey(album)}
	for _, r := range recordings {
		args = append(args, r)
	}
	return ids(ctx, tx, `SELECT DISTINCT rt.recording_id FROM release_tracks rt
		JOIN releases rl ON rl.id = rt.release_id AND rl.merged_into IS NULL
		JOIN release_aliases ra ON ra.release_id = rl.id AND ra.match_key = ?
		WHERE rt.recording_id IN (`+placeholders(len(recordings))+`) ORDER BY rt.recording_id`, args...)
}

// CreateRecordingTx creates a new song with one recording and its credits.
func CreateRecordingTx(ctx context.Context, tx *sql.Tx, userID int64, title string, credits []Credit) (int64, error) {
	return CreateSongVersionTx(ctx, tx, userID, title, "", false, credits)
}

// CreateSongVersionTx creates a new song with one recording of the given
// version. solo marks it to rank on its own.
func CreateSongVersionTx(ctx context.Context, tx *sql.Tx, userID int64, title, version string, solo bool, credits []Credit) (int64, error) {
	var songID int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO songs (user_id, name, created_at) VALUES (?, ?, ?) RETURNING id`, userID, title, unix()).Scan(&songID); err != nil {
		return 0, err
	}
	if err := AddAliasTx(ctx, tx, "song_aliases", songID, title); err != nil {
		return 0, err
	}
	return CreateVersionTx(ctx, tx, userID, songID, version, solo, credits)
}

// CreateVersionTx adds a recording to an existing song: another version by
// the same artists, like an instrumental.
func CreateVersionTx(ctx context.Context, tx *sql.Tx, userID, songID int64, version string, solo bool, credits []Credit) (int64, error) {
	var recID int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO recordings (user_id, song_id, version, rank_alone, created_at) VALUES (?, ?, ?, ?, ?) RETURNING id`,
		userID, songID, version, boolInt(solo), unix()).Scan(&recID); err != nil {
		return 0, err
	}
	for i, c := range credits {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO recording_credits (recording_id, artist_id, role, position) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
			recID, c.ArtistID, c.Role, i); err != nil {
			return 0, err
		}
	}
	return recID, nil
}

// RecordingVersion is where a recording belongs, for picking the right
// version.
type RecordingVersion struct {
	ID, SongID int64
	Version    string
}

func RecordingVersionsTx(ctx context.Context, tx *sql.Tx, ids []int64) ([]RecordingVersion, error) {
	var out []RecordingVersion
	for _, id := range ids {
		var v RecordingVersion
		if err := tx.QueryRowContext(ctx, `SELECT id, song_id, version FROM recordings WHERE id = ?`, id).Scan(&v.ID, &v.SongID, &v.Version); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// SongVersionsTx lists a song's recordings credited (as main) to any of
// the given artists.
func SongVersionsTx(ctx context.Context, tx *sql.Tx, songID int64, mainArtists []int64) ([]RecordingVersion, error) {
	if len(mainArtists) == 0 {
		return nil, nil
	}
	args := []any{songID}
	for _, a := range mainArtists {
		args = append(args, a)
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT r.id, r.song_id, r.version FROM recordings r
		JOIN recording_credits rc ON rc.recording_id = r.id AND rc.role = 'main'
		WHERE r.song_id = ? AND r.merged_into IS NULL AND rc.artist_id IN (`+placeholders(len(mainArtists))+`) ORDER BY r.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecordingVersion
	for rows.Next() {
		var v RecordingVersion
		if err := rows.Scan(&v.ID, &v.SongID, &v.Version); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// AddMemberTx makes member part of group, and marks the group as a group.
// It does nothing if the membership exists or would make a loop.
func AddMemberTx(ctx context.Context, tx *sql.Tx, group, member int64) error {
	if group == member {
		return nil
	}
	if loop, err := WouldLoop(ctx, tx, "group_members", group, member); err != nil || loop {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO group_members (group_id, member_id) VALUES (?, ?) ON CONFLICT DO NOTHING`, group, member); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE artists SET kind = 'group' WHERE id = ? AND kind = 'other'`, group)
	return err
}

// CreditedIDsTx lists a recording's main and featured artists.
func CreditedIDsTx(ctx context.Context, tx *sql.Tx, recordingID int64) ([]int64, error) {
	return ids(ctx, tx, `SELECT DISTINCT artist_id FROM recording_credits WHERE recording_id = ? AND role IN ('main', 'featured') ORDER BY artist_id`, recordingID)
}

// MainArtistsTx lists a recording's main artists.
func MainArtistsTx(ctx context.Context, tx *sql.Tx, recordingID int64) ([]int64, error) {
	return ids(ctx, tx, `SELECT artist_id FROM recording_credits WHERE recording_id = ? AND role = 'main' ORDER BY position`, recordingID)
}

// Releases

// FindReleaseTx finds a release with this title credited to any of the
// given artists.
func FindReleaseTx(ctx context.Context, tx *sql.Tx, userID int64, album string, artists []int64) (int64, error) {
	if len(artists) == 0 {
		return 0, ErrNotFound
	}
	args := []any{userID, names.MatchKey(album)}
	for _, a := range artists {
		args = append(args, a)
	}
	found, err := ids(ctx, tx, `SELECT DISTINCT rl.id FROM release_aliases ra
		JOIN releases rl ON rl.id = ra.release_id AND rl.user_id = ? AND rl.merged_into IS NULL
		JOIN release_credits rc ON rc.release_id = rl.id
		WHERE ra.match_key = ? AND rc.artist_id IN (`+placeholders(len(artists))+`) ORDER BY rl.id LIMIT 1`, args...)
	if err != nil {
		return 0, err
	}
	if len(found) == 0 {
		return 0, ErrNotFound
	}
	return found[0], nil
}

// FindReleaseByNameTx finds the user's oldest album with this name,
// whoever it's credited to.
func FindReleaseByNameTx(ctx context.Context, tx *sql.Tx, userID int64, album string) (int64, error) {
	found, err := ids(ctx, tx, `SELECT DISTINCT rl.id FROM release_aliases ra
		JOIN releases rl ON rl.id = ra.release_id AND rl.user_id = ? AND rl.merged_into IS NULL
		WHERE ra.match_key = ? ORDER BY rl.id LIMIT 1`, userID, names.MatchKey(album))
	if err != nil {
		return 0, err
	}
	if len(found) == 0 {
		return 0, ErrNotFound
	}
	return found[0], nil
}

// AddReleaseCreditsTx credits an album to more artists, after the ones it
// has.
func AddReleaseCreditsTx(ctx context.Context, tx *sql.Tx, releaseID int64, artists []int64) error {
	for _, a := range artists {
		if _, err := tx.ExecContext(ctx, `INSERT INTO release_credits (release_id, artist_id, position)
			VALUES (?, ?, (SELECT coalesce(max(position), -1) + 1 FROM release_credits WHERE release_id = ?)) ON CONFLICT DO NOTHING`,
			releaseID, a, releaseID); err != nil {
			return err
		}
	}
	return nil
}

func CreateReleaseTx(ctx context.Context, tx *sql.Tx, userID int64, title string, artists []int64) (int64, error) {
	var id int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO releases (user_id, name, created_at) VALUES (?, ?, ?) RETURNING id`, userID, title, unix()).Scan(&id); err != nil {
		return 0, err
	}
	if err := AddAliasTx(ctx, tx, "release_aliases", id, title); err != nil {
		return 0, err
	}
	for i, a := range artists {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO release_credits (release_id, artist_id, position) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, id, a, i); err != nil {
			return 0, err
		}
	}
	return id, nil
}

func AddReleaseTrackTx(ctx context.Context, tx *sql.Tx, releaseID, recordingID int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO release_tracks (release_id, recording_id) VALUES (?, ?) ON CONFLICT DO NOTHING`, releaseID, recordingID)
	return err
}

// Links

// LinkSourceTx points received text at a recording (and release) through
// the edit log, marked automatic. Its listens follow.
func LinkSourceTx(ctx context.Context, tx *sql.Tx, s Source, recordingID, releaseID int64) error {
	return LinkSourceByRuleTx(ctx, tx, s, recordingID, releaseID, 0)
}

// LinkSourceByRuleTx is LinkSourceTx for a link a rule decided, so undoing
// the rule can find what it linked.
func LinkSourceByRuleTx(ctx context.Context, tx *sql.Tx, s Source, recordingID, releaseID, ruleID int64) error {
	var rel any
	if releaseID != 0 {
		rel = releaseID
	}
	summary := fmt.Sprintf("Linked %s by %s", orDash(s.Title), orDash(s.Artist))
	_, err := ApplyEditTx(ctx, tx, s.UserID, EditMeta{Kind: "link", Summary: summary, Automatic: true, RuleID: ruleID}, func(editID int64) []Change {
		return []Change{{
			Table:  "sources",
			ID:     s.ID,
			Before: map[string]any{"recording_id": nullable(s.RecordingID), "release_id": nullable(s.ReleaseID), "linked_by": currentLinkedBy(ctx, tx, s.ID)},
			After:  map[string]any{"recording_id": recordingID, "release_id": rel, "linked_by": editID},
		}}
	})
	return err
}

// UnlinkSourceTx takes an automatic link off received text, as an automatic
// edit, for text that reads as having no artist or title after all.
func UnlinkSourceTx(ctx context.Context, tx *sql.Tx, s Source) error {
	summary := fmt.Sprintf("Unlinked %s by %s", orDash(s.Title), orDash(s.Artist))
	_, err := ApplyEditTx(ctx, tx, s.UserID, EditMeta{Kind: "link", Summary: summary, Automatic: true}, func(editID int64) []Change {
		return []Change{{
			Table:  "sources",
			ID:     s.ID,
			Before: map[string]any{"recording_id": nullable(s.RecordingID), "release_id": nullable(s.ReleaseID), "linked_by": currentLinkedBy(ctx, tx, s.ID)},
			After:  map[string]any{"recording_id": nil, "release_id": nil, "linked_by": editID},
		}}
	})
	return err
}

func nullable(n sql.NullInt64) any {
	if n.Valid {
		return n.Int64
	}
	return nil
}

func currentLinkedBy(ctx context.Context, tx *sql.Tx, sourceID int64) any {
	var n sql.NullInt64
	tx.QueryRowContext(ctx, `SELECT linked_by FROM sources WHERE id = ?`, sourceID).Scan(&n)
	return nullable(n)
}

// Credit expansion

// RebuildRecordingArtistsTx recomputes who gets credit for listens of a
// recording. Credited artists get "credited", and so does everyone their
// listens also count for. Group members get "group", and so does everyone
// they count for. A credit override on the recording (or else on one of its
// releases) replaces an artist's members and counts-for targets. An artist
// reached both ways is "credited", so nobody is counted twice.
func RebuildRecordingArtistsTx(ctx context.Context, tx *sql.Tx, recordingID int64) error {
	credited, err := ids(ctx, tx,
		`SELECT artist_id FROM recording_credits WHERE recording_id = ? AND role IN ('main', 'featured') ORDER BY position`, recordingID)
	if err != nil {
		return err
	}
	releases, err := ids(ctx, tx, `SELECT release_id FROM release_tracks WHERE recording_id = ? ORDER BY release_id`, recordingID)
	if err != nil {
		return err
	}

	type item struct {
		id  int64
		via string
	}
	var credQ, groupQ []item
	for _, a := range credited {
		credQ = append(credQ, item{a, "credited"})
	}
	seen := map[int64]string{}
	var order []int64
	for len(credQ)+len(groupQ) > 0 {
		var it item
		if len(credQ) > 0 {
			it, credQ = credQ[0], credQ[1:]
		} else {
			it, groupQ = groupQ[0], groupQ[1:]
		}
		if v, ok := seen[it.id]; ok && (v == "credited" || v == it.via) {
			continue
		}
		if _, ok := seen[it.id]; !ok {
			order = append(order, it.id)
		}
		seen[it.id] = it.via

		push := func(id int64, via string) {
			if via == "credited" {
				credQ = append(credQ, item{id, via})
			} else {
				groupQ = append(groupQ, item{id, via})
			}
		}
		override, err := overrideTargets(ctx, tx, recordingID, releases, it.id)
		if err != nil {
			return err
		}
		if override != nil {
			via := it.via
			if isGroup, err := isGroupTx(ctx, tx, it.id); err != nil {
				return err
			} else if isGroup {
				via = "group"
			}
			for _, t := range override {
				push(t, via)
			}
			continue
		}
		members, err := ids(ctx, tx, `SELECT member_id FROM group_members WHERE group_id = ? ORDER BY member_id`, it.id)
		if err != nil {
			return err
		}
		for _, m := range members {
			push(m, "group")
		}
		targets, err := CountsForTx(ctx, tx, it.id)
		if err != nil {
			return err
		}
		for _, t := range targets {
			push(t, it.via)
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM recording_artists WHERE recording_id = ?`, recordingID); err != nil {
		return err
	}
	for _, a := range order {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO recording_artists (recording_id, artist_id, via) VALUES (?, ?, ?)`, recordingID, a, seen[a]); err != nil {
			return err
		}
	}
	return nil
}

// overrideTargets returns the override for artistID on this recording, or
// else on the first of its releases that has one, or nil for none.
func overrideTargets(ctx context.Context, tx *sql.Tx, recordingID int64, releases []int64, artistID int64) ([]int64, error) {
	t, err := ids(ctx, tx, `SELECT to_id FROM credit_overrides WHERE scope = 'recording' AND scope_id = ? AND from_id = ? ORDER BY to_id`,
		recordingID, artistID)
	if err != nil || len(t) > 0 {
		return t, err
	}
	for _, rel := range releases {
		t, err := ids(ctx, tx, `SELECT to_id FROM credit_overrides WHERE scope = 'release' AND scope_id = ? AND from_id = ? ORDER BY to_id`,
			rel, artistID)
		if err != nil || len(t) > 0 {
			return t, err
		}
	}
	return nil, nil
}

func isGroupTx(ctx context.Context, tx *sql.Tx, artistID int64) (bool, error) {
	var kind string
	err := tx.QueryRowContext(ctx, `SELECT kind FROM artists WHERE id = ?`, artistID).Scan(&kind)
	if err != nil {
		return false, err
	}
	if kind == "group" {
		return true, nil
	}
	var members int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM group_members WHERE group_id = ?`, artistID).Scan(&members)
	return members > 0, err
}

func ids(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
