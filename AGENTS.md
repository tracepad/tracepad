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
- ✅ Spec 018 (the eval harness in Python) shipped: the loop
  `docs/datasets.md` prints in forty lines of shell, in ten of Python — a
  `Dataset` that pages its items on its own, a `Run` that pins a version and
  closes itself (`finished`, or `failed` with the exception's `repr`), and
  `with run.item(case):`, inside which every span the *application* starts
  carries `tracepad.run_id` and `tracepad.item_id` because a `SpanProcessor`
  writes them at `on_start` (#3). The block opens no span of its own: the root
  is the framework's, another SDK's or a decorator's, and it is stamped all
  the same — as far as `contextvars` reaches, which is an `await` and a copied
  context but not a bare thread or another process (`Attempt.attributes()` is
  for those). `finish` flushes the scores and then the spans before it posts,
  so the summary on the next line is over everything the run produced (#5).
  Nothing is computed here: every number is the server's (#8).
- ✅ Spec 019 (the way out) shipped: the archive finally has a reader other
  than the sweeper. Schema 0012 adds `raw_batches.content_type` (NULL reads as
  protobuf, so nothing is backfilled); `GET /api/v1/raw` pages it **oldest
  first** — the one listing in the API whose natural order is forward, because
  a replay must preserve the order the world produced the spans in — and
  `GET /api/v1/raw/{id}` answers one body in the type it arrived in,
  budget-exempt like `/observations/{id}/io`. `tracepad export --otlp` is their
  first client: `--to <url>` posts each body to an OTLP receiver with retry on
  `429`/`5xx` and a stop-with-a-cursor on anything else `4xx`, `--dir <path>`
  writes `<received_at_ms>-<id>.pb|.json` beside `manifest.jsonl`. It
  synthesizes nothing — a trace older than the raw window is parsed rows only,
  and `traces_before_window` in `/system`'s new `raw` block is the honest edge
  of the promise. `POST /v1/traces` also learned the OTLP/JSON encoding (hex
  ids, not protobuf-JSON's base64), through the same mapper, archived as it
  arrived; the whole fixture corpus is replayed through that door and diffed
  against the protobuf rows.
- ✅ Spec 020 (packaging) shipped: the docs stop describing something that is
  not there. A multi-stage `Dockerfile` at the root builds the interface and
  the binary from a checkout and ships them on distroless static as uid 65532
  with `/data` as its volume — self-contained on purpose (#1), so a fork or an
  operator with a patch builds the artifact the docs describe rather than one
  only a release pipeline can produce. `release-server.yml` splits a version
  tag between GoReleaser (archives and checksums on GitHub Releases) and
  `buildx` (one multi-arch manifest on `ghcr.io/tracepad/tracepad`, with `X.Y`
  and `latest` moving only for a full release). `tracepad health` is the new
  CLI command and the image's `HEALTHCHECK`: distroless has no shell to write a
  probe in, so the binary is the probe, and `/health` needs no key so neither
  does it. The `docker` CI job builds and *boots* the image on every push,
  which is the seam nothing else covers — the Go suite calls `cli.Run`
  directly, so only a running container proves the real binary dispatches a
  command at all.
- ✅ Spec 021 (prompts in the web interface) shipped: the *Prompts* section —
  a listing, a prompt page with its versions and the body of the one being
  read, the server's diff between any two painted per line, an editor that
  appends a version, and a label control on which promoting and rolling back
  are the one-line operation spec 003 designed them to be. Top-level in the
  sidebar rather than under *Evals* (#1): a prompt is what the application
  ships, not an eval noun. One write was missing from the API and is added
  here — `DELETE /api/v1/prompts/{name}`, a name whole, dry run until
  `?confirm=` echoes it (#7) — with `tracepad prompts rm` and `prompts label`
  beside it, because the interface may do nothing the CLI cannot (spec 004 #1).
  Traces that ran a deleted prompt keep the columns they recorded: those are a
  string pair the client sent (spec 012), not a reference into the table, and
  the filter goes on answering. Three amendments the screens paid for: both
  prompt listings page **both ways** (#11), the version listing answers with
  the name's whole `labels` map (#12), and the API client fetches
  `no-store` (#13) — the prompt reads are the only ones carrying
  `Cache-Control`, and a screen that had just moved a label was re-reading the
  minute-old answer.
- ✅ Spec 022 (scores on screen) shipped: the judgements have been in the API
  since spec 003 and on the eval screens since spec 016, and were invisible on
  the screens where the graded thing itself is read. A *Scores* block now sits
  on the trace header, the observation panel and the session header — one chip
  per score, the value rendered by the type its config pins, the source it
  claims, the comment on expand — off **one** read per trace, split between the
  header and the panels by a field and counted into the tree's badge, because
  splitting a loaded document is rendering and not the client logic spec 004 #1
  forbids (#1, #8). The write half is the one a reviewer needs: *Score* is a
  dialog that builds its control from the name's config, *Edit* is spec 003's
  correction — the same POST carrying the row's id — and *Delete* is the
  endpoint retraction was missing, `DELETE /api/v1/scores/{id}`, with no dry run
  and no echo because a re-POST puts the row back (#6). `tracepad scores add`
  and `scores rm` land in the same PR, since the interface may do nothing the
  CLI cannot (#7).
- ✅ Spec 023 (users) shipped: schema 0013 — `users_hourly`, spec 013's tuple
  plus `user_id`, and a `users` summary row per account — rolled by the *same*
  aggregator pass, in the same `(project, hour)` job, under the same watermark
  and the same freeze (#2). A user id rode on most traces and the store could
  filter by it, erase by it and nothing else; now "who are my heaviest users",
  "when did this one last show up" and "what does this account cost me" are
  index seeks. `sessions_started` is counted where a session *begins*, because
  a start sums exactly over a range and a distinct count does not (#1, #12),
  which costs the dirty set one addition: a changed trace with a session id
  also dirties the hours the rest of that session sits in. `GET /api/v1/users`
  answers from the rollup alone and trails it by the published lag; `GET
  /api/v1/users/{id}` merges the live tail and is exact for any id (#4).
  `/stats` gains `user_id`, and with it `sessions` per bucket on a timeline
  (#6). Erasure deletes the per-user rows outright rather than re-rolling them
  — they are *about* the user, and a frozen hour could not recompute them at
  all (#10). The *Users* screens, `users ls`/`show`, `stats --user`,
  `list_users`/`get_user`; the interface's line ceiling rises to 16,000 (#11).
  Later: each erase chunk re-rolls the hours it emptied **inside its own
  transaction** (#19) — the handler used to submit the rolls after the last
  chunk, so a client that hung up between chunks (spec 010 #10's clock) left
  the emptied hours counting traces that were gone, and a repeat could not
  find them; the same conclusion spec 025 #22 reached for the score rollup.
- ✅ Spec 024 (annotation queues) shipped: schema 0014 —
  `annotation_queues` and `annotation_items`, the list of what a team decided
  deserves a human verdict and the bookkeeping of who gave one. The verdicts
  are *scores*, unchanged: a queue names the score configs a reviewer must
  fill, and `complete` is the server checking those scores are on the item's
  target, whoever wrote them (#7) — so a judge's verdict prefills the desk and
  is confirmed rather than repeated. Two ways in, both idempotent (#4): one
  target, or every trace a *listing filter* matches, capped — the filters are
  `traces.go`'s own, so "queue what I am looking at" needs no second grammar.
  `next` is a GET that writes: being handed an item claims it for ten minutes,
  resume-first so a reload does not move a reviewer mid-verdict (#5). Items
  follow their trace through the sweep, the erasure and the purge (#3), and
  deleting a queue takes its items and no scores. The *Queues* screens, the
  desk, both *Add to queue* gestures, `queues …`, `list_queues` /
  `get_queue_items`; the interface's ceiling rises to 18,000 (#17).
- ✅ Spec 025 (quality trends) shipped: schema 0015 — `scores_hourly`, spec
  013's tuple plus the score's name, data type and category, rolled by the
  same pass in the same transaction as the other two tables. A score is
  counted in **the hour of the trace it grades** (#1), which is what makes a
  quality curve line up with the traffic and cost curves; a score that names
  only a session, and a `text` score, are not on a timeline at all. Four
  numbers per row — count, sum, min, max — so a day is the exact merge of its
  24 hours and a mean of means cannot be computed (#2). The dirty set grows by
  one question, because a score's arrival touches no trace row: `scores
  .created_at > last_pass` over a new `idx_scores_created` (#3), and a
  *deletion* re-rolls its hour in the request, since a row that is gone cannot
  be found by when it arrived (#4). `GET /api/v1/stats/scores` answers through
  spec 013's seam with `targets` saying what a model grouping can count;
  `scores trend`, `get_score_trends`, and the *Quality* screen as the fifth
  child of *Evals* (#9–#12). Two corrections from the first review: a re-POST
  that *moves* a score re-rolls the hour it left, the way a deletion does
  (#20), and the freeze is asked of each table the job writes rather than once
  for all of them (#21) — so a backfill fills an hour past the window whose
  traces are intact, and still cannot demolish one whose rows already stand.
  Four more from the second: that re-roll happens **inside the write's own
  transaction** and touches only `scores_hourly`, where the handler used to
  submit the whole `RollHour` afterwards — an hour of user summaries inside a
  single `DELETE`, and a `500` over a write already on disk (#22);
  `created_at` is stamped in the transaction, because `commitMargin` was
  sized for a stamp taken there and a handler-side one could fall behind the
  next pass's cutoff for ever (#23); the answer is bounded on both sides —
  `limit`/`omitted` over series, the twenty busiest categories and an `other`
  key inside one (#24); and `scores trend` prints three significant digits
  without an exponent, as the screen does (#25).
- ✅ Spec 026 (revision: the listing shell) shipped: no feature. The `{#if}`
  ladder eleven listings each wrote out — the failure line, the table, the
  bar, the page a stale cursor found empty, the empty state — is
  `ListingShell`, with the table and the empty state as snippets and the
  three real differences as props (#1, #3); the header's count is
  `ListingCount` (#2) and a trace panel's five spans are `TracePeekMeta`
  (#4). `PageHeader` wraps at a phone's width — and only there — instead of
  squeezing the meta to an ellipsis, with the wrap one level in so the
  actions cannot be the thing that moves (#5, #9, #10, #14).
  `scripts/doc-anchors.sh` reads the
  hundred cross-references nothing read before, in the gate and against its
  own fixture (#6, #13). `users_hourly` is the third table to ask the freeze
  of its own rows, which closes spec 023 #15 (#7). Measured rather than
  promised: the application half is eighteen lines smaller and the tests are
  264 larger (#8, #12) — the win is that a listing defect now has one place
  to be, not the budget.
- ✅ Spec 027 (facets) shipped: `environment`, `release` and `name` take a
  comma-separated list on every endpoint that takes them as a trace filter, and
  a trace matches when its column equals **any** item (#1) — one parameter reads
  as one filter in a URL, a shell and a chip, which is where these values are
  typed and shown. `tag` is untouched, because its repeatable form is an AND on
  purpose. `GET /api/v1/facets` answers what those three can be set to over a
  range: the distinct values with their trace counts, busiest first, a hundred
  per column with `omitted` for the rest (#2). It rides spec 013's seam a fourth
  time — schema 0016's `names_hourly` and `stats_hourly`'s own trace-unit rows
  behind the watermark, the live scan past it (#3) — so a value first seen a
  minute ago is on the list *now*, which is what lets the filter panel drop
  free-text entry altogether. The counts ignore the other filters (#4), because
  filter-aware counts mean a refetch per checkbox and the "why did production
  vanish" confusion every faceted search has to explain. CLI `facets`, MCP
  `get_facets` (#5), and in the panel the three fields become checkbox lists
  with counts, loaded when it opens (#6–#8). The ceiling rose to 18,500 for a
  component whose lines *are* a checkbox list (#9).
- ✅ Spec 033 (the Go package) shipped: a nested module `sdk/go`
  (`github.com/tracepad/tracepad/sdk/go`), thin over the OTel Go SDK with
  the Python package's vocabulary in the shape of the OTel API — `Init` that
  adapts to the provider it finds and hands back its `shutdown`, `Span`,
  `Event`, `Generation` with an explicit `Result` and no response reader
  (#5), `Update`, `UpdateTrace`, `Score` over a batching goroutine, `Prompt`
  with the `max-age` cache, and the eval harness as a context (#10). Go has
  one namespace for a function and a type, so the handles are `*Call` and
  `*PromptVersion` (#14); the floor is Go 1.25, where the OTel SDK puts it
  (#15). Fixture `014-tracepad-sdk-go.pb` is the package's own export,
  deterministic byte for byte (#13); the `sdk-go` CI job runs the module on
  the two newest Go lines, and `make gate` includes its unit suite because
  the root `go test ./...` cannot see a nested module (#11).
- ✅ Spec 031 (tokens in the statistics, streams in the SDK, where a price
  comes from) shipped: `stats_hourly` carries three token sums per cell —
  input, output, cache read — each read off an observation's `usage` under
  the first key present of one closed list per class (`tokenClasses` in
  `rollup.go`, #1), summed into the model cell and the trace-unit cell of the
  observation's trace (#2), nullable like `total_cost` (#3, migration 0018
  with the `last_pass = 0` backfill). `GET /api/v1/stats` buckets carry
  `tokens: {input?, output?, cache_read?}`, absent when nothing carried a
  count (#4), the live scan sums the trace unit with one aggregate over the
  same join (#5), and never on a `user_id` answer, whose rollup holds none
  (#11); a span with usage and no model is not a call to a model and counts
  in neither cell (#12). The Stats screen gets a Tokens chart with three
  lines, `N tokens` in the header and a Tokens column in the three
  breakdowns — input plus output, cache read on the chart only (#6) — and
  `tracepad stats` a TOKENS column (#13). The Python package gains
  `call.stream(chunks)` / `astream`: a pass-through over a streamed answer
  that stamps the first token, gathers the output, reads the usage and cost
  from the chunk that carries them and ends the generation when the stream
  does (#7, #8, #21). `docs/ingest.md` gains *Where the price comes from*
  (#9); fetching a price from OpenRouter after the fact is recorded as not
  built (#10).
- ✅ Spec 030 (bare usage keys) shipped: `mapUsage` gains a third source —
  ten **bare** token spellings (`input_tokens`, `cache_read_tokens`, …) read
  when neither `langfuse.observation.usage_details` nor any `gen_ai.usage.*`
  count is on the span (#1), each stored under the key as sent. The three
  sources are a chain and never a merge: an exporter sending both spellings is
  describing one number twice, and the standard one wins whole. The list is a
  closed constant in `rules.go` and a row in `docs/ingest.md`'s table (#2),
  because a bare word is exactly the key that collides with somebody's
  unrelated attribute. What it unlocks is Claude Code, whose four counts sat in
  metadata until now: `docs/ingest.md` gains the section that turns "it happens
  to work" into a supported path — the six variables as a shell block and as
  `~/.claude/settings.json`'s `env`, the protocol line that is not optional
  because gRPC is not served, and what does and does not arrive (no prompt
  text; Claude Code redacts it before exporting) (#3). Fixture
  `012-claude-code-interaction` is synthetic to the shape a live session
  emits — a live one carries the account's email and organization id (#4).
- ✅ Spec 029 (the project in the URL, the switcher) shipped: every screen
  inside the shell lives under `/p/{project id}` (#1), and the project on
  screen is derived from that route parameter rather than chosen from
  `me.projects` (#2) — one helper, `href`, writes every link under it. Bare
  paths redirect to the remembered project (#3), an unreachable id and an
  account with no projects each get a screen rather than `403`s (#4), the
  sidebar's name became a switcher with each project's traces of the last day
  (#5), a switch keeps the section and the filters and drops the answers (#6),
  *New project* sits in the menu for owners over the dialog the Server tab
  shares (#7), and `GET /api/v1/projects?activity=24h` counts through the
  stats seam (#8). The ceiling rose to 20,500 (#9). The Account tab is the
  one screen inside the shell that lives bare, `/settings/account` (#14).
- ✅ Spec 034 (the dashboard) shipped: the Stats screen is the project's
  front page — `/p/{id}/dashboard`, first in the sidebar, the target of `/`,
  `/p/{id}` and the switcher, with `/stats` redirecting (#1). A summary row
  of four tiles with the change against the previous window of the same
  length (#2) reads `GET /api/v1/stats?group_by=total`, one bucket with the
  percentiles merged over the window's histograms, which the CLI and MCP take
  through the same enum (#3). The header says when the last trace arrived,
  from the listing with no window (#4); a Quality block draws the six
  busiest score cards (#5); a fresh project sees the exporter snippet on the
  dashboard and Traces keeps a one-line pointer (#6). The time window is
  remembered per account in `localStorage`, a preset as the preset (#7).
  Customize hides and reorders the ten blocks with `svelte-dnd-action` and
  its keyboard path (#8); the arrangement lives in `accounts.preferences`
  (schema 0019), an opaque object under 16 KiB replaced whole by `PATCH
  /auth/me` (#9, #10). The ceiling rose to 21,200 (#11); the login without
  `next`, the setup and invitation landings, a project just made and the
  error page's way out all open the dashboard (#12).
- ✅ Spec 035 (delete traces) shipped: `DELETE /api/v1/traces/{id}` and
  `DELETE /api/v1/traces?<filters>&to=`, editor's routes, each a dry run
  until `confirm` echoes the trace id or the project name (#1, #2). The
  store job `TraceDelete` takes a set of ids the way `UserDataErase` takes a
  user's, and erasure is refactored onto the shared body `traceRemoval`
  rather than copied (#3); the bulk form deletes one round of at most
  `limit` traces per request, in chunks of one hour, and says `more` (#4),
  and at most fifty chunks (#14), recounting nothing (#5); a run's pin is
  overridden and the preview names the run (#6). Migration 0020 adds four
  partial indexes on the columns referencing `payloads(id)`: deleting a
  payload is a foreign-key check in each, and each was a scan (#13) —
  erasure and the sweep gain the same. The ceiling rose to 21,500 (#17). CLI `traces rm <id>` and `traces rm --to … [filters]`
  through the one `destructive` ceremony, the bulk form looping rounds; no
  MCP tool (#7). *Delete…* on the trace header with the id prefilled as the
  echo (#8) and on the listing with the filter's chips, `to` pinned to the
  moment the dialog opened, a progress line and *Stop* (#9). The ingest
  race is documented, not fought (#10).
- ✅ Spec 037 (the agent skill) shipped: one skill, `tracepad`, in
  `agent/skills/tracepad/` — `SKILL.md` under 200 lines and four references
  under 900 in all (#1) — that teaches the order of work and restates no
  flag, route or field, pointing at `tracepad help`, `GET /api/v1` and the
  MCP tool list instead (#2); its one rule of policy is that a destructive
  command stops at the dry run until the human confirms (#3). Embedded by
  `agent/skills/embed.go` and installed by `tracepad skills install
  [--project | --dir DIR] [--force]`, which replaces the directory whole,
  stamps the binary's version into the frontmatter and a `.version` marker,
  and refuses a directory without the marker (#5–#7); `skills show` prints
  it. A drift test asks the binary for its commands, flags, tools and routes
  and fails on any name in the skill the binary does not have (#8).
- ✅ Spec 039 (tracing off) shipped: a process that never called `init` has
  tracing off, and the packages now know it — `score` with no target there
  is dropped with a debug line instead of raising (an initialised process
  still raises `ValueError` / throws / returns `ErrNoTrace` outside every
  span), a `score` by id is sent as ever (#1, #2), and an observation's ids
  with no trace behind them are `None` / `undefined` / `""` rather than
  zeros (#3). Node's `Logger` gained an optional `debug` (#5).
- ✅ Spec 038 (SDK parity) shipped: Python's `span(type=…)` and Node's
  `span(name, { type })` take the step's kind when it opens, as Go's
  `WithType` did (#1) — `type` on `span` only, so Node's `event` takes the
  new `ObservationOptions` (#6); Python's `generation(metadata=…)` (#2); and a
  trace's own version from all three — `update_trace(version=)`,
  `updateTrace({ version })`, `WithTraceVersion` — as `tracepad.trace.version`,
  which the mapper ranks beside `langfuse.version` (#3, #4). An unknown kind
  warns through one helper per package, `observe` included (#5).
- ✅ Spec 032 (the Node package) shipped: `tracepad` on npm, source in
  `sdk/js/`, the Python package's surface with promises where Python has
  context managers and the same vocabulary on the wire — `init` adapts to
  the provider it finds and, where the 2.x SDK has no hook to add to, hands
  over `spanProcessor()` for the constructor (#2); `observe` is a wrapper,
  never a decorator, with the arguments as a positional array (#4); `span`,
  `event` and `generation` are callback-scoped, with `stream()` as an async
  iterable that ends the generation on exhaustion and leaves an early exit to
  the callback (#5); scores ride an `unref`'d timer and a `beforeExit` flush
  (#6); `prompt` is a promise (#7). Fixture `013-tracepad-sdk-js.pb` is the
  package's own export (#12, #13), and `@opentelemetry/resources` is the
  fourth runtime dependency (#14). The eval harness is spec 018 with
  promises (#10): `run.item(item, fn)` runs the callback in a context the
  package's own processor reads at `onStart`, a `Run` closes through
  `wrap` or `await using`, and the read side is the server's JSON as plain
  objects.
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
| Scores & prompts | `internal/server/scores.go`, `prompts.go`, `docs/scores.md`, `docs/prompts.md`, spec 003 — a version is never edited and never deleted alone; `PromptDelete` in `internal/store/prompts.go` takes a name whole, and the echo is checked inside its transaction (spec 021 #7). A score is the opposite: `ScoreDelete` takes one row with no echo at all, because a re-POST with the same id puts it back (spec 022 #6) |
| The Scores block (the chips, the dialog, the tree badge) | `ui/src/lib/components/scores/` (the block on all three surfaces, the create/edit dialog), `ui/src/lib/scores.ts` (the pure part: the value per type, the source, the split of one response between the header and the observation panels, the form and the body it posts), `ui/src/lib/scores.svelte.ts` (the one read a trace or a session makes), `docs/ui.md#scores`, spec 022 — the trace reads its scores **once** and the split between the header, the panels and the tree's badge is rendering (#1); a new score is stamped `source: "web"` and an edit resends the row's own `metadata` and `timestamp` (#10) |
| Datasets, runs, score configs | `internal/store/datasets.go` (the version clock, "items at V", the pin's release), `runs.go` (the summary, the item view, the values a comparison needs), `scoreconfigs.go` (the binding by name, checked inside `ScoreWrite.apply`), `internal/server/datasets.go`, `runs.go`, `scoreconfigs.go`, `internal/cli/datasets.go`, `internal/mcpserver/evals.go`, `docs/datasets.md`, spec 014 — the store executes nothing; a trace joins a run through two columns the mapper claims from `tracepad.run_id`/`tracepad.item_id`, `notPinned` in `sweep.go` is the one predicate the sweep and the retention dry run share, and every number a comparison reports is computed server-side so that two clients cannot disagree about what improved means |
| JSON API plumbing (auth, strict decode, pagination) | `internal/server/api.go`, spec 003 |
| Read API (traces, sessions, stats, system) | `internal/server/traces.go` and neighbours, `docs/api.md`, specs 004 and 009 — the route table in `routes.go` is the surface, and `openapi.json` must agree with it. Paging is keyset in both directions: `trimPage` in `api.go` owns which cursor a page may claim |
| Response budgets and truncation | `internal/server/budget.go`, spec 004 #2 |
| CLI | `internal/cli/`, `internal/client/`, `docs/cli.md`, spec 004 |
| MCP | `internal/mcpserver/`, `docs/mcp.md`, spec 004 — tools call the read API over HTTP, never the store, and only ever with a GET (spec 005 #13) |
| Search | `internal/store/search.go` (the query language, what of a field is indexed, and the snippet) and `searchindex.go` (the index's lifetime), `docs/api.md#search`, spec 011 — the user's text never reaches `MATCH` as written, `searchableField` is what both the index and the snippet see of a payload, and every path that deletes observations deletes their entries in the same transaction |
| Retention and the sweeper | `internal/store/sweep.go`, `docs/retention.md`, spec 005 — every chunk is a `WriteJob`, never a second write connection. `raw_retention_days` is now also the export's reach (spec 019 #1) |
| The raw archive and the way out | `internal/store/raw.go` (the listing, one body, the counters, and where `size_bytes` comes from), `internal/server/raw.go` (the two endpoints and `/system`'s `raw` block), `internal/cli/export.go` + `exportsink.go` (the walk, the two destinations, the retry), `docs/export.md`, spec 019 — the export replays **bodies**, never a synthesis from parsed rows, and it is a client of the API like every other command; the listing runs oldest first on purpose, and the resume cursor names the last batch the receiver *took*, so `--after` starts again at the one that failed |
| OTLP/JSON | `internal/mapping/otlpjson.go`, spec 019 #7 — one mapper for both encodings; the only difference is that OTLP/JSON writes ids as hex where `protojson` writes base64, which is rewritten by field name on the way in and out. A JSON batch is archived as JSON (#8): converting at ingest would make the archive the converter's output |
| Statistics rollup | `internal/store/rollup.go` (the table and one hour's recomputation), `aggregator.go` (the pass, the watermark, the freeze), `histogram.go` (why a percentile is summable), the seam in `internal/server/stats.go`, spec 013 — an hour is recomputed whole and never delta-maintained, and the rollup is the one store the trace sweep spares. The three token sums (spec 031) are read under `tokenClasses` in `rollup.go`, the one closed list the rollup's `rollHour` and the live `StatsSamples`/`StatsTokens` in `query.go` both build their SQL from; it grows by a Decision on spec 031 |
| The per-user rollup and the Users screens | `internal/store/users.go` (both tables, one hour's per-user recomputation, the summary, the listing's four keysets and the live tail), the `statsRoll` job that writes it in `rollup.go` and the session half of `dirtyHours` in `aggregator.go`, `internal/server/users.go` (the two endpoints and the merge) with `user_id` in `stats.go`, `ui/src/routes/users/`, `ui/src/lib/components/users/` + `UserTable.svelte`, `ui/src/lib/api/users.ts` (the pure part: the filters held to `openapi.json`, the sorts, the page's URL), `docs/users.md`, spec 023 — one aggregator writes both tables in one transaction, a session is counted in the hour its user's earliest trace of it starts (#12), the listing is the rollup alone while one user merges the live tail (#4), and an erasure *deletes* the per-user rows rather than re-rolling them (#10) |
| The filter values (`/facets`), the many-valued filters, the name rollup | `internal/store/facets.go` (both halves of the read seam, the hour's `DELETE`/`INSERT … SELECT`, the sweep) and `matchAny` in `query.go` (the one place a list becomes SQL), the `statsRoll` job that writes `names_hourly` in `rollup.go`, `internal/server/facets.go` (the endpoint, the seam, the cap) with `filterList` in `api.go` (the one place a comma list is parsed), `internal/cli/commands.go`'s `facets`, `get_facets` in `internal/mcpserver/tools.go`, `ui/src/lib/api/facets.ts` (the pure part: the comma form, the options a list draws, the chip) with `facets.svelte.ts` (the one read) and `ui/src/lib/components/FacetField.svelte`, `docs/api.md#filter-values`, spec 027 — one value is still `col = ?` and only a list becomes `IN`, so the plan tests keep naming the index the ordinary case seeks (#1); the endpoint takes the **range and nothing else** (#4); `names_hourly` is frozen by its own rows like the other three (#3); and a value the URL carries but the range no longer holds is pinned to the top of its list, because a filter that cannot be seen cannot be undone (#6) |
| The score rollup and the Quality screen | `internal/store/quality.go` (the hour's `DELETE`/`INSERT … SELECT`, both halves of the read seam, the score-side dirty query and the sweep), the `statsRoll` job that calls it in `rollup.go` and the score half of `dirtyHours` in `aggregator.go`, `internal/server/quality.go` (the endpoint, the bucketing and the seam) with the re-roll in `scores.go`, `ui/src/routes/quality/`, `ui/src/lib/components/quality/ScoreBreakdown.svelte`, `ui/src/lib/api/quality.ts` (the pure part: the filters held to `openapi.json`, the series shaping, the card order), `docs/quality.md`, spec 025 — a score is filed under the hour of the **trace** it grades, never its own (#1); the row carries four numbers so the mean is a read-time division and never a mean of means (#2); a score's own arrival is a second dirty question because the trace did not move (#3); and deleting — or *moving* — one re-rolls the hour it leaves, in the write job's own transaction and in that table alone (#4, #20, #22 — `correctScoreHours` in `quality.go`; the handlers do nothing after the writer answers). `created_at` is stamped by `ScoreWrite.apply`, not by the handler, so `commitMargin` covers it (#23). The freeze is per table (#21): `hourFrozenIn` asks whether *this* table already holds the hour, which is what lets 0015's backfill fill an hour past the window whose traces are intact. The answer is capped on both sides — `limit`/`omitted` over series, twenty categories and an `other` key inside one (#24) |
| Admin API (projects, keys, retention, erasure) | `internal/server/admin.go`, `internal/store/admin.go`, `docs/admin.md`, spec 005 — destructive endpoints are a dry run until `?confirm=` echoes the name, checked inside the write transaction; a user-data erasure is chunks of 500, each re-rolling the hours it emptied in its own transaction (spec 023 #19) |
| Deleting traces (by id, by the listing's filter) | `internal/store/tracedelete.go` (`traceRemoval`, the one body erasure and deletion share; `TraceDelete`; `tracesPreview`, the one preview behind all three), `internal/server/tracedelete.go` (the two handlers, the round and its hour-chunking, the preview rendering erasure shares), `traces rm` in `internal/cli/traces.go` with `confirmDestructive` in `cli/admin.go`, `ui/src/lib/components/traces/` (the two dialogs) over `ConfirmCard`'s `immediate`/`prefill`, `chips` exported from `FilterBar.svelte`, `docs/admin.md#deleting-traces`, spec 035 — the bulk form requires `to` (#2), a confirmed round recounts nothing (#5) and ends at `limit` traces or fifty hour-chunks (#4, #14), the client loops the rounds, and nothing is remembered about deleted ids (#10). Migration 0020's partial indexes are what make a payload delete a seek (#13) |
| Who may call what: accounts, sessions, roles | `internal/server/auth.go` is the **one guard** — the policy column of `routes.go`, the credential (header beats cookie), the origin check, the project scope — with `internal/server/policy_test.go` walking every route as each of the six callers; `login.go` holds the three ways in and the account's own routes, `accounts.go` an owner's management of people, `internal/store/accounts.go` the four tables and their jobs (`password.go` is bcrypt, and the hash is unexported so it cannot be rendered by accident), `docs/accounts.md`, spec 028 — a handler asks `callerFrom(r.Context())` and never a header; the permission matrix is the policy column and nothing else (#7); the last enabled owner cannot stand down, checked inside the write transaction (#2); a session's project comes from the path under `/api/v1/projects` and from `X-Tracepad-Project` everywhere else (#6); timestamps in these tables are nanoseconds, correcting the spec's schema comment (#18) |
| Annotation queues and the desk | `internal/store/queues.go` (the queue, the two adds and their dedupe, the claim and its TTL, the completeness check), `internal/server/queues.go`, `internal/cli/queues.go`, the two tools in `internal/mcpserver/evals.go`, `ui/src/routes/queues/`, `ui/src/lib/components/queues/` + `scores/ScoreControl.svelte`, `ui/src/lib/queues.ts` (the pure part: the New-queue gate, the progress, the desk's prefilled form and what it posts) and `annotator.svelte.ts`, `docs/annotation.md`, spec 024 — a queue stores no verdicts: `complete` checks the *scores* on the target and a trace item ignores an observation's scores of the same name (#7); `from-traces` runs `traceQuery` inside the write transaction so two clients composing one filter enqueue the same traces (#4); `next` claims resume-first and `reopen` releases a claim (#5, #16); items are deleted by `traceSweep` and `UserDataErase` in the same job as their trace (#3) |
| Attribute mapping | `internal/mapping/rules.go` is the table; `mapping.go` applies it; `value.go` holds `attrs`, where reading and claiming are separate and an unclaimed attribute keeps the origin it arrived at (spec 012 #7) |
| Web interface | `ui/` (SvelteKit SPA), `internal/ui/` (the embed and the tagless stub), `internal/server/ui.go` (delivery and the SPA fallback), `docs/ui.md`, specs 006 to 010 and 015 — the API types in `ui/src/lib/api/schema.d.ts` are generated from `openapi.json` and the gate fails on drift, and the application-line budget is 21,500 (`scripts/ui-lines.sh`, spec 035 #17) |
| The dashboard | `ui/src/routes/p/[project]/dashboard/` (the page; `stats/+page.ts` is the redirect), `ui/src/lib/dashboard.ts` (the pure part: the ten blocks, the arrangement read out of and written into the opaque preferences object, the order after a drop), `preferences.svelte.ts` (the one write, through `PATCH /auth/me`, and `auth.revise`), `range.svelte.ts` (the remembered window), `summarize` and `previousRange` in `ui/src/lib/api/stats.ts` and `range.ts`, `ui/src/lib/components/dashboard/` (the tile, the block frame, the onboarding card), `quality/QualityCards.svelte` (shared with the Quality overview), `GroupByTotal` in `internal/store/query.go` with `carriesSessions` in `internal/server/stats.go`, `readPreferences` in `internal/server/login.go`, `docs/ui.md#dashboard`, spec 034 — the comparison is arithmetic in the interface and the API has no `compare` (#3); the server validates preferences as *an object under 16 KiB* and nothing else, so a block a later spec adds needs no migration (#9); a hidden block makes no request, and the timeline is read while any chart shows (#8) |
| Which project a screen is about | `ui/src/lib/project.svelte.ts` (the project derived from `page.params.project` against `me.projects`, the remembered id per account, `href`, `bareTarget`, `within`, `switchTarget`), `ui/src/routes/p/[project]/+layout.{ts,svelte}` (remembers the id, keys the children on it, renders the not-there screen), `ui/src/routes/[...path]/+page.ts` and `routes/+page.ts` (the redirects), `ui/src/routes/p/+page.svelte` (no projects yet), `ui/src/lib/components/ProjectSwitcher.svelte`, `settings/NewProjectDialog.svelte`, `tracesLastDay` in `internal/server/admin.go`, `docs/ui.md#the-project-in-the-address`, spec 029 — a component never writes a bare `/traces`: every link goes through `href` (#2); the children of the project layout are `{#key}`ed on the id, so a switch that keeps the route remounts the screen rather than leaving it holding the other project's rows (#6); the switcher asks for `traces_24h` on every open and never on load (#8); the Account tab is bare at `routes/settings/account` with `settings/SettingsTabs.svelte` shared under both addresses, and the account menu links to it without `href` (#14) |
| A payload, shown or edited | `ui/src/lib/components/json/` — `setup.ts` is everything that is not a DOM node (the document a value becomes, where it stops being JSON, which nodes a long one folds, the extension list and the themed chrome), `CodeArea.svelte` is the instance, `JsonView`/`JsonEditor` are the two modes, spec 015 — one surface for reading and writing, so there is one answer to "what does this payload look like"; the mode is the `readOnly` facet and nothing else, `indentWithTab` is deliberately absent (#7), truncation belongs to `Payload.svelte` rather than to the editor (#3), and an editor over a field that may be absent takes `optional`, where an empty document is valid and unmarked (spec 016 #22) |
| A listing (rows, cursors, count, the bar, the panel's walk) | `ui/src/lib/listing.svelte.ts` and its tests, spec 010 — all three listings are one loader, so a listing defect is one defect. `$lib/page` and `$lib/peek` hold the pure part; a listing read oldest first (a dataset's items, a run's) sets `ascending` on its walk (spec 016 #19) |
| What a listing draws around the loader | `ui/src/lib/components/ListingShell.svelte` (the failure line, the table snippet, the bar, the emptied-page line, the empty-state snippet, the spacer) and `ListingCount.svelte` (the header's number), with their tests, spec 026 — the fifteen listings' ladder is one component, so a ladder defect is one defect. A caller keeps its own table and empty state as snippets and passes what genuinely differs: `back` (« goes to the newest or the first), `total` (an exact count the loader never asked for), `problem` (a second failure to fold in, or none). `TracePeekMeta.svelte` is the same move for the five spans a trace's panel shows, with `hide` for the sites that show four |
| The Evals screens (datasets, runs, the comparison) | `ui/src/routes/{datasets,runs,score-configs}/`, `ui/src/lib/components/evals/` (the tables, the three peek bodies, the summary cards), `ui/src/lib/evals.ts` (the pure part: the checkbox rule, the *changed only* filter, the words a cell uses), `ui/src/lib/api/runs.ts` (the run filters, held to `openapi.json`), `docs/ui.md#evals`, spec 016 — every number on these screens is the server's (spec 014 #18); the client decides which rows to draw and never what a verdict is |
| Writing an eval (the item editor, the forms, the deletions) | `ui/src/lib/components/evals/ItemEditor.svelte` over the routes `datasets/items/new` and `datasets/[name]/items/[id]/edit` (spec 016 #21), `ScoreConfigDialog.svelte` with `ui/src/lib/api/score-configs.ts` (the vocabularies and the rules, held to `openapi.json`), `NewDatasetDialog`/`DeleteDatasetDialog`, `ui/src/lib/components/ConfirmDialog.svelte`, `itemBody`/`savedMessage` in `$lib/evals`, `docs/datasets.md#the-same-loop-from-the-web-interface` — a write is one of spec 014's endpoints and never a verb of the screen's own; the echo ceremony (`ConfirmCard`) is only where the server has a dry run, and the dialog is where it does not (#6) |
| The Prompts screens (the listing, the versions, the diff, the editor) | `ui/src/routes/prompts/`, `ui/src/lib/components/prompts/` (the chip, the label control, the version view, the painted diff, the editor, the delete card), `ui/src/lib/prompts.ts` (the pure part: the Save gate per field, the chip order, the diff painter's classification, the two conversions between a stored body and the form), `docs/ui.md#prompts`, spec 021 — the diff is the server's and is only painted here (#3), the gate mirrors the `400`s spec 003 already gives and never replaces them (#5), a move or a removal of a label goes through `ConfirmDialog` naming it and a new label does not (#6), and the editor is only ever "new version from this one" because the store is append-only |
| The Python package | `sdk/python/` (`src/tracepad/` is the package, `tests/` its suite and `tests/e2e/` the run against a real binary), `docs/sdk-python.md`, spec 017 — two dependencies and no third, no provider-client wrapper ever (design §6.5); `_tracing.py` holds the provider adaptation and defers the SDK's own imports into `init`, `_attributes.py` is the vocabulary that `internal/mapping/rules.go` reads back, and the application-line budget is 1,700 shared with spec 018 (`scripts/sdk-lines.sh`, spec 036 #8). `scripts/fixtures/tracepad_sdk.py` rewrites `testdata/otlp/010-tracepad-sdk.pb` from the package's own exporter |
| The Node package | `sdk/js/` (`src/` is the package, `test/` its suite and `test/e2e/` the run against a real binary), `docs/sdk-js.md`, spec 032 — `tracing.ts` holds the provider adaptation and the shapes, `attributes.ts` is the vocabulary of `internal/mapping/rules.go` key for key with the Python one, `test/docs.test.ts` type-checks every example in the doc, and the application-line budget is 2,000 shared with the harness (`scripts/sdk-js-lines.sh`, spec 036 #8). `scripts/fixtures/tracepad_sdk_js.mjs` rewrites `testdata/otlp/013-tracepad-sdk-js.pb` from the package's own exporter, and `internal/server/sdkfixture_test.go` reads the Python and Node packages' fixtures back through the OTLP path |
| The Go package | `sdk/go/` — a nested module (`github.com/tracepad/tracepad/sdk/go`; the root `go test ./...` does not see it, `make sdk-go-unit` and the gate do), `docs/sdk-go.md`, spec 033 — the same vocabulary as the Python package in the shape of the OTel API (a context in, a context out, `defer step.End()`), three dependencies and no `replace`, one package-level default configured by `Init` (`tracepad.go` holds the provider adaptation), an explicit `Result` and no response reader (#5). Function and type share one namespace in Go, so `Generation` hands out a `*Call` and `Prompt` a `*PromptVersion` (#14). `sdk/go/internal/fixture` rewrites `testdata/otlp/014-tracepad-sdk-go.pb` from the package's own exporter, deterministically (#13); the budget is 2,100 (`scripts/sdk-go-lines.sh`, spec 036 #8). Trace deletion from all three packages is spec 036: `_traces.py`, `traces.ts`, `traces.go`, a module the tracing path never imports |
| The eval harness in Node | `sdk/js/src/harness.ts` (`stamp`, which `tracing.ts`'s processor calls at `onStart`, `Run`, `Attempt`, `scoreConfigs`, `compare`, `itemId`, the paging loop) and `datasets.ts` (`Dataset`, `Item`), `docs/datasets.md#the-same-loop-from-node`, `docs/sdk-js.md#evals`, spec 032 #10 — the attempt rides the OTel context, so it reaches an `await` and a callback and not a worker; the block opens no span; `wrap` is the close that can `fail`, `Symbol.asyncDispose` the one that cannot see the error and finishes |
| The eval harness in Go | `sdk/go/harness.go` (the processor, `Run`, `Attempt`, `ScoreConfigs`, `Compare`, `ItemID`, the paging iterator) and `datasets.go` (`Dataset`, `Item`), `docs/datasets.md#the-same-loop-from-go`, `docs/sdk-go.md#evals`, spec 033 #10 — the item block is a context (`run.Item(ctx, item)` returns one), read by a `SpanProcessor` at `OnStart` and never a span the harness opened; `Init` registers it before the exporter and under `WithExport(false)` too; `Items` is an `iter.Seq2` over every page; there is no block that closes a run, so `Fail` on the error path is the caller's (#16) |
| The eval harness in Python | `sdk/python/src/tracepad/_harness.py` (the processor, `Run`, `Attempt`, the score configs, `compare`, the paging loop) and `_datasets.py` (`Dataset`, `Item`), `docs/datasets.md#the-same-loop-from-python`, `docs/sdk-python.md#evals`, spec 018 — the stamping is a `ContextVar` read at `on_start` and never a span the harness opened (#3), `init` registers the processor before the exporting one and under `export=False` too, and the read side is the server's JSON as `dict`s because a model layer is a place to start disagreeing with it (#8) |
| Packaging: the image and the release | `Dockerfile` + `.dockerignore` (the whole recipe — the image builds both halves from the checkout and copies no prebuilt binary), `scripts/image-check.sh` (the contract, asserted from outside because the image has no shell), `.github/workflows/release-server.yml` (GoReleaser for the archives, `buildx` for one multi-arch manifest on GHCR), the `docker` job in `ci.yml`, `docs/docker.md`, spec 020 — `tracepad health` (`internal/cli/commands.go`) is the container's `HEALTHCHECK` and the one command that needs no key |
| Configuration | `internal/config/`, spec 001 + spec 002 Configuration tables |
| The docs' cross-references | `scripts/doc-anchors.sh` and `scripts/doc-anchors-fixture/`, spec 026 #6 — every `[…](file.md#anchor)` in `docs/*.md`, `README.md` and `AGENTS.md` is checked against the target's headings under GitHub's slug rule, fenced code blocks and inline code spans read as neither headings nor links. It runs in `make gate`; the fixture run is its own CI step, and it also builds a file long enough that a pipe would break the check (#13, #14) |
| The agent skill | `agent/skills/tracepad/` (the skill itself: `SKILL.md` and `references/`), `agent/skills/command.go` (`skills install`/`show`, the stamp), `agent/skills/drift_test.go` with its fixture in `testdata/drift/`, `docs/agents.md`, spec 037 — the skill teaches order and never restates what the binary says about itself (#2). **A PR that changes a command, a flag, an MCP tool or a route the skill names updates the skill in the same PR**: the drift test enforces the names, the reviewer the meaning (#9). Budgets: `SKILL.md` ≤ 200 lines, the whole skill ≤ 900 (#1) |
| A test that needs a store | `internal/storetest` for the store's clients (`Open`, `Path`, `Writes`), `harness_test.go` inside `internal/store` for its own suite — one migrated template copied per test and a one-millisecond commit window, because a suite that opens an empty database per test and waits out the default window per lone write spends most of its time on neither the code under test nor its own assertions. The migration tests and the writer's own tests are the exceptions, on purpose |

Attribute semantics for the `langfuse.*` dialect are derived from Langfuse
(MIT) — see `NOTICE`. Keep new rules in the table in `rules.go`, with the
reason in a comment; adding a dialect should be a table edit.

## Commands

- `make gate` — the full gate (format-check + vet + `go test ./...` + the
  doc-anchor sweep + `svelte-check`, vitest and the API-type drift check). It
  is what CI runs and what the git **pre-push** hook runs. The Go half is
  bounded by its slowest package, and every package is under ten seconds: the
  suites used to spend most of their time waiting rather than on the code
  under test — the writer's fifty-millisecond commit window on every lone
  write, the migrations on every empty database, bcrypt at cost 12 on every
  login — and each is now paid once per binary (`internal/storetest`,
  `internal/store/harness_test.go`, `internal/server/main_test.go`). A new
  suite that opens a store should start there rather than from `store.Open`.
- `make precommit` — the fast gate the git **pre-commit** hook runs:
  format-check, vet, the tests of the Go packages you staged, and the
  interface checks only when `ui/` or `openapi.json` is staged. Seconds, not
  minutes. Both hooks self-install on first run (and on Claude Code session
  start); `make install-hooks` force-reinstalls them. Worktrees share the
  hooks directory, so installing once covers every checkout.
- `make doc-anchors` — check every anchor in `docs/`, `README.md` and
  `AGENTS.md` against the heading it names (part of the gate);
  `make doc-anchors-self-test` runs the checker over its fixture.
- `make dev` — run the server, output mirrored to `.dev.log` (read that file
  first when debugging a running server). `npm run dev` inside `ui/` serves
  the interface with hot reload against it.
- `make build` — binary with the web interface into `./bin`;
  `make build-server` builds without it and needs no Node.
- `make e2e` — boot the real binary on a temp database and run the Playwright
  smoke. Its own CI job, never part of the gate.
- `make image` — build the Docker image as `tracepad:dev`; `make image-check`
  boots it on an ephemeral volume and asserts the contract (healthy through the
  container's own `HEALTHCHECK`, the version, uid 65532, the database on the
  volume, the licence files). Docker is a prerequisite of these two only. Both
  are the `docker` CI job, so a local run and CI prove the same thing.
- `make smoke` — export from pinned real SDKs and from our own package into a
  real binary and assert the rows. Needs network on first run (installs them).
- `make sdk-test` — the Python package's unit suite, then its end-to-end suite
  against a binary it builds. `uv` if present, `venv` otherwise;
  `SDK_SKIP_E2E=1` runs the unit half alone. `make sdk-lines` reports its
  budget.
- `make sdk-js-test` — the Node package's type check and unit suite, then
  its end-to-end suite against a binary it builds. `SDK_SKIP_E2E=1` runs the
  unit half alone. `make sdk-js-lines` reports its budget.
- `make sdk-go-test` — the Go package's vet and unit suite inside its module,
  then its end-to-end package against a binary it builds; `make sdk-go-unit`
  is the unit half alone, which the gate runs. `make sdk-go-lines` reports
  its budget.
- `make fixtures` — regenerate `testdata/otlp/*.pb` and their goldens after a
  deliberate mapping change. Review the golden diff; it *is* the change. The
  three bodies that are not synthetic are rewritten from the packages first:
  `010-tracepad-sdk.pb` needs `uv` and `013-tracepad-sdk-js.pb` needs `npm`,
  and without them each stands as committed; `014-tracepad-sdk-go.pb` needs
  only Go.

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
- **Run what you touched, not everything, until the push.** While iterating:
  `go test ./internal/<package>/` for the package you changed, `npx vitest run
  <file>` for the component, `npx playwright test <spec>` for the flow. The
  full Go suite, the full interface suite and the full Playwright run are
  each once per push — the pre-push hook runs the first two, and CI runs
  all three. A day of work was measured at 88 full Playwright runs, 71
  minutes of a 380-minute session; the pre-commit hook's full gate cost
  another 17 minutes over 25 commits.
- **Long output goes to a file, once.** `npx playwright test > /tmp/e2e.log
  2>&1; tail -20 /tmp/e2e.log` — then `grep` the file for the next question.
  Re-running a seven-minute suite to read a different part of its output is
  the single most expensive habit in the transcripts.
- **Push, then report.** After `git push`, tell whoever asked for the work
  that the PR is up; do not sit in `gh pr checks --watch` — the person or
  agent coordinating watches CI, and the session is free for the next thing.
- Stage git changes with explicit paths (never `git add -A`); review
  `git diff --cached --name-only` before committing.

## Releasing

A version tag is the one act that publishes anything. `v0.2.0` runs
`release-server.yml`: a `check` job validates the tag and works out which image
tags it may move, GoReleaser puts the archives and checksums on GitHub
Releases, then `buildx` pushes `ghcr.io/tracepad/tracepad` as `0.2.0`, `0.2`
and `latest`. A pre-release tag (`v0.2.0-rc.1`) publishes its exact tag alone —
no `X.Y`, no `latest`. Nothing about this runs on a push to `main`.

**A back-patch is safe to tag.** `latest` and `X.Y` move only when the tag is
the newest of its kind, so releasing `v0.2.5` after `v0.3.0` publishes `0.2.5`
and `0.2` and leaves `latest` where it is (spec 020 #16). The workflow logs
which moving tags it declined and why.

Before tagging:

- **Tag a green commit.** The workflow does not check CI and cannot: a tag is
  yours to place, and it runs on whatever commit it names. What a release
  publishes should be what passed, so look at the commit's checks first —
  `gate`, `e2e`, `smoke`, `sdk`, `sdk-js`, `sdk-go` and `docker`.
- The Python package has its own tag and its own workflow
  (`sdk-py/v*`, `release-sdk-py.yml`), and so does the Node package
  (`sdk-js/v*`, `release-sdk-js.yml`, npm trusted publishing); the server's
  tag moves neither.
- The Go package's tag is `sdk/go/vX.Y.Z` — the toolchain's rule for a module
  in a subdirectory — and the tag is the whole release: no workflow, the
  module proxy fetches it from the repository (spec 033 #1).

One-time, and the owner's to do by hand:

- **Make the GHCR package public.** The first push creates
  `ghcr.io/tracepad/tracepad` as a *private* package, which means the
  quickstart's `docker run` fails for everyone but you. It is a switch in the
  package's settings on GitHub, and it has to be flipped once before the beta.
- Keep `docker` a required check on `main` alongside the other jobs.
