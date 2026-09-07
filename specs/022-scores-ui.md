# Spec 022 — Scores on the trace, the observation and the session, and scoring by hand

**Status:** 🚧 DRAFT
**Sprint:** September 2026

> Scores exist since spec 003 and the eval screens of spec 016 render them
> — on a run, on a comparison. On the screens where the graded thing
> itself is read they are invisible: a trace with three judge verdicts
> and a thumbs-down from the product's widget shows none of them, a
> session with a CSAT rating shows a list of traces. And a person reading
> a trace who wants to say *this one is wrong* has no way to. This spec
> renders the scores where their target is, and adds the one write a
> reviewer needs — a score by hand, edited or retracted — plus the endpoint
> retraction was missing.

---

## Overview

Deliverables, one PR (the last commit flips the status):

- A *Scores* block on the trace header, the observation panel and the
  session header (Decisions 1–3).
- *Score* on each of the three: a dialog that posts one score, edits one
  by re-posting its id, and deletes one (Decisions 4–6).
- `DELETE /api/v1/scores/{id}`, `tracepad scores add`, `tracepad scores
  rm` (Decisions 6, 7).
- The API client grows the score reads and writes. `docs/ui.md` gains a
  *Scores* section; `docs/scores.md`, `docs/cli.md`, `docs/api.md`,
  `openapi.json` and `schema.d.ts` follow the server change.

