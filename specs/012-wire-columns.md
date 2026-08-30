# Spec 012 — What the wire already carries: types, timings, releases, prompt links, provenance

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> The SDKs already send more than the model keeps. Time to first token, the
> release that produced a trace, the prompt version a generation ran with,
> the kind of step an observation is — all of it arrives on every export and
> lands in `metadata`, where nothing can filter on it, sort by it or show it
> in a column. This spec promotes those facts to columns while the schema is
> still free to change, and gives every attribute that stays in `metadata`
> its origin, so a resource attribute and a span attribute of the same name
> stop overwriting each other.

---

## Overview

Deliverables, all in one PR (REPOS §2):

- **Observation type widened** to the Langfuse vocabulary: `span`,
  `generation`, `event`, `agent`, `tool`, `chain`, `retriever`, `guardrail`,
  `evaluator`, `embedding`. The tree shows the kind of each step; the
  listing can be asked for traces that contain one (`type=tool`).
- **Time to first token**: `completion_start_time` on the observation,
  `ttft_ms` derived on the observation and aggregated on the trace; a column
  in the trace listing and in the observation panel.
- **Release and version** on the trace, from `langfuse.release` /
  `service.version` and `langfuse.version`; a filter, a listing field, a
  `group_by` for the statistics.
- **Prompt link**: `prompt_name` and `prompt_version` on the observation; a
  filter `prompt=name[@version]` that answers "which traces ran this
  prompt", and a badge in the observation panel that leads there.
- **Payload sizes** on the observation (`input_bytes`, `output_bytes`), read
  from the payload table — no schema change, one more join.
- **Provenance in metadata**: unmapped resource attributes land as
  `resource.<key>`, unmapped scope attributes as `scope.<key>`, and the
  instrumentation scope's own name and version as `scope.name` /
  `scope.version`. Span attributes keep their bare names.
- The same fields and filters in the CLI and the MCP tools; the mapping
  table in `docs/ingest.md` grows the new rows.

