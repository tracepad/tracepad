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
`GET /api/v1/traces`: time, name, environment, user, session, cost, latency
and how many observations failed. The filter bar offers exactly the filters
the endpoint accepts (`from`, `to`, `environment`, `user_id`, `session_id`,
`name`, `tag`, `status`, `min_cost`) — a test reads `openapi.json` and fails
if the two ever disagree. **Load more** follows the cursor.

**Live** re-reads the newest page every five seconds and merges it into what
is on screen by trace id, so nothing duplicates and nothing jumps. It is off
by default and pauses while the tab is hidden. Same caveat as `tracepad
tail`: traces are ordered by their own timestamps, so a span that arrives
late appears where it belongs rather than at the top.

**Sessions** — one row per session over `GET /api/v1/sessions`: last seen,
id, how many traces, how many of them failed, cost, first seen. The filters
are the endpoint's four (`from`, `to`, `environment`, `user_id`), and a row
opens the session: its totals over its traces, and a trace opens from there.
Every number counts traces, which is what a session is a collection of.

## The peek panel

A row does not navigate away from its listing. It opens in a panel that
slides in from the right, over the listing, which stays where it was — with
its filters, the pages you already loaded and the position you scrolled to.
Reading a listing is a loop, and this is the loop:

- **Click a row** to open it, and click another to swap the panel over to it;
  the row it is showing stays lit. **⌘/Ctrl-click** still opens the full page
  in a new tab, because the row is a real link to it.
- **‹ ›** in the panel's header walk the rows the listing has loaded. They
  stop at the ends rather than fetching the next cursor page.
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
model and by environment with proportion bars. The bucket switcher is
hourly/daily and defaults to hours for windows up to 48 hours, days above.

A bucket the server did not return is drawn as a **gap**, never as a zero,
and a bucket that reported no cost has no cost point: the API refuses to
fabricate rows and so does the screen. An empty window says so rather than
drawing an empty frame.

Neither screen has a live mode. Both re-read on a filter change and on the
**Refresh** control; the Traces live toggle is the only poller in the app.

**Settings** — see [Settings and administration](#settings-and-administration).

**Trace** — the observation tree on the left, the selected observation on the
right. A node shows its type, name, duration, cost and whether it failed;
a parent whose descendant failed is marked too, so collapsing a subtree never
hides a failure. Arrow keys walk the tree — up and down move, right opens,
left closes and then leaves.

The detail panel shows the observation's timings, level, model, usage and
cost, then `input`, `output` and `metadata` as collapsible JSON. This is the
same view the peek panel shows — one component, two frames around it.

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

Filters, live mode, the time window, the stats bucket and the selected
observation all live in the query string, so any view is a link:
`/traces?status=error&environment=prod`, `/traces/{id}?obs={observation_id}`,
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
