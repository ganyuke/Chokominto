-- Other names of artists, songs and albums are hidden unless the owner
-- turns them on in Settings.
ALTER TABLE users ADD COLUMN show_other_names INTEGER NOT NULL DEFAULT 0;
