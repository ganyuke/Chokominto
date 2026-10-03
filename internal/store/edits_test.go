package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// Whole-row changes must log every column, or an undo can't put a row back
// as it was.
func TestWholeRowColumns(t *testing.T) {
	db := openTest(t)
	for table, shape := range wholeRows {
		rows, err := db.r.Query(`SELECT name, type, pk FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		var cols []string
		key := map[int]string{}
		for rows.Next() {
			var name, typ string
			var pk int
			rows.Scan(&name, &typ, &pk)
			cols = append(cols, name)
			if pk > 0 {
				key[pk] = name
			}
			if strings.EqualFold(typ, "blob") {
				t.Errorf("%s.%s is a BLOB, which the log can't hold", table, name)
			}
		}
		rows.Close()
		if all := append(slices.Clone(shape.cols), shape.derived...); !slices.Equal(slices.Sorted(slices.Values(cols)), slices.Sorted(slices.Values(all))) {
			t.Errorf("%s has columns %v, wholeRows lists %v", table, cols, all)
		}
		var pk []string
		for i := 1; i <= len(key); i++ {
			pk = append(pk, key[i])
		}
		if !slices.Equal(slices.Sorted(slices.Values(pk)), slices.Sorted(slices.Values(shape.key))) {
			t.Errorf("%s has primary key %v, wholeRows lists %v", table, pk, shape.key)
		}
	}
}

func addLabel(t *testing.T, db *DB, u int64, name string) int64 {
	t.Helper()
	var id int64
	err := db.Write(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow(`INSERT INTO labels (user_id, name) VALUES (?, ?) RETURNING id`, u, name).Scan(&id)
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func tagKey(label int64, typ string, id int64) map[string]any {
	return map[string]any{"label_id": label, "entity_type": typ, "entity_id": id}
}

func TestDeleteLabelAndUndo(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := testUser(t, db)
	voc := addLabel(t, db, u, "Vocaloid")
	other := addLabel(t, db, u, "Game")
	tag, err := db.ApplyEdit(ctx, u, "label", "tag", func(int64) []Change {
		return []Change{
			{Op: OpInsert, Table: "entity_labels", After: tagKey(voc, "artist", 7)},
			{Op: OpInsert, Table: "entity_labels", After: tagKey(voc, "song", 3)},
			{Op: OpInsert, Table: "entity_labels", After: tagKey(other, "song", 3)},
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, db)

	if _, err := db.DeleteLabel(ctx, u, voc); err != nil {
		t.Fatal(err)
	}
	ls, _ := db.Labels(ctx, u)
	if len(ls) != 2 || ls[0].Name != "Character" || ls[1].Name != "Game" || ls[1].Uses != 1 {
		t.Fatalf("after delete %+v", ls)
	}
	es, _ := db.Edits(ctx, u, 0, 1)
	if es[0].Summary != "Deleted label Vocaloid, used on 2" {
		t.Fatal(es[0].Summary)
	}
	if _, err := db.DeleteLabel(ctx, u, voc); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	// The tagging edit can't be undone while its label is gone.
	var ce *ConflictError
	if _, err := db.UndoEdit(ctx, u, tag); !errors.As(err, &ce) || !slices.Equal(ce.Later, []int64{es[0].ID}) {
		t.Fatalf("undo tag: %v", err)
	}

	undo, err := db.UndoEdit(ctx, u, es[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, db); got != before {
		t.Fatalf("after undo\n%s\nwant\n%s", got, before)
	}
	// Redo deletes it again.
	if _, err := db.UndoEdit(ctx, u, undo); err != nil {
		t.Fatal(err)
	}
	if ls, _ := db.Labels(ctx, u); len(ls) != 2 {
		t.Fatalf("after redo %+v", ls)
	}
}

func TestWholeRowStale(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := testUser(t, db)
	ls, _ := db.Labels(ctx, u)
	char := ls[0].ID
	insert := func() error {
		_, err := db.ApplyEdit(ctx, u, "label", "tag", func(int64) []Change {
			return []Change{{Op: OpInsert, Table: "entity_labels", After: tagKey(char, "artist", 1)}}
		})
		return err
	}
	if err := insert(); err != nil {
		t.Fatal(err)
	}
	if err := insert(); !errors.Is(err, ErrStale) {
		t.Fatalf("adding a row that's there: %v", err)
	}
	remove := func(before map[string]any) error {
		_, err := db.ApplyEdit(ctx, u, "label", "untag", func(int64) []Change {
			return []Change{{Op: OpDelete, Table: "entity_labels", Key: tagKey(char, "artist", 1), Before: before}}
		})
		return err
	}
	if err := remove(tagKey(char, "artist", 2)); !errors.Is(err, ErrStale) {
		t.Fatalf("removing a row that doesn't match: %v", err)
	}
	if err := remove(nil); err != nil {
		t.Fatal(err)
	}
	if err := remove(nil); !errors.Is(err, ErrStale) {
		t.Fatalf("removing a row that's gone: %v", err)
	}
	// A label with the same name taken again can't come back.
	if _, err := db.DeleteLabel(ctx, u, char); err != nil {
		t.Fatal(err)
	}
	es, _ := db.Edits(ctx, u, 0, 1)
	addLabel(t, db, u, "Character")
	if _, err := db.UndoEdit(ctx, u, es[0].ID); !errors.Is(err, ErrStale) {
		t.Fatalf("undo onto a taken name: %v", err)
	}
	// Tables and keys outside the whitelist are refused.
	for _, c := range []Change{
		{Op: OpDelete, Table: "users", ID: u},
		{Op: OpInsert, Table: "users", After: map[string]any{"id": 9}},
		{Op: OpInsert, Table: "entity_labels", After: map[string]any{"label_id": char, "entity_type": "artist", "entity_id": 1, "x": 1}},
		{Op: OpDelete, Table: "entity_labels", Key: map[string]any{"label_id": char}},
		{Op: OpDelete, Table: "entity_labels", Key: map[string]any{"label_id": char, "entity_type": "artist", "entity_id; DROP TABLE labels": 1}},
	} {
		if _, err := db.ApplyEdit(ctx, u, "x", "x", func(int64) []Change { return []Change{c} }); err == nil {
			t.Errorf("%+v was allowed", c)
		}
	}
}

// snapshot dumps the rows edits can change, and everything derived from
// them.
func snapshot(t *testing.T, db *DB) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT id, user_id, name, hide_default, position FROM labels ORDER BY id`,
		`SELECT label_id, entity_type, entity_id FROM entity_labels ORDER BY 1, 2, 3`,
		`SELECT id, deleted_by IS NULL, recording_id, release_id, fixed_by FROM listens ORDER BY id`,
		`SELECT user_id, listen_count FROM user_stats ORDER BY 1`,
		`SELECT id, recording_id, release_id, linked_by FROM sources ORDER BY id`,
		`SELECT id, kind, name, other_names, pinned_alias, mbid, merged_into FROM artists ORDER BY id`,
		`SELECT id, name, other_names, pinned_alias, mbid, merged_into FROM songs ORDER BY id`,
		`SELECT id, song_id, version, is_original, rank_alone, mbid, merged_into FROM recordings ORDER BY id`,
		`SELECT id, name, other_names, pinned_alias, kind, context, released, mbid, merged_into FROM releases ORDER BY id`,
		`SELECT * FROM artist_aliases ORDER BY id`,
		`SELECT * FROM song_aliases ORDER BY id`,
		`SELECT * FROM release_aliases ORDER BY id`,
		`SELECT * FROM recording_credits ORDER BY 1, 2, 3`,
		`SELECT * FROM release_credits ORDER BY 1, 2`,
		`SELECT * FROM release_tracks ORDER BY 1, 2`,
		`SELECT * FROM group_members ORDER BY 1, 2`,
		`SELECT * FROM artist_counts_for ORDER BY 1, 2`,
		`SELECT * FROM credit_overrides ORDER BY 1, 2, 3, 4`,
		`SELECT * FROM recording_artists ORDER BY 1, 2`,
		`SELECT * FROM listen_totals ORDER BY 1, 2, 3`,
		`SELECT * FROM listen_years ORDER BY 1, 2, 3, 4`,
		`SELECT id, recording_id, release_id, enabled FROM rules ORDER BY id`,
		`SELECT id, status FROM suggestions ORDER BY id`,
	} {
		rows, err := db.r.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rows.Scan(ptrs...)
			fmt.Fprintln(&b, vals...)
		}
		rows.Close()
		b.WriteString("--\n")
	}
	return b.String()
}

