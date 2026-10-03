package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// The edit log records every change to user-facing data so it can be
// undone. A Change updates columns of one row, or adds or removes a whole
// row. Only whitelisted tables and columns can be changed, because table and
// column names end up in SQL.
var editable = map[string]map[string]bool{
	"listens":           {"deleted_by": true},
	"sources":           {"recording_id": true, "release_id": true, "linked_by": true},
	"labels":            {"name": true, "hide_default": true, "position": true},
	"artists":           {"kind": true, "pinned_alias": true, "second_alias": true, "second_set": true, "mbid": true, "merged_into": true, "artwork_id": true, "artwork_pinned": true},
	"songs":             {"pinned_alias": true, "second_alias": true, "second_set": true, "mbid": true, "merged_into": true, "buried_by": true},
	"recordings":        {"song_id": true, "version": true, "is_original": true, "rank_alone": true, "mbid": true, "merged_into": true},
	"releases":          {"pinned_alias": true, "second_alias": true, "second_set": true, "kind": true, "context": true, "released": true, "mbid": true, "merged_into": true, "artwork_id": true, "artwork_pinned": true},
	"artist_aliases":    aliasColumns("artist_id"),
	"song_aliases":      aliasColumns("song_id"),
	"release_aliases":   aliasColumns("release_id"),
	"recording_credits": {"position": true, "credited_as": true},
	"release_credits":   {"position": true},
	"release_tracks":    {"disc": true, "position": true},
	"artist_counts_for": {"note": true},
	"rules": {"enabled": true, "priority": true, "pattern": true, "recording_id": true, "release_id": true,
		"artist_match": true, "title_match": true, "album_match": true, "match_mode": true},
	"suggestions": {"status": true},
}

func aliasColumns(owner string) map[string]bool {
	return map[string]bool{owner: true, "name": true, "lang": true, "lang_set": true, "shown": true}
}

// wholeRows lists the tables whose rows can be added and removed, with
// their primary key and every column, so a removed row comes back exactly.
// TestWholeRowColumns keeps the column lists in step with the schema.
// Values go through JSON in the log, so these tables must not have BLOB
// columns.
var wholeRows = map[string]rowShape{
	"labels":            {key: []string{"id"}, cols: []string{"id", "user_id", "name", "hide_default", "position"}},
	"entity_labels":     {key: []string{"label_id", "entity_type", "entity_id"}, cols: []string{"label_id", "entity_type", "entity_id"}},
	"artist_aliases":    aliasShape("artist_id"),
	"song_aliases":      aliasShape("song_id"),
	"release_aliases":   aliasShape("release_id"),
	"group_members":     {key: []string{"group_id", "member_id"}, cols: []string{"group_id", "member_id"}},
	"artist_counts_for": {key: []string{"artist_id", "target_id"}, cols: []string{"artist_id", "target_id", "note"}},
	"recording_credits": {key: []string{"recording_id", "artist_id", "role"}, cols: []string{"recording_id", "artist_id", "role", "position", "credited_as"}},
	"release_credits":   {key: []string{"release_id", "artist_id"}, cols: []string{"release_id", "artist_id", "position"}},
	"release_tracks":    {key: []string{"release_id", "recording_id"}, cols: []string{"release_id", "recording_id", "disc", "position"}},
	"credit_overrides":  {key: []string{"scope", "scope_id", "from_id", "to_id"}, cols: []string{"scope", "scope_id", "from_id", "to_id"}},
	// New songs and recordings, from splitting a song or "New song" on the
	// Fix page. A song's name, other_names and byline are cached from its
	// aliases. Artists are added when credited by hand, and artists and
	// albums nothing uses can be deleted.
	"songs": {key: []string{"id"}, cols: []string{"id", "user_id", "name", "other_names", "pinned_alias", "mbid", "merged_into", "created_at",
		"second_alias", "second_set", "byline", "buried_by"}},
	"artists": {key: []string{"id"}, cols: []string{"id", "user_id", "kind", "name", "other_names", "pinned_alias", "mbid", "merged_into", "created_at",
		"artwork_id", "artwork_pinned", "second_alias", "second_set", "byline"}},
	"releases": {key: []string{"id"}, cols: []string{"id", "user_id", "name", "other_names", "pinned_alias", "kind", "context", "released", "mbid",
		"merged_into", "created_at", "artwork_id", "artwork_pinned", "second_alias", "second_set", "byline"}},
	"recordings": {key: []string{"id"}, cols: []string{"id", "user_id", "song_id", "version", "is_original", "rank_alone", "duration_ms", "mbid",
		"merged_into", "created_at"}},
	"rules": {key: []string{"id"}, cols: []string{"id", "user_id", "kind", "field", "artist_match", "title_match", "album_match",
		"match_mode", "pattern", "recording_id", "release_id", "priority", "enabled", "created_by", "created_at"}},
}

