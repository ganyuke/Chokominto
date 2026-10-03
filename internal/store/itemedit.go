package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"chokominto/internal/names"
)

// Editing artists, songs and albums from their own pages. Each change is
// one edit. See docs/architecture.md, "Canonical entities".

var (
	ErrName     = errors.New("give it a name")
	ErrLastName = errors.New("the only name can't be removed")
	ErrLoop     = errors.New("that would go round in a circle")
	ErrNoArtist = errors.New("no artist by that name")
	ErrValue    = errors.New("that value isn't allowed")
)

// Item kinds on pages, with their tables.
var itemTables = map[string]struct{ entity, alias, owner string }{
	"artist":  {"artists", "artist_aliases", "artist_id"},
	"song":    {"songs", "song_aliases", "song_id"},
	"release": {"releases", "release_aliases", "release_id"},
}

// Alias is one of an item's names.
type Alias struct {
	ID      int64
	Name    string
	Lang    string // en, romaji or original
	LangSet bool   // set by the owner, not guessed
	Pinned  bool
	Shown   bool // listed on the item's own page
	InLists bool // shown under the item's name in lists
}

// Aliases lists an item's names in display order. The first is the one
// it's shown by.
func (db *DB) Aliases(ctx context.Context, kind string, id int64) ([]Alias, error) {
	t, ok := itemTables[kind]
	if !ok {
		return nil, ErrNotFound
	}
	var second sql.NullInt64
	var secondSet bool
	if err := db.r.QueryRowContext(ctx, `SELECT second_alias, second_set FROM `+t.entity+` WHERE id = ?`, id).Scan(&second, &secondSet); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	rows, err := db.r.QueryContext(ctx, fmt.Sprintf(
		`SELECT a.id, a.name, a.lang, a.lang_set, coalesce(a.id = e.pinned_alias, 0), a.shown FROM %s a JOIN %s e ON e.id = a.%s WHERE a.%s = ?
		 ORDER BY (a.id = e.pinned_alias) DESC, a.shown DESC, CASE a.lang WHEN 'en' THEN 0 WHEN 'romaji' THEN 1 ELSE 2 END, a.id`,
		t.alias, t.entity, t.owner, t.owner), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alias
	picked := false
	for rows.Next() {
		var a Alias
		if err := rows.Scan(&a.ID, &a.Name, &a.Lang, &a.LangSet, &a.Pinned, &a.Shown); err != nil {
			return nil, err
		}
		// The same choice refreshNamesTx makes.
		if len(out) > 0 && !picked {
			if secondSet {
				a.InLists = second.Valid && a.ID == second.Int64
			} else {
				a.InLists = a.Shown
			}
			picked = a.InLists
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// editItem runs fn to plan changes to one live item of the user's, and
// applies them as one edit with the summary fn returns.
func (db *DB) editItem(ctx context.Context, userID int64, kind string, id int64, fn func(p *plan, name string) (string, error)) (int64, error) {
	t, ok := itemTables[kind]
	if !ok {
		return 0, ErrNotFound
	}
	var editID int64
	err := db.Write(ctx, func(tx *sql.Tx) error {
		var name string
		var merged sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT name, merged_into FROM `+t.entity+` WHERE id = ? AND user_id = ?`, id, userID).Scan(&name, &merged)
		if errors.Is(err, sql.ErrNoRows) || merged.Valid {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		p := &plan{ctx: ctx, tx: tx, userID: userID}
		summary, err := fn(p, name)
		if err != nil {
			return err
		}
		if len(p.changes) == 0 {
			return nil // nothing to change
		}
		editID, err = p.apply(EditMeta{Kind: "edit", Summary: summary})
		return err
	})
	return editID, err
}

func cleanName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || len([]rune(s)) > 200 {
		return "", ErrName
	}
	return s, nil
}

func (p *plan) aliasRow(kind string, owner, aliasID int64) (map[string]any, error) {
	t := itemTables[kind]
	rows, err := p.query(t.alias, "id = ? AND "+t.owner+" = ?", aliasID, owner)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	return rows[0], nil
}

// pinIfNeeded pins aliasID when, with the given language, it wouldn't be
// the name shown first.
func (p *plan) pinIfNeeded(kind string, id, aliasID int64, lang string) error {
	t := itemTables[kind]
	var pinned sql.NullInt64
	if err := p.tx.QueryRowContext(p.ctx, `SELECT pinned_alias FROM `+t.entity+` WHERE id = ?`, id).Scan(&pinned); err != nil {
		return err
	}
	if pinned.Valid {
		if pinned.Int64 == aliasID {
			return nil
		}
		return p.update(t.entity, id, map[string]any{"pinned_alias": aliasID})
	}
	// Hidden names come after shown ones, so only shown ones can come first.
	rows, err := queryRows(p.ctx, p.tx, fmt.Sprintf(`SELECT id, lang FROM %s WHERE %s = ? AND id <> ? AND shown = 1`, t.alias, t.owner), []string{"id", "lang"}, id, aliasID)
	if err != nil {
		return err
	}
	for _, r := range rows {
		oid, _ := asInt(r["id"])
		ol, _ := r["lang"].(string)
		if langRank(ol) < langRank(lang) || langRank(ol) == langRank(lang) && oid < aliasID {
			return p.update(t.entity, id, map[string]any{"pinned_alias": aliasID})
		}
	}
	return nil
}

// Rename replaces the name an item is shown by. If the item already has
// the new name among its other names, that one is shown first instead.
func (db *DB) Rename(ctx context.Context, userID int64, kind string, id int64, newName string) (int64, error) {
	newName, err := cleanName(newName)
	if err != nil {
		return 0, err
	}
	t := itemTables[kind]
	return db.editItem(ctx, userID, kind, id, func(p *plan, old string) (string, error) {
		if old == newName {
			return "", nil
		}
		summary := fmt.Sprintf("Renamed %s to %s", old, newName)
		var existing int64
		err := p.tx.QueryRowContext(p.ctx, fmt.Sprintf(`SELECT id FROM %s WHERE %s = ? AND name = ?`, t.alias, t.owner), id, newName).Scan(&existing)
		if err == nil {
			return summary, p.update(t.entity, id, map[string]any{"pinned_alias": existing})
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		var primary int64
		var langSet bool
		var lang string
		if err := p.tx.QueryRowContext(p.ctx, fmt.Sprintf(`SELECT id, lang, lang_set FROM %s WHERE %s = ? AND name = ? ORDER BY id LIMIT 1`, t.alias, t.owner), id, old).
			Scan(&primary, &lang, &langSet); err != nil {
			return "", err
		}
		after := map[string]any{"name": newName}
		if !langSet {
			if lang, err = aliasLang(p.ctx, p.tx, t.alias, t.owner, id, newName); err != nil {
				return "", err
			}
			after["lang"] = lang
		}
		if err := p.update(t.alias, primary, after); err != nil {
			return "", err
		}
		return summary, p.pinIfNeeded(kind, id, primary, lang)
	})
}

// AddName gives an item another name.
func (db *DB) AddName(ctx context.Context, userID int64, kind string, id int64, name string) (int64, error) {
	name, err := cleanName(name)
	if err != nil {
		return 0, err
	}
	t := itemTables[kind]
	return db.editItem(ctx, userID, kind, id, func(p *plan, item string) (string, error) {
		var n int
		p.tx.QueryRowContext(p.ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s = ? AND name = ?`, t.alias, t.owner), id, name).Scan(&n)
		if n > 0 {
			return "", nil
		}
		lang, err := aliasLang(p.ctx, p.tx, t.alias, t.owner, id, name)
		if err != nil {
			return "", err
		}
		aid, err := p.newID(t.alias)
		if err != nil {
			return "", err
		}
		p.add(Change{Op: OpInsert, Table: t.alias, After: map[string]any{"id": aid, t.owner: id, "name": name, "lang": lang, "lang_set": int64(0), "shown": int64(1)}})
		return fmt.Sprintf("Added the name %s to %s", name, item), nil
	})
}

// RemoveName takes one of an item's names away. The last one stays.
func (db *DB) RemoveName(ctx context.Context, userID int64, kind string, id, aliasID int64) (int64, error) {
	t := itemTables[kind]
	return db.editItem(ctx, userID, kind, id, func(p *plan, item string) (string, error) {
		row, err := p.aliasRow(kind, id, aliasID)
		if err != nil {
			return "", err
		}
		var n int
		p.tx.QueryRowContext(p.ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s = ?`, t.alias, t.owner), id).Scan(&n)
		if n <= 1 {
			return "", ErrLastName
		}
		var pinned, second sql.NullInt64
		p.tx.QueryRowContext(p.ctx, `SELECT pinned_alias, second_alias FROM `+t.entity+` WHERE id = ?`, id).Scan(&pinned, &second)
		if pinned.Valid && pinned.Int64 == aliasID {
			if err := p.update(t.entity, id, map[string]any{"pinned_alias": nil}); err != nil {
				return "", err
			}
		}
		if second.Valid && second.Int64 == aliasID {
			if err := p.update(t.entity, id, map[string]any{"second_alias": nil, "second_set": int64(0)}); err != nil {
				return "", err
			}
		}
		p.add(Change{Op: OpDelete, Table: t.alias, ID: aliasID, Before: row})
		return fmt.Sprintf("Removed the name %s from %s", row["name"], item), nil
	})
}

var langNames = map[string]string{"en": "English", "romaji": "romaji", "original": "original script"}

// SetNameLang says which kind of name one of an item's names is, so it's
// never guessed again.
func (db *DB) SetNameLang(ctx context.Context, userID int64, kind string, id, aliasID int64, lang string) (int64, error) {
	if _, ok := langNames[lang]; !ok {
		return 0, ErrValue
	}
	t := itemTables[kind]
	return db.editItem(ctx, userID, kind, id, func(p *plan, item string) (string, error) {
		row, err := p.aliasRow(kind, id, aliasID)
		if err != nil {
			return "", err
		}
		if err := p.update(t.alias, aliasID, map[string]any{"lang": lang, "lang_set": int64(1)}); err != nil {
			return "", err
		}
		return fmt.Sprintf("Marked %s as %s", row["name"], langNames[lang]), nil
	})
}

// NameChoice is how one of an item's names is shown: its kind (en,
// romaji or original) and whether it's listed on the item's own page.
type NameChoice struct {
	AliasID int64
	Lang    string
	Shown   bool
}

// NamesChange is what SetNames changes. Names left out stay as they are.
type NamesChange struct {
	Names []NameChoice
	// InLists is the name shown under the item's name in lists, 0 for
	// none, or nil to leave it.
	InLists *int64
	// First is the name to show the item by, 0 to leave it.
	First int64
}

// SetNames changes how an item's names are shown, as one edit. Every name
// still recognizes scrobbles and finds duplicates, shown or not.
func (db *DB) SetNames(ctx context.Context, userID int64, kind string, id int64, ch NamesChange) (int64, error) {
	t, ok := itemTables[kind]
	if !ok {
		return 0, ErrNotFound
	}
	current, err := db.Aliases(ctx, kind, id)
	if err != nil {
		return 0, err
	}
	byID := map[int64]Alias{}
	var inLists int64
	for _, a := range current {
		byID[a.ID] = a
		if a.InLists {
			inLists = a.ID
		}
	}
	return db.editItem(ctx, userID, kind, id, func(p *plan, item string) (string, error) {
		var said []string
		for _, n := range ch.Names {
			a, ok := byID[n.AliasID]
			if !ok {
				return "", ErrNotFound
			}
			after := map[string]any{}
			if n.Lang != "" && n.Lang != a.Lang {
				if _, ok := langNames[n.Lang]; !ok {
					return "", ErrValue
				}
				after["lang"], after["lang_set"] = n.Lang, int64(1)
				said = append(said, fmt.Sprintf("marked %s as %s", a.Name, langNames[n.Lang]))
			}
			if n.Shown != a.Shown {
				after["shown"] = boolInt(n.Shown)
				if n.Shown {
					said = append(said, fmt.Sprintf("listed %s on the page", a.Name))
				} else {
					said = append(said, fmt.Sprintf("stopped listing %s on the page", a.Name))
				}
			}
			if len(after) > 0 {
				if err := p.update(t.alias, a.ID, after); err != nil {
					return "", err
				}
			}
		}
		if ch.InLists != nil && *ch.InLists != inLists {
			v := any(nil)
			if *ch.InLists != 0 {
				a, ok := byID[*ch.InLists]
				if !ok {
					return "", ErrNotFound
				}
				v = a.ID
				said = append(said, fmt.Sprintf("showed %s under the main name in lists", a.Name))
			} else {
				said = append(said, "showed no other name in lists")
			}
			if err := p.update(t.entity, id, map[string]any{"second_alias": v, "second_set": int64(1)}); err != nil {
				return "", err
			}
		}
		if ch.First != 0 && ch.First != current[0].ID {
			a, ok := byID[ch.First]
			if !ok {
				return "", ErrNotFound
			}
			if err := p.update(t.entity, id, map[string]any{"pinned_alias": a.ID}); err != nil {
				return "", err
			}
			said = append(said, "made "+a.Name+" the main name")
		}
		if len(said) == 0 {
			return "", nil
		}
		if len(said) > 3 {
			return fmt.Sprintf("Changed how the names of %s are shown", item), nil
		}
		return fmt.Sprintf("On %s, %s", item, strings.Join(said, ", ")), nil
	})
}

// PinName shows one of an item's names first.
func (db *DB) PinName(ctx context.Context, userID int64, kind string, id, aliasID int64) (int64, error) {
	t := itemTables[kind]
	return db.editItem(ctx, userID, kind, id, func(p *plan, item string) (string, error) {
		row, err := p.aliasRow(kind, id, aliasID)
		if err != nil {
			return "", err
		}
		if err := p.update(t.entity, id, map[string]any{"pinned_alias": aliasID}); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s is now shown as %s", item, row["name"]), nil
	})
}

// SetLabel puts a label on an item or takes it off.
func (db *DB) SetLabel(ctx context.Context, userID int64, kind string, id, labelID int64, on bool) (int64, error) {
	return db.editItem(ctx, userID, kind, id, func(p *plan, item string) (string, error) {
		var label string
		err := p.tx.QueryRowContext(p.ctx, `SELECT name FROM labels WHERE id = ? AND user_id = ?`, labelID, p.userID).Scan(&label)
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		if err != nil {
			return "", err
		}
		row := map[string]any{"label_id": labelID, "entity_type": kind, "entity_id": id}
		there, err := p.exists("entity_labels", row)
		if err != nil || there == on {
			return "", err
		}
		if on {
			p.add(Change{Op: OpInsert, Table: "entity_labels", After: row})
			return fmt.Sprintf("Labelled %s %s", item, label), nil
		}
		p.add(Change{Op: OpDelete, Table: "entity_labels", Key: row})
		return fmt.Sprintf("Took the label %s off %s", label, item), nil
	})
}

// ItemLabels lists the labels on an item, in the owner's order.
func (db *DB) ItemLabels(ctx context.Context, kind string, id int64) ([]Label, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT l.id, l.name, l.hide_default FROM entity_labels el JOIN labels l ON l.id = el.label_id
		WHERE el.entity_type = ? AND el.entity_id = ? ORDER BY l.position, l.name`, kind, id)
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

var artistKinds = map[string]string{"person": "a person", "group": "a group", "other": "neither a person nor a group"}

// SetArtistKind says whether an artist is a person or a group.
func (db *DB) SetArtistKind(ctx context.Context, userID, id int64, kind string) (int64, error) {
	if _, ok := artistKinds[kind]; !ok {
		return 0, ErrValue
	}
	return db.editItem(ctx, userID, "artist", id, func(p *plan, item string) (string, error) {
		if err := p.update("artists", id, map[string]any{"kind": kind}); err != nil {
			return "", err
		}
		return fmt.Sprintf("Marked %s as %s", item, artistKinds[kind]), nil
	})
}

// ArtistKind returns person, group or other.
func (db *DB) ArtistKind(ctx context.Context, id int64) (string, error) {
	var k string
	err := db.r.QueryRowContext(ctx, `SELECT kind FROM artists WHERE id = ?`, id).Scan(&k)
	return k, err
}

// FindArtist finds one of the user's artists by name, in any script.
func (db *DB) FindArtist(ctx context.Context, userID int64, name string) (Ref, error) {
	key := names.MatchKey(name)
	if key == "" {
		return Ref{}, ErrNoArtist
	}
	refs, err := db.refs(ctx, `SELECT DISTINCT a.id, a.name, a.other_names FROM artist_aliases al JOIN artists a ON a.id = al.artist_id
		WHERE a.user_id = ? AND a.merged_into IS NULL AND (al.match_key = ? OR al.romaji_key = ?) ORDER BY a.id LIMIT 1`,
		userID, key, names.FoldLongVowels(key))
	if err != nil {
		return Ref{}, err
	}
	if len(refs) == 0 {
		return Ref{}, ErrNoArtist
	}
	return refs[0], nil
}

// SetArtistLink adds or removes an "also counts for" link (with its note)
// or a group member.
func (db *DB) SetArtistLink(ctx context.Context, userID int64, table string, from, to int64, note string, on bool) (int64, error) {
	if table != "artist_counts_for" && table != "group_members" {
		return 0, ErrValue
	}
	return db.editItem(ctx, userID, "artist", from, func(p *plan, item string) (string, error) {
		var other string
		var merged sql.NullInt64
		err := p.tx.QueryRowContext(p.ctx, `SELECT name, merged_into FROM artists WHERE id = ? AND user_id = ?`, to, p.userID).Scan(&other, &merged)
		if errors.Is(err, sql.ErrNoRows) || merged.Valid {
			return "", ErrNoArtist
		}
		if err != nil {
			return "", err
		}
		row := map[string]any{"group_id": from, "member_id": to}
		if table == "artist_counts_for" {
			row = map[string]any{"artist_id": from, "target_id": to, "note": strings.TrimSpace(note)}
		}
		there, err := p.exists(table, row)
		if err != nil {
			return "", err
		}
		if !on {
			if !there {
				return "", nil
			}
			p.add(Change{Op: OpDelete, Table: table, Key: keyOf(table, row)})
			if table == "group_members" {
				return fmt.Sprintf("Removed %s from %s's members", other, item), nil
			}
			return fmt.Sprintf("%s no longer counts for %s", item, other), nil
		}
		if there {
			if table == "artist_counts_for" {
				return fmt.Sprintf("%s counts for %s as %s", item, other, row["note"]),
					p.updateKeyed(table, keyOf(table, row), map[string]any{"note": row["note"]})
			}
			return "", nil
		}
		if loop, err := WouldLoop(p.ctx, p.tx, table, from, to); err != nil || loop {
			if err == nil {
				err = ErrLoop
			}
			return "", err
		}
		p.add(Change{Op: OpInsert, Table: table, After: row})
		if table == "group_members" {
			return fmt.Sprintf("Added %s to %s's members", other, item), nil
		}
		return fmt.Sprintf("%s now also counts for %s", item, other), nil
	})
}

// updateKeyed plans a change to columns of a row with a composite key.
func (p *plan) updateKeyed(table string, key map[string]any, after map[string]any) error {
	cols := make([]string, 0, len(after))
	for c := range after {
		cols = append(cols, c)
	}
	before, err := readRow(p.ctx, p.tx, table, key, cols)
	if err != nil || before == nil || sameValues(before, after) {
		return err
	}
	p.add(Change{Table: table, Key: key, Before: before, After: after})
	return nil
}

// ArtistLinks lists an artist's "also counts for" links with their notes.
type ArtistLink struct {
	Ref
	Note string
}

func (db *DB) CountsForLinks(ctx context.Context, id int64) ([]ArtistLink, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT a.id, a.name, a.other_names, c.note FROM artist_counts_for c JOIN artists a ON a.id = c.target_id
		WHERE c.artist_id = ? ORDER BY a.name`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ArtistLink
	for rows.Next() {
		var l ArtistLink
		if err := rows.Scan(&l.ID, &l.Name, &l.OtherNames, &l.Note); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Songs and their recordings

// SongRecording is one recording of a song, with what the song page edits.
type SongRecording struct {
	ID       int64
	Version  string
	Original bool
	OwnRow   bool // ranks on its own row even when versions are combined
	Artists  []Ref
	Listens  int
}

func (db *DB) SongRecordingDetails(ctx context.Context, userID, songID int64) ([]SongRecording, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT r.id, r.version, r.is_original, r.rank_alone,
		coalesce((SELECT sum(n) FROM listen_totals WHERE user_id = ? AND recording_id = r.id), 0)
		FROM recordings r WHERE r.song_id = ? AND r.merged_into IS NULL ORDER BY r.id`, userID, songID)
	if err != nil {
		return nil, err
	}
	var out []SongRecording
	for rows.Next() {
		var r SongRecording
		if err := rows.Scan(&r.ID, &r.Version, &r.Original, &r.OwnRow, &r.Listens); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	for i := range out {
		if out[i].Artists, err = db.CreditedArtists(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CreditedArtists lists a recording's main and featured artists as credited.
func (db *DB) CreditedArtists(ctx context.Context, recordingID int64) ([]Ref, error) {
	return db.refs(ctx, `SELECT a.id, a.name, a.other_names FROM recording_credits c JOIN artists a ON a.id = c.artist_id
		WHERE c.recording_id = ? AND c.role IN ('main', 'featured') ORDER BY c.position`, recordingID)
}

func (p *plan) songRecording(songID, recordingID int64) error {
	var n int
	err := p.tx.QueryRowContext(p.ctx, `SELECT count(*) FROM recordings WHERE id = ? AND song_id = ? AND merged_into IS NULL`, recordingID, songID).Scan(&n)
	if err == nil && n == 0 {
		err = ErrNotFound
	}
	return err
}

// SetOriginal marks which recording of a song is the original, whose
// artist a combined song row shows.
func (db *DB) SetOriginal(ctx context.Context, userID, songID, recordingID int64) (int64, error) {
	return db.editItem(ctx, userID, "song", songID, func(p *plan, item string) (string, error) {
		if err := p.songRecording(songID, recordingID); err != nil {
			return "", err
		}
		recs, err := ids(p.ctx, p.tx, `SELECT id FROM recordings WHERE song_id = ? ORDER BY id`, songID)
		if err != nil {
			return "", err
		}
		for _, r := range recs {
			v := int64(0)
			if r == recordingID {
				v = 1
			}
			if err := p.update("recordings", r, map[string]any{"is_original": v}); err != nil {
				return "", err
			}
		}
		name, err := entityName(p.ctx, p.tx, "recording", recordingID)
		if err != nil {
			return "", err
		}
		artists, _ := ids(p.ctx, p.tx, `SELECT artist_id FROM recording_credits WHERE recording_id = ? AND role = 'main' ORDER BY position`, recordingID)
		by := ""
		if len(artists) > 0 {
			by, _ = entityName(p.ctx, p.tx, "artist", artists[0])
			by = " by " + by
		}
		return fmt.Sprintf("Marked %s%s as the original", name, by), nil
	})
}

// SetVersion names a recording's version, like "TV size" or a cover.
func (db *DB) SetVersion(ctx context.Context, userID, songID, recordingID int64, version string) (int64, error) {
	version = strings.TrimSpace(version)
	if len([]rune(version)) > 100 {
		return 0, ErrValue
	}
	return db.editItem(ctx, userID, "song", songID, func(p *plan, item string) (string, error) {
		if err := p.songRecording(songID, recordingID); err != nil {
			return "", err
		}
		if err := p.update("recordings", recordingID, map[string]any{"version": version}); err != nil {
			return "", err
		}
		if version == "" {
			return fmt.Sprintf("Removed a version name of %s", item), nil
		}
		return fmt.Sprintf("Named a version of %s: %s", item, version), nil
	})
}

// SetOwnRow marks a recording to rank on its own row in Top songs even
// when versions are combined, or to count with its song again.
func (db *DB) SetOwnRow(ctx context.Context, userID, songID, recordingID int64, own bool) (int64, error) {
	return db.editItem(ctx, userID, "song", songID, func(p *plan, item string) (string, error) {
		if err := p.songRecording(songID, recordingID); err != nil {
			return "", err
		}
		if err := p.update("recordings", recordingID, map[string]any{"rank_alone": boolInt(own)}); err != nil {
			return "", err
		}
		name, err := entityName(p.ctx, p.tx, "recording", recordingID)
		if err != nil {
			return "", err
		}
		if own {
			return fmt.Sprintf("%s now ranks on its own", name), nil
		}
		return fmt.Sprintf("%s now counts with %s", name, item), nil
	})
}

// Albums

// AlbumDetails are an album's editable details.
type AlbumDetails struct {
	Kind     string
	Released string
	Context  string
}

var albumKinds = map[string]bool{"album": true, "single": true, "ep": true, "soundtrack": true, "video": true, "other": true}

func (db *DB) Album(ctx context.Context, id int64) (AlbumDetails, error) {
	var d AlbumDetails
	var rel sql.NullString
	err := db.r.QueryRowContext(ctx, `SELECT kind, released, context FROM releases WHERE id = ?`, id).Scan(&d.Kind, &rel, &d.Context)
	d.Released = rel.String
	return d, err
}

// SetAlbumDetails changes an album's kind, release date and the note that
// tells same-named albums apart.
func (db *DB) SetAlbumDetails(ctx context.Context, userID, id int64, d AlbumDetails) (int64, error) {
	d.Released, d.Context = strings.TrimSpace(d.Released), strings.TrimSpace(d.Context)
	if !albumKinds[d.Kind] || !validDate(d.Released) || len([]rune(d.Context)) > 100 {
		return 0, ErrValue
	}
	return db.editItem(ctx, userID, "release", id, func(p *plan, item string) (string, error) {
		var released any
		if d.Released != "" {
			released = d.Released
		}
		if err := p.update("releases", id, map[string]any{"kind": d.Kind, "released": released, "context": d.Context}); err != nil {
			return "", err
		}
		return fmt.Sprintf("Changed the details of %s", item), nil
	})
}

// validDate accepts an ISO date of any precision: 2023, 2023-04 or
// 2023-04-12, or nothing.
func validDate(s string) bool {
	if s == "" {
		return true
	}
	for i, part := range strings.Split(s, "-") {
		if i > 2 || (i == 0 && len(part) != 4) || (i > 0 && len(part) != 2) {
			return false
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

// Credit overrides

// Override is "on this recording or album, listens credited to From count
// for To instead of From's members or the artists it counts for".
type Override struct {
	Scope   string // recording or release
	ScopeID int64
	From    Ref
	To      Ref
}

func (db *DB) Overrides(ctx context.Context, scope string, scopeIDs []int64) ([]Override, error) {
	var out []Override
	for _, sid := range scopeIDs {
		rows, err := db.r.QueryContext(ctx, `SELECT f.id, f.name, f.other_names, t.id, t.name, t.other_names FROM credit_overrides o
			JOIN artists f ON f.id = o.from_id JOIN artists t ON t.id = o.to_id
			WHERE o.scope = ? AND o.scope_id = ? ORDER BY f.name, t.name`, scope, sid)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			o := Override{Scope: scope, ScopeID: sid}
			if err := rows.Scan(&o.From.ID, &o.From.Name, &o.From.OtherNames, &o.To.ID, &o.To.Name, &o.To.OtherNames); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, o)
		}
		rows.Close()
	}
	return out, nil
}

// SetOverride adds or removes a credit override. kind and id are the page
// it's made from: a song (scope recording, one of its recordings) or an
// album (scope release, the album itself).
func (db *DB) SetOverride(ctx context.Context, userID int64, kind string, id int64, o Override, on bool) (int64, error) {
	return db.editItem(ctx, userID, kind, id, func(p *plan, item string) (string, error) {
		switch {
		case kind == "song" && o.Scope == "recording":
			if err := p.songRecording(id, o.ScopeID); err != nil {
				return "", err
			}
		case kind == "release" && o.Scope == "release" && o.ScopeID == id:
		default:
			return "", ErrValue
		}
		if o.From.ID == o.To.ID {
			return "", ErrLoop
		}
		var from, to string
		for _, a := range []struct {
			id   int64
			name *string
		}{{o.From.ID, &from}, {o.To.ID, &to}} {
			err := p.tx.QueryRowContext(p.ctx, `SELECT name FROM artists WHERE id = ? AND user_id = ? AND merged_into IS NULL`, a.id, p.userID).Scan(a.name)
			if errors.Is(err, sql.ErrNoRows) {
				return "", ErrNoArtist
			}
			if err != nil {
				return "", err
			}
		}
		row := map[string]any{"scope": o.Scope, "scope_id": o.ScopeID, "from_id": o.From.ID, "to_id": o.To.ID}
		there, err := p.exists("credit_overrides", row)
		if err != nil || there == on {
			return "", err
		}
		if on {
			p.add(Change{Op: OpInsert, Table: "credit_overrides", After: row})
			return fmt.Sprintf("On %s, listens credited to %s now count for %s", item, from, to), nil
		}
		p.add(Change{Op: OpDelete, Table: "credit_overrides", Key: row})
		return fmt.Sprintf("On %s, listens credited to %s no longer count for %s", item, from, to), nil
	})
}

// Labels in Settings

// CreateLabel adds a label at the end of the owner's order.
func (db *DB) CreateLabel(ctx context.Context, userID int64, name string, hideDefault bool) (int64, error) {
	name, err := cleanName(name)
	if err != nil {
		return 0, err
	}
	var editID int64
	err = db.Write(ctx, func(tx *sql.Tx) error {
		var clash int
		tx.QueryRowContext(ctx, `SELECT count(*) FROM labels WHERE user_id = ? AND name = ?`, userID, name).Scan(&clash)
		if clash > 0 {
			return ErrLabelName
		}
		var pos int64
		tx.QueryRowContext(ctx, `SELECT coalesce(max(position), -1) + 1 FROM labels WHERE user_id = ?`, userID).Scan(&pos)
		p := &plan{ctx: ctx, tx: tx, userID: userID}
		id, err := p.newID("labels")
		if err != nil {
			return err
		}
		p.add(Change{Op: OpInsert, Table: "labels", After: map[string]any{"id": id, "user_id": userID, "name": name,
			"hide_default": boolInt(hideDefault), "position": pos}})
		editID, err = p.apply(EditMeta{Kind: "label", Summary: "Created label " + name})
		return err
	})
	return editID, err
}

// MoveLabel moves a label one place up (-1) or down (1) in the order.
func (db *DB) MoveLabel(ctx context.Context, userID, labelID int64, by int) (int64, error) {
	var editID int64
	err := db.Write(ctx, func(tx *sql.Tx) error {
		rows, err := queryRows(ctx, tx, `SELECT id, name, position FROM labels WHERE user_id = ? ORDER BY position, name`, []string{"id", "name", "position"}, userID)
		if err != nil {
			return err
		}
		at := -1
		for i, r := range rows {
			if id, _ := asInt(r["id"]); id == labelID {
				at = i
			}
		}
		if at < 0 {
			return ErrNotFound
		}
		to := at + by
		if to < 0 || to >= len(rows) {
			return nil
		}
		rows[at], rows[to] = rows[to], rows[at]
		// Positions become 0, 1, 2… in the new order.
		p := &plan{ctx: ctx, tx: tx, userID: userID}
		for i, r := range rows {
			id, _ := asInt(r["id"])
			if err := p.update("labels", id, map[string]any{"position": int64(i)}); err != nil {
				return err
			}
		}
		dir := "up"
		if by > 0 {
			dir = "down"
		}
		editID, err = p.apply(EditMeta{Kind: "label", Summary: fmt.Sprintf("Moved label %s %s", rows[to]["name"], dir)})
		return err
	})
	return editID, err
}

// SearchItems finds the user's artists, songs or albums by any name, for
// picking what to merge into.
func (db *DB) SearchItems(ctx context.Context, userID int64, kind string, q string, limit int) ([]Ref, error) {
	t, ok := itemTables[kind]
	key := names.MatchKey(q)
	if !ok || key == "" {
		return nil, nil
	}
	like := "%" + escapeLike(key) + "%"
	romaji := "%" + escapeLike(names.FoldLongVowels(key)) + "%"
	// Songs in the graveyard aren't offered.
	live := "e.merged_into IS NULL"
	if kind == "song" {
		live += " AND e.buried_by IS NULL"
	}
	return db.refs(ctx, fmt.Sprintf(`SELECT e.id, e.name, e.other_names FROM %s e WHERE e.user_id = ? AND `+live+`
		AND e.id IN (SELECT %s FROM %s WHERE match_key LIKE ?2 ESCAPE '\' OR romaji_key LIKE ?3 ESCAPE '\' OR guess_key LIKE ?3 ESCAPE '\')
		ORDER BY e.name LIMIT ?4`, t.entity, t.owner, t.alias), userID, like, romaji, limit)
}

// Names from MusicBrainz

// ImportName is a name to add, with its kind ("" to guess it).
type ImportName struct {
	Name string
	Lang string
}

// ImportLink is an "also counts for" or member link to add to an artist.
type ImportLink struct {
	Table string // artist_counts_for or group_members
	To    int64
	Note  string
}

// ItemMBID returns an item's MBID, or "".
func (db *DB) ItemMBID(ctx context.Context, kind string, id int64) (string, error) {
	t, ok := itemTables[kind]
	if !ok {
		return "", ErrNotFound
	}
	var m sql.NullString
	err := db.r.QueryRowContext(ctx, `SELECT mbid FROM `+t.entity+` WHERE id = ?`, id).Scan(&m)
	return m.String, err
}

// ImportNames stores an item's MBID and adds names from MusicBrainz, as one
// edit. Existing names are never removed or renamed, but a name that's
// already there gets the kind MusicBrainz gives it. Albums without a
// release date get one.
func (db *DB) ImportNames(ctx context.Context, userID int64, kind string, id int64, mbid string, add []ImportName, links []ImportLink, released string) (int64, error) {
	t := itemTables[kind]
	return db.editItem(ctx, userID, kind, id, func(p *plan, item string) (string, error) {
		if err := p.update(t.entity, id, map[string]any{"mbid": mbid}); err != nil {
			return "", err
		}
		added := 0
		for _, n := range add {
			name, err := cleanName(n.Name)
			if err != nil {
				continue
			}
			if n.Lang != "" && langNames[n.Lang] == "" {
				return "", ErrValue
			}
			rows, err := queryRows(p.ctx, p.tx, fmt.Sprintf(`SELECT id, lang, lang_set FROM %s WHERE %s = ? AND name = ?`, t.alias, t.owner),
				[]string{"id", "lang", "lang_set"}, id, name)
			if err != nil {
				return "", err
			}
			if len(rows) > 0 {
				aid, _ := asInt(rows[0]["id"])
				if n.Lang != "" && (rows[0]["lang"] != n.Lang || rows[0]["lang_set"] != int64(1)) {
					if err := p.update(t.alias, aid, map[string]any{"lang": n.Lang, "lang_set": int64(1)}); err != nil {
						return "", err
					}
				}
				continue
			}
			lang, set := n.Lang, int64(1)
			if lang == "" {
				if lang, err = aliasLang(p.ctx, p.tx, t.alias, t.owner, id, name); err != nil {
					return "", err
				}
				set = 0
			}
			aid, err := p.newID(t.alias)
			if err != nil {
				return "", err
			}
			p.add(Change{Op: OpInsert, Table: t.alias, After: map[string]any{"id": aid, t.owner: id, "name": name, "lang": lang, "lang_set": set, "shown": int64(1)}})
			added++
		}
		for _, l := range links {
			if kind != "artist" || (l.Table != "artist_counts_for" && l.Table != "group_members") {
				return "", ErrValue
			}
			row := map[string]any{"group_id": id, "member_id": l.To}
			if l.Table == "artist_counts_for" {
				row = map[string]any{"artist_id": id, "target_id": l.To, "note": l.Note}
			}
			there, err := p.exists(l.Table, row)
			if err != nil {
				return "", err
			}
			if there {
				continue
			}
			if loop, err := WouldLoop(p.ctx, p.tx, l.Table, id, l.To); err != nil || loop {
				continue // left out rather than refusing the whole import
			}
			p.add(Change{Op: OpInsert, Table: l.Table, After: row})
			p.mark(l.Table, row, true)
		}
		if kind == "release" && released != "" && validDate(released) {
			var cur sql.NullString
			p.tx.QueryRowContext(p.ctx, `SELECT released FROM releases WHERE id = ?`, id).Scan(&cur)
			if !cur.Valid {
				if err := p.update("releases", id, map[string]any{"released": released}); err != nil {
					return "", err
				}
			}
		}
		if added == 0 {
			return "Got details for " + item + " from MusicBrainz", nil
		}
		return fmt.Sprintf("Got %s for %s from MusicBrainz", plural(added, "name"), item), nil
	})
}
