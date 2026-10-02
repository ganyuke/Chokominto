package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Merging moves everything attached to one artist, song, recording or
// release (the loser) onto another (the winner), as one edit. Nothing is
// deleted: the loser gets merged_into, so old links redirect and one Undo
// reverses the whole merge. Where the winner already has the same row (the
// same name, credit or label), the loser's copy is removed instead of
// duplicated. See docs/architecture.md, "Canonical entities".

var (
	ErrMergeSelf  = errors.New("can't merge something into itself")
	ErrMergedAway = errors.New("already merged into something else")
)

// MergeKinds are the entity kinds that can be merged, with their tables.
var MergeKinds = map[string]string{"artist": "artists", "song": "songs", "recording": "recordings", "release": "releases"}

// Merge merges loser into winner and returns the edit.
func (db *DB) Merge(ctx context.Context, userID int64, kind string, loser, winner int64) (int64, error) {
	var id int64
	err := db.Write(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = MergeTx(ctx, tx, userID, kind, loser, winner)
		return err
	})
	return id, err
}

func MergeTx(ctx context.Context, tx *sql.Tx, userID int64, kind string, loser, winner int64) (int64, error) {
	p := &plan{ctx: ctx, tx: tx, userID: userID}
	lname, err := p.mergeInto(kind, loser, winner)
	if err != nil {
		return 0, err
	}
	wname, _ := entityName(ctx, tx, kind, winner)
	return p.apply(EditMeta{Kind: "merge", Summary: fmt.Sprintf("Merged %s into %s", lname, wname)})
}

// plan collects the changes of one edit before it's applied. Changes to
// sources need the new edit's id, so they're built when it's known.
type plan struct {
	ctx     context.Context
	tx      *sql.Tx
	userID  int64
	changes []func(editID int64) Change
	// rows planned so far, by table and key, so later steps see earlier
	// ones: true = there, false = removed.
	rows    map[string]map[string]bool
	nextIDs map[string]int64 // for rows the plan adds
}

func (p *plan) add(c Change) {
	p.changes = append(p.changes, func(int64) Change { return c })
}

func (p *plan) apply(m EditMeta) (int64, error) {
	return ApplyEditTx(p.ctx, p.tx, p.userID, m, p.build)
}

func (p *plan) build(editID int64) []Change {
	out := make([]Change, len(p.changes))
	for i, f := range p.changes {
		out[i] = f(editID)
	}
	return out
}

// applyTo applies the plan's changes as part of an open edit.
func (p *plan) applyTo(e *OpenEdit) error {
	return e.Apply(p.build(e.ID)...)
}

func keyText(table string, row map[string]any) string {
	k := map[string]any{}
	for _, c := range wholeRows[table].key {
		k[c] = row[c]
	}
	b, _ := json.Marshal(k)
	return string(b)
}

// exists reports whether a row with this key is there, counting planned
// changes.
func (p *plan) exists(table string, row map[string]any) (bool, error) {
	k := keyText(table, row)
	if v, ok := p.rows[table][k]; ok {
		return v, nil
	}
	cur, err := readRow(p.ctx, p.tx, table, keyOf(table, row), wholeRows[table].key)
	return cur != nil, err
}

func (p *plan) mark(table string, row map[string]any, there bool) {
	if p.rows == nil {
		p.rows = map[string]map[string]bool{}
	}
	if p.rows[table] == nil {
		p.rows[table] = map[string]bool{}
	}
	p.rows[table][keyText(table, row)] = there
}

func keyOf(table string, row map[string]any) map[string]any {
	k := map[string]any{}
	for _, c := range wholeRows[table].key {
		k[c] = row[c]
	}
	return k
}

// query reads whole rows (the logged columns) of a table.
func (p *plan) query(table, cond string, args ...any) ([]map[string]any, error) {
	cols := wholeRows[table].cols
	if cols == nil {
		return nil, fmt.Errorf("no row shape for %s", table)
	}
	return queryRows(p.ctx, p.tx, fmt.Sprintf(`SELECT %s FROM %s WHERE %s ORDER BY %s`,
		strings.Join(cols, ", "), table, cond, strings.Join(wholeRows[table].key, ", ")), cols, args...)
}

