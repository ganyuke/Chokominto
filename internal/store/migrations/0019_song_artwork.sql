-- A song can have a picture of its own. Without one it shows the cover of
-- an album it's on.
ALTER TABLE songs ADD COLUMN artwork_id INTEGER REFERENCES artwork(id);
ALTER TABLE songs ADD COLUMN artwork_pinned INTEGER NOT NULL DEFAULT 0;
CREATE INDEX songs_by_artwork ON songs (artwork_id) WHERE artwork_id IS NOT NULL;
