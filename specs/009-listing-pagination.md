# Spec 009 — Listing Pagination: a page you can turn, name and come back to

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> Every listing ends in one button. "Load more" appends a page and forgets it:
> reload the tab and four pages of scrolling are gone, the rows only ever
> accumulate, and the page size is a constant nobody can change. This spec
> turns the listings into pages that are turned rather than grown — forwards,
> backwards, and to either end — without giving up the keyset pagination that
> makes page four hundred cost what page one costs.

---

## Overview

Deliverables:

- The read API pages **both ways**: `direction=next|prev` beside the existing
  opaque `cursor`, and `prev_cursor` beside `next_cursor`.
- An opt-in, **capped** count: `?count=1` answers with how many rows the
  filter matches, up to a cap, and says when it stopped counting.
- A pagination bar under every listing: rows per page, what is on screen
  against what matches, and « ‹ › » — newest, previous, next, oldest.
- The page lives in the URL, so a listing is reload-safe and sendable.
- CLI and MCP grow the same two capabilities in the same PR (REPOS §2).

Not here: page *numbers* and "of N" (Decision 3).

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-08-29** — Listings **turn pages** instead of accumulating them: a page replaces the rows on screen, and `limit`/`cursor`/`direction` live in the URL | "Load more" loses three things at once. The view is not reproducible — a reload drops every page after the first, which is the one place in this interface where state is not in the URL (spec 006's contract). The DOM only grows, and with the peek panel mounted over it (spec 008) a long scan is a long list plus a whole observation tree. And it can only move one way: there is no gesture for "back to where I was two pages ago". A window fixes all three and costs one parameter |
| 2 | **2026-08-29** — The API pages backwards with `?direction=prev`, keeping the same opaque `cursor`; responses grow `prev_cursor`. **The end anchors fall out of it**: no cursor with `direction=next` is the newest page, and no cursor with `direction=prev` is the oldest one | Keyset already reads both ways — `(timestamp, id) > (?, ?)` with `ORDER BY … ASC`, rows reversed on the way out, over the very index that serves the forward query (`idx_traces_timestamp`, spec 004 #4). So « ‹ › » are four buttons over one mechanism and one extra parameter, all four costing what page one costs. A separate `before=` parameter would have been a second cursor to keep consistent and a mutual-exclusion rule to enforce; `direction` also gives "the last page" for free, which offset pagination has to compute |
| 3 | **2026-08-29** — **No page numbers and no "of N"**: the bar shows how many rows are on screen, how many match, and the four arrows | The screenshot this came from shows "Page 1 of 1", and that is the one part that cannot be bought cheaply. `OFFSET` in SQLite counts and discards, so page 400 reads 20 000 rows — trading spec 004 #4's constant for a linear one, to render an ordinal. Measured on a 2 M-row table of our shape: an unfiltered `COUNT(*)` is 0.4–2.0 s, and a count on a column we do not index is 5.7 s — and `name`, `tag`, `status` and `min_cost` are exactly the unindexed filters somebody uses when hunting. Beyond cost, an ordinal is not an address in a feed sorted by time: nobody wants "page 40", they want "26 August", which the time range control already answers. And in a live listing the number lies — rows arrive between the count and the page |
| 4 | **2026-08-29** — Counting is **opt-in and capped**: `?count=1` adds `"total"` and `"total_capped"`, computed as `SELECT COUNT(*) FROM (… LIMIT 1001)`. ⚠️ **The cost claim below is wrong — see #12** | The reader's real question is "a handful, or thousands?", and the honest answer to it is bounded work: at most a thousand rows scanned on any filter, indexed or not, so the number appears at the same speed on every screen and no filter can turn the listing slow. Opt-in because the count changes with the *filters*, not with the page — the interface asks for it when the filter bar moves and not on every page turn — and because the CLI and MCP page without ever needing it. `total_capped` rather than a magic value: a client that wants to print "1000+" can, and one that wants the number can tell that it did not get it |
| 5 | **2026-08-29** — Rows per page is 25 / 50 / 100 / 250, in the URL, default 50 | The API already accepts `limit` 1–500 (spec 004); this exposes the part of that range a person picks from. 250 rather than 500 as the top step: a 500-row table is past what the peek panel's neighbour walk and the browser's own scrolling stay pleasant on, and the API keeps the wider range for the clients that page programmatically |
| 6 | **2026-08-29** — **‹ › move the listing; `j`/`k` move the panel** — and `j` on the last row of a page turns the page and lands on its first row (`k` symmetrically) | Two gestures that both "go to the next thing" have to mean different things or one of them is noise. The arrows in the bar belong to the table; the keys belong to the panel (spec 008 #13), and rolling over the edge is what keeps a scan from stopping at a boundary that is an artefact of paging. This amends spec 008 #7, which stopped the walk at the ends of what was loaded: with a window, "loaded" is a page, and stopping there would make the panel worse than it is today |
| 7 | **2026-08-29** — **Live mode runs on the first page only**; on any other page it is paused and the toggle says why | Live re-reads the newest page (spec 006 #12), which is meaningful for a window anchored at "newest" and meaningless for one anchored at a cursor — it would replace the page a reader deliberately navigated to with the top of the listing. Pausing rather than hiding the control, so that the state is explained rather than mysterious |
| 8 | **2026-08-29** — All three listings get the same bar: Traces, Sessions, and a session's traces | The bar is one component over the pagination all three already share, and a listing that pages differently from the one beside it is a listing somebody has to learn twice. The session's own table is where this matters most in practice: a long conversation is exactly the case "load more" served worst |
| 9 | **2026-08-29** — A live tick **replaces** the rows instead of merging into them, and `mergeRows` goes with its tests | Spec 006 #12 merged because the listing *accumulated*: a re-fetched first page overlapped rows already on screen, and folding by id was what kept a trace from appearing twice and a late arrival from jumping to the top. A window has neither problem — the page just fetched is, by definition, the newest N rows in the server's own order — and merging into it would grow it past the size somebody picked from the control right below it. Deleting the helper rather than leaving it: dead code with a passing test around it is the kind of thing that reads as load-bearing to whoever finds it next |
| 10 | **2026-08-29** — A session's own trace table pages in **component state**, not in the URL — the one listing that does | Decision 1 puts the page in the URL, and this is where that rule cannot hold: on `/sessions` the session's traces are rendered *inside* the peek panel, over a URL whose `limit` and `cursor` already belong to the sessions listing underneath. Two listings on one address cannot own one set of keys, and prefixing a second set (`trace_cursor=`…) would put a parameter in the contract to serve one screen's layout. Local state keeps them apart, and keeps `SessionDetail` identical in both of its homes — the panel and the full page — rather than reading the URL in one and not the other. What is lost is a link straight to page three of a session's traces, which is not a link anybody sends |
| 11 | **2026-08-29** — The UI-line budget moves from 8,300 to 8,700, and then to **9,000** after review | The number is the budget of the newest spec that moved it (spec 007 #13, spec 008 #14), and the code landed at 8,623: a bar, the URL helpers, and the tests for both. The second raise, +100, is the argument this decision said the next raise would have to be: it is two rounds of review turned into regression tests — the empty-page anchors, the walk that survives an abort, `isLastPage` and its cases. Those lines are the ones least worth trimming, and cutting comments to sit under a number I set myself would be gaming the budget rather than respecting it. A third round landed on 8,814 and was absorbed by trimming instead — its lines were my own restatement, which is a different thing from the reasoning the earlier rounds added. A fourth put it at 8,826, and that overflow is a regression test. The fifth rebuilt the walk (Decision 13) and spent the hundred lines that were left: **9,000**, which is where this row stops, because it is design §8's own ceiling. Five rounds of review turned roughly 300 lines of this spec into tests that hold what they found, and cutting those to sit under a number would be exactly the trade this budget exists to prevent. What was trimmed instead is prose the rebuild made redundant — a rule stated once in a decision does not need restating beside each of its three call sites. From here there is no raise a spec can grant: the next feature argues with §8 |
| 12 | **2026-08-29** (from the PR #11 review) — **The cap bounds the answer, not the work.** Decision 4 claimed a count "reads at most a thousand rows on any filter, indexed or not"; that is not what the SQL does, and the docs repeated it. `LIMIT` ends a scan once that many rows have *matched*, so a filter with few matches over a column no index covers is read to the end — and the session count, which must form its groups before it can count them, is barely bounded at all | Measured on 500k rows: an unfiltered trace count 1 ms, the same count under `name=` matching nothing 260 ms; an unfiltered session count 23 ms, the same inside a 1 % window 719 ms. The claim is corrected rather than the feature withdrawn, because the same measurement says what the count actually costs *relative to the screen it sits on*: the listing under that same selective filter is 864 ms — it reads the same rows and then sorts them. A count adds a fraction to a screen that is already expensive for that filter, and it is never the thing that made it so. What is not defensible is a document promising a bound the code does not have: somebody sizing a deployment would believe it |
| 13 | **2026-08-30** (from the PR #11 review) — **The panel walks the listing's order, not the page's indices.** `j`/`k` ask for the nearest row strictly older or strictly newer than the one the panel is showing, compared on the same `(timestamp, id)` the server pages by — whether or not that row is still on the page. When the page holds no such row, the walk turns the page, which is what Decision 6 already said | The panel's row leaves the page from either side, and which side is not something the walk can know from how it got there: a live tick prepends newer rows and pushes it off the *oldest* end, while a link somebody sent, a page size changed under it, or a row swept by retention leave it off the newest end or out of the middle. Four review rounds tried to name the right edge for each and produced a rule that was right for whichever case the last round had looked at: `j` walking to the far end of a page it should have entered from the near one, skipping every row between. A comparison has no cases. It also subsumes the on-page walk (the nearest older row of a row that *is* on the page is the next one down) and the row swept out of the middle by retention, so one function answers where three branches disagreed. Its one cost is needing the panel row's own timestamp when the row is off the page: it comes from the detail already on screen, and in the moment before that arrives the walk does not move rather than guess |

## API contract

Both listing endpoints and `GET /api/v1/sessions/{id}` take:

| Parameter | Meaning |
|---|---|
| `limit` | 1–500, default 50. Unchanged. |
| `cursor` | The `next_cursor` or `prev_cursor` of a previous page. |
| `direction` | `next` (default) or `prev`. With no `cursor`, `next` is the newest page and `prev` is the oldest. |
| `count` | `1`/`true` adds the totals below. Off by default. |

and answer with:

```json
{
  "traces": [ … ],
  "next_cursor": "MTc4ODIy…",
  "prev_cursor": "MTc4ODIx…",
  "total": 847,
  "total_capped": false
}
```

`prev_cursor` is `null` on the newest page and `next_cursor` is `null` on the
oldest one, which is what disables « ‹ and › ». `total` is present only when
`count` was asked for; `total_capped: true` means the count stopped at the cap
(1000) and the real number is larger. Rows are always newest first, whichever
direction the page was fetched in.

The oldest page is a **full page anchored at the oldest row**, not the ragged
remainder that walking forward ends on: with five rows and pages of two, ›››
ends on one row and » shows two. Both end on the same row, which is what "the
end" means without a total to count backwards from — and the alternative is
the offset arithmetic Decision 3 refuses.

## Application contract

Under every listing, one bar: **Rows per page** (a select), what is on screen
(`50 of 847 traces`, or just `50 traces` before a count arrives), then
« ‹ › ». The page is `?limit=&cursor=&direction=` beside the filters, so a
reload lands on the same page and a link carries it.

Not `51–100`, which this spec's first draft promised: **a keyset knows where a
page sits only as a cursor, never as an ordinal.** Computing "the fifty-first
row" needs the same offset arithmetic Decision 3 refuses, and a deep link
carrying only a cursor could not answer it at all. So the bar says how many
rows are here and how many match — which is the question behind "where am I",
without the number that would have to be counted to answer it literally.

Changing a filter, the time range or the page size returns to the first page:
a cursor is a position in one ordering, and it means nothing in another.

## Testing

- **Go**: the backward page of a listing is the forward page of the one
  before it, reversed — asserted over a fixture with ties on `timestamp`, the
  case the `id` tie-break exists for; the oldest page equals the last forward
  page; `total_capped` flips exactly at the cap; `count` is absent unless
  asked for. `EXPLAIN QUERY PLAN` shows a seek, not a scan, in both
  directions (the method of spec 003 #25).
- **Component (vitest)**: the bar's URL round-trip, including that a filter
  change drops the cursor.
- **E2E**: turn a page and come back; reload the second page and land on it;
  `j` on the last row turns the page with the panel open; live is paused off
  the first page.

## Out of scope

- Page numbers, "of N", and any offset-based paging (Decision 3).
- An exact count above the cap.
- Sorting by anything but time; a sort control changes what a cursor means
  and deserves its own spec.
- Virtualised scrolling: a bounded page is what this spec buys instead.
