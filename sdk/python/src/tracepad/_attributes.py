"""The vocabulary the package writes (spec 017 #3).

The OTel GenAI semantic conventions where a name exists, `tracepad.*` where
none does, and `langfuse.*` never: a span this package produced should mean
the same thing to any OTel backend and to any instrumentation reading beside
it, and the gaps the conventions leave — a trace name, tags, free metadata, an
observation kind, a prompt reference — are ours to name rather than to borrow
from a competitor's dialect.

The `tracepad.*` half of this file is the mapper's table in
`internal/mapping/rules.go`, and `docs/ingest.md` prints both.
"""

from __future__ import annotations

import json
from datetime import datetime, timezone
from typing import Any

# GenAI semantic conventions.
REQUEST_MODEL = "gen_ai.request.model"
RESPONSE_MODEL = "gen_ai.response.model"
REQUEST_PREFIX = "gen_ai.request."
USAGE_PREFIX = "gen_ai.usage."
USAGE_COST = "gen_ai.usage.cost"
INPUT = "gen_ai.input.messages"
OUTPUT = "gen_ai.output.messages"

# Trace-level names the conventions do have.
USER_ID = "user.id"
SESSION_ID = "session.id"
ENVIRONMENT = "deployment.environment.name"
SERVICE_NAME = "service.name"
SERVICE_VERSION = "service.version"

# Tracepad's own, for the facts the conventions have no word for.
TRACE_NAME = "tracepad.trace.name"
TRACE_TAGS = "tracepad.trace.tags"
TRACE_METADATA = "tracepad.trace.metadata"
TRACE_VERSION = "tracepad.trace.version"
OBSERVATION_TYPE = "tracepad.observation.type"
OBSERVATION_LEVEL = "tracepad.observation.level"
OBSERVATION_STATUS_MESSAGE = "tracepad.observation.status_message"
OBSERVATION_METADATA = "tracepad.observation.metadata"
COMPLETION_START_TIME = "tracepad.observation.completion_start_time"
PROMPT_NAME = "tracepad.prompt.name"
PROMPT_VERSION = "tracepad.prompt.version"

# The run link (spec 014 #2): trace-level, stamped on every span of an eval
# by the processor of spec 018, and not a dialect of its own — any harness in
# any language writes these over whatever SDK the application already runs.
RUN_ID = "tracepad.run_id"
ITEM_ID = "tracepad.item_id"

#: The ten kinds an observation may be (`docs/ingest.md`). A spelling outside
#: them is stored in metadata and the span is classified by the mapper's
#: heuristics, so this list warns rather than rejects.
OBSERVATION_TYPES = (
    "span",
    "generation",
    "event",
    "agent",
    "tool",
    "chain",
    "retriever",
    "guardrail",
    "evaluator",
    "embedding",
)

#: The four levels schema 0002 allows.
LEVELS = ("DEBUG", "DEFAULT", "WARNING", "ERROR")


def dumps(value: Any) -> str:
    """Render a payload for an attribute.

    A `str` is sent as it is — a plain-text prompt is a plain-text payload,
    not a JSON string of one (spec 015 #12) — and everything else is JSON with
    `default=repr`, the rule that never raises: a value the encoder cannot
    express is still a string in the trace, and the alternative is a decorator
    that breaks the function it decorates (spec 017 #4).

    `default` alone is not that rule, though. It is never consulted for a
    dict's *key*, and never for a cycle, so a value that reaches the encoder
    with either still raises — on the decorator's path, before the decorated
    function has run at all. `repr` is the floor under it (found in review of
    PR #35): it renders both, cycles as `[...]`.
    """
    if isinstance(value, str):
        return value
    try:
        return json.dumps(value, default=repr, ensure_ascii=False, separators=(",", ":"))
    except (TypeError, ValueError):
        return repr(value)


def scalar(value: Any) -> Any:
    """Render a model parameter or a metadata entry, keeping the types OTLP has
    of its own.

    An `int` outside OTLP's 64 bits is JSON, since protobuf refuses it at
    export and the whole batch with it; and a value that encodes as a JSON
    string — a `datetime`, a `UUID` through `repr` — is that string, not the
    string with its quotes (found in review of PR #83).
    """
    fits = isinstance(value, int) and -(2**63) <= value < 2**63
    if fits or isinstance(value, (float, str)):
        return value
    encoded = dumps(value)
    if encoded.startswith('"'):
        try:
            return str(json.loads(encoded))
        except ValueError:
            pass
    return encoded


def rfc3339(nanoseconds: int) -> str:
    """An instant as the mapper reads it (`docs/ingest.md`, time to first token)."""
    moment = datetime.fromtimestamp(nanoseconds / 1e9, tz=timezone.utc)
    return moment.isoformat(timespec="milliseconds").replace("+00:00", "Z")
