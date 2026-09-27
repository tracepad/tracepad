# Spec 004 — Read API, CLI & MCP

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> The product thesis made concrete: the primary consumer of traces is an
> agent, and the read API is the product's contract — a feature does not
> exist until it is readable over HTTP (design §3). This spec ships that API
> with agent-first affordances (response budgets, truncation markers, task
> shortcuts, self-description), plus the two zero-logic clients that ride on
> it: a CLI in the same binary and an MCP server speaking the 2026-07-28
> stateless protocol. After this spec ships, "show me the last failed trace
> with its stack trace" is one command in a terminal, one tool call in an
> agent, and one HTTP request in a script — all returning the same bytes.

---

## Overview

Deliverables:

- Read API: `GET /api/v1/traces` (filters, cursor, `?fields=`),
  `GET /api/v1/traces/{id}` (observation tree, `?expand=io`),
  `GET /api/v1/traces/last` (task shortcut),
  `GET /api/v1/observations/{id}/io`, `GET /api/v1/sessions/{id}`,
  `GET /api/v1/stats`, `GET /api/v1/prompts/{name}/diff`,
  `GET /api/v1/system`, `GET /api/v1` (self-description),
  `GET /api/v1/openapi.json`.
- CLI (same binary, pure API client): `traces ls|show|last`, `tail`,
  `sessions show`, `scores ls`, `prompts ls|get|push|diff`, `stats`,
  `system`. Global `--json`; human-readable output on a TTY.
- MCP server: streamable HTTP at `/mcp` on the running server (stateless,
  protocol 2026-07-28, official `modelcontextprotocol/go-sdk` v1.7.0+) and
  `tracepad mcp` stdio mode for local clients; 7 tools, each a thin wrapper
  over one read-API call.
- Schema 0004: read-path indexes only (no new tables).
- Docs: `docs/api.md`, `docs/cli.md`, `docs/mcp.md` (same PR).

## Decisions log

