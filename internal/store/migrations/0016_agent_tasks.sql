-- An agent's changes are grouped into tasks, so the owner (or the agent)
-- can undo a whole batch at once. An agent names one when it starts on
-- something. Otherwise one is started for it, and a new one after half an
-- hour without changes. agent_token_id is NULL for agents started with
-- chokominto mcp.
CREATE TABLE agent_tasks (
  id             INTEGER PRIMARY KEY,
  user_id        INTEGER NOT NULL REFERENCES users(id),
  agent_token_id INTEGER REFERENCES api_tokens(id),
  name           TEXT NOT NULL DEFAULT '',   -- '' when the agent didn't name it
  created_at     INTEGER NOT NULL,
  last_at        INTEGER NOT NULL,           -- its latest change
  ended_at       INTEGER,                    -- finished, or another started
  undone_at      INTEGER                     -- undone all at once
) STRICT;
CREATE INDEX agent_tasks_open ON agent_tasks (user_id, agent_token_id) WHERE ended_at IS NULL;

ALTER TABLE edits ADD COLUMN task_id INTEGER REFERENCES agent_tasks(id);
CREATE INDEX edits_by_task ON edits (task_id) WHERE task_id IS NOT NULL;
