-- The name pages show for an account. '' shows the account name, which
-- never changes and is what you log in with.
ALTER TABLE users ADD COLUMN display_name TEXT NOT NULL DEFAULT '';
