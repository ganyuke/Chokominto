# Chokominto architecture

Developer-facing design for Chokominto. Implementation details belong here and in code comments, not in the UI or operator docs.

## Goals

- **Never lose a listen.** A scrobble that got a `200` is on disk. A batch is stored in full or not at all.
- **Raw data is sacred.** What the scrobbler sent is stored exactly and never modified. Everything else is derived from it and can be rebuilt.
- **Every edit can be undone.** Merges, links and credit changes go through one edit log.
- **Self-contained.** One Go binary, one SQLite file, one artwork folder. Runs on a Raspberry Pi 4 without Docker or other services.
- **Fast on a Pi.** Every page renders in under 150 ms of server time with 500k listens.

## Overview

```
 scrobbler ──► ListenBrainz API ─┐
 manual scrobble page ───────────┼──► ingest ──► SQLite ◄── web pages
 Maloja importer (CLI) ──────────┘      │           ▲
                                        ▼           │
                                   job queue ──► workers: resolve, suggest, artwork
                                                    │
                                                    ▼
                                             artwork folder
```

- **Ingest** validates and stores listens in one transaction, then returns. It does no network calls and no fuzzy matching.
- **Workers** do everything slow afterwards: linking listens to songs, finding merge suggestions and fetching artwork. Their work lives in a durable job table, so a restart never drops it.
- **Web pages** are server-rendered `html/template`, reading straight from SQLite. There's no cache layer. If a query is slow, fix the query or add a rollup table (see [Performance](#performance)).

## Stack

| Concern | Choice | Why |
|---|---|---|
| Language | Go (latest stable) | Owner knows it. Cross-compiles to `linux/arm64` with `GOOS/GOARCH`, no C toolchain |
| Database | SQLite via `modernc.org/sqlite` | Pure Go, no cgo, single file |
| HTTP | `net/http` with method + path patterns | Production-grade stdlib, no framework |
| Templates | `html/template` + `embed` | Contextual autoescaping, built into the binary |
| Passwords | argon2id (`golang.org/x/crypto/argon2`) | Current recommendation (RFC 9106) |
| Images | `image/jpeg`, `image/png`, `golang.org/x/image/webp`, `golang.org/x/image/draw` | Decode to validate, resize for thumbnails |
| Unicode | `golang.org/x/text/unicode/norm`, `golang.org/x/text/width` | NFKC and width folding for matching |
| Kanji readings | `github.com/ikawaha/kagome/v2` with its IPA dictionary | Guessed readings for search only. Pure Go. Adds a sizeable dictionary to the binary |
| Config | `github.com/BurntSushi/toml` | Small, stable |

No ORM, no web framework, no frontend build step. Queries are hand-written SQL in the `store` package. Kana-to-romaji is a small in-house table-driven converter rather than a dependency.

## Code layout

```
cmd/chokominto/          main and CLI subcommands: serve, user, token, import, backup, mcp
internal/config/         config file + env overrides
internal/store/          all SQL, migrations (embedded .sql files), transactions
internal/auth/           passwords, sessions, API tokens, login rate limiting
internal/ingest/         validate + store listens (shared by every entry point)
internal/listenbrainz/   ListenBrainz API handlers
internal/resolve/        clean/split/link rules, match keys, auto-linking
internal/edits/          merges and other multi-step edits (M3). The generic edit log and undo live in store
internal/suggest/        merge suggestions
internal/artwork/        fetchers, validation, storage, thumbnails
internal/jobs/           durable queue and worker pool
internal/maloja/         Maloja importer
internal/mcp/            MCP server for AI agents (chokominto mcp)
internal/web/            page handlers, templates/, static/ (embedded)
deploy/                  systemd unit, example config
docs/
```

## Data model

Three layers:

1. **Listens and sources:** what was received. Immutable apart from the derived link columns.
2. **Canonical entities:** artists, songs, recordings, releases. These are what you see and rank.
3. **Links, rules and edits:** how sources map onto entities, and the history of every change to that mapping.

All tables are `STRICT`. Every user-owned table carries `user_id` so multiple users are possible later. Times are Unix seconds, UTC.

### Listens and sources

```sql
CREATE TABLE sources (
  id            INTEGER PRIMARY KEY,
  user_id       INTEGER NOT NULL REFERENCES users(id),
  artist_text   TEXT NOT NULL,
  title_text    TEXT NOT NULL,
  album_text    TEXT NOT NULL DEFAULT '',   -- '' when none was sent
  album_artist_text TEXT NOT NULL DEFAULT '', -- Web Scrobbler's release_artist_name, '' when none was sent
  msid          TEXT NOT NULL UNIQUE,       -- random UUID, returned to clients as recording_msid
  recording_id  INTEGER REFERENCES recordings(id),  -- current link, NULL = unlinked
  release_id    INTEGER REFERENCES releases(id),
  linked_by     INTEGER REFERENCES edits(id),       -- the edit that set the link
  UNIQUE (user_id, artist_text, title_text, album_text, album_artist_text)
) STRICT;

CREATE TABLE listens (
  id            INTEGER PRIMARY KEY,
  user_id       INTEGER NOT NULL REFERENCES users(id),
  listened_at   INTEGER NOT NULL,
  source_id     INTEGER NOT NULL REFERENCES sources(id),
  recording_id  INTEGER REFERENCES recordings(id),  -- copy of sources.recording_id
  release_id    INTEGER REFERENCES releases(id),    -- copy of sources.release_id
  origin        TEXT NOT NULL,     -- 'listenbrainz', 'manual', 'import:maloja'
  token_id      INTEGER REFERENCES api_tokens(id),
  received_at   INTEGER NOT NULL,
  incomplete    INTEGER NOT NULL DEFAULT 0,  -- artist or title missing, or timestamp out of range
  deleted_by    INTEGER REFERENCES edits(id),  -- deleted listens are hidden, never removed
  UNIQUE (user_id, listened_at, source_id)
) STRICT;
CREATE INDEX listens_by_time      ON listens (user_id, listened_at);
CREATE INDEX listens_by_recording ON listens (recording_id, listened_at);
CREATE INDEX listens_by_release   ON listens (release_id, listened_at);

CREATE TABLE listen_payloads (
  listen_id INTEGER PRIMARY KEY REFERENCES listens(id) ON DELETE CASCADE,
  payload   TEXT NOT NULL          -- the listen object exactly as received
) STRICT;
```

