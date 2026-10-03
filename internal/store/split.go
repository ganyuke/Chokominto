package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

var ErrSplitAll = errors.New("choose some of the recordings, not all of them")

// newID picks the id for a row the plan adds, after any ids it has already
// picked for that table.
func (p *plan) newID(table string) (int64, error) {
	if p.nextIDs == nil {
		p.nextIDs = map[string]int64{}
	}
	if id, ok := p.nextIDs[table]; ok {
		p.nextIDs[table] = id + 1
		return id, nil
	}
	var id int64
	if err := p.tx.QueryRowContext(p.ctx, `SELECT coalesce(max(id), 0) + 1 FROM `+table).Scan(&id); err != nil {
		return 0, err
	}
	p.nextIDs[table] = id + 1
	return id, nil
}

// newSong plans a new song with one name, and returns its id.
func (p *plan) newSong(name, lang string, langSet int64) (int64, error) {
	id, err := p.newID("songs")
	if err != nil {
		return 0, err
	}
	aliasID, err := p.newID("song_aliases")
	if err != nil {
		return 0, err
	}
	p.add(Change{Op: OpInsert, Table: "songs", After: map[string]any{
		"id": id, "user_id": p.userID, "name": name, "other_names": "", "pinned_alias": nil, "mbid": nil, "merged_into": nil, "created_at": unix(),
		"second_alias": nil, "second_set": int64(0), "byline": "", "buried_by": nil, "artwork_id": nil, "artwork_pinned": int64(0)}})
	p.add(Change{Op: OpInsert, Table: "song_aliases", After: map[string]any{
		"id": aliasID, "song_id": id, "name": name, "lang": lang, "lang_set": langSet, "shown": int64(1)}})
	return id, nil
}

// SplitSong moves some of a song's recordings to a new song with the same
// name, for two songs that only share a title. It returns the new song.
func (db *DB) SplitSong(ctx context.Context, userID, songID int64, recordingIDs []int64) (int64, int64, error) {
	var newSong, editID int64
	err := db.Write(ctx, func(tx *sql.Tx) error {
		p := &plan{ctx: ctx, tx: tx, userID: userID}
		var name string
		var merged, buried sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT name, merged_into, buried_by FROM songs WHERE id = ? AND user_id = ?`, songID, userID).Scan(&name, &merged, &buried)
		if errors.Is(err, sql.ErrNoRows) || merged.Valid {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if buried.Valid {
			return ErrBuried
		}
		all, err := ids(ctx, tx, `SELECT id FROM recordings WHERE song_id = ? AND merged_into IS NULL ORDER BY id`, songID)
		if err != nil {
			return err
		}
		move := slices.Sorted(slices.Values(recordingIDs))
		move = slices.Compact(move)
		if len(move) == 0 || len(move) >= len(all) {
			return ErrSplitAll
		}
		for _, r := range move {
			if !slices.Contains(all, r) {
				return ErrNotFound
			}
		}
		var lang string
		var langSet int64
		if err := tx.QueryRowContext(ctx, `SELECT lang, lang_set FROM song_aliases WHERE song_id = ? AND name = ?`, songID, name).
			Scan(&lang, &langSet); err != nil {
			return err
		}
		if newSong, err = p.newSong(name, lang, langSet); err != nil {
			return err
		}
		for _, r := range move {
			if err := p.update("recordings", r, map[string]any{"song_id": newSong, "is_original": int64(0)}); err != nil {
				return err
			}
		}
		editID, err = p.apply(EditMeta{Kind: "split", Summary: fmt.Sprintf("Split %s into two songs", name)})
		return err
	})
	return newSong, editID, err
}
