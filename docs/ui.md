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
refuses it with an explanation.

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

**Trace** — the observation tree on the left, the selected observation on the
right. A node shows its type, name, duration, cost and whether it failed;
a parent whose descendant failed is marked too, so collapsing a subtree never
hides a failure. Arrow keys walk the tree — up and down move, right opens,
left closes and then leaves.

The detail panel shows the observation's timings, level, model, usage and
cost, then `input`, `output` and `metadata` as collapsible JSON.

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

## State in the URL

Filters, live mode and the selected observation all live in the query string,
so any view is a link: `/traces?status=error&environment=prod`,
`/traces/{id}?obs={observation_id}`. Reloading, sharing and the back button
all behave.

## Appearance

Light and dark ship together and follow the operating system by default; the
toggle in the sidebar pins one and remembers it.

Everything the page needs is inside the binary — fonts included. The
interface makes **no request to any external origin**, which an air-gapped
install depends on and which the end-to-end suite asserts.

The layout is usable on a phone: the sidebar becomes a top bar, the filters
live in a popover, and the trace screen switches between the tree and the
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
Tailwind v4, built by Vite into `ui/dist`.

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
