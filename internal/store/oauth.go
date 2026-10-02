package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Agents that connect by signing in (OAuth 2.1 with PKCE, as MCP asks).
// The web package does the protocol. This keeps its records.

// OAuthClient is an app that registered itself to connect.
type OAuthClient struct {
	ID           int64
	ClientID     string
	Name         string
	RedirectURIs []string
}

// OAuthCode is a one-time code the owner's approval produced.
type OAuthCode struct {
	ClientID    int64 // oauth_clients.id
	UserID      int64
	RedirectURI string
	Challenge   string
	Access      string // "look" or "change"
	ExpiresAt   int64
}

// ErrTooManyClients refuses registrations once many apps registered and
// never connected, since anyone can register.
var ErrTooManyClients = errors.New("too many unconnected apps")

const (
	maxWaitingClients = 100
	// Apps that registered but never connected are removed after this.
	clientWait = 24 * 60 * 60
)

func (db *DB) CreateOAuthClient(ctx context.Context, clientID, name string, redirectURIs []string) error {
	uris, err := json.Marshal(redirectURIs)
	if err != nil {
		return err
	}
	return db.Write(ctx, func(tx *sql.Tx) error {
		var waiting int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM oauth_clients c WHERE NOT EXISTS (SELECT 1 FROM api_tokens t WHERE t.client_id = c.id)`).Scan(&waiting); err != nil {
			return err
		}
		if waiting >= maxWaitingClients {
			return ErrTooManyClients
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO oauth_clients (client_id, name, redirect_uris, created_at) VALUES (?, ?, ?, ?)`,
			clientID, name, string(uris), unix())
		return err
	})
}

func (db *DB) OAuthClient(ctx context.Context, clientID string) (OAuthClient, error) {
	c := OAuthClient{ClientID: clientID}
	var uris string
	err := db.r.QueryRowContext(ctx, `SELECT id, name, redirect_uris FROM oauth_clients WHERE client_id = ?`, clientID).Scan(&c.ID, &c.Name, &uris)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal([]byte(uris), &c.RedirectURIs)
}

func (db *DB) CreateOAuthCode(ctx context.Context, codeHash []byte, c OAuthCode) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO oauth_codes (code_hash, client_id, user_id, redirect_uri, challenge, access, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			codeHash, c.ClientID, c.UserID, c.RedirectURI, c.Challenge, c.Access, c.ExpiresAt)
		return err
	})
}

// TakeOAuthCode returns an unexpired code and removes it, so each code
// works once.
func (db *DB) TakeOAuthCode(ctx context.Context, codeHash []byte) (OAuthCode, error) {
	var c OAuthCode
	err := db.Write(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`DELETE FROM oauth_codes WHERE code_hash = ? RETURNING client_id, user_id, redirect_uri, challenge, access, expires_at`, codeHash).
			Scan(&c.ClientID, &c.UserID, &c.RedirectURI, &c.Challenge, &c.Access, &c.ExpiresAt)
	})
	if errors.Is(err, sql.ErrNoRows) || err == nil && c.ExpiresAt <= unix() {
		return OAuthCode{}, ErrNotFound
	}
	return c, err
}

// ConnectAgent makes the agent token for an app the owner allowed. It's
// listed in Settings under the app's name.
func (db *DB) ConnectAgent(ctx context.Context, c OAuthCode, label string, accessHash, refreshHash []byte, expiresAt int64) (int64, error) {
	var id int64
	err := db.Write(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`INSERT INTO api_tokens (user_id, label, token_hash, created_at, access, client_id, expires_at, refresh_hash)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			c.UserID, label, accessHash, unix(), c.Access, c.ClientID, expiresAt, refreshHash).Scan(&id)
	})
	return id, err
}

// RefreshAgent swaps a connected app's refresh token for a new access and
// refresh token. The old ones stop working. Revoked connections can't
// refresh.
func (db *DB) RefreshAgent(ctx context.Context, clientID int64, refreshHash, newAccessHash, newRefreshHash []byte, expiresAt int64) (access string, err error) {
	err = db.Write(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`UPDATE api_tokens SET token_hash = ?, refresh_hash = ?, expires_at = ?
			 WHERE refresh_hash = ? AND client_id = ? AND revoked_at IS NULL RETURNING access`,
			newAccessHash, newRefreshHash, expiresAt, refreshHash, clientID).Scan(&access)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return access, err
}

// DeleteStaleOAuth clears expired codes and apps that registered a day ago
// or more and never connected.
func (db *DB) DeleteStaleOAuth(ctx context.Context) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		now := unix()
		if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_codes WHERE expires_at <= ?`, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`DELETE FROM oauth_clients AS c WHERE created_at <= ? AND NOT EXISTS (SELECT 1 FROM api_tokens t WHERE t.client_id = c.id)`,
			now-clientWait)
		return err
	})
}
