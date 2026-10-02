-- Milestone 2: artists, songs, recordings and releases, and the links from
-- received text to them. See docs/architecture.md, "Data model".

CREATE TABLE artists (
  id           INTEGER PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id),
  kind         TEXT NOT NULL DEFAULT 'other' CHECK (kind IN ('person', 'group', 'other')),
  name         TEXT NOT NULL,              -- primary name, derived from aliases
  other_names  TEXT NOT NULL DEFAULT '',   -- the rest in display order, derived
  pinned_alias INTEGER,                    -- an artist_aliases id
  mbid         TEXT,
  merged_into  INTEGER REFERENCES artists(id),
  created_at   INTEGER NOT NULL
) STRICT;
CREATE INDEX artists_by_user ON artists (user_id);

CREATE TABLE artist_aliases (
  id         INTEGER PRIMARY KEY,
  artist_id  INTEGER NOT NULL REFERENCES artists(id),
  name       TEXT NOT NULL,
  lang       TEXT NOT NULL CHECK (lang IN ('en', 'romaji', 'original')),
  lang_set   INTEGER NOT NULL DEFAULT 0,   -- 1 = set by the owner or MusicBrainz
  match_key  TEXT NOT NULL,
  romaji_key TEXT,
  guess_key  TEXT,
  UNIQUE (artist_id, name)
) STRICT;
CREATE INDEX artist_aliases_by_key    ON artist_aliases (match_key);
CREATE INDEX artist_aliases_by_romaji ON artist_aliases (romaji_key) WHERE romaji_key IS NOT NULL;

CREATE TABLE group_members (
  group_id  INTEGER NOT NULL REFERENCES artists(id),
  member_id INTEGER NOT NULL REFERENCES artists(id),
  PRIMARY KEY (group_id, member_id),
  CHECK (group_id <> member_id)
) STRICT;
CREATE INDEX group_members_by_member ON group_members (member_id);

-- "Also counts for": listens credited to artist_id also count for target_id.
CREATE TABLE artist_counts_for (
  artist_id INTEGER NOT NULL REFERENCES artists(id),
  target_id INTEGER NOT NULL REFERENCES artists(id),
  note      TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (artist_id, target_id),
  CHECK (artist_id <> target_id)
) STRICT;
CREATE INDEX artist_counts_for_by_target ON artist_counts_for (target_id);

CREATE TABLE songs (
  id           INTEGER PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id),
  name         TEXT NOT NULL,
  other_names  TEXT NOT NULL DEFAULT '',
  pinned_alias INTEGER,
  mbid         TEXT,
  merged_into  INTEGER REFERENCES songs(id),
  created_at   INTEGER NOT NULL
) STRICT;
CREATE INDEX songs_by_user ON songs (user_id);

CREATE TABLE song_aliases (
  id         INTEGER PRIMARY KEY,
  song_id    INTEGER NOT NULL REFERENCES songs(id),
  name       TEXT NOT NULL,
  lang       TEXT NOT NULL CHECK (lang IN ('en', 'romaji', 'original')),
  lang_set   INTEGER NOT NULL DEFAULT 0,
  match_key  TEXT NOT NULL,
  romaji_key TEXT,
  guess_key  TEXT,
  UNIQUE (song_id, name)
) STRICT;
CREATE INDEX song_aliases_by_key    ON song_aliases (match_key);
CREATE INDEX song_aliases_by_romaji ON song_aliases (romaji_key) WHERE romaji_key IS NOT NULL;

CREATE TABLE recordings (
  id          INTEGER PRIMARY KEY,
  user_id     INTEGER NOT NULL REFERENCES users(id),
  song_id     INTEGER NOT NULL REFERENCES songs(id),
  version     TEXT NOT NULL DEFAULT '',
  is_original INTEGER NOT NULL DEFAULT 0,
  duration_ms INTEGER,
  mbid        TEXT,
  merged_into INTEGER REFERENCES recordings(id),
  created_at  INTEGER NOT NULL
) STRICT;
CREATE INDEX recordings_by_song ON recordings (song_id);

