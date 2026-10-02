-- Milestone 4: album covers and artist pictures. See docs/architecture.md,
-- "Artwork" and "Artwork pipeline".

-- One row per stored picture. The files live in the artwork folder, named
-- by sha256 (hex), and are only removed by the cleanup in Settings.
CREATE TABLE artwork (
  id         INTEGER PRIMARY KEY,
  sha256     TEXT NOT NULL UNIQUE,
  format     TEXT NOT NULL CHECK (format IN ('jpeg', 'png')),
  width      INTEGER NOT NULL,
  height     INTEGER NOT NULL,
  origin     TEXT NOT NULL,    -- 'upload', 'coverartarchive', 'itunes', 'deezer'
  origin_url TEXT,
  created_at INTEGER NOT NULL
) STRICT;

-- The picture shown. artwork_pinned = the owner chose or uploaded it, and
-- background lookups never change it.
ALTER TABLE artists ADD COLUMN artwork_id INTEGER REFERENCES artwork(id);
ALTER TABLE artists ADD COLUMN artwork_pinned INTEGER NOT NULL DEFAULT 0;
ALTER TABLE releases ADD COLUMN artwork_id INTEGER REFERENCES artwork(id);
ALTER TABLE releases ADD COLUMN artwork_pinned INTEGER NOT NULL DEFAULT 0;

-- Pictures a lookup found but couldn't be sure of, for choosing on the
-- item's page. Nothing is downloaded until one is chosen.
CREATE TABLE artwork_candidates (
  id          INTEGER PRIMARY KEY,
  entity_type TEXT NOT NULL CHECK (entity_type IN ('artist', 'release')),
  entity_id   INTEGER NOT NULL,
  origin      TEXT NOT NULL,
  url         TEXT NOT NULL,   -- the full-size picture
  thumb_url   TEXT,            -- a small one for showing the choice
  thumb_sha   TEXT,            -- that small one, once fetched and kept
  thumb_format TEXT,
  title       TEXT NOT NULL DEFAULT '',
  artist      TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  UNIQUE (entity_type, entity_id, url)
) STRICT;

-- Where each lookup stands, for trying again later.
CREATE TABLE artwork_lookups (
  entity_type TEXT NOT NULL CHECK (entity_type IN ('artist', 'release')),
  entity_id   INTEGER NOT NULL,
  state       TEXT NOT NULL CHECK (state IN ('found', 'candidates', 'notfound', 'error')),
  tries       INTEGER NOT NULL DEFAULT 0,
  last_error  TEXT,
  checked_at  INTEGER NOT NULL,
  PRIMARY KEY (entity_type, entity_id)
) STRICT;

-- Whether to look for pictures online. On by default, switched in
-- Settings. Lookups send album and artist names to iTunes, Deezer and
-- Cover Art Archive.
ALTER TABLE users ADD COLUMN find_artwork INTEGER NOT NULL DEFAULT 1;

-- Look for pictures of everything there so far, in the background.
INSERT INTO jobs (kind, key, run_after, created_at)
  SELECT 'artwork', 'release:' || id || ':0', 0, 0 FROM releases WHERE merged_into IS NULL
  UNION ALL SELECT 'artwork', 'artist:' || id || ':0', 0, 0 FROM artists WHERE merged_into IS NULL
  ON CONFLICT (kind, key) DO NOTHING;
