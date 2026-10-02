-- Agents that connect by signing in (OAuth), like the Claude app. An app
-- registers itself first and gets a client id. Anyone can register, so
-- apps that never connect are cleared after a day.
CREATE TABLE oauth_clients (
  id            INTEGER PRIMARY KEY,
  client_id     TEXT NOT NULL UNIQUE,
  name          TEXT NOT NULL,
  redirect_uris TEXT NOT NULL,          -- JSON array, matched exactly
  created_at    INTEGER NOT NULL
) STRICT;

-- One-time codes handed back to the app after the owner allows it, traded
-- for tokens within minutes.
CREATE TABLE oauth_codes (
  code_hash    BLOB PRIMARY KEY,        -- sha256 of the code
  client_id    INTEGER NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  redirect_uri TEXT NOT NULL,
  challenge    TEXT NOT NULL,           -- PKCE S256 code challenge
  access       TEXT NOT NULL CHECK (access IN ('look', 'change')),
  expires_at   INTEGER NOT NULL
) STRICT;

-- A connected app is an agent token like any other, so Settings lists it,
-- revoking cuts it off and Changes names it. Its token_hash is the current
-- access token, which expires and is replaced using the refresh token.
ALTER TABLE api_tokens ADD COLUMN client_id INTEGER REFERENCES oauth_clients(id);
ALTER TABLE api_tokens ADD COLUMN expires_at INTEGER;
ALTER TABLE api_tokens ADD COLUMN refresh_hash BLOB;
CREATE UNIQUE INDEX api_tokens_by_refresh ON api_tokens (refresh_hash) WHERE refresh_hash IS NOT NULL;
