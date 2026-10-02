-- migrate: rebuilds tables
-- The album artist a scrobbler sent (Web Scrobbler's release_artist_name)
-- becomes part of the received text, so an OST credited to "Various
-- Artists" is one album instead of one per track artist. See
-- docs/architecture.md, "Data model" and "Resolving".
--
-- sources is rebuilt because its unique key changes. Text received so far
-- gets its album artist back from the stored payloads. Where listens of the
-- same text carried different album artists, the text is split: the first
-- one received keeps the original row (and msid), the others get new rows
-- and are linked again in the background.

-- Album artist of every listen, as received. Only a string counts, like the
-- decoder.
CREATE TEMP TABLE m3_listen_aa AS
  SELECT l.id AS listen_id, l.source_id,
    coalesce(CASE WHEN json_valid(p.payload)
      AND json_type(p.payload, '$.track_metadata.additional_info.release_artist_name') = 'text'
      THEN json_extract(p.payload, '$.track_metadata.additional_info.release_artist_name') END, '') AS aa
  FROM listens l LEFT JOIN listen_payloads p ON p.listen_id = l.id;

CREATE TEMP TABLE m3_groups AS
  SELECT source_id, aa, min(listen_id) AS first FROM m3_listen_aa GROUP BY source_id, aa;

-- The group each existing row stays with. Text without listens keeps ''.
CREATE TEMP TABLE m3_keep AS
  SELECT g.source_id, g.aa FROM m3_groups g
  WHERE g.first = (SELECT min(first) FROM m3_groups g2 WHERE g2.source_id = g.source_id);

CREATE TABLE sources_new (
  id                INTEGER PRIMARY KEY,
  user_id           INTEGER NOT NULL REFERENCES users(id),
  artist_text       TEXT NOT NULL,
  title_text        TEXT NOT NULL,
  album_text        TEXT NOT NULL DEFAULT '',       -- '' when none was sent
  album_artist_text TEXT NOT NULL DEFAULT '',       -- '' when none was sent
  msid              TEXT NOT NULL UNIQUE,           -- returned to clients as recording_msid
  recording_id      INTEGER REFERENCES recordings(id),
  release_id        INTEGER REFERENCES releases(id),
  linked_by         INTEGER REFERENCES edits(id),
  UNIQUE (user_id, artist_text, title_text, album_text, album_artist_text)
) STRICT;

INSERT INTO sources_new (id, user_id, artist_text, title_text, album_text, album_artist_text, msid, recording_id, release_id, linked_by)
  SELECT s.id, s.user_id, s.artist_text, s.title_text, s.album_text, coalesce(k.aa, ''), s.msid, s.recording_id, s.release_id, s.linked_by
  FROM sources s LEFT JOIN m3_keep k ON k.source_id = s.id;

-- The other groups become new, unlinked text with a fresh msid (random,
-- version 4 UUID like newUUID in listens.go).
INSERT INTO sources_new (user_id, artist_text, title_text, album_text, album_artist_text, msid)
  SELECT s.user_id, s.artist_text, s.title_text, s.album_text, g.aa,
    lower(hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' || substr(hex(randomblob(2)), 2) || '-'
      || substr('89ab', 1 + abs(random() % 4), 1) || substr(hex(randomblob(2)), 2) || '-' || hex(randomblob(6)))
  FROM m3_groups g
  JOIN sources s ON s.id = g.source_id
  JOIN m3_keep k ON k.source_id = g.source_id
  WHERE g.aa <> k.aa;

-- Their listens move over and lose the old link. The listen count triggers
-- take the counts with them.
UPDATE listens SET
    source_id = (SELECT n.id FROM m3_listen_aa a
      JOIN sources s ON s.id = a.source_id
      JOIN sources_new n ON n.user_id = s.user_id AND n.artist_text = s.artist_text AND n.title_text = s.title_text
        AND n.album_text = s.album_text AND n.album_artist_text = a.aa
      WHERE a.listen_id = listens.id),
    recording_id = NULL, release_id = NULL
  WHERE id IN (SELECT a.listen_id FROM m3_listen_aa a JOIN m3_keep k ON k.source_id = a.source_id WHERE a.aa <> k.aa);

-- Text that kept its row but now has an album artist was linked by track
-- artist alone. Automatic links are dropped so it's linked again. Links the
-- owner made stay. Undoing the old automatic link is refused afterwards,
-- since the row no longer matches it.
CREATE TEMP TABLE m3_relink AS
  SELECT n.id FROM sources_new n JOIN edits e ON e.id = n.linked_by
  WHERE n.album_artist_text <> '' AND e.automatic = 1;
UPDATE sources_new SET recording_id = NULL, release_id = NULL, linked_by = NULL
  WHERE id IN (SELECT id FROM m3_relink);
UPDATE listens SET recording_id = NULL, release_id = NULL
  WHERE source_id IN (SELECT id FROM m3_relink);

DROP TABLE sources;
ALTER TABLE sources_new RENAME TO sources;
CREATE INDEX sources_by_recording ON sources (recording_id);

-- Link the split and unlinked text again. Text that couldn't be linked
-- before is simply tried once more.
INSERT INTO jobs (kind, key, run_after, created_at)
  SELECT 'resolve', 'source:' || id, 0, 0 FROM sources WHERE recording_id IS NULL
  ON CONFLICT (kind, key) DO NOTHING;

DROP TABLE m3_listen_aa;
DROP TABLE m3_groups;
DROP TABLE m3_keep;
DROP TABLE m3_relink;
