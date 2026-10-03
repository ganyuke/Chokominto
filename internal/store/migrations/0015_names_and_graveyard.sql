-- Which of an item's names are shown. Every name, shown or not, still
-- recognizes scrobbles and finds likely duplicates.
ALTER TABLE artist_aliases ADD COLUMN shown INTEGER NOT NULL DEFAULT 1;
ALTER TABLE song_aliases ADD COLUMN shown INTEGER NOT NULL DEFAULT 1;
ALTER TABLE release_aliases ADD COLUMN shown INTEGER NOT NULL DEFAULT 1;

-- second_alias is the name shown under the item's name in lists, when the
-- owner chose it (second_set = 1, and NULL there means none). Otherwise
-- it's the first shown name after the main one. other_names now caches
-- just that name, and byline caches the shown names after the main one,
-- listed on the item's own page.
ALTER TABLE artists ADD COLUMN second_alias INTEGER;
ALTER TABLE artists ADD COLUMN second_set INTEGER NOT NULL DEFAULT 0;
ALTER TABLE artists ADD COLUMN byline TEXT NOT NULL DEFAULT '';
ALTER TABLE songs ADD COLUMN second_alias INTEGER;
ALTER TABLE songs ADD COLUMN second_set INTEGER NOT NULL DEFAULT 0;
ALTER TABLE songs ADD COLUMN byline TEXT NOT NULL DEFAULT '';
ALTER TABLE releases ADD COLUMN second_alias INTEGER;
ALTER TABLE releases ADD COLUMN second_set INTEGER NOT NULL DEFAULT 0;
ALTER TABLE releases ADD COLUMN byline TEXT NOT NULL DEFAULT '';

-- A song in the graveyard: the edit that put it there. That edit hides
-- its listens too, and later listens of it are hidden with it.
ALTER TABLE songs ADD COLUMN buried_by INTEGER REFERENCES edits(id);

-- Listens of a song in the graveyard are hidden with it, however they
-- arrive: sent later, scrobbled by hand, or linked to it afterwards.
CREATE TRIGGER listens_to_graveyard AFTER INSERT ON listens
  WHEN NEW.deleted_by IS NULL AND NEW.recording_id IS NOT NULL BEGIN
  UPDATE listens SET deleted_by = (SELECT s.buried_by FROM recordings r JOIN songs s ON s.id = r.song_id WHERE r.id = NEW.recording_id)
  WHERE id = NEW.id AND (SELECT s.buried_by FROM recordings r JOIN songs s ON s.id = r.song_id WHERE r.id = NEW.recording_id) IS NOT NULL;
END;
CREATE TRIGGER listens_relinked_to_graveyard AFTER UPDATE OF recording_id ON listens
  WHEN NEW.deleted_by IS NULL AND NEW.recording_id IS NOT NULL BEGIN
  UPDATE listens SET deleted_by = (SELECT s.buried_by FROM recordings r JOIN songs s ON s.id = r.song_id WHERE r.id = NEW.recording_id)
  WHERE id = NEW.id AND (SELECT s.buried_by FROM recordings r JOIN songs s ON s.id = r.song_id WHERE r.id = NEW.recording_id) IS NOT NULL;
END;

-- Names that came over in a merge are mostly other spellings players
-- sent, not names anyone chose, so they're hidden. Merges from now on do
-- the same.
UPDATE artist_aliases SET shown = 0 WHERE id IN (
  SELECT json_extract(c.row_key, '$.id') FROM edit_changes c JOIN edits e ON e.id = c.edit_id
  WHERE c.tbl = 'artist_aliases' AND c.op = 'update' AND e.kind = 'merge' AND e.undone_at IS NULL
    AND json_extract(c.after, '$.artist_id') IS NOT NULL);
UPDATE song_aliases SET shown = 0 WHERE id IN (
  SELECT json_extract(c.row_key, '$.id') FROM edit_changes c JOIN edits e ON e.id = c.edit_id
  WHERE c.tbl = 'song_aliases' AND c.op = 'update' AND e.kind = 'merge' AND e.undone_at IS NULL
    AND json_extract(c.after, '$.song_id') IS NOT NULL);