// Alias search keys are worked out from the name, so they're derived rather
// than logged. A key guessed differently by a later version never makes an
// undo stale.
func aliasShape(owner string) rowShape {
	return rowShape{key: []string{"id"}, cols: []string{"id", owner, "name", "lang", "lang_set", "shown"},
		derived: []string{"match_key", "romaji_key", "guess_key"}}
}

type rowShape struct {
	key, cols []string
	derived   []string // filled in from the logged columns, never compared
}

const (
	OpUpdate = "update"
	OpInsert = "insert"
	OpDelete = "delete"
)

type Change struct {
	Op    string // OpUpdate when empty
	Table string
	ID    int64          // the row's id, for tables keyed by id
	Key   map[string]any // the primary key instead of ID, for tables with a composite key
	// Update: the values the row must have now. Delete: optional, filled in
	// with the whole row as it was.
	Before map[string]any
	// Update: the values to set. Insert: the whole row.
	After map[string]any
}

func (c Change) op() string {
	if c.Op == "" {
		return OpUpdate
	}
	return c.Op
}

// key returns the change's primary key. Inserts take it from the new row.
func (c Change) key() (map[string]any, error) {
	if c.op() == OpInsert {
		shape, ok := wholeRows[c.Table]
		if !ok {
			return nil, fmt.Errorf("rows can't be added to table %q", c.Table)
		}
		k := map[string]any{}
		for _, col := range shape.key {
			v, ok := c.After[col]
			if !ok {
				return nil, fmt.Errorf("new %s row has no %s", c.Table, col)
			}
			k[col] = v
		}
		return k, nil
	}
	if c.Key != nil {
		return c.Key, nil
	}
	return map[string]any{"id": c.ID}, nil
}

type Edit struct {
	ID        int64
	Kind      string
	Summary   string
	Automatic bool
	Undoes    sql.NullInt64
	CreatedAt int64
	UndoneAt  sql.NullInt64
	Agent     string // the agent token's label, when an AI agent made it
	TaskID    int64  // the agent task it's part of, or 0
}

var (
	ErrAlreadyUndone = errors.New("already undone")
	// ErrStale means a row no longer has the value an edit expects, or a row
	// to be added is already there.
	ErrStale = errors.New("data changed since")
)

// ConflictError is returned by UndoEdit when later edits changed the same
// rows. Those have to be undone first.
type ConflictError struct{ Later []int64 }

func (e *ConflictError) Error() string {
	return fmt.Sprintf("later edits changed the same data: %v", e.Later)
}

// ApplyEdit records a new edit and applies its changes in one transaction.
// build receives the new edit's id, for changes that point at the edit.
func (db *DB) ApplyEdit(ctx context.Context, userID int64, kind, summary string, build func(editID int64) []Change) (int64, error) {
	var id int64
	err := db.Write(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = applyEdit(ctx, tx, userID, EditMeta{Kind: kind, Summary: summary}, sql.NullInt64{}, build)
		return err
	})
	return id, err
}

// EditMeta describes an edit for the log.
type EditMeta struct {
	Kind      string
	Summary   string
	Automatic bool  // made by auto-linking or a rule, not by the owner
	RuleID    int64 // the rule that made it, if any
}

// ApplyEditTx is ApplyEdit inside an existing transaction.
func ApplyEditTx(ctx context.Context, tx *sql.Tx, userID int64, m EditMeta, build func(editID int64) []Change) (int64, error) {
	return applyEdit(ctx, tx, userID, m, sql.NullInt64{}, build)
}

func applyEdit(ctx context.Context, tx *sql.Tx, userID int64, m EditMeta, undoes sql.NullInt64, build func(int64) []Change) (int64, error) {
	e, err := openEdit(ctx, tx, userID, m, undoes)
	if err != nil {
		return 0, err
	}
	if err := e.Apply(build(e.ID)...); err != nil {
		return 0, err
	}
	return e.ID, e.Close()
}

