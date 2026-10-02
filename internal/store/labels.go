package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type LabelUse struct {
	Label
	Uses int
}

// Labels lists all of a user's labels with how many things carry each.
func (db *DB) Labels(ctx context.Context, userID int64) ([]LabelUse, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT l.id, l.name, l.hide_default, (SELECT count(*) FROM entity_labels el WHERE el.label_id = l.id)
		 FROM labels l WHERE l.user_id = ? ORDER BY l.position, l.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LabelUse
	for rows.Next() {
		var l LabelUse
		if err := rows.Scan(&l.ID, &l.Name, &l.HideDefault, &l.Uses); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

var ErrLabelName = errors.New("a label with that name already exists")

// UpdateLabel renames a label and sets whether it's hidden in rankings by
// default, through the edit log.
func (db *DB) UpdateLabel(ctx context.Context, userID, labelID int64, name string, hideDefault bool) error {
	name = strings.TrimSpace(name)
	return db.Write(ctx, func(tx *sql.Tx) error {
		var cur Label
		err := tx.QueryRowContext(ctx, `SELECT id, name, hide_default FROM labels WHERE id = ? AND user_id = ?`, labelID, userID).
			Scan(&cur.ID, &cur.Name, &cur.HideDefault)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if cur.Name == name && cur.HideDefault == hideDefault {
			return nil
		}
		if cur.Name != name {
			var clash int
			tx.QueryRowContext(ctx, `SELECT count(*) FROM labels WHERE user_id = ? AND name = ? AND id <> ?`, userID, name, labelID).Scan(&clash)
			if clash > 0 {
				return ErrLabelName
			}
		}
		var parts []string
		if cur.Name != name {
			parts = append(parts, fmt.Sprintf("Renamed label %s to %s", cur.Name, name))
		}
		if cur.HideDefault != hideDefault {
			state := "shown"
			if hideDefault {
				state = "hidden"
			}
			parts = append(parts, fmt.Sprintf("Label %s now %s in rankings by default", name, state))
		}
		_, err = ApplyEditTx(ctx, tx, userID, EditMeta{Kind: "label", Summary: strings.Join(parts, ". ")}, func(int64) []Change {
			return []Change{{Table: "labels", ID: labelID,
				Before: map[string]any{"name": cur.Name, "hide_default": boolInt(cur.HideDefault)},
				After:  map[string]any{"name": name, "hide_default": boolInt(hideDefault)}}}
		})
		return err
	})
}

// boolInt stores flags as SQLite does, 0 or 1, so the edit log compares
// like with like.
func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// DeleteLabel takes a label off everything that has it and removes the
// label, as one edit, so undoing it puts both back. It returns the edit.
func (db *DB) DeleteLabel(ctx context.Context, userID, labelID int64) (int64, error) {
	var editID int64
	err := db.Write(ctx, func(tx *sql.Tx) error {
		var name string
		err := tx.QueryRowContext(ctx, `SELECT name FROM labels WHERE id = ? AND user_id = ?`, labelID, userID).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT entity_type, entity_id FROM entity_labels WHERE label_id = ? ORDER BY entity_type, entity_id`, labelID)
		if err != nil {
			return err
		}
		var changes []Change
		for rows.Next() {
			var typ string
			var id int64
			if err := rows.Scan(&typ, &id); err != nil {
				rows.Close()
				return err
			}
			changes = append(changes, Change{Op: OpDelete, Table: "entity_labels",
				Key: map[string]any{"label_id": labelID, "entity_type": typ, "entity_id": id}})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		summary := "Deleted label " + name
		if n := len(changes); n > 0 {
			summary += fmt.Sprintf(", used on %d", n)
		}
		changes = append(changes, Change{Op: OpDelete, Table: "labels", ID: labelID})
		editID, err = ApplyEditTx(ctx, tx, userID, EditMeta{Kind: "label", Summary: summary}, func(int64) []Change { return changes })
		return err
	})
	return editID, err
}
