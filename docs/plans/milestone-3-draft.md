# Milestone 3 plan

Status: all five parts are built. In progress on the `milestone-3` branch, started from `milestone-2` since that isn't merged yet. The owner's answers are built in.

## Goal

Make every wrong or messy listen fixable from the page it shows on, and every fix undoable: relink a listen, merge duplicates, split a song that's really two, rename and edit things on their own page, and work through the backlog a Maloja import leaves in Review.

## Parts

Each part ends with the docs updated, tests passing and a commit.

**3a: everything goes through the edit log**
1. The edit log covers every table a fix can touch: artists, songs, recordings, albums, their names, credits, tracks, group members, "also counts for", credit overrides, rules, suggestions and label order.
2. Who gets credit is recalculated after every such change, and after its undo.
3. Merging an artist, song, recording or album moves everything attached to it onto the one it's merged into, as one edit. Old links to the merged item go to the one it was merged into.
4. Splitting a song moves chosen recordings to a new song.
5. The randomized undo test covers all of it.

**3b: Fix page and rules**
1. A Fix link on every listen row, to one page: the text received, what it's linked to now, search to relink or "New song", and delete.
2. Search finds names by English, romaji, original script and romaji typed for kana names. Kanji readings are guessed for search only.
3. After relinking, "Remember this?" offers up to three broader rules, shows how many listens each would change, and saving one is one undoable edit.
4. Linking several spellings at once. Moved to 3c, where Review's Unlinked section needs the same thing.

**3c: Review**
1. Merge suggestions, found in the background.
2. Review with Suggestions, Which one?, Unlinked and Incomplete, biggest first, with checkboxes for acting on many rows at once. Every bulk action is one edit.

**3d: item pages and Settings**
1. Pencil to rename. Labels added and removed on the page. Merging starts from the page.
2. Other names: add, remove, say which language each is, choose the one shown first.
3. Artist pages: "also counts for" and group members.
4. Song pages: which recording is the original, version names, split in two, credit overrides.
5. Album pages: kind, release date, the note that tells same-named albums apart, credit overrides.
6. Settings: create labels and set their order.

**3e: names from MusicBrainz**
1. The "Get names from MusicBrainz" button on artist, song and album pages. One edit, so one Undo removes it.

## Risks

- **Merges touch many tables.** A missed one leaves something pointing at a merged item. The undo test runs random merges and undos and checks the database ends up exactly as it started, and a check after each merge looks for anything still pointing at the merged item.
- **Review after a large import.** Years of history can give thousands of rows. Sorting by listens affected and bulk actions are what keep it workable. Checked against the owner's real Maloja history.
- **Reparsing after a rule change** can touch many sources. It runs in the background in small batches, like the backfill after upgrading.

## Owner's answers (2026-09-30)

- All four proposed page edits are wanted: other names, "also counts for" and members, original and versions, album details.
- The real Maloja history can be used, read-only and on a copy, for checking Review and building resolver cases. Nothing from it goes into the repo.