// OpenEdit is an edit whose changes are added a few at a time, each seeing
// the ones before. Bulk actions use it so several merges are one edit.
type OpenEdit struct {
	ID  int64
	ctx context.Context
	tx  *sql.Tx
	seq int
	w   *derivedWork
}

// OpenEditTx starts an edit. Close it when all changes are applied.
func OpenEditTx(ctx context.Context, tx *sql.Tx, userID int64, m EditMeta) (*OpenEdit, error) {
	return openEdit(ctx, tx, userID, m, sql.NullInt64{})
}

func openEdit(ctx context.Context, tx *sql.Tx, userID int64, m EditMeta, undoes sql.NullInt64) (*OpenEdit, error) {
	e := &OpenEdit{ctx: ctx, tx: tx, w: newDerivedWork()}
	agent, _ := ctx.Value(agentKey{}).(int64)
	task, err := editTaskTx(ctx, tx, userID, m)
	if err != nil {
		return nil, err
	}
	err = tx.QueryRowContext(ctx,
		`INSERT INTO edits (user_id, kind, summary, automatic, rule_id, undoes, created_at, agent_token_id, task_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		userID, m.Kind, m.Summary, m.Automatic, nullID(m.RuleID), undoes, unix(), nullID(agent), task).Scan(&e.ID)
	return e, err
}

type agentKey struct{}

// WithAgent marks every edit made with ctx as made by the AI agent with
// this token, so Changes can say so.
func WithAgent(ctx context.Context, tokenID int64) context.Context {
	return context.WithValue(ctx, agentKey{}, tokenID)
}

// Apply makes changes and logs them.
func (e *OpenEdit) Apply(cs ...Change) error {
	for _, c := range cs {
		key, err := c.key()
		if err != nil {
			return err
		}
		if c, err = applyChange(e.ctx, e.tx, e.w, c, key); err != nil {
			return err
		}
		_, err = e.tx.ExecContext(e.ctx,
			`INSERT INTO edit_changes (edit_id, seq, op, tbl, row_key, before, after) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			e.ID, e.seq, c.op(), c.Table, jsonOrNull(key), jsonOrNull(c.Before), jsonOrNull(c.After))
		if err != nil {
			return err
		}
		e.seq++
	}
	return nil
}

// Summarize sets the edit's summary, for bulk edits that know it at the end.
func (e *OpenEdit) Summarize(summary string) error {
	_, err := e.tx.ExecContext(e.ctx, `UPDATE edits SET summary = ? WHERE id = ?`, summary, e.ID)
	return err
}

