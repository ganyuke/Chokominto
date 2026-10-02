package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// Fixing links by hand: the Fix page, relinking received text, "New song",
// and saving "remember this?" rules. Every change is one edit.

// SourceInfo is received text with its current link.
type SourceInfo struct {
	Source
	MSID          string
	LinkedByOwner bool // linked by hand or by a merge, so reparsing leaves it alone
	Listens       int  // listens with exactly this text, not filled in by SourcesFor
}

// FixListen is everything the Fix page shows about one listen.
type FixListen struct {
	Listen
	Source SourceInfo
	Client string // the scrobbler that sent it, if it said
	Page   string // the page it was playing on, if sent
}

func (db *DB) FixListen(ctx context.Context, userID, listenID int64) (FixListen, error) {
	var f FixListen
	ls, err := db.scanListens(ctx, `SELECT `+listenCols+` FROM listens l JOIN sources s ON s.id = l.source_id
		WHERE l.id = ? AND l.user_id = ? AND l.deleted_by IS NULL`, listenID, userID)
	if err != nil {
		return f, err
	}
	if len(ls) == 0 {
		return f, ErrNotFound
	}
	f.Listen = ls[0]
	var sourceID int64
	if err := db.r.QueryRowContext(ctx, `SELECT source_id FROM listens WHERE id = ?`, listenID).Scan(&sourceID); err != nil {
		return f, err
	}
	if f.Source, err = db.SourceInfo(ctx, userID, sourceID); err != nil {
		return f, err
	}
	var payload sql.NullString
	db.r.QueryRowContext(ctx, `SELECT payload FROM listen_payloads WHERE listen_id = ?`, listenID).Scan(&payload)
	var p struct {
		TrackMetadata struct {
			AdditionalInfo map[string]any `json:"additional_info"`
		} `json:"track_metadata"`
	}
	if json.Unmarshal([]byte(payload.String), &p) == nil {
		info := p.TrackMetadata.AdditionalInfo
		str := func(k string) string { s, _ := info[k].(string); return s }
		f.Client = str("submission_client")
		if svc := str("music_service_name"); svc != "" {
			f.Client = joinNonEmpty(f.Client, svc)
		}
		f.Page = str("origin_url")
	}
	return f, nil
}

func joinNonEmpty(a, b string) string {
	if a == "" {
		return b
	}
	return a + ", " + b
}

func (db *DB) SourceInfo(ctx context.Context, userID, sourceID int64) (SourceInfo, error) {
	var s SourceInfo
	var auto sql.NullBool
	err := db.r.QueryRowContext(ctx, `SELECT s.id, s.user_id, s.artist_text, s.title_text, s.album_text, s.album_artist_text,
		s.recording_id, s.release_id, s.msid, e.automatic,
		(SELECT count(*) FROM listens l WHERE l.source_id = s.id AND l.deleted_by IS NULL)
		FROM sources s LEFT JOIN edits e ON e.id = s.linked_by WHERE s.id = ? AND s.user_id = ?`, sourceID, userID).
		Scan(&s.ID, &s.UserID, &s.Artist, &s.Title, &s.Album, &s.AlbumArtist, &s.RecordingID, &s.ReleaseID, &s.MSID, &auto, &s.Listens)
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrNotFound
	}
	s.LinkedByOwner = auto.Valid && !auto.Bool
	return s, err
}

