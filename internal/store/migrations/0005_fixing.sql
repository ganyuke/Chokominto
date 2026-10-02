-- Milestone 3: fixing metadata. See docs/architecture.md, "Labels" and
-- "Merge suggestions".

-- Labels are listed in the owner's order everywhere. Existing labels start
-- in name order, which is how they were listed until now.
ALTER TABLE labels ADD COLUMN position INTEGER NOT NULL DEFAULT 0;
UPDATE labels SET position = (SELECT count(*) FROM labels l2 WHERE l2.user_id = labels.user_id AND l2.name < labels.name);

-- Candidate pairs for merging, found in the background and answered in
-- Review. a_id < b_id, so each pair is stored once.
CREATE TABLE suggestions (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id),
  kind       TEXT NOT NULL CHECK (kind IN ('artist', 'song', 'recording', 'release')),
  a_id       INTEGER NOT NULL,
  b_id       INTEGER NOT NULL,
  reason     TEXT NOT NULL,
  score      REAL NOT NULL,
  status     TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'accepted', 'dismissed')),
  created_at INTEGER NOT NULL,
  UNIQUE (kind, a_id, b_id),
  CHECK (a_id < b_id)
) STRICT;
CREATE INDEX suggestions_open ON suggestions (user_id, status, kind);

-- Merges look these up by the other column of a composite key.
CREATE INDEX credit_overrides_by_from ON credit_overrides (from_id);
CREATE INDEX credit_overrides_by_to   ON credit_overrides (to_id);
CREATE INDEX rules_by_recording ON rules (recording_id) WHERE recording_id IS NOT NULL;
CREATE INDEX rules_by_release   ON rules (release_id) WHERE release_id IS NOT NULL;
CREATE INDEX sources_by_release ON sources (release_id) WHERE release_id IS NOT NULL;
CREATE INDEX song_aliases_by_guess    ON song_aliases (guess_key) WHERE guess_key IS NOT NULL;
CREATE INDEX artist_aliases_by_guess  ON artist_aliases (guess_key) WHERE guess_key IS NOT NULL;
CREATE INDEX release_aliases_by_guess ON release_aliases (guess_key) WHERE guess_key IS NOT NULL;
CREATE INDEX release_aliases_by_romaji ON release_aliases (romaji_key) WHERE romaji_key IS NOT NULL;

-- Guess kanji readings of names added so far, for search, in the
-- background.
INSERT INTO jobs (kind, key, run_after, created_at)
  SELECT 'guess_keys', t || ':0', 0, 0 FROM (
    SELECT 'artist_aliases' AS t WHERE EXISTS (SELECT 1 FROM artist_aliases)
    UNION ALL SELECT 'song_aliases' WHERE EXISTS (SELECT 1 FROM song_aliases)
    UNION ALL SELECT 'release_aliases' WHERE EXISTS (SELECT 1 FROM release_aliases))
  WHERE true ON CONFLICT (kind, key) DO NOTHING;

-- Look for merge suggestions in what's there so far.
INSERT INTO jobs (kind, key, run_after, created_at)
  SELECT 'suggest', 'user:' || id, 0, 0 FROM users WHERE EXISTS (SELECT 1 FROM artists)
  ON CONFLICT (kind, key) DO NOTHING;

-- Received text searched in Review, normalized once: the match keys of
-- artist, title and album, joined with a unit separator. Derived from the
-- text, which never changes.
ALTER TABLE sources ADD COLUMN search_key TEXT NOT NULL DEFAULT '';
UPDATE sources SET search_key = match_key(artist_text) || char(31) || match_key(title_text) || char(31) || match_key(album_text);

-- Review finds listens stored as incomplete without reading every listen.
CREATE INDEX listens_incomplete ON listens (source_id) WHERE incomplete = 1 AND deleted_by IS NULL;
