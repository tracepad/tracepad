# Spec 002 — Trace Data Model & OTLP Ingest

**Status:** ✅ SHIPPED
**Sprint:** August–September 2026

> The core of the product: accept OTLP/HTTP trace exports from any
> OpenTelemetry-instrumented app, map attribute dialects into a
> trace → observations model, and persist it durably in SQLite behind a
> group-commit writer. After this spec ships, a real application emits traces
> into Tracepad by changing one environment variable.

---

## Overview

Deliverables:

- `POST /v1/traces` — OTLP/HTTP protobuf endpoint (gzip supported), plus the
  Langfuse-SDK alias `POST /api/public/otel/v1/traces`.
- Auth: `Bearer <tp-sk>` and `Basic base64(pk:sk)` against project keys.
- Schema 0002: `traces`, `observations`, `payloads`, `raw_batches`.
- Attribute mapping with two dialects: OTel GenAI semconv and `langfuse.*`,
  organized as a priority table; unmapped attributes are preserved.
- Group-commit write pipeline: decode/map in handlers, one writer goroutine,
  ack after fsync.
- Raw request bodies stored (zstd) for future `remap`/`export`.
- Golden-fixture test suite (`testdata/otlp/`) + e2e smoke with real
  instrumentation libraries.

## Decisions log

