-- Players send "Unknown" (and the like) when they don't know the artist or
-- album, and it used to become an artist or album of that name. It now
-- counts as nothing sent, so received text like that is read again in the
-- background. Text linked by hand stays as it is. The list matches
-- placeholder in internal/resolve/resolve.go.
INSERT INTO jobs (kind, key, run_after, created_at)
  SELECT 'reparse', 'source:' || id, 0, 0 FROM sources
  WHERE lower(trim(artist_text)) IN ('unknown', 'unknown artist', 'unknown album', '<unknown>', '[unknown]', '不明なアーティスト', '不明なアルバム')
     OR lower(trim(album_text)) IN ('unknown', 'unknown artist', 'unknown album', '<unknown>', '[unknown]', '不明なアーティスト', '不明なアルバム')
     OR lower(trim(album_artist_text)) IN ('unknown', 'unknown artist', 'unknown album', '<unknown>', '[unknown]', '不明なアーティスト', '不明なアルバム')
  ON CONFLICT (kind, key) DO NOTHING;

-- Soundtracks credited to everyone heard on them picked up "Unknown" as one
-- of their artists. Re-reading the text doesn't take album credits away, so
-- it's taken off here, where the album has someone else to credit.
DELETE FROM release_credits
  WHERE artist_id IN (SELECT id FROM artists WHERE lower(trim(name)) IN ('unknown', 'unknown artist', 'unknown album', '<unknown>', '[unknown]', '不明なアーティスト', '不明なアルバム'))
    AND EXISTS (SELECT 1 FROM release_credits o WHERE o.release_id = release_credits.release_id AND o.artist_id <> release_credits.artist_id);

-- The same albums list the tracks those texts were read as, by "Unknown".
-- Re-reading unlinks the text but leaves the track, so it's taken off too,
-- unless the owner linked text to it by hand.
DELETE FROM release_tracks
  WHERE recording_id IN (
    SELECT rc.recording_id FROM recording_credits rc JOIN artists a ON a.id = rc.artist_id
    WHERE rc.role = 'main' GROUP BY rc.recording_id
    HAVING min(lower(trim(a.name)) IN ('unknown', 'unknown artist', 'unknown album', '<unknown>', '[unknown]', '不明なアーティスト', '不明なアルバム')) = 1)
  AND NOT EXISTS (SELECT 1 FROM sources s JOIN edits e ON e.id = s.linked_by
    WHERE s.recording_id = release_tracks.recording_id AND e.automatic = 0);
