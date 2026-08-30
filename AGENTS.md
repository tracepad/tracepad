# Tracepad — agent guide

Lightweight, self-hosted, OTLP-native store and viewer for LLM/agent traces.
Single Go binary, embedded SQLite, UI/CLI/MCP as thin clients over one read
API. This file routes; it does not duplicate what specs and docs say.

## Current status

- ✅ Spec 001 (server skeleton) shipped: config, self-migrating SQLite store,
  project/key bootstrap, `/health`, CI gate, GoReleaser config. Go 1.27,
  `modernc.org/sqlite`.
- ✅ Spec 002 (trace model + OTLP ingest) shipped: `POST /v1/traces` and the
  Langfuse alias, schema 0002, attribute mapping with two dialects,
  group-commit writer, raw body storage.
- ✅ Spec 003 (scores & prompts) shipped: `/api/v1/scores` and
  `/api/v1/prompts`, schema 0003, versioned prompts with movable labels, the
  group-commit writer generalized to carry every durable write.
- ✅ Spec 004 (read API, CLI, MCP) shipped: the read API with response
  budgets and truncation markers, self-description and OpenAPI, schema 0004,
  a CLI in the same binary, and an MCP server on protocol 2026-07-28. Both
  clients are HTTP clients of the read API and contain no logic of their own.
- ✅ Spec 006 (UI core) shipped: a SvelteKit SPA in `ui/`, embedded behind the
  `ui` build tag and served from `/`, with key-based auth and a pre-authed
  first-run URL, the Traces list and the Trace detail view. It is a client of
  the read API — no endpoint was added for it. Node is a dev prerequisite of
  the interface only; the Go suite and a tagless build never need it.
- ✅ Spec 007 (sessions, stats, settings) shipped: `GET /api/v1/sessions` —
  the first growth of the read API since 004, landed in the API, the CLI
  (`sessions ls`) and MCP (`list_sessions`) in the same PR as the screen that
  needed it — plus the Sessions, Stats and Settings screens, a shared time
  range control, and the admin token as a second UI credential that never
  touches the data plane.
- ✅ Spec 009 (listing pagination) shipped: the read API pages both ways
  (`direction=prev` beside the opaque cursor, `prev_cursor` beside
  `next_cursor`) and counts on request, capped at 1000 inside a `LIMIT`ed
  subquery. With no cursor a direction *is* an end, so « ‹ › » cost what one
  page costs and no offset appears anywhere. "Load more" becomes a bar with a
  page size, and the page joins the filters in the URL. CLI (`--oldest`,
  `--total`) and MCP moved in the same PR.
- ✅ Spec 008 (peek panel) shipped: a row on any listing opens in a panel
  over it rather than navigating away — `?peek=` in the URL, the full-page
  routes kept as the canonical link, and the trace and session detail bodies
  extracted so the page and the panel render the same component. `j`/`k`
  walk the rows; a session panel drills one level into a trace. No server
  change: it is a second arrangement of the same two GETs.