Not here: an annotation queue, a scores column in the listings, score
trends in *Stats*, scoring from the MCP.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-07** — The trace page (and `TraceDetail` wherever it renders — the peek panel, the session drill-down, the run-item drill-down) reads **`GET /api/v1/scores?trace_id=<id>&limit=500`** once, beside the trace, and shows a **Scores block** in the header: one chip per score — name, the value rendered by type (Decision 3), the source — with the comment revealed on expand. Scores that carry an `observation_id` are shown on the **observation panel** of that observation and not in the trace header; the header says *N more on observations* | One request answers both surfaces, and splitting one loaded document by a field is rendering, not the client logic spec 004 #1 forbids. The header is where a reader decides whether to open a trace at all; a verdict belongs there. Observation scores at the observation because that is what they grade. 500 is the endpoint's ceiling and more scores than that on one trace is Edge cases' problem, not the common case. |
| 2 | **2026-09-07** — The session page reads `GET /scores?session_id=<id>&limit=500` and shows the same block in the session header. Trace rows inside the session show nothing new | A session score (CSAT, a conversation-level verdict) grades the session; each trace's own scores are one click away, where Decision 1 puts them. |
| 3 | **2026-09-07** — **Rendering by type**: `numeric` to three significant digits (the config's `min`/`max` shown as a title when there is one); `boolean` as *yes* / *no*; `categorical` as the string; `text` as the first 120 characters with expand. The **source** is `metadata.source` when it is a string, else *api*; a score's time is relative with the absolute as its title. Order: newest first, as the endpoint returns them | These are the four types spec 003 pins, and the rendering is the same table the run page uses (spec 016), so a name means the same on both screens. `metadata.source` is what the SDK harness and this spec's own dialog write (Decision 4); a score with none came from somewhere with a key, which is what *api* says. |
| 4 | **2026-09-07** — **Scoring by hand** is a dialog (`ScoreDialog`, bits-ui `Dialog`) opened by *Score* on the trace header, *Score this observation* on the observation panel, and *Score* on the session header. Fields: *name* — a select over the project's score configs, plus *other…* revealing a free name and a `data_type` radio; *value* — the control the type dictates: a number input bounded by the config's `min`/`max`, two buttons for `boolean`, a select over `categories`, a textarea for `text`; *comment*. Save posts **one** score with `metadata: {"source": "web"}` and no `timestamp`, then re-reads the block | A config is what a name means (spec 003), so the dialog offers the names the project declared and builds the control from the config — one place where a range or a category list is typed, and the server still checks it. The free-name path exists because a project without configs must still be able to score. `source: "web"` is how the chip says *a person, here*; receive time is the right timestamp for a judgement made now. |
| 5 | **2026-09-07** — **Edit** is the same dialog prefilled from the score and a re-post with its `id`, every field resent; offered on every score, not only web ones. **Delete** is `ConfirmDialog` naming the score and its value, then Decision 6's endpoint | Spec 003: a correction is another POST with the id, and nothing about a score says who may correct it — a judge's verdict overruled by a person is the review the eval loop exists for. The dialog names the value because *delete accuracy 0.9* and *delete accuracy 0.1* are different mistakes to make. |
| 6 | **2026-09-07** — The API gains **`DELETE /api/v1/scores/{id}`**: `200 {"id": "…"}` when the row went, `404` when there was none. No dry run, no echo | A score is one row that a re-post can recreate; spec 005 #8's ceremony is for what cannot be undone. A retraction that the store refused would leave a wrong verdict on a trace with no way to remove it but retention. |
| 7 | **2026-09-07** — The CLI grows **`tracepad scores add`** (`--trace ID [--observation ID] \| --session S`, `--name N`, `--value V \| --string S`, `[--type T] [--comment C] [--id ID]`) and **`tracepad scores rm <id>`**. MCP is unchanged | The interface may do nothing the CLI cannot (spec 004 #1): `scores ls` was the whole family, and both writes this spec makes from a screen need a command. MCP tools are stateless read wrappers over the read API by design (spec 004 #14, #16). |
| 8 | **2026-09-07** — The tree (`TraceTree`) marks an observation that has scores with a **count badge** beside its name, from the same response | "Which step was graded" is the question a reader of an agent trace asks before opening panels one by one; a number from a loaded document is rendering. |
| 9 | **2026-09-07** — The application-line budget stays **14,000** (spec 016 #14); spec 021 and this spec land under it together (spec 021 #10) | Same ceiling, two specs. |
| 10 | **2026-09-07** — Decision 4's `metadata: {"source": "web"}` and its absent `timestamp` are what a **new** score is stamped with. An **edit** (Decision 5) resends the score's own `metadata` and `timestamp` unchanged along with its id; only the fields the dialog shows are the reader's to change | "Every field resent" is about a re-POST replacing the row whole, not about overwriting what the row already said. A judge's verdict corrected by a person is still the judge's verdict, and relabelling its source as *web* would make the chip lie about where it came from; moving its event time to now would slide it to the top of a listing ordered by when the graded interaction happened. Both are facts an edit would have invented. |

## Application contract

- **Trace header** (`TraceDetail`): after the existing summary line, a
  *Scores* block: chips (Decision 3), expandable comment, *Edit* and
  *Delete* per chip (Decision 5), *Score* button (Decision 4). Empty:
  *No scores* and the button.
- **Observation panel** (`ObservationDetail`): a *Scores* section between
  the payloads and the metadata with the observation's scores and *Score
  this observation*. `TraceTree` rows get Decision 8's badge.
- **Session header** (`SessionDetail`): the same block over
  `?session_id=`.
- **`ScoreDialog`**: Decision 4; the same component in create and edit
  mode; the server's `400` shown at the field it names when it can be
  matched, else at the Save button.

The API client gains `listScores(filters)`, `createScore(body)`,
`deleteScore(id)` and `DELETE` in its `Request` union if absent. Both
themes; console clean; 375 px never scrolls the page. `docs/ui.md` gains
a *Scores* section; `docs/scores.md` gains *Deleting a score* and a
*From the web interface* paragraph.

## Server contract (Decisions 6, 7)

`DELETE /api/v1/scores/{id}` — `id` as 32 lower-case hex; `200 {"id"}` or
`404`; the row goes through the writer queue like every write (spec 001).
`openapi.json` (+ `schema.d.ts` in the same commit), `docs/api.md`,
`docs/scores.md`; the router ↔ openapi parity test extended.

CLI: `scores add` posts one object and prints the id; `scores rm` prints
the id or the `404`. Usage block, usage parity test, `docs/cli.md`.

## Testing

- **Vitest**: the chip renderer per type and per source; the split of one
  response into header and observation scores with the *N more* count;
  the dialog builds the control the config dictates and requires a type on
  the free-name path; edit mode resends the id; the tree badge counts.
- **Go**: `DELETE` on an existing and a missing id, and on another
  project's id (`404`, never cross-project); CLI `add` (numeric, boolean,
  categorical, text, on a session) round-trips through `ls`; `rm`; openapi
  ↔ router parity; usage parity.
- **e2e** (`scores.spec.ts`, seeding through the API in a project of its
  own, spec 016 #18, over fixture 001's trace): post a trace score, an
  observation score and a session score → the trace header shows one chip
  and *1 more on observations*, the observation panel shows its score, the
  tree shows the badge, the session header shows its chip → *Score* with
  a configured categorical name saves and the chip appears → *Edit*
  changes the value in place (same id in the listing) → *Delete* removes
  it → the free-name path with a numeric value; 375 px.
- **Mutations** (table in the PR): the split dropping observation scores
  from both surfaces; the dialog sending `data_type` on the config path;
  edit posting without the id; `DELETE` answering `200` for a missing id.
- **Lines and bytes**: `make ui-lines` with the per-screen breakdown.
- **Chrome (DoD)**: the trace page, the peek panel, a session, the run-item
  drill-down, both themes, 375 px, console clean, screenshots in the PR.

## Edge cases

- **More than 500 scores on a target**: the block shows the page and *and
  more* when `next_cursor` is not null; no second page is fetched.
- **A score whose `observation_id` is not in the trace** (a late or wrong
  id): counted in *N more on observations*, shown on no panel; the header
  chip list gets it with an *unknown observation* note.
- **A config replaced between opening the dialog and saving**: the
  server's `400` is shown at the value field.
- **Editing a score whose name has a config it no longer satisfies**: the
  server refuses (spec 003: configs check what comes next); the message is
  shown, the old row stays.
- **A `text` score of many kilobytes**: the chip shows 120 characters;
  expand shows the whole string in a scrolling block.

## Config additions

None.

## Out of scope

An annotation queue and its routing; a scores column or filter in the
traces and sessions listings; score trends and per-name charts in *Stats*
(spec 013's rollup does not know scores); scoring from the MCP; a
reviewer identity on a score (the store has no users); bulk scoring.
