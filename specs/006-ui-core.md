# Spec 006 — UI Core: Scaffold, Design System, Traces & Trace Detail

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> The UI is the third client of the read API, after the CLI and MCP (design
> §3): it has no endpoints and no business logic of its own, and nothing it
> shows is unreachable with `curl`. This spec ships the SPA scaffold, the
> design system, authentication, and the two screens that carry the product's
> core value: the trace list and the trace detail view. Sessions, Stats and
> Settings follow in spec 007 on the same foundation.

---

## Overview

Deliverables:

- `ui/`: SvelteKit (Svelte 5 + TypeScript) SPA built by Vite, embedded into
  the server binary; served from the same port as the API.
- Design system: Tailwind v4 token file (light + dark), bundled fonts,
  bits-ui primitives, an application shell (sidebar, header, theme toggle).
- Auth: key-based login, pre-authed first-run URL, 401 handling.
- **Traces** screen: filterable, cursor-paginated, live-updating table.
- **Trace** screen: observation tree + detail panel with lazy JSON viewers
  built on the API's truncation markers.
- Typed API client generated from `openapi.json`, drift-checked.
- Build & gate integration: `make` targets, UI checks in the precommit gate,
  Playwright smoke as a separate CI job, UI line-count warning (design §8).

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-08-28** — The app is **SvelteKit** with `adapter-static` and `ssr = false`: a pure SPA compiled to static files, no Node at runtime. The Go server serves it at `/`; any GET that matches no API route and accepts HTML falls back to `index.html` | Design §8 fixed Svelte 5 + Vite but left routing open. A hand-rolled router is where deep links, scroll restoration and history quietly rot; SvelteKit's file-based routing is the mainstream path with the deepest tooling and the largest training corpus for the agents writing this code. `adapter-static` emits exactly the static bundle `embed.FS` needs — the Kit server layer is compiled away. |
| 2 | **2026-08-28** — Styling is **Tailwind CSS v4**. The `@theme` block in one `app.css` *is* the single token file design §8 asks for (colors, spacing, typography, light/dark), and components use utilities plus the occasional scoped style for what utilities express poorly | Deviation from design §8's "no UI kit, one CSS token file" *letter*, keeping its intent: Tailwind v4 is a dev-only dependency compiled to a small static sheet, the token file remains singular (`@theme` is CSS, not JS config), and utility classes cost fewer budget lines than per-component scoped blocks (~30–50 lines × every component). The deciding argument is the development method: agent-written CSS drifts component by component; a constrained utility vocabulary over shared tokens is the cheapest consistency mechanism we can buy. |
| 3 | **2026-08-28** — Interaction primitives that are hard to get right — dialog, dropdown/select, tooltip, popover, tabs — come from **bits-ui** (headless, unstyled, Svelte 5 native), styled with our tokens. Domain components — trace tree, JSON viewer, data table — are written by hand. shadcn-svelte is rejected | Focus traps, aria wiring and keyboard navigation are exactly where hand-rolled UIs read as amateur and agents silently cut corners; headless primitives buy correctness without imposing a visual language. shadcn-svelte would vendor thousands of styled lines into our tree — spent against the design §8 line budget for code we did not design. Hand-writing the domain components is the budget's purpose: they *are* the product. |
| 4 | **2026-08-28** — Both themes ship from day one as token pairs; the default follows `prefers-color-scheme`, a manual toggle in the shell overrides it, persisted in `localStorage` | Themes multiply tokens, not code — cheap now, painful to retrofit after components have hardcoded assumptions. The dev-tool audience lives in dark mode; the office next door does not. Contrast is checked per theme (§ Testing). |
| 5 | **2026-08-28** — Fonts are bundled: **Inter** (UI) and **JetBrains Mono** (identifiers, numbers, JSON) as subset woff2 files inside `embed.FS`, `font-display: swap`, tabular figures (`font-variant-numeric: tabular-nums`) on all numeric columns. The UI makes **zero requests to external origins** — no font CDNs, no telemetry, nothing | A self-hosted, possibly air-gapped tool must render identically offline and must not phone out — an observability product that leaks its operator's presence to a CDN has failed its own pitch. System font stacks render differently per OS and lack reliable tabular figures, which live-updating numeric columns need to not jitter. Cost: ~100 KB in the binary. |
| 6 | **2026-08-28** — Visual direction: quiet dev-tool minimalism (the Linear school). Dark slate surfaces (light: near-white), 1 px low-contrast borders instead of shadows, one restrained accent for interactive states, small type (13–14 px body in data views), density via an 8 px spacing rhythm, no decorative motion — transitions only on state (100–200 ms) | A trace viewer is read for hours; visual noise is fatigue. This direction is also the cheapest to execute consistently with utilities + tokens (flat surfaces, borders, no elevation system) and the house style of the tools this audience already trusts. Concrete palette lives in `app.css` as tokens; this spec fixes the direction, not the hex values. |
| 7 | **2026-08-28** — The data layer is a thin typed client: TypeScript types are **generated from `openapi.json`** (`openapi-typescript`, committed output, a gate check regenerates and fails on drift), fetch wrappers add the key and map errors; state is Svelte 5 runes — `$state.raw` for API responses, `$derived` for views, polling via `$effect`. No TanStack Query, no stores libraries | The OpenAPI document already exists and is parity-tested against the router (spec 004 #9) — generating types extends that single source of truth into the browser, and the drift check extends the parity culture. Runes cover our needs (design §8 chose Svelte partly for this); a query cache would add a second reactivity model on top of runes and invite React idioms into agent-written Svelte. |
| 8 | **2026-08-28** — The UI always authenticates: a key or admin token pasted at a login screen, kept in `localStorage`, sent as `Authorization: Bearer`. First run prints a **pre-authed URL** built from the configured listen address (`http://localhost:4318/#key=tp-sk-…` on the default of spec 001 #1) next to the connection strings; the app reads the fragment, stores the key, and strips it from the URL. There is **no localhost auth bypass**. A 401 clears the stored key and returns to login | Deviation from design §6.3 ("localhost without a password by default"): "is this localhost?" is answered from the connection's remote address, and behind any local reverse proxy — the standard TLS setup — *every* request becomes loopback and auth silently turns off for the internet. The pre-authed URL keeps the zero-friction first contact (click the link the server printed) without the silent hole; the fragment never reaches the server (URL fragments are not sent in requests), and the secret is only printable at creation time anyway — later startups print the URL without the key, which is the login screen's job. |
| 9 | **2026-08-28** — `ui/dist` is gitignored. `go build` without the `ui` tag embeds a stub page ("built without UI, use an official artifact — the API is fully functional"); `make build` and every official artifact (releases, brew, docker) build the SPA first and compile with `-tags ui`. **`go install` is dropped from the distribution channels** (design §9 amended) | Committing built bundles poisons every UI PR's diff and history; `go install` compiles only what is in the repo and cannot run Vite, so it can never carry the SPA. Rather than ship a degraded flagship channel, we promise only channels we control end to end. The stub keeps sourced builds honest: server, CLI and MCP work, and the page says why the UI is absent. Go-side tests run tagless, so the Go gate never needs Node. |
| 10 | **2026-08-28** — Gate: `svelte-check` + `vitest` run in `make precommit` (Node and `npm ci` are dev prerequisites, documented); Playwright smoke against the real binary is `make e2e` and a separate CI job, never in precommit. npm with a committed lockfile; Node version pinned in `package.json` `engines` | The 30-second precommit budget (PROCESS) holds type-checking and component tests but not browser automation. Playwright still must exist — the embed, the SPA fallback and the auth flow only break at the seam the unit layer cannot see — so it gates merges in CI instead of every commit. npm because it is the boring default contributors already have. |
| 11 | **2026-08-28** — The UI ships in two specs: **006** (this one) — scaffold, design system, auth, Traces, Trace detail; **007** — Sessions, Stats (uPlot), Settings. The design §8 budget (~6–9k lines) spans both; 006 aims to stay under ~5k, and the CI line-count warning lands here | One PR for five screens would be unreviewable at the depth our review rounds actually reach. The cut is by dependency, not size symmetry: 006 builds everything 007 stands on, and Traces → Trace detail is the value path a beta user judges the product by. |
| 12 | **2026-08-28** — Live mode on the trace list is polling: every 5 s (paused when the tab is hidden), re-fetch the first page with the active filters and merge rows by `(timestamp, id)`. Off by default, toggled in the UI, state in the URL | The CLI already walked this path: `tracepad tail` polls the public API (spec 004 #13) because a push channel is a new server surface for one consumer. The UI inherits the same reasoning and the same arrival-vs-timestamp caveat (spec 004 #32) — merging a re-fetched first page by id is the windowed variant, and hidden-tab pause keeps an always-open dashboard from hammering a small server. |
| 13 | **2026-08-29** — The interface authenticates with a **project key**. The admin token is refused at the login screen with an explanation (the interface reads traces with a project key; the admin token manages projects and keys). Login validates the credential with the request the app actually needs — `GET /api/v1/traces?limit=1` — so a credential that cannot read the screens cannot get past it. The **project picker leaves this spec**: the shell shows the project name from `GET /api/v1/projects`, which is exactly one entry for a project key | Written after reading the shipped code: every read endpoint resolves its caller through `ProjectBySecret`, and the admin token is only a credential in `admin.go` — spec 005 #11 keeps it on the control plane, off the data plane. So the picker this spec promised could only ever appear for a credential that cannot read the screens behind it, and an admin token allowed through login would 401 on Traces, be cleared by this spec's own 401 rule, and land back on login in a loop. Validating with the request the app needs is spec 003 #23 applied to the login screen: a check that cannot mean what it says is worse than no check. Giving the admin token a data-plane scope would be a security-model change smuggled in through a UI PR; the picker returns in 007 with Settings, where the admin token has something to do |
| 14 | **2026-08-29** — Icons come from **Lucide** (`@lucide/svelte`), never hand-drawn ad-hoc SVG; the product logo is the one exception. Sizes are tokens (`size-3.5` / `size-4` on the component), icons are imported one module at a time (`@lucide/svelte/icons/…`) rather than from the package root, and every icon-only control still carries our own `aria-label` | Amends Decision 3, on the same reasoning: one icon family keeps stroke width, alignment and sizing consistent across the product, and hand-drawn glyphs drift the moment a second screen needs one. Lucide is ISC, tree-shaken to only the icons imported, and its 24px / 2px-stroke language matches the visual direction of Decision 6. The per-icon import path is not style: importing from the package root pulls the whole barrel into everything that is not a production build, which took `svelte-check` from 939 files to 4666 and the unit suite from 2 s to 10 s — straight out of the gate's 30-second budget. Buying the drawing does not buy the accessibility: naming an icon-only button is still ours to do |
| 15 | **2026-08-29** — The interface is **usable on a phone**, not merely unbroken: 375 px portrait is a supported width. The sidebar becomes a top bar below the `md` breakpoint; the filter bar collapses into a popover; the trace detail's two panes become one pane with a tree/detail switch; wide tables scroll inside their own container and never the page; touch targets grow on coarse pointers rather than at a width, and text inputs reach 16 px there so iOS does not zoom on focus; heights are `dvh`. The Playwright smoke runs at 375 × 812 as well as on the desktop viewport | Replaces this spec's original "phone-first ergonomics are a non-goal". Somebody paged at night reads the failing trace on the phone in their hand, and that is exactly the moment this product is for. Doing it now costs a handful of variants on layouts we are writing anyway; retrofitting it costs re-deciding every screen. Coarse pointers rather than a width breakpoint, because a narrow window on a laptop is still a mouse, and a tablet at 1024 px is still a finger |
| 16 | **2026-09-27** — **One Node, 24; TypeScript 6.0 until 7.1.** Amends #10's pin: `engines.node` is `^24` (was `^22.13 \|\| ^24`), and `make ui-deps` — so `ui`, `ui-check`, the gate and `e2e` — first checks the Node on PATH against it with `scripts/node-engines.mjs` and stops, naming the version it found. `@types/node` is on major 24, and Dependabot does not propose another. TypeScript stays on 6.0 here: `typescript` is `npm:@typescript/typescript6@^6.0.2`, the alias the Node package uses for the same API (spec 032 #19), so every tool that imports the compiler as a library — `svelte-check`, `openapi-typescript` — runs on TypeScript 6.0's API. `overrides` hands `openapi-typescript` the alias, whose peer range stops at 5.x. TypeScript 7 (`@typescript/native`, `tsc`) is not installed: the interface's type check is `svelte-check`, and plain `tsc` does not read `.svelte` files, on 5.9 or on 7, so it would be a 26 MB binary in every `npm ci`, the image's included, that nothing runs. The tsconfig needed no change: it sets no `baseUrl` and nothing 6.0 deprecates. The interface moves to TypeScript 7 in full — the alias goes, `tsc` comes — when 7.1 ships a stable API and `svelte-check` runs on it, the condition the Node package's alias ends on too | The interface's Node is a build tool: users run the bundle inside the Go binary, so one version is enough, and the active LTS is the one. A Node 26 first on PATH failed 98 unit tests with nothing saying why; the check says why before `npm ci`. TypeScript 7 has no stable programmatic API before 7.1, and the TypeScript team's own announcement says Svelte workflows stay on 6.0 until then. Without the override `npm ci` refuses the tree (`ERESOLVE`), and a lockfile that needs a flag is the trap spec 032 #15 names. Measured on Node 24: `tsc --noEmit` over the interface's `.ts` files took 1.80 s on 5.9 and 0.29 s on 7, which is what the interface gains when 7.1 lets it move; `svelte-check` on 6.0 took what it took on 5.9. |
| 17 | **2026-09-26** — The login form's return path (`?next=`) is refused — the fallback is used — when it resolves to a path that **starts with two slashes**, on top of the same-origin check; and the screens outside the shell are compared decoded and without trailing slashes, so `/%6Cogin` and `/login/` are `/login`; a path that does not decode falls back too | `/.//elsewhere.example/x` and `/%2e//elsewhere.example/x` resolve on this origin, so the origin comparison passed them, and their path is `//elsewhere.example/x`: handed on as a path, a protocol-relative URL to another host. SvelteKit's `goto` refuses a cross-origin target, so nothing left the site, but a redirect that is safe only because the next layer catches it is one refactor from not being safe. No screen of this interface has an empty first path segment, so the rule refuses nothing real. |
| 18 | **2026-09-28** — **On a phone a listing folds rather than scrolls** (amends #15 for three tables). Below `md` — the width at which the sidebar becomes a top bar — the trace table, a queue's items and the Accounts card render a layout of their own instead of a 768 px table in a box: the columns that say which row it is and whether it went wrong stay columns, and every other column folds into lines under the row's name, joined with `·`, absent values left out, in an order of its own rather than the columns' — what it ran as, took and cost before whose it was. A folded line **wraps, and never truncates**: it breaks only between values, after the dot, so a value is never split and no line starts with a dot, and a value longer than the whole line is the only thing cut, alone (`Folded.svelte`). The traces keep *Time*, *Name*, *Errors*; under the name go the environment, the latency, `TTFT …` and the cost, and on a third line the user and the session — still the links of spec 023 #16/#17, each cut on its own so a long user id cannot push the session out of reach, and 24 px tall on a coarse pointer (#15). A queue keeps *Target*, *Status* and the verbs, which become icons named for a screen reader (*Reopen*, *Remove*); under the target go `#seq`, who and when, and the skip reason on a line of its own. Accounts keep *Email* and *Edit*/*Delete* stacked; under the email go the name, the standing in its colour, `last login …`, and `Projects: …`, and that cell is a plain cell rather than the row's header — a header is read before every cell of the row, and this one is the whole account — so the buttons are named *Edit {email}* and *Delete {email}* instead. The choice is made by `MediaQuery` on `(width < 48rem)` (`ui/src/lib/phone.ts`), the query Tailwind's `max-md` is, so the two leave no width between them; one layout is in the document, never both with one hidden. The switch is the viewport, not the table's own box: from `md` up the box can still be narrower than 768 px — beside the sidebar up to about 975 px, or in a panel beside a listing — and there the table scrolls inside it, as #15 allows. Every other table still scrolls in its own box | The page never scrolled sideways — #15 held on all three — but on a 390 px phone the box scrolled to Environment and stopped: the errors, the cost, the skip reason, the Reopen and Remove buttons and an account's Edit were past its edge, and iOS draws no scrollbar until you scroll, so nothing said they were there. The person #15 names — paged at night, on a phone — needs *did it fail* on the first screen, not behind a swipe. Hiding the other columns alone would have taken information away; folding keeps all of it at the price of a second line. The user and the session stay because the panel's own meta hides the session below `md`, so without them a phone had no way to either page; the TTFT stays because it is the trace's, and the panel shows the generation's instead. A CSS-hidden copy per breakpoint was the cheaper build and was refused: hidden text is still found by find-in-page and matched by tests (`getByText` saw two elements), and a screen reader on a resized window hears the wrong one |
| 19 | **2026-09-28** — **The application-line ceiling (`UI_BUDGET`) rises from 22,700 to 22,900** (owner decision 2026-09-28) | #18 was written against a ceiling of 22,200 and raised it by 200, to 22,400; spec 047 #27(h) raised the same ceiling to 22,700 before #18 landed, so the two raises are added rather than one taking the other's place. `main` measured 22,580 at the merge, and #18 adds 139 — the phone layouts of three tables, the folded line and the module that names the breakpoint — for 22,719. The raise leaves 181 lines, not a reserve for the next spec, which argues for its own |

## Application contract

Routes (SvelteKit file routes; all deep-linkable):

- `/login` — key entry; consumes a `#key=` fragment from any route.
- `/traces` — the trace list. `/` redirects here.
- `/traces/{id}` — the trace detail; `?obs={observation_id}` selects a node.
- Unknown routes render a 404 page inside the shell.

Filter, live-mode and selection state live in the URL (query params), so any
view a person is looking at is shareable and reload-safe.

The shell: left sidebar with navigation (Traces now; Sessions, Stats,
Settings appear in 007), the project name (from `GET /api/v1/projects`,
which answers with exactly one entry for a project key — the picker moves to
007, Decision 13), theme toggle, and the server version (from the
`X-Tracepad-Version` response header the server stamps on every response,
spec 004 #28).

**Traces screen.** A dense table — time, name, environment, user, session,
cost, latency, errors — mapping 1:1 onto `GET /api/v1/traces` rows; a filter
bar exposing exactly the API's filters (`from`/`to`, `environment`, `name`,
`user_id`, `session_id`, `tag`, `status`, `min_cost`); keyset "load more"
pagination via `next_cursor`; the live toggle (Decision 12). Error traces
carry a visible error state, not a color change alone. The empty state on a
fresh install shows a short "point your SDK here" snippet (no secrets — the
key is only known to whoever created it) and links `docs/quickstart.md`.

**Trace detail screen.** Left: the observation tree from
`GET /api/v1/traces/{id}` — type, name, duration, cost, error badge per
node; siblings ordered by `start_time`; error nodes visible when collapsed
(a parent with a failing descendant shows it). Right: the selected node's
detail — metadata fields, usage, model, timings, and `input` / `output` /
`metadata` as JSON viewers. Keyboard: arrows walk the tree, the URL tracks
selection via `?obs=`.

**JSON viewer.** The hand-written lazy tree component (design §8): collapse/
expand, copy value, string previews. It is built *on* the truncation
contract of spec 004: the initial render uses `?expand=io` previews; a
truncation marker (`truncated`, `size`, `preview`, `full`) renders as the
preview plus a "load full (N KB)" affordance that fetches
`GET /api/v1/observations/{id}/io` — the one budget-exempt endpoint — and
swaps it in. The `expansion` refusal object (spec 004 #31) renders as
per-payload load buttons. The viewer never re-implements budgeting; it
consumes the markers as designed.

## Build & repo contract

- `ui/` at the repo root: SvelteKit app, its own `package.json`, npm
  lockfile, `engines`-pinned Node.
- `make ui` → `npm ci && npm run build` → `ui/dist`, copied where the `ui`
  build tag embeds it; `make build` = `make ui` + `go build -tags ui`.
- `make precommit` additions: `svelte-check`, `vitest run`, and the
  openapi-types drift check (Decision 7). Total gate stays ≤ 30 s.
- `make e2e`: builds with UI, boots the binary on a temp database, runs
  Playwright. CI runs it as its own job.
- CI line-count warning over `ui/` (design §8): warns past the budget,
  never fails the build.

## Testing

- **Component (vitest)**: tree rendering incl. deep nesting and error
  propagation to collapsed parents; JSON viewer truncation flow (marker →
  load full → swap, `expansion` refusal → per-payload buttons); filter bar ↔
  URL round-trip; login fragment consumption (key stored, fragment stripped
  from history); 401 → login redirect with stored key cleared; cursor merge
  logic for live mode (no duplicates, no reorder).
- **Contract**: generated types match `openapi.json` (drift check); every
  filter the API accepts appears in the filter bar and vice versa — a parity
  test in the spirit of spec 004 #9.
- **E2E (Playwright, CI)**: boot real binary → land on login → pre-authed
  URL logs in → ingest a synthetic OTLP fixture → trace appears in the list
  → open it → walk the tree → load a full payload → deep-link with `?obs=`
  reloads to the same selection. Dark and light rendering smoke-checked.
- **Accessibility floor**: interactive elements are focusable with visible
  focus states; the tree is keyboard-walkable; text contrast meets 4.5:1 in
  both themes (checked for the token palette, not per-screen).

## Out of scope (007 and later)

- Sessions, Stats (uPlot), Settings screens — spec 007.
- Search/FTS, prompts UI, datasets, annotations — later iterations.
- Server push (SSE/WebSocket) — polling per Decision 12.
