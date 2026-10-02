-- Milestone 1 schema. See docs/architecture.md for the full data model.
-- Times are Unix seconds, UTC.

CREATE TABLE users (
  id            INTEGER PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,              -- argon2id, PHC string
  time_zone     TEXT NOT NULL DEFAULT 'UTC',
  week_start    INTEGER NOT NULL DEFAULT 1 CHECK (week_start IN (0, 1)),  -- 0 Sunday, 1 Monday
  created_at    INTEGER NOT NULL
) STRICT;

CREATE TABLE sessions (
  token_hash BLOB PRIMARY KEY,               -- sha256 of the cookie value
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
) STRICT;
CREATE INDEX sessions_by_user ON sessions (user_id);

CREATE TABLE api_tokens (
  id           INTEGER PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  label        TEXT NOT NULL,
  token_hash   BLOB NOT NULL UNIQUE,          -- sha256 of the token
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER,
  revoked_at   INTEGER
) STRICT;
CREATE INDEX api_tokens_by_user ON api_tokens (user_id);

CREATE TABLE edits (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id),
  kind       TEXT NOT NULL,
  summary    TEXT NOT NULL,
  automatic  INTEGER NOT NULL DEFAULT 0,
  undoes     INTEGER REFERENCES edits(id),    -- set when this edit is an undo of another
  created_at INTEGER NOT NULL,
  undone_at  INTEGER
) STRICT;
CREATE INDEX edits_by_user ON edits (user_id, id);

CREATE TABLE edit_changes (
  edit_id INTEGER NOT NULL REFERENCES edits(id),
  seq     INTEGER NOT NULL,
  op      TEXT NOT NULL CHECK (op IN ('insert', 'update', 'delete')),
  tbl     TEXT NOT NULL,
  row_key TEXT NOT NULL,                      -- JSON primary key
  before  TEXT,                               -- JSON columns before
  after   TEXT,                               -- JSON columns after
  PRIMARY KEY (edit_id, seq)
) STRICT;
CREATE INDEX edit_changes_by_row ON edit_changes (tbl, row_key);

CREATE TABLE sources (
  id          INTEGER PRIMARY KEY,
  user_id     INTEGER NOT NULL REFERENCES users(id),
  artist_text TEXT NOT NULL,
  title_text  TEXT NOT NULL,
  album_text  TEXT NOT NULL DEFAULT '',       -- '' when none was sent
  msid        TEXT NOT NULL UNIQUE,           -- returned to clients as recording_msid
  UNIQUE (user_id, artist_text, title_text, album_text)
) STRICT;

CREATE TABLE listens (
  id          INTEGER PRIMARY KEY,
  user_id     INTEGER NOT NULL REFERENCES users(id),
  listened_at INTEGER NOT NULL,
  source_id   INTEGER NOT NULL REFERENCES sources(id),
  origin      TEXT NOT NULL,                  -- 'listenbrainz', 'manual', 'import:maloja'
  token_id    INTEGER REFERENCES api_tokens(id) ON DELETE SET NULL,
  received_at INTEGER NOT NULL,
  incomplete  INTEGER NOT NULL DEFAULT 0,     -- artist or title missing, or timestamp out of range
  deleted_by  INTEGER REFERENCES edits(id),   -- deleted listens are hidden, never removed
  UNIQUE (user_id, listened_at, source_id)
) STRICT;
CREATE INDEX listens_by_time   ON listens (user_id, listened_at, id) WHERE deleted_by IS NULL;
CREATE INDEX listens_by_source ON listens (source_id);

CREATE TABLE listen_payloads (
  listen_id INTEGER PRIMARY KEY REFERENCES listens(id) ON DELETE CASCADE,
  payload   TEXT NOT NULL                     -- the listen object exactly as received
) STRICT;

-- Live listen count per user. Counting listens on every request is too slow
-- on a Pi once there are hundreds of thousands, so triggers keep this exact
-- through inserts, deletes and undos, whatever code path makes them.
CREATE TABLE user_stats (
  user_id      INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  listen_count INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE TRIGGER user_stats_new_user AFTER INSERT ON users BEGIN
  INSERT INTO user_stats (user_id) VALUES (NEW.id);
END;

CREATE TRIGGER listens_count_insert AFTER INSERT ON listens WHEN NEW.deleted_by IS NULL BEGIN
  UPDATE user_stats SET listen_count = listen_count + 1 WHERE user_id = NEW.user_id;
END;

CREATE TRIGGER listens_count_delete AFTER UPDATE OF deleted_by ON listens
  WHEN OLD.deleted_by IS NULL AND NEW.deleted_by IS NOT NULL BEGIN
  UPDATE user_stats SET listen_count = listen_count - 1 WHERE user_id = NEW.user_id;
END;

CREATE TRIGGER listens_count_restore AFTER UPDATE OF deleted_by ON listens
  WHEN OLD.deleted_by IS NOT NULL AND NEW.deleted_by IS NULL BEGIN
  UPDATE user_stats SET listen_count = listen_count + 1 WHERE user_id = NEW.user_id;
END;
