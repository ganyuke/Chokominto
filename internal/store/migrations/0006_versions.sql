-- migrate: rebuilds tables
-- Milestone 4: reading the owner's real history better, and every way of
-- reading scrobbles as a rule the owner can see and switch in Settings.
-- See docs/architecture.md, "Resolving" and "Rules".

-- A recording marked to rank on its own keeps its own row in Top songs,
-- even when versions of a song are combined. Instrumental, off vocal and
-- karaoke versions start marked.
ALTER TABLE recordings ADD COLUMN rank_alone INTEGER NOT NULL DEFAULT 0;

-- Rules get three more kinds, for readings that were built in:
--   titles   "君のせい - Kiminosei" is one song with two names
--   version  "(Instrumental)", "- movie ver." make versions of a song
--   group    a group named with its characters credits the group
--   covers   "Song (Cover) - Singer", sent by Singer, is the song
--   compilation  a soundtrack's tracks by different artists are one album
-- rules is rebuilt because its CHECK changes. Ids stay, so edits that
-- point at rules keep pointing at them. The defaults trigger refers to
-- rules, so it goes first and comes back below.
DROP TRIGGER user_defaults;
CREATE TABLE rules_new (
  id           INTEGER PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id),
  kind         TEXT NOT NULL CHECK (kind IN ('clean', 'split', 'cv', 'link', 'titles', 'version', 'group', 'covers', 'compilation')),
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
INSERT INTO rules_new SELECT id, user_id, kind, field, artist_match, title_match, album_match, match_mode, pattern,
  recording_id, release_id, priority, enabled, created_by, created_at FROM rules;
DROP TABLE rules;
ALTER TABLE rules_new RENAME TO rules;
CREATE INDEX rules_by_user ON rules (user_id, kind);
CREATE INDEX rules_by_recording ON rules (recording_id) WHERE recording_id IS NOT NULL;
CREATE INDEX rules_by_release   ON rules (release_id) WHERE release_id IS NOT NULL;

-- New defaults for every user (owner, 2026-09-30, from the real history):
-- artist lists split on ", " and " / ", and the three readings above.
-- Ordinary rules, changed or switched off in Settings. Text linked so far
-- keeps its links. New text uses them.
INSERT INTO rules (user_id, kind, field, match_mode, pattern, created_at)
  SELECT u.id, d.kind, d.field, 'exact', d.pattern, u.created_at FROM users u,
  (SELECT 'split' AS kind, 'artist' AS field, ', ' AS pattern
   UNION ALL SELECT 'split', 'artist', ' / '
   UNION ALL SELECT 'titles', 'title', NULL
   UNION ALL SELECT 'version', 'title', NULL
   UNION ALL SELECT 'group', 'artist', NULL
   UNION ALL SELECT 'covers', 'title', NULL
   UNION ALL SELECT 'compilation', 'album', NULL) d
  WHERE NOT EXISTS (SELECT 1 FROM rules r WHERE r.user_id = u.id AND r.kind = d.kind AND r.pattern IS d.pattern);

CREATE TRIGGER user_defaults AFTER INSERT ON users BEGIN
  INSERT INTO labels (user_id, name, hide_default) VALUES (NEW.id, 'Character', 1);
  INSERT INTO rules (user_id, kind, field, match_mode, pattern, created_at) VALUES
    (NEW.id, 'split', 'artist', 'exact', ' feat. ', NEW.created_at),
    (NEW.id, 'split', 'artist', 'exact', ' ft. ', NEW.created_at),
    (NEW.id, 'split', 'artist', 'exact', ' featuring ', NEW.created_at),
    (NEW.id, 'split', 'artist', 'exact', ', ', NEW.created_at),
    (NEW.id, 'split', 'artist', 'exact', ' / ', NEW.created_at),
    (NEW.id, 'cv', 'artist', 'regex', NULL, NEW.created_at),
    (NEW.id, 'group', 'artist', 'exact', NULL, NEW.created_at),
    (NEW.id, 'titles', 'title', 'exact', NULL, NEW.created_at),
    (NEW.id, 'version', 'title', 'exact', NULL, NEW.created_at),
    (NEW.id, 'covers', 'title', 'exact', NULL, NEW.created_at),
    (NEW.id, 'compilation', 'album', 'exact', NULL, NEW.created_at);
END;
