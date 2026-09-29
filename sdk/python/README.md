# tracepad

The Python package for [Tracepad](https://github.com/tracepad/tracepad) — a
lightweight, self-hosted, OTLP-native store and viewer for LLM and agent
traces.

It is a thin layer over the OpenTelemetry SDK: it owns no transport, no
batching and no context propagation, and it wraps no provider client.
Everything it does is reachable with `opentelemetry-sdk` and `curl` — this is
the one line that points an application at the store, and the ten lines after
it that a person writes the same way every time.

```sh
pip install tracepad
```

Python 3.10+. Two dependencies: `opentelemetry-sdk` and
`opentelemetry-exporter-otlp-proto-http`.

## Point an application at a store

```python
import tracepad

tracepad.init()  # or init(host="http://localhost:4318", key="tp-sk-…")
```

`TRACEPAD_URL` and `TRACEPAD_API_KEY` are the two variables it reads;
`TRACEPAD_ENVIRONMENT`, `TRACEPAD_RELEASE` and `TRACEPAD_EXPORT_TIMEOUT` are the
ones it can also use. If
the application already has a `TracerProvider` — FastAPI instrumentation,
another SDK — `init` adds an exporter to it rather than replacing it.

## Three lines

```python
@tracepad.observe                      # a span, with the arguments and the return value
def answer(question: str) -> str:
    tracepad.update_trace(user_id="u-42", tags=["support"])

    with tracepad.generation("chat", model="gpt-4o-mini") as call:
        response = client.chat.completions.create(model="gpt-4o-mini", messages=…)
        call.end(response=response)    # model, usage, and the cost as charged

    tracepad.score("helpful", 1)       # against the trace in flight
    return response.choices[0].message.content
```

A prompt by label, cached for as long as the server says and served stale when
the server is away:

```python
support = tracepad.prompt("support-answer", label="production")
messages = support.compile(product="Tracepad")
```

## What it does not do

No provider-client wrapper and no auto-instrumentation: OpenLLMetry and the
OpenTelemetry GenAI instrumentations are the answer there, and they work
because the transport is shared. No price table either — the cost recorded is
the one the provider charged, and what cannot be read is not sent.

Full documentation:
[docs/sdk-python.md](https://github.com/tracepad/tracepad/blob/main/docs/sdk-python.md).

Apache-2.0.
