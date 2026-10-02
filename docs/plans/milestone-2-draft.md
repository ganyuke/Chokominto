# Milestone 2 plan (draft for review)

Status: built on the `milestone-2` branch, not merged into `main` yet, so the owner can review it. The owner's answers to the questions below are in. Deleting labels is built too, on the whole-row undo that milestone 3's merges will use. Tested on an emulated Pi 4 (see "Performance" in architecture.md). Still open: resolver cases from the owner's real Maloja history, and a run on the real Pi with both real clients.

## Goal

Turn raw listens into songs, artists and albums automatically, and show rankings: top songs, artists and albums for any period, with the table layout from `docs/style.md`. No manual fixing yet (that's milestone 3), but everything is linked so fixing later is just changing links.

## Proposed split

M2 is big. Two halves, each shippable on its own:

**2a: entities and rankings**
1. Migration `0002`: artists, aliases, songs, recordings, releases, credits, release tracks, `recording_artists`, labels (with the default Character label per user), `jobs`, plus `recording_id`/`release_id` columns on `sources` and `listens`. Schema as in architecture.md.
2. Match keys: NFKC, case fold, katakana to hiragana, punctuation stripping. Exact kana-to-romaji table for `romaji_key`. (Kanji guesses with kagome wait for M3, where suggestions use them.)
3. Job queue (`internal/jobs`): durable, two workers, retries with backoff.
4. Resolver (`internal/resolve`): split rules (feat./ft./featuring), the CV rule, alias lookup, and create-if-missing. Every automatic link is recorded in the edit log as automatic. Ambiguous matches stay unlinked.
5. Credit expansion into `recording_artists` (credited vs via groups, "also counts for" chains, credit overrides table present but unused until M3).
6. Backfill: after upgrading, a job resolves every existing source. History keeps showing the text as received until each listen is linked.
7. Pages: Home (now playing, last 10 listens, top 10 songs/artists/albums this week), Top songs, Top artists, Top albums with period tabs and label filters, and simple Song, Artist and Album pages (names, listens per period, recent listens). History links names to those pages.
8. Performance: rankings under 150 ms at 500k listens, measured with the existing benchmark harness. Daily rollups only if needed.

**2b: the rest of M2**
1. Manual scrobbling page: search by any alias (match key, romaji key, prefix), "Scrobble now" or at a chosen time.
2. ListenBrainz stats endpoints for Pano's charts screen, backed by the same ranking queries.
3. Settings: week start (used by the Week tab), label management (rename, delete, hidden by default).

## Risks

- **Resolver quality decides how good M2 feels.** Plan: build a fixture file of real messy anisong strings from the owner's Maloja history (after importing it) and test the resolver against it before shipping.
- **Backfill on a Pi.** Tens of thousands of sources resolved once. Needs to run in the background in small transactions so scrobbling isn't held up. Measure under the ARM64 emulator and on the Pi.

## Open questions for the owner

1. Is the 2a/2b split fine, or should M2 ship as one?
2. Home page: rankings for this week, or this month?
3. Ranking page size: 50 or 100 rows per page?
4. When "Combine covers" is on, which recording counts as the original by default? The doc says the earliest release date, then the earliest first listen. Most anisong listens won't have release dates until MusicBrainz names come in M3, so in practice it's "the first one you listened to". OK for now?
5. Web Scrobbler sends `release_artist_name`. Use it as the album artist when present? (Proposed: yes, otherwise the track's main artist.)

Answered by the owner on 2026-09-30: 1 is moot since both halves are built. Home shows this week (2), 50 rows per page (3), the first recording listened to is the original (4), and the album artist is used when sent (5, added in migration 0003).
