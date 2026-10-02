package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"chokominto/internal/names"
)

// GuessKeysJob fills in guessed kanji readings for names added before
// readings were guessed, a batch at a time. The key is
// "<alias table>:<last id done>". Each batch queues the next.
func (db *DB) GuessKeysJob(ctx context.Context, j Job) error {
	table, after, ok := strings.Cut(j.Key, ":")
	if _, known := aliasTables[table]; !known || !ok {
		return fmt.Errorf("guess job with key %q", j.Key)
	}
	last, err := strconv.ParseInt(after, 10, 64)
	if err != nil {
		return err
	}
	const batch = 500
	return db.Write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(
			`SELECT id, name FROM %s WHERE id > ? AND guess_key IS NULL AND romaji_key IS NULL AND lang = 'original' ORDER BY id LIMIT ?`, table),
			last, batch)
		if err != nil {
			return err
		}
		type alias struct {
			id   int64
			name string
		}
		var as []alias
		for rows.Next() {
			var a alias
			if err := rows.Scan(&a.id, &a.name); err != nil {
				rows.Close()
				return err
			}
			as = append(as, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, a := range as {
			if k := names.GuessKey(a.name); k != "" {
				if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET guess_key = ? WHERE id = ?`, table), k, a.id); err != nil {
					return err
				}
			}
			last = a.id
		}
		if len(as) < batch {
			return nil
		}
		return EnqueueTx(ctx, tx, "guess_keys", fmt.Sprintf("%s:%d", table, last), "", 0)
	})
}
