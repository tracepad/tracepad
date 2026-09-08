# Spec 026 — Revision: one listing shell, a header that wraps, anchors that hold

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> Spec 010 pulled the loader out from under three listings so that a paging
> bug would be one bug. Since then the interface has grown to fifteen
> listings, and what sits *around* the loader — the failure line, the table,
> the bar, the page that emptied under a cursor, the count in the header —
> is written out eleven times, line for line, with the same comment pointing
> at the same review. The header itself is a fixed-height row that folds
> into three lines on a phone, and the docs carry forty-six cross-file
> anchors that nothing checks. This spec is the revision the line budget
> exists to trigger (design §8.2): no feature, one component, one rule, one
> script, and the number reported rather than promised.

---

## Overview

Deliverables, one PR (the last commit flips the status):

- One **listing shell** — `ui/src/lib/components/ListingShell.svelte` —
  that owns what every listing writes out today: the failure line, the
  table, the pagination bar, the emptied-page message and the empty state;
  and one **listing count** for the header's meta slot. The eleven
  consumers rewritten over them, every behaviour kept (Decisions 1–3).
- The **trace peek meta** — timestamp, release, latency, cost, id — as one
  snippet where it is rendered identically (Decision 4).
- `PageHeader` **wraps** on a narrow screen instead of folding into three
  lines (Decision 5).
- `scripts/doc-anchors.sh`, wired into the gate, and every anchor it finds
  broken fixed (Decision 6).
- `users_hourly` asks the freeze of its **own** rows, as spec 025 #21 made
  the other two tables do (Decision 7).
- The application-line count **measured per file before and after** and
  reported in the PR; the ceiling does not move (Decision 8).