Not here: sorting by anything but time (spec 009), a prompts screen in the
UI, `tracepad remap`, backfilling databases written before this migration,
`langfuse.internal.*` and `langfuse.experiment.*` (the latter belongs to the
datasets spec).

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-08-30** — This spec exists because the public release is deferred until after iteration 2 (design §12, amendment of 2026-08-30): every change here touches the schema or the stored shape of data, and none of them needs a backfill because no database exists outside the developers' machines. `tracepad remap` stays out of scope; a database migrated from schema 0006 keeps its collapsed types and empty new columns | A column added after the release costs a migration on data we do not own, and a migration that must fill the column from raw bodies is the expensive kind (spec 002 #9 makes it *possible*, not free). Before the release the same column costs one `ALTER TABLE`. The order of work follows from that: schema first, everything else after. |
| 2 | **2026-08-30** — `observations.type` is widened to the ten-value Langfuse vocabulary; the mapper stops collapsing (spec 002 #21 is superseded) and an explicit type is stored as sent. `embedding` counts as a model call wherever a reader today selects `generation` — cost, usage, model breakdowns, latency by model | The three-value set was honest for a column that had a CHECK, and the spelling was preserved in metadata precisely so that this widening could happen later (#21's own words). The kinds are what an agent trace is *about* — "the tool call failed", "the guardrail fired" — and a column that says `span` for all of them says nothing. `embedding` keeps the meaning #21 gave it: it is a call to a model with usage and cost, and every aggregate that sums model calls must not lose it when the spelling changes. The heuristics for spans that carry no explicit type are unchanged: a model attribute means `generation`, a zero-duration childless span is an `event`, the rest are `span`. |
| 3 | **2026-08-30** — `observations.completion_start_time` (Unix nanoseconds, nullable) from `langfuse.observation.completion_start_time`, accepted as an RFC 3339 string — including the SDK's double-JSON-encoded form, one layer of quotes removed — or as an integer of nanoseconds; any other shape stays unclaimed in metadata. The observation's `ttft_ms` is `completion_start_time − start_time`; the trace's `ttft_ms` is the earliest `completion_start_time` among its observations minus the trace's `timestamp`, `null` when no observation carries one. Both are stored as computed — no clamping, no clock correction (spec 002 #4) | Time to first token is the latency a person actually waits for, and it is the first number anybody asks about when a pipeline "feels slow"; the SDK sends it on every generation and we drop it. Storing the timestamp rather than the difference keeps the raw fact (the difference is one subtraction in the query); the trace-level aggregate is defined from the trace's own start because that is the moment the user's wait began, and it is recomputed with the other aggregates inside the ingest transaction (spec 002 #22). The double-encoded string is what the SDK 4.7 emits on the wire (verified 2026-08-30); accepting it is the difference between a feature and a metadata entry. |
| 4 | **2026-08-30** — `traces.release` and `traces.version` (nullable text): `release` from `langfuse.release`, then the resource's `service.version`; `version` from `langfuse.version`. `release` is a listing filter (exact), a listing field, and a `group_by` value for `/stats`; `version` is a filter and a field. Neither is set from the observation-level `langfuse.observation.version`, which stays in metadata | "Did the deploy break it" is a filter, and a filter needs a column. `service.version` is the OTel resource attribute every plain-OTel app already sets, so the fallback makes the release filter work for people who never heard of the Langfuse dialect. `version` is kept as its own column because the SDK distinguishes them — release is the deployment, version is the trace's logic — and merging them would lose that. Release goes into the statistics because the question it answers ("did cost or latency move with the release") is a chart, not a list. |
| 5 | **2026-08-30** — `observations.prompt_name` and `observations.prompt_version` (nullable) from `langfuse.observation.prompt.name` / `.prompt.version`; a listing filter `prompt=<name>` or `prompt=<name>@<version>` that keeps traces one of whose observations carries it. **No foreign key** to `prompts`: the columns record what the client said, whether or not this store manages that prompt | Spec 003 gave the store a prompt registry and spec 002 an observation model, and nothing joins them: a prompt page cannot say what it produced, and a bad answer cannot say which prompt version wrote it. The link is data the SDK already sends when a prompt was fetched by name. It is not a foreign key because the client may manage prompts elsewhere and still label its generations; a label that points at nothing is still a label. The UI resolves it best-effort: the badge links to the filtered listing, which needs no registry. |
| 6 | **2026-08-30** — `input_bytes` and `output_bytes` on the observation in the API, read from `payloads.size_raw` through the join the observation read already makes; no column is added | The sizes exist; they are one select away. A denormalized copy would be a column to keep in step for a number that is never filtered on (ordering by anything but time is out of scope, spec 009). What the sizes are for is the panel — "12 KB in, 3 KB out" — and the API field is where the panel gets it. |
| 7 | **2026-08-30** — Provenance in metadata: an attribute nobody claimed lands in the observation's metadata under `resource.<key>` when it came from the Resource, `scope.<key>` when it came from the InstrumentationScope, and under its bare key when it was on the span. The scope's `name` and `version` fields land as `scope.name` and `scope.version`. The **mapping chains keep reading all three levels by bare key** — `deployment.environment` on the resource still sets the environment. The spelling is ours (`resource.`, not the reference platform's `resourceAttributes.`) | Today `mapping.go` merges the three levels into one flat namespace and `service.name` from the resource is indistinguishable from a span attribute called `service.name`; whichever merged last wins and the other is gone, which breaks the one invariant the mapper promises (design §7: nothing is lost). Prefixing the origin of what *falls through* keeps every value and says where it came from, and leaves the mapping chains alone — a rule that wants a resource key should not have to know it is one. The scope's name and version are not attributes at all in OTLP, and they are the answer to "which SDK sent this" — the fact the reference platform's SDK-name facets are built on. The short spelling is chosen because this is a metadata contract of our own, not a compatibility promise (design §6.4). |
| 8 | **2026-08-30** — Two filters over observations join the listing as `EXISTS` subqueries, each backed by an index: `type=<kind>` on `idx_observations_type(project_id, type, trace_id)` and `prompt=` on `idx_observations_prompt(project_id, prompt_name, prompt_version, trace_id) WHERE prompt_name IS NOT NULL`. `EXPLAIN QUERY PLAN` must show the outer scan still seeking `idx_traces_timestamp`; the ingest benchmark is run with and without the two indexes and both numbers go in the PR | A filter that scans a trace's observations per row is fine on a page of fifty and a disaster on the capped count (spec 009 #4). Two indexes on the biggest table are a write cost, and spec 011 #7 set the precedent for what to do about that: measure it, in the PR, on the store's benchmark fixture. The partial index on `prompt_name` is free for the common case — most observations carry no prompt. |
| 9 | **2026-08-30** — Three clients in one PR: the API fields and filters; `traces ls`/`last`/`tail` gain `--release`, `--version`, `--type`, `--prompt`, the table gains a `ttft` column after latency, and `traces get` prints the type, TTFT and prompt of each observation; the MCP tools `list_traces`, `search`, `get_last_trace` take the same filters and `get_stats` takes `release` in `group_by`. The UI takes the **minimum**: a TTFT column in the trace table, `release`/`prompt`/`type` in the filter popover, the type icon in the tree, TTFT, sizes and the prompt badge in the observation panel, `release` in the statistics' group-by. No new screens | REPOS §2: a capability the server has is a capability all three clients have in the same PR. The UI budget is at 6 399 of 9 000 (spec 010 #7) and this is columns and icons, not screens; a prompts screen is iteration 2's (design §8). |
| 10 | **2026-08-30** — Widening the CHECK rebuilds `observations` (SQLite cannot alter a constraint); the new nullable columns on `traces` are `ALTER TABLE ADD COLUMN`. The rebuild follows the pattern schema 0005 set for `traces_new` and must preserve the `idx_observations_*` set | It is the only way, and it is the last cheap time to do it (Decision 1). |
| 11 | **2026-08-30** (from the implementation) — A trace-level priority chain keeps its priority when the spans of one export disagree: a span resolves the chain and reports the *rank* of the key that won, and a value from a lower-ranked key never overwrites one from a higher-ranked key. At equal rank the later span still wins, which is spec 002 #6 unchanged. Applied to every chained trace field at once — `release`, `version`, `environment`, `user_id`, `session_id`. The rank lives only inside one export: the store's per-field upsert across batches does not carry it, so a root delivered with `langfuse.release` in one batch can still be overwritten by children delivered with only `service.version` in the next | Without this the release column is wrong on the ordinary shape of traffic, which is what fixture 008 showed. A chain is resolved per span but the field belongs to the trace, and the two disagree exactly when the winning key sits on one span while a lower-priority key of the same chain sits on the Resource — where *every* span of the export can see it. Merging by "last non-empty wins" alone then lets the fallback beat the explicit key on any trace longer than one span: `langfuse.release` on the root and `service.version` on the resource stored the latter. The same hazard was already latent for `environment` (`deployment.environment` on the resource), `session_id` and `user_id`; one mechanism for all five is easier to explain than a special case for release. The cross-batch gap is left open deliberately: closing it means a "where did this come from" column beside every trace field, which is not worth it until a real case appears, and the SDK v4 `propagate_attributes` puts the explicit keys on every span of the context, so the ordinary form of traffic does not reach it. |
| 12 | **2026-08-30** (from the implementation) — The filter popover gets a control for `version` too, so the interface offers all four of this spec's filters and not the three the Application contract first listed. The type vocabulary gets a module of its own in the interface, checked against `openapi.json` by a test beside the filter one | The interface has a parity test that reads `openapi.json` and fails when the API accepts a filter the screen does not offer, and `FilterBar` is typed so that a filter with no control does not compile (the UI's own reading of spec 004 #9). A filter in the API and not in the interface is not a smaller deliverable, it is a red gate — and weakening the parity test to allow one would remove the only mechanical guard against the two drifting apart. The spec was wrong, not the test. The same reasoning covers the ten types: the tree draws a kind per row, so a type the API can send and the icon map has never seen would be a hole in the one column somebody is skimming, and the second parity test is what stops that. |
| 13 | **2026-08-30** (from the review of PR #19) — An attribute name may be **bound to one level**, and `service.version` is bound to the Resource: the release chain reads it there and nowhere else, and a span or scope attribute of the same name is neither the release nor claimed — it lands in metadata like any unclaimed attribute. Every other name stays level-agnostic, as Decision 7's chains describe | Decision 4 says "then the resource's `service.version`", and the implementation read the merged view, where a span attribute of that name outranks the resource's. `service.version` on a span is semconv-legal and means the version of whatever that span talked to — a peer service, a model gateway — so a client that stamps one would have had it name this deployment's release. Worse, claiming a key claims it at every level, so the Resource's real `service.version` vanished from metadata at the same time: the one loss Decision 7 exists to prevent, reintroduced on the very key this spec promoted into a chain. Binding the name is a smaller change than making every claim level-aware, and it is the only name in the table whose meaning depends on where it arrived. |
| 14 | **2026-08-30** (from the review of PR #19) — The trace's `ttft_ms` is the earliest `completion_start_time` minus the earliest **positive** start among its observations, and is `null` when none of them reported one — where Decision 3 said "minus the trace's `timestamp`". The two differ on exactly one shape: a trace no span of which said when it started, where `timestamp` falls back to the minimum start of any span (spec 004 #26) and that minimum is zero | A wait is measured from a moment, and a span that never said when it began did not give one. `timestamp` may fall back to zero because a trace must be somewhere in a listing ordered by time; a *duration* measured from the epoch is not a fallback, it is a number some 10^12 milliseconds wrong, and it would sit in a column beside a `latency_ms` that is `null` on the very same rows for the very same reason. Null is the honest answer, and the two aggregates agreeing about a degenerate trace is worth more than the literal wording of Decision 3. |
| 15 | **2026-08-30** (from the implementation) — The `prompt=` grammar has no exceptions: the **version is a run of digits after the last `@` that has a name in front of it**, and every other string is a name, `@` and all. Digits, not "whatever parses as an integer": a sign is not part of a version, so `svc@-1` and `name@+7` are names too — reading them as versions would leave `svc@-1` unfilterable and make `name@+7` answer about a different prompt, which is this same defect at a third position (noted in review of PR #19). The other half of that: **a prompt version counts from one**, refused by the mapper (the name is still recorded, the version stays in metadata, as for any other unusable shape — Decision 5) and by a `CHECK` on the column, so the store cannot hold a version no `prompt=` string can ask for. So `@acme/support`, `team@acme/answer`, `name@latest` and even `@7` are names and all of them filter; `support-answer@7` is the one shape that carries a version. This amends Decision 5: a label is no longer a `400` but a name nothing ran, and the only refusal left is a `prompt=` that names nothing at all, which the handler already treats as absent | A prompt name belongs to somebody else's namespace, and `@` is ordinary inside one — npm-style scopes begin with it, team-scoped registries put it in the middle. Any grammar that reads an `@` as a separator *before* knowing what follows makes a whole family of real names unfilterable, and the interface's own badge, built from whatever the client sent, then links at the API's error. Two rounds of review found the same defect at two positions of the same character (a leading `@`, then an embedded one); the third position would have been next. Decision 5 traded that away for a typo guard on `name@latest`, which was the wrong trade: an empty listing under a filter chip spelling `name@latest` says "nothing ran this" clearly enough, while a name that cannot be typed at all has no workaround. Doing it now costs nothing — no release, no clients (#1). |

## Mapping contract (additions to spec 002's table)

| Target | Keys, in priority order | Notes |
|---|---|---|
| `observation.type` | `langfuse.observation.type` | Known spellings: the ten of Decision 2. An unknown spelling is preserved in metadata and the heuristics apply, as today. |
| `observation.completion_start_time` | `langfuse.observation.completion_start_time` | RFC 3339 string (one layer of JSON quotes stripped if present) or integer nanoseconds. Claimed only when parsed. |
| `observation.prompt_name` | `langfuse.observation.prompt.name` | String. |
| `observation.prompt_version` | `langfuse.observation.prompt.version` | Integer; a non-integer stays in metadata and `prompt_name` is still set. |
| `trace.release` | `langfuse.release`, the resource's `service.version` | Per-field trace upsert as for every trace field (spec 002 #6). `service.version` is read at the Resource only (Decision 13). |
| `trace.version` | `langfuse.version` | |
| `observation.metadata` | the rest, by origin | `resource.<key>`, `scope.<key>`, `scope.name`, `scope.version`, bare `<key>` for span attributes (Decision 7). |

Every chain is consumed only where a rule uses the value (spec 002 #25).

## API contract

**Trace row and trace** gain `release`, `version` (strings, `null` when
unset) and `ttft_ms` (integer milliseconds, `null` when no observation
carried a completion start).

**Observation** gains:

```json
{
  "type": "tool",
  "completion_start_time": "2026-08-30T10:15:03.412Z",
  "ttft_ms": 388,
  "prompt": { "name": "support-answer", "version": 7 },
  "input_bytes": 12480,
  "output_bytes": 3011
}
```

`completion_start_time` is in the representation of `start_time`;
`prompt` is `null` when the observation carries no prompt name;
`input_bytes`/`output_bytes` are `null` when there is no payload. `type` is
one of the ten values of Decision 2.

**Filters** on `GET /api/v1/traces` and `GET /api/v1/traces/last`
(`/sessions` is unchanged):

| Parameter | Meaning |
|---|---|
| `release` | Exact match on the trace's release. |
| `version` | Exact match on the trace's version. |
| `type` | Traces with at least one observation of this type. One of the ten values; anything else is a `400`. |
| `prompt` | `name` or `name@version`: traces with at least one observation that ran this prompt (any version, or that version). The version is a run of digits after the last `@` with a name before it; every other string is a name, `@` and all — `@acme/support`, `team@acme/answer`, `name@latest` and `svc@-1` all filter as names (Decision 15). |

All are one more condition on the same keyset listing (spec 004 #4); a
cursor is valid only with the same filters, as today.

**Statistics**: `group_by=release` on `GET /api/v1/stats`, beside
`environment` and `model`; a trace with no release groups under `null`,
rendered as *(no release)* by the clients.

`openapi.json` grows with all of it (spec 004 #9 parity test).

## CLI contract

`traces ls`, `traces last`, `traces tail`: `--release`, `--version`,
`--type`, `--prompt name[@version]`. The listing table gets a `ttft` column
after `latency`, `-` when null, like every empty cell of that table.
`traces get` prints, per observation, its
type (already printed) in the widened vocabulary, `ttft` when present and
`prompt name@version` when present. `stats --group-by release`. Usage lines
and the flag-parity test (spec 011, CLI contract) cover the new flags.

## MCP contract

`list_traces`, `search`, `get_last_trace`: the four filters, described as
triggers in the tool schema (*"the user names a release, a prompt, or a kind
of step — a tool call, a guardrail — and wants the traces that contain
it"*). `get_stats`: `release` joins the `group_by` enum. `get_trace` and
`get_observation_io` carry the new observation fields by construction.
`docs/mcp.md` updates the parameter lists.

## Data contract (schema 0008)

```sql
-- traces: three nullable columns.
ALTER TABLE traces ADD COLUMN release TEXT;
ALTER TABLE traces ADD COLUMN version TEXT;
ALTER TABLE traces ADD COLUMN ttft_ms INTEGER;
CREATE INDEX idx_traces_release ON traces(project_id, release);

-- observations: rebuilt to widen the CHECK and add three nullable columns.
CREATE TABLE observations_new (
    project_id            TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    trace_id              TEXT NOT NULL,
    id                    TEXT NOT NULL,
    parent_observation_id TEXT,
    type                  TEXT NOT NULL CHECK (type IN (
                              'span', 'generation', 'event', 'agent', 'tool', 'chain',
                              'retriever', 'guardrail', 'evaluator', 'embedding')),
    name                  TEXT,
    start_time            INTEGER,
    end_time              INTEGER,
    completion_start_time INTEGER,
    model                 TEXT,
    model_parameters      TEXT,
    level                 TEXT NOT NULL DEFAULT 'DEFAULT'
                          CHECK (level IN ('DEBUG', 'DEFAULT', 'WARNING', 'ERROR')),
    status_message        TEXT,
    usage                 TEXT,
    cost_details          TEXT,
    provided_cost         INTEGER NOT NULL DEFAULT 0,
    prompt_name           TEXT,
    prompt_version        INTEGER,
    input_id              INTEGER REFERENCES payloads(id),
    output_id             INTEGER REFERENCES payloads(id),
    metadata_id           INTEGER REFERENCES payloads(id),
    PRIMARY KEY (project_id, trace_id, id)
) STRICT;
INSERT INTO observations_new (…) SELECT … FROM observations;
DROP TABLE observations;
ALTER TABLE observations_new RENAME TO observations;
-- every index that existed on observations, recreated, plus:
CREATE INDEX idx_observations_type   ON observations(project_id, type, trace_id);
CREATE INDEX idx_observations_prompt ON observations(project_id, prompt_name, prompt_version, trace_id)
    WHERE prompt_name IS NOT NULL;
```

The schema number is 0008 because 0007 is taken by spec 011 Decision 14
(the search index rebuild), which lands first; the two migrations touch
different tables and are independent.

The trace aggregate query (spec 002 #22) grows `ttft_ms`:

```sql
(MIN(completion_start_time) - MIN(start_time)) / 1000000
  -- over the trace's observations; NULL when MIN(completion_start_time) is NULL
```

The listing with `type` or `prompt` is, in shape:

```sql
SELECT … FROM traces t
 WHERE t.project_id = ? AND … AND EXISTS (
   SELECT 1 FROM observations o
    WHERE o.project_id = t.project_id AND o.trace_id = t.id AND o.type = ?)
 ORDER BY t.timestamp DESC, t.id DESC LIMIT ?
```

## Application contract

- **Trace table**: a `TTFT` column right after `Latency`, formatted like it,
  blank when null. No release column — release is a filter and is shown in
  the trace header of the detail page and the peek panel, beside the
  environment.
- **Filter popover**: `Release` and `Version` and `Prompt` (text, exact;
  `Prompt` accepts `name@version`), `Type` (select over the ten values plus
  *any*). All in the URL like every filter (spec 007). Four, not the three an
  earlier draft of this line named — see #12.
- **Tree**: an icon per type from `lucide-svelte` (spec 006 #3): `Box` span,
  `Sparkles` generation, `Zap` event, `Bot` agent, `Wrench` tool, `Link`
  chain, `Search` retriever, `Shield` guardrail, `Gauge` evaluator, `Layers`
  embedding; the icon has a `title` naming the type.
- **Observation panel**: `TTFT` in the timings block when present;
  `In`/`Out` sizes beside the payload headings; a prompt badge
  `name · v7` that links to `/traces?prompt=name@7`.
- **Stats**: `release` in the group-by select; the *(no release)* series.
- Budget measured at the end (spec 010 #7); the growth is the column, the
  three filter controls, the icon map and the panel lines.

## Testing

- **Go, mapper** (golden fixtures + table tests): each row of the mapping
  contract; the double-encoded `completion_start_time` and the integer form
  both parse, a garbage value stays in metadata; a prompt with a
  non-integer version sets the name and keeps the version in metadata;
  `service.version` on the resource sets `release` and is claimed; all ten
  types round-trip, `embedding` no longer collapses; an unknown type spelling
  still falls to the heuristics.
- **Go, provenance**: a resource `service.name`, a scope `service.name` and
  a span `service.name` all survive into metadata under three different
  keys; `deployment.environment` on the resource still sets the environment
  and does not appear in metadata; `scope.name`/`scope.version` are present
  when the scope has them and absent when it does not; a span's
  `service.version` names no release and stays in metadata beside the
  resource's, which does (Decision 13).
- **Go, aggregates**: trace `ttft_ms` is the earliest completion start
  minus the trace start; `null` when no observation carries one, and `null`
  when none of them said when it started, where `latency_ms` is `null` too
  (Decision 14); recomputed on re-delivery (spec 002 #22).
- **Go, readers that select generations**: every query that tested
  `type = 'generation'` before this spec is listed in the PR description
  and now includes `embedding` (Decision 2), with a test per aggregate on a
  fixture that carries one embedding call.
- **Go, filters and plan**: each new filter alone and combined with the
  keyset; `type` with a value outside the vocabulary is a 400; `prompt`
  reads a version only when it is a number, every other `@` belonging to
  the name (Decision 15); `EXPLAIN QUERY PLAN` shows the keyset seek and
  the two indexes; ingest benchmark with and without the indexes, both
  numbers in the PR.
- **Go, migration**: a schema-0006 database with collapsed types and
  populated observations migrates, keeps every row and every index, and
  answers the new fields as `null`.
- **CLI / MCP**: flag-and-usage parity; `openapi.json` parity; the MCP
  schemas name the new filters and the `group_by` enum.
- **UI**: type icon map covers all ten values (a test that fails when the
  vocabulary and the map disagree); the filter round-trip with the three
  new filters; E2E — filter by type `tool` on a seeded fixture, open the
  panel, see TTFT and the prompt badge, follow it to the filtered listing.
- **Chrome (DoD)**: both themes, 375 px, console clean.

## Edge cases

- `completion_start_time` earlier than `start_time`: stored as sent,
  `ttft_ms` negative (spec 002 #4); the UI shows the number as is.
- A trace whose generations arrive in several batches: `ttft_ms` is
  recomputed on each, like every aggregate.
- `type=generation` filter: matches `generation` only, not `embedding` —
  the *filter* is exact; it is the *aggregates* that treat embedding as a
  model call (Decision 2).
- `prompt=name` and `prompt=name@7` on a trace with two observations of the
  same prompt at versions 6 and 7: the first matches, the second matches,
  `prompt=name@5` does not.
- A resource attribute and a span attribute of the same name, both
  unmapped: both in metadata, `resource.key` and `key`.
- A root span carrying `langfuse.release` and its children carrying only the
  resource's `service.version`: inside one export the rank keeps the explicit
  release (#11). Delivered as two batches — the root first, the children
  after — the second delivery overwrites it, because the stored row does not
  record which key a field came from. Left open deliberately (#11); the SDK's
  `propagate_attributes` puts the explicit keys on every span of the context,
  so ordinary traffic does not reach it.

## Out of scope

- Ordering by TTFT, size or anything but time (spec 009).
- A prompts screen, or a "generations of this prompt" list on the prompt
  API — the filtered trace listing is that list.
- `tracepad remap`; backfilling schema-0006 databases from raw bodies.
- `langfuse.internal.is_app_root`; `langfuse.experiment.*` (datasets spec).
- Observation-level `version`; tool-call attributes (`gen_ai.tool.*`) —
  a later spec, once an instrumentation that sends them is in the fixture
  corpus.
