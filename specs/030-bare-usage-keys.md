# Spec 030 — Usage from bare token keys: Claude Code and friends

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> Claude Code exports its sessions as OpenTelemetry spans: one
> `claude_code.interaction` per prompt, an `llm_request` per API call, a
> `tool` per tool call. Pointed at a tracepad server it lands whole — the
> tree, the model, the session id, the version — with one hole: the
> token counts. Claude Code writes them as `input_tokens`, `output_tokens`,
> `cache_read_tokens` and `cache_creation_tokens`, bare, and the mapping
> reads usage only from `gen_ai.usage.*` or Langfuse's `usage_details`, so
> four numbers that are on the span end up in metadata and the usage
> panel says nothing. This spec adds the bare spellings as a third usage
> source, and writes the page that tells a Claude Code user which three
> variables to set.

---

## Overview

Deliverables, one PR (the last commit flips the status):

- The mapping reads usage from **bare token keys** when neither
  `langfuse.observation.usage_details` nor any `gen_ai.usage.*` count is
  present (Decision 1).
- `docs/ingest.md`: the mapping table row for usage names the third
  source; a new **Claude Code** section with the environment block that
  makes every session export to tracepad (Decision 3).
- A synthetic fixture shaped like a Claude Code interaction — root, two
  `llm_request` generations, one `tool` span with its two children —
  exercised through the OTLP path end to end (Decision 4).

Not here: cost estimation from a price table (spec 002 #14 stands: cost
is recorded, never estimated — the question is open for the owner, see
Out of scope); aggregating tokens into `/stats`; mapping Claude Code's
`duration_ms`, `stop_reason`, `tool_name` or `user_prompt_length` to
anything beyond metadata, where they already are.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-10** — `mapUsage` gains a third source, consulted only when the first two yield nothing: the bare keys `input_tokens`, `output_tokens`, `total_tokens`, `cache_read_tokens`, `cache_creation_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`, `prompt_tokens`, `completion_tokens`, `reasoning_tokens`. Each numeric value is claimed and stored under the **key as sent**, the way `gen_ai.usage.*` keys keep their suffix; a non-numeric value is left in metadata. Precedence: `usage_details` object, else any `gen_ai.usage.*` count, else the bare keys — sources are not merged | The first two sources are the two conventions the product promised (spec 002 #19's chain rule); the bare spellings are what Claude Code, the Anthropic SDK's own `usage` object and OpenAI-style clients actually put on a span when nobody normalised them. Keeping the key as sent is what the existing source does and what the usage panel renders; renaming `cache_read_tokens` to something canonical would be a fourth vocabulary. No merging, because an exporter that sends both `gen_ai.usage.input_tokens` and `input_tokens` is describing one number twice, and the standard spelling should win whole. |
| 2 | **2026-09-10** — The list in Decision 1 is a constant in `rules.go` beside the other chains, documented in `docs/ingest.md`'s mapping table, and grows only by a Decision here | Every attribute the mapping reads is named in the table (spec 002's contract); a bare word like `input_tokens` is exactly the kind of key that collides with somebody's unrelated attribute, so the set is closed and visible rather than a prefix scan. |
| 3 | **2026-09-10** — `docs/ingest.md` gets a **Claude Code** section: the five variables (`CLAUDE_CODE_ENABLE_TELEMETRY=1`, `CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1`, `OTEL_TRACES_EXPORTER=otlp`, `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf`, `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer <key>`) as a shell block and as the `env` object for `~/.claude/settings.json`, a note that tracepad speaks HTTP and not gRPC so the protocol line is not optional, `OTEL_RESOURCE_ATTRIBUTES=deployment.environment=…` as the way to keep those traces in their own environment, and one paragraph on what arrives (interaction → llm_request / tool → blocked_on_user, execution; tokens and cache counts; no prompt text — Claude Code redacts it on its side; the user id is a hash) | The feature exists the moment the three variables are set; the page is what turns "it happens to work" into a supported path, and the two facts people will hit first — gRPC is not served, prompt text will not appear — are the ones the page has to say out loud. |
| 4 | **2026-09-10** — The fixture is **synthetic**, written by hand in `testdata/` to the shape Claude Code emits (span names, `span.type`, `gen_ai.system`, `model`, the four token keys, `tool_name`, `gen_ai.tool.call.id`, `stop_reason`, `success`, scope `com.anthropic.claude_code.tracing`), with invented ids and no account, email or organization attributes | The public tree takes no raw traffic (workspace rule 4): a live Claude Code span carries the account's email and organization id. The shape is what the test protects; the values are nobody's. |
| 5 | **2026-09-10** — "Numeric" in Decision 1 is what `asNumber` has meant since spec 002: an int, a double **or a decimal string**. So `input_tokens = "10"` is a count and is claimed, and the Testing line about "a bare key with a string value" is about a value that is not a number (`"many"`), which stays in metadata. Both readings are unit-tested | The mapper has one definition of a number and this source does not get a second: an exporter that stringifies its attributes is not sending a different fact, and `gen_ai.usage.*` has read decimal strings all along. Recorded because the Testing line reads as the opposite on its own. Claude Code 2.1.267 sends them as OTLP ints, which the fixture reproduces; the string form is what an SDK-less HTTP client tends to produce, and it is a real shape either way. |

## Application contract

The usage panel on the observation shows the four Claude Code counts as
they were sent. Trace-level `total_cost` stays absent for these traces
(no cost was sent). Nothing else in the interface changes.

## Testing

- Unit: an attribute set with only bare keys yields usage with those keys
  and claims them (they are absent from metadata); with `gen_ai.usage.*`
  present the bare keys are ignored and stay in metadata; with
  `usage_details` present both are ignored; a bare key with a string
  value is not claimed; `total_tokens` alone is enough.
- OTLP end to end: the synthetic Claude Code fixture through `/v1/traces`
  → `GET /api/v1/traces/{id}` shows two generations with model
  `claude-…` and usage `{input_tokens, output_tokens,
  cache_read_tokens, cache_creation_tokens}`, one `tool` span with two
  children, session id and release on the trace.
- Docs: the anchor checker passes on the new section; the `env` block is
  valid JSON (a test parses it out of the page, the way the CLI usage
  lines are parsed).
- Live: one `claude -p` with the block from the doc against a scratch
  server; the observation panel shows the four counts.

## Out of scope

- **Cost for Claude Code** — needs a price table, which spec 002 #14
  refused on principle (routing makes slug prices wrong). If the owner
  wants estimated cost as an opt-in for known Anthropic models, that is a
  revision of 002 #14 with its own spec, including cache read and cache
  creation rates, which dominate Claude Code's traffic.
- Token totals in `/stats` and the Stats screen.
- Mapping Claude Code's logs/events signal (`OTEL_LOGS_EXPORTER`) —
  tracepad ingests traces only.
