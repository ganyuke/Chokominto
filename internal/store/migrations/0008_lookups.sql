-- Indexes for lookups that otherwise read a whole table each time.

-- The next due job of one kind. Each runner asks per kind, so a long queue
-- of another kind (picture lookups) is never read through.
CREATE INDEX jobs_kind_due ON jobs (kind, run_after);

-- Whether a picture is still shown anywhere.
CREATE INDEX artists_by_artwork  ON artists (artwork_id) WHERE artwork_id IS NOT NULL;
CREATE INDEX releases_by_artwork ON releases (artwork_id) WHERE artwork_id IS NOT NULL;