// Close recalculates derived data for everything the edit touched.
func (e *OpenEdit) Close() error {
	return e.w.flush(e.ctx, e.tx)
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// jsonOrNull encodes m, with nil (no row) stored as NULL. Map keys come out
// sorted, so equal keys give equal row_key text.
func jsonOrNull(m map[string]any) any {
	if m == nil {
		return nil
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func checkColumns(table string, vals map[string]any) ([]string, error) {
	allowed, ok := editable[table]
	if !ok {
		return nil, fmt.Errorf("table %q is not editable", table)
	}
	cols := slices.Sorted(maps.Keys(vals))
	for _, c := range cols {
		if !allowed[c] {
			return nil, fmt.Errorf("column %s.%s is not editable", table, c)
		}
	}
	return cols, nil
}

// checkKey makes sure key names exactly the table's primary key.
func checkKey(table string, key map[string]any) ([]string, error) {
	want := []string{"id"}
	if shape, ok := wholeRows[table]; ok {
		want = shape.key
	}
	cols := slices.Sorted(maps.Keys(key))
	if !slices.Equal(cols, slices.Sorted(slices.Values(want))) {
		return nil, fmt.Errorf("%s rows are not keyed by %v", table, cols)
	}
	return cols, nil
}

// where builds a WHERE clause and its arguments for one row.
func where(table string, key map[string]any) (string, []any, error) {
	cols, err := checkKey(table, key)
	if err != nil {
		return "", nil, err
	}
	conds := make([]string, len(cols))
	args := make([]any, len(cols))
	for i, c := range cols {
		conds[i] = c + " = ?"
		args[i] = key[c]
	}
	return strings.Join(conds, " AND "), args, nil
}

func describe(table string, key map[string]any) string {
	if id, ok := key["id"]; ok && len(key) == 1 {
		return fmt.Sprintf("%s %v", table, id)
	}
	return fmt.Sprintf("%s %s", table, jsonOrNull(key))
}

// applyChange makes one change, but only if the row is as the change
// expects. It returns the change as it should be logged.
func applyChange(ctx context.Context, tx *sql.Tx, w *derivedWork, c Change, key map[string]any) (Change, error) {
	cond, args, err := where(c.Table, key)
	if err != nil {
		return c, err
	}
	switch c.op() {
	case OpUpdate:
		cols, err := checkColumns(c.Table, c.Before)
		if err != nil {
			return c, err
		}
		if !slices.Equal(cols, slices.Sorted(maps.Keys(c.After))) {
			return c, fmt.Errorf("change to %s: before and after name different columns", describe(c.Table, key))
		}
		ok, err := rowMatches(ctx, tx, c.Table, key, cols, c.Before)
		if err != nil {
			return c, err
		}
		if !ok {
			return c, fmt.Errorf("%s: %w", describe(c.Table, key), ErrStale)
		}
		sets := make([]string, len(cols))
		vals := make([]any, 0, len(cols)+len(args))
		for i, col := range cols {
			sets[i] = col + " = ?"
			vals = append(vals, c.After[col])
		}
		_, err = tx.ExecContext(ctx, fmt.Sprintf("UPDATE %s SET %s WHERE %s", c.Table, strings.Join(sets, ", "), cond), append(vals, args...)...)
		if err != nil {
			return c, constraintStale(err, c.Table, key)
		}

	case OpInsert:
		shape := wholeRows[c.Table] // key() checked it exists
		if !slices.Equal(slices.Sorted(maps.Keys(c.After)), slices.Sorted(slices.Values(shape.cols))) {
			return c, fmt.Errorf("new %s row must give every column", c.Table)
		}
		if c.Before != nil {
			return c, fmt.Errorf("new %s row can't have a before", c.Table)
		}
		row := maps.Clone(c.After)
		maps.Copy(row, derivedValues(c.Table, c.After))
		cols := slices.Sorted(maps.Keys(row))
		vals := make([]any, len(cols))
		for i, col := range cols {
			vals[i] = row[col]
		}
		_, err := tx.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s (%s) VALUES (?%s)",
			c.Table, strings.Join(cols, ", "), strings.Repeat(", ?", len(cols)-1)), vals...)
		if err != nil {
			return c, constraintStale(err, c.Table, key)
		}

	case OpDelete:
		shape, ok := wholeRows[c.Table]
		if !ok {
			return c, fmt.Errorf("rows can't be removed from table %q", c.Table)
		}
		row, err := readRow(ctx, tx, c.Table, key, shape.cols)
		if err != nil {
			return c, err
		}
		if row == nil {
			return c, fmt.Errorf("%s: %w", describe(c.Table, key), ErrStale)
		}
		if c.Before != nil && !sameValues(row, c.Before) {
			return c, fmt.Errorf("%s: %w", describe(c.Table, key), ErrStale)
		}
		if c.After != nil {
			return c, fmt.Errorf("removed %s row can't have an after", c.Table)
		}
		c.Before = row
		if err := beforeDelete(ctx, tx, c.Table, key); err != nil {
			return c, err
		}
		// Rows that still point at this one make the delete fail, rather than
		// changing them unlogged.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE %s", c.Table, cond), args...); err != nil {
			return c, constraintStale(err, c.Table, key)
		}

	default:
		return c, fmt.Errorf("unknown change %q", c.Op)
	}
	return c, afterChange(ctx, tx, w, c, key)
}

// constraintStale turns a failed constraint into ErrStale: the row, or
// another with the same unique name, is already there, a row it points at
// is gone, or other rows still point at it. All mean the data changed since
// the edit was planned.
func constraintStale(err error, table string, key map[string]any) error {
	if msg := err.Error(); strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "FOREIGN KEY constraint failed") {
		return fmt.Errorf("%s: %w", describe(table, key), ErrStale)
	}
	return err
}