Not here: any change of behaviour a user can see except the header on a
phone; *Add to queue*, which is already one component used from three
places; new listings; the ceiling.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-08** — **`ListingShell`** takes the `Listing` (spec 010) and a `noun`, and renders in order: the failure line (`listing.problem`, `role="alert"`), the **table** as a snippet, `PaginationBar` over `listing.bar`, the emptied-page line (*Nothing on this page any more. Use « to go back to the newest.*) when the page is empty and not the newest, and otherwise the **empty state** as a snippet — shown only when not loading and not failed; the flex spacer when nothing else applies. The visibility rules are spec 009 #11's and PR #11's, moved, not changed | Eleven copies of one `{#if}` ladder — `SessionDetail`, `RunsTab`, `ItemsTab`, the traces, sessions, users, prompts, datasets and runs listings, a queue's items and a run's items — each carrying the same comment about the cursor whose rows retention took. A listing bug in the ladder is eleven bugs; spec 010's own count was five defects found in pairs. Snippets rather than props for the two variable parts because a table and an empty state are markup, and the shell must not know a `TraceTable` from a `SessionTable`. |
| 2 | **2026-09-08** — **`ListingCount`** renders the header's meta for a listing: the spinner while loading, else the capped total with its `+`, else the rows on screen (PR #11: what is on screen is still a number). Used from the four listings that have a `PageHeader` count | Four identical snippets, one of which the next listing will copy. |
| 3 | **2026-09-08** — The rewrite is **behaviour-preserving by construction**: every consumer keeps its markup for the table and the empty state, its own `navigate`, its own peek panel; only the ladder and the count move. A divergence between two consumers — a spinner drawn where another draws none, a spacer omitted — is listed in the PR's divergence table with one answer each, as spec 010 did; the e2e suites of every listing stay green **unchanged**, which is the test that the shell is the same thing | A revision that changes what a screen does is a feature in disguise; keeping the suites untouched is what proves it is not. Where the eleven genuinely differ today, the difference is either a defect (fixed, with the row saying so) or a reason (kept, as a prop). |
| 4 | **2026-09-08** — The **trace peek meta** — `timestamp`, `release` (with its title), `latency_ms`, `total_cost`, the id with its copy button, each at the breakpoint it has today — becomes one `TracePeekMeta` snippet, used from every peek panel and drilled panel that renders a trace's meta identically today (the traces and sessions listings, the user's traces tab, a queue's items, a run's items). A site that renders a *subset* today keeps rendering that subset through the snippet's props, not through its own copy | The same five spans with the same five `hidden sm:inline` classes appear in up to eight files; the next responsive fix would be made in one and missed in seven, which is spec 010's opening sentence again. |
| 5 | **2026-09-08** — **`PageHeader` may wrap**: `min-h-12` instead of `h-12`, `flex-wrap`; the title and the actions stay on the first line (the title truncates before the actions move), the meta wraps to a second line below when the width runs out, and the meta is the only thing that ever wraps. Never three lines; the page never scrolls horizontally at 375 px | A fixed twelve-unit row with a `whitespace-nowrap` title and an `ml-auto` action group folds into three lines on `/quality?name=` and `/users/{id}` at phone width, with the actions pushed under the meta. Two lines with the actions where the thumb is (the top right) is the ordinary phone layout; three is a defect spec 025's DoD found and parked. |
| 6 | **2026-09-08** — **`scripts/doc-anchors.sh`** checks every `[…](file.md#anchor)` and `[…](#anchor)` in `docs/*.md`, `README.md` and `AGENTS.md` against the headings of the target file, using GitHub's slug rule (lower-case; punctuation other than hyphens and spaces removed; spaces to hyphens; a repeated slug gets `-1`, `-2`, …), **skipping fenced code blocks** (a `# comment` inside a shell block is not a heading). It runs in `make precommit` and in the CI gate, fails on the first broken anchor naming file, line and target, and every anchor it finds broken in the tree today is fixed in this PR | Forty-six cross-file anchors and nothing that reads them; spec 023's worker found one that had been broken for weeks by hand. `datasets.md` alone has nine `# …` lines inside code fences that a naive heading grep would take for anchors, which is why the rule is stated rather than left to the script. |
| 7 | **2026-09-08** — **`users_hourly` is frozen by its own rows**: `rollTraffic` asks `hourFrozenIn` of `stats_hourly` and of `users_hourly` separately and writes each table on its own answer; `recomputeUsers` runs when the per-user table was written. Spec 023 #15's known limit — *an hour past the window is frozen and gets no per-user rows, ever* — is closed the way spec 025 #21 closed it for scores | Spec 025 #21 left one table on the shared gate and said so; the same import-into-a-retained-install case leaves `/users` blind to the imported history for ever. The rule is already written once; asking it of a third table is one call. |
| 8 | **2026-09-08** — The PR reports **`make ui-lines` before and after, per file**, and the ceiling stays at 18,000 (spec 024 #17). The expectation, stated so that it can be wrong in public: eleven ladders of twenty to thirty lines against a shell of about seventy, four counts against one, up to eight meta blocks against one — a net saving in the low hundreds. Spec 010 promised forty lines and was line-neutral; if this one is neutral too, the number is reported and the win is the one place a listing bug now lives, not the budget | A revision that quietly turns into a squeeze to hit a number would be the thing the ceiling exists to prevent. The number is measured; the reason to do the work does not depend on it. |
| 9 | **2026-09-08** (from the implementation) — **The wrap lives one level in.** `PageHeader` is `min-h-12` as #5 says, but the `flex-wrap` is on a pair holding the title and the meta, with the actions outside it — not on the header itself | A wrapping flex row moves its *last* items to the second line, and the last item of `[title, meta, actions]` is the actions: `flex-wrap` on the header produces exactly the layout #5 forbids, the controls pushed under the meta. Ordering the DOM `[title, actions, meta]` fixes the narrow case and breaks the wide one, where the meta belongs beside the title and not past the buttons. Wrapping the pair is what states the rule structurally instead: the meta is the only thing that can move, the actions are outside the container that wraps, and two lines is the most the pair can produce. The h1 gains `truncate` and `min-w-0` — #5's "the title truncates before the actions move", which the `whitespace-nowrap` title could not do |
| 10 | **2026-09-08** (from the implementation) — **The defect #5 names is not the one at 375 px.** The fixed row does not fold into three lines; it cannot, having no `flex-wrap` and no wrappable child. What it does is squeeze the meta: on `/quality?name=hallucination` at 375 px the score's name renders as `h…` beside a full-width *Refresh*, and on `/users/{id}` the id truncates to nothing. The fix is #5's and the contract is #5's; the sentence describing the symptom was wrong | Checked in Chrome and in both Playwright projects on the pre-change build: 48 px, one line, `scrollWidth === clientWidth`, and an ellipsis where the identifier should be. The parked observation was of a header that had run out of room, and "three lines" was the wrong description of what it does about that. It matters for the test: an e2e case asserting "at most two lines and no sideways scroll" **passes on the unfixed header**, so the case asserts the meta is on a line of its own and the actions are above it, which fails on it |
| 11 | **2026-09-08** (from the implementation) — **`ListingCount` serves six listings and `TracePeekMeta` six sites**, not the four and five Decisions 2 and 4 enumerate. The two extra counts are Prompts and Datasets, whose two-branch copies print exactly what the three-branch component prints because `count: false` leaves `total` at null for the life of the page; the extra meta is the session page's own peek panel, which renders the same four spans and the copy button as the sessions listing's drilled panel | The enumerations were of the sites that were *identical*, and both rules — "the header's meta for a listing", "every peek panel that renders a trace's meta identically" — reach further than their own parentheses. A degenerate copy of the count is the same defect one step earlier: it is what the next listing copies. What stays out is `/traces/{id}`, whose header interleaves a `user_id` link between the release and the latency: a different rendering, which #4 leaves to the site |
| 12 | **2026-09-08** (from the implementation) — **The measurement, against #8's expectation: line-neutral.** The application half goes from 17,440 to **17,422** — eighteen lines — and the tests from 8,679 to 8,940. Eleven ladders gave up 191 lines and the three components cost 162; the header cost 11. The per-file table is in the PR | #8 expected "a net saving in the low hundreds" and said the number would be reported rather than promised. It is the same answer spec 010 #7 got for the same shape of work, to within a line: what a component boundary costs in props, types and prose is about what the copies cost in repetition, and the win is not the budget. It is that the emptied-page condition, the visibility of an empty state and the cap on a count are now one place each — three of the five mutations in this PR's table are single-line edits that eleven files used to have to agree about |
| 13 | **2026-09-08** (from the implementation) — **The anchor checker reports every broken anchor, not the first**, and it skips **inline code spans** as well as fenced blocks. The fixture keeps #6's five cases and gains a sixth for the code span | "Fails on the first broken anchor" is satisfied either way — one is enough to fail the run — and stopping there turns a documentation sweep into as many runs as there are breakages, in a checker whose whole job is to be run after a rename. The fixture is the other reason: it asserts two findings in one pass, which a checker that stopped could not produce. Code spans are the fence rule one scale down, and this PR found it the way it would be found again: `AGENTS.md` documents what is checked by writing the shape of a link in backticks, and the gate demanded the file it names. The fixture's broken same-file anchor is deliberately one whose only heading is a `#` line inside a shell block, so the fence rule is what the run asserts rather than what it assumes — a checker that read fences resolves that link and reports one thing instead of two |

No route changes. `PageHeader` at 375 px on `/quality?name=…` and
`/users/{id}`: two lines at most, actions on the first, no horizontal
scroll. Every listing renders exactly as before: the same states in the
same order with the same texts (Decision 3); every e2e project stays green
without edits.

## Testing

- **Vitest, shell**: `ListingShell` over a stubbed `Listing` renders each
  state — failure line with the message; table + bar with rows; the
  emptied-page line on an empty non-newest page; the empty-state snippet
  when idle and empty; nothing but the spacer while loading the first
  page. `ListingCount`: spinner / capped total with `+` / row count.
- **Vitest, meta**: `TracePeekMeta` renders the five parts with their
  breakpoint classes; a subset prop hides the named parts.
- **e2e**: every existing project green **without changes** (Decision 3);
  one new case at 375 px on the two screens Decision 5 names: header height
  at most two rows, `document.documentElement.scrollWidth ===
  clientWidth`.
- **Doc anchors**: a fixture directory with one broken cross-file anchor,
  one broken same-file anchor, one anchor to a heading with backticks, one
  `# line` inside a code fence, one duplicate heading (`-1`) — the script
  fails on the first two and passes the rest; wired into `precommit` and
  the gate, asserted by the fixture run in CI.
- **Go, users freeze**: the three tests of spec 025 #21, asked of
  `users_hourly`: backfill of a frozen hour with intact traces writes
  per-user rows and the summary; a frozen hour with rows is not rewritten;
  a swept hour writes nothing.
- **Mutations** (table in the PR): the emptied-page condition inverted;
  the empty state shown while loading; the count snippet dropping the `+`;
  the anchor script not skipping fences; `users_hourly` back on the shared
  gate.
- **Lines**: `make ui-lines` before and after, per file, in the PR
  (Decision 8).
- **Chrome (DoD)**: the eleven listings on the demo corpus (rows, an
  emptied page reached by a stale cursor, the empty state under a filter
  that matches nothing), both themes; the two 375 px screens; console
  clean; screenshots in the PR.

## Edge cases

- **A consumer with no empty state of its own** (a tab inside a page): the
  snippet is optional; the shell renders the spacer.
- **A consumer whose table needs the listing's rows and more** (the users
  tabs pass a filter through): the table snippet closes over whatever the
  page has; the shell passes nothing but renders it.
- **A heading with inline code or an emoji** in the docs: GitHub's slug of
  it is what the script computes; the fixture covers backticks.
- **The same heading twice in one file**: `-1` on the second; the fixture
  covers it.
- **`PageHeader` with no meta**: one line at any width, unchanged.

## Config additions

None.

## Out of scope

A shared filter bar for the listings (three different bars today);
the twin *pages* `/traces` and `/sessions` merging further (their peek
panels differ by a level, spec 008 #9); a design pass on the header;
checking external links.
