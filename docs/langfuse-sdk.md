# Coming from the Langfuse SDK

Tracepad accepts traces from the Langfuse SDK on the route the SDK already
uses, `POST /api/public/otel/v1/traces`. An application that sends to Langfuse
today sends to Tracepad by changing its base URL and its keys. The code stays
the same.

What moves across is everything the SDK *exports*: traces, observations,
payloads, usage, cost, and the images it uploads through its media channel.
Those two routes, `/api/public/otel/v1/traces` and `/api/public/media`, are the
only Langfuse paths Tracepad serves. What does not move is anything the SDK
*asks Langfuse's REST API for*. Scores, prompts, datasets and reads go through
Tracepad's own API instead, and the table below lists the equivalent for each.

## The switch

1. Run Tracepad ([quickstart](quickstart.md)). The first start prints a key
   that holds `ingest`, which is everything an application needs.
2. Point the SDK at it:

    ```sh
    export LANGFUSE_BASE_URL=http://localhost:4318
    export LANGFUSE_HOST=http://localhost:4318   # the older name, for older SDKs
    export LANGFUSE_PUBLIC_KEY=tp-pk-…
    export LANGFUSE_SECRET_KEY=tp-sk-…
    ```

    **Set `LANGFUSE_BASE_URL`, not only `LANGFUSE_HOST`.** The Python SDK 4.x
    reads `LANGFUSE_BASE_URL` first and keeps `LANGFUSE_HOST` as a deprecated
    fallback. The JavaScript SDK 5.x (`@langfuse/otel`) reads `LANGFUSE_BASE_URL`
    and never reads `LANGFUSE_HOST`. With only the old name set, a Node
    application keeps sending to Langfuse's cloud, its default. A `base_url`
    (Python) or `baseUrl` (JavaScript) passed in code beats both variables, so
    check for one in the code as well. The server's first-start output and the
    key dialog still print the old name, so add the new one beside it.

3. Run the application and open the interface. The trace list fills as the
   SDK flushes.

The public key and the secret key are a Tracepad key pair. The SDK sends
them as Basic auth, and the secret key alone also works as a bearer token
([ingest.md](ingest.md#authentication)). A key for an application should hold
`ingest` and nothing else. Mint one in the interface under Settings → Project →
API keys, where `ingest` is ticked by default. From a shell, use the admin token
and the project's id ([admin.md](admin.md#keys)):

```sh
TRACEPAD_API_KEY=$TRACEPAD_ADMIN_TOKEN \
  tracepad keys create --project <project id> --scope ingest --name "my app"
```

CI runs the Langfuse Python SDK 4.14.5 against the server, in
[`scripts/smoke`](../scripts/smoke). The variable names above were read from
the source of that release and of `@langfuse/otel` 5.11.1 on 2026-10-04. The
JavaScript SDK exports to the same route, but no test runs it
([What beta means](../README.md#what-beta-means)).

## What carries over

Tracepad maps the `langfuse.*` attributes listed in
[ingest.md's table](ingest.md#what-tracepad-reads-from-your-spans) into its own
fields. Any attribute it does not map, `langfuse.experiment.*` among them, is
kept in the observation's metadata, so nothing is dropped. In SDK terms:

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

These calls go to Langfuse's REST API: `/api/public/scores`, the prompt,
dataset and trace routes, and the rest of `/api/public/…` apart from the OTLP
and media routes above. Tracepad does not serve them, so against Tracepad they
get a `404`.

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
