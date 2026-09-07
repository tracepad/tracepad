# Spec 021 — Prompts in the web interface: versions, labels, diff, and the editor

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> Spec 003 gave the store versioned prompts and labels; the CLI pushes
> them, the SDK fetches them, and a generation's panel names the version it
> used. A person still has no screen: seventeen versions on a project are a
> `curl` and a JSON dump, and "what is in production right now" is a
> question for the terminal. This spec adds the *Prompts* section — a
> listing, a prompt page with its versions and a diff between any two, a
> label control that promotes and rolls back, an editor that appends a
> version — and the one write the API lacked, deleting a name.

---

## Overview

Deliverables, one PR (the last commit flips the status):

- The *Prompts* sidebar item (Decision 1). `/prompts` listing,
  `/prompts/{name}` with the versions list, the version view, the diff
  view and the label control; `/prompts/new` and
  `/prompts/{name}/versions/new` — the editor (Decisions 2–6).
- `DELETE /api/v1/prompts/{name}` with a dry run (Decision 7), `tracepad
  prompts rm` and `tracepad prompts label` (Decision 8).
- The API client grows one method per prompt endpoint and `DELETE` where
  it lacks one. `docs/ui.md` gains a *Prompts* section; `docs/prompts.md`,
  `docs/cli.md`, `docs/api.md`, `openapi.json` and `schema.d.ts` follow the
  server change.

