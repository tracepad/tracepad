# Web interface

A browser client of the read API, served by the same binary on the same port.
It has no endpoints and no logic of its own: everything it shows is reachable
with `curl`, and a feature that is not in [api.md](api.md) does not exist.

## Opening it

The interface is at the server's root — `http://localhost:4318/` on the
default listen address.

It always authenticates. There is no localhost bypass: behind any local
reverse proxy every request looks like loopback, which would silently turn
authentication off for the internet.

- **First run.** The server prints a pre-authed link next to the connection
  strings (`http://localhost:4318/#key=tp-sk-…`). Open it and the interface
  is signed in. The key travels in the URL fragment, which browsers never
  send to a server; the app stores it and removes it from the address bar.
- **Later runs.** The server prints the plain URL, and the login screen asks
  for a project key.
- The key is kept in `localStorage` and sent as `Authorization: Bearer`. A
  `401` clears it and returns to the login screen. "Sign out" in the sidebar
  forgets it.

The interface reads traces, which needs a **project key**. The admin token is
a control-plane credential (see [admin.md](admin.md)) and the login screen
refuses it with an explanation. It is entered further in, on the Settings
screen, where the endpoints it can actually call live.

## Screens

**Traces** — the listing. One row per trace, mapping onto
`GET /api/v1/traces`: time, name, environment, user, session, cost, latency,
TTFT and how many observations failed. TTFT sits beside latency because the
two answer the same question from opposite ends — how long the whole run took,
and how long somebody waited before anything appeared. The filter bar offers
exactly the filters the endpoint accepts (`q`, `from`, `to`, `environment`,
`user_id`, `session_id`, `name`, `tag`, `status`, `min_cost`, `release`,
`version`, `type`, `prompt`) — a test reads `openapi.json` and fails if the
two ever disagree. The bar underneath turns the pages.

There is no release column: release is a filter, and it is shown in the header
of a trace rather than in every row of the listing.

**Search** — the box on the bar, beside the time window. It searches what the
observations carried, not their labels: prompts, answers, metadata, names and
error messages. It commits on Enter and on leaving the box, never on a
keystroke, and it lives in the URL like every other filter.

