-- Tokens for AI agents, kept apart from scrobbler tokens: a scrobbler
-- token can't be used by an agent, or the other way round. "look" agents
-- can only read, "change" agents can also make edits.
ALTER TABLE api_tokens ADD COLUMN access TEXT NOT NULL DEFAULT 'scrobble' CHECK (access IN ('scrobble', 'look', 'change'));

-- The agent token an edit was made with, so Changes can say so.
ALTER TABLE edits ADD COLUMN agent_token_id INTEGER REFERENCES api_tokens(id);
