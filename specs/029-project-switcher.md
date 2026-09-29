# Spec 029 — The project in the URL, and a switcher in the sidebar

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> Spec 028 gave the server accounts, and an account reaches projects
> rather than one project. The interface still shows one: the first of
> `me.projects` by name, remembered per account, with no control to
> choose another (spec 028 #13 said where the control would go and left
> it to this spec). So a person with two projects sees one of them and
> the way to the other is the address bar, which nobody knows. This spec
> puts the project in the URL — every screen inside the shell lives
> under `/p/{id}` — and puts a switcher where the sidebar shows the
> project name: the projects the account can reach, each with its
> traffic of the last day, and *New project* for an owner. A link now
> says which project it is about, which is what a link shared between two
> people has needed since there were two people.

---

## Overview

Deliverables, one PR (its last commit flips the status):

- Every route inside the shell moves under **`/p/{project}`**: `/traces`
  becomes `/p/{id}/traces`, and so on for every screen (Decision 1). The
  three screens outside the shell stay where they are.
- The project on screen is the one in the URL; the API client reads
  `X-Tracepad-Project` from there (Decision 2). Bare paths — `/`, and
  every old path such as `/traces?status=error` — redirect to the same
  path under the remembered project (Decision 3).
- A project the account cannot reach, and an account that reaches no
  project at all, get a screen inside the shell rather than a wall of
  `403`s (Decision 4).
- The **switcher** in the sidebar: the projects the account can reach,
  by name, with the number of traces of the last 24 hours beside each,
  and *New project* for an owner (Decisions 5–7).
- Switching keeps the section, keeps the filters, and drops what was
  open (Decision 6).
- `GET /api/v1/projects?activity=24h` puts `traces_24h` on each row
  (Decision 8).
- The application-line ceiling rises to 20,500 (Decision 9).
- `docs/ui.md`, `docs/accounts.md`, `docs/quickstart.md`, `openapi.json`,
  `schema.d.ts`.