- ✅ Spec 010 (shared listing) shipped: the page in force, the rows, both
  cursors, the capped count and the effects that keep them true live once, in
  `ui/src/lib/listing.svelte.ts`, under all three listings — Traces, Sessions
  and a session's own trace table — with the panel's walk as a second layer
  over the two that have one. No behaviour changed; the E2E suites are the
  freeze's evidence. The interface's line budget now counts the application
  and reports its tests beside it (#7).
- ✅ Spec 011 (search) shipped: schema 0006 — a contentless FTS5 index over
  observation input, output, metadata, name and status message, plus the trace
  name — written in the ingest transaction and deleted by every path that
  deletes observations, with a one-off backfill on the first start after the
  upgrade. `q=` is one more filter on the trace listing, not a new endpoint and
  not a new ordering, and every matching row carries `match`: where the hit was
  and a snippet of the text. The CLI grew `--search`, MCP grew the `search`
  tool spec 004 #17 was waiting for an endpoint to justify, and the Traces
  screen grew a search box, the snippet under the row and a click that lands on
  the observation that matched. Schema 0007 (Decision 14) rebuilt that index
  over the **scalar leaves** of a payload's JSON rather than its text: the keys
  and brackets are not words, and the snippet reads as a sentence.
- ✅ Spec 012 (wire columns) shipped: schema 0008 — the observation type
  widened to the ten-value Langfuse vocabulary, `completion_start_time`,
  `prompt_name`/`prompt_version`, and `release`/`version`/`ttft_ms` on the
  trace. What the SDKs already send stops landing in `metadata` where nothing
  could filter on it: four filters (`release`, `version`, `type`, `prompt`),
  a TTFT column, `group_by=release`, and payload sizes read from the payload
  table rather than stored twice. Everything an unclaimed attribute keeps now
  says where it came from — `resource.<key>`, `scope.<key>`, bare for a span
  attribute — so a resource `service.name` and a span one stop overwriting
  each other. No backfill: the release is deferred, so the migration is free
  (#1).
- ✅ Spec 005 (retention & admin) shipped: schema 0005, the hourly sweeper
  writing every chunk through the group-commit writer, the admin API under
  `/api/v1/projects` with a dry-run/confirm contract on every destructive
  endpoint, and the `projects`/`keys`/`retention`/`users` CLI. MCP unchanged,
  by design.

## Where things are

| Working on | Read first |
|---|---|
| Any feature | Its spec in `specs/` (spec-first — see Process below) |
| Storage, schema, migrations | `internal/store/`, spec 001 |
| Write pipeline (group commit) | `internal/store/writer.go`, spec 002 #15, spec 003 #9 — every durable write is a `WriteJob` |
| HTTP surface | `internal/server/` |
| OTLP ingest | `internal/server/otlp.go`, `docs/ingest.md`, spec 002 |
| Scores & prompts | `internal/server/scores.go`, `prompts.go`, `docs/scores.md`, `docs/prompts.md`, spec 003 |
| JSON API plumbing (auth, strict decode, pagination) | `internal/server/api.go`, spec 003 |
| Read API (traces, sessions, stats, system) | `internal/server/traces.go` and neighbours, `docs/api.md`, specs 004 and 009 — the route table in `routes.go` is the surface, and `openapi.json` must agree with it. Paging is keyset in both directions: `trimPage` in `api.go` owns which cursor a page may claim |
| Response budgets and truncation | `internal/server/budget.go`, spec 004 #2 |
| CLI | `internal/cli/`, `internal/client/`, `docs/cli.md`, spec 004 |
| MCP | `internal/mcpserver/`, `docs/mcp.md`, spec 004 — tools call the read API over HTTP, never the store, and only ever with a GET (spec 005 #13) |
| Search | `internal/store/search.go` (the query language, what of a field is indexed, and the snippet) and `searchindex.go` (the index's lifetime), `docs/api.md#search`, spec 011 — the user's text never reaches `MATCH` as written, `searchableField` is what both the index and the snippet see of a payload, and every path that deletes observations deletes their entries in the same transaction |
| Retention and the sweeper | `internal/store/sweep.go`, `docs/retention.md`, spec 005 — every chunk is a `WriteJob`, never a second write connection |
| Admin API (projects, keys, retention, erasure) | `internal/server/admin.go`, `internal/store/admin.go`, `docs/admin.md`, spec 005 — destructive endpoints are a dry run until `?confirm=` echoes the name, checked inside the write transaction |
| Attribute mapping | `internal/mapping/rules.go` is the table; `mapping.go` applies it; `value.go` holds `attrs`, where reading and claiming are separate and an unclaimed attribute keeps the origin it arrived at (spec 012 #7) |
| Web interface | `ui/` (SvelteKit SPA), `internal/ui/` (the embed and the tagless stub), `internal/server/ui.go` (delivery and the SPA fallback), `docs/ui.md`, specs 006 to 010 — the API types in `ui/src/lib/api/schema.d.ts` are generated from `openapi.json` and the gate fails on drift |
| A listing (rows, cursors, count, the bar, the panel's walk) | `ui/src/lib/listing.svelte.ts` and its tests, spec 010 — all three listings are one loader, so a listing defect is one defect. `$lib/page` and `$lib/peek` hold the pure part |
| Configuration | `internal/config/`, spec 001 + spec 002 Configuration tables |

Attribute semantics for the `langfuse.*` dialect are derived from Langfuse
(MIT) — see `NOTICE`. Keep new rules in the table in `rules.go`, with the
reason in a comment; adding a dialect should be a table edit.

## Commands

- `make precommit` — full gate (format-check + vet + Go tests + `svelte-check`,
  vitest and the API-type drift check). Budget: under 30 seconds. The gate
  self-installs as the git pre-commit hook on first run (and on Claude Code
  session start); `make install-hooks` force-reinstalls it.
- `make dev` — run the server, output mirrored to `.dev.log` (read that file
  first when debugging a running server). `npm run dev` inside `ui/` serves
  the interface with hot reload against it.
- `make build` — binary with the web interface into `./bin`;
  `make build-server` builds without it and needs no Node.
- `make e2e` — boot the real binary on a temp database and run the Playwright
  smoke. Its own CI job, never part of the gate.
- `make smoke` — export from pinned real SDKs into a real binary and assert
  the rows. Needs network on first run (installs the SDKs).
- `make fixtures` — regenerate `testdata/otlp/*.pb` and their goldens after a
  deliberate mapping change. Review the golden diff; it *is* the change.

## Process

- **Spec-first.** A feature is implemented against a spec in `specs/`
  (`NNN-name.md`). Deviating from the spec during implementation means adding
  a dated entry to that spec's Decisions log — not silently diverging, and
  not rewriting history.
- **The spec outranks code and tests.** A red test is a question, not a
  command: do not "green" code against a stale test. Intentional behavior
  changes update the test *and* the Decisions log.
- **Docs ship in the same PR** as the behavior they describe.
- **Trunk-based PR flow**: short-lived branch → PR → squash-merge. PR titles
  follow Conventional Commits (they become the commit history).
- Stage git changes with explicit paths (never `git add -A`); review
  `git diff --cached --name-only` before committing.