// SourcesFor lists all of a user's received text, for checking which of it
// a rule would change. Listens is left at 0: counting them for every text
// reads every listen, so ListensWithText counts just the ones a rule hits.
func (db *DB) SourcesFor(ctx context.Context, userID int64) ([]SourceInfo, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT s.id, s.user_id, s.artist_text, s.title_text, s.album_text, s.album_artist_text,
		s.recording_id, s.release_id, s.msid, e.automatic
		FROM sources s LEFT JOIN edits e ON e.id = s.linked_by WHERE s.user_id = ? ORDER BY s.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourceInfo
	for rows.Next() {
		var s SourceInfo
		var auto sql.NullBool
		if err := rows.Scan(&s.ID, &s.UserID, &s.Artist, &s.Title, &s.Album, &s.AlbumArtist, &s.RecordingID, &s.ReleaseID, &s.MSID, &auto); err != nil {
			return nil, err
		}
		s.LinkedByOwner = auto.Valid && !auto.Bool
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListensWithText counts the listens of the given received texts.
func (db *DB) ListensWithText(ctx context.Context, sourceIDs []int64) (int, error) {
	if len(sourceIDs) == 0 {
		return 0, nil
	}
	list, _ := json.Marshal(sourceIDs)
	var n int
	err := db.r.QueryRowContext(ctx, `SELECT count(*) FROM listens
		WHERE source_id IN (SELECT value FROM json_each(?)) AND deleted_by IS NULL`, string(list)).Scan(&n)
	return n, err
}

// liveRecording checks a recording belongs to the user and wasn't merged.
func liveRecording(ctx context.Context, tx *sql.Tx, userID, id int64) error {
	var merged sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT merged_into FROM recordings WHERE id = ? AND user_id = ?`, id, userID).Scan(&merged)
	if errors.Is(err, sql.ErrNoRows) || merged.Valid {
		return ErrNotFound
	}
	return err
}

// releaseFor picks the album a relinked source goes with: the album it's on
// now (which gets the recording as a track if it hasn't got it), or one of
// the recording's albums with the same name as the album received.
func (p *plan) releaseFor(s Source, recordingID int64, planned map[[2]int64]bool) (any, error) {
	if s.ReleaseID.Valid {
		rel := s.ReleaseID.Int64
		track := map[string]any{"release_id": rel, "recording_id": recordingID, "disc": int64(1), "position": nil}
		there, err := p.exists("release_tracks", track)
		if err != nil {
			return nil, err
		}
		if !there && !planned[[2]int64{rel, recordingID}] {
			p.add(Change{Op: OpInsert, Table: "release_tracks", After: track})
			planned[[2]int64{rel, recordingID}] = true
		}
		return rel, nil
	}
	if s.Album == "" {
		return nil, nil
	}
	found, err := OnReleaseAlbumsTx(p.ctx, p.tx, recordingID, s.Album)
	if err != nil || len(found) == 0 {
		return nil, err
	}
	return found[0], nil
}

// OnReleaseAlbumsTx lists a recording's albums named like album.
func OnReleaseAlbumsTx(ctx context.Context, tx *sql.Tx, recordingID int64, album string) ([]int64, error) {
	match, _, _ := aliasKeys(album)
	return ids(ctx, tx, `SELECT DISTINCT rt.release_id FROM release_tracks rt
		JOIN releases rl ON rl.id = rt.release_id AND rl.merged_into IS NULL
		JOIN release_aliases ra ON ra.release_id = rl.id AND ra.match_key = ?
		WHERE rt.recording_id = ? ORDER BY rt.release_id`, match, recordingID)
}

// LinkResult is what linking by hand did.
type LinkResult struct {
	EditID  int64
	Listens int // listens that moved
	Name    string
}

// LinkSources links received text to a recording by hand, as one edit.
// Every listen with that text moves with it.
func (db *DB) LinkSources(ctx context.Context, userID int64, sourceIDs []int64, recordingID int64) (LinkResult, error) {
	var res LinkResult
	err := db.Write(ctx, func(tx *sql.Tx) error {
		if err := liveRecording(ctx, tx, userID, recordingID); err != nil {
			return err
		}
		name, err := entityName(ctx, tx, "recording", recordingID)
		if err != nil {
			return err
		}
		res.Name = name
		p := &plan{ctx: ctx, tx: tx, userID: userID}
		planned := map[[2]int64]bool{}
		ids := slices.Compact(slices.Sorted(slices.Values(sourceIDs)))
		for _, id := range ids {
			s, err := SourceTx(ctx, tx, id)
			if err != nil || s.UserID != userID {
				return ErrNotFound
			}
			rel, err := p.releaseFor(s, recordingID, planned)
			if err != nil {
				return err
			}
			var n int
			tx.QueryRowContext(ctx, `SELECT count(*) FROM listens WHERE source_id = ? AND deleted_by IS NULL`, id).Scan(&n)
			res.Listens += n
			p.linkSource(s, recordingID, rel)
		}
		if len(ids) == 0 {
			return ErrNotFound
		}
		res.EditID, err = p.apply(EditMeta{Kind: "link", Summary: fmt.Sprintf("Linked %s to %s", plural(res.Listens, "listen"), name)})
		return err
	})
	return res, err
}

// linkSource plans pointing received text at a recording, as linked by the
// owner.
func (p *plan) linkSource(s Source, recordingID int64, release any) {
	before := map[string]any{"recording_id": nullable(s.RecordingID), "release_id": nullable(s.ReleaseID),
		"linked_by": currentLinkedBy(p.ctx, p.tx, s.ID)}
	p.changes = append(p.changes, func(editID int64) Change {
		return Change{Table: "sources", ID: s.ID, Before: before,
			After: map[string]any{"recording_id": recordingID, "release_id": release, "linked_by": editID}}
	})
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%s %ss", formatCount(n), word)
}

// formatCount writes n with thousands separators.
func formatCount(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// NewSongTx splits received text off into a new song with one recording
// and the given credits, as one edit. The artists must exist already. The
// listens keep the album they're on.
func NewSongTx(ctx context.Context, tx *sql.Tx, userID, sourceID int64, title string, credits []Credit) (int64, LinkResult, error) {
	var res LinkResult
	s, err := SourceTx(ctx, tx, sourceID)
	if err != nil || s.UserID != userID {
		return 0, res, ErrNotFound
	}
	if len(credits) == 0 {
		return 0, res, errors.New("a new song needs an artist")
	}
	p := &plan{ctx: ctx, tx: tx, userID: userID}
	lang, _ := aliasLang(ctx, tx, "song_aliases", "song_id", 0, title)
	song, err := p.newSong(title, lang, 0)
	if err != nil {
		return 0, res, err
	}
	rec, err := p.newID("recordings")
	if err != nil {
		return 0, res, err
	}
	p.add(Change{Op: OpInsert, Table: "recordings", After: map[string]any{"id": rec, "user_id": userID, "song_id": song, "version": "",
		"is_original": int64(0), "rank_alone": int64(0), "duration_ms": nil, "mbid": nil, "merged_into": nil, "created_at": unix()}})
	seen := map[int64]bool{}
	for i, c := range credits {
		if seen[c.ArtistID] {
			continue
		}
		seen[c.ArtistID] = true
		p.add(Change{Op: OpInsert, Table: "recording_credits", After: map[string]any{"recording_id": rec, "artist_id": c.ArtistID,
			"role": c.Role, "position": int64(i), "credited_as": nil}})
	}
	rel, err := p.releaseFor(s, rec, map[[2]int64]bool{})
	if err != nil {
		return 0, res, err
	}
	p.linkSource(s, rec, rel)
	tx.QueryRowContext(ctx, `SELECT count(*) FROM listens WHERE source_id = ? AND deleted_by IS NULL`, sourceID).Scan(&res.Listens)
	res.Name = title
	res.EditID, err = p.apply(EditMeta{Kind: "link", Summary: fmt.Sprintf("Made %s a new song, with %s", title, plural(res.Listens, "listen"))})
	return rec, res, err
}

// AddRuleTx saves a new rule as one edit and returns the rule and the edit.
func AddRuleTx(ctx context.Context, tx *sql.Tx, userID int64, r Rule, summary string) (int64, int64, error) {
	p := &plan{ctx: ctx, tx: tx, userID: userID}
	id, err := p.newID("rules")
	if err != nil {
		return 0, 0, err
	}
	str := func(s *string) any {
		if s == nil {
			return nil
		}
		return *s
	}
	opt := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	idOrNil := func(v int64) any {
		if v == 0 {
			return nil
		}
		return v
	}
	now := unix()
	p.changes = append(p.changes, func(editID int64) Change {
		return Change{Op: OpInsert, Table: "rules", After: map[string]any{
			"id": id, "user_id": userID, "kind": r.Kind, "field": opt(r.Field),
			"artist_match": str(r.ArtistMatch), "title_match": str(r.TitleMatch), "album_match": str(r.AlbumMatch),
			"match_mode": r.MatchMode, "pattern": opt(r.Pattern), "recording_id": idOrNil(r.RecordingID), "release_id": idOrNil(r.ReleaseID),
			"priority": r.Priority, "enabled": int64(1), "created_by": editID, "created_at": now}}
	})
	editID, err := p.apply(EditMeta{Kind: "rule", Summary: summary})
	return id, editID, err
}

// RuleEdit returns the rule an edit added, or 0 when it didn't add one.
func RuleEditTx(ctx context.Context, tx *sql.Tx, editID int64) (int64, error) {
	var key sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT row_key FROM edit_changes WHERE edit_id = ? AND tbl = 'rules' ORDER BY seq LIMIT 1`, editID).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var k map[string]any
	if err := decodeValues(key.String, &k); err != nil {
		return 0, err
	}
	id, _ := asInt(k["id"])
	return id, nil
}

// RuleTx reads one rule, enabled or not.
func RuleTx(ctx context.Context, tx *sql.Tx, id int64) (Rule, error) {
	r, err := scanRule(tx.QueryRowContext(ctx, `SELECT `+ruleCols+` FROM rules WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// AutoLinkedTx reports whether text is unlinked or was linked automatically.
func AutoLinkedTx(ctx context.Context, tx *sql.Tx, sourceID int64) (bool, error) {
	var auto sql.NullBool
	err := tx.QueryRowContext(ctx, `SELECT e.automatic FROM sources s LEFT JOIN edits e ON e.id = s.linked_by WHERE s.id = ?`, sourceID).Scan(&auto)
	return !auto.Valid || auto.Bool, err
}

// RecordingTitles returns the names of a recording's song, and whether
// another song by the same main artists shares one of them (like two
// games' "Main Theme"), which makes rules on the title alone unsafe.
func (db *DB) RecordingTitles(ctx context.Context, userID, recordingID int64) ([]string, bool, error) {
	var song int64
	err := db.r.QueryRowContext(ctx, `SELECT song_id FROM recordings WHERE id = ? AND user_id = ?`, recordingID, userID).Scan(&song)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, err
	}
	rows, err := db.r.QueryContext(ctx, `SELECT name FROM song_aliases WHERE song_id = ? ORDER BY length(name) DESC, id`, song)
	if err != nil {
		return nil, false, err
	}
	var titles []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, false, err
		}
		titles = append(titles, n)
	}
	rows.Close()
	var others int
	// CROSS JOIN keeps SQLite starting from this song's names. Left to
	// itself, it starts from every song the user has.
	err = db.r.QueryRowContext(ctx, `SELECT count(DISTINCT s2.id) FROM song_aliases a
		CROSS JOIN song_aliases a2 ON a2.match_key = a.match_key AND a2.song_id <> a.song_id
		JOIN songs s2 ON s2.id = a2.song_id AND s2.merged_into IS NULL AND s2.user_id = ?
		JOIN recordings r2 ON r2.song_id = s2.id AND r2.merged_into IS NULL
		JOIN recording_credits c2 ON c2.recording_id = r2.id AND c2.role = 'main'
		WHERE a.song_id = ? AND c2.artist_id IN (SELECT artist_id FROM recording_credits WHERE recording_id = ? AND role = 'main')`,
		userID, song, recordingID).Scan(&others)
	return titles, others > 0, err
}

// RecordingSongTx returns a recording's song name and its main and
// featured credits.
func RecordingSongTx(ctx context.Context, tx *sql.Tx, recordingID int64) (string, []Credit, error) {
	var title string
	if err := tx.QueryRowContext(ctx, `SELECT s.name FROM recordings r JOIN songs s ON s.id = r.song_id WHERE r.id = ?`, recordingID).Scan(&title); err != nil {
		return "", nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT artist_id, role FROM recording_credits WHERE recording_id = ? AND role IN ('main', 'featured') ORDER BY position`, recordingID)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var cs []Credit
	for rows.Next() {
		var c Credit
		if err := rows.Scan(&c.ArtistID, &c.Role); err != nil {
			return "", nil, err
		}
		cs = append(cs, c)
	}
	return title, cs, rows.Err()
}

// AllRules lists all of a user's rules, on or off, oldest first.
func (db *DB) AllRules(ctx context.Context, userID int64) ([]Rule, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT `+ruleCols+` FROM rules WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecordingName is how a recording is named in rule descriptions.
func (db *DB) RecordingName(ctx context.Context, id int64) (string, error) {
	var name, version string
	err := db.r.QueryRowContext(ctx, `SELECT s.name, r.version FROM recordings r JOIN songs s ON s.id = r.song_id WHERE r.id = ?`, id).
		Scan(&name, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return "a song that's gone", nil
	}
	if version != "" {
		name += " (" + version + ")"
	}
	return name, err
}

// RuleSwitch is a rule with no conditions and whether it should be on.
type RuleSwitch struct {
	Rule
	On bool
}

// SetRulesTx switches rules with no conditions on or off, adding the ones
// switching on that aren't there, as one edit. It returns 0 when nothing
// changed.
func SetRulesTx(ctx context.Context, tx *sql.Tx, userID int64, want []RuleSwitch, summary string) (int64, error) {
	p := &plan{ctx: ctx, tx: tx, userID: userID}
	for _, w := range want {
		on := w.On
		ids, err := ids(ctx, tx, `SELECT id FROM rules WHERE user_id = ? AND kind = ? AND pattern IS ?
			AND artist_match IS NULL AND title_match IS NULL AND album_match IS NULL ORDER BY id`, userID, w.Kind, nullStr(w.Pattern))
		if err != nil {
			return 0, err
		}
		if len(ids) == 0 {
			if !on {
				continue
			}
			id, err := p.newID("rules")
			if err != nil {
				return 0, err
			}
			field := any(nil)
			if w.Field != "" {
				field = w.Field
			}
			now := unix()
			p.changes = append(p.changes, func(editID int64) Change {
				return Change{Op: OpInsert, Table: "rules", After: map[string]any{
					"id": id, "user_id": userID, "kind": w.Kind, "field": field, "artist_match": nil, "title_match": nil, "album_match": nil,
					"match_mode": w.MatchMode, "pattern": nullStr(w.Pattern), "recording_id": nil, "release_id": nil,
					"priority": int64(0), "enabled": int64(1), "created_by": editID, "created_at": now}}
			})
			continue
		}
		for _, id := range ids {
			if err := p.update("rules", id, map[string]any{"enabled": boolInt(on)}); err != nil {
				return 0, err
			}
		}
	}
	if len(p.changes) == 0 {
		return 0, nil
	}
	return p.apply(EditMeta{Kind: "rule", Summary: summary})
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ChangeRuleTx switches one of the user's rules on ("on") or off ("off"),
// or deletes it ("delete"), as one edit. It returns 0 when nothing changed.
func ChangeRuleTx(ctx context.Context, tx *sql.Tx, userID, ruleID int64, action, text string) (int64, error) {
	var owner int64
	if err := tx.QueryRowContext(ctx, `SELECT user_id FROM rules WHERE id = ?`, ruleID).Scan(&owner); err != nil || owner != userID {
		return 0, ErrNotFound
	}
	p := &plan{ctx: ctx, tx: tx, userID: userID}
	var summary string
	switch action {
	case "on", "off":
		if err := p.update("rules", ruleID, map[string]any{"enabled": boolInt(action == "on")}); err != nil {
			return 0, err
		}
		summary = fmt.Sprintf("Turned %s rule: %s", action, text)
	case "delete":
		p.add(Change{Op: OpDelete, Table: "rules", ID: ruleID})
		summary = "Deleted rule: " + text
	default:
		return 0, ErrValue
	}
	if len(p.changes) == 0 {
		return 0, nil
	}
	return p.apply(EditMeta{Kind: "rule", Summary: summary})
}

// AutoSourcesTx lists a user's text that isn't linked by hand: unlinked, or
// linked automatically.
func AutoSourcesTx(ctx context.Context, tx *sql.Tx, userID int64) ([]int64, error) {
	return ids(ctx, tx, `SELECT s.id FROM sources s LEFT JOIN edits e ON e.id = s.linked_by
		WHERE s.user_id = ? AND (s.linked_by IS NULL OR e.automatic = 1) ORDER BY s.id`, userID)
}
