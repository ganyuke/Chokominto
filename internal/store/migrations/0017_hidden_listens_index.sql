-- Listens hidden by an edit (deleted, or in the graveyard) are found by
-- that edit: the graveyard's counts, bringing a song back, and undo.
-- Without this, each of those read every listen.
CREATE INDEX listens_by_deleted ON listens (deleted_by) WHERE deleted_by IS NOT NULL;