UPDATE release_aliases SET shown = 0 WHERE id IN (
  SELECT json_extract(c.row_key, '$.id') FROM edit_changes c JOIN edits e ON e.id = c.edit_id
  WHERE c.tbl = 'release_aliases' AND c.op = 'update' AND e.kind = 'merge' AND e.undone_at IS NULL
    AND json_extract(c.after, '$.release_id') IS NOT NULL);

-- Recompute the cached names, in display order: the pinned name, then
-- English, romaji, original script.
CREATE TABLE m15_names (owner_table TEXT NOT NULL, owner INTEGER NOT NULL, name TEXT NOT NULL, shown INTEGER NOT NULL, rn INTEGER NOT NULL) STRICT;
INSERT INTO m15_names SELECT 'artists', a.artist_id, a.name, a.shown, row_number() OVER (PARTITION BY a.artist_id
  ORDER BY (a.id = e.pinned_alias) DESC, a.shown DESC, CASE a.lang WHEN 'en' THEN 0 WHEN 'romaji' THEN 1 ELSE 2 END, a.id)
  FROM artist_aliases a JOIN artists e ON e.id = a.artist_id;
INSERT INTO m15_names SELECT 'songs', a.song_id, a.name, a.shown, row_number() OVER (PARTITION BY a.song_id
  ORDER BY (a.id = e.pinned_alias) DESC, a.shown DESC, CASE a.lang WHEN 'en' THEN 0 WHEN 'romaji' THEN 1 ELSE 2 END, a.id)
  FROM song_aliases a JOIN songs e ON e.id = a.song_id;
INSERT INTO m15_names SELECT 'releases', a.release_id, a.name, a.shown, row_number() OVER (PARTITION BY a.release_id
  ORDER BY (a.id = e.pinned_alias) DESC, a.shown DESC, CASE a.lang WHEN 'en' THEN 0 WHEN 'romaji' THEN 1 ELSE 2 END, a.id)
  FROM release_aliases a JOIN releases e ON e.id = a.release_id;
CREATE INDEX m15_names_by_owner ON m15_names (owner_table, owner, rn);

UPDATE artists SET
  name = coalesce((SELECT name FROM m15_names WHERE owner_table = 'artists' AND owner = artists.id AND rn = 1), name),
  other_names = coalesce((SELECT name FROM m15_names WHERE owner_table = 'artists' AND owner = artists.id AND rn > 1 AND shown = 1 ORDER BY rn LIMIT 1), ''),
  byline = coalesce((SELECT group_concat(name, ' · ' ORDER BY rn) FROM m15_names WHERE owner_table = 'artists' AND owner = artists.id AND rn > 1 AND shown = 1), '');
UPDATE songs SET
  name = coalesce((SELECT name FROM m15_names WHERE owner_table = 'songs' AND owner = songs.id AND rn = 1), name),
  other_names = coalesce((SELECT name FROM m15_names WHERE owner_table = 'songs' AND owner = songs.id AND rn > 1 AND shown = 1 ORDER BY rn LIMIT 1), ''),
  byline = coalesce((SELECT group_concat(name, ' · ' ORDER BY rn) FROM m15_names WHERE owner_table = 'songs' AND owner = songs.id AND rn > 1 AND shown = 1), '');
UPDATE releases SET
  name = coalesce((SELECT name FROM m15_names WHERE owner_table = 'releases' AND owner = releases.id AND rn = 1), name),
  other_names = coalesce((SELECT name FROM m15_names WHERE owner_table = 'releases' AND owner = releases.id AND rn > 1 AND shown = 1 ORDER BY rn LIMIT 1), ''),
  byline = coalesce((SELECT group_concat(name, ' · ' ORDER BY rn) FROM m15_names WHERE owner_table = 'releases' AND owner = releases.id AND rn > 1 AND shown = 1), '');
DROP TABLE m15_names;

-- View counts written out in words ("4 million views") are now read as
-- no album, like "33M plays" already were. Read those again.
INSERT OR IGNORE INTO jobs (kind, key, run_after, created_at)
  SELECT 'reparse', 'source:' || id, 0, 0 FROM sources
  WHERE album_text GLOB '[0-9]*' AND (lower(album_text) LIKE '% thousand views' OR lower(album_text) LIKE '% million views'
    OR lower(album_text) LIKE '% billion views' OR lower(album_text) LIKE '% thousand plays' OR lower(album_text) LIKE '% million plays'
    OR lower(album_text) LIKE '% billion plays');
