-- A listen can be linked on its own, apart from the other listens sent with
-- the same text. fixed_by is the edit that did it. While it's set, the
-- listen keeps its own recording and album, and no longer follows its text.
ALTER TABLE listens ADD COLUMN fixed_by INTEGER REFERENCES edits(id);
CREATE INDEX listens_fixed ON listens (user_id) WHERE fixed_by IS NOT NULL;

-- Finding the text whose link was set by hand, for the Fixed links page,
-- without reading every text.
CREATE INDEX sources_linked ON sources (user_id, linked_by) WHERE recording_id IS NOT NULL AND linked_by IS NOT NULL;