- **A source** is a distinct (artist, title, album, album artist) text exactly as received. Only Web Scrobbler sends an album artist, so for most text it's empty. Linking a source links every past and future listen with that text. So an exact-text merge is remembered automatically. The "remember this?" question is only about broader rules (see [Rules](#rules)).
- **Listens have their own id.** Two different songs in the same second are fine. The same source at the same second is a duplicate. That makes client retries and re-running an import harmless.
- **`listens.recording_id`** is a denormalized copy so rankings only touch `listens` plus small tables. It's updated in the same transaction as the source link, with one indexed `UPDATE ... WHERE source_id = ?`.
- **Payloads** live in a separate table to keep `listens` narrow for scans.
- **Deleting a listen** sets `deleted_by` through the edit log, so it can be undone like any other edit. Every query filters on `deleted_by IS NULL`, and the time and recording indexes are partial indexes with that condition. Listens hidden by one edit (the graveyard's counts, bringing a song back, undo) are found through `listens_by_deleted`, a partial index on `deleted_by IS NOT NULL`. Before it (2026-10-03), Review's graveyard list read every listen once per song and timed out on the real history.
- **`msid`** gives each source a stable id in the format ListenBrainz clients expect. Pano Scrobbler uses it to delete listens.

### Canonical entities

```sql
CREATE TABLE artists (
  id             INTEGER PRIMARY KEY,
  user_id        INTEGER NOT NULL REFERENCES users(id),
  kind           TEXT NOT NULL CHECK (kind IN ('person','group','other')),
  pinned_alias   INTEGER REFERENCES artist_aliases(id),  -- overrides the display order
  mbid           TEXT,
  artwork_id     INTEGER REFERENCES artwork(id),
  artwork_pinned INTEGER NOT NULL DEFAULT 0,
  merged_into    INTEGER REFERENCES artists(id),
  created_at     INTEGER NOT NULL
) STRICT;

CREATE TABLE artist_aliases (
  id         INTEGER PRIMARY KEY,
  artist_id  INTEGER NOT NULL REFERENCES artists(id),
  name       TEXT NOT NULL,
  lang       TEXT NOT NULL CHECK (lang IN ('en','romaji','original')),
  lang_set   INTEGER NOT NULL DEFAULT 0,    -- 1 = set by you or MusicBrainz, 0 = guessed
  match_key  TEXT NOT NULL,                 -- see Matching
  romaji_key TEXT,                          -- exact, from kana
  guess_key  TEXT                           -- guessed kanji reading, search and hints only
) STRICT;
CREATE INDEX artist_aliases_by_key    ON artist_aliases (match_key);
CREATE INDEX artist_aliases_by_romaji ON artist_aliases (romaji_key);
CREATE INDEX artist_aliases_by_guess  ON artist_aliases (guess_key);

CREATE TABLE group_members (
  group_id  INTEGER NOT NULL REFERENCES artists(id),
  member_id INTEGER NOT NULL REFERENCES artists(id),
  PRIMARY KEY (group_id, member_id)
) STRICT;

-- "Also counts for": listens credited to artist_id also count for target_id.
-- Covers character -> voice actor, persona -> person, project -> artist.
-- Any artist can have any number. Unrelated to labels. Followed transitively,
-- and a link that would create a cycle is refused on save.
CREATE TABLE artist_counts_for (
  artist_id INTEGER NOT NULL REFERENCES artists(id),
  target_id INTEGER NOT NULL REFERENCES artists(id),
  note      TEXT NOT NULL DEFAULT '',   -- display only: 'voice', 'persona', 'project', ...
  PRIMARY KEY (artist_id, target_id),
  CHECK (artist_id <> target_id)
) STRICT;

-- The composition. Covers and versions are recordings of the same song.
CREATE TABLE songs (
  id          INTEGER PRIMARY KEY,
  user_id     INTEGER NOT NULL REFERENCES users(id),
  pinned_alias INTEGER REFERENCES song_aliases(id),
  mbid        TEXT,                 -- MusicBrainz work
  merged_into INTEGER REFERENCES songs(id),
  created_at  INTEGER NOT NULL
) STRICT;

-- Same columns and indexes as artist_aliases.
CREATE TABLE song_aliases (
  id         INTEGER PRIMARY KEY,
  song_id    INTEGER NOT NULL REFERENCES songs(id),
  title      TEXT NOT NULL,
  lang       TEXT NOT NULL CHECK (lang IN ('en','romaji','original')),
  lang_set   INTEGER NOT NULL DEFAULT 0,
  match_key  TEXT NOT NULL,
  romaji_key TEXT,
  guess_key  TEXT
) STRICT;

-- One performance. "World Is Mine" by supercell and Alya's cover are two
-- recordings of one song.
CREATE TABLE recordings (
  id             INTEGER PRIMARY KEY,
  user_id        INTEGER NOT NULL REFERENCES users(id),
  song_id        INTEGER NOT NULL REFERENCES songs(id),
  version        TEXT NOT NULL DEFAULT '',   -- 'TV size', 'Alya cover', ...
  is_original    INTEGER NOT NULL DEFAULT 0, -- whose artist a combined song row shows
  duration_ms    INTEGER,
  mbid           TEXT,
  artwork_id     INTEGER REFERENCES artwork(id),  -- falls back to release art
  artwork_pinned INTEGER NOT NULL DEFAULT 0,
  merged_into    INTEGER REFERENCES recordings(id),
  created_at     INTEGER NOT NULL
) STRICT;

CREATE TABLE recording_credits (
  recording_id INTEGER NOT NULL REFERENCES recordings(id),
  artist_id    INTEGER NOT NULL REFERENCES artists(id),
  role         TEXT NOT NULL CHECK (role IN ('main','featured','composer','lyricist','arranger')),
  position     INTEGER NOT NULL,
  credited_as  TEXT,                -- the name as printed, if it differs
  PRIMARY KEY (recording_id, artist_id, role)
) STRICT;

CREATE TABLE releases (
  id             INTEGER PRIMARY KEY,
  user_id        INTEGER NOT NULL REFERENCES users(id),
  pinned_alias   INTEGER REFERENCES release_aliases(id),
  kind           TEXT NOT NULL CHECK (kind IN ('album','single','ep','soundtrack','video','other')),
  context        TEXT NOT NULL DEFAULT '',   -- 'Star Citizen', shown to tell same-named releases apart
  released       TEXT,                       -- ISO date, any precision
  mbid           TEXT,
  artwork_id     INTEGER REFERENCES artwork(id),
  artwork_pinned INTEGER NOT NULL DEFAULT 0,
  merged_into    INTEGER REFERENCES releases(id),
  created_at     INTEGER NOT NULL
) STRICT;

-- Same columns and indexes as artist_aliases.
CREATE TABLE release_aliases (
  id         INTEGER PRIMARY KEY,
  release_id INTEGER NOT NULL REFERENCES releases(id),
  title      TEXT NOT NULL,
  lang       TEXT NOT NULL CHECK (lang IN ('en','romaji','original')),
  lang_set   INTEGER NOT NULL DEFAULT 0,
  match_key  TEXT NOT NULL,
  romaji_key TEXT,
  guess_key  TEXT
) STRICT;

CREATE TABLE release_credits (
  release_id INTEGER NOT NULL REFERENCES releases(id),
  artist_id  INTEGER NOT NULL REFERENCES artists(id),
  position   INTEGER NOT NULL,
  PRIMARY KEY (release_id, artist_id)
) STRICT;

CREATE TABLE release_tracks (
  release_id   INTEGER NOT NULL REFERENCES releases(id),
  recording_id INTEGER NOT NULL REFERENCES recordings(id),
  disc         INTEGER NOT NULL DEFAULT 1,
  position     INTEGER,
  PRIMARY KEY (release_id, recording_id)
) STRICT;

-- "Here, listens credited to from_id go to exactly these artists."
-- Replaces the default expansion of from_id (group members and/or
-- "also counts for") for one recording or release. Covers lazy group
-- credits, character recasts, and anything else that differs per song.
CREATE TABLE credit_overrides (
  scope     TEXT NOT NULL CHECK (scope IN ('recording','release')),
  scope_id  INTEGER NOT NULL,
  from_id   INTEGER NOT NULL REFERENCES artists(id),
  to_id     INTEGER NOT NULL REFERENCES artists(id),
  PRIMARY KEY (scope, scope_id, from_id, to_id)
) STRICT;
```

- **Nothing is deleted on merge.** The losing entity gets `merged_into`, and every record attached to it is rewritten to point at the winner: aliases, sources and listens, credits, recordings under a song, release tracks, labels, "also counts for" links, credit overrides and link rules. Each rewrite is a logged change in one edit, so one Undo reverses the merge. Where the winner already has the same row (the same name, label or credit), the loser's copy is removed as a logged delete instead of creating a duplicate. A moved "also counts for" or member link that would point at itself or make a loop is dropped the same way. Derived tables (`recording_artists`, listen totals, cached names) are rebuilt after. Queries never follow `merged_into` chains. It exists for redirecting old URLs (with a temporary redirect, since undo can bring the page back) and for undo.
  - **The winner keeps its name.** If a moved name would come first in display order, the winner's current name is pinned.
  - **Blanks are filled in** from the loser: MBID, and an album's release date.
  - **Text moved by a merge counts as linked by you** (`linked_by` is the merge), so a reparse never moves it back.
  - **Merging two recordings of different songs** also merges the loser's song into the winner's, when the loser was the only recording of it. That's how "Same recording" in Review joins アイドル and Idol.
- **Splitting a song** moves the chosen recordings to a new song with the same name. The new song and its name are logged whole rows, so Undo removes them again. Splitting off every recording is refused.
- **Editing is on the item's own page.** Logged in, an artist, song or album page has a pencil next to its name to rename it to anything. It replaces the name, and nothing else changes. Scrobbles already linked keep going to the same item, because links follow the exact text received, not the name. If the new name wouldn't come first in display order, it's pinned. Renaming to one of the item's other names pins that one instead. Each change on the page is one edit with its own Undo. The Edit tab has:
  - **All names:** add and remove names (the last one stays), say which kind each is (English, romaji, original script, which is then never guessed again), and choose the one shown first.
  - **Labels:** add any label, in the order set in Settings, or take one off.
  - **Artists:** type (person, group, other), "also counts for" with a note like voice or persona, and members. A link that would go round in a circle is refused.
  - **Songs:** each recording's version name, which recording is the original, and splitting checked recordings into a new song.
  - **Albums:** type, release date (a year, year and month, or full date) and the note that tells same-named albums apart.
  - **Who gets credit:** credit overrides, per recording on song pages and for the whole album on album pages.
  - **Merge:** find another item of the same kind by any name and merge into it. The page then leads to that one.
- **Same-named songs can be kept apart.** Auto-linking treats the same artist and title as one song (see [Resolving](#resolving)). When that's wrong, like Pedro Macedo Camacho's "Main Theme" for Star Citizen and for another game, the owner splits it into two songs, each on its own release. `releases.context` shows which is which wherever the title alone is ambiguous.
- **YouTube MV-only songs** are recordings with no release. They can have their own artwork.
- **Combined song rows** (covers pooled) show the artist of the recording marked `is_original`. If none is marked, it's the recording with the earliest release date, then the earliest first listen. You can mark a different one on the song page.

### Names

Every artist, song and release can have names in several forms. Owner's display order: **English, then romaji, then original script**.

- **Primary name:** the pinned alias if there is one, otherwise the first alias in display order. Shown aliases come before hidden ones, so a hidden alias is never the primary unless pinned. Pinning is per entity, from its page.
- **Shown or hidden** (owner, 2026-10-02): each alias has `shown`. Shown aliases after the primary are the entity's byline, listed under the title on its own page, separated by " · " (cached in `byline`). One alias is shown under the entity's name in tables and rankings (cached in `other_names`): the owner's choice (`second_alias` with `second_set = 1`, NULL there meaning none), or else the first shown alias after the primary. Both only show with Settings' "Show other names" on. Hidden aliases still link new scrobbles and find suggestions, which the Edit tab says. Merges move the loser's aliases hidden, since they're mostly other spellings players sent, like YouTube titles or file names (the Fukashigi no Carte page had dozens). Migration 0015 hid aliases moved by earlier merges that weren't undone.
- **Telling English from romaji:** a Latin alias is `romaji` if it equals the romanization of one of the entity's kana aliases, or if MusicBrainz marks it as a Japanese transliteration. Otherwise it's `en`. Anything else is `original`. You can correct it, which sets `lang_set` so it's never guessed again.
- **Name as sent:** tables that show individual listens (History) use the linked names, and the Fix page always shows the exact text received.

### Matching

Every alias has up to three search keys:

| Key | How | Used for |
|---|---|---|
| `match_key` | NFKC → case fold → katakana to hiragana → drop whitespace and punctuation (brackets, quotes, `・`, `-`, `~`), keeping `ー` | Auto-linking, suggestions, search |
| `romaji_key` | Only for aliases whose letters are all kana: exact Hepburn romanization via an in-house table, then long vowels folded (ō, ou, oo → o, ū, uu → u) and particles accepted both ways (は as ha or wa) | Search (`aidoru` finds アイドル), suggestions |
| `guess_key` | Kanji reading guessed with the `kagome` tokenizer (IPA dictionary), then romanized like `romaji_key`. Empty when a kanji word isn't in the dictionary | Search and weak suggestion hints only. Never shown, never used to auto-link, because readings of names are often wrong |

Search queries in Latin letters are matched against all three keys. Queries with kana are also romanized so they find Latin aliases. Recording search (Scrobble, Fix) also matches album names, and leaves out recordings no received text is linked to anymore, which a relink leaves behind.

The dictionary adds about 13 MB to the binary and takes about 90 MB of memory while loaded. It's loaded when a name is added and dropped after 5 minutes without one. Names from before milestone 3 get their `guess_key` from a background job after the update.

### Who gets credit for a listen

Owner decisions: an artist's listens also count for every artist in its "also counts for" list. A group's listens count toward the group and each member. A credit override on a recording or release replaces either of those for that song.

The expansion is materialized so ranking queries stay simple:

```sql
CREATE TABLE recording_artists (      -- derived, rebuilt when credits change
  recording_id INTEGER NOT NULL REFERENCES recordings(id),
  artist_id    INTEGER NOT NULL REFERENCES artists(id),
  via          TEXT NOT NULL CHECK (via IN ('credited','group')),
  PRIMARY KEY (recording_id, artist_id)
) STRICT;
```

For each `main` or `featured` credit on a recording:

1. The credited artist gets `credited`.
2. Expand the artist: its group members get `group`, and its "also counts for" targets get the same `via` as the artist they came from. Keep expanding from each new artist, skipping any already visited.
3. **Overrides:** before expanding an artist, look for a credit override for it on the recording, then on the recording's release. If one exists, its `to` list replaces that artist's members and "also counts for" targets for this recording. The `to` artists get `group` if the overridden artist is a group, otherwise `credited`, and are expanded in turn.
4. If an artist qualifies both ways, `credited` wins, so a listen is never counted twice for one artist.

Examples: a lazy "結束バンド" credit on a solo song gets an override to just that one member. A recast character gets a release-level override pointing at the voice actor for that release.

Rebuilds are triggered by any change to credits, group members, "also counts for" or overrides, and only touch the affected recordings.

Characters are ordinary artists with an ordinary artist page. Whether they show in rankings is controlled by labels (below). Composer, lyricist and arranger credits show on pages but don't count toward rankings.

### Labels

Labels are the owner's own tags on artists, songs and releases, like "Character", "Game soundtrack" or "Vocaloid". Their main job is filtering rankings.

```sql
CREATE TABLE labels (
  id            INTEGER PRIMARY KEY,
  user_id       INTEGER NOT NULL REFERENCES users(id),
  name          TEXT NOT NULL,
  hide_default  INTEGER NOT NULL DEFAULT 0,  -- hidden from rankings unless unchecked
  UNIQUE (user_id, name)
) STRICT;

CREATE TABLE entity_labels (
  label_id    INTEGER NOT NULL REFERENCES labels(id),
  entity_type TEXT NOT NULL CHECK (entity_type IN ('artist','song','release')),
  entity_id   INTEGER NOT NULL,
  PRIMARY KEY (label_id, entity_type, entity_id)
) STRICT;
```

- **No special labels.** Every label is an ordinary row that can be renamed, deleted, or have its default changed.
- **Created and ordered in Settings.** The Labels section of Settings is where labels are added, renamed, deleted, set hidden by default, and moved up or down. That order (`labels.position`) is used everywhere labels are listed: item pages, the "Hide" row on rankings, and the choices when labelling something. Labels from before milestone 3 start in name order.
- **Put on things from their page.** An artist, song or album page lists its labels and, when logged in, lets you add any existing label or take one off. Each is one undoable edit (a whole-row insert or delete on `entity_labels`).
- **Default labels:** creating a user also creates a "Character" label with `hide_default = 1`. Having that label is the only thing that makes an artist a character, and it only affects ranking visibility.
- **Labels and "also counts for" are independent.** Neither sets, requires or changes the other.
- **Ranking checkboxes:** every ranking page has a "Hide" row with a checkbox per label that's in use for that ranking. Checkboxes start from each label's `hide_default`. It's a plain GET form, so the choice is in the URL and shareable.
- **Showing characters** (unchecking the box) lists them alongside the artists they count for. Their listens then show up under both, which is expected when you ask to see both.
- Label changes go through the edit log like everything else. Deleting a label takes it off everything that has it and removes it, as one edit, so one Undo brings both back.

### Artwork

```sql
CREATE TABLE artwork (
  id         INTEGER PRIMARY KEY,
  sha256     TEXT NOT NULL UNIQUE,     -- hex, of the stored full-size file
  format     TEXT NOT NULL CHECK (format IN ('jpeg','png')),
  width      INTEGER NOT NULL,
  height     INTEGER NOT NULL,
  origin     TEXT NOT NULL,            -- 'upload', 'coverartarchive', 'itunes', 'deezer'
  origin_url TEXT,
  created_at INTEGER NOT NULL
) STRICT;
```

Artists and albums have `artwork_id` and `artwork_pinned` (milestone 4, migration 0007). Recordings have no picture of their own for now (owner, 2026-09-30: no YouTube thumbnails yet), so songs show their album's cover.

- **Choosing is an edit.** Showing a picture is a logged change of `artwork_id` and `artwork_pinned`, so Undo works. Lookups make automatic edits and never touch a pinned picture. `artwork` rows and files aren't logged, and are only removed by the cleanup in Settings, which leaves alone any picture an edit in the log could bring back.
- **Merging** gives the winner the loser's picture when it has none.

See [Artwork pipeline](#artwork-pipeline).

### Edits and undo

```sql
CREATE TABLE edits (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id),
  kind       TEXT NOT NULL,   -- 'link', 'merge', 'credit', 'rename', 'alias', 'rule', ...
  summary    TEXT NOT NULL,   -- shown in merge history: "Merged 12 listens into World Is Mine"
  automatic  INTEGER NOT NULL DEFAULT 0,      -- 1 = done by a rule or auto-linking
  rule_id    INTEGER REFERENCES rules(id),
  created_at INTEGER NOT NULL,
  undone_at  INTEGER
) STRICT;

CREATE TABLE edit_changes (
  edit_id  INTEGER NOT NULL REFERENCES edits(id),
  seq      INTEGER NOT NULL,
  op       TEXT NOT NULL CHECK (op IN ('insert','update','delete')),
  tbl      TEXT NOT NULL,
  row_key  TEXT NOT NULL,     -- JSON primary key
  before   TEXT,              -- JSON row or columns before
  after    TEXT,              -- JSON row or columns after
  PRIMARY KEY (edit_id, seq)
) STRICT;
```

- **Everything goes through the edit log.** Any change to links, entities, credits or aliases goes through the generic edit log in `internal/store` (`ApplyEdit`, `UndoEdit`), which writes the change and its `edit_changes` rows in one transaction. Derived tables (`listens.recording_id`, `recording_artists`) are rebuilt from the result and aren't logged.
- **Three kinds of change.** An `update` sets whitelisted columns of one row (`editable`). An `insert` adds a whole row and a `delete` removes one, for tables listed in `wholeRows` with their primary key and every column. A delete logs the whole row as it was, so undo puts it back exactly. `row_key` is the primary key as JSON with sorted keys, so composite keys (like `entity_labels`) work and equal keys give equal text.
- **Derived columns aren't logged.** Alias search keys (`match_key`, `romaji_key`, `guess_key`) are worked out from the name whenever an alias is added or renamed, so a better romanizer in a later version never makes an old undo stale. Cached entity names, `listens.recording_id` and `recording_artists` are recalculated once at the end of each edit (and each undo), for everything the edit touched.
- **Loops are refused.** Adding an "also counts for" or member link that closes a loop fails, including when an undo or redo would bring one back.
- **New entities.** Auto-linking creates artists, songs, recordings and albums outside the log, since undoing a link doesn't need them gone. Songs and recordings you create by hand (splitting a song, "New song" on the Fix page) are logged whole rows, so Undo takes them away again.
- **Nothing changes outside the log.** Foreign keys between logged tables never cascade. An edit that removes a row takes everything pointing at it off first, as its own changes. If something still points at a row when it's deleted, the delete is refused as stale rather than removing things unlogged.
- **Undo** applies the opposite changes in reverse `seq` order, but only if every affected row still equals its `after` (for a delete: is still gone). If a later edit touched the same rows, undo is refused and the page lists those later edits so they can be undone first. If the data changed some other way (a unique name taken again, a row it points at gone), undo is refused with a plain message. Undo is itself an edit, so it can be redone. Undoing an undo brings the edit it undid back into effect, and so on down the chain.
- **Tested** by a randomized test: random edits, label deletions, listen deletions and undos, some refused, then rewinding the log newest first must leave the database as it started.

### Rules

```sql
CREATE TABLE rules (
  id           INTEGER PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id),
  kind         TEXT NOT NULL CHECK (kind IN ('clean','split','cv','link','titles','version','group','covers')),
  field        TEXT CHECK (field IN ('artist','title','album')),  -- clean and split
  artist_match TEXT,          -- NULL = any
  title_match  TEXT,
  album_match  TEXT,
  match_mode   TEXT NOT NULL CHECK (match_mode IN ('exact','key','prefix','suffix','regex')),
  pattern      TEXT,          -- clean: what to strip, split: the delimiter
  recording_id INTEGER REFERENCES recordings(id),  -- link
  release_id   INTEGER REFERENCES releases(id),    -- link
  priority     INTEGER NOT NULL DEFAULT 0,
  enabled      INTEGER NOT NULL DEFAULT 1,
  created_by   INTEGER REFERENCES edits(id),
  created_at   INTEGER NOT NULL
) STRICT;
```

- **clean:** strips or rewrites part of a field before matching, like removing `【MV】` and ` (Official Music Video)` from titles sent by a particular YouTube channel.
- **split:** splits a field into several artists. Defaults are ` feat. `, ` ft. ` and ` featuring ` (case-insensitive), plus `(feat. X)` in titles, which make the rest featured. `, ` and ` / ` (with spaces) are also on by default (owner, 2026-09-30, from the real history) and make artists of equal standing, so "Asami Seto, Nao Toyama" is two main artists. A name suffix after a comma stays with its name ("Wilbert Roget, II", Jr., Sr.), and "and" before the last name of a comma list is dropped. Splits never cut inside brackets, so voices in "X (CV: a, b)" stay together. `&`, `×`, `、` and a bare `/` are off by default because they're common inside Japanese group and unit names ("MYTH & ROID", "Leo/need"). The owner can add them, optionally limited to specific artist strings.
- **cv:** reads character credits out of an artist name. It runs after splitting, on each artist name. It's on by default and, like every rule, can be edited or turned off. The default patterns, each matched in both full-width and half-width forms and with or without spaces:
  - `X (CV: Y)`, `X（CV：Y）`, `X (CV. Y)`, `X CV.Y`
  - `X (starring Y)`, `X (VO: Y)`

  For each match, X becomes (or matches) an artist with the Character label, Y becomes (or matches) an artist, and X gets "also counts for" Y with the note "voice". Lists like `A (CV: a), B (CV: b)` are split on `,`, `、` and `/` **only** when every part matches a CV pattern, so plain group names stay whole. `Y` itself can be a list (`X (CV: Y, Z)`) for duet voices.
- **link:** sends matching text straight to a recording (and release). Needed when the match is broader than one exact source, for example "this artist and title with any album". Without a release, the album is found or created from the album text like auto-linking does.
- **How rules match:** `artist_match`, `title_match` and `album_match` pick the text a rule applies to (NULL = any). For link rules, `match_mode` says how they compare: `exact`, `key` (match keys, so case, width and spacing don't matter), `prefix`, `suffix` or `regex`. Clean rule conditions always compare exactly, and `match_mode` says how `pattern` is removed from `field`: every occurrence (`exact`), at the start (`prefix`), at the end (`suffix`), or `regex`, which keeps the first group when there is one and otherwise removes every match. Clean rules run first, in priority order, then the first matching link rule. Rules made from "remember this?" get priority 10, ahead of the defaults.
- **Where links came from:** an automatic link made because of a rule records the rule in `edits.rule_id`. Undoing a rule (or redoing it) reparses the text it linked and the text it applies to, so undoing a rule also undoes what it did. Text linked by hand is never touched by a reparse.

- **titles, version, group, covers:** the readings of titles and group credits described under [Resolving](#resolving), as rules with no pattern so they can be switched off like any other. On by default.

**Reading scrobbles in Settings.** Every built-in reading is listed in Settings with an example of what it does, and a checkbox: featured artists (feat., ft., featuring together), lists with commas, spaced slashes, &, ×, 、 and a bare slash, character credits, groups named with their characters, titles in two languages, versions, and covers named after the singer. Readings that are off by default (&, ×, 、, bare slash) are added as rules the first time they're turned on. Below them, the owner's own rules (from "Remember this?", or split rules with other separators) are listed in words, with the listens each applies to, and can be turned off or deleted. Checking or unchecking readings changes nothing yet. Save opens a page that shows, for each reading switched, a few of the owner's own listens that would read differently, with how they read now and after (the soundtrack reading works on albums, so it has none). Only confirming there saves. The examples compare how the text is read, not where linking would put it, because working that out is the whole reparse. Saving several readings at once is one edit. Every change, and every undo of one, is one edit that reads all text not linked by hand again in the background.

**"Remember this merge?"** After you link a source by hand, the page offers up to three broader rules, built from the difference between the source text and the target:

1. **Any album:** a link rule on artist + title with any album. Not offered when the title is ambiguous for that artist, as with "Main Theme".
2. **Ignoring case, width and spacing:** a link rule with `match_mode = 'key'`.
3. **Titles like this:** when the target title appears inside the source title, the leftover prefix and suffix become a clean rule scoped to that artist text. `【MV】YOASOBI「アイドル」Official Music Video` linked to `アイドル` offers "Remove 【MV】YOASOBI「 and 」Official Music Video from titles by this artist".

Before a new or changed rule is saved, the page shows how many existing listens it would change: listens it applies to that aren't linked by hand and aren't linked there already. Saving applies it as one edit, so it can be undone, and reparses the text it applies to in the background. Only the rule's kind comes from the form. The rule itself is worked out again on the server.

## Ingest

`internal/ingest` is the only code that writes listens. The ListenBrainz API, manual scrobbling and importers all call it.

1. **Check structure first.** Invalid JSON, wrong types, a missing `payload`, or too many listens or bytes rejects the whole request with nothing written.
2. **Store everything else, even if incomplete.** An empty artist or title, or a timestamp before 2002 or more than a day in the future, is still stored with `incomplete = 1` and shows up in Review. It's never rejected. Web Scrobbler treats any 400 as a bad token and can send an empty artist (`artist_name: song.getArtist() ?? ''`). Rejecting a whole batch over one bad listen would make it look like the token broke, and would make it resend the batch forever.
3. **One transaction** per request, or per chunk of 5,000 for imports. For each listen:
   - upsert the source by its exact text
   - insert the listen with `ON CONFLICT DO NOTHING` (a duplicate is success)
   - insert the payload
   - copy the source's current link, if it has one
   - enqueue a `resolve` job for new, unlinked sources
4. **Commit, then respond.**

SQLite settings: WAL, `synchronous=FULL` (power loss on a Pi is realistic, and the write rate is tiny), `foreign_keys=ON`, `busy_timeout=5000`. All writes go through one writer connection, and reads use a small pool.

## ListenBrainz API

Served at `/1/…`, and also at `/apis/listenbrainz/1/…` and `/apis/lbrnz/1/…` so scrobblers set up for Maloja keep working after pointing them at Chokominto.

### What the target clients do

From reading the source of both clients (cloned at `../pano-scrobbler` and `../web-scrobbler`):

| | Pano Scrobbler (Android) | Web Scrobbler (browser) |
|---|---|---|
| Configured URL | API root, and it appends `1/<method>`. The root must end in `/` | The full submit URL, used as is |
| Auth header | `token <t>` (lowercase) | `Token <t>` |
| Checks the token | `GET validate-token` on setup. Needs `valid` and `user_name` | Never, for custom servers |
| Listen types | `single`, `import`, `playing_now` (with `?return_msid=true`) | `single`, `import` (up to 50 queued listens), `playing_now` |
| Duration field | `additional_info.duration_ms` | `additional_info.duration` (seconds) |
| Other fields | `submission_client`, `submission_client_version` | `submission_client`, `origin_url` (the page URL, e.g. the YouTube video), `release_artist_name`, `spotify_id` |
| Success check | `status == "ok"` | `status == "ok"`. Any 400 or 401 counts as a bad token |
| Reads back | Recent listens, now playing, listen count, charts, loves, following | Nothing |
| Deletes | `POST delete-listen` with `listened_at` + `recording_msid` | — |

Web Scrobbler's "love" button always goes to the real listenbrainz.org, even with a custom server, and sends the token there. Operator docs should say not to use love with a Chokominto account.

### Endpoints

Served at `/1/…`, and also at `/apis/listenbrainz/1/…` and `/apis/lbrnz/1/…` so scrobblers set up for Maloja keep working after pointing them at Chokominto.

| Endpoint | Milestone | Behavior |
|---|---|---|
| `POST submit-listens` | 1 | `single` (1 listen), `import` (1–1000), `playing_now` (1, no `listened_at`). Body limit 10 MB, 10 KB per listen. Response `{"status":"ok"}`, plus `recording_msid` when `return_msid=true` |
| `GET validate-token` | 1 | `{"code":200,"message":"Token valid.","valid":true,"user_name":…}` or `valid:false` |
| `GET user/{name}/listens` | 1 | `min_ts`, `max_ts`, `count` (default 25, max 1000). Payload `count`, `listens[]` (`listened_at`, `inserted_at`, `recording_msid`, `track_metadata`), `latest_listen_ts`, `oldest_listen_ts`. Pano's history screen |
| `GET user/{name}/playing-now` | 1 | Payload `count`, `listens[]` with `playing_now: true` |
| `GET user/{name}/listen-count` | 1 | Payload `count` |
| `POST delete-listen` | 1 | `listened_at` + `recording_msid` → soft delete through the edit log, undoable from the website |
| `GET stats/user/{name}/{artists,releases,recordings}` | 2 | Backed by the rankings. `range` is mapped to Chokominto periods |
| `GET stats/user/{name}/listening-activity` | 2 | Listens per day, week or month |
| Loves, following, metadata lookup | — | Return valid empty results so Pano's screens show "nothing here" instead of an error |

- **Auth:** the `Authorization` scheme is case-insensitive. `?token=` is also accepted on `validate-token`. Read endpoints are public when `public_pages` is on, like the website.
- **Returned track metadata** is the source text as received, plus the linked song, artist and album names under `mbid_mapping`-style fields where Pano reads them. Returned timestamps are Unix seconds.
- **Errors** use the ListenBrainz shape `{"code":N,"error":"…"}`, always as JSON (Web Scrobbler calls `response.json()` on every reply), with the real status code. Unknown methods return 404, not 200.
- **Now playing** is kept in memory per user and expires after the track's duration if sent, or 10 minutes otherwise. Home and History show it. It's not stored. To show the song's page, artists, album and cover, the text is looked up in `sources` read-only (`SourceLink`): the exact text first, then the same artist and title when they only ever went to one recording. Text never received shows as sent until its scrobble is linked.
- **CORS:** API routes send `Access-Control-Allow-Origin: *`. They're token-authenticated and never read cookies, so this is safe. Web Scrobbler sends an extension `Origin`, which is why API routes are also excluded from cross-origin protection.
- **Contract tests** replay payloads built from the two clients' serializers. Fuzz tests cover the JSON decoder.

Other ListenBrainz endpoints are out of scope.

## Resolving

A `resolve` job for a new source:

1. Apply enabled clean rules in priority order.
2. If a link rule matches, link and stop.
3. Split artist text with split rules, pull `(feat. X)` out of the title, then apply CV rules to each artist name. Then read the title (owner, 2026-09-30):
   - **Two titles:** "君のせい - Kiminosei", Japanese on the left of " - " and none on the right, is the song "君のせい" with "Kiminosei" as another of its names. Both are looked up, so either spelling finds the song.
   - **Versions:** a tail like "(Instrumental)", "(Off Vocal)", "(TV size)", "(LFZ Remix)", "(English ver.)", "(Remastered 2009)", "(Deluxe Edition)", "-Instrumental-", "- movie ver." or "- From THE FIRST TAKE" is the version. A title written twice around a dash ("Feel the winds(TV size) - Feel the Winds (TV Size)") is one title. The listen goes to that version of the song, made the first time it's heard, so versions are recordings of one song. Instrumental, off vocal and karaoke versions start with their own row in Top songs (`recordings.rank_alone`), and the rest count with the song when versions are combined. A dash tail with brackets of its own is another name, not a version.
   - **Groups named with their characters:** "平沢唯(CV:豊崎愛生), 秋山澪(CV:日笠陽子), 桜高軽音部" and "桜高軽音部 [平沢唯・秋山澪(CV:豊崎愛生、日笠陽子)]" credit the group. A new group gets the characters as members, and each character counts for their voice. In a character list joined with 、 or commas, a first name with a space where no other has one is a group and its first character ("B小町 ルビー(CV:…)、有馬かな(CV:…)"). Characters with only a space between them ("小糸 侑(CV:…) 七海燈子(CV:…)") are two characters.
   - **One artist's take:** a bracket naming a credited artist ("(Sayori)" when Sayori is credited) or a name and "Ver." at the end ("不可思議のカルテ 桜島麻衣 Ver.") is a version of the song, under the versions reading.
   - **Covers named after the singer** (rule kind `covers`): "Shōjo Rei (Cover) - Hoshimachi Suisei" and "フォニイ / 星街すいせい(Cover)" are the song "Shōjo Rei" or "フォニイ" when the trailing name is one of the names of the artist sent. In another script it's checked against the artist's names, and a title naming anyone else is read as sent. "Song (Cover)" alone loses the "(Cover)".
   - **Left as sent** (owner, 2026-09-30): fan edits like "(slowed + reverb)", "(8bit)" and "Nightcore - …", and one character with a group name in front ("B小町 有馬かな(CV:…)").
   - **YouTube-style titles** ("wowaka 『ローリンガール』feat. 初音ミク / wowaka - Rollin Girl (Official Video)") are left as they are, for the Fix page and "Remember this?".
4. Look up each artist name by `match_key` in `artist_aliases`, and the title in `song_aliases`, limited to recordings credited to those artists. If album text is present and several recordings fit, prefer the one on a release whose alias matches.
   - **One candidate:** link it, as an automatic edit.
   - **Several candidates** (only after the owner split a song, and the album doesn't pick one): leave it unlinked and add it to Review as "Which one?"
   - **Same artist and title on another album is the same song:** the recording is added to this album too. A single and the album it's on, OST editions with different names, and compilations are far more common than one artist reusing a title. The owner's Maloja history had 228 such pairs split into 513 songs under the opposite rule. The rare real duplicate (two games' "Main Theme" by one composer) is split by hand, and from then on the album decides which one a listen goes to.
   - **Album text that isn't an album:** YouTube view counts that Web Scrobbler sometimes sends as the album ("33M plays", "1,234 views", "2.1万回再生") are resolved as if no album was sent. The source keeps the text as received.
   - **"Unknown":** what players send when they don't know ("Unknown", "Unknown Artist", "Unknown Album", Android's "<unknown>", 不明なアーティスト) counts as nothing sent, as the artist, the album or the album artist. A listen with no other artist goes to Review like any listen without one. Updating to this re-reads received text like that in the background (migration 0009).
   - **Which recording of the song:** the one of this version with the same artists, or with one featured artist more or less ("siinamota" and "siinamota feat. Kagamine Rin"). Otherwise it's a new recording of the same song, like six characters singing one character's song.
   - **No candidate:** create artists, song, recording and (if album text is present) release, then link. Rankings work with no manual effort, and merge suggestions clean up later.
   - **Which album:** a release with that title credited to the album artist, else a new one. The album artist is the one the client sent (read with the same split and CV rules, main credits only), or the track's main artists when none was sent or nothing usable is left. "Various Artists" (and V.A., オムニバス) as the album artist counts as none sent.
   - **Soundtracks and compilations** (rule kind `compilation`, owner 2026-10-01): when no album with that title is credited to these artists and no real album artist was sent, the album with that title is used anyway and the track's artists are added to its credits. So a game soundtrack whose tracks name different composers is one album, credited to all of them in the order they were heard, and each composer's page can list it. Only for names that say soundtrack (OST, Soundtrack, サウンドトラック, Original Score, compilation…) or are at least 12 letters long as match keys, so "Best" by two bands stays two albums. Albums split before are suggested in Review ("Same album name, different artists").
5. Enqueue artwork and suggestion jobs for anything created.

Auto-linking only uses `match_key` (see [Matching](#matching)).

**Reparse** re-runs resolving for sources that were linked automatically. It's triggered by rule changes, or by hand. Sources you linked yourself are never changed by a reparse.

## Merge suggestions

The `suggest` worker compares entities of the same kind and records candidate pairs:

```sql
CREATE TABLE suggestions (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id),
  kind       TEXT NOT NULL CHECK (kind IN ('artist','song','recording','release')),
  a_id       INTEGER NOT NULL,
  b_id       INTEGER NOT NULL,
  reason     TEXT NOT NULL,        -- shown in Review: "Same title in kana and romaji"
  score      REAL NOT NULL,
  status     TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','accepted','dismissed')),
  created_at INTEGER NOT NULL,
  UNIQUE (kind, a_id, b_id)
) STRICT;
```

Signals, strongest first:

| Signal | Example |
|---|---|
| Same MBID | Set on both by hand or from a payload |
| Same match key on different entities | `ＹＯＡＳＯＢＩ` / `yoasobi` |
| `romaji_key` equals a Latin alias's `match_key` | `アイドル` → `aidoru` / `Aidoru` |
| Same credited artist, titles equal after removing bracketed or dash-separated suffixes | `アイドル` / `アイドル - Idol` / `アイドル (TV size)` |
| Same credited artist, duration within 2 s, one title contains the other | English vs kana titles on the same video |
| `guess_key` equals a Latin alias's `match_key` (weak, shown as "Possibly") | `青春コンプレックス` → `seishun konpurekkusu` |

English titles can't be matched from text at all. Those pairs only surface through duration, MBIDs, or names pulled from MusicBrainz. (Duration isn't used yet, since almost no scrobbles carry one.)

- **What's compared:** artists by any two names. Songs by their names, as a pair of recordings (each song's first recording): with an artist in common it's a likely duplicate, and with none only the same match key counts, as a possible cover ("Same title, different artists"). Albums only when they share an artist, since many albums have generic names.
- **When:** after linking, a `suggest` job looks through all names a minute later, so a burst of new listens is looked at once. Updating to milestone 3 queues one for what's there.
- **Pairs are stored once** (`a_id < b_id`). A stronger reason replaces a weaker one on an open pair. Pairs where either side was merged away are left out of Review.

For recordings, Review offers three answers: **Same recording** (merge), **Same song, different version** (move the recording under the other's song, which is how covers get pooled) and **Different**. Dismissed pairs are never suggested again.

## Review and fixing

Review is where the backlog lives. It must stay workable after importing years of Maloja history.

- **Biggest first:** every item shows how many listens it affects, and Review is sorted by that, so an hour of fixing covers the most listening.
- **Bulk:** every row has a checkbox. Answer "Same", "Same song, different version" or "Different" for all checked rows at once. Each bulk action is one edit, so one Undo reverts it. "Same" keeps the side with more listens. Pairs an earlier answer in the same batch already joined are skipped.
- **Bulk linking:** the Received text section searches all received text (linked or not, showing where each is linked now). Check several ("these 6 spellings"), search for the song and link them all to it as one edit. With no search it lists text that isn't linked. Auto-linking gives nearly all text a song, so most duplicates are text linked to separate songs, not unlinked text.
- **Sections:** Suggestions, Which one?, Received text, Incomplete. Each has a count, and empty ones are left out. Which one? lists each candidate with its albums, with a Link button each. Incomplete is text with no artist or title to go on, and listens stored as incomplete.

- **Graveyard** (owner, 2026-10-02): for songs that aren't music, like videos. Moving a song there (`songs.buried_by`, from its Edit tab or an agent) hides its listens through the same edit (`listens.deleted_by` = that edit), so they stop counting but nothing is lost. Triggers hide listens that arrive later or are linked to it afterwards. The last section of Review lists songs there with listens, with "Bring back" (an edit that restores every listen hidden by it, those that arrived later too) and "Delete listens" (moves them to a delete edit of their own, owner only). Undoing the move also restores the listens that arrived later. Songs in the graveyard aren't searched, suggested, linked to or merged.

**Cleaning up credits and albums** (owner, 2026-10-02), on the Edit tab and for agents, each one edit:

- **Credited artists** per recording: replaces its main and featured `recording_credits`, for a channel or voice actor sent in place of the character. Someone with no artist yet is added in the same edit (an `artists` row, logged whole), so one Undo takes both away. Other roles stay. A recording keeps at least one main artist.
- **Album artists:** replaces `release_credits`.
- **Take off this album:** for recordings on an album that isn't one ("4:00 AM"). Their sources lose the album and count as linked by hand (`linked_by` = that edit), so reading them again never puts it back, and the track rows go.
- **Delete** (owner: only when unused): an artist or album nothing uses is removed whole with its names, labels and album credits, as logged row deletes, so Undo brings it back. Anything still pointing at it (credits, tracks, listens, group or "counts for" links, remembered links, or other entities merged into it) blocks it, and the Edit tab says what. Songs aren't deleted, they go to the graveyard.

**Fix page** (`/listen/{id}/fix`), linked from every listen row on every page:

1. The exact text received (artist, title, album, album artist), plus the client and page URL if sent.
2. What it's linked to now, with names in display order.
3. A search box (all three match keys), starting from the title as received, to relink to another recording, or "New song" to split it off. Relinked text keeps its album, and the album gets the recording as a track. "New song" keeps the song's name and artists when the text was linked, and otherwise reads the text with the owner's rules. The new song is a logged row, so Undo removes it.
4. After relinking, the "remember this?" offers from [Rules](#rules).
5. Delete listen, as an undoable edit.

Relinking changes the **source**, so every listen with that exact text moves together. The page says how many.

## Agents (MCP)

AI agents can help the owner tidy their music, and answer questions about their listening, over the Model Context Protocol (owner, 2026-10-01), in three ways with the same tools (`internal/mcp`, written by hand with no SDK: `initialize`, `ping`, `tools/list`, `tools/call`):

- **`/mcp` on the website** ("Streamable HTTP"), for agents that can't run commands where Chokominto runs: a hosted server or container, or another machine. Each POST carries one JSON-RPC message and gets plain JSON back. Notifications get 202. There's no event stream and no session, since no tool sends anything later, and GET is 405. Agents authenticate with `Authorization: Bearer <token>` using agent tokens made in Settings, kept apart from scrobbler tokens (`api_tokens.access`: `scrobble`, `look` or `change`). Neither kind works for the other. A `look` token gets only the tools that look. A request with an Origin header other than the site's is refused, as the protocol asks against DNS rebinding. The path skips the website's cross-origin and cookie handling, like the ListenBrainz API. Edits made through it record the token in `edits.agent_token_id` (set through the request context in `store.WithAgent`), and Changes shows "(by <token name>)".
- **Signing in** (`internal/web/oauth.go`), for agents that can't take a token pasted in, like the Claude app's connectors (owner, 2026-10-02). This is MCP's authorization (2025-06-18 and later): /mcp's 401 carries `resource_metadata` pointing at the protected resource metadata (RFC 9728), which names Chokominto itself as the authorization server (RFC 8414 metadata). Apps register with dynamic client registration (RFC 7591, public clients only, `token_endpoint_auth_method` `none`) and use the authorization code flow with PKCE S256. `/oauth/authorize` needs a login and shows a page where the owner allows the app and picks look or change, with a "Don't allow" link back. Redirects must be https, http to loopback (any port, RFC 8252) or an app's own scheme, and match a registered one, or the page refuses without redirecting. The page's CSP adds the redirect's origin to `form-action`, or browsers stop the redirect after Allow. An approval becomes an `api_tokens` row like a Settings token (`client_id` set), so listing, revoking and the name in Changes work the same. Its `token_hash` is the current access token, which lasts an hour (`expires_at`), and refreshing rotates both it and `refresh_hash`. Codes last 5 minutes and work once. Anyone can register, so at most 100 apps may wait unconnected, and those older than a day are cleared hourly. Registration, token and metadata endpoints allow any origin and skip the website's protections, since they read no cookies.
- **`chokominto mcp`** on stdin and stdout, one message per line, for an agent that starts it itself, locally, over ssh or with `docker exec -i`. No token: only someone who can already run commands as the chokominto account can use it. It opens the same database as the running server, which is safe because of WAL and immediate write transactions. `-read-only` offers only the tools that look.

Tools look (search, search_sent_text, show_song, show_artist, show_album, review, recent_changes, top, listens_in, history) or change (answer_suggestions, merge, link_sent_text, set_version, set_own_row, split_song, add_name, set_name, remove_name, rename, set_counts_for, set_member, set_credits, set_album_artists, create_artist, take_off_album, delete, move_to_graveyard, bring_back, start_task, finish_task, undo_task, undo). **Listening** (owner, 2026-10-02): top, listens_in and history take a period like the ranking pages (a calendar week, month or year in the owner's time zone, containing `date`, or days `from` to `to`), and each result names the period it counted. top hides labels hidden by default unless asked. listens_in counts from the same yearly totals as the rankings (`EntityListenCountIn`), so a long span stays fast. history pages with an opaque `next` (time and listen id). review gives each suggested merge's id, and answer_suggestions answers them like Review's bulk buttons, as one edit. **Tasks** (owner, 2026-10-02): every edit an agent makes joins its open task (`edits.task_id`, table `agent_tasks`, per agent token, NULL for `chokominto mcp`), so a batch can be undone at once. The agent names one with start_task and ends it with finish_task. Without one, or 30 minutes after an unnamed one's last change, a new unnamed one starts by itself. Changes shows a task as one row where its newest edit is, with its edits listed when opened and Undo all. Undo all (and the agent's undo_task) undoes the task's edits in effect, newest first, in one transaction, skipping undos of its own edits, and the undos join the task. If an edit outside the task changed the same rows since, the whole transaction rolls back and Changes names those edits. Automatic edits and the owner's own never join a task.

Every tool carries `readOnlyHint`, and delete and move_to_graveyard carry `destructiveHint`, so apps like Claude list them apart and can ask before each. Deleting listens in the graveyard is left to the owner. show_song, show_artist and show_album list every name with its id, kind, and whether it's the main one, on the page or in lists. Changes call the same store functions as the web pages, so each is one edit in the log, shown on Changes and undoable, and the result gives the agent its edit id. Results are JSON text. Tool failures (bad arguments, merged ids, undo conflicts) come back as tool results marked as errors, so the agent can read them and try something else. `initialize` sends instructions explaining sent text, songs, recordings and versions.

## MusicBrainz names

A "Get names from MusicBrainz" button on artist, song and release pages. Nothing runs without the button.

1. If the entity has an MBID, fetch it directly. Otherwise search by the primary name, and let you pick from the matches (with disambiguation text), or cancel.
2. Store the MBID, and add every alias MusicBrainz lists with its locale (`ja` → `original`, `ja-Latn` → `romaji`, `en` → `en`) and `lang_set = 1`. Existing aliases are never removed or renamed.
3. For artists, optionally also offer group members and "also counts for" links that MusicBrainz knows, each as a checkbox.
4. The whole import is one edit, so one Undo removes it.

Requests go through the same safe fetcher as artwork (`internal/fetch`), at most 1 per second, with the required `User-Agent`: `Chokominto/<version> ( <public_url> )`, with the site's public URL as the contact when it's set.

- **Songs are works** in MusicBrainz, and albums are releases. A release's other names come from its release group, and an album without a release date gets the release's.
- **Which names:** the entry's own name (its kind guessed as usual) and every alias with a `ja`, `ja-Latn` or `en` locale. Aliases in other languages and search hints aren't used. A name the item has already only gets its kind set.
- **Artists:** members of a group ("member of band") and the person behind a persona ("is person", added as "also counts for" with the note "person") are offered as checkboxes when they're in the owner's listens. The rest are listed as not added. MusicBrainz has no artist-to-artist link for voice actors, so characters keep getting theirs from the CV rule.
- **Nothing from the form is trusted:** only the MBID and the checked links are posted. The names are looked up again when adding.

## Artwork pipeline

Fixes for each Maloja failure: images never expire, nothing unverified is saved, and lookups never block a page.

**Sources**, in order. None of them need API keys:

1. Cover Art Archive, when the release has an MBID
2. iTunes Search API with `country=JP` first, then `US`. Good coverage of Japanese releases. Results come with a 100 px picture, and the same address with 1200x1200 gives the large one
3. Deezer search, for releases and artist pictures. Artists without a photo get a blank placeholder, which is skipped
4. (Not yet, owner 2026-09-30) The YouTube thumbnail, for recordings with no release when the payload's `origin_url` is a YouTube link

Titles are compared as written and without shop suffixes (" - Single", " - EP"), one trailing bracket ("(Original Game Soundtrack)", "(アーティスト盤)") or soundtrack wording, with を read as "o" or "wo", and also by the romaji of kana names. On the shop's side, an artist list ("Yu-Peng Chen & HOYO-MiX") matches when one of its names does. A search result is used automatically only when its title matches one of the album's or artist's names and, for albums, its artist matches a name of one of the album's artists, comparing match keys (so ＴＨＥ ＢＯＯＫ ３ matches THE BOOK 3). A Cover Art Archive result is looked up by the album's own MBID, so it's used as is. Otherwise the candidates are shown on the entity page to choose from.

**Fetching:**
- Only allowlisted hosts (coverartarchive.org, *.archive.org, itunes.apple.com, *.mzstatic.com, api.deezer.com, *.dzcdn.net, musicbrainz.org). "*.example.org" allows that name and every name under it. Redirects are followed only to allowlisted hosts.
- A custom dialer rejects private, loopback and link-local addresses at connect time, which also closes DNS-rebinding holes.
- 15 s timeout, 10 MB body limit, a `User-Agent` with the project URL (MusicBrainz requires it), and at most 1 request per second per host.

**Validating and storing:**
1. Require status 200. Decode the full image (JPEG, PNG, WebP or GIF first frame). Reject anything that fails to decode or is smaller than 64 px or larger than 8000 px.
2. Re-encode as JPEG (quality 90), or PNG if it has transparency. This strips metadata and makes sure what's stored is a clean image.
3. Also write 64 px and 440 px versions (2× the thumb and cover sizes in the style spec), scaled so the shorter side fits and cropped square by the page. The size is checked from the file's header before decoding, so a small file claiming to be huge is refused without decoding it.
4. Write to a temp file, `fsync`, rename into `artwork/<ab>/<sha256>[-64|-440].jpg`. A crash can never leave a half-written image.
5. Serve at `/art/<sha256>[-64|-440].<jpg|png>` with `Cache-Control: public, max-age=31536000, immutable`. Private sites only serve them to the logged-in owner.

**When:** an `artwork` job per album and artist, queued when auto-linking creates one, and for everything there when updating to milestone 4. Lookups run on a job runner of their own, since each waits on other sites and linking new listens shouldn't wait on them. Each runner only takes the kinds of job it handles. A picture that's found is used as an automatic edit, results that don't match are stored as candidates (`artwork_candidates`) without downloading anything, and `artwork_lookups` records where each lookup stands. A result whose picture turns out broken is skipped for the next one.

**Switching it off:** Settings, under Pictures, switches looking online on (the default) or off. It's the only thing besides the MusicBrainz button that contacts other sites, and it sends album and artist names to iTunes, Deezer and Cover Art Archive.

**Retrying:** network errors retry after 1 h, 6 h, 1 d and then 7 d. "Not found" retries monthly, since releases get added to these services over time. Artwork is never refetched once found.

**Your choice wins:** uploaded or chosen artwork sets `artwork_pinned`, and workers never touch pinned artwork. Artwork files are only deleted by an explicit cleanup in Settings, never automatically.

## Jobs

```sql
CREATE TABLE jobs (
  id         INTEGER PRIMARY KEY,
  kind       TEXT NOT NULL,        -- 'resolve', 'suggest', 'artwork', 'rebuild_credits', 'backup'
  key        TEXT NOT NULL,        -- dedupe key, e.g. 'release:42'
  payload    TEXT,
  run_after  INTEGER NOT NULL,
  attempts   INTEGER NOT NULL DEFAULT 0,
  last_error TEXT,
  created_at INTEGER NOT NULL,
  UNIQUE (kind, key)
) STRICT;
```

Two runners, each with one worker: one for linking and Review (`resolve`, `reparse`, `guess_keys`, `suggest`), one for picture lookups (`artwork`), so a slow site never holds up new listens. A job is claimed and finished in transactions, so a crash mid-job just means it runs again. Each job must be safe to run twice.

- **Claimed in batches.** The linking runner claims up to 32 due jobs in one transaction and finishes the ones that worked in another, so a job costs one disk sync (its own work) instead of three. The picture runner claims one at a time. Jobs not started when the server stops are handed back at once rather than waiting out their 10-minute lease.
- **Claimed per kind.** The next due job is looked up once per kind through `jobs_kind_due (kind, run_after)`, and the oldest of those wins. One query over several kinds made SQLite sort every due job of all of them on each claim, which grows with the queue: 1 to 4 ms per claim with 8,000 queued, 10 s over sorting the owner's history.

## Web

- **Routes:** `/`, `/history`, `/top/songs`, `/top/artists`, `/top/albums`, `/song/{id}`, `/recording/{id}`, `/artist/{id}`, `/album/{id}`, `/listen/{id}/fix`, `/scrobble`, `/review`, `/changes`, `/settings`, `/login`. Merged entity ids redirect to the survivor.
- **No JavaScript needed** for reading, sorting and paging. Small vanilla scripts add search-as-you-type on Scrobble and inline actions on Review. All script and CSS are files under `static/`, with no inline code, so the CSP stays strict.
- **Live parts.** `static/live.js` keeps elements marked `data-live` current while the tab is visible: now playing on Home and History, Recent listens on Home, the newest History page, and the sorting-out note on Home and rankings. It asks `GET /live?parts=…` (same access as the pages) every 15 s, or every 5 s while the sorting note is up. The answer has each part rendered by the same template as the page, and the script only swaps a part whose HTML changed. The sorting note appears at 25 songs waiting (as before) and then counts down to zero, where it goes away. Older History pages don't update, so paging stays put.
- **Periods** are computed in the owner's time zone. `time/tzdata` is embedded so the Pi doesn't need system tzdata.
- **Manual scrobbling** searches `artist_aliases`, `song_aliases` and `release_aliases` by all three match keys, exact and prefix, then goes through `ingest` with `origin = 'manual'` and a synthetic payload. Being logged in is enough. There's no admin mode.

## Security

- **Accounts:** the first start of `serve` (or `import`) with no account makes one called `ruby` (owner, 2026-10-01). Its password comes from `CHOKOMINTO_PASSWORD` when set (like Maloja's `MALOJA_FORCE_PASSWORD`, for Docker or a service file), and is only used then. Otherwise it's made up (five groups of four letters and digits, about 99 bits) and written to the log once, so setup never waits at a prompt. Nothing on the web can create an account. The owner can change the password in Settings. `user add` adds more accounts and `user passwd` makes up a new password, printed the same way. Account names can't be changed (owner). There's no first-run web setup page that someone else could reach first.
- **Passwords:** argon2id (m = 64 MiB, t = 3, p = 4), stored as PHC strings.
- **Sessions:** 32 random bytes in the cookie, SHA-256 in the database, 30-day sliding expiry, `HttpOnly` and `SameSite=Lax`. When `public_url` is https, the cookie is `Secure` and uses the `__Host-` prefix.
- **CSRF:** `http.CrossOriginProtection` on every non-API route. API routes are excluded because they use tokens and never cookies.
- **API tokens:** 32 random bytes, shown once, stored as SHA-256, labeled per scrobbler, revocable, and last use recorded.
- **Login rate limit:** 5 failures per client IP per 15 minutes. The client IP comes from `X-Forwarded-For` only when the request comes from a proxy in `trusted_proxies`.
- **Headers:** `Content-Security-Policy: default-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'`, plus `X-Content-Type-Options: nosniff` and `Referrer-Policy: same-origin`. All artwork is served locally, so no third-party image hosts are needed.
- **Server limits:** `ReadHeaderTimeout` 10 s, read and write timeouts 60 s, and `MaxBytesReader` on every body (10 MB API, 16 MB uploads, 64 KB forms).
- **Logging:** structured `log/slog`. Payloads are never logged at info level, and tokens and cookies are never logged.
- **systemd hardening:** a fixed `chokominto` system account (not `DynamicUser`, so CLI commands run with `sudo -u chokominto` can share the database safely), `StateDirectory=chokominto`, `UMask=0077`, `ProtectSystem=strict`, `ProtectHome`, `NoNewPrivileges`, `PrivateTmp`, `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`, `MemoryDenyWriteExecute`, and a `SystemCallFilter=@system-service`.

## Configuration

A small TOML file, with each key overridable by a `CHOKOMINTO_*` environment variable. Everything else is a per-user setting in the database, edited on the Settings page.

```toml
listen          = "127.0.0.1:3939"    # ミク. Also differs from Maloja's 42010, so both can run during the switch
data_dir        = "/var/lib/chokominto"
public_url      = "https://music.example.org"
public_pages    = true
trusted_proxies = ["127.0.0.1/32", "::1/128"]
```

**Public pages** (the default) means anyone can see History, rankings, entity pages and now playing. Scrobble, Review, Changes, Settings and every edit need a login. With `public_pages = false`, everything except the API and `/login` needs a login.

## Backups and migrations

- **Daily backup:** `VACUUM INTO data_dir/backups/chokominto-YYYY-MM-DD.db`, keeping the last 14. `chokominto backup` does the same on demand. Artwork can always be refetched, and uploads are also copied under `backups/uploads/`.
- **Migrations** are numbered `.sql` files embedded in the binary, tracked with `PRAGMA user_version`, and run at startup, each in a transaction. A backup is taken before any migration. There are no down migrations. Restore the backup instead.
- **Safe to interrupt and to race.** Each migration commits on its own, so a stop mid-update resumes at the next one. Each checks `user_version` again inside its write transaction, so two processes opening the same database at once (the service and a CLI command) take turns instead of both running it. `serve` logs before and after updating, since a big history takes a minute or two on a Pi.

## Maloja import

`chokominto import maloja --db /path/to/malojadb.sqlite [--apikeys /path/to/apikeys.yml]`

- Reads Maloja's database file directly and read-only (`mode=ro`). Maloja's JSON export can't be used, because it leaves out the original scrobbles.
- For each Maloja scrobble:
  - **With a raw scrobble:** use its `track_artists` (one string, or several joined with `, `), `track_title` and `album_title` as the source text, and store the raw scrobble as the payload.
  - **Without one** (older or imported scrobbles): use Maloja's stored artists, title and album as the source text, and store a payload marking it as reconstructed.
  - `listened_at` is Maloja's timestamp, and `origin` is `import:maloja`.
- Goes through `ingest` in chunks of 5,000 and prints progress. Duplicates are skipped, so the import can be re-run safely, for example once more right before switching over.
- `--apikeys` imports Maloja's scrobbler keys as Chokominto tokens (hashed on import), so clients don't need new tokens.
- Everything then resolves through Chokominto's own rules. None of Maloja's merges or artist splits are carried over.

## Performance

- **Budget:** under 150 ms server time per page on a Pi 4 with 500k listens.
- **Measured:** benchmarks seed 500k listens (`internal/store/bench_test.go` for History, `internal/resolve/bench_test.go` for rankings). Counting listens directly took 300 to 650 ms per ranking on a desktop, so it was replaced by trigger-maintained totals:
  - `user_stats`: live listen count per user (History, Pano's listen count).
  - `listen_totals(user_id, recording_id, release_id, n)`: all-time counts.
  - `listen_years(user_id, year, recording_id, release_id, n)`: counts per UTC calendar year.
- **Triggers keep them exact** through inserts, deletes, undos and relinks, whatever code path makes the change. A randomized test compares them with real counts.
- **How rankings use them:** all time reads `listen_totals`. Other periods use `listen_years` for every UTC year at least half inside the period, add the listens in the period outside those years, and subtract the listens in those years outside the period. For a calendar year in the owner's time zone that means a few hours at each end. Weeks and months have no whole years and are counted directly (a few thousand rows). A randomized test checks rankings against brute force for many periods in Tokyo time.
- **Result:** every ranking takes 8 to 17 ms at 500k listens on a desktop, and History pages take about 0.5 ms.
- **Emulated Pi 4** (QEMU, 4 Cortex-A72 cores, 4 GB, Debian 13 arm64, 2026-09-30, synthetic 80k-listen Maloja history): Maloja import 2.5 min. Updating from milestone 1 ran the migrations in about 1 min, then linked the whole history in the background in about 12 min, with scrobbling, Pano's reads and every page working throughout (scrobbles took up to 0.5 s while linking, 25 ms otherwise). A reboot mid-linking resumed on its own. The emulator runs queries about 20 times slower than the desktop (History 11.7 ms against 0.58 ms), well beyond the roughly 5 times a real Pi 4 is, so its page times are only an upper bound. Real-Pi numbers are still to be measured.
- **Linking speed** is bound by disk syncs, not CPU, with `synchronous=FULL`. Each linked text was three commits (claim, link, finish). 5,000 texts took 3 min on a desktop SSD, using 33 s of CPU. Claiming and finishing in batches (see [Jobs](#jobs)) brought it down to about one commit per text. Linking also leaves alone listens whose link doesn't change, which on a reparse is most of them, so the count triggers only run for real changes.
- **Review on the owner's real history** (2026-09-30, desktop, a copy of the Maloja database: 39,126 listens, 8,363 texts, 3,187 artists, 6,943 songs, 3,732 albums): the import took 12 s and linking 40 s. The suggestion job found 871 pairs in about 0.4 s. Review loads in about 40 ms, and a text search in 80 ms (about 120 ms for a one-letter search that matches nearly everything). Getting there took three changes: unlinked text is found through indexes, received text keeps its search form in `sources.search_key` (worked out once, when it's first received), and names and listen counts are looked up once per page rather than per row. All-time Top songs took 50 to 65 ms on this history at the time.
- **Picture lookups on the owner's real albums** (2026-10-01, 50 albums: the 35 most listened and 15 at random, from the development machine with the owner's go-ahead): 35 found on their own, 14 kept to choose from, 1 not found. The first try found 23, before shop suffixes, artist lists and romaji were matched, and ran into iTunes' limit of about 20 searches a minute, so iTunes now gets one every 3 seconds (`Client.Pace`). The ones left to choose are mostly soundtracks named differently in each shop.
- **Kanji readings** add about 13 MB to the binary (23.5 to 36.2 MB) and 90 MB of memory while the dictionary is loaded. See [Matching](#matching).
- **Every page on the owner's real history** (2026-10-01, desktop, 39,126 listens, 239 pages covering every kind): all under 20 ms. Before that day's fixes, the slowest were Settings (66 ms), artist pages (37 ms), song pages (29 ms) and all-time Top songs (25 ms). The causes, worth avoiding in new queries:
  - **Newest-first scans.** "Recent listens of X" filtered with `recording_id IN (…)` makes SQLite walk the whole history by time until it finds enough, which for a rarely played artist is every listen. Item pages now start from the item's recordings (`listens_by_recording`), and first and last listened are a `min` and a `max` per recording. Both went from 15 and 5 ms to under 0.1 ms.
  - **Window functions over a whole ranking.** `RANK() OVER` and `count(*) OVER` sort every ranked row again, which took about 12 of Top songs' 15 ms of query time. Rankings now read the ordered list once and number it in Go (`pager`), then look up names and artists for the page only, in a couple of queries.
  - **Counting per received text.** Settings counted listens for all 8,000 texts to show numbers next to the owner's own rules. It now counts only the texts each rule applies to, and nothing at all when there are no such rules.

## Milestones

1. **Scrobble host (production-ready minimum):** store, migrations, auth (CLI user and tokens, login, sessions), ListenBrainz API including the read and delete endpoints Pano Scrobbler uses, History page, stylesheet, Maloja import, backups, systemd unit, operator README. At this point Chokominto can replace Maloja for receiving and viewing listens, on the website and in Pano Scrobbler.
2. **Rankings:** entities, auto-linking with split rules, credit expansion, labels and ranking filters, top songs / artists / albums, entity pages, manual scrobbling, ListenBrainz stats endpoints.
3. **Fixing metadata:** edit log and undo, Fix page, linking and merging, "remember this?" rules, Review with bulk actions, suggestions and "Which one?", credit overrides, covers pooled under songs, MusicBrainz names, editing on item pages (rename at minimum), creating and ordering labels in Settings and putting them on things from their page.
4. **Artwork:** fetchers, validation, storage, uploads, choosing from candidates.

## Testing

- **Resolver fixtures:** a table of real messy titles and artists (kana, romaji, English, YouTube MV titles, anisong credits) with expected results. The owner's own Maloja history is the best source for these.
- **ListenBrainz contract tests** from real Pano Scrobbler and Web Scrobbler payloads, and fuzzing on the decoder.
- **Undo property tests:** random sequences of edits and undos must leave the database identical to the start.
- **Migration tests:** each migration runs against a snapshot of the previous schema with data in it.
- **Benchmarks** against the seeded 500k-listen database.
