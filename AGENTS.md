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
- ✅ Spec 013 (stats rollup) shipped: schema 0009 — `stats_hourly`, a few
  thousand rows a month that answer what a scan of millions used to, and that
  **outlive the traces they summarize**, so configuring retention no longer
  amputates the charts. A background aggregator beside the sweeper rolls
  closed hours and re-rolls the ones late spans touched; `/api/v1/stats`
  splits the asked range at a per-project watermark and merges the two halves,
  which is invisible because latency is a summable histogram on both sides
  (#2) and the exact-sort path is gone. An hour past the trace-retention
  window is frozen, because recomputing it from rows the sweep deliberately
  took would replace the history with a fragment (#11). The rollup gets a
  window of its own, `stats_retention_days`, null by default.
- ✅ Spec 014 (datasets, runs & score configs) shipped: schema 0010 —
  `datasets` with one version clock over append-only `dataset_items`,
  `dataset_runs`, `score_configs`, and `run_id`/`item_id` on the trace. An eval
  now lives where its evidence does: the harness stamps two attributes on the
  traces it exports, the store links them to the cases they answered, and
  `GET /api/v1/runs/{a}/compare/{b}` answers "did this change make it better"
  once, server-side, for the CLI and the MCP tools alike. A live run's traces
  are the one thing retention spares (#13), which `/system` reports so the
  operator can see the exception's size. Tracepad still executes nothing.
- ✅ Spec 015 (JSON on CodeMirror) shipped: every payload the interface shows
  — an observation's input, output and metadata, and a trace's own metadata —
  goes through one CodeMirror 6 surface that reads *and* writes, replacing the
  hand-written lazy tree everywhere. A prompt is shown whole and wrapped rather
  than cut at 180 characters, `Cmd/Ctrl-F` searches the document rather than
  the part of it that is drawn, a long document opens folded two levels deep,
  and the four syntax colours are `app.css` tokens under the contrast test like
  every other colour. `JsonEditor` ships with tests and no consumer — spec 016
  is its first — because it is the viewer with `readOnly` off (#2). Truncation
  stays the marker's business: `Payload` shows the preview as text under a
  banner that names both sizes and loads the whole payload (#3). The UI line
  budget moves to 14,000 (#9) and the bundle's ceiling is stated at 1.2 MB
  (#10). No server change.
- ✅ Spec 016 (evals in the web interface) shipped: the *Evals* section —
  datasets, runs, score configs — and with it the screens spec 014's nouns had
  been waiting for: a dataset with its items and versions, a run with the
  summary the server computes and the traces that answered each case, and the
  comparison rendered exactly as `GET /runs/{a}/compare/{b}` returns it, with
  no arithmetic of its own. One endpoint was added, `GET /api/v1/runs`
  (schema 0011), because *Runs* is "what ran lately, whatever the set" and
  fanning out per dataset would be logic in a client; the CLI and MCP moved in
  the same PR. The write half follows the same rule — every write is one of
  spec 014's endpoints: the item editor over spec 015's surface (a save
  answers "saved as version V" or "unchanged"), *Add to dataset* on any
  observation, the score-config form, and deletions that wear the echo
  ceremony exactly where the server has a dry run and a named consequence
  where it has none.
- ✅ Spec 017 (the Python package) shipped: `tracepad` on PyPI, source in
  `sdk/python/`, a thin layer over `opentelemetry-sdk` that owns no transport
  and all of the ergonomics — `init` *adapts* to the provider it finds rather
  than replacing it (#2), `@observe` and the three context managers capture
  arguments and return values by default (#4), the generation helper reads an
  OpenAI-compatible response including the cost the provider actually charged
  (#5), scores go through a queue and a thread (#6), prompts through a cache
  that honours the server's `max-age` and serves stale when it is away (#8).
  The mapper gained the `tracepad` dialect — the handful of attributes the
  GenAI conventions have no word for, each ranked beside the `langfuse.*` key
  it mirrors, `langfuse.*` never written (#3) — and with it a golden fixture
  that is not synthetic: `010-tracepad-sdk.pb` is the package's own export.
  Failure semantics are split by path: the tracing half never raises into
  application code, the REST half raises `TracepadError` (#9).
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
| Datasets, runs, score configs | `internal/store/datasets.go` (the version clock, "items at V", the pin's release), `runs.go` (the summary, the item view, the values a comparison needs), `scoreconfigs.go` (the binding by name, checked inside `ScoreWrite.apply`), `internal/server/datasets.go`, `runs.go`, `scoreconfigs.go`, `internal/cli/datasets.go`, `internal/mcpserver/evals.go`, `docs/datasets.md`, spec 014 — the store executes nothing; a trace joins a run through two columns the mapper claims from `tracepad.run_id`/`tracepad.item_id`, `notPinned` in `sweep.go` is the one predicate the sweep and the retention dry run share, and every number a comparison reports is computed server-side so that two clients cannot disagree about what improved means |
| JSON API plumbing (auth, strict decode, pagination) | `internal/server/api.go`, spec 003 |
| Read API (traces, sessions, stats, system) | `internal/server/traces.go` and neighbours, `docs/api.md`, specs 004 and 009 — the route table in `routes.go` is the surface, and `openapi.json` must agree with it. Paging is keyset in both directions: `trimPage` in `api.go` owns which cursor a page may claim |
| Response budgets and truncation | `internal/server/budget.go`, spec 004 #2 |
| CLI | `internal/cli/`, `internal/client/`, `docs/cli.md`, spec 004 |
| MCP | `internal/mcpserver/`, `docs/mcp.md`, spec 004 — tools call the read API over HTTP, never the store, and only ever with a GET (spec 005 #13) |
| Search | `internal/store/search.go` (the query language, what of a field is indexed, and the snippet) and `searchindex.go` (the index's lifetime), `docs/api.md#search`, spec 011 — the user's text never reaches `MATCH` as written, `searchableField` is what both the index and the snippet see of a payload, and every path that deletes observations deletes their entries in the same transaction |
| Retention and the sweeper | `internal/store/sweep.go`, `docs/retention.md`, spec 005 — every chunk is a `WriteJob`, never a second write connection |
| Statistics rollup | `internal/store/rollup.go` (the table and one hour's recomputation), `aggregator.go` (the pass, the watermark, the freeze), `histogram.go` (why a percentile is summable), the seam in `internal/server/stats.go`, spec 013 — an hour is recomputed whole and never delta-maintained, and the rollup is the one store the trace sweep spares |
| Admin API (projects, keys, retention, erasure) | `internal/server/admin.go`, `internal/store/admin.go`, `docs/admin.md`, spec 005 — destructive endpoints are a dry run until `?confirm=` echoes the name, checked inside the write transaction |
| Attribute mapping | `internal/mapping/rules.go` is the table; `mapping.go` applies it; `value.go` holds `attrs`, where reading and claiming are separate and an unclaimed attribute keeps the origin it arrived at (spec 012 #7) |
| Web interface | `ui/` (SvelteKit SPA), `internal/ui/` (the embed and the tagless stub), `internal/server/ui.go` (delivery and the SPA fallback), `docs/ui.md`, specs 006 to 010 and 015 — the API types in `ui/src/lib/api/schema.d.ts` are generated from `openapi.json` and the gate fails on drift, and the application-line budget is 14,000 (`scripts/ui-lines.sh`, spec 015 #9) |
| A payload, shown or edited | `ui/src/lib/components/json/` — `setup.ts` is everything that is not a DOM node (the document a value becomes, where it stops being JSON, which nodes a long one folds, the extension list and the themed chrome), `CodeArea.svelte` is the instance, `JsonView`/`JsonEditor` are the two modes, spec 015 — one surface for reading and writing, so there is one answer to "what does this payload look like"; the mode is the `readOnly` facet and nothing else, `indentWithTab` is deliberately absent (#7), truncation belongs to `Payload.svelte` rather than to the editor (#3), and an editor over a field that may be absent takes `optional`, where an empty document is valid and unmarked (spec 016 #22) |
| A listing (rows, cursors, count, the bar, the panel's walk) | `ui/src/lib/listing.svelte.ts` and its tests, spec 010 — all three listings are one loader, so a listing defect is one defect. `$lib/page` and `$lib/peek` hold the pure part; a listing read oldest first (a dataset's items, a run's) sets `ascending` on its walk (spec 016 #19) |
| The Evals screens (datasets, runs, the comparison) | `ui/src/routes/{datasets,runs,score-configs}/`, `ui/src/lib/components/evals/` (the tables, the three peek bodies, the summary cards), `ui/src/lib/evals.ts` (the pure part: the checkbox rule, the *changed only* filter, the words a cell uses), `ui/src/lib/api/runs.ts` (the run filters, held to `openapi.json`), `docs/ui.md#evals`, spec 016 — every number on these screens is the server's (spec 014 #18); the client decides which rows to draw and never what a verdict is |
| Writing an eval (the item editor, the forms, the deletions) | `ui/src/lib/components/evals/ItemEditor.svelte` over the routes `datasets/items/new` and `datasets/[name]/items/[id]/edit` (spec 016 #21), `ScoreConfigDialog.svelte` with `ui/src/lib/api/score-configs.ts` (the vocabularies and the rules, held to `openapi.json`), `NewDatasetDialog`/`DeleteDatasetDialog`, `ui/src/lib/components/ConfirmDialog.svelte`, `itemBody`/`savedMessage` in `$lib/evals`, `docs/datasets.md#the-same-loop-from-the-web-interface` — a write is one of spec 014's endpoints and never a verb of the screen's own; the echo ceremony (`ConfirmCard`) is only where the server has a dry run, and the dialog is where it does not (#6) |
| The Python package | `sdk/python/` (`src/tracepad/` is the package, `tests/` its suite and `tests/e2e/` the run against a real binary), `docs/sdk-python.md`, spec 017 — two dependencies and no third, no provider-client wrapper ever (design §6.5); `_tracing.py` holds the provider adaptation and defers the SDK's own imports into `init`, `_attributes.py` is the vocabulary that `internal/mapping/rules.go` reads back, and the application-line budget is 1,500 shared with spec 018 (`scripts/sdk-lines.sh`). `scripts/fixtures/tracepad_sdk.py` rewrites `testdata/otlp/010-tracepad-sdk.pb` from the package's own exporter |
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
- `make smoke` — export from pinned real SDKs and from our own package into a
  real binary and assert the rows. Needs network on first run (installs them).
- `make sdk-test` — the Python package's unit suite, then its end-to-end suite
  against a binary it builds. `uv` if present, `venv` otherwise;
  `SDK_SKIP_E2E=1` runs the unit half alone. `make sdk-lines` reports its
  budget.
- `make fixtures` — regenerate `testdata/otlp/*.pb` and their goldens after a
  deliberate mapping change. Review the golden diff; it *is* the change. The
  one body that is not synthetic (`010-tracepad-sdk.pb`) is rewritten from the
  package first, which needs `uv`; without it that body stands as committed.

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
