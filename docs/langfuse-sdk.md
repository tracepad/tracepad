# Coming from the Langfuse SDK

Tracepad accepts traces from the Langfuse SDK on the route the SDK already
uses, `POST /api/public/otel/v1/traces`. An application that sends to Langfuse
today sends to Tracepad by changing its host and its keys. The code stays the
same.

What moves across is everything the SDK *exports*: traces, observations,
payloads, usage, cost, and the images it uploads. What does not move is
anything the SDK *asks Langfuse's REST API for*. Scores, prompts, datasets and
reads go through Tracepad's own API instead, and the table below lists the
equivalent for each.

## The switch

1. Run Tracepad ([quickstart](quickstart.md)). The first start prints a key
   that holds `ingest`, which is everything an application needs.
2. Point the SDK at it:

    ```sh
    export LANGFUSE_HOST=http://localhost:4318
    export LANGFUSE_PUBLIC_KEY=tp-pk-…
    export LANGFUSE_SECRET_KEY=tp-sk-…
    ```

3. Run the application and open the interface. The trace list fills as the
   SDK flushes.

The public key and the secret key are a Tracepad key pair. The SDK sends
them as Basic auth, and the secret key alone also works as a bearer token
([ingest.md](ingest.md#authentication)). A key for an application should hold `ingest`
and nothing else. Mint one under Settings → Project → API keys, or with
`tracepad keys create --scope ingest` ([admin.md](admin.md#keys)).

CI runs the Langfuse Python SDK against the server at one pinned version, in
[`scripts/smoke`](../scripts/smoke). Other versions and the JavaScript SDK
export to the same route but are not part of that test
([What beta means](../README.md#what-beta-means)).

## What carries over

Every `langfuse.*` attribute the SDK writes is read
([ingest.md](ingest.md#what-tracepad-reads-from-your-spans)):

| From the SDK | In Tracepad |
|---|---|
| `propagate_attributes(trace_name, user_id, session_id, tags, metadata, environment)` | The trace's name, user, session, tags, metadata and environment. Each one is a filter, and sessions and users get their own screens. |
| `release`, `version` | The `release` and `version` filters. |
| `as_type=` (`generation`, `agent`, `tool`, `chain`, `retriever`, …) | The observation's type, with all ten values kept as sent. `?type=tool` filters on it. |
| `model`, `model_parameters` | The model and its parameters. The model feeds the cost and token breakdowns. |
| `input`, `output`, `metadata` | The payloads, shown in the interface's JSON viewer and covered by full-text search. |
| `usage_details` | Token counts, every class kept under the name the SDK sent. |
| `cost_details` | The cost, summed into the trace, the session, the user and the statistics. |
| `level`, `status_message` | Errors: the trace's error count and the `status=error` filter. |
| `completion_start_time` | Time to first token. |
| A generation linked to a prompt (`prompt=`) | The prompt name and version on the observation, and the `prompt=name@version` filter. |
| Images in an input or output | The SDK's media upload channel is served, so each image is stored once and shown in the trace view ([media.md](media.md#the-langfuse-sdks-media-channel)). |

Two things work differently:

- **Cost is what the SDK sends.** Tracepad keeps no price table and never
  estimates a cost from the model and the tokens. A generation without
  `cost_details` counts as no cost. Pass the cost your provider reported, as
  shown in [ingest.md](ingest.md#where-the-price-comes-from).
- **Eval runs link through Tracepad's attributes.** `langfuse.experiment.*` is
  kept in metadata and links nothing. To tie a trace to a dataset run, set
  `tracepad.run_id` and `tracepad.item_id` on the trace's root observation
  ([datasets.md](datasets.md)).

## What goes through Tracepad's API instead

These calls go to Langfuse's REST API under `/api/public/…`, which Tracepad
does not serve. Against Tracepad they get a `404`.

| Langfuse SDK call | In Tracepad |
|---|---|
| Scores (`create_score`, `score_current_trace`, …) | `POST /api/v1/scores`, or `score()` in the [Python](sdk-python.md#scores), [Node](sdk-js.md) and [Go](sdk-go.md) packages ([scores.md](scores.md)) |
| Prompt management (`get_prompt`, `create_prompt`) | `GET` and `POST /api/v1/prompts`, or `prompt()` in the packages ([prompts.md](prompts.md)) |
| Datasets and experiments | `/api/v1/datasets` and `/api/v1/runs`, or the eval harness in the packages ([datasets.md](datasets.md)) |
| Reading traces back (`api.trace.get`, `api.trace.list`, …) | The read API under `/api/v1`, the CLI or MCP ([api.md](api.md)). The read API has its own shape: flat JSON with cursors. |

Both libraries can run in one application. The Langfuse SDK keeps tracing,
and the `tracepad` package handles prompts, scores and datasets against the
same server. Initialise it with `export=False` so that each span is sent
only once ([sdk-python.md](sdk-python.md#init)).

## The way back

Tracepad keeps every export body exactly as the SDK sent it. `tracepad export
--otlp` replays that archive into any OTLP receiver, including Langfuse's own
endpoint, so the move is not one-way ([export.md](export.md)). In that command
the receiver's keys are Langfuse's `pk-lf-…`/`sk-lf-…` pair. The `LANGFUSE_*`
variables on a machine that sends to Tracepad hold Tracepad's keys.