| # | Decision | Why |
|---|---|---|
| 1 | Transport is OTLP/HTTP **protobuf only**; OTLP/JSON and gRPC are out of scope | Both SDK families we target (OTel default exporters, Langfuse SDK) speak protobuf over HTTP. JSON/gRPC add surface without a single known consumer; revisit on demand. |
| 2 | Both auth schemes on both routes: `Bearer <secret>` and `Basic base64(public:secret)` | One key pair serves native and Langfuse-SDK wire formats (spec 001 #8). Accepting both on both paths means nobody debugs "right key, wrong route". Lookup stays a single `sha256(secret)` index hit either way. |
| 3 | Trace and span IDs stored as lower-case hex TEXT (32 / 16 chars), primary keys composite with `project_id` | Hex is directly readable in every UI/CLI/JSON surface and joins fine; the byte savings of BLOB are irrelevant at our scale (design gives ~100× headroom). `project_id` in the PK isolates tenants and makes cross-project trace-id collisions a non-event. |
| 4 | Timestamps stored as INTEGER Unix **nanoseconds** | OTLP delivers ns; keeping them lossless makes duration math trivial and avoids TZ/format bugs. Rendering as ISO strings is the API layer's job. |
| 5 | Observations are **upserted** by `(project_id, trace_id, id)`, last delivery wins | OTLP exporters retry on timeout/429, so duplicate span delivery is normal, not exceptional. Idempotent ingest by natural key beats dedup bookkeeping. |
| 6 | Trace rows are **derived**, merged per field: non-empty beats empty, later-received non-empty overwrites | In OTel there is no "trace" message — trace-level fields ride on spans (`langfuse.trace.*` may arrive on any span, in any order, across batches). Field-wise merge with last-write-wins mirrors the reference implementation's semantics and is order-tolerant. |
| 7 | Trace-row aggregates (`observation_count`, `error_count`, `total_cost`, `latency_ms`, `timestamp`) updated incrementally in the same transaction | The trace list is the hottest read and must not join observations (design §5.2). Incremental update in the ingest tx keeps it exact without a background job. |
| 8 | Large values live in `payloads`, zstd-compressed above 128 bytes, one row per value | Keeps `traces`/`observations` rows narrow so list scans never touch blobs. Tiny values skip compression (zstd overhead exceeds gains); the `compression` column makes this explicit per row. |
| 9 | Every accepted request body is stored raw (zstd) in `raw_batches`, default on (`TRACEPAD_STORE_RAW=off` to disable) | The insurance policy of the whole design: mapping bugs and SDK convention drift become retroactively fixable (`remap`) instead of data loss, and `export --otlp` gets its source for free. Reference install held 21.7 MB of useful data in 30 days — raw copies are cheap. |
| 10 | Mapping is a **priority table per target field**: explicit `langfuse.*` > OTel GenAI semconv > bare fallbacks (`user.id`, `session.id`, `llm.model_name`, `model`) | Explicit beats conventional beats guessed. The table is data (one Go slice per field), so adding a dialect (OpenInference/OpenLLMetry) is a table edit, not a refactor. |
| 11 | Unmapped attributes are never dropped — they land in the observation's `metadata` | Degradation without loss: a new SDK convention appears as visible metadata immediately, and becomes a mapped field later via table edit + `remap` (#9). |
| 12 | Observation type: explicit `langfuse.observation.type` wins; else a model attribute (any dialect) ⇒ `generation`; else zero-duration span with no children ⇒ `event`; else `span` | Matches observed SDK behavior: Langfuse SDK stamps the type explicitly; plain GenAI instrumentation marks generations by carrying `gen_ai.request.model`. The heuristics only run when nothing explicit is present. |
| 13 | One bad span never rejects the batch: it is skipped in mapped form, counted in OTLP `partial_success`, and still present in the raw body | OTLP semantics expect partial success, and #9 means "skipped" is recoverable. A whole-batch 400 would make one malformed span destroy N−1 good ones. |
| 14 | Cost is stored only when the client provides it (`provided_cost` flag); no price table, no estimation | Design §5.2: slug-based price estimation is systematically wrong for OpenRouter-style routing; the provider's actual figure is the only truth. Absent cost renders as "no data", never as $0. |
| 15 | Ack after commit: handlers decode/map concurrently, submit to a bounded channel, one writer goroutine batches windows (≤50 ms / ≤64 submissions) into single transactions, responds 200 only after fsync | SQLite has one writer; group commit turns that into throughput instead of contention. A 200 to the exporter must mean "on disk" — anything weaker silently loses data on crash. Overflow returns 429 (+ `Retry-After`), which OTLP exporters handle natively. |
| 16 | Request body cap `TRACEPAD_MAX_BODY_BYTES`, default 20 MiB (413 above it) | Default OTel batch processors ship ≤512 spans per export — far below the cap; the cap only stops pathological/malicious bodies from ballooning memory. |
| 17 | `x-langfuse-ingestion-version` is logged and counted, never enforced | Early drift signal (design §7.4). Refusing on an unknown version would break users on newer SDKs for no benefit — raw-first (#9) means we can always catch up retroactively. |
| 18 | **2026-08-26** — The `ExportTraceServiceRequest`/`Response` envelope is coded against the wire format with `protowire`; only `otlp/trace/v1` is imported | `otlp/collector/trace/v1` declares the gRPC TraceService, so importing it pulls `google.golang.org/grpc` and grpc-gateway into a binary that speaks OTLP over plain HTTP only (design §4: every dependency justified). Both messages are one field deep; the generated `ResourceSpans` type still does all real decoding. Skipping a malformed `ResourceSpans` instead of failing the body also falls out of it. |
| 19 | **2026-08-26** — Where the reference implementation reads an attribute the Mapping table does not name, the reference wins and the key is added to the chain | The table was written from documentation; `OtelIngestionProcessor.ts` is the only executable description of what the SDKs emit, and the spec already makes fact outrank the table. Concretely: `langfuse.observation.model.parameters` is the real spelling (the table's `model_parameters` is accepted as an alias), `gen_ai.conversation.id` joins the session chain, `deployment.environment` (pre-1.0 semconv) joins the environment chain, metadata is read both as a JSON object at the bare key and as `<prefix>.*` entries, `gen_ai.input.messages`/`gen_ai.output.messages` join the input/output chains ahead of `gen_ai.prompt`/`gen_ai.completion`, the flattened `gen_ai.prompt.0.content` form is reassembled, and observation levels are normalized through the reference's alias table (`INFO`→`DEFAULT`, `FATAL`→`ERROR`, …). Confirmed against `langfuse` 4.14.5, whose `LangfuseOtelSpanAttributes` emits exactly these names. |
| 20 | **2026-08-26** — `gen_ai.usage.cost` feeds the cost chain, not usage, and `cost_details` is normalized to always carry a `total` | Collecting `gen_ai.usage.*` verbatim would file a price as a token count, which is worse than dropping it. A client-provided price is exactly what #14 wants recorded. The `total` is derived once at map time (summing the components when the client sent no total) so the trace-list aggregate is a single `json_extract`, not a per-reader convention. |
| 21 | **2026-08-26** — Langfuse observation types beyond `span`/`generation`/`event` collapse onto the nearest of the three (`embedding`→`generation`, the rest→`span`), original preserved in metadata | Schema 0002 closes the type set with a CHECK; the SDK's vocabulary is wider (`agent`, `tool`, `chain`, `retriever`, `guardrail`, `evaluator`). Collapsing keeps the column honest, and keeping the original spelling in metadata means a later spec can widen the column and backfill from raw bodies (#9) instead of from nothing. |
| 22 | **2026-08-26** — Trace aggregates are recomputed from the trace's own observations inside the ingest transaction, not accumulated as deltas | #7 asks for exact aggregates in the ingest tx; #5 makes re-delivery routine, and a delta would double-count every retried span. One indexed aggregate query per affected trace is bounded by that trace's span count and is exact by construction. |
| 23 | **2026-08-26** — The ingest writer holds one connection with `PRAGMA synchronous=FULL`; every other connection keeps the `NORMAL` spec 001 set in the DSN | #15 promises a 200 means "on disk". Under WAL, `NORMAL` only syncs at checkpoints, so a power loss could take back an acknowledged export. The setting is per connection and the writer is the only one committing ingest, so the guarantee costs at most one fsync per commit window (≤20/s) and nothing at all for readers. |
| 24 | **2026-08-26** — The fixture corpus is synthesized end to end (`internal/otlptest` builds it, `make fixtures` writes it) until live captures exist | Testing #1 wants structure from live captures with synthesized content; there is no capture corpus yet, and blocking ingest on one would invert the order (the proxy capture needs a running ingest endpoint). The structures are built from the spec and from the SDKs' own attribute definitions, and verified end to end by the real-SDK smoke test, which exports through the actual exporters. When captures arrive they replace the builders; the `.pb` files on disk are the contract either way. |
| 25 | **2026-08-27** (from PR #2 review) — An attribute is consumed only when a rule actually *uses* its value, never when a rule merely inspects it; the losers of a priority chain stay unconsumed | #11 says unmapped attributes are never dropped, and the first implementation broke it in exactly the cases that matter most: an unknown `langfuse.observation.level` spelling, a `cost_details`/`usage_details`/`model_parameters` that did not parse as JSON, and every runner-up in a chain were marked consumed at lookup and then discarded — landing nowhere at all. The reference implementation deletes whole chains, and here we deliberately do not follow it (cf. #19): `gen_ai.response.model` is the model that answered, `gen_ai.request.model` the one that was asked for, and collapsing them loses a fact rather than a duplicate. Regression tests pin each case. |

## API contract

### `POST /v1/traces` (canonical) and `POST /api/public/otel/v1/traces` (alias)

- Request: `ExportTraceServiceRequest` protobuf; `Content-Type:
  application/x-protobuf`; optional `Content-Encoding: gzip`.
- Auth per Decision 2; unknown credentials → 401 `{"error": "unauthorized"}`.
- Success: 200, `ExportTraceServiceResponse` protobuf; `partial_success`
  filled when spans were skipped (Decision 13).
- 400 undecodable body · 413 over cap · 429 writer backpressure
  (`Retry-After: 1`) · 415 wrong content type.
- Empty batch: 200, empty response (per OTLP spec).

## Data contract (schema 0002)

```sql
traces(
  project_id TEXT, id TEXT,                       -- PK (project_id, id); id = 32-hex trace id
  name TEXT, user_id TEXT, session_id TEXT,
  environment TEXT NOT NULL DEFAULT 'default',
  tags TEXT,                                      -- JSON array
  metadata_id INTEGER → payloads,
  timestamp INTEGER,                              -- ns; min(start_time) over observations
  total_cost REAL, latency_ms INTEGER,
  error_count INTEGER NOT NULL DEFAULT 0,
  observation_count INTEGER NOT NULL DEFAULT 0
)
observations(
  project_id TEXT, trace_id TEXT, id TEXT,        -- PK (project_id, trace_id, id); id = 16-hex span id
  parent_observation_id TEXT,
  type TEXT CHECK (type IN ('span','generation','event')),
  name TEXT, start_time INTEGER, end_time INTEGER,-- ns
  model TEXT, model_parameters TEXT,              -- JSON
  level TEXT NOT NULL DEFAULT 'DEFAULT'
       CHECK (level IN ('DEBUG','DEFAULT','WARNING','ERROR')),
  status_message TEXT,
  usage TEXT, cost_details TEXT,                  -- JSON
  provided_cost INTEGER NOT NULL DEFAULT 0,
  input_id INTEGER → payloads, output_id INTEGER → payloads,
  metadata_id INTEGER → payloads
)
payloads(id INTEGER PK, compression TEXT ('none'|'zstd'), size_raw INTEGER, body BLOB)
raw_batches(id INTEGER PK, project_id TEXT, received_at INTEGER,
            dialect TEXT, content_encoding TEXT, body BLOB)  -- zstd of raw request
```

Indexes: `traces(project_id, timestamp DESC)`; `traces(project_id, user_id)`,
`(project_id, session_id)`, `(project_id, environment)`;
`observations(project_id, trace_id)`; `raw_batches(project_id, received_at)`.

All STRICT (spec 001 #7). Payload rows are reference-counted implicitly by
their single owner column; orphan cleanup is the retention stage's concern.

## Mapping

Per-field priority chains (Decision 10). First non-empty attribute wins.

| Target | Priority chain |
|---|---|
| trace.name | `langfuse.trace.name` > root span's name |
| trace.user_id | `langfuse.user.id` > `user.id` |
| trace.session_id | `langfuse.session.id` > `session.id` |
| trace.environment | `langfuse.environment` > `deployment.environment.name` > `'default'` |
| trace.tags | `langfuse.trace.tags` (JSON array) |
| trace.metadata | `langfuse.trace.metadata.*` (key-stripped, merged) |
| obs.type | Decision 12 |
| obs.model | `langfuse.observation.model.name` > `gen_ai.request.model` > `gen_ai.response.model` > `llm.model_name` > `model` |
| obs.model_parameters | `langfuse.observation.model_parameters` > `gen_ai.request.*` (temperature, top_p, max_tokens, …, collected) |
| obs.input | `langfuse.observation.input` > `gen_ai.prompt` |
| obs.output | `langfuse.observation.output` > `gen_ai.completion` |
| obs.usage | `langfuse.observation.usage_details` (JSON) > `gen_ai.usage.*` (input_tokens, output_tokens, and every other `gen_ai.usage.<key>` collected verbatim) |
| obs.cost | `langfuse.observation.cost_details` (JSON) — sets `provided_cost` |
| obs.level | `langfuse.observation.level` > span status ERROR ⇒ `ERROR` |
| obs.status_message | `langfuse.observation.status_message` > span status message |
| obs.metadata | `langfuse.observation.metadata.*` + **every attribute no rule consumed** (Decision 11) |

The full executable table lives in `internal/mapping` with per-rule comments;
semantics for the `langfuse.*` dialect are ported from Langfuse's MIT-licensed
`OtelIngestionProcessor` (NOTICE attribution). **Fixtures are the authority**:
disagreement between this table and a captured real body is resolved by the
body, and the resolution lands here as a new dated decision.

Resource-level attributes are merged into every span's attribute set at lower
priority than the span's own attributes.

## Testing

1. **Golden fixtures** — `testdata/otlp/NNN-name.pb` (sanitized: structure
   from live captures, content synthesized) + `testdata/golden/NNN-name.json`
   (the mapped result rendered through a stable debug encoding). `go test`
   replays every fixture through decode→map and diffs. `-update` flag
   regenerates goldens.
2. **Ingest e2e** — in-process server, real protobuf bodies, asserts rows,
   aggregates, idempotency (double-send), partial success, auth failures,
   429 under a saturated writer.
3. **Real-SDK smoke (CI)** — a pinned `langfuse` (Python) and a pinned
   `opentelemetry-sdk` + GenAI-semconv script export to a running binary;
   assertions via direct DB reads (the read API arrives in a later spec).

## Edge cases

- **Out-of-order and cross-batch spans**: children before parents, trace
  fields on a late span — all fine by Decisions 5/6; the tree is assembled at
  read time.
- **Span with zero trace_id / span_id**: skipped, counted in partial_success.
- **Clock skew**: timestamps are stored as sent; no server-side correction.
- **Unknown resource/scope attributes**: metadata, like any unmapped attribute.
- **Writer stall (slow disk)**: bounded channel fills → 429s; nothing is
  dropped silently; the stall is visible in logs.
- **`TRACEPAD_STORE_RAW=off`**: ingest works; `remap`/`export` honestly
  report there is nothing to replay.

## Config additions

| Env | Default | Meaning |
|---|---|---|
| `TRACEPAD_STORE_RAW` | `on` | Keep raw OTLP bodies (Decision 9) |
| `TRACEPAD_MAX_BODY_BYTES` | `20971520` | Request body cap (Decision 16) |

## Out of scope (later specs)

Scores & prompts APIs (003), native read API + CLI/MCP (004), retention
sweeper & payload orphan cleanup, `tracepad remap` / `export --otlp` (need the
read/admin surface), OTLP/JSON & gRPC transports, OpenInference/OpenLLMetry
dialects, rate limiting beyond writer backpressure.