CREATE TABLE recording_credits (
  recording_id INTEGER NOT NULL REFERENCES recordings(id),
  artist_id    INTEGER NOT NULL REFERENCES artists(id),
  role         TEXT NOT NULL CHECK (role IN ('main', 'featured', 'composer', 'lyricist', 'arranger')),
  position     INTEGER NOT NULL,
  credited_as  TEXT,
  PRIMARY KEY (recording_id, artist_id, role)
) STRICT;
CREATE INDEX recording_credits_by_artist ON recording_credits (artist_id);

CREATE TABLE releases (
  id           INTEGER PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id),
  name         TEXT NOT NULL,
  other_names  TEXT NOT NULL DEFAULT '',
  pinned_alias INTEGER,
  kind         TEXT NOT NULL DEFAULT 'other' CHECK (kind IN ('album', 'single', 'ep', 'soundtrack', 'video', 'other')),
  context      TEXT NOT NULL DEFAULT '',
  released     TEXT,
  mbid         TEXT,
  merged_into  INTEGER REFERENCES releases(id),
  created_at   INTEGER NOT NULL
) STRICT;
CREATE INDEX releases_by_user ON releases (user_id);

CREATE TABLE release_aliases (
  id         INTEGER PRIMARY KEY,
  release_id INTEGER NOT NULL REFERENCES releases(id),
  name       TEXT NOT NULL,
  lang       TEXT NOT NULL CHECK (lang IN ('en', 'romaji', 'original')),
  lang_set   INTEGER NOT NULL DEFAULT 0,
  match_key  TEXT NOT NULL,
  romaji_key TEXT,
  guess_key  TEXT,
  UNIQUE (release_id, name)
) STRICT;
CREATE INDEX release_aliases_by_key ON release_aliases (match_key);

CREATE TABLE release_credits (
  release_id INTEGER NOT NULL REFERENCES releases(id),
  artist_id  INTEGER NOT NULL REFERENCES artists(id),
  position   INTEGER NOT NULL,
  PRIMARY KEY (release_id, artist_id)
) STRICT;
CREATE INDEX release_credits_by_artist ON release_credits (artist_id);

CREATE TABLE release_tracks (
  release_id   INTEGER NOT NULL REFERENCES releases(id),
  recording_id INTEGER NOT NULL REFERENCES recordings(id),
  disc         INTEGER NOT NULL DEFAULT 1,
  position     INTEGER,
  PRIMARY KEY (release_id, recording_id)
) STRICT;
CREATE INDEX release_tracks_by_recording ON release_tracks (recording_id);

-- Here, listens credited to from_id go to exactly these artists.
CREATE TABLE credit_overrides (
  scope    TEXT NOT NULL CHECK (scope IN ('recording', 'release')),
  scope_id INTEGER NOT NULL,
  from_id  INTEGER NOT NULL REFERENCES artists(id),
  to_id    INTEGER NOT NULL REFERENCES artists(id),
  PRIMARY KEY (scope, scope_id, from_id, to_id)
) STRICT;

-- Derived: who gets credit for a listen of each recording.
CREATE TABLE recording_artists (
  recording_id INTEGER NOT NULL REFERENCES recordings(id),
  artist_id    INTEGER NOT NULL REFERENCES artists(id),
  via          TEXT NOT NULL CHECK (via IN ('credited', 'group')),
  PRIMARY KEY (recording_id, artist_id)
) STRICT;
CREATE INDEX recording_artists_by_artist ON recording_artists (artist_id, recording_id);

CREATE TABLE labels (
  id           INTEGER PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id),
  name         TEXT NOT NULL,
  hide_default INTEGER NOT NULL DEFAULT 0,
  UNIQUE (user_id, name)
) STRICT;

CREATE TABLE entity_labels (
  label_id    INTEGER NOT NULL REFERENCES labels(id) ON DELETE CASCADE,
  entity_type TEXT NOT NULL CHECK (entity_type IN ('artist', 'song', 'release')),
  entity_id   INTEGER NOT NULL,
  PRIMARY KEY (label_id, entity_type, entity_id)
) STRICT;
CREATE INDEX entity_labels_by_entity ON entity_labels (entity_type, entity_id);