A row that matched grows a second line under it: which observation and field
the hit was in, and the text around it, with the words of the query marked.
Clicking such a row opens the panel **on that observation** rather than at the
top of the trace — which is the whole reason the row says where it matched.
The matching rules are the API's, and worth knowing at the box: words rather
than substrings (`err*` for a prefix), `"quoted words"` must be adjacent, and
all the words have to occur in the same field of the same observation. See
[api.md](api.md#search).

**Live** re-reads the newest page every five seconds and shows it. It is off
by default, pauses while the tab is hidden, and only runs on the newest page
(see [Turning pages](#turning-pages)). Same caveat as `tracepad tail`: traces
are ordered by their own timestamps, so a span that arrives late appears
where it belongs rather than at the top — which on a full page means it can
push the oldest row of that page off it.

**Sessions** — one row per session over `GET /api/v1/sessions`: last seen,
id, how many traces, how many of them failed, cost, first seen. The filters
are the endpoint's four (`from`, `to`, `environment`, `user_id`), and a row
opens the session: its totals over its traces, and a trace opens from there.
Every number counts traces, which is what a session is a collection of.

## Turning pages

Every listing sits on a bar: how many rows per page, what is on screen
against what matches, and four ways to move — « newest, ‹ previous, next ›,
oldest ». The page is in the URL (`?limit=&cursor=&direction=`), so a reload
comes back to it and a link carries it.

There are no page numbers, and the reason is the same one that makes the
listing fast. Pagination is a **keyset**: a page is found by seeking to a
cursor, so page four hundred costs what page one costs — and both ends are
just a direction to read the index in, which is why « and » are as cheap as ‹
and ›. An ordinal ("page 12 of 40") would need `OFFSET`, which counts and
discards every row it skips, plus a full count on every filter change. In a
listing sorted by time an ordinal is not an address anyway: nobody wants page
40, they want 26 August — and the time range control answers that.

The count beside the rows is capped at 1000 and reads `1000+` past that. It
follows the filters, not the page, so turning a page does not re-count. The
cap bounds the number, not always the work behind it — see
[Counting](api.md#counting) for where that costs something — but it is always
a fraction of what the listing on the same filter already costs.

**Live** only runs on the newest page. Anywhere else the toggle is paused and
says why: re-reading the newest page would replace the page you navigated to.

## The peek panel

A row does not navigate away from its listing. It opens in a panel that
slides in from the right, over the listing, which stays where it was — with
its filters, the pages you already loaded and the position you scrolled to.
Reading a listing is a loop, and this is the loop:

- **Click a row** to open it, and click another to swap the panel over to it;
  the row it is showing stays lit. **⌘/Ctrl-click** still opens the full page
  in a new tab, because the row is a real link to it. Dragging across a cell
  selects its text as it would anywhere else — an id in a listing is
  something you copy into a terminal — and a click that ended a selection
  opens nothing.
- **`k`** and **`j`**, or the two chevrons in the panel's header, walk the
  rows — the buttons carry the key they answer to. On the last row of a page
  `j` turns the page and opens the first row of the next one, so a scan does
  not stop at a boundary that is an artefact of paging; both keys dim only at
  the ends of the whole listing. If the panel is open on a row the page no
  longer holds — a link somebody sent, a live tick that pushed it out of the
  newest page — the keys still mean what they say, and go to the nearest row
  that way. A letter typed into a filter field is a letter, not a shortcut.
- **⤢** opens what the panel is showing as a full page — `/traces/{id}` or
  `/sessions/{id}`, the selected observation included. That is the link to
  send somebody.
- **Escape**, or **✕**, closes it. So does the browser's Back button: what
  the panel shows is in the URL (`?peek=`), so a reload comes back to it.

A session's panel goes one level deeper: a trace in its table replaces the
panel's body, and **‹ Session** in the header returns to the session. On a
phone the panel covers the screen, which is the whole reason it is there —
closing it costs nothing, where a page navigation would read the listing
again.

**Stats** — four charts over `GET /api/v1/stats` — traces, cost, latency
(p50 and p95) and errors — sharing one x cursor, plus breakdown tables by
model, by environment and by release with proportion bars. The bucket switcher
is hourly/daily and defaults to hours for windows up to 48 hours, days above.
In the release table, the traces that named none are one row called
*(no release)* rather than a row that is missing.

A bucket the server did not return is drawn as a **gap**, never as a zero,
and a bucket that reported no cost has no cost point: the API refuses to
fabricate rows and so does the screen. An empty window says so rather than
drawing an empty frame.

Neither screen has a live mode. Both re-read on a filter change and on the
**Refresh** control; the Traces live toggle is the only poller in the app.

**Settings** — see [Settings and administration](#settings-and-administration).

**Trace** — the observation tree on the left, the selected observation on the
right. A node shows its kind, name, duration, cost and whether it failed;
a parent whose descendant failed is marked too, so collapsing a subtree never
hides a failure. Arrow keys walk the tree — up and down move, right opens,
left closes and then leaves.

The kind is an icon — a wrench for a tool call, a shield for a guardrail, a
box for a plain span — each carrying its name for a tooltip and for a screen
reader. There are ten kinds, which is more than a three-letter label can keep
readable.

The detail panel shows the observation's timings (including TTFT, when the
client reported when its first token came back), level, model, usage and cost,
then `input`, `output` and `metadata` as collapsible JSON with each payload's
size beside its heading. A generation that ran a named prompt carries a badge
saying which — `support-answer · v7` — that leads to the traces which ran it.
The badge is a link to a filtered listing, not a lookup: this store need not
manage that prompt for the link to work. This is the same view the peek panel
shows — one component, two frames around it.

The header above the trace names its release when it has one, beside the
timestamp, latency and cost.

## Payloads and the response budget

A trace is fetched with `?expand=io`, so the server spends its byte budget on
the payloads and replaces the ones that do not fit with truncation markers
(see [api.md](api.md#the-response-budget)). The interface consumes those markers
rather than working around them:

- a truncated payload renders as its preview plus **Load the whole N KB**,
  which fetches `GET /api/v1/observations/{id}/io` — the one endpoint no
  budget applies to — and swaps the whole value in;
- a trace with more payloads than the budget can carry markers for gets none
  of them, and each payload offers a load button of its own.

Raising `TRACEPAD_RESPONSE_BUDGET_BYTES` inlines more of them up front.

## Settings and administration

Settings runs on **two credentials with disjoint powers**.

The session's project key manages its own project, which is what a project
key is for (see [admin.md](admin.md)):

- **Project** — its name and id. Renaming needs the admin token, because a
  project's name is the echo every destructive confirmation is typed against;
  the field says so and names `tracepad projects rename`.
- **Retention** — both windows, with `null` spelled out: "keep forever" for
  the queryable data, "follow the window above" for the raw bodies.
- **API keys** — the public keys with their dates, minting, and revocation.
  A minted pair is shown **once**, in both connection formats, exactly as
  first run prints them; the secret is stored as a hash and the dialog says
  so rather than implying it can be found again.
- **Danger zone** — erasing everything stored about one end user.

Below them, **Administration** unlocks with this server's
`TRACEPAD_ADMIN_TOKEN` and covers project lifecycle only: list (soft-deleted
projects included, with their purge dates), create, delete, restore. The
token is stored separately from the project key and is sent only to those
endpoints — it never reads a trace. "Lock" forgets it, and so does signing
out.

Every destructive action is the server's dry-run/confirm contract rendered
(see [admin.md](admin.md#dry-run-by-default)): the card asks the API
what the change would delete, shows that answer, and enables its button only
once you have typed back the identity the server named — the project's name,
or the user id. Nothing is counted in the browser, and a refusal is reported
in the server's own words.

## State in the URL

Filters, the search, live mode, the time window, the stats bucket and the
selected observation all live in the query string, so any view is a link:
`/traces?q=refund+failed`, `/traces?status=error&environment=prod`,
`/traces/{id}?obs={observation_id}`,
`/sessions?environment=prod`, `/stats?from=…&to=…&group_by=hour`. Reloading,
sharing and the back button all behave.

The time window is one control on every screen that has one — presets for the
last hour, day, week and month, plus a calendar — and it travels as the
`from`/`to` the API itself takes. A preset sets `from` and leaves the end
open, so "the last 24 hours" keeps ending now.

## Appearance

Light and dark ship together and follow the operating system by default; the
toggle in the sidebar pins one and remembers it.

Everything the page needs is inside the binary — fonts included. The
interface makes **no request to any external origin**, which an air-gapped
install depends on and which the end-to-end suite asserts.

The layout is usable on a phone: the sidebar becomes a two-row top bar, the
filters live in a popover, tables scroll inside their own box rather than
scrolling the page, and the trace screen switches between the tree and the
observation instead of showing both.

## Builds without it

The interface is compiled by Node and Vite, which `go build` cannot run. A
plain source build therefore ships without it and serves a page that says so;
the API, the CLI and the MCP server of such a build are complete.

Every official artifact carries it — the release binaries, Homebrew and the
Docker image — as does a local `make build`, which builds the bundle first.
`go install` is not a supported channel for this reason.

## Working on it

The sources are in `ui/`: a SvelteKit SPA (`adapter-static`, no SSR) with
Tailwind v4, built by Vite into `ui/dist`. The only runtime dependencies are
bits-ui (headless primitives), Lucide (icons) and uPlot (the four charts).

```sh
make ui          # build the bundle and stage it for embedding
make build       # the above, then `go build -tags ui`
make ui-check    # svelte-check, unit tests, and the API-type drift check
make e2e         # boot the real binary on a temp database, run Playwright
```

`npm run dev` inside `ui/` serves the app with hot reload and proxies `/api`
to a `tracepad serve` running on the default port.

The TypeScript types of the API are generated from
`internal/server/openapi.json` and committed; `make ui-types` regenerates
them, and the gate fails if the committed copy has drifted.
