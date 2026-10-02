# Milestone 4 plan

Status: built on the `milestone-4` branch, started from `milestone-3` since that isn't merged yet. Still to do: trying picture lookups against the real services (waiting on the owner's go-ahead to send names from this machine), and a look on the real Pi.

## Goal

Read the owner's real history right, and show album covers and artist pictures that never disappear or come out broken.

## Parts

**A: test cases from the owner's history** (done)
1. 629 cases from the owner's Maloja history, kept outside the repo, run with `CHOKOMINTO_HISTORY_CASES`. All pass.
2. The owner's answers changed how scrobbles are read: artist lists with commas and spaced slashes, titles in two languages, versions, groups named with their characters, one artist's take on a song, and covers named after the singer. See architecture.md, "Resolving".
3. Two bugs found on the way: a crash on a lone separator, and a crash inside a write leaving every later write waiting.

**Reading scrobbles in Settings** (done, owner's request)
1. Every way of reading scrobbles is a rule listed in Settings with an example, and can be turned on or off. The owner's own remembered rules are listed in words there too.

**4a: storing and serving pictures**
1. Pictures are checked (they really are images of a sensible size) and stored as clean copies with small and medium sizes. A crash can't leave half a file.
2. Served so browsers keep them for good. Choosing one is an undoable edit.

**4b: finding pictures**
1. Albums: Cover Art Archive when the album has an MBID, then iTunes (Japan first), then Deezer. Artists: Deezer.
2. Used on its own only when the title and an artist both match. Otherwise kept as candidates to choose from.
3. In the background, never while a page loads. Failed lookups are tried again later, and found pictures are never fetched again.

**4c: showing and choosing**
1. Small pictures in table rows (History, Home, rankings, item pages) and a cover in the infobox.
2. On the item page: the current picture, candidates to choose from, upload, and look again. A chosen or uploaded picture is never replaced by the background lookups.
3. Settings: remove pictures nothing uses anymore.

## Owner's answers (2026-09-30)

- Unsure test cases are asked in chat, in batches.
- Artwork covers album covers, artist pictures, and small pictures in table rows. No YouTube thumbnails for now.
- Before sending album and artist names to iTunes, Deezer and Cover Art Archive from the development machine, ask first.