| # | Decision | Why |
|---|---|---|
| 1 | One spec ships the API and both clients together; clients contain zero logic (call → HTTP request → formatting) | The design rule "a feature doesn't exist until it's in the read API" is only enforceable if the clients physically cannot do more than the API does. Landing all three together proves the API is sufficient — any client-side workaround is an API gap found before merge, not after. |
| 2 | Every response respects a byte budget: default 50 KiB, `?budget=` override (min 4 KiB, max 5 MiB). Oversized payloads are cut at a UTF-8 boundary and marked `{"truncated": true, "size": N, "trace_id": …, "observation_id": …, "full": "<url>"}` | The consumer's context window is a scarce resource the API must respect (design §3.2). An explicit marker lets the agent decide whether to expand — silent truncation or unbounded responses both waste the consumer's budget, in opposite ways. The marker carries both the ready-made URL (HTTP consumers) and the raw id pair (MCP consumers, Decision 17) so every consumer type has a working follow-up. |
| 3 | `GET /api/v1/observations/{id}/io` is the one budget-exempt endpoint and returns the payloads whole | It exists to be the `full` target of every truncation marker; a budget here would recurse. Payload size is already bounded at ingest by `TRACEPAD_MAX_BODY_BYTES`. |
| 4 | Trace list pagination is keyset from day one: cursor over `(timestamp, id)` with a row-value predicate; migration 0004 rebuilds `idx_traces_timestamp` as `(project_id, timestamp DESC, id DESC)` | Spec 003 #25 caught the scores cursor scanning from the newest row on every page because the index lacked the tie-break; the traces list is the hottest read and gets the fix preemptively, verified by EXPLAIN QUERY PLAN in a test. |
| 5 | `GET /api/v1/traces/{id}` returns the observation tree **nested** (children inside parents, siblings by `start_time`), with `input`/`output` omitted unless `?expand=io` | The tree is the mental model of a trace; flat lists push assembly onto every consumer. Spec 002 #6 already promises assembly at read time. Without `expand=io` a trace of hundreds of observations still fits the budget. |
| 6 | `?expand=io` inlines payload previews: each observation gets an equal share of the remaining budget, cut per Decision 2 with per-observation markers | A debugging agent usually needs the *shape* of every IO and the *full text* of one or two; previews-plus-markers serves that in one round trip, and the markers name the follow-up (§3.2's list → filter → get → N×get collapse). |
| 7 | `GET /api/v1/traces/last` accepts every list filter and returns the same shape as `GET /traces/{id}` (incl. `?expand=io`) | "The last failed trace, whole" is the canonical agent task (design §3.2, spec 002 #26). One round trip instead of list-then-get; implementation is `LIMIT 1` on the list query feeding the tree renderer — no new logic. |
| 8 | `GET /api/v1/stats` computes on the fly (no rollup table): `group_by=hour\|day\|model\|environment`, returning count, error_count, total_cost, latency p50/p95 | Design §5.4: at MVP scale (30 days × tens of thousands of traces) SQLite aggregates in tens of ms; a rollup table is premature state to keep consistent. Percentiles are computed exactly in Go over the grouped scan — approximation is not worth its explanation. |
| 9 | `GET /api/v1` returns the endpoint map with one-line descriptions; `GET /api/v1/openapi.json` serves a hand-authored, embedded OpenAPI 3.1 document; a test walks the router and fails on any route missing from the document (and vice versa) | Self-description is how an agent orients without external docs (design §3.2). Hand-authored beats generated because the document *is* the contract (reviewable in diffs); the parity test is the `docs-gen && git diff --exit-code` idea (PROCESS §3) turned inward. |
| 10 | `GET /api/v1/system` reports version, uptime, DB file size, per-table row counts, writer queue depth, and in-process counters since start: batches/spans accepted and skipped per dialect, plus every distinct `x-langfuse-ingestion-version` seen | Self-diagnosability (design §3.4) and the counter half of spec 002 #17 finally get a surface. Counters are in-memory and say so (`"since": <start time>`): honest process-lifetime numbers now beat a metrics subsystem later. |
| 11 | The CLI is an HTTP client of the read API — it never opens the database | Same reason as #1, plus operational truth: the server holds the SQLite writer; a second process reading the live DB file is exactly the class of corruption-adjacent cleverness this project refuses. Connection via `TRACEPAD_URL` + `TRACEPAD_API_KEY` (flags override). |
| 12 | CLI output: human-readable tables when stdout is a TTY, JSON otherwise; `--json` forces JSON. Exit codes: 0 ok, 1 request/server error, 2 usage error | An agent piping `tracepad traces ls` gets machine JSON with zero flags — the TTY check makes agent-first the default rather than an option. The codes let scripts distinguish "no such trace" from "you typoed a flag". |
| 13 | `tracepad tail` polls the list API (default every 2 s, `--interval`), printing traces newer than the last seen `(timestamp, id)` | A push channel (SSE/WS) is a new server surface for one command; polling through the public API needs nothing and inherits auth, filters, and budgets. The cursor pair makes polling exact, not time-window fuzzy. |
| 14 | MCP speaks protocol **2026-07-28** via the official Go SDK ≥ v1.7.0: streamable HTTP at `/mcp` on the main listener with `Stateless = true`; older clients fall back to `2025-11-25` stateful per SDK negotiation | The month-old 2026-07-28 revision made the protocol stateless — no handshake, no `Mcp-Session-Id`, version and capabilities ride in `_meta` per request. Our tools are stateless read wrappers, so the new core fits exactly (any LB works, zero session bookkeeping); the SDK carries the compatibility window so we don't. |
| 15 | `tracepad mcp` runs the same MCP server over stdio for clients that can't speak remote HTTP; it connects to a running server via `TRACEPAD_URL`/`TRACEPAD_API_KEY` | Design §3.3 promised the stdio path. It is the same tool registry with a different transport — no second implementation. |
| 16 | MCP tool handlers call the HTTP read API (in-process loopback when embedded at `/mcp`), never the store | One source of truth for budgets, truncation, auth, and JSON shape. A tool result and a curl of the corresponding endpoint are byte-identical `structuredContent` — testable, and the #1 invariant holds by construction. |
| 17 | Eight tools: `list_traces`, `get_trace`, `get_last_trace`, `get_observation_io`, `get_session`, `get_prompt`, `list_scores`, `get_stats`. No `search` tool | Each maps 1:1 onto an endpoint. `get_observation_io` must exist because truncation markers point at an HTTP URL a pure-MCP consumer cannot fetch — the expansion affordance of Decision 2 has to be reachable as a tool or the markers are dead ends. The design sketch listed `search`, but there is no search endpoint yet — a tool faking it over list filters would misrepresent capability to the model; it arrives with FTS. Descriptions are written as *when-to-use triggers* ("the user asks why the last run failed…"), not endpoint restatements. |
| 18 | Tools declare `outputSchema` (JSON Schema 2020-12), return `structuredContent`, and carry annotations: `title` and `readOnlyHint: true` on every tool (the whole surface is reads); `tools/list` returns a deterministic order with `ttlMs: 3600000, cacheScope: "private"` | Schemas let clients validate and models plan; annotations drive host-side auto-permissions (a read-only tool should not prompt like a write); deterministic ordering plus TTL — both 2026-07-28 affordances — make the tool list prompt-cache-friendly; `private` is honest for an authenticated, per-project surface. Tight input schemas (enums for `group_by`/`status`, patterns for hex ids, bounded `limit`) are part of the contract, not decoration. |
| 19 | Deprecated-in-2026-07-28 features are not adopted: no roots, sampling, or logging capabilities, no tasks extension, no elicitation/MRTR | Roots/sampling/logging are formally deprecated (12-month removal window) — adopting them now is building on a condemned floor. Tasks exist for long-running work; every tool here answers in milliseconds. Server logs go to stderr/log file as they already do. |
| 20 | `/mcp` auth is the same `Bearer tp-sk-…`/Basic as the rest of the server; no OAuth | Self-hosted with pre-shared project keys — the OAuth authorization framework in the MCP spec targets multi-tenant public servers. MCP clients (Claude Code included) pass static headers to HTTP servers. Revisit only if a hosted offering ever exists. |
| 21 | `GET /api/v1/prompts/{name}/diff?from=N&to=M` returns a unified text diff of the pretty-printed `prompt` and `config` between two versions | Design §6.3 names version diff as part of the read surface, and "what changed in the prompt between yesterday's and today's run" is an agent question. Unified text over structural JSON-diff: every consumer already reads patches, and it needs no diff vocabulary of our own. |
| 22 | Incoming OTel trace context on `_meta` (`traceparent`) is recorded in the request log when present; nothing more | The 2026-07-28 spec documents the convention, and a tracing product should at least not drop trace context on the floor. Full self-instrumentation is deliberately out of scope. |
| 23 | **2026-08-27** — `GET /api/v1/stats` answers with `{"group_by", "unit", "buckets"}`: `hour`/`day`/`environment` count traces, `model` counts observations, and `unit` says which | Decision 8 fixes the bucket shape but not what a bucket counts, and a trace has no model — only an observation does. Grouping traces by "a model one of their spans used" would count a two-model trace twice under a trace-shaped `count`, and grouping everything by observation would make `group_by=day` answer a question nobody asked. So the unit differs by dimension, which is only safe if the response says so: `count=41` under `group_by=day` and `count=41` under `group_by=model` are not comparable, and two numbers that look alike and are not is exactly the silent lie an agent-first API cannot afford. For `model` the fields are the observation's own: `error_count` counts `level='ERROR'`, cost sums `cost_details.total` over `provided_cost=1`, latency is `end_time - start_time`. `from`/`to` bound the trace timestamp under every grouping — one rule for the time window is worth more than a per-grouping clock. |
| 24 | **2026-08-27** — An observation's `metadata` rides with `input`/`output` under `?expand=io`, budgeted and marked the same way. A trace's own `metadata` is returned whole in `GET /traces/{id}` and is the only payload outside `/observations/{id}/io` that no budget applies to | Decision 5 hides `input`/`output` without `expand=io` and is silent about `metadata`, but `metadata` is a payload row like the other two and Decision 3 returns all three from the IO endpoint. Inlining it unasked would falsify Decision 5's own promise that a trace of hundreds of observations still fits the budget, and a truncation marker on it would point at an endpoint that — before this decision — did not return it. Trace-level metadata is the deliberate exception: it is assembled from attributes (kilobytes of config and context), not from LLM IO (megabytes), and no expansion endpoint exists for it, so truncating it would be loss with no path back. Revisit if real-world trace metadata starts blowing budgets. |
| 25 | **2026-08-27** — The truncation marker of Decision 2 also carries `"preview"`: the prefix of the payload that fit | Decision 6 promises `?expand=io` inlines payload *previews*, and Decision 2's marker shape has nowhere to put one. Without it "cut at a UTF-8 boundary" has no cut text to describe, and an agent seeing only a size and a URL has to spend a round trip to learn whether the payload is even the one it wants. `preview` is omitted rather than empty when the share left no room for a prefix worth showing. |
| 26 | **2026-08-27** — `traces.timestamp` is made total: ingest falls back to the smallest start time of any kind when no span carried a positive one, and migration 0004 backfills the rows written before it did | The trace list pages on `(timestamp, id)` and the keyset predicate is a row-value comparison, which evaluates to NULL — and therefore excludes the row — whenever `timestamp` is NULL. A trace whose every span carried an unset start time was listed while it fit on the first page and then invisible on every page after it: a silent gap in a listing the caller believes is complete, which is the failure class spec 003 #23 refuses. Spec 002's rule (a span that never started says nothing about when the trace did) is kept as the *preference*; the fallback only replaces the NULL, and such a trace sorts last, where a trace of unknown time belongs. |
| 27 | **2026-08-27** — The router is a table: one list of `{method, path, description, handler}` feeds the mux, `GET /api/v1` and the OpenAPI parity test. `/mcp` is registered outside it | Decision 9 asks for a test that walks the router, and Go's `http.ServeMux` does not enumerate its routes — so the enumeration has to exist before the mux does, and everything that must agree about the surface reads the same list. A route added anywhere else fails the parity test, which is the point. `/mcp` stays out because it is a JSON-RPC transport rather than an endpoint of this API: OpenAPI would describe it as a single opaque POST, and the endpoint map would advertise it to consumers that cannot speak it. Whether it is serving, at what path and at which protocol version, is reported by `GET /api/v1/system`, where the rest of this process's own state already lives. |
| 28 | **2026-08-27** — Every response carries `X-Tracepad-Version`, and the CLI warns once per invocation when it differs from its own build | The edge case asks the CLI to report version skew "by comparing CLI version with `GET /api/v1/system`", which as written costs an extra round trip on every command — so in practice it would be spent nowhere, and the skew would go unreported exactly when it matters. A response header costs nothing, works on every endpoint including the ones a script pipes, and is the same fact. `/api/v1/system` still reports the version in its body; the header is how a client learns it without asking. |
| 29 | **2026-08-27** — CLI flags may follow positional arguments: the argument list is reordered before the standard `flag` parser sees it | Go's `flag` stops parsing at the first non-flag word, so `tracepad traces show <id> --full` — the form the CLI contract in this very spec is written in — would silently ignore `--full` and print the trace without its payloads. Silently, which is the problem: the caller asked for something and got something else. Reordering keeps the standard parser (and its error messages) and asks the flag set itself whether a flag takes a value, rather than guessing, so `--limit 5` keeps its value and `--full` does not swallow the id. |
| 30 | **2026-08-27** (from PR #5 review) — A parent cycle is *broken*: the cycle's entry span is detached from its parent and rendered at the root, keeping its own children and its `parent_observation_id` | The edge-case rule that an unparented span renders at the root was implemented by re-rooting the span without cutting the edge that led into it, which left `children` cyclic. Reachability was memoised so tree construction terminated, but rendering recursed until the goroutine stack was exhausted — a fatal error in Go, so the process died, and died again on every retry of that trace. `parent_span_id` is client bytes stored verbatim (spec 002), so two spans naming each other is something a caller can send, deliberately or by accident. The fix keeps every span visible exactly once and keeps what each claimed about its parent; only the edge is gone. |
| 31 | **2026-08-27** (from PR #5 review) — Truncation markers are budgeted like the payloads they replace: when the share cannot cover even a bare marker, no payload is expanded and the response carries one `expansion` object instead, with `payloads`, `budget_needed` and a reason | Decision 6 divides the remaining budget by the number of payloads and assumes a marker fits in the share. On a wide trace it does not: 200 observations × 3 payloads leaves ~80 bytes each against a ~200-byte marker, and emitting one anyway produced a 157 KB answer to a 50 KiB budget — three times over, on the one endpoint whose whole purpose is to respect the consumer's context window. Refusing the expansion is not a loss of affordance: the tree already lists every observation id, so each payload is one `/observations/{id}/io` call away, and `budget_needed` is the exact number to retry `?budget=` with. It also makes Decision 2's promise literally true for the first time, which the docs had already been stating. |
| 32 | **2026-08-27** (from PR #5 review) — `tracepad tail` follows a one-minute window rather than only what is newer than the newest row seen, skipping ids it has already printed | Decision 13 calls the cursor pair "exact, not time-window fuzzy". It is exact about *ordering* and wrong about *arrival*: `traces.timestamp` is when a trace's earliest span started, not when it was committed, and exporters batch — the OTel SDK's default processor flushes every five seconds. A run that began at 12:00:00 and landed at 12:00:05 sorts behind one that began at 12:00:03, so a forward-only follow never printed it. Missing lines, silently, in something a person reads as a live log. The window costs one page per poll and the id set makes duplicates impossible; it is bounded at 10000 ids, past which the follow narrows back towards its watermark rather than growing without limit. |
| 33 | **2026-08-27** (from the second PR #5 review) — `GET /api/v1/system` reports row counts within the asking project: `payloads` is dropped and `projects` becomes a bare tenant count | Decision 10 says "per-table row counts" and the implementation read that as the whole table, so any project key learned how many traces, observations and payloads the other tenants on the process held, and how many API keys they had. Projects are the unit of isolation from spec 001 onward, and design §6.3 puts cross-project access behind an admin token that does not exist yet (spec 005) — so until it does, the honest scope is the caller's own. `payloads` has no project column and cannot be attributed, so it is not reported at all rather than reported whole; `projects` stays as a count because how many tenants share a process is an operator fact that names none of them. The ingest counters are scoped the same way and for the same reason: process-wide totals told one tenant how much traffic another was sending and which SDK versions it ran. Every observation point is reached after the request has authenticated, so there is no ingest traffic without a project to file it under and nothing is lost by scoping. `size_bytes` is the one number that stays deployment-wide: it is the file on disk, which is exactly the operator question `/system` exists to answer, and payloads and compression are shared so it cannot be split — the docs say so rather than leaving it to be assumed. |
| 34 | **2026-09-25** — An MCP request that omits the optional `params` member, or sends it as `null`, is served like one with `params: {}`. A panic in the receiving handler chain — our middleware, the SDK's method dispatch and the tool handlers — answers that one request with JSON-RPC `-32603 internal error` and is logged with the method, the tool, the panic's type and the function and line it was raised at — plus the runtime's message when the panic is a `runtime.Error` — but not any other panic value and not the request. A kind of panic is its type, its site and its tool: the first of each kind is logged with its stack, repeats as a one-line count at most once a minute, and past 64 kinds new ones are counted together under a line of their own that names no method or type. The SDK's own work before that chain (checking the request, decoding its params) and after it (shaping and encoding the result) is not covered. Decision 22's `traceparent` is logged only when it is well-formed W3C trace context (Trace Context §3.2.4: lowercase hex, non-zero trace and parent ids, version not `ff`; version `00` exactly 55 characters; a later version read by its first 55 characters when a dash or nothing follows them, and only those logged) and is otherwise dropped | For methods whose `params` are optional (`tools/list`, `ping` and the other list methods) the SDK hands the middleware a typed nil pointer inside a non-nil interface, so the trace-context middleware's nil check passed and reading `_meta` dereferenced nil. The SDK runs each request in a goroutine of its own, which net/http's recovery does not cover, so one unauthenticated sixty-byte POST ended the whole process — ingest, UI and every other client — and it ran before any credential was checked. The nil check now looks through the interface with reflection, because the SDK's own check is unexported and a type switch over today's params types would miss the next one; go-sdk v1.8.0 has the same shape, so this is ours to guard either way. The recovery middleware is outermost in the receiving chain because that is where the SDK calls our code; a wrapper around the `http.Handler` would never see the panic, and the SDK's code on either side of the chain runs in the same goroutine with no hook of ours around it, so the limit is named rather than implied. A panic's value is left out because a message built from an argument would carry the request, arbitrary input from a caller nobody has authenticated yet, into the log; a `runtime.Error` is the exception, because the Go runtime writes its message and the most of the request it can carry is a number derived from it (an index, a length), never its bytes. The stack says where it happened, which is what a fix needs, and it needs it once: a panic reachable without a credential can be sent in a loop, so the stack is logged once per kind and repeats cost one line a minute, which also keeps a panic that is still happening visible rather than silent. The kind includes the site and the tool because every tool shares the method `tools/call`: keyed on the method, a second tool's bug of the same type would never get a stack. The tracked kinds are capped so the bookkeeping cannot grow either. The state belongs to the server, and a process serves one. Recovering here also covers the read API the tools reach in-process, which is what net/http already does for the same handlers reached over HTTP, one request at a time. The same reasoning bounds the `traceparent` line, which is written before authentication too: a strict format means a caller cannot write arbitrary bytes, of arbitrary size, into the operator's log through it. Later versions are accepted as the W3C spec asks, so a newer propagator's context is not dropped, but no more than their first 55 characters is ever logged. It is logged once per request; a test pins that on both protocol paths. |
| 35 | **2026-09-26** — **The human mode never hands a terminal a control character from the data.** Every string the server sends that the CLI prints outside JSON mode — trace, observation, session, user, score, prompt, dataset, run, queue, project and account fields, search snippets, payload text, cursors, an export receiver's answers, and the error messages on stderr — goes through one function (`internal/termsafe`), which prints C0 controls, DEL, C1 (U+0080–U+009F), the bidi embeddings, overrides and isolates (U+202A–U+202E, U+2066–U+2069) and bytes that are not UTF-8 as visible escapes: `\x1b`, `\u009b`, `\u202e`, `\xff`. Tab and newline are escaped in a one-line field — a name, an id, a table header or cell — and kept in the few values that are text by nature and print as a block: a prompt, a prompt diff, a dataset or score-config description, a server note, and every error message — an observation's status message, a run's failure, a retry's reason, the CLI's own errors — whose lines after the first are indented under their label; carriage return is escaped everywhere. Backslash is left alone, so a value that needs nothing prints exactly as before; the price is that a value holding the text `\x1b` reads like one holding ESC, which is cosmetic and was accepted. Table headers and cells are escaped in the table writer. **Human-readable output also goes through an escaping writer**: on a terminal the CLI's stdout, and its stderr always, escape every control character except newline, tab and the exact faint-style pair, so a value a renderer prints without termsafe still reaches the terminal inert — the per-field calls remain where a one-line field must also lose its newlines and tabs. The JSON mode writes to the unwrapped stdout. The dimmed search snippet is escaped *before* it is wrapped, so the faint-style pair is the only escape sequence a human-mode run writes. JSON mode is unchanged and byte-exact (#1), including C1, which Go's encoder does not escape: a pipe is read by a program, and escaping is that program's job. That holds for `--json` on a terminal too: the encoder escapes ESC and the rest of C0, C1 and the bidi controls stay the values they are, and whoever reads JSON by eye on a terminal has the human mode for it. The MCP one-line summaries (#18) put trace-derived values through the same function where they used `%s`; the ones already using `%q` are unchanged | Trace content is written by the key holder or by any end user whose text the application logs, and a terminal acts on what it is given: OSC 52 rewrites the clipboard (on by default in several terminals), CSI 2J wipes the screen and can hide or forge rows, OSC 8 draws a link to anywhere, OSC 0 retitles the window. A tab or newline inside a name is the milder form of the same thing — the data choosing the layout. Visible escapes rather than stripping or replacement characters, because the person debugging a trace needs to see that the bytes are there. |
| 36 | **2026-09-27** (spec 043 #15–#19) — **Reads are bounded, and so is a tree.** Every `GET` route but the public ones runs under `TRACEPAD_READ_TIMEOUT` and in one of `TRACEPAD_READ_CONCURRENCY` slots: no slot before the deadline is `503` "the server is busy; retry shortly" with `Retry-After: 1`, a read the deadline stops is `503` "the read took longer than …s and was stopped; narrow the time range or the filters" without it. `?tag=` takes at most 50 distinct values; more is `400`. `GET /traces/{id}` and `/traces/last` render at most 10,000 observations and 32 MiB of their own fields — the prefix in `(start_time, id)` order — and say `"observations_omitted": N` when they cut; an observation whose parent was left out renders at the root, as an orphan does. The tree is at most 100 levels deep: an observation at depth 101 is detached to the root with its children, as #30 detaches a cycle's entry. This amends the edge case "structure is never truncated": the budget still never truncates it | A read had no deadline and no limit, and a tree no bound at all: its observations grow across exports without limit, each carries maps no budget counts, and a chain five thousand deep rendered in seconds and then failed every parser. The reasoning is spec 043's. |

## API contract

Auth, error shape, strict query params: per specs 001–003 (unknown parameter
⇒ 400, spec 003 #21). All list endpoints: `limit` (default 50, max 500) +
opaque `cursor` (spec 003 #18).

### Traces

`GET /api/v1/traces` — filters: `from`/`to` (RFC 3339, half-open, on
`timestamp`), `environment`, `user_id`, `session_id`, `name`, `tag`
(repeatable, AND), `status` (`error` = error_count > 0, `ok` = 0),
`min_cost`. `?fields=` selects top-level fields of each row. Newest first.
Response: `{"traces": […], "next_cursor": …}` — rows carry the aggregate
columns, never payloads.

`GET /api/v1/traces/{id}` — trace fields + `"observations"` as a nested tree
(Decision 5); each observation carries its row fields; `?expand=io` per
Decision 6. `GET /api/v1/traces/last` — Decision 7; 404 when nothing
matches.

`GET /api/v1/observations/{id}/io?trace_id=…` — full `input`, `output`,
`metadata` (Decision 3). `trace_id` is optional; without it, if the 16-hex
span id appears in more than one trace of the project, 409 listing candidate
trace ids (truncation markers always embed `trace_id`, so agent flows never
hit the 409).

### Sessions, stats, prompts diff

`GET /api/v1/sessions/{id}` — `{"id", "trace_count", "total_cost",
"error_count", "first_seen", "last_seen", "traces": [list rows]}`, traces
paginated with the same cursor as the trace list.

`GET /api/v1/stats` — filters `from`/`to`/`environment`; `group_by` per
Decision 8. Buckets: `{"key", "count", "error_count", "total_cost",
"latency_ms": {"p50", "p95"}}`. Cost is summed only over `provided_cost`
rows; absent cost stays absent (spec 002 #14), never zero.

`GET /api/v1/prompts/{name}/diff?from=N&to=M` — Decision 21:
`{"name", "from", "to", "diff": "<unified>"}`; 404 on unknown versions.

### Meta

`GET /api/v1` — `{"endpoints": [{"method", "path", "description"}, …]}`.
`GET /api/v1/openapi.json` — Decision 9. `GET /api/v1/system` — Decision 10.

## CLI contract

```
tracepad traces ls   [--env …] [--error] [--since 1h] [--user …] [--session …] [--limit N]
tracepad traces show <id> [--full]         # --full = ?expand=io with a large budget
tracepad traces last [--error] [--full]
tracepad tail        [--env …] [--error] [--interval 2s]
tracepad sessions show <id>
tracepad scores ls   [--trace …] [--name …] [--since …]
tracepad prompts ls | get <name> [--label …|--version N] | push <name> [--file …] [--label …] | diff <name> --from N --to M
tracepad stats       [--group-by day] [--since …]
tracepad system
```

Connection: `TRACEPAD_URL` (default `http://localhost:4318`),
`TRACEPAD_API_KEY`; `--url`/`--key` override. Output per Decision 12.
`--since` accepts Go durations (`1h`, `30m`) and RFC 3339.

## MCP contract

Transport and protocol per Decisions 14–15; tools per Decisions 16–18;
`TRACEPAD_MCP=off` disables `/mcp` on the server. Tool inputs mirror their
endpoint's query parameters (same names, same semantics); outputs are the
endpoint's JSON as `structuredContent` plus a short text summary in
`content`. Truncation markers pass through untouched, and each marker's
`observation_id`/`trace_id` pair is exactly what `get_observation_io` takes —
the tool-side expansion path (Decision 17).

## Data contract (schema 0004)

No new tables. Index changes only:

```sql
DROP INDEX idx_traces_timestamp;
CREATE INDEX idx_traces_timestamp ON traces(project_id, timestamp DESC, id DESC);
CREATE INDEX idx_observations_span ON observations(project_id, id);
```

## Testing

1. **Handler tests with golden responses** — every endpoint, including
   budget/truncation behavior (adversarial multi-MB payloads; response size
   asserted ≤ budget), cursor walks at `limit=1`, `?fields=`, 409 on
   ambiguous observation id, EXPLAIN QUERY PLAN asserting the keyset seek
   (Decision 4, method of spec 003 #25).
2. **e2e** — seed through real OTLP ingest (spec 002 fixtures), read back
   through every endpoint; OpenAPI↔router parity test (Decision 9).
3. **CLI** — golden tests for both output modes (TTY simulated), exit codes,
   `tail` against a server ingesting concurrently.
4. **MCP** — go-sdk client in-process over both transports: every tool
   called, `structuredContent` asserted byte-equal to the corresponding API
   response (Decision 16); stateless HTTP verified (no session header, fresh
   connection per call); tools/list asserted deterministic with cache
   fields.
5. **Race/stress** — existing suites stay green; `tail` and list reads race
   the ingest writer under `-race`.

## Edge cases

- **Empty project**: lists return empty arrays, `traces/last` 404s with a
  message naming the filters; stats return zero buckets, never fabricated
  rows.
- **Budget smaller than one skeleton response**: structure is never
  truncated, only payload previews — the floor (4 KiB) guarantees the
  skeleton fits or the request 400s honestly. The tree has bounds of its
  own since Decision 36.
- **Trace with observations still arriving**: reads see the committed state;
  the tree renders whatever parent/child rows exist (orphans render at the
  root with their `parent_observation_id` intact).
- **`?fields=` naming an unknown field**: 400 (spec 003 #21 lineage).
- **MCP client on 2025-11-25**: SDK negotiates the stateful session
  transparently; tool behavior identical.
- **MCP request with `params` missing or `null`**: served as if `params`
  were `{}` where the protocol makes them optional; where it requires them,
  refused — by the SDK's transport with a 400 when `params` is missing, with
  a JSON-RPC error when it is `null`. Never a crash (Decision 34).
- **CLI against an older/newer server**: version skew reported by comparing
  CLI version with `GET /api/v1/system`; mismatch is a stderr warning, not
  an error.

## Config additions

| Env | Default | Meaning |
|---|---|---|
| `TRACEPAD_RESPONSE_BUDGET_BYTES` | `51200` | Default response budget (Decision 2) |
| `TRACEPAD_MCP` | `on` | Serve MCP at `/mcp` |
| `TRACEPAD_URL` | `http://localhost:4318` | (client commands) server to talk to |
| `TRACEPAD_API_KEY` | — | (client commands) key for requests |

## Out of scope (later specs)

Admin API (projects/keys/retention CRUD, `DELETE …/users/{id}/data`) and the
retention sweeper (005); search/FTS and the MCP `search` tool; UI (§8);
`stats` rollup table; `tracepad remap` / `export --otlp`; push-based `tail`
(SSE/WS); MCP resources/prompts primitives, tasks extension, MCP Apps;
OpenTelemetry self-instrumentation beyond Decision 22.
