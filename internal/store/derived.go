package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"

	"chokominto/internal/names"
)

// Derived data follows the logged rows: listens copy their source's link,
// alias search keys come from the name, entities cache their display
// names, and recording_artists expands credits. None of it is logged. It's
// recalculated after every change, and so after every undo.

// derivedWork collects what an edit made stale, so a merge touching
// hundreds of rows recalculates each recording and name once, at the end.
type derivedWork struct {
	recordings map[int64]bool
	names      map[string]map[int64]bool // alias table -> owner ids
}

func newDerivedWork() *derivedWork {
	return &derivedWork{recordings: map[int64]bool{}, names: map[string]map[int64]bool{}}
}

func (w *derivedWork) name(table string, owner any) {
	id, ok := asInt(owner)
	if !ok {
		return
	}
	if w.names[table] == nil {
		w.names[table] = map[int64]bool{}
	}
	w.names[table][id] = true
}

func (w *derivedWork) recordingsOf(ctx context.Context, tx *sql.Tx, q string, args ...any) error {
	rs, err := ids(ctx, tx, q, args...)
	for _, r := range rs {
		w.recordings[r] = true
	}
	return err
}

func (w *derivedWork) flush(ctx context.Context, tx *sql.Tx) error {
	for _, table := range slices.Sorted(maps.Keys(w.names)) {
		for _, id := range slices.Sorted(maps.Keys(w.names[table])) {
			if err := refreshNamesTx(ctx, tx, table, id); err != nil {
				return err
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(w.recordings)) {
		if err := RebuildRecordingArtistsTx(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}

// aliasKeys returns an alias's search keys, NULL where there's none.
func aliasKeys(name string) (match string, romaji, guess any) {
	if k := names.RomajiKey(name); k != "" {
		romaji = k
	}
	if k := names.GuessKey(name); k != "" {
		guess = k
	}
	return names.MatchKey(name), romaji, guess
}

// derivedValues fills in the derived columns of a new row.
func derivedValues(table string, row map[string]any) map[string]any {
	if _, ok := aliasTables[table]; !ok {
		return nil
	}
	name, _ := row["name"].(string)
	m, r, g := aliasKeys(name)
	return map[string]any{"match_key": m, "romaji_key": r, "guess_key": g}
}

var entityAliasTable = map[string]string{"artists": "artist_aliases", "songs": "song_aliases", "releases": "release_aliases"}

// afterChange keeps derived data in step with one changed row.
func afterChange(ctx context.Context, tx *sql.Tx, w *derivedWork, c Change, key map[string]any) error {
	// The row's values after the change, falling back to before for a
	// removed row or columns the change didn't touch.
	val := func(col string) any {
		if v, ok := c.After[col]; ok {
			return v
		}
		if v, ok := c.Before[col]; ok {
			return v
		}
		return key[col]
	}
	switch t := c.Table; t {
	case "sources":
		id := key["id"]
		// Only listens whose link changes. Rewriting the rest to the same
		// values still runs the count triggers for every listen, which on a
		// reparse is nearly all of them. A listen linked on its own keeps
		// its link.
		_, err := tx.ExecContext(ctx,
			`UPDATE listens SET (recording_id, release_id) = (SELECT recording_id, release_id FROM sources WHERE id = ?1)
			 WHERE source_id = ?1 AND fixed_by IS NULL AND (recording_id, release_id) IS NOT (SELECT recording_id, release_id FROM sources WHERE id = ?1)`,
			id)
		return err

	case "artist_aliases", "song_aliases", "release_aliases":
		owner := aliasTables[t].owner
		w.name(t, c.Before[owner])
		w.name(t, c.After[owner])
		if c.op() == OpUpdate {
			var cur int64
			if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE id = ?`, owner, t), key["id"]).Scan(&cur); err != nil {
				return err
			}
			w.name(t, cur)
			if name, ok := c.After["name"].(string); ok {
				m, r, g := aliasKeys(name)
				if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET match_key = ?, romaji_key = ?, guess_key = ? WHERE id = ?`, t),
					m, r, g, key["id"]); err != nil {
					return err
				}
			}
		}

	case "artists", "songs", "releases":
		for _, col := range []string{"pinned_alias", "second_alias", "second_set"} {
			if _, ok := c.After[col]; ok && c.op() == OpUpdate {
				w.name(entityAliasTable[t], key["id"])
			}
		}
		// Listens that arrived while a song was in the graveyard were hidden
		// with it but not logged, so bringing it back by undo finds them here.
		if before, ok := c.Before["buried_by"]; ok && before != nil && c.After["buried_by"] == nil && c.op() == OpUpdate {
			if _, err := tx.ExecContext(ctx, `UPDATE listens SET deleted_by = NULL WHERE deleted_by = ?`, before); err != nil {
				return err
			}
		}
		if _, ok := c.After["kind"]; ok && t == "artists" {
			return w.recordingsOf(ctx, tx, `SELECT recording_id FROM recording_artists WHERE artist_id = ?`, key["id"])
		}

	case "recording_credits", "release_tracks":
		if id, ok := asInt(val("recording_id")); ok {
			w.recordings[id] = true
		}

	case "group_members":
		if c.op() == OpInsert {
			if err := refuseLoop(ctx, tx, `SELECT member_id FROM group_members WHERE group_id = ?`, key["group_id"], key["member_id"]); err != nil {
				return err
			}
		}
		return w.recordingsOf(ctx, tx, `SELECT recording_id FROM recording_artists WHERE artist_id = ?`, key["group_id"])

	case "artist_counts_for":
		if c.op() == OpInsert {
			if err := refuseLoop(ctx, tx, `SELECT target_id FROM artist_counts_for WHERE artist_id = ?`, key["artist_id"], key["target_id"]); err != nil {
				return err
			}
		}
		if c.op() == OpUpdate {
			return nil // only the note changed
		}
		return w.recordingsOf(ctx, tx, `SELECT recording_id FROM recording_artists WHERE artist_id = ?`, key["artist_id"])

	case "credit_overrides":
		if key["scope"] == "recording" {
			if id, ok := asInt(key["scope_id"]); ok {
				w.recordings[id] = true
			}
			return nil
		}
		return w.recordingsOf(ctx, tx, `SELECT recording_id FROM release_tracks WHERE release_id = ?`, key["scope_id"])
	}
	return nil
}

// beforeDelete clears derived rows that point at a row about to be removed.
// Edits made by a rule stop pointing at it, since they only record where a
// link came from.
func beforeDelete(ctx context.Context, tx *sql.Tx, table string, key map[string]any) error {
	var err error
	switch table {
	case "recordings":
		_, err = tx.ExecContext(ctx, `DELETE FROM recording_artists WHERE recording_id = ?`, key["id"])
	case "artists":
		// Only ever left from credits the same edit also takes away, whose
		// recordings are expanded again at the end.
		_, err = tx.ExecContext(ctx, `DELETE FROM recording_artists WHERE artist_id = ?`, key["id"])
	case "rules":
		_, err = tx.ExecContext(ctx, `UPDATE edits SET rule_id = NULL WHERE rule_id = ?`, key["id"])
	}
	return err
}

// refuseLoop fails with ErrStale when the link from -> to just added lets
// to reach back to from. Saving a loop is refused with a message before
// it gets here. This catches undo and redo bringing back a link that a
// later edit made into a loop.
func refuseLoop(ctx context.Context, tx *sql.Tx, next string, from, to any) error {
	seen := map[int64]bool{}
	queue := []any{to}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		n, _ := asInt(id)
		if f, _ := asInt(from); n == f {
			return fmt.Errorf("link would loop: %w", ErrStale)
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		more, err := ids(ctx, tx, next, n)
		if err != nil {
			return err
		}
		for _, m := range more {
			queue = append(queue, m)
		}
	}
	return nil
}

// WouldLoop reports whether linking from -> to would make a loop, for
// "also counts for" (table artist_counts_for) or group members.
func WouldLoop(ctx context.Context, tx *sql.Tx, table string, from, to int64) (bool, error) {
	next := map[string]string{
		"artist_counts_for": `SELECT target_id FROM artist_counts_for WHERE artist_id = ?`,
		"group_members":     `SELECT member_id FROM group_members WHERE group_id = ?`,
	}[table]
	if from == to {
		return true, nil
	}
	err := refuseLoop(ctx, tx, next, from, to)
	if errors.Is(err, ErrStale) {
		return true, nil
	}
	return false, err
}
