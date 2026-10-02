package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type User struct {
	ID           int64
	Name         string
	PasswordHash string
	TimeZone     string
	WeekStart    int
	FindArtwork  bool // look for pictures online
	// ShowOtherNames shows an item's other names under its name.
	ShowOtherNames bool
}

var ErrNameTaken = errors.New("name already taken")

func (db *DB) CreateUser(ctx context.Context, name, passwordHash string) (int64, error) {
	var id int64
	err := db.Write(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`INSERT INTO users (name, password_hash, created_at) VALUES (?, ?, ?) RETURNING id`,
			name, passwordHash, unix()).Scan(&id)
	})
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return 0, ErrNameTaken
	}
	return id, err
}

const userCols = `id, name, password_hash, time_zone, week_start, find_artwork, show_other_names`

func scanUser(row *sql.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Name, &u.PasswordHash, &u.TimeZone, &u.WeekStart, &u.FindArtwork, &u.ShowOtherNames)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

func (db *DB) UserByName(ctx context.Context, name string) (User, error) {
	return scanUser(db.r.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE name = ?`, name))
}

func (db *DB) UserByID(ctx context.Context, id int64) (User, error) {
	return scanUser(db.r.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// FirstUser returns the oldest account. While Chokominto is single-user,
// that's whose listens the public pages show.
func (db *DB) FirstUser(ctx context.Context) (User, error) {
	return scanUser(db.r.QueryRowContext(ctx, `SELECT `+userCols+` FROM users ORDER BY id LIMIT 1`))
}

// SetPassword changes the password and signs out every session.
func (db *DB) SetPassword(ctx context.Context, userID int64, passwordHash string) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
		return err
	})
}

func (db *DB) SetPreferences(ctx context.Context, userID int64, timeZone string, weekStart int) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE users SET time_zone = ?, week_start = ? WHERE id = ?`,
			timeZone, weekStart, userID)
		return err
	})
}
