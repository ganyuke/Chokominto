-- "君のせい - Kiminosei (Instrumental)" lost its version when the version
-- was only on the other-language side. Text like that is read again in
-- the background. Text linked by hand stays as it is.
INSERT INTO jobs (kind, key, run_after, created_at)
  SELECT 'reparse', 'source:' || id, 0, 0 FROM sources
  WHERE title_text LIKE '% - %)' OR title_text LIKE '% - %）' OR title_text LIKE '% - %-' OR title_text LIKE '% - %－'
  ON CONFLICT (kind, key) DO NOTHING;