// readRow returns the named columns of one row, or nil if there's no such
// row. Column names must come from a whitelist.
func readRow(ctx context.Context, tx *sql.Tx, table string, key map[string]any, cols []string) (map[string]any, error) {
	cond, args, err := where(table, key)
	if err != nil {
		return nil, err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	err = tx.QueryRowContext(ctx,
		fmt.Sprintf("SELECT %s FROM %s WHERE %s", strings.Join(cols, ", "), table, cond), args...).Scan(ptrs...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row := map[string]any{}
	for i, c := range cols {
		row[c] = vals[i]
	}
	return row, nil
}

// rowMatches reports whether the row's columns equal want.
func rowMatches(ctx context.Context, tx *sql.Tx, table string, key map[string]any, cols []string, want map[string]any) (bool, error) {
	have, err := readRow(ctx, tx, table, key, cols)
	if err != nil || have == nil {
		return false, err
	}
	return sameValues(have, want), nil
}

// sameValues compares both sides as JSON, so stored values and values
// decoded from the log line up.
func sameValues(a, b map[string]any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

type storedChange struct {
	seq    int
	op     string
	table  string
	rowKey string
	key    map[string]any
	before map[string]any
	after  map[string]any
}

func loadChanges(ctx context.Context, tx *sql.Tx, editID int64) ([]storedChange, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT seq, op, tbl, row_key, before, after FROM edit_changes WHERE edit_id = ? ORDER BY seq`, editID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cs []storedChange
	for rows.Next() {
		var c storedChange
		var before, after sql.NullString
		if err := rows.Scan(&c.seq, &c.op, &c.table, &c.rowKey, &before, &after); err != nil {
			return nil, err
		}
		if err := decodeValues(c.rowKey, &c.key); err != nil {
			return nil, err
		}
		if before.Valid {
			if err := decodeValues(before.String, &c.before); err != nil {
				return nil, err
			}
		}
		if after.Valid {
			if err := decodeValues(after.String, &c.after); err != nil {
				return nil, err
			}
		}
		cs = append(cs, c)
	}
	return cs, rows.Err()
}

func decodeValues(s string, m *map[string]any) error {
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	if err := d.Decode(m); err != nil {
		return err
	}
	// Integers go back to SQLite as integers, not text.
	for k, v := range *m {
		if n, ok := v.(json.Number); ok {
			if i, err := n.Int64(); err == nil {
				(*m)[k] = i
			} else if f, err := n.Float64(); err == nil {
				(*m)[k] = f
			}
		}
	}
	return nil
}

// reverse returns the change that takes a row back to how it was.
func (c storedChange) reverse() Change {
	switch c.op {
	case OpInsert:
		return Change{Op: OpDelete, Table: c.table, Key: c.key, Before: c.after}
	case OpDelete:
		return Change{Op: OpInsert, Table: c.table, After: c.before}
	}
	return Change{Op: OpUpdate, Table: c.table, Key: c.key, Before: c.after, After: c.before}
}

// UndoEdit reverts an edit by recording a new edit with the opposite
// changes, in reverse order. It refuses when a later edit changed the same
// rows, returning a *ConflictError naming them. If rows changed some other
// way, it returns ErrStale.
func (db *DB) UndoEdit(ctx context.Context, userID, editID int64) (int64, error) {
	var undoID int64
	err := db.Write(ctx, func(tx *sql.Tx) error {
		var err error
		undoID, err = UndoEditTx(ctx, tx, userID, editID)
		return err
	})
	return undoID, err
}

// UndoEditTx is UndoEdit inside an existing transaction.
func UndoEditTx(ctx context.Context, tx *sql.Tx, userID, editID int64) (int64, error) {
	var undoID int64
	err := func() error {
		var e Edit
		err := tx.QueryRowContext(ctx,
			`SELECT id, summary, undoes, undone_at FROM edits WHERE id = ? AND user_id = ?`, editID, userID).
			Scan(&e.ID, &e.Summary, &e.Undoes, &e.UndoneAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if e.UndoneAt.Valid {
			return ErrAlreadyUndone
		}
		changes, err := loadChanges(ctx, tx, editID)
		if err != nil {
			return err
		}
		summary := "Undo: " + e.Summary
		if e.Undoes.Valid {
			summary = "Redo: " + strings.TrimPrefix(e.Summary, "Undo: ")
		}
		// Applying the reverse checks every row is still as the edit left it.
		// If one isn't, the savepoint drops the half-made undo before looking
		// for the later edits to blame.
		if _, err := tx.ExecContext(ctx, `SAVEPOINT undo`); err != nil {
			return err
		}
		undoID, err = applyEdit(ctx, tx, userID, EditMeta{Kind: "undo", Summary: summary}, sql.NullInt64{Int64: editID, Valid: true}, func(int64) []Change {
			rev := make([]Change, 0, len(changes))
			for _, c := range slices.Backward(changes) {
				rev = append(rev, c.reverse())
			}
			return rev
		})
		if err != nil {
			if _, rerr := tx.ExecContext(ctx, `ROLLBACK TO undo`); rerr != nil {
				return rerr
			}
		}
		if errors.Is(err, ErrStale) {
			later, lerr := laterEdits(ctx, tx, editID, changes)
			if lerr != nil {
				return lerr
			}
			if len(later) > 0 {
				return &ConflictError{Later: later}
			}
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `RELEASE undo`); err != nil {
			return err
		}
		t := unix()
		if _, err := tx.ExecContext(ctx, `UPDATE edits SET undone_at = ? WHERE id = ?`, t, editID); err != nil {
			return err
		}
		// Undoing an undo brings the edit it undid back into effect. If that
		// was an undo too, what it undid goes out of effect again, and so on
		// down the chain.
		back := true
		for next := e.Undoes; next.Valid; back = !back {
			var at any
			if !back {
				at = t
			}
			if _, err := tx.ExecContext(ctx, `UPDATE edits SET undone_at = ? WHERE id = ?`, at, next.Int64); err != nil {
				return err
			}
			if err := tx.QueryRowContext(ctx, `SELECT undoes FROM edits WHERE id = ?`, next.Int64).Scan(&next); err != nil {
				return err
			}
		}
		return nil
	}()
	return undoID, err
}

// laterEdits finds edits in effect, made after editID, that touched any of
// the same rows.
func laterEdits(ctx context.Context, tx *sql.Tx, editID int64, changes []storedChange) ([]int64, error) {
	seen := map[int64]bool{}
	var ids []int64
	for _, c := range changes {
		rows, err := tx.QueryContext(ctx,
			`SELECT DISTINCT e.id FROM edit_changes ec JOIN edits e ON e.id = ec.edit_id
			 WHERE ec.tbl = ? AND ec.row_key = ? AND e.id > ? AND e.undone_at IS NULL ORDER BY e.id`,
			c.table, c.rowKey, editID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		rows.Close()
	}
	slices.Sort(ids)
	return ids, nil
}

// EditSummaries returns the summaries of the given edits, in order.
func (db *DB) EditSummaries(ctx context.Context, userID int64, ids []int64) ([]string, error) {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		var s string
		err := db.r.QueryRowContext(ctx, `SELECT summary FROM edits WHERE id = ? AND user_id = ?`, id, userID).Scan(&s)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// Edits lists a user's own edits (not automatic ones), newest first, older
// than beforeID (0 = newest).
func (db *DB) Edits(ctx context.Context, userID, beforeID int64, limit int) ([]Edit, error) {
	if beforeID <= 0 {
		beforeID = 1<<63 - 1
	}
	return db.scanEdits(ctx, `WHERE e.user_id = ? AND e.id < ? AND e.automatic = 0 ORDER BY e.id DESC LIMIT ?`, userID, beforeID, limit)
}

// ChangeRows lists what Changes shows a row for, newest first, older than
// beforeID (0 = newest): each of the user's own edits outside a task, and
// each task once, as its newest edit. Paging by these keeps every page
// full however many edits a task holds.
func (db *DB) ChangeRows(ctx context.Context, userID, beforeID int64, limit int) ([]Edit, error) {
	if beforeID <= 0 {
		beforeID = 1<<63 - 1
	}
	return db.scanEdits(ctx, `WHERE e.user_id = ? AND e.id < ? AND e.automatic = 0
		AND (e.task_id IS NULL OR e.id = (SELECT max(n.id) FROM edits n WHERE n.task_id = e.task_id AND n.automatic = 0))
		ORDER BY e.id DESC LIMIT ?`, userID, beforeID, limit)
}

func (db *DB) scanEdits(ctx context.Context, where string, args ...any) ([]Edit, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT e.id, e.kind, e.summary, e.automatic, e.undoes, e.created_at, e.undone_at, coalesce(t.label, ''), coalesce(e.task_id, 0) FROM edits e
		 LEFT JOIN api_tokens t ON t.id = e.agent_token_id `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var es []Edit
	for rows.Next() {
		var e Edit
		if err := rows.Scan(&e.ID, &e.Kind, &e.Summary, &e.Automatic, &e.Undoes, &e.CreatedAt, &e.UndoneAt, &e.Agent, &e.TaskID); err != nil {
			return nil, err
		}
		es = append(es, e)
	}
	return es, rows.Err()
}
