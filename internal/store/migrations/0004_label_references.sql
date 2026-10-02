-- migrate: rebuilds tables
-- Removing a label no longer takes it off things by itself. Everything
-- that changes labels goes through the edit log, which takes the label off
-- each thing as its own logged change first, so undo can put it back. A
-- cascade would silently drop tags added after an edit was planned, for
-- example when redoing a label deletion.

CREATE TABLE entity_labels_new (
  label_id    INTEGER NOT NULL REFERENCES labels(id),
  entity_type TEXT NOT NULL CHECK (entity_type IN ('artist', 'song', 'release')),
  entity_id   INTEGER NOT NULL,
  PRIMARY KEY (label_id, entity_type, entity_id)
) STRICT;
INSERT INTO entity_labels_new SELECT label_id, entity_type, entity_id FROM entity_labels;
DROP TABLE entity_labels;
ALTER TABLE entity_labels_new RENAME TO entity_labels;
CREATE INDEX entity_labels_by_entity ON entity_labels (entity_type, entity_id);
