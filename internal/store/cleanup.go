package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Cleaning up what scrobblers got wrong: the artists credited on a
// recording or album, recordings that ended up on an album they're not
// on, deleting artists and albums nothing uses anymore, and the graveyard
// for songs that aren't music. Each is one edit, undoable.

var (
	ErrBuried = errors.New("that song is in the graveyard. Bring it back first")
	ErrInUse  = errors.New("it's still in use")
	ErrNoMain = errors.New("a recording needs at least one main artist")
)

// CreditRef is an artist credited on a recording, with their role.
type CreditRef struct {
	Ref
	Role string // main or featured
}

// RecordingCredits lists the artists credited on a recording, in order.
func (db *DB) RecordingCredits(ctx context.Context, recordingID int64) ([]CreditRef, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT a.id, a.name, a.other_names, c.role FROM recording_credits c JOIN artists a ON a.id = c.artist_id
		WHERE c.recording_id = ? AND c.role IN ('main', 'featured') ORDER BY c.position`, recordingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CreditRef
	for rows.Next() {
		var c CreditRef
		if err := rows.Scan(&c.ID, &c.Name, &c.OtherNames, &c.Role); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreditChoice is an artist to credit: one that exists, or a new one by
// name for someone no scrobble has named yet, like a character.
type CreditChoice struct {
	ArtistID  int64
	NewArtist string // when ArtistID is 0
	Role      string // main or featured, for recordings
}

// artist resolves a credit choice to an artist id and name, planning the
// new artist when it's one.
func (p *plan) artist(c CreditChoice) (int64, string, error) {
	if c.ArtistID != 0 {
		name, err := p.liveArtist(c.ArtistID)
		return c.ArtistID, name, err
	}
	return p.newArtist(c.NewArtist)
}

// newArtist plans an artist with one name.
func (p *plan) newArtist(name string) (int64, string, error) {
	name, err := cleanName(name)
	if err != nil {
		return 0, "", err
	}
	id, err := p.newID("artists")
	if err != nil {
		return 0, "", err
	}
	aliasID, err := p.newID("artist_aliases")
	if err != nil {
		return 0, "", err
	}
	lang, err := aliasLang(p.ctx, p.tx, "artist_aliases", "artist_id", id, name)
	if err != nil {
		return 0, "", err
	}
	p.add(Change{Op: OpInsert, Table: "artists", After: map[string]any{
		"id": id, "user_id": p.userID, "kind": "other", "name": name, "other_names": "", "pinned_alias": nil, "mbid": nil,
		"merged_into": nil, "created_at": unix(), "artwork_id": nil, "artwork_pinned": int64(0), "second_alias": nil, "second_set": int64(0), "byline": ""}})
	p.add(Change{Op: OpInsert, Table: "artist_aliases", After: map[string]any{
		"id": aliasID, "artist_id": id, "name": name, "lang": lang, "lang_set": int64(0), "shown": int64(1)}})
	p.made = append(p.made, name)
	return id, name, nil
}

// madeNote adds the artists a plan made to its summary.
func (p *plan) madeNote(summary string) string {
	if len(p.made) == 0 {
		return summary
	}
	return summary + ", adding the artist " + strings.Join(p.made, ", ")
}

// liveArtist returns the name of an artist of the user's that wasn't
// merged away.
func (p *plan) liveArtist(id int64) (string, error) {
	var name string
	var merged sql.NullInt64
	err := p.tx.QueryRowContext(p.ctx, `SELECT name, merged_into FROM artists WHERE id = ? AND user_id = ?`, id, p.userID).Scan(&name, &merged)
	if errors.Is(err, sql.ErrNoRows) || merged.Valid {
		return "", ErrNoArtist
	}
	return name, err
}

func artistList(names []string) string {
	if len(names) == 0 {
		return "no one"
	}
	return strings.Join(names, ", ")
}

// SetRecordingCredits replaces the main and featured artists credited on
// one of a song's recordings, for a recording credited to a channel or a
// voice actor instead of the character. Listens already linked stay. Other
// roles (composer and the like) are kept.
func (db *DB) SetRecordingCredits(ctx context.Context, userID, songID, recordingID int64, credits []CreditChoice) (int64, error) {
	return db.editItem(ctx, userID, "song", songID, func(p *plan, song string) (string, error) {
		if err := p.songRecording(songID, recordingID); err != nil {
			return "", err
		}
		before, err := p.query("recording_credits", "recording_id = ? AND role IN ('main', 'featured')", recordingID)
		if err != nil {
			return "", err
		}
		var oldNames, newNames []string
		old := map[[2]any]map[string]any{}
		for _, row := range before {
			id, _ := asInt(row["artist_id"])
			name, _ := p.liveArtist(id)
			oldNames = append(oldNames, name)
			old[[2]any{id, row["role"]}] = row
		}
		keep := map[[2]any]bool{}
		main := false
		for i, c := range credits {
			if c.Role != "main" && c.Role != "featured" {
				return "", ErrValue
			}
			main = main || c.Role == "main"
			artistID, name, err := p.artist(c)
			if err != nil {
				return "", err
			}
			if c.Role == "featured" {
				name += " (featured)"
			}
			newNames = append(newNames, name)
			k := [2]any{artistID, c.Role}
			if keep[k] {
				continue
			}
			keep[k] = true
			if row, ok := old[k]; ok {
				if pos, _ := asInt(row["position"]); pos != int64(i) {
					p.add(Change{Table: "recording_credits", Key: keyOf("recording_credits", row), Before: map[string]any{"position": row["position"]}, After: map[string]any{"position": int64(i)}})
				}
				continue
			}
			p.add(Change{Op: OpInsert, Table: "recording_credits", After: map[string]any{
				"recording_id": recordingID, "artist_id": artistID, "role": c.Role, "position": int64(i), "credited_as": nil}})
		}
		for k, row := range old {
			if !keep[k] {
				p.add(Change{Op: OpDelete, Table: "recording_credits", Key: keyOf("recording_credits", row), Before: row})
			}
		}
		if !main {
			return "", ErrNoMain
		}
		if slices.Equal(oldNames, newNames) {
			return "", nil
		}
		return p.madeNote(fmt.Sprintf("Credited %s on %s instead of %s", artistList(newNames), recordingTitle(p, recordingID, song), artistList(oldNames))), nil
	})
}

// recordingTitle names a recording in a summary: the song, with the
// version when it has one.
func recordingTitle(p *plan, recordingID int64, song string) string {
	var version string
	p.tx.QueryRowContext(p.ctx, `SELECT version FROM recordings WHERE id = ?`, recordingID).Scan(&version)
	if version != "" {
		return song + " (" + version + ")"
	}
	return song
}

// SetAlbumArtists replaces the artists an album is credited to.
func (db *DB) SetAlbumArtists(ctx context.Context, userID, releaseID int64, artists []CreditChoice) (int64, error) {
	return db.editItem(ctx, userID, "release", releaseID, func(p *plan, album string) (string, error) {
		before, err := p.query("release_credits", "release_id = ?", releaseID)
		if err != nil {
			return "", err
		}
		var oldNames, newNames []string
		old := map[int64]map[string]any{}
		for _, row := range before {
			id, _ := asInt(row["artist_id"])
			name, _ := p.liveArtist(id)
			oldNames = append(oldNames, name)
			old[id] = row
		}
		keep := map[int64]bool{}
		for i, c := range artists {
			id, name, err := p.artist(c)
			if err != nil {
				return "", err
			}
			if keep[id] {
				continue
			}
			keep[id] = true
			newNames = append(newNames, name)
			if row, ok := old[id]; ok {
				if pos, _ := asInt(row["position"]); pos != int64(i) {
					p.add(Change{Table: "release_credits", Key: keyOf("release_credits", row), Before: map[string]any{"position": row["position"]}, After: map[string]any{"position": int64(i)}})
				}
				continue
			}
			p.add(Change{Op: OpInsert, Table: "release_credits", After: map[string]any{"release_id": releaseID, "artist_id": id, "position": int64(i)}})
		}
		for id, row := range old {
			if !keep[id] {
				p.add(Change{Op: OpDelete, Table: "release_credits", Key: keyOf("release_credits", row), Before: row})
			}
		}
		if slices.Equal(oldNames, newNames) {
			return "", nil
		}
		return p.madeNote(fmt.Sprintf("Credited %s on the album %s instead of %s", artistList(newNames), album, artistList(oldNames))), nil
	})
}

// CreateArtist adds an artist with one name, for crediting someone no
// scrobble has named yet, like a character. It returns the artist and the
// edit.
func (db *DB) CreateArtist(ctx context.Context, userID int64, name string) (int64, int64, error) {
	name, err := cleanName(name)
	if err != nil {
		return 0, 0, err
	}
	var artistID, editID int64
	err = db.Write(ctx, func(tx *sql.Tx) error {
		p := &plan{ctx: ctx, tx: tx, userID: userID}
		if artistID, _, err = p.newArtist(name); err != nil {
			return err
		}
		editID, err = p.apply(EditMeta{Kind: "edit", Summary: "Added the artist " + name})
		return err
	})
	return artistID, editID, err
}

// TakeOffAlbum takes recordings off an album: their listens that were on
// it have no album from then on, and it no longer lists them. The text
// those listens were sent with counts as linked by hand, so reading it
// again never puts it back.
func (db *DB) TakeOffAlbum(ctx context.Context, userID, releaseID int64, recordingIDs []int64) (int64, error) {
	return db.editItem(ctx, userID, "release", releaseID, func(p *plan, album string) (string, error) {
		var names []string
		for _, rec := range slices.Compact(slices.Sorted(slices.Values(recordingIDs))) {
			tracks, err := p.query("release_tracks", "release_id = ? AND recording_id = ?", releaseID, rec)
			if err != nil {
				return "", err
			}
			srcs, err := queryRows(p.ctx, p.tx, `SELECT id, recording_id, release_id, linked_by FROM sources WHERE release_id = ? AND recording_id = ? AND user_id = ?`,
				[]string{"id", "recording_id", "release_id", "linked_by"}, releaseID, rec, p.userID)
			if err != nil {
				return "", err
			}
			if len(tracks) == 0 && len(srcs) == 0 {
				return "", ErrNotFound
			}
			for _, t := range tracks {
				p.add(Change{Op: OpDelete, Table: "release_tracks", Key: keyOf("release_tracks", t), Before: t})
			}
			for _, s := range srcs {
				before := map[string]any{"release_id": s["release_id"], "linked_by": s["linked_by"]}
				id, _ := asInt(s["id"])
				p.changes = append(p.changes, func(editID int64) Change {
					return Change{Table: "sources", ID: id, Before: before, After: map[string]any{"release_id": nil, "linked_by": editID}}
				})
			}
			var song string
			p.tx.QueryRowContext(p.ctx, `SELECT s.name FROM recordings r JOIN songs s ON s.id = r.song_id WHERE r.id = ?`, rec).Scan(&song)
			names = append(names, recordingTitle(p, rec, song))
		}
		if len(names) == 0 {
			return "", nil
		}
		return fmt.Sprintf("Took %s off the album %s", artistList(names), album), nil
	})
}

// Usage is what still points at an artist or album, which has to be moved
// or fixed before it can be deleted. Listens count hidden ones too, since
// they still name the album.
type Usage struct {
	Songs, Albums, Listens, Links, Rules int
	MergedInto                           bool // others were merged into it
}

func (u Usage) Used() bool {
	return u.Songs+u.Albums+u.Listens+u.Links+u.Rules > 0 || u.MergedInto
}

// ItemUsage says what uses an artist or album.
func (db *DB) ItemUsage(ctx context.Context, kind string, id int64) (Usage, error) {
	var u Usage
	err := db.r.QueryRowContext(ctx, usageQuery[kind], id).Scan(&u.Songs, &u.Albums, &u.Listens, &u.Links, &u.Rules, &u.MergedInto)
	return u, err
}

var usageQuery = map[string]string{
	"artist": `SELECT
		(SELECT count(DISTINCT r.song_id) FROM recording_credits c JOIN recordings r ON r.id = c.recording_id WHERE c.artist_id = ?1),
		(SELECT count(*) FROM release_credits WHERE artist_id = ?1),
		0,
		(SELECT count(*) FROM group_members WHERE group_id = ?1 OR member_id = ?1) +
		(SELECT count(*) FROM artist_counts_for WHERE artist_id = ?1 OR target_id = ?1) +
		(SELECT count(*) FROM credit_overrides WHERE from_id = ?1 OR to_id = ?1),
		0,
		EXISTS (SELECT 1 FROM artists WHERE merged_into = ?1)`,
	"release": `SELECT
		(SELECT count(*) FROM release_tracks WHERE release_id = ?1),
		0,
		(SELECT count(*) FROM listens WHERE release_id = ?1),
		0,
		(SELECT count(*) FROM rules WHERE release_id = ?1),
		EXISTS (SELECT 1 FROM releases WHERE merged_into = ?1)`,
}

// DeleteItem deletes an artist or album nothing uses anymore, with its
// names, labels and album credits.
func (db *DB) DeleteItem(ctx context.Context, userID int64, kind string, id int64) (int64, error) {
	if kind != "artist" && kind != "release" {
		return 0, ErrValue
	}
	t := itemTables[kind]
	var editID int64
	err := db.Write(ctx, func(tx *sql.Tx) error {
		p := &plan{ctx: ctx, tx: tx, userID: userID}
		rows, err := p.query(t.entity, "id = ? AND user_id = ? AND merged_into IS NULL", id, userID)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return ErrNotFound
		}
		var u Usage
		if err := tx.QueryRowContext(ctx, usageQuery[kind], id).Scan(&u.Songs, &u.Albums, &u.Listens, &u.Links, &u.Rules, &u.MergedInto); err != nil {
			return err
		}
		var sent int
		if kind == "release" {
			tx.QueryRowContext(ctx, `SELECT count(*) FROM sources WHERE release_id = ?`, id).Scan(&sent)
		}
		if u.Used() || sent > 0 {
			return ErrInUse
		}
		if kind == "release" {
			credits, err := p.query("release_credits", "release_id = ?", id)
			if err != nil {
				return err
			}
			for _, c := range credits {
				p.add(Change{Op: OpDelete, Table: "release_credits", Key: keyOf("release_credits", c), Before: c})
			}
		}
		labels, err := p.query("entity_labels", "entity_type = ? AND entity_id = ?", kind, id)
		if err != nil {
			return err
		}
		for _, l := range labels {
			p.add(Change{Op: OpDelete, Table: "entity_labels", Key: keyOf("entity_labels", l), Before: l})
		}
		if kind == "release" {
			overrides, err := p.query("credit_overrides", "scope = 'release' AND scope_id = ?", id)
			if err != nil {
				return err
			}
			for _, o := range overrides {
				p.add(Change{Op: OpDelete, Table: "credit_overrides", Key: keyOf("credit_overrides", o), Before: o})
			}
		}
		// Its names go first and the item last, so undo brings the item
		// back before its names.
		aliases, err := p.query(t.alias, t.owner+" = ?", id)
		if err != nil {
			return err
		}
		for _, a := range aliases {
			p.add(Change{Op: OpDelete, Table: t.alias, Key: keyOf(t.alias, a), Before: a})
		}
		row := rows[0]
		p.add(Change{Op: OpDelete, Table: t.entity, Key: keyOf(t.entity, row), Before: row})
		if _, err := tx.ExecContext(ctx, `DELETE FROM suggestions WHERE kind = ? AND (a_id = ? OR b_id = ?)`, kind, id, id); err != nil {
			return err
		}
		noun := map[string]string{"artist": "artist", "release": "album"}[kind]
		editID, err = p.apply(EditMeta{Kind: "edit", Summary: fmt.Sprintf("Deleted the %s %s", noun, row["name"])})
		return err
	})
	return editID, err
}

// The graveyard

// BurySong moves a song to the graveyard: its listens are kept but hidden,
// and later listens of it are hidden too, until the owner brings it back
// or deletes them.
func (db *DB) BurySong(ctx context.Context, userID, songID int64) (int64, error) {
	return db.editItem(ctx, userID, "song", songID, func(p *plan, song string) (string, error) {
		var buried sql.NullInt64
		if err := p.tx.QueryRowContext(p.ctx, `SELECT buried_by FROM songs WHERE id = ?`, songID).Scan(&buried); err != nil {
			return "", err
		}
		if buried.Valid {
			return "", nil
		}
		listens, err := ids(p.ctx, p.tx, `SELECT l.id FROM listens l JOIN recordings r ON r.id = l.recording_id
			WHERE r.song_id = ? AND l.deleted_by IS NULL ORDER BY l.id`, songID)
		if err != nil {
			return "", err
		}
		// The song first, so undo restores the logged listens before the
		// song, and then finds the ones hidden since.
		p.changes = append(p.changes, func(editID int64) Change {
			return Change{Table: "songs", ID: songID, Before: map[string]any{"buried_by": nil}, After: map[string]any{"buried_by": editID}}
		})
		for _, l := range listens {
			p.changes = append(p.changes, func(editID int64) Change {
				return Change{Table: "listens", ID: l, Before: map[string]any{"deleted_by": nil}, After: map[string]any{"deleted_by": editID}}
			})
		}
		return fmt.Sprintf("Moved %s to the graveyard with %s", song, plural(len(listens), "listen")), nil
	})
}

// UnburySong brings a song back from the graveyard with its listens.
func (db *DB) UnburySong(ctx context.Context, userID, songID int64) (int64, error) {
	return db.editItem(ctx, userID, "song", songID, func(p *plan, song string) (string, error) {
		var buried sql.NullInt64
		if err := p.tx.QueryRowContext(p.ctx, `SELECT buried_by FROM songs WHERE id = ?`, songID).Scan(&buried); err != nil {
			return "", err
		}
		if !buried.Valid {
			return "", nil
		}
		listens, err := ids(p.ctx, p.tx, `SELECT id FROM listens WHERE deleted_by = ? ORDER BY id`, buried.Int64)
		if err != nil {
			return "", err
		}
		for _, l := range listens {
			p.add(Change{Table: "listens", ID: l, Before: map[string]any{"deleted_by": buried.Int64}, After: map[string]any{"deleted_by": nil}})
		}
		p.add(Change{Table: "songs", ID: songID, Before: map[string]any{"buried_by": buried.Int64}, After: map[string]any{"buried_by": nil}})
		return fmt.Sprintf("Brought %s back from the graveyard with %s", song, plural(len(listens), "listen")), nil
	})
}

// DeleteBuriedListens deletes the listens of a song in the graveyard, the
// same way as deleting one from its Fix page. The song stays there, so
// later listens of it are hidden too.
func (db *DB) DeleteBuriedListens(ctx context.Context, userID, songID int64) (int64, error) {
	return db.editItem(ctx, userID, "song", songID, func(p *plan, song string) (string, error) {
		var buried sql.NullInt64
		if err := p.tx.QueryRowContext(p.ctx, `SELECT buried_by FROM songs WHERE id = ?`, songID).Scan(&buried); err != nil {
			return "", err
		}
		if !buried.Valid {
			return "", ErrNotFound
		}
		listens, err := ids(p.ctx, p.tx, `SELECT id FROM listens WHERE deleted_by = ? ORDER BY id`, buried.Int64)
		if err != nil || len(listens) == 0 {
			return "", err
		}
		for _, l := range listens {
			p.changes = append(p.changes, func(editID int64) Change {
				return Change{Table: "listens", ID: l, Before: map[string]any{"deleted_by": buried.Int64}, After: map[string]any{"deleted_by": editID}}
			})
		}
		return fmt.Sprintf("Deleted the %s of %s from the graveyard", plural(len(listens), "listen"), song), nil
	})
}

// GraveSong is a song in the graveyard that still has listens there.
type GraveSong struct {
	Ref
	Artists  []Ref
	Listens  int
	BuriedAt int64
	By       string // the agent that moved it, when one did
}

// Graveyard lists the songs in the graveyard with listens to decide on,
// most listens first.
func (db *DB) Graveyard(ctx context.Context, userID int64) ([]GraveSong, error) {
	// Only songs in the graveyard are counted, each through the
	// listens_by_deleted index. Counting for every song read all listens
	// once per song.
	rows, err := db.r.QueryContext(ctx, `SELECT s.id, s.name, s.other_names, e.created_at, coalesce(t.label, ''),
			(SELECT count(*) FROM listens l WHERE l.deleted_by IS NOT NULL AND l.deleted_by = s.buried_by) AS n
		FROM songs s JOIN edits e ON e.id = s.buried_by LEFT JOIN api_tokens t ON t.id = e.agent_token_id
		WHERE s.user_id = ? AND s.buried_by IS NOT NULL AND s.merged_into IS NULL AND n > 0 ORDER BY n DESC, s.id`, userID)
	if err != nil {
		return nil, err
	}
	var out []GraveSong
	for rows.Next() {
		var g GraveSong
		if err := rows.Scan(&g.ID, &g.Name, &g.OtherNames, &g.BuriedAt, &g.By, &g.Listens); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Artists, err = db.refs(ctx, `SELECT DISTINCT a.id, a.name, a.other_names FROM recordings r
			JOIN recording_credits c ON c.recording_id = r.id AND c.role = 'main' JOIN artists a ON a.id = c.artist_id
			WHERE r.song_id = ? AND r.merged_into IS NULL`, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SongBuried reports whether a song is in the graveyard.
func (db *DB) SongBuried(ctx context.Context, songID int64) (bool, error) {
	var buried bool
	err := db.r.QueryRowContext(ctx, `SELECT buried_by IS NOT NULL FROM songs WHERE id = ?`, songID).Scan(&buried)
	return buried, err
}

// buriedTx reports whether a song, or the song of a recording, is in
// the graveyard.
func buriedTx(ctx context.Context, tx *sql.Tx, kind string, id int64) (bool, error) {
	q := `SELECT buried_by IS NOT NULL FROM songs WHERE id = ?`
	if kind == "recording" {
		q = `SELECT s.buried_by IS NOT NULL FROM recordings r JOIN songs s ON s.id = r.song_id WHERE r.id = ?`
	}
	var buried bool
	err := tx.QueryRowContext(ctx, q, id).Scan(&buried)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return buried, err
}

// RecordingSong returns the song a recording belongs to.
func (db *DB) RecordingSong(ctx context.Context, userID, recordingID int64) (int64, error) {
	var song int64
	err := db.r.QueryRowContext(ctx, `SELECT song_id FROM recordings WHERE id = ? AND user_id = ? AND merged_into IS NULL`, recordingID, userID).Scan(&song)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return song, err
}
