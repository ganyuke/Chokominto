package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const (
	SessionLifetime = 30 * 24 * time.Hour
	// Sessions are extended at most once a day, so reads don't turn every
	// page view into a write.
	sessionRefresh = 24 * time.Hour
)

func (db *DB) CreateSession(ctx context.Context, userID int64, tokenHash []byte) error {
	t := unix()
	return db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
			tokenHash, userID, t, t+int64(SessionLifetime/time.Second))
		return err
	})
}

// SessionUser returns the user of a live session and slides its expiry.
func (db *DB) SessionUser(ctx context.Context, tokenHash []byte) (int64, error) {
	var userID, expires int64
	err := db.r.QueryRowContext(ctx,
		`SELECT user_id, expires_at FROM sessions WHERE token_hash = ?`, tokenHash).Scan(&userID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	t := unix()
	if expires <= t {
		return 0, ErrNotFound
	}
	newExpiry := t + int64(SessionLifetime/time.Second)
	if newExpiry-expires >= int64(sessionRefresh/time.Second) {
		err = db.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE sessions SET expires_at = ? WHERE token_hash = ?`, newExpiry, tokenHash)
			return err
		})
	}
	return userID, err
}

func (db *DB) DeleteSession(ctx context.Context, tokenHash []byte) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
		return err
	})
}

func (db *DB) DeleteExpiredSessions(ctx context.Context) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, unix())
		return err
	})
}