CREATE TABLE rules (
  id           INTEGER PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id),
  kind         TEXT NOT NULL CHECK (kind IN ('clean', 'split', 'cv', 'link')),
  field        TEXT CHECK (field IN ('artist', 'title', 'album')),
  artist_match TEXT,
  title_match  TEXT,
  album_match  TEXT,
  match_mode   TEXT NOT NULL CHECK (match_mode IN ('exact', 'key', 'prefix', 'suffix', 'regex')),
  pattern      TEXT,
  recording_id INTEGER REFERENCES recordings(id),
  release_id   INTEGER REFERENCES releases(id),
  priority     INTEGER NOT NULL DEFAULT 0,
  enabled      INTEGER NOT NULL DEFAULT 1,
  created_by   INTEGER REFERENCES edits(id),
  created_at   INTEGER NOT NULL
) STRICT;
CREATE INDEX rules_by_user ON rules (user_id, kind);

CREATE TABLE jobs (
  id         INTEGER PRIMARY KEY,
  kind       TEXT NOT NULL,
  key        TEXT NOT NULL,
  payload    TEXT,
  run_after  INTEGER NOT NULL,
  attempts   INTEGER NOT NULL DEFAULT 0,
  last_error TEXT,
  created_at INTEGER NOT NULL,
  UNIQUE (kind, key)
) STRICT;
CREATE INDEX jobs_due ON jobs (run_after);

ALTER TABLE sources ADD COLUMN recording_id INTEGER REFERENCES recordings(id);
ALTER TABLE sources ADD COLUMN release_id INTEGER REFERENCES releases(id);
ALTER TABLE sources ADD COLUMN linked_by INTEGER REFERENCES edits(id);
ALTER TABLE listens ADD COLUMN recording_id INTEGER REFERENCES recordings(id);
ALTER TABLE listens ADD COLUMN release_id INTEGER REFERENCES releases(id);
ALTER TABLE edits ADD COLUMN rule_id INTEGER REFERENCES rules(id);

CREATE INDEX sources_by_recording ON sources (recording_id);
CREATE INDEX listens_by_recording ON listens (recording_id, listened_at) WHERE deleted_by IS NULL;
CREATE INDEX listens_by_release   ON listens (release_id, listened_at) WHERE deleted_by IS NULL;
-- Covers ranking queries over a time range without touching table rows.
CREATE INDEX listens_for_rankings ON listens (user_id, listened_at, recording_id, release_id) WHERE deleted_by IS NULL;

-- Defaults every user starts with. They're ordinary rows the owner can
-- change or delete later.
CREATE TRIGGER user_defaults AFTER INSERT ON users BEGIN
  INSERT INTO labels (user_id, name, hide_default) VALUES (NEW.id, 'Character', 1);
  INSERT INTO rules (user_id, kind, field, match_mode, pattern, created_at) VALUES
    (NEW.id, 'split', 'artist', 'exact', ' feat. ', NEW.created_at),
    (NEW.id, 'split', 'artist', 'exact', ' ft. ', NEW.created_at),
    (NEW.id, 'split', 'artist', 'exact', ' featuring ', NEW.created_at),
    (NEW.id, 'cv', 'artist', 'regex', NULL, NEW.created_at);
END;

INSERT INTO labels (user_id, name, hide_default) SELECT id, 'Character', 1 FROM users;
INSERT INTO rules (user_id, kind, field, match_mode, pattern, created_at)
  SELECT u.id, d.kind, 'artist', d.mode, d.pattern, u.created_at FROM users u,
  (SELECT 'split' AS kind, 'exact' AS mode, ' feat. ' AS pattern
   UNION ALL SELECT 'split', 'exact', ' ft. '
   UNION ALL SELECT 'split', 'exact', ' featuring '
   UNION ALL SELECT 'cv', 'regex', NULL) d;

-- Link everything received so far, in the background.
INSERT INTO jobs (kind, key, run_after, created_at)
  SELECT 'resolve', 'source:' || id, 0, 0 FROM sources;

-- Listen counts per UTC calendar year, recording and release (0 = none),
-- kept exact by triggers through inserts, deletes, undos and relinks.
-- Long rankings add these up instead of counting every listen. See
-- rankings.go for how periods that don't line up with UTC years are
-- corrected.
CREATE TABLE listen_years (
  user_id      INTEGER NOT NULL REFERENCES users(id),
  year         INTEGER NOT NULL,
  recording_id INTEGER NOT NULL,
  release_id   INTEGER NOT NULL DEFAULT 0,
  n            INTEGER NOT NULL,
  PRIMARY KEY (user_id, year, recording_id, release_id)
) STRICT, WITHOUT ROWID;

