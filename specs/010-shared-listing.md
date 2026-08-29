# Spec 010 — Shared Listing: one loader under three tables

**Status:** 📝 DRAFT
**Sprint:** September 2026

> Three listings page the same way — Traces, Sessions, and a session's own
> traces — and two of them are written out twice, line for line. Seven review
> rounds of spec 009 found at least five defects in *pairs*: the same bug in
> the neighbouring file, fixed in one and missed in the other. This spec pulls
> the shared part into one module with its own tests, so that a listing bug is
> one bug, and the interface comes back under the line budget it crossed.

---

## Overview

Deliverables:

- One **listing loader** — `ui/src/lib/listing.svelte.ts` — that owns what
  `/traces`, `/sessions` and `SessionDetail` currently each own: the page in
  force, the rows, both cursors, the capped count, loading and failure, the
  load and count effects with their keys and aborts, and the bar's bindings.
- One **walk layer** over it for the two listings that have a peek panel: the
  panel's position in the listing's order, the previous/next gestures, and the
  page turn a walk rolls into (spec 009 #6, #13).
- Both pages and the component rewritten over the module, with every
  behaviour they have today kept — except the ones the divergence table below
  names, each of which gets one answer.
- The UI back **under 9,000 lines** with no change to the budget (spec 009
  #11), measured and reported. ⚠ **What that budget counts changed — see #7.**

Not here: any new listing feature. No search bar, no presets, no column
choice, no sort. A refactor that ships a feature is two changes reviewed as
one.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-08-30** — The shared part is a **runes module**, `$lib/listing.svelte.ts`, exporting a factory that a page calls once during setup — not a wrapper component with snippets, and not a plain `.ts` module of pure functions | The duplicated code is reactive: effects keyed on strings, `untrack`ed reads, an `AbortController` per key change, state that several effects write. A component could hold that only by exposing every piece through props and snippets, which is the same wiring again in a new shape; a pure module cannot own an effect at all. A factory in a `.svelte.ts` file can — an `$effect` created during a component's setup belongs to that component and is torn down with it — and it can be driven under `$effect.root` in vitest with a fake reader, which is what makes the tests below possible. The interface already has this shape in `auth.svelte.ts`. |
| 2 | **2026-08-30** — Two layers, not one: the **loader** serves all three listings; the **walk** serves the two with a panel. The page in force comes from a **page source** with two implementations — the URL (spec 009 #1) and component state (spec 009 #10) | The three listings share loading, cursors, aborts and the bar exactly, and share the panel walk only two ways: a session's own trace table has no panel of its own to walk (spec 008 #9). One factory with a "has a panel" flag would carry the walk's state into the listing that has nothing to do with it. Where the page *lives* is the one place the third listing had to differ (spec 009 #10) and stays different; it becomes the loader's one pluggable part rather than a reason to leave the listing out. |
| 3 | **2026-08-30** — **Behaviour is frozen**, and the divergence table is the whole list of exceptions. Anything else the rewrite turns up where the two pages disagree is a question to the coordinator, not a judgement call in the branch | A refactor is reviewable only if the reviewer knows what is supposed to change. The pairs of defects that motivate this spec are also the reason to distrust "obviously the other page is right" — that is how each half was fixed alone. The existing E2E suites (`pagination`, `peek`, `sessions`) pass unchanged and are the freeze's evidence. |
| 4 | **2026-08-30** — The loader hands the bar its props as **one object**, spread into `PaginationBar`; the bar's own contract does not change | Thirteen bindings, written three times, are where "the edges of the bar" defects came in pairs. The bar is the consumer with the most bindings and the least variation: only `total` and `noun` differ, and a spread with two overrides expresses exactly that. Changing `PaginationBar` to take the loader instead would couple a presentational component to a data module and lose its existing tests. |
| 5 | **2026-08-30** — Live mode and Refresh stay features of their pages, over two named loader actions: **`tick()`** re-reads the newest page silently, replacing the rows and refreshing the count, and reports failure in its own slot; **`reload()`** re-runs the load and the count as a page turn would, with the loading state | These are two re-read semantics, not one with a flag: a live tick must not blank the count or raise the loading state (spec 006 #12; PR #11), and a Refresh must. Naming both in the loader is what lets the pages keep their controls without keeping their copies of the machinery. Neither page gains the other's control: no live on Sessions (spec 007 #8), no Refresh on Traces. |
| 6 | **2026-08-30** — The budget is **not raised**; the DoD is that `make ui-lines` prints no warning, and the report states the count before and after. The loader's tests count, and are worth their lines. ⚠ **The premise below did not survive the measurement — see #7** | Spec 009 #11 said the next raise would be an amendment to design §8, and this spec exists so that no such amendment is needed yet. The tests are the trade this makes: the regressions that took seven review rounds to find become assertions over a fake reader, and review rounds cost more lines of somebody's attention than the tests do of the budget. |
| 7 | **2026-08-30** (from the implementation) — **The budget counts the application; the tests are reported beside it under no ceiling.** Decision 6 assumed the extraction would free the forty lines the interface was over. It did not. The three listings gave up 348 lines, the module cost 309 and `page.ts` 9: the refactor is line-neutral to within a dozen lines, and the whole of the overflow is the 223 lines of tests that hold the five invariants. Design §8 is amended (owner, 2026-08-30) to count application code against 9,000 and to print the test count beside it under no budget, and **the number 9,000 does not move**. Splitting the counter turned up a defect in it: `git ls-files 'ui/src'` matches the two `.woff2` web fonts, and `grep` counts 319 "lines" in them — so every figure the budget was ever set against, 8,300 and 9,000 and the 8,623 and 9,040 measured under them, stood on 319 lines of binary nobody reads. Counting text extensions only, `main` is **6,170** of application and 2,551 of tests where the old counter said 9,040, and this spec lands at **6,149** and 2,774 | The measurement says something the single number could not. What the two pages shared was real, but so is what they do not share — filters, live mode, the drill, three different empty states — and the shared third comes back as a module with a contract, paying in types and a page source what two concrete copies never had to. Line-neutral is what extracting a duplicate of that shape actually buys; what it buys besides is one place for a listing defect to be, which is what spec 009's seven review rounds were paying for in attention instead. That leaves the tests as the entire overflow, and a budget that charges for them argues for exactly one fix — write fewer — which is the opposite of what Decision 6 said they were worth. Cutting the module's prose and the tests' to sit under the number is the other way out, and it is the trade spec 009 #11 named and refused. Spec 009 #11 said the next raise had to be an amendment to §8; this is that amendment, and it changes what is counted rather than the count, because the 6–9k envelope was always a claim about how much *interface* somebody has to hold in their head, and nobody holds a test suite in their head. The count that matters did not grow: the application is twenty-one lines smaller than it was. The font correction is named here rather than folded in quietly, because it moves the number in this branch's favour by fifteen times what the branch itself does — it was found while splitting the counter, it is the counter's defect and not the interface's, and one command checks it: `git ls-files 'ui/src' | grep '\.woff2$' | xargs grep -chv '^[[:space:]]*$'`. It buys honesty rather than room: the ceiling is unchanged and is now a number about code |

## Module contract

The shapes below are the contract; names inside the module are the worker's.

**Page source.** Where the page in force lives, and how it moves:

```ts
type Spot = {
  readonly at: PageState;               // limit, cursor, direction ($lib/page)
  turn(to: Partial<PageState>): void;   // keys left out of `to` reset (pageSearch semantics)
};
```

- `urlSpot()` reads `readPage(page.url.searchParams)` and turns with `goto`,
  keeping every other query key, `keepFocus` and `noScroll` — what `turn` does
  on both pages today.
- `stateSpot(scope: () => string)` is what `SessionDetail` does today: the page
  is component state paired with the scope it was taken in (a session id); a
  different scope *derives* the first page at the same size without a write
  and without a second request (spec 009 #10, the `chosen`/`spot` pair).

**Loader.** `createListing<Row>(spec)` where

```ts
type Answer<Row> = { rows: Row[]; next_cursor: string | null; prev_cursor: string | null;
                     total?: number; total_capped?: boolean };
type Spec<Row> = {
  key: () => string;                        // everything that names *which listing*: filters, scope
  spot: Spot;                               // where the page lives
  read: (at: PageState, count: boolean, signal: AbortSignal) => Promise<Answer<Row>>;
  count?: boolean;                          // ask the capped count on key change (default true)
  failed: string;                           // the fallback message for a non-ApiError
};
```

returns reactive state and actions:

| Field | Meaning |
|---|---|
| `rows`, `nextCursor`, `prevCursor` | The page on screen and its cursors. `$state.raw`. |
| `total` | `{ value, capped } \| null` — the count, or `null` before it lands or when it could not be taken. |
| `loading` | True from a load's start to its landing. Not raised by `tick()`. |
| `failure` | The load's failure, or `null`. `liveFailure` is `tick()`'s own slot; `problem` is `failure ?? liveFailure`, what the banner shows. |
| `newest`, `oldest` | `isFirstPage(spot.at)` and `isLastPage(spot.at)`. |
| `bar` | The `PaginationBar` props object (Decision 4): `limit`, `rows`, `total`, `hasPrev`, `hasNext`, `busy`, `atNewest`, `atOldest`, and the five handlers over `spot.turn`. |
| `turn(to)` | `spot.turn`. |
| `reload()` | Decision 5. |
| `tick()` | Decision 5. A no-op off the newest page (spec 009 #7). |

Invariants the loader keeps, each with a test (below):

1. **The load effect depends on `key()` and `spot.at` and on nothing else.**
   `read` is called under `untrack`. Opening the panel — a `?peek=` on the
   same URL — does not re-run the load (PR #10 review, the reason `filterKey`
   exists on both pages).
2. **One request per page of one key.** A key or page change aborts the
   request in flight, and an aborted request never lands: not its rows, not
   its cursors, not `loading`, not `failure`.
3. **A failed page cannot be landed on.** Failure empties the rows and both
   cursors and clears any walk intent (spec 009 #6); an *abort* clears none of
   them.
4. **The count is a question about the key.** It runs on key change and on
   `reload()`, never on a page turn (spec 009 #4). A count that fails leaves
   `total` at `null` and is not a `failure`.
5. **`tick()` replaces, never merges** (spec 009 #9), refreshes `total` when
   the answer carries one (PR #11 review), and lands nothing if the page or
   key moved underneath it.

**Walk.** `createWalk(listing, spec)` where

```ts
type WalkSpec = {
  key: (row: Row) => string;                // the listing's sort key: `timestamp`, or `last_seen`
  peekID: () => string | null;              // from the URL ($lib/peek)
  showing: () => Ordered | null;            // the panel's own row when it is off the page — caller-supplied (divergence 1)
  open: (id: string) => void;               // the page's `peek`
};
```

returns `position`, `hasPrev`/`hasNext` (dead while `loading`, spec 009 #13,
PR #11 sixth review), and `step(±1)`, and settles a rolled turn on the page
it asked for and no other (`settled` in `$lib/peek`, spec 009 #6, #14). The
pure part — `anchor`, `neighbour`, `walkable`, `settled` — stays in `peek.ts`
untouched, tests included; the walk is only the reactive wiring around it.

## Divergences

Where the pages disagree today, and what the rewrite does. Nothing off this
list changes (Decision 3).

| # | Today | Resolution |
|---|---|---|
| 1 | `/traces` places an off-page panel row by `peeked.timestamp` unconditionally; `/sessions` only when unfiltered, because a filtered sessions listing sorts by a span the session's own `last_seen` is not (spec 009 #14) | **Both stay right.** `showing` is caller-supplied; the walk has no opinion. The reason is written once, at the `showing` parameter, instead of in the sessions page alone. |
| 2 | `/traces` re-reads via `poll()` (silent, own failure slot, replaces rows, refreshes the count); `/sessions` via `generation++` (loading state, rows kept until the answer, count re-asked) | Decision 5: `tick()` and `reload()`. Same observable behaviour on each page. |
| 3 | The banner shows `failure ?? liveFailure` on `/traces` and `failure` on `/sessions` | `problem` on both. Identical output — `liveFailure` is never set where `tick()` is never called. |
| 4 | A filter change carries the page size through `filterSearch(next, extra)` on `/traces` and through hand-built `?`/`&` on `/sessions` | One way of carrying a non-default `limit` into a fresh listing URL, shared. The URLs produced do not change (default size absent, spec 009 #1). |
| 5 | The "Nothing on this page any more" notice: `flex flex-1 items-start justify-center p-8 text-center` on the pages, `p-8 text-center` inside `SessionDetail` | One rendering; the pages' classes. Verified in both places in Chrome: the panel body is a flex column and the notice must not push the bar. |
| 6 | `SessionDetail`'s bar takes `total` from `session.trace_count`, exact and uncapped; the pages take it from the count | `count: false` on that loader; `{ ...listing.bar, total: … }` at the call site (Decision 4). |
| 7 | `SessionDetail` shows its spinner only for a session not yet on screen (spec 009 #15); the pages never replace the table with a spinner | Unchanged and page-level: `loading` is the loader's, what to draw for it is the caller's. |

## Testing

- **Loader (vitest, `$effect.root`, fake `read`)**: one test per invariant
  above, plus: a `stateSpot` with a new scope yields the first page without a
  write; `reload()` runs load and count once each; `tick()` off the newest
  page does nothing; the settle of a rolled walk fires on the landed page
  whose cursor *and* direction match, and is cleared by a failure.
- **Pure part**: `peek.test.ts` and `page.test.ts` unchanged.
- **E2E**: the existing `pagination`, `peek` and `sessions` suites pass
  unchanged — no new suites, no edited expectations. An expectation that has
  to change is a behaviour change, and a question (Decision 3).
- **Chrome (DoD)**: on both pages and inside a session panel — turn pages
  both ways and to both ends, `j`/`k` across a page edge with the panel open,
  live on and off the first page, Refresh, a failure banner from stopping the
  server mid-session and its recovery, 375 px; console clean; no request
  fired by opening the panel (invariant 1, watched in the network panel).
- **Budget**: `make ui-lines` before and after, in both layouts — the single
  number it printed until now, and the application/tests split it prints from
  here (#7). All four in the PR.

## Out of scope

- Any listing feature: search, presets, saved views, column choice, sorting.
- A generic table component; `TraceTable` and `SessionTable` stay as they are.
- `PaginationBar` and `PeekPanel` API changes.
- The Stats page, which is not a listing.
- Raising the line budget (Decision 6; spec 009 #11).
