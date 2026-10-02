package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type Token struct {
	ID         int64
	UserID     int64
	Label      string
	CreatedAt  int64
	LastUsedAt sql.NullInt64
	RevokedAt  sql.NullInt64
	Access     string // "scrobble", or for agents "look" or "change"
}

var ErrTokenExists = errors.New("token already exists")

// CreateToken makes a scrobbler token.
func (db *DB) CreateToken(ctx context.Context, userID int64, label string, tokenHash []byte) (int64, error) {
	return db.createToken(ctx, userID, label, tokenHash, "scrobble")
}

// CreateAgentToken makes a token for an AI agent, one that can only look
// or one that can also change things.
func (db *DB) CreateAgentToken(ctx context.Context, userID int64, label string, tokenHash []byte, canChange bool) (int64, error) {
	access := "look"
	if canChange {
		access = "change"
	}
	return db.createToken(ctx, userID, label, tokenHash, access)
}

func (db *DB) createToken(ctx context.Context, userID int64, label string, tokenHash []byte, access string) (int64, error) {
	var id int64
	err := db.Write(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`INSERT INTO api_tokens (user_id, label, token_hash, created_at, access) VALUES (?, ?, ?, ?, ?) RETURNING id`,
			userID, label, tokenHash, unix(), access).Scan(&id)
	})
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return 0, ErrTokenExists
	}
	return id, err
}

// TokenUser resolves an unrevoked scrobbler token and records its use.
func (db *DB) TokenUser(ctx context.Context, tokenHash []byte) (tokenID, userID int64, err error) {
	tokenID, userID, _, err = db.tokenUser(ctx, tokenHash, `access = 'scrobble'`)
	return tokenID, userID, err
}

// AgentTokenUser resolves an unrevoked agent token and records its use.
func (db *DB) AgentTokenUser(ctx context.Context, tokenHash []byte) (tokenID, userID int64, canChange bool, err error) {
	tokenID, userID, access, err := db.tokenUser(ctx, tokenHash, `access IN ('look', 'change')`)
	return tokenID, userID, access == "change", err
}

func (db *DB) tokenUser(ctx context.Context, tokenHash []byte, which string) (tokenID, userID int64, access string, err error) {
	var lastUsed sql.NullInt64
	err = db.r.QueryRowContext(ctx,
		`SELECT id, user_id, last_used_at, access FROM api_tokens
		 WHERE token_hash = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?) AND `+which, tokenHash, unix()).
		Scan(&tokenID, &userID, &lastUsed, &access)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, "", ErrNotFound
	}
	if err != nil {
		return 0, 0, "", err
	}
	// Settings shows when a token was last used, to the minute. Writing it
	// on every request would make each of Pano's reads a disk sync.
	if now := unix(); !lastUsed.Valid || now-lastUsed.Int64 >= 60 {
		err = db.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, now, tokenID)
			return err
		})
	}
	return tokenID, userID, access, err
}

func (db *DB) Tokens(ctx context.Context, userID int64) ([]Token, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT id, user_id, label, created_at, last_used_at, revoked_at, access FROM api_tokens
		 WHERE user_id = ? ORDER BY revoked_at IS NOT NULL, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ts []Token
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.UserID, &t.Label, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt, &t.Access); err != nil {
			return nil, err
		}
		ts = append(ts, t)
	}
	return ts, rows.Err()
}

func (db *DB) RevokeToken(ctx context.Context, userID, tokenID int64) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
			unix(), tokenID, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}