-- All-time totals, kept by the same triggers. All-time rankings read one row
-- per recording and release instead of one per year.
CREATE TABLE listen_totals (
  user_id      INTEGER NOT NULL REFERENCES users(id),
  recording_id INTEGER NOT NULL,
  release_id   INTEGER NOT NULL DEFAULT 0,
  n            INTEGER NOT NULL,
  PRIMARY KEY (user_id, recording_id, release_id)
) STRICT, WITHOUT ROWID;

CREATE TRIGGER listen_counts_insert AFTER INSERT ON listens
  WHEN NEW.deleted_by IS NULL AND NEW.recording_id IS NOT NULL BEGIN
  INSERT INTO listen_totals (user_id, recording_id, release_id, n)
    VALUES (NEW.user_id, NEW.recording_id, coalesce(NEW.release_id, 0), 1)
    ON CONFLICT DO UPDATE SET n = n + 1;
  INSERT INTO listen_years (user_id, year, recording_id, release_id, n)
    VALUES (NEW.user_id, CAST(strftime('%Y', NEW.listened_at, 'unixepoch') AS INTEGER), NEW.recording_id, coalesce(NEW.release_id, 0), 1)
    ON CONFLICT DO UPDATE SET n = n + 1;
END;

CREATE TRIGGER listen_counts_update AFTER UPDATE OF deleted_by, recording_id, release_id ON listens BEGIN
  UPDATE listen_totals SET n = n - 1
    WHERE OLD.deleted_by IS NULL AND OLD.recording_id IS NOT NULL
      AND user_id = OLD.user_id AND recording_id = OLD.recording_id AND release_id = coalesce(OLD.release_id, 0);
  INSERT INTO listen_totals (user_id, recording_id, release_id, n)
    SELECT NEW.user_id, NEW.recording_id, coalesce(NEW.release_id, 0), 1
    WHERE NEW.deleted_by IS NULL AND NEW.recording_id IS NOT NULL
    ON CONFLICT DO UPDATE SET n = n + 1;
  DELETE FROM listen_totals
    WHERE OLD.recording_id IS NOT NULL AND user_id = OLD.user_id
      AND recording_id = OLD.recording_id AND release_id = coalesce(OLD.release_id, 0) AND n <= 0;
  UPDATE listen_years SET n = n - 1
    WHERE OLD.deleted_by IS NULL AND OLD.recording_id IS NOT NULL
      AND user_id = OLD.user_id AND year = CAST(strftime('%Y', OLD.listened_at, 'unixepoch') AS INTEGER)
      AND recording_id = OLD.recording_id AND release_id = coalesce(OLD.release_id, 0);
  INSERT INTO listen_years (user_id, year, recording_id, release_id, n)
    SELECT NEW.user_id, CAST(strftime('%Y', NEW.listened_at, 'unixepoch') AS INTEGER), NEW.recording_id, coalesce(NEW.release_id, 0), 1
    WHERE NEW.deleted_by IS NULL AND NEW.recording_id IS NOT NULL
    ON CONFLICT DO UPDATE SET n = n + 1;
  DELETE FROM listen_years
    WHERE OLD.recording_id IS NOT NULL AND user_id = OLD.user_id AND year = CAST(strftime('%Y', OLD.listened_at, 'unixepoch') AS INTEGER)
      AND recording_id = OLD.recording_id AND release_id = coalesce(OLD.release_id, 0) AND n <= 0;
END;

INSERT INTO listen_totals (user_id, recording_id, release_id, n)
  SELECT user_id, recording_id, coalesce(release_id, 0), count(*)
  FROM listens WHERE deleted_by IS NULL AND recording_id IS NOT NULL GROUP BY 1, 2, 3;

INSERT INTO listen_years (user_id, year, recording_id, release_id, n)
  SELECT user_id, CAST(strftime('%Y', listened_at, 'unixepoch') AS INTEGER), recording_id, coalesce(release_id, 0), count(*)
  FROM listens WHERE deleted_by IS NULL AND recording_id IS NOT NULL GROUP BY 1, 2, 3, 4;