Not here: a project in the CLI's URLs (the CLI takes `--project`), a
project-level dashboard or home page, favourites or ordering of the
list, an organisation above projects, moving data between projects, a
project archive.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-11** — Every screen inside the shell lives under **`/p/{project}`**, where `{project}` is the 32-hex project id: `/p/{id}/traces`, `/p/{id}/traces/{trace}`, `/p/{id}/sessions`, `/p/{id}/users`, `/p/{id}/stats`, `/p/{id}/prompts…`, `/p/{id}/datasets…`, `/p/{id}/runs…`, `/p/{id}/score-configs`, `/p/{id}/queues…`, `/p/{id}/quality`, `/p/{id}/settings/{tab}`. The three screens outside the shell — `/login`, `/setup`, `/invite` — keep their paths. In SvelteKit terms the whole tree under `routes/` except those three moves under `routes/p/[project]/`, unchanged inside | The project belongs in the path and not the query, for the reason spec 028 #6 gave the header rather than a query parameter: the query is where the filters live, and a shared `/traces?status=error` must mean the same rows to the person it was sent to. A path segment is also what every SvelteKit route above it can read without a store, and what `page.params` already carries. The id rather than a slug, because a name is renamable (spec 028 #3) and a link that breaks on a rename is worse than a link with a hex segment in it. `/p/` is short, unambiguous, and not a word the sections could ever want. The Server tab of Settings is about the whole server and not the project, and it lives under the prefix anyway: one rule for what is inside the shell beats a second one for one tab. |
| 2 | **2026-09-11** — The project on screen is **the one in the URL**. `project.svelte.ts` derives it from `page.params.project`, and the API client sends that id as `X-Tracepad-Project` (spec 028 #6). Nothing is chosen from `me.projects` any more: the list answers what the id *is* (name, role) and whether the account can reach it; it no longer answers which project to show. Every link the interface writes is built by one helper, `href(path)`, that prefixes the current project — the sidebar's sections, every row link, every `goto`, the settings tabs; a bare `/traces` no longer appears in a component | One source of truth for "which project", and the URL is the one the reader can see, copy and send. Deriving the id from the route rather than syncing a store to it is what keeps back/forward, a reload and a pasted link all agreeing: there is no second state to fall behind. One helper for every link because seventy hand-written paths is seventy places for the prefix to be forgotten, and the test for the helper is the test for all of them. |
| 3 | **2026-09-11** — Bare paths **redirect**. `/` and any path that is not `/p/…` and not one of the three outside the shell redirect (`307`, client-side) to the same path and query under the **remembered project**: the id kept in `localStorage` under `tracepad.project.<account id>` while it is still in `me.projects`, otherwise the first of `me.projects` by name. `/` lands on `/p/{id}/traces`. Opening a screen under `/p/{id}` remembers `{id}`. `?next=` after a sign-in keeps working unchanged, because whatever it names is redirected the same way | Every link in every chat and bookmark today is a bare path, and the pre-authed and `next=` links the server and the login form write are too. A redirect keeps all of them working, and the remembered project keeps spec 028 #13's promise that a reload — or now a bookmark — comes back to what the person was looking at. Client-side, because the server serves one `index.html` for every non-API path (spec 006 #1) and knows nothing about who is signed in. |
| 4 | **2026-09-11** — Two screens inside the shell for a project that is not there. `/p/{id}/…` with an `{id}` that is not in `me.projects` renders **"This project is not yours to see"** — a one-line explanation (it does not exist, or the account is not a member; the interface cannot tell and does not try), the switcher open, and no API request for the screen behind it. An account whose `me.projects` is empty is sent to **`/p`**, which renders **"No projects yet"**: for an owner, the *New project* button; for anyone else, "ask an owner to add you to one". `/p` and `/p/{id}` with no section redirect to `…/traces` when the project is reachable | A `403` per request is what the client would get if it asked (spec 028 #6), and a listing screen full of red cards is not a message. Saying it once, in the shell, with the switcher beside it, is: the person's next move is to pick a project that is theirs. The interface does not distinguish "no such project" from "not a member" because the server does not either (ids are random, nothing to enumerate — spec 028 #6). An account with no projects had no defined screen before this spec; now it has one, and for an owner it is the first step of a fresh server. |
| 5 | **2026-09-11** — The **switcher** replaces the project name in the sidebar: a button showing the current name (a chevron beside it) that opens a bits-ui `DropdownMenu` (spec 006 #3) listing `me.projects` by name, the current one marked, each row carrying its **traces of the last 24 hours** as a caption (`1,234 traces · 24h`; `no traces · 24h` muted when zero). Over eight projects the menu gets a filter box at the top that narrows the list by substring. Below the list, separated, **New project** for an owner (Decision 7). On the phone bar the switcher is the same control in the space the name had | A menu, not a select: the rows carry two lines and the last is an action. The count is what the person opening a switcher wants to know about a project that is not on screen — is it alive — and a day is the window that answers it for a development server and a production one alike. The filter box appears only when the list needs it, so an account with three projects sees three rows and nothing else. Eight is where a column of names stops fitting in one glance. |
| 6 | **2026-09-11** — Switching goes to the **same section** of the other project: the first path segment after `/p/{id}` is kept (`traces`, `sessions`, `users`, `stats`, `prompts`, `datasets`, `runs`, `score-configs`, `queues`, `quality`, `settings`), and for Settings the tab too; everything deeper is dropped — `/p/a/traces/{trace}` becomes `/p/b/traces`, `/p/a/prompts/{name}/versions/new` becomes `/p/b/prompts`, `/p/a/queues/{name}/annotate` becomes `/p/b/queues`. The **query is kept** except the keys that name something in the old project: the page (`cursor`, `direction` — spec 009) and the peek panel (`peek`, `trace`, `obs` — spec 008); `limit`, the filters, the range, `q`, `live` all stay. `switchTarget(url, id)` is a pure function with its own tests | What carries across projects is the question; what does not is the answer. A filter is a question ("errors in production this week") and is the same question of the other project; a trace id, a prompt name, a queue, a cursor are answers and belong to the project they came from — the other one would `404` on every one of them. Keeping the section keeps the person where they were working; dropping the detail keeps the URL honest. |
| 7 | **2026-09-11** — **New project** in the switcher, owners only, opens the same dialog the Server tab's projects table uses — a name, then the keys-once dialog (spec 007 #3) — and on success calls `me` again and navigates to `/p/{new id}/traces`. The Server tab keeps its own button; the two are one component | The switcher is where a person is when they notice the project they want does not exist, and a round trip through Settings for a name is the kind of friction this spec removes. One component so that the keys-once rule has one implementation. Owners only because creating a project is an owner's action (spec 028 #3), and the menu hides what the role cannot do (spec 028 #15). |
| 8 | **2026-09-11** — `GET /api/v1/projects` takes **`activity=24h`** (the one accepted value; anything else `400`): each row then carries **`traces_24h`**, the number of traces whose `timestamp` falls in the last 24 hours, counted the way `GET /api/v1/stats` counts traces — from the hourly roll-up behind the watermark and from the raw rows for the tail (spec 013) — so it costs what one stats call costs and trails live traffic by the same lag. Without the parameter the listing is exactly what it was. The switcher asks on every open, never on load | On the listing rather than on `me`, because `me` is answered on every load and the count is wanted only when the menu opens; on the listing rather than a new route, because the listing already answers each caller with what it can reach (spec 028 #19) and the count is a column of that answer. One value for the parameter today, and a parameter rather than a bare flag, so that `activity=7d` can exist without a second field name. From the roll-up because a raw `COUNT(*)` over a day of a busy project is the query the roll-up exists to avoid, and the switcher is opened often. |
| 9 | **2026-09-11** — The application-line ceiling rises from 20,000 to **20,500**. `main` after spec 028 is 19,481; the switcher with its filter box, the two not-there screens, the redirect route, the `href` and `switchTarget` helpers and the shared create-project dialog are estimated at 300–400 lines net of what the move of the routes leaves unchanged, and one review cycle needs room | The raise rule of spec 015 #9: `main` plus the measured estimate plus a cycle. The route move is a move, not an addition — the counter reads the same files at new paths — so the estimate is the new components alone. The PR reports `make ui-lines` before and after, by file. |
| 10 | **2026-09-11** — A switch that keeps the route **remounts the screen**: the children of `routes/p/[project]/+layout.svelte` are keyed on the project id. `/p/a/traces` → `/p/b/traces` is the same route with one parameter changed, and SvelteKit reuses the page component; every read that component holds — the listing's rows, its count, a stats chart, a settings card — was about the other project and keyed on the URL's query, not its path, so nothing re-asked. Found in the browser: the new project's listing showed the old project's thousand rows | One place, the layout that already knows the id, rather than a project-id dependency added to every loader on eleven screens. A remount is what "another project" means: nothing carries over, which is also what Decision 6 promises about the answers. The cost is one render of a screen that was going to re-read everything anyway |
| 11 | **2026-09-11** — Where *New project* lands depends on where it was opened. From the switcher it is Decision 7 as written: `me` again, then `/p/{new id}/traces`, with the keys dialog over it — the switcher lives in the sidebar, which survives the navigation. From the no-projects screen (`/p`) the navigation waits until the keys are dismissed, because that screen is what the dialog is mounted in and leaving it would take the keys with it. From the Server tab's table the owner stays on the tab and the table re-reads, as before this spec: creating three projects in a row from the admin table is the admin table's job, and a round trip through another project's listing for each would be the friction Decision 7 removes | One component either way (`NewProjectDialog`, with an `oncreated` callback the caller decides about), so the keys-once rule still has one implementation |
| 12 | **2026-09-11** — The API client reads the project id **untracked** (`untrack(() => project.id)` in `projectHeader`). The id now comes off `page.params`, which SvelteKit replaces with a new object on every navigation, and most reads start inside an `$effect` before its first `await` — so a tracked read made every such effect depend on the URL as a whole. Found by the suite: a trace re-read itself on every arrow key (`?obs=`), the item editor re-seeded its panes when `?dataset=` changed under it | A request's project is a fact at the moment the request goes out, not a subscription for the effect that sent it. One line in the one place the header is written, rather than an `untrack` around every call site's read |
| 13 | **2026-09-11** — Errors: `me` is **not re-read on navigation**; the not-there screen is decided at navigation from the `me` already held, and `me` is re-read on sign-in and after the actions that change it (spec 028 #15). The Application contract's Errors paragraph said "`me` is read again on the next navigation", which the code never did; the paragraph now says what it does | A navigation is a question about a project the shell already knows the answer to; a request per navigation would pay for a fact that changes only when somebody changes it. Deciding at the navigation — rather than live, on every `me` — is what keeps the Server tab, with its Restore, under an owner who has just deleted the project on screen (Edge cases) |
| 14 | **2026-09-11** — The **Account tab lives bare**, at `/settings/account`, for everybody: it is the one screen inside the shell that is about the person, not a project. `/p/{id}/settings/account` redirects to it, query kept; the rest route never sees it, because it is a route of its own — the one exception to Decision 1, inside the shell, with the sidebar. Under `/p/{id}/settings` the strip keeps Project and Server, and its Account tab is a link to the bare route; the account menu's *Account* leads there too. On the bare screen the strip shows Account active, with Project and Server as links under the remembered project when there is one and absent when there is none — as the sidebar's sections are on `/p`. The sidebar there is as on `/p`; the switcher works and lands on `/p/{id}/traces` (`switchTarget` treats a path under no project as having no section to keep) | Review of #60 found the *Account* item on `/p` linking to `/p`: under Decision 1 the tab needed a project id, and an account that reaches no project has none — so a member whose last membership was removed could not change their own password. A name and a password belong to the account, not to whichever project it happens to be looking at, and the address should say so |
| 15 | **2026-09-14** — A project made is a project **opened**, wherever it was made from; the second half of Decision 11 — the Server tab keeps the owner on it — is withdrawn. From the switcher it is Decision 7 as written: `me` again, `/p/{new id}/traces`, the keys dialog over it. From `/p` and from the Server tab's table the navigation waits until the keys are put away — both screens are what the dialog is mounted in — and the Server tab lands on **the same tab of the new project**, `/p/{new id}/settings/server`: the rule of Decision 6, `switchTarget(page.url, id)`, applied to the project just made rather than one chosen. `me` is read again before the caller hears of it, as `NewProjectDialog` already does, so the new project is in the switcher and reachable when the URL names it | Found on the stand: an owner created a project from the Server tab and stayed in the old one, which is not what creating one feels like — the person who has just named a project expects to be looking at it. The argument of Decision 11 — three projects in a row from the admin table — does not hold: the projects table is the server's, and the same table on the new project's Server tab is one click from the next one. The same tab rather than the listing because that is what "another project" means everywhere else in the shell (Decision 6): the section stays, the project changes |
| 16 | **2026-09-29** — **The sections a switch keeps are the navigation's own list**, not a second one: the first path segment of each of the eleven screens the navigation lists, and `stats` besides for its redirect (spec 034 #1), twelve names in all. The list in Decision 6 is what that gives today, and a screen added to the navigation is kept by a switch with no second edit. The list is data in `screens.ts`, which imports nothing; `sections.ts` gives each screen its icon and draws it, and `project.svelte.ts` reads only the data, so a switch does not load the navigation's icons. The shape of a project's URL — the prefix, `under()` that writes it and `within()` that reads it — is in one place, `paths.ts`, which imports nothing either | The switch kept its own set of twelve names beside the navigation's eleven (spec 006 #20, spec 016 #1): two lists to keep in step by hand, the second of which is only read when a person changes project, which is where a missed screen shows up as a landing on the dashboard. Deriving it from the navigation needed `within()` on the navigation's side of the import, since `sections.ts` used it from `project.svelte.ts`; a data module and a paths module, each with no imports, are the two places both can read without a cycle and without one of them loading the other's icons or `$app/state`. Behaviour is unchanged for every screen that exists; a test walks the list through `switchTarget` |

## API contract

`GET /api/v1/projects?activity=24h` — policy `member` (unchanged); the
same rows as without the parameter, each with:

```json
{ "id": "…", "name": "…", "role": "editor", "traces_24h": 1234, "…": "…" }
```

`traces_24h` is an integer, present on every row when the parameter is
given and absent otherwise (`openapi.json` marks it so). A soft-deleted
project (`include=deleted`, owners) carries `0`. `activity` with any
other value → `400 "activity: only 24h"`.

Nothing else on the API changes. The interface's own paths are not API
routes: the server's SPA fallback (spec 006 #1) serves `index.html` for
`/p/…` as for any path that is not an API route.

## Application contract

**Routes.** `routes/p/[project]/` holds what `routes/` held, with a
`+layout.svelte` that renders the not-there screen of Decision 4 when the
id is not in `me.projects` and the child routes otherwise; `routes/p/`
renders "No projects yet" (Decision 4); `routes/[...path]/+page.ts`
redirects a bare path per Decision 3; `routes/+page.ts` is the `/` case
of the same. The three screens outside the shell are untouched.

**Project.** `project.svelte.ts` keeps its name and its readers
(`current`, `id`, `name`, `role`, `editor`) and derives them from
`page.params.project` against `auth.projects`; `restore`/`choose` become
`remembered()` and `remember(id)` over the same `localStorage` key.
`href(path, query?)` returns the path under the current project;
`switchTarget(url, id)` per Decision 6. The API client's `projectHeader`
reads `project.id` as it does now.

**Sidebar.** The switcher of Decision 5 where the name was. The sections
link through `href`; `active` compares the path after the prefix. The
account menu's role caption reads `project.role` as now.

**Settings.** `/p/{id}/settings/{project|server}`; `/p/{id}/settings`
redirects to `project`. The Account tab is bare, `/settings/account`, and
`/p/{id}/settings/account` redirects to it (Decision 14); the tab strip
is one component under both. The Server tab's *New project* and the
switcher's share one `NewProjectDialog` (Decision 7).

**Errors.** A `403` that reaches a screen regardless (a membership
revoked under an open tab) renders as spec 028 #15 says. `me` is not
re-read on navigation (Decision 13): it is read on sign-in and after the
actions that change it, and the not-there screen is decided at the next
navigation from the `me` the shell already holds.

## Testing

Unit (vitest):

- `href` prefixes every section path and keeps a query it is given;
  `switchTarget` keeps each section and the Settings tab, drops every
  deeper segment, keeps `limit` and the filter keys, and drops `cursor`,
  `direction`, `peek`, `trace`, `obs` (a table of cases, one per section).
- `project.svelte.ts`: the current project follows the URL, an id not in
  `me.projects` reads as `null`, and the remembered id survives a reload
  and is ignored once the account cannot reach it.
- The switcher component: lists by name, marks the current, shows the
  count and the zero caption, shows the filter box over eight projects
  and narrows by substring, shows *New project* to an owner and not to a
  viewer.
- The redirect: bare `/traces?status=error` → `/p/{remembered}/traces?status=error`;
  `/` → `/p/{id}/traces`; an account with no projects → `/p`.

Server (Go):

- `activity=24h` puts `traces_24h` on each row and matches what
  `GET /api/v1/stats` answers over the same day for that project, across
  the watermark; a wrong value is `400`; without the parameter the field
  is absent and the body is byte-for-byte what it was.

e2e (Playwright), against the seeded corpus with an owner and an editor:

- The owner opens `/`, lands on `/p/{id}/traces`, and a bare
  `/traces?environment=prod` lands on the same project with the filter kept.
- The switcher names both of the owner's projects with a count on each;
  choosing the other from `/p/a/traces/{trace}?environment=prod` lands on
  `/p/b/traces?environment=prod`, from `/p/a/settings/server` on
  `/p/b/settings/server`, and from the bare `/settings/account` on
  `/p/b/traces` (Decision 14).
- An account with no projects opens *Account* from the menu and changes its
  password there; `/p/{id}/settings/account` lands on the bare tab, and the
  Project tab leads back under the same id (Decision 14).
- The editor, who reaches one project, sees one row and no *New project*;
  `/p/{the owner's other id}/traces` renders the not-there screen and
  sends no request with that id in the header.
- *New project* from the switcher creates one, shows the keys once, and
  lands on its empty trace listing, whose sidebar names it.
- *New project* from the Server tab's table shows the keys once and, when
  they are put away, lands on `/p/{new}/settings/server`: the switcher
  names the new project and the table lists it (Decision 15).
- A reload of `/p/{id}/prompts` stays there; the `?next=` round trip
  through `/login` returns to a `/p/…` path unchanged.

## Edge cases

- **A project renamed under an open tab.** The URL carries the id, so
  nothing breaks; the sidebar shows the new name after the next `me`.
- **A project deleted under an open tab.** The next `me` drops it, and the
  not-there screen renders on the next navigation; the switcher lists
  what is left. If nothing is left the person is sent to `/p`.
- **A membership revoked under an open tab.** Same as deleted, from that
  account's side; the remembered id is ignored (Decision 3).
- **Two accounts on one browser.** The remembered project is per account
  (spec 028 #13), so signing out and in as somebody else lands on their
  project, not the previous person's.
- **The id in the URL is malformed** (not 32 hex): the not-there screen,
  the same as an unknown one; no request goes out.
- **An owner with no projects** (a fresh server after setup, or every
  project deleted): `/p` with the *New project* button, and the switcher
  shows *New project* alone.
- **A phone width.** The switcher's menu opens over the page the way the
  account menu does; the filter box is a text input with the sections
  still reachable behind the menu when it closes.
- **The same project chosen again.** Nothing happens; the menu closes.

## Config additions

None.

## Out of scope

The CLI's own notion of the current project (`--project`, spec 004);
favourites, ordering or grouping of projects; an organisation or a team
above projects; a per-project home or dashboard; moving or copying data
between projects; other activity windows than 24 hours on the listing.