// Random edits and undos, some refused, and then rewinding the log, must
// leave everything as it started.
func TestUndoProperty(t *testing.T) {
	for seed := range uint64(20) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { undoProperty(t, seed) })
	}
}

func undoProperty(t *testing.T, seed uint64) {
	ctx := context.Background()
	db := openTest(t)
	m := newMusic(t, db)
	u := m.u
	rnd := rand.New(rand.NewPCG(seed, 1))
	addLabel(t, db, u, "Game")
	var ls []NewListen
	for i := range 6 {
		ls = append(ls, NewListen{ListenedAt: int64(1000 + i), Artist: "A", Title: fmt.Sprint("T", i), Payload: []byte(`{}`)})
	}
	if _, err := db.InsertListens(ctx, u, "listenbrainz", nil, ls); err != nil {
		t.Fatal(err)
	}
	start := snapshot(t, db)
	var first int64 // edits before this one set up the library
	db.r.QueryRow(`SELECT coalesce(max(id), 0) FROM edits`).Scan(&first)
	artists := []int64{m.yoasobiFW, m.yoasobi, m.ayase, m.band, m.kg}
	recordings := []int64{m.idolJP, m.idolEN, m.demo}
	pick := func(xs []int64) int64 { return xs[rnd.IntN(len(xs))] }
	live := func(table string) []int64 {
		var out []int64
		rows, _ := db.r.Query(`SELECT id FROM ` + table + ` WHERE merged_into IS NULL`)
		for rows.Next() {
			var id int64
			rows.Scan(&id)
			out = append(out, id)
		}
		rows.Close()
		return out
	}

	labelIDs := func() []int64 {
		var ids []int64
		rows, _ := db.r.Query(`SELECT id FROM labels WHERE user_id = ?`, u)
		for rows.Next() {
			var id int64
			rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		return ids
	}
	names := []string{"Character", "Vocaloid", "Game", "Idol"}
	ok := func(err error) bool {
		var ce *ConflictError
		return err == nil || errors.Is(err, ErrStale) || errors.Is(err, ErrNotFound) ||
			errors.Is(err, ErrLabelName) || errors.Is(err, ErrAlreadyUndone) || errors.As(err, &ce) ||
			errors.Is(err, ErrMergeSelf) || errors.Is(err, ErrMergedAway) || errors.Is(err, ErrSplitAll)
	}
	for range 80 {
		before := snapshot(t, db)
		var err error
		ids := labelIDs()
		label := int64(1000) // a label that isn't there
		if len(ids) > 0 && rnd.IntN(5) > 0 {
			label = ids[rnd.IntN(len(ids))]
		}
		entity := tagKey(label, []string{"artist", "song", "release"}[rnd.IntN(3)], int64(rnd.IntN(3)))
		switch rnd.IntN(14) {
		case 7:
			kind := []string{"artist", "song", "recording", "release"}[rnd.IntN(4)]
			ids := live(MergeKinds[kind])
			if len(ids) > 1 {
				_, err = db.Merge(ctx, u, kind, pick(ids), pick(ids))
			}
		case 8:
			songs := live("songs")
			song := pick(songs)
			var in []int64
			rows, _ := db.r.Query(`SELECT id FROM recordings WHERE song_id = ? AND merged_into IS NULL`, song)
			for rows.Next() {
				var id int64
				rows.Scan(&id)
				if rnd.IntN(2) == 0 {
					in = append(in, id)
				}
			}
			rows.Close()
			_, _, err = db.SplitSong(ctx, u, song, in)
		case 9:
			// Relink one source by hand.
			src := int64(1 + rnd.IntN(3))
			var rec, rel, by sql.NullInt64
			db.r.QueryRow(`SELECT recording_id, release_id, linked_by FROM sources WHERE id = ?`, src).Scan(&rec, &rel, &by)
			to := pick(live("recordings"))
			_, err = db.ApplyEdit(ctx, u, "link", "relink", func(e int64) []Change {
				return []Change{{Table: "sources", ID: src,
					Before: map[string]any{"recording_id": nullable(rec), "release_id": nullable(rel), "linked_by": nullable(by)},
					After:  map[string]any{"recording_id": to, "release_id": nil, "linked_by": e}}}
			})
		case 10:
			row := map[string]any{"artist_id": pick(artists), "target_id": pick(artists), "note": ""}
			if row["artist_id"] == row["target_id"] {
				continue
			}
			op := []string{OpInsert, OpDelete}[rnd.IntN(2)]
			_, err = db.ApplyEdit(ctx, u, "credit", "counts for", func(int64) []Change {
				if op == OpDelete {
					return []Change{{Op: op, Table: "artist_counts_for", Key: map[string]any{"artist_id": row["artist_id"], "target_id": row["target_id"]}}}
				}
				return []Change{{Op: op, Table: "artist_counts_for", After: row}}
			})
		case 11:
			row := map[string]any{"group_id": pick(artists), "member_id": pick(artists)}
			if row["group_id"] == row["member_id"] {
				continue
			}
			op := []string{OpInsert, OpDelete}[rnd.IntN(2)]
			_, err = db.ApplyEdit(ctx, u, "credit", "member", func(int64) []Change {
				if op == OpDelete {
					return []Change{{Op: op, Table: "group_members", Key: row}}
				}
				return []Change{{Op: op, Table: "group_members", After: row}}
			})
		case 12:
			row := map[string]any{"recording_id": pick(recordings), "artist_id": pick(artists), "role": "featured", "position": int64(5), "credited_as": nil}
			op := []string{OpInsert, OpDelete}[rnd.IntN(2)]
			_, err = db.ApplyEdit(ctx, u, "credit", "credit", func(int64) []Change {
				if op == OpDelete {
					return []Change{{Op: op, Table: "recording_credits", Key: map[string]any{"recording_id": row["recording_id"], "artist_id": row["artist_id"], "role": "featured"}}}
				}
				return []Change{{Op: op, Table: "recording_credits", After: row}}
			})
		case 13:
			row := map[string]any{"scope": "release", "scope_id": pick([]int64{m.single, m.book}), "from_id": pick(artists), "to_id": pick(artists)}
			op := []string{OpInsert, OpDelete}[rnd.IntN(2)]
			_, err = db.ApplyEdit(ctx, u, "credit", "override", func(int64) []Change {
				if op == OpDelete {
					return []Change{{Op: op, Table: "credit_overrides", Key: row}}
				}
				return []Change{{Op: op, Table: "credit_overrides", After: row}}
			})
		case 0:
			err = db.UpdateLabel(ctx, u, label, names[rnd.IntN(len(names))], rnd.IntN(2) == 0)
		case 1:
			_, err = db.ApplyEdit(ctx, u, "label", "tag", func(int64) []Change {
				return []Change{{Op: OpInsert, Table: "entity_labels", After: entity}}
			})
		case 2:
			_, err = db.ApplyEdit(ctx, u, "label", "untag", func(int64) []Change {
				return []Change{{Op: OpDelete, Table: "entity_labels", Key: entity}}
			})
		case 3:
			_, err = db.DeleteLabel(ctx, u, label)
		case 4:
			all, _ := db.Listens(ctx, u, ListenRange{Limit: 10})
			if len(all) > 0 {
				_, err = db.DeleteListen(ctx, u, all[rnd.IntN(len(all))])
			}
		default:
			es, _ := db.Edits(ctx, u, 0, 1000)
			if len(es) > 0 {
				_, err = db.UndoEdit(ctx, u, es[rnd.IntN(len(es))].ID)
			}
		}
		if !ok(err) {
			t.Fatal(err)
		}
		if err != nil && snapshot(t, db) != before {
			t.Fatalf("refused edit changed data: %v", err)
		}
	}

	// Rewind: walk the log newest first and undo each edit still in effect,
	// undos included. Undoing an undo re-applies what it undid, which is
	// then undone when the walk reaches it.
	var last int64
	db.r.QueryRow(`SELECT coalesce(max(id), 0) FROM edits`).Scan(&last)
	for id := last; id > first; id-- {
		var inEffect bool
		if err := db.r.QueryRow(`SELECT undone_at IS NULL FROM edits WHERE id = ?`, id).Scan(&inEffect); err != nil || !inEffect {
			continue
		}
		if _, err := db.UndoEdit(ctx, u, id); err != nil {
			t.Fatalf("undo %d: %v\n%s", id, err, editLog(t, db))
		}
	}
	if got := snapshot(t, db); got != start {
		t.Fatalf("after undoing everything\n%s\nwant\n%s", got, start)
	}
}

func editLog(t *testing.T, db *DB) string {
	var b strings.Builder
	rows, _ := db.r.Query(`SELECT e.id, e.summary, coalesce(e.undoes, 0), e.undone_at IS NOT NULL, ec.op, ec.tbl, ec.row_key, coalesce(ec.before, ''), coalesce(ec.after, '')
		FROM edits e JOIN edit_changes ec ON ec.edit_id = e.id ORDER BY e.id, ec.seq`)
	defer rows.Close()
	for rows.Next() {
		var id, undoes int64
		var sum, op, tbl, key, before, after string
		var undone bool
		rows.Scan(&id, &sum, &undoes, &undone, &op, &tbl, &key, &before, &after)
		fmt.Fprintf(&b, "%d undoes=%d undone=%v %q %s %s %s %s -> %s\n", id, undoes, undone, sum, op, tbl, key, before, after)
	}
	return b.String()
}

// Undoing a redo takes the original edit out of effect again.
func TestUndoChain(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := testUser(t, db)
	ls, _ := db.Labels(ctx, u)
	if err := db.UpdateLabel(ctx, u, ls[0].ID, "Characters", true); err != nil {
		t.Fatal(err)
	}
	es, _ := db.Edits(ctx, u, 0, 1)
	id := es[0].ID
	for range 3 {
		es, _ := db.Edits(ctx, u, 0, 1)
		if _, err := db.UndoEdit(ctx, u, es[0].ID); err != nil {
			t.Fatal(err)
		}
	}
	// Undo, redo, undo: the rename is not in effect and can't be undone.
	if ls, _ := db.Labels(ctx, u); ls[0].Name != "Character" {
		t.Fatalf("name %q", ls[0].Name)
	}
	if _, err := db.UndoEdit(ctx, u, id); !errors.Is(err, ErrAlreadyUndone) {
		t.Fatalf("undo of the original: %v", err)
	}
}