Not here: templating or variable preview, a prompt playground, editing a
version in place (the store is append-only, spec 003), prompt-level stats.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-07** — *Prompts* is a **top-level sidebar item** between *Stats* and the *Evals* section, route `/prompts`, icon `scroll-text` | A prompt is a production artefact — what the application ships — not an eval noun; putting it under *Evals* would say it belongs to the test loop. One item, because the section holds one screen family. |
| 2 | **2026-09-07** — Two screens, not three. `/prompts` is a listing (name, type, latest version, labels as chips, updated; row → the prompt page). `/prompts/{name}` is the **prompt page**: a header (name, type, labels), the **versions list** newest first (version, commit message, labels, created) as the shared listing (spec 010), and a **version view** for the version the URL names (`?version=V`, default the latest): the body — messages as role-labelled blocks for `chat`, one plain block for `text` — and `config` as `JsonView`. Two links on the view: *Traces with this version* (`/traces?prompt=name@V`) and *any version* (`/traces?prompt=name`) | Versions and the body are one thing seen at two depths, and a reader picks a version to read it; a separate route per version would be a page with one paragraph. The trace filter spec 012 added already answers "where did this version run", so the page links rather than counts (a count per row is the fan-out spec 016 #16 refused). |
| 3 | **2026-09-07** — The **diff** is a mode of the prompt page, `?diff=A..B`: two version selects in the header (defaults: the version on screen and the one before it) and the server's unified diff from `GET /prompts/{name}/diff` painted per line — additions, removals and hunk headers each their own colour — in a scrolling `<pre>`. The interface computes no diff | Spec 003 put the diff on the server so that every client shows the same one; a client-side diff library would be a second answer and a dependency spec 006 #4 did not budget. Painting lines by their first character is rendering, not logic. |
| 4 | **2026-09-07** — The **editor** is a full page, `/prompts/new` (name, type) and `/prompts/{name}/versions/new?from=V` (prefilled from version `V`, default the latest). Bodies are edited in `<textarea>`s — one for `text`, one per message for `chat`, with a role input beside each (a datalist of `system`, `user`, `assistant`, free text allowed) and *add*, *remove*, *up*, *down*; `config` is an optional `JsonEditor` (spec 015, with #22's `optional`); *commit message* is a text input; *labels* to point at the new version are chips picked from the name's existing labels plus a free entry. *Save* posts `POST /prompts/{name}/versions` once and lands on `/prompts/{name}?version=V` with the version the server assigned | An edit *is* a new version (spec 003: append-only), so the only editor is "new version from this one", and a page — not a panel — because a chat prompt is several long texts. A textarea rather than a CodeMirror instance per message: a prompt body has no syntax to lint, and the editor's cost is paid once for `config`, where JSON is what is typed. The role is free text because the API stores any non-empty role; the datalist is the three a person means. |
| 5 | **2026-09-07** — The editor's **Save gate**, client-side, with the server as the oracle: the name matches `^[A-Za-z0-9][A-Za-z0-9._-]*$` and is ≤ 200 chars (new name only); a `text` body is non-empty; a `chat` body has at least one message and every message has a non-empty role and content; `config` is empty or parses; no label is `latest`. Every rule is shown at the field, not as a dialog | Spec 003 refuses each of these with a `400` naming the rule, and a form that lets a person reach the `400` has not done its job; the check is mirrored, not moved — the server still decides. `latest` is the one label name the server reserves. |
| 6 | **2026-09-07** — The **label control** lives on the version view: the version's labels as chips with a remove ×, and *Add label…* (existing labels of the name, or a new one). Attaching a label that points at another version, and removing a label, go through `ConfirmDialog` naming the move (*production: v6 → v7*, *remove production from v7*); attaching a label that points nowhere is immediate. Moves are `PUT /prompts/{name}/labels/{label}`, removals `DELETE` | Moving `production` is a deploy (spec 003: a label move is the release), and a deploy from a mis-click needs one more click; a new label is a note, and a dialog for a note is ceremony. The dialog says *from* and *to* because that is what a rollback reads before confirming. |
| 7 | **2026-09-07** — The API gains **`DELETE /api/v1/prompts/{name}`** following spec 005 #8: without `?confirm=` it answers `200` with the dry run `{"name", "would_delete": {"versions": N, "labels": M}}`; with `?confirm=<name>` it deletes the name, its versions and its labels in one transaction and answers `200` with the same shape plus `"deleted": true`; a wrong echo is `400`, an unknown name `404`. Traces that reference the name keep their `prompt` columns; the note in the dry run says so. The interface offers *Delete* on the prompt page through `ConfirmCard` | Full management (owner decision 2026-09-07) includes removing a prompt that was a mistake or a rename-by-recreation; the store had no write for it. The echo ceremony is spec 005's rule for an act with a blast radius — a name's whole history — and `ConfirmCard` already renders that contract (spec 016 #24). The trace side is a plain string pair on the observation (spec 012); severing it would be a second deletion nobody asked for, and the filter keeps answering. |
| 8 | **2026-09-07** — The CLI grows **`tracepad prompts rm <name> [--confirm]`** (dry run without the flag, spec 005's shape) and **`tracepad prompts label <name> <label> (--version N \| --rm)`**. MCP is unchanged | The interface may do nothing the CLI cannot (spec 004 #1). `push --label` covered creation-time labels only; a label move or removal had no command, and the new endpoint needs its client. MCP tools are stateless read wrappers over the read API by design (spec 004 #14, #16). |
| 9 | **2026-09-07** — Empty states teach the other clients: `/prompts` with no prompts shows `tracepad prompts push <name> --file prompt.json --label production` and the SDK line `tracepad.prompt("name", label="production")`; a prompt with one version shows *no diff yet* in place of the diff selects | Spec 016 #15's rule: the person on an empty screen is the one about to write the client. |
| 10 | **2026-09-07** — The application-line budget stays **14,000** (spec 016 #14) and this spec and spec 022 must land under it together; the PR reports the number per screen | Two UI specs are in flight against one ceiling; each landing "under" alone could still land over together. |
| 11 | **2026-09-07** — Both prompt listings — `GET /prompts` and `GET /prompts/{name}/versions` — gain **`direction`** and **`prev_cursor`**, the paging spec 009 gave the read API. An amendment to this spec's Server contract, which named only the deletion. The CLI keeps its one-way walk (no `--oldest`/`--newer`), as `datasets ls` does | The Application contract asks for the shared listing "cursor both ways", and spec 010's loader *is* the bar with « ‹ › on it. The two prompt listings were the last one-way ones in the read API — written before spec 009 — so the alternative was a screen whose bar has two dead buttons on it, or a second listing component for one screen. It is the change spec 016 already made to `GET /datasets` for the same reason, in the same shape: `trimPage` owns which cursor a page may claim, and the store reverses a backward page. |
| 12 | **2026-09-07** — `GET /prompts/{name}/versions` answers with **`labels`**: every label of the *name* and the version it points at, beside the per-version labels it already carried. A second amendment to the Server contract | The prompt page's header and the editor's label chips both need "the labels this name has", and the version rows only carry the ones that fall on the page — so on page two of a long history *production* would vanish from the header. The store already reads the whole map on this path (`promptLabelsByVersion`), so it is a field, not a query; the alternative was a client scanning `GET /prompts` for one row, which is logic in a client (spec 004 #1). |
| 13 | **2026-09-07** — The API client fetches with **`cache: 'no-store'`**, for every request | The prompt reads are the only ones in this API that carry `Cache-Control` — `max-age=60`, for the SDKs that poll by label (spec 003 #14) — and the browser honours it: a screen that had just moved `production` re-read the minute-old answer and drew the move as not having happened (found running this spec's e2e). A `PUT` to `…/labels/{label}` does not invalidate the cache entry for `…/prompts/{name}`, so nothing about the write could fix it. Every other endpoint sends no caching headers, so the flag changes nothing for them and states what the data plane is: live. |
| 14 | **2026-09-07** — `POST /prompts/{name}/versions` takes an optional **`expect_version`**: the version the author believed the name was at, `0` for a name they believe is new. A mismatch is a `409` whose body carries `version`, the version the name is actually at, and writes nothing; absent, the append is unconditional as before. `/prompts/new` sends `0`; `/prompts/{name}/versions/new` sends the **latest it loaded**, not the `?from=` it is prefilled with. `tracepad prompts push --expect N` sends it too | There is no create-only endpoint — a first version and a seventh are the same `POST` — so *New prompt* aimed at a name somebody already published silently extended it, and with a label in the form moved their `production` while doing it (found in review of PR #40). The fix belongs on the server, not in a `GET` before the `POST`: that is a race, and the number is assigned inside a transaction that can check the precondition for free. It closes the lost-update case in the same stroke — two people appending to v7 no longer produce a v8 that quietly buries the other's. `Rejection` grows `Details` so a refusal a client must act on can say where to go. |
| 15 | **2026-09-07** — The **label control writes nothing until the name's label map has actually loaded**: *Add label…* and the chips' × are disabled, with the reason in their title, until a version listing has answered. Loaded means "a response arrived", never "the map is non-empty" | An empty map is what a name with no labels looks like *and* what a failed or unfinished read looks like, and the control read the first: with the listing failed — its banner above, the version view below still drawn from a prompt fetch that succeeded — typing `production` fired an immediate `PUT` and moved it off whatever version held it, skipping exactly the dialog Decision 6 exists for (found in review of PR #40). A control that cannot see the labels has no business writing them. |

## Application contract

Routes (all behind the layout guard):

| Route | Screen |
|---|---|
| `/prompts` | Listing (shared listing, spec 010, cursor both ways): name, type, latest version, labels as chips (`production` first, then alphabetical), updated. Header: *New prompt* → `/prompts/new`. Empty state: Decision 9. |
| `/prompts/{name}` | Header: name, type chip, labels with the version each points at, *New version* (→ `/prompts/{name}/versions/new?from=V`, `V` the version on screen), *Diff*, *Delete* (Decision 7). Left: versions list (Decision 2), the selected row highlighted. Right: the version view — `v7 · commit message · created · labels` (Decision 6's control), body, config, the two trace links. `?diff=A..B` replaces the view with Decision 3's. On the narrow layout the list stacks above the view. |
| `/prompts/new` | Editor with name and type (radio `text` / `chat`) at the top; the body form for the chosen type; config; commit message; labels (free entry only — a new name has none). |
| `/prompts/{name}/versions/new?from=V` | Editor prefilled from `GET /prompts/{name}?version=V`; name and type fixed and shown; labels from the name's existing ones. |

The API client gains `listPrompts`, `getPrompt(name, {version | label})`,
`listPromptVersions`, `promptDiff(name, from, to)`, `createPromptVersion`,
`putPromptLabel`, `deletePromptLabel`, `deletePrompt(name, confirm?)`.
Every table is the shared listing; every JSON is `JsonView`; every
destructive action is Decision 6 or 7. Widths: 375 px never scrolls the
page. Both themes; console clean.

`docs/ui.md` gains a *Prompts* section describing the four screens and the
label ceremony; `docs/prompts.md` gains the deletion and a *From the web
interface* paragraph.

## Server contract (Decisions 7, 8)

Both listings page in both directions (Decision 11): `direction=next|prev`
beside `cursor`, `prev_cursor` beside `next_cursor`, `trimPage` owning which
cursor a page may claim, as every other listing in the read API does. The
version listing also answers with the name's whole `labels` map (Decision 12).

`DELETE /api/v1/prompts/{name}[?confirm=<name>]` as Decision 7. One
transaction over `prompts`, `prompt_versions` and `prompt_labels` (or
whatever the schema of spec 003 names); no migration unless a foreign key
needs `ON DELETE CASCADE` it lacks — then a numbered migration, noted as a
Decision. `openapi.json` (+ `schema.d.ts` in the same commit), `docs/api.md`
and `docs/prompts.md` updated; router ↔ openapi parity test extended.

CLI: `prompts rm`, `prompts label` in `internal/cli`, the usage block and
the usage parity test, `docs/cli.md`.

## Testing

- **Vitest**: the sidebar renders *Prompts* and marks it active; the
  editor's Save gate (Decision 5) per rule, including `latest` refused as
  a label and a chat body with an empty content; the label control asks
  for confirmation exactly when Decision 6 says; the diff painter classes
  `+`, `-`, `@@` and context lines; the listing's chips order.
- **Go**: `DELETE` dry run, confirm, wrong echo (`400`), unknown name
  (`404`), labels gone after delete, traces' `prompt` columns intact after
  delete; CLI `rm` (dry run and confirm) and `label` (set, move, `--rm`);
  openapi ↔ router parity; usage parity.
- **e2e** (`prompts.spec.ts`, seeding through the API in a project of its
  own, spec 016 #18): push two versions of a chat prompt with `production`
  on v1 → the listing shows the row with the chip → the page shows v2's
  body and the versions list → `?diff=1..2` paints a removal and an
  addition → *New version* from v2, edit a message, save → lands on v3 →
  move `production` to v3 through the dialog → chips update → *Delete*
  shows the dry run (3 versions, 1 label), refuses a wrong echo, deletes on
  the right one → the listing is empty; `/prompts/new` creates a text
  prompt; 375 px for every screen.
- **Mutations** (table in the PR): the Save gate with the `latest` rule
  removed; the confirm rule inverted; the dry run returning `deleted:
  true`; the delete leaving labels behind.
- **Lines and bytes**: `make ui-lines` with the per-screen breakdown;
  `dist` under spec 015 #10's ceiling.
- **Chrome (DoD)**: every route above on a seeded corpus, both themes,
  375 px, console clean, one screenshot per screen in the PR.

## Edge cases

- **A name with hundreds of versions**: the versions list pages (shared
  listing); the diff selects are number inputs bounded by the latest, not
  dropdowns of every version.
- **A chat prompt with a role outside the datalist** (`tool`, `function`):
  rendered as the string it is; the editor keeps it.
- **A version view for `?version=` that does not exist**: the page's
  not-found state, the versions list still shown.
- **Deleting the prompt while the editor for it is open elsewhere**: the
  editor's Save gets the server's `404`, shown at the Save button.
- **A label that points at a deleted version**: cannot happen — versions
  are never deleted alone (Decision 7 removes the name whole).
- **`?diff=3..3`**: the empty diff is shown as *identical*.

## Config additions

None.

## Out of scope

Editing a version in place; variable interpolation or a preview with
values; a playground that runs the prompt; per-prompt stats and cost
(spec 013's rollup does not group by prompt); prompt-level access rules;
importing prompts from files in the browser (the CLI's `push` is the
import); showing which traces used a version as a count (Decision 2).