func queryRows(ctx context.Context, tx *sql.Tx, q string, cols []string, args ...any) ([]map[string]any, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := map[string]any{}
		for i, c := range cols {
			row[c] = vals[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// rekey moves rows of a composite-key table from loser to winner in the
// given columns: each is removed and added back pointing at the winner,
// unless the winner already has that row, or the moved row would be
// dropped by skip.
func (p *plan) rekey(table string, cols []string, loser, winner int64, match map[string]any, skip func(row map[string]any) (bool, error)) error {
	conds := make([]string, len(cols))
	args := []any{}
	for i, c := range cols {
		conds[i] = c + " = ?"
		args = append(args, loser)
	}
	cond := "(" + strings.Join(conds, " OR ") + ")"
	for _, col := range slices.Sorted(maps.Keys(match)) {
		cond += " AND " + col + " = ?"
		args = append(args, match[col])
	}
	rows, err := p.query(table, cond, args...)
	if err != nil {
		return err
	}
	for _, row := range rows {
		p.add(Change{Op: OpDelete, Table: table, Key: keyOf(table, row), Before: row})
		p.mark(table, row, false)
		moved := map[string]any{}
		for k, v := range row {
			moved[k] = v
		}
		for _, c := range cols {
			if v, _ := asInt(moved[c]); v == loser {
				moved[c] = winner
			}
		}
		if skip != nil {
			if drop, err := skip(moved); err != nil || drop {
				if err != nil {
					return err
				}
				continue
			}
		}
		there, err := p.exists(table, moved)
		if err != nil {
			return err
		}
		if there {
			continue
		}
		p.add(Change{Op: OpInsert, Table: table, After: moved})
		p.mark(table, moved, true)
	}
	return nil
}

// update plans a change to columns of a row keyed by id, reading the
// current values for before.
func (p *plan) update(table string, id int64, after map[string]any) error {
	cols := make([]string, 0, len(after))
	for c := range after {
		cols = append(cols, c)
	}
	slices.Sort(cols)
	before, err := readRow(p.ctx, p.tx, table, map[string]any{"id": id}, cols)
	if err != nil {
		return err
	}
	if before == nil {
		return ErrNotFound
	}
	if sameValues(before, after) {
		return nil
	}
	p.add(Change{Table: table, ID: id, Before: before, After: after})
	return nil
}

func (p *plan) mergeInto(kind string, loser, winner int64) (string, error) {
	table, ok := MergeKinds[kind]
	if !ok {
		return "", fmt.Errorf("can't merge %q", kind)
	}
	if loser == winner {
		return "", ErrMergeSelf
	}
	for _, id := range []int64{loser, winner} {
		var merged sql.NullInt64
		err := p.tx.QueryRowContext(p.ctx, `SELECT merged_into FROM `+table+` WHERE id = ? AND user_id = ?`, id, p.userID).Scan(&merged)
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		if err != nil {
			return "", err
		}
		if merged.Valid {
			return "", ErrMergedAway
		}
	}
	name, err := entityName(p.ctx, p.tx, kind, loser)
	if err != nil {
		return "", err
	}
	switch kind {
	case "artist":
		err = p.mergeArtist(loser, winner)
	case "song":
		err = p.mergeSong(loser, winner)
	case "recording":
		err = p.mergeRecording(loser, winner)
	case "release":
		err = p.mergeRelease(loser, winner)
	}
	if err != nil {
		return "", err
	}
	if err := p.fillBlanks(table, loser, winner); err != nil {
		return "", err
	}
	if err := p.update(table, loser, map[string]any{"merged_into": winner}); err != nil {
		return "", err
	}
	a, b := min(loser, winner), max(loser, winner)
	var sug int64
	err = p.tx.QueryRowContext(p.ctx, `SELECT id FROM suggestions WHERE user_id = ? AND kind = ? AND a_id = ? AND b_id = ? AND status = 'open'`,
		p.userID, kind, a, b).Scan(&sug)
	if err == nil {
		p.add(Change{Table: "suggestions", ID: sug, Before: map[string]any{"status": "open"}, After: map[string]any{"status": "accepted"}})
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return name, nil
}

// fillBlanks copies details the winner doesn't have from the loser.
func (p *plan) fillBlanks(table string, loser, winner int64) error {
	cols := map[string][]string{
		"artists":    {"mbid", "artwork_id"},
		"songs":      {"mbid"},
		"recordings": {"mbid"},
		"releases":   {"mbid", "released", "artwork_id"},
	}[table]
	l, err := readRow(p.ctx, p.tx, table, map[string]any{"id": loser}, cols)
	if err != nil {
		return err
	}
	w, err := readRow(p.ctx, p.tx, table, map[string]any{"id": winner}, cols)
	if err != nil {
		return err
	}
	after := map[string]any{}
	for _, c := range cols {
		if w[c] == nil && l[c] != nil {
			after[c] = l[c]
		}
	}
	if len(after) == 0 {
		return nil
	}
	return p.update(table, winner, after)
}

// moveAliases gives the winner the loser's names, except ones it has.
func (p *plan) moveAliases(table string, loser, winner int64) error {
	owner := aliasTables[table].owner
	entity := aliasTables[table].entity
	var pinned sql.NullInt64
	if err := p.tx.QueryRowContext(p.ctx, `SELECT pinned_alias FROM `+entity+` WHERE id = ?`, loser).Scan(&pinned); err != nil {
		return err
	}
	if pinned.Valid {
		if err := p.update(entity, loser, map[string]any{"pinned_alias": nil}); err != nil {
			return err
		}
	}
	rows, err := p.query(table, owner+" = ?", loser)
	if err != nil {
		return err
	}
	// The winner keeps its name. If a moved name would come first in
	// display order, the winner's current name is pinned.
	var wPinned sql.NullInt64
	var wAlias int64
	var wLang string
	err = p.tx.QueryRowContext(p.ctx, fmt.Sprintf(
		`SELECT e.pinned_alias, a.id, a.lang FROM %s e JOIN %s a ON a.%s = e.id WHERE e.id = ?
		 ORDER BY (a.id = e.pinned_alias) DESC, CASE a.lang WHEN 'en' THEN 0 WHEN 'romaji' THEN 1 ELSE 2 END, a.id LIMIT 1`,
		entity, table, owner), winner).Scan(&wPinned, &wAlias, &wLang)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && !wPinned.Valid {
		for _, row := range rows {
			id, _ := asInt(row["id"])
			l, _ := row["lang"].(string)
			if r, wr := langRank(l), langRank(wLang); r < wr || r == wr && id < wAlias {
				if err := p.update(entity, winner, map[string]any{"pinned_alias": wAlias}); err != nil {
					return err
				}
				break
			}
		}
	}
	for _, row := range rows {
		var dup int
		if err := p.tx.QueryRowContext(p.ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s = ? AND name = ?`, table, owner), winner, row["name"]).Scan(&dup); err != nil {
			return err
		}
		id, _ := asInt(row["id"])
		if dup > 0 {
			p.add(Change{Op: OpDelete, Table: table, ID: id, Before: row})
			continue
		}
		p.add(Change{Table: table, ID: id, Before: map[string]any{owner: loser}, After: map[string]any{owner: winner}})
	}
	return nil
}

func (p *plan) moveLabels(entityType string, loser, winner int64) error {
	return p.rekey("entity_labels", []string{"entity_id"}, loser, winner, map[string]any{"entity_type": entityType}, nil)
}

// moveLinks points rows keyed by id (sources, rules) at the winner.
func (p *plan) moveLinks(table, col string, loser, winner int64, relink bool) error {
	rs, err := ids(p.ctx, p.tx, fmt.Sprintf(`SELECT id FROM %s WHERE %s = ? ORDER BY id`, table, col), loser)
	if err != nil {
		return err
	}
	for _, id := range rs {
		if !relink {
			p.add(Change{Table: table, ID: id, Before: map[string]any{col: loser}, After: map[string]any{col: winner}})
			continue
		}
		// Text moved by the owner counts as linked by the owner, so a
		// reparse never moves it back.
		before := map[string]any{col: loser, "linked_by": currentLinkedBy(p.ctx, p.tx, id)}
		p.changes = append(p.changes, func(editID int64) Change {
			return Change{Table: table, ID: id, Before: before, After: map[string]any{col: winner, "linked_by": editID}}
		})
	}
	return nil
}

// loopGraph is a user's "also counts for" or group member links, kept up to
// date as a merge plans its changes, so moved links that would make a loop
// are dropped.
type loopGraph map[int64][]int64

func (p *plan) loadGraph(q string) (loopGraph, error) {
	rows, err := p.tx.QueryContext(p.ctx, q, p.userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	g := loopGraph{}
	for rows.Next() {
		var a, b int64
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		g[a] = append(g[a], b)
	}
	return g, rows.Err()
}

func (g loopGraph) reaches(from, to int64) bool {
	seen := map[int64]bool{}
	queue := []int64{from}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if n == to {
			return true
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		queue = append(queue, g[n]...)
	}
	return false
}

// moveGraph rekeys a link table, dropping links that would point at
// themselves or make a loop.
func (p *plan) moveGraph(table, fromCol, toCol, q string, loser, winner int64) error {
	g, err := p.loadGraph(q)
	if err != nil {
		return err
	}
	// Take the loser's links out first, then add each back as the winner's.
	delete(g, loser)
	for a := range g {
		g[a] = slices.DeleteFunc(g[a], func(b int64) bool { return b == loser })
	}
	return p.rekey(table, []string{fromCol, toCol}, loser, winner, nil, func(row map[string]any) (bool, error) {
		a, _ := asInt(row[fromCol])
		b, _ := asInt(row[toCol])
		if a == b || g.reaches(b, a) {
			return true, nil
		}
		g[a] = append(g[a], b)
		return false, nil
	})
}

func (p *plan) mergeArtist(loser, winner int64) error {
	if err := p.moveAliases("artist_aliases", loser, winner); err != nil {
		return err
	}
	if err := p.moveGraph("group_members", "group_id", "member_id",
		`SELECT g.group_id, g.member_id FROM group_members g JOIN artists a ON a.id = g.group_id WHERE a.user_id = ?`, loser, winner); err != nil {
		return err
	}
	if err := p.moveGraph("artist_counts_for", "artist_id", "target_id",
		`SELECT c.artist_id, c.target_id FROM artist_counts_for c JOIN artists a ON a.id = c.artist_id WHERE a.user_id = ?`, loser, winner); err != nil {
		return err
	}
	if err := p.rekey("recording_credits", []string{"artist_id"}, loser, winner, nil, nil); err != nil {
		return err
	}
	if err := p.rekey("release_credits", []string{"artist_id"}, loser, winner, nil, nil); err != nil {
		return err
	}
	if err := p.rekey("credit_overrides", []string{"from_id", "to_id"}, loser, winner, nil, func(row map[string]any) (bool, error) {
		return row["from_id"] == row["to_id"], nil
	}); err != nil {
		return err
	}
	return p.moveLabels("artist", loser, winner)
}

func (p *plan) mergeSong(loser, winner int64) error {
	if err := p.moveAliases("song_aliases", loser, winner); err != nil {
		return err
	}
	var winnerHasOriginal int
	if err := p.tx.QueryRowContext(p.ctx, `SELECT count(*) FROM recordings WHERE song_id = ? AND is_original = 1 AND merged_into IS NULL`, winner).
		Scan(&winnerHasOriginal); err != nil {
		return err
	}
	rows, err := queryRows(p.ctx, p.tx, `SELECT id, is_original FROM recordings WHERE song_id = ? ORDER BY id`, []string{"id", "is_original"}, loser)
	if err != nil {
		return err
	}
	for _, r := range rows {
		id, _ := asInt(r["id"])
		after := map[string]any{"song_id": winner}
		// The winner's original stays the original.
		if orig, _ := asInt(r["is_original"]); orig == 1 && winnerHasOriginal > 0 {
			after["is_original"] = int64(0)
		}
		if err := p.update("recordings", id, after); err != nil {
			return err
		}
	}
	return p.moveLabels("song", loser, winner)
}

func (p *plan) mergeRecording(loser, winner int64) error {
	if err := p.moveLinks("sources", "recording_id", loser, winner, true); err != nil {
		return err
	}
	if err := p.rekey("release_tracks", []string{"recording_id"}, loser, winner, nil, nil); err != nil {
		return err
	}
	if err := p.rekey("recording_credits", []string{"recording_id"}, loser, winner, nil, nil); err != nil {
		return err
	}
	if err := p.rekey("credit_overrides", []string{"scope_id"}, loser, winner, map[string]any{"scope": "recording"}, nil); err != nil {
		return err
	}
	if err := p.moveLinks("rules", "recording_id", loser, winner, false); err != nil {
		return err
	}
	// Two recordings of different songs being the same recording means the
	// songs are the same too, if the loser's song has nothing else in it.
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
	if others > 0 {
		return nil
	}
	if err := p.mergeSong(ls, ws); err != nil {
		return err
	}
	if err := p.fillBlanks("songs", ls, ws); err != nil {
		return err
	}
	return p.update("songs", ls, map[string]any{"merged_into": ws})
}

func (p *plan) mergeRelease(loser, winner int64) error {
	if err := p.moveAliases("release_aliases", loser, winner); err != nil {
		return err
	}
	if err := p.rekey("release_credits", []string{"release_id"}, loser, winner, nil, nil); err != nil {
		return err
	}
	if err := p.rekey("release_tracks", []string{"release_id"}, loser, winner, nil, nil); err != nil {
		return err
	}
	if err := p.moveLinks("sources", "release_id", loser, winner, true); err != nil {
		return err
	}
	if err := p.rekey("credit_overrides", []string{"scope_id"}, loser, winner, map[string]any{"scope": "release"}, nil); err != nil {
		return err
	}
	if err := p.moveLinks("rules", "release_id", loser, winner, false); err != nil {
		return err
	}
	return p.moveLabels("release", loser, winner)
}

// entityName is how an entity is named in edit summaries.
func entityName(ctx context.Context, tx *sql.Tx, kind string, id int64) (string, error) {
	var name string
	var err error
	if kind == "recording" {
		var version string
		err = tx.QueryRowContext(ctx, `SELECT s.name, r.version FROM recordings r JOIN songs s ON s.id = r.song_id WHERE r.id = ?`, id).Scan(&name, &version)
		if version != "" {
			name += " (" + version + ")"
		}
	} else {
		err = tx.QueryRowContext(ctx, `SELECT name FROM `+MergeKinds[kind]+` WHERE id = ?`, id).Scan(&name)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return name, err
}

// langRank is the display order of names: English, romaji, original script.
func langRank(lang string) int {
	switch lang {
	case "en":
		return 0
	case "romaji":
		return 1
	}
	return 2
}
