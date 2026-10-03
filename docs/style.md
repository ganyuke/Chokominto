# Chokominto style spec

This is the single source of truth for how Chokominto looks and reads. Every page follows it. If a page needs something this spec doesn't cover, extend the spec first, then build the page.

## Principles

1. **Reference-site boring, in chocolate mint.** The look is MyAnimeList and AO3 more than Wikipedia (owner, 2026-10-01: "a little too Wikipedia like"), not a streaming app. Pages are dark text and tables on a white background, with a mint top bar and chocolate brown headings, as the name says (チョコミント). No dark mode, cards, hero images, gradients, glows, shadows or animation. The one box besides the infobox is [Now playing](#now-playing), which the owner asked for (2026-10-02).
2. **Tables are the main component.** Listens, rankings and entity lists are all tables with real columns. Numbers line up.
3. **Dense but readable.** Lots of rows on screen, compact padding, generous line height for mixed Japanese and Latin text.
4. **One way to do each thing.** One button style, one table style, one notice style. No one-off variants.
5. **Works without JavaScript.** Reading pages, sorting and paging are plain links and forms. Script only adds convenience, like search-as-you-type and [live parts](#live-parts).
6. **Words earn their place.** See [Microcopy](#microcopy).

## Tokens

All colors, sizes and spacing come from these custom properties. Nothing in the stylesheet uses a raw color or size outside this block.

```css
:root {
  /* color: light, chocolate mint */
  --bg:           #ffffff;
  --bg-alt:       #f4faf8;  /* zebra rows, infobox */
  --bg-head:      #e6f6f1;  /* table headers, section bands, buttons, tabs */
  --bg-hover:     #e8f5f0;  /* row hover */
  --bar:          #a6e3d0;  /* top bar: mint */
  --bar-text:     #3d2418;  /* on the bar: chocolate */
  --heading:      #3d2418;  /* h1, h2: chocolate */
  --text:         #202122;
  --text-muted:   #54595d;
  --border:       #9ccfc0;  /* table and box lines: deeper mint */
  --border-light: #d3ece4;
  --link:         #0b6e63;  /* teal, readable on every background here */
  --link-visited: #5c4033;  /* milk chocolate, in prose only */
  --danger:       #b32424;
  --success:      #14866d;
  --focus:        #0b6e63;

  /* type */
  --font-sans:  system-ui, -apple-system, "Segoe UI", Roboto, "Noto Sans JP",
                "Hiragino Sans", "Yu Gothic UI", Meiryo, sans-serif;
  --font-mono:  ui-monospace, "SFMono-Regular", Menlo, Consolas, monospace;

  --text-base:  0.9375rem;  /* 15px, body and forms */
  --text-table: 0.875rem;   /* 14px, table cells */
  --text-small: 0.8125rem;  /* 13px, secondary info, second bar row */
  --text-h1:    1.75rem;
  --text-h2:    1.375rem;
  --text-h3:    1.0625rem;
  --leading:    1.5;

  /* space: 4px scale */
  --s1: 4px;  --s2: 8px;  --s3: 12px;  --s4: 16px;  --s5: 24px;  --s6: 32px;

  /* shape */
  --radius: 2px;
  --thumb:  32px;   /* artwork in table rows */
  --cover:  220px;  /* artwork in infobox */
  --playing: 96px;  /* artwork in the Now playing box */
  --page-max: 1200px;
}
```

There is one theme, and it's light. No dark mode, no `prefers-color-scheme` handling, no theme toggle.

All text/background pairs meet WCAG AA (4.5:1). Check any new pair before adding it.

## Typography

- **Body:** `--font-sans` at `--text-base`, `--leading`. Japanese fonts are in every stack so kana and kanji never fall back to a mismatched font.
- **Headings:** all sans. `h1` is bold, in `--heading`. `h2` is a section band like MyAnimeList's: bold, `--heading` text on a `--bg-head` band with a 2px `--border` bottom line, full width. `h3` is bold, plain. Date headings in History are `h2.day`, styled like `h3`.
- **Numbers:** every count, rank, duration and time column uses `font-variant-numeric: tabular-nums` and is right-aligned.
- **Weights:** only 400 and 700. Bold marks the current item (active tab, current page), never decoration.
- **Case:** sentence case everywhere, including headings, buttons and column headers. No all-caps.

## Layout

```
┌────────────────────────────────────────────────────────────┐
│ Chokominto   History  Songs  Artists  Albums   [Search   ] │  ← top bar (--bar)
│ Scrobble  Review  Changes  Settings        ruby  Log out   │  ← second row, --bg-head, --text-small
├────────────────────────────────────────────────────────────┤
│                                                            │
│  Page title (h1, bold)                                     │
│  ───────────────────────────────────────────────────────── │
│  content                                   ┌────────────┐  │
│                                            │  infobox   │  │  ← entity pages only
│                                            └────────────┘  │
└────────────────────────────────────────────────────────────┘
```

- Content is centered, `max-width: var(--page-max)`, with `--s4` side padding. At phone width the side gutter is 16px and nothing scrolls horizontally except wide tables, which scroll inside their own wrapper.
- The top bar holds the site name, the main sections and a search box. Right under it, a second row on `--bg-head` holds your own pages (Scrobble, Review, Changes, Settings) on the left and your account name with Log out on the right, or Log in for visitors. It wraps on narrow screens and never moves to the bottom (owner, 2026-10-01). There's no footer.
- Entity pages (song, artist, album) put an infobox on the right at ≥ 900px, above the content below that. See [Infobox](#infobox).
- On the home page, the three top-10 tables sit side by side at ≥ 1100px and stack below that.

## Components

### Links
- `--link`, no underline, underline on hover and focus. `--link-visited` only inside prose, not in tables (visited colors in a ranking are noise).
- Entity names in tables are always links to that entity.

### Names
- Pages show only the primary name. Other names are hidden unless the owner turns them on in Settings ("Other names", owner, 2026-10-01). Then they sit on a line under the primary name, in `--text-small` `--text-muted`, separated by a space: *Idol* over *アイドル Aidoru*. On item pages they're a muted line under the title.
- Display order is English, romaji, original script, unless a name is pinned on that entity.
- In tables, the other names get cut off with an ellipsis when space runs out. The primary name never does.

### Tables
The core component. One class, `.table`.

- Full width, `border-collapse: collapse`. Horizontal lines only: a 1px `--border-light` line between rows, and a 2px `--border` line under the header row. No vertical lines and no outer box.
- Header row: `--bg-head` background, bold, left-aligned (right-aligned over numeric columns).
- Cells: `--text-table`, padding `var(--s1) var(--s2)`.
- Zebra rows with `--bg-alt`, and `--bg-hover` on hover.
- Column types:
  - `.num`: right-aligned, tabular numbers (rank, listens, duration)
  - `.time`: right-aligned, tabular numbers, never wraps
  - `.thumb`: fixed `--thumb` wide, artwork only
  - `.fill`: the main text column, takes remaining width, wraps
  - `.wide`: a text column that shares space evenly with the other `.wide` columns (40% each), like Song and Artist in History
  - `.muted`: `--text-muted`, for secondary columns
- **Sortable headers** are links. The active sort column shows `▲` or `▼` after its label and is bold. No other header decorations.
- **Unresolved listens** (not yet linked to a song) show the raw artist and title in `--text-muted` italics.
- **Many artists in one cell** (a soundtrack by several composers): tables show the first three and "and N more" in `--text-muted`. The album page shows them all.
- **Album columns** in listen tables get 24% of the width, since soundtrack names are long.
- **Every listen row** ends with a "Fix" link in `--text-small` (logged in only). It's the same link whether the listen is linked, unlinked or wrong.
- **Bulk tables** (Review, search results used for linking) have a checkbox as the first column and the bulk action buttons directly above the table.
- **Key/value tables** (`.table.kv`) list the fields of one thing, like the text a listen was received with on the Fix page: a row header per field, its value next to it, as wide as the content needs.
- **Rows with one action** (search results to link or scrobble, offers under "Rules for similar listens") end with an `.action` column holding one button with a short label ("Scrobble", "Save rule"). A button that moves listens says how many ("Move 21 listens here").
- Wide tables go in `.table-scroll` so they scroll inside the page instead of the page scrolling.

### Artwork
- Square, 1px `--border-light` border, no radius, `object-fit: cover`.
- Row thumbnails are `--thumb` and the infobox cover is `--cover`.
- Missing artwork is a plain `--bg-alt` square. No placeholder icons or "no image" text.
- **Where:** a `.thumb` column after the rank or time in History, Home, every ranking, and the listen and song tables on item pages. Songs and listens show their album's cover. The infobox of an artist, album or song page starts with the cover.
- **Picture part of the Edit section** (artists and albums): the current picture at cover size, "You chose this one, so it stays." when the owner picked it, candidates as 110 px squares with their title, artist and a "Use this" button, "Look again" while lookups are on and nothing was chosen, and an upload field. Under the picture one line always says where the lookup stands: "Looking for one." or "Looking for other pictures." while it runs (the button is gone until it ends, and the page fills in what was found by itself), then the candidates, "No picture found. You can upload one.", "No other pictures found." or "The picture sites couldn't be reached. Look again later." With a picture that was found automatically, Look again keeps it and offers the others to choose from. Candidate pictures are served by Chokominto itself, never loaded from other sites.

### Infobox
- A box with `--bg-alt` background and 1px `--border`, like a Wikipedia infobox. At 900px and wider the page is a two-column grid, content on the left and the infobox (18rem) on the right, so tables never flow around it. Narrower, the infobox comes first.
- Cover at the top, then a two-column key/value table of reference details: type, release date, labels, members, also counts for, the albums a song is on. Listens aren't here (owner, 2026-10-01).

### Now playing
```
┌──────────────────────────────────────────────┐
│ ┌──────┐  Now playing                        │
│ │ art  │  Prologue                           │
│ │      │  柳川和樹                            │
│ └──────┘  Atelier Ryza … Original Soundtrack │
└──────────────────────────────────────────────┘
```
- On Home and History, under the title, while something is playing. A box like the infobox (`--bg-alt`, 1px `--border`), as wide as its content up to the page width.
- The album's cover at `--playing` on the left, then "Now playing" in `--text-small` `--text-muted`, the song in bold at `--text-h3`, the artists, and the album when there is one.
- When the song is already known, the song, artists and album are links and the cover is its album's. Otherwise the text is shown as sent, plain, with an empty square.

### Live parts
- With JavaScript, a few parts keep themselves current while the page is open: Now playing, Recent listens on Home, the newest History page, and the "Still sorting out your history" notice, which counts down and goes away at zero.
- A part changes only when there's something new. No spinners, highlights or animation.
- Without JavaScript the page is the same, as of when it loaded.

### Stats line
- Right under an artist's, song's or album's name: **listens** (bold, `--heading`, `--text-h3`), then in `--text-muted`, separated by " · ": its all-time rank as the ranking page counts it by default ("#1 album of all time", left out for items hidden by default, like characters), and when it was first and last heard ("heard 1 Oct 2026" when that's the same day). "No listens yet." when there are none.

### Tabs (period selector)
```
 Week │ Month │ Year │ All time │ Custom      ‹ Sep 2026 ›
```
- A row of links separated by `│`, the current one bold. No boxes, no underline bar.
- The previous/next arrows step through periods of the same length.
- "Custom" opens a pair of date inputs with a Show button.

### Ranking filters
```
 ☑ Combine versions   Hide: ☑ Character  ☐ Game soundtrack   [Apply]
```
- One line under the period tabs, with a native checkbox for each label used in that ranking and an Apply button. It's a GET form, so the filter is part of the URL.
- Each ranking only offers labels on its own kind of entity: artist labels on Top artists, song labels on Top songs, album labels on Top albums.
- Top songs also has "Combine versions", on by default. It counts covers and other versions with their song, except recordings marked to rank on their own row (instrumentals start marked).
- The line is left out entirely when there's nothing to offer.
- "Custom" shows a From and To date row with a Show button above it.

### Buttons
- One style: `--bg-head` background, 1px `--border`, `--radius`, `--text-base`, padding `var(--s1) var(--s3)`.
- Destructive buttons use the same style with `--danger` text.
- Always a text label. No icon-only buttons.
- Actions that are just navigation are links, not buttons.
- The one exception: an action that changes state but reads as navigation, like Log out in the top bar, is a `.link-button`, a button that looks like a link.

### Forms
- Native controls, 1px `--border`, `--radius`, `--bg` background, and `--focus` outline on focus.
- Labels above inputs. Help text, when needed, below the input (or below a lone button) in `--text-small` `--text-muted`.
- One primary action per form, at the bottom left.
- A choice of one among a few is a `fieldset.field` of radio buttons, its legend styled like a label and one option per line.

### Notices
- A one-line box with a 4px left border (`--success`, `--danger` or `--border`) on `--bg-alt`.
- Used for results of actions ("Merged 12 listens into World Is Mine. Undo") and errors. A result of an edit is the edit's own summary with an Undo button, and undoing returns to the same page. They never explain how anything works internally.

### Pagination
```
‹ Newer   Page 3   Older ›
```
Plain links. History pages by date, rankings by 50 rows.

### Focus and accessibility
- Every interactive element has a visible 2px `--focus` outline with 2px offset.
- Tables use real `<th scope>` headers. Sort links say the sort direction to screen readers.
- Nothing depends on color alone. The active tab is also bold, and errors also have text.
- No motion, so there's nothing to reduce.

## Page inventory

| Page | Main content |
|---|---|
| Home | Now playing box, the last 10 listens, top 10 songs / artists / albums this week |
| History | Listens grouped under date headings. Columns: time, artwork, song, artist, album |
| Top songs | Period tabs, "Combine versions" on/off. Columns: rank, artwork, song, artist, listens |
| Top artists | Period tabs. Columns: rank, artwork, artist, total, credited, via groups |
| Top albums | Period tabs. Columns: rank, artwork, album, artist, listens |
| Song / recording | Infobox, listens per period, recordings of this song (covers), recent listens |
| Artist | Infobox (aliases, labels, members or groups, also counts for, counted from), top songs, albums credited to them (soundtracks shared with other composers too), recent listens |
| Album | Infobox, track list with listen counts, recent listens |

Logged in, song, artist and album pages have two views, like Wikipedia's tabs (owner, 2026-10-01): **Read** (the page itself, at `/album/1`) and **Edit** (at `/album/1/edit`). Songs and albums have a third, **Scrobbles** (at `/album/1/scrobbles`, owner, 2026-10-03). The tabs sit on the right of the title line, the current one bold, in the `.tabs` style. Visitors see no tabs. The Edit view repeats the title and has plain `h3` subsections, one small form each, so every change is one click with its own Undo: names, labels (in the order set in Settings), the kind-specific details, who gets credit, the picture, names from MusicBrainz, and merging it into another. After a change it stays on the Edit view, with the change and its Undo in the notice.

**Coming back to where a button was used** (owner, 2026-10-03): on every page, a button that changes something brings the page back to the heading or row it sits under, not the top. Each section heading with a form under it has an `id`, and so do rows on Changes. The green and red notices stay at the top of the window while scrolling (`position: sticky`), so what happened and its Undo are in view after the jump, and `:target` leaves room for them. Undo on Changes comes back to the same page and row.

The names table has a Kind select per name, an "On the page" checkbox column, an "In lists" radio column with "No other name in lists" under it, and one "Save names" button for all three, with "Use as main name" and "Remove" per row. Help text under it says which names show where and that every name, shown or not, links new scrobbles and finds duplicates. Songs have "Credited artists" per recording, albums have "Album artists" and "Songs on it" (checkboxes, "Take checked off this album"). The last section is Graveyard on songs and Delete on artists and albums. Delete says in a sentence what still uses the item, or offers the danger button when nothing does.

- **Inline forms** (`.inline-form`): an input and its button on one line, wrapping on narrow screens. Used where a form has one or two fields, like adding a name.
- **Lists of removable things** (`ul.plain`): one item per line, each with its own small button after it ("Remove", "Take off").
| Scrobble | One search box, results table, "Scrobble now" or a time field |
| Fix listen | Received text and how often it was sent, Scope (all listens with this text, or only this one), Linked to (with how it was linked), Version (every version of the song), Song (search), Album, Correction (type what should have been sent), New song, Rules for similar listens, Delete. Every section says what it moves, with the numbers |
| Scrobbles (tab on song and album pages) | The received texts behind the item, one table per version on songs. Columns: checkbox, artist, title and album as received, listens, album, linked by, Fix. Checked rows can be moved to another version, another song, another album or no album |
| Fixed links | Every received text whose link was set by a person or an agent, so reading never moves it. Search, most listens first. Checked rows can be read again automatically |
| Review | Sections for suggestions, which one, received text (to link several spellings at once), incomplete, and the graveyard (songs that aren't music, with Bring back and Delete listens). Sorted by listens affected, with checkboxes for bulk actions |
| Connect an agent | Shown when an agent signs in. What it will reach, look or change as radio buttons, Allow, and a "Don't allow" link back to the agent |
| Changes | Every merge, deletion and other edit, newest first, with an Undo button each. Called "merge history" in early notes. An AI agent's task is one row: its name (or "Changes made together"), the agent, the number of changes, and Undo all, with its changes listed when the row is opened (`details`), each with its own Undo |
| Settings | Scrobbler tokens, time zone, week start, display name, labels (add, rename, order, hidden by default, delete), reading scrobbles (each reading with an example and a checkbox, one Save that first shows examples from your own listens and asks to confirm, then your own rules), password |

### Listen history mockup
```
History
────────────────────────────────────────────────────────────────
┌────┐ Now playing
│art │ Idol
└────┘ YOASOBI

 Tuesday, 30 September 2026
┌───────┬────┬───────────────────────────────┬────────────────────┬────────────────────┬─────┐
│  Time │    │ Song                          │ Artist             │ Album              │     │
├───────┼────┼───────────────────────────────┼────────────────────┼────────────────────┼─────┤
│ 14:02 │ ▢  │ World Is Mine ワールドイズマイン │ Alya               │ —                  │ Fix │
│ 13:58 │ ▢  │ Guitar, Loneliness and Blue P… │ Kessoku Band 結束… │ Kessoku Band 結束… │ Fix │
│ 13:51 │    │ 【MV】Foo - Bar (Official Mu…    │ FooChannel         │ —                  │ Fix │
└───────┴────┴───────────────────────────────┴────────────────────┴────────────────────┴─────┘
                                   ‹ Newer   Older ›
```

### Top artists mockup
```
Top artists
 Week │ Month │ Year │ All time │ Custom            ‹ 2026 ›
 Hide: ☑ Characters  ☐ Game soundtrack   [Apply]
┌────┬────┬───────────────┬───────┬──────────┬─────────────┐
│  # │    │ Artist        │ Total▼│ Credited │ Via groups  │
├────┼────┼───────────────┼───────┼──────────┼─────────────┤
│  1 │ ▢  │ Kessoku Band… │   812 │      812 │           0 │
│  2 │ ▢  │ Yoshino Aoya… │   640 │       38 │         602 │
└────┴────┴───────────────┴───────┴──────────┴─────────────┘
```

## Microcopy

- **Say what the person gets or does.** "Merged 12 listens into World Is Mine" rather than "Updated source mapping".
- **No implementation details** in any UI text: no table names, job queues, cache, IDs, library or service names. Artwork is just artwork, not where it came from.
- **Short.** Buttons are one or two verbs ("Merge", "Undo", "Scrobble now"). Headings are nouns ("Top songs").
- **Headings are short noun phrases**, like Wikipedia's (owner, 2026-10-03): "Scrobbles", "Received text", "Version", "Correction", "Related albums", "Rules". No questions and no "what", "how" or "why" headings. Table headers follow the same rule.
- **Say exactly what a change touches, with the real numbers and names** (owner, 2026-10-03): "Moves these 21 listens to the song you pick. Fukashigi no Carte (All Heroine ver.) keeps its other 35 listens." rather than "Linking one links them all". A button that moves listens says how many ("Move 21 listens here").
- **No lead-in sentences** (owner, 2026-10-03): nothing that only announces what follows ("Two separate choices. Make both before changing anything below."). A heading and its options are enough. A sentence stays only when it states a fact the labels don't.
- **Things don't act** (owner, 2026-10-03): no sentence where a song, scrobble, rule, page or name does something. Not "the song keeps its other 35 listens", "scrobbles go to the same song", "the rule moves listens", "this page leads there", "letters count as the same". Say what is done, in the passive or as an instruction: "the other 35 listens are not moved", "existing scrobbles are linked to the same song", "this page is redirected there", "letters are treated as the same". Labels that are actions start with the verb ("Match exact title and artist in future scrobbles (disregarding album)").
- **Words for scrobbles:** "scrobbles" or "listens" for what was heard, "received text" for the exact artist, title and album a scrobbler sent. A version with no name of its own is "Main version".
- **Empty states** are one sentence, with a link if there's an obvious next step: "No listens this week." / "No suggestions right now."
- **Errors** say what went wrong and what to do: "That password is wrong." / "Pick a song first."
- **No semicolons.** Use a period, a comma, or "and"/"or".
- **Numbers:** thousands separators (1,234), no decimals for counts.
- **Times:** 24-hour `14:02` inside date-grouped tables, full date `30 Sep 2026, 14:02` elsewhere, always in the configured time zone. No relative times like "3 hours ago" in tables.
- **Missing values** are an em dash `—`, never "Unknown" or "N/A".
