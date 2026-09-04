"""The three exceptions, in the one module every other one may import.

Failure semantics are split by path (spec 017 #9). The tracing path never
raises into application code and logs instead; the REST path — `prompt`,
`flush`, and the harness's clients — raises, because a caller that asked for a
value must not be handed `None` with a log line nobody reads. `init` is both,
so misconfiguration is raised at the call rather than discovered as a 401 in a
log file an hour later.
"""

from __future__ import annotations


class TracepadError(Exception):
    """Anything the REST path could not do."""


class TracepadHTTPError(TracepadError):
    """A non-2xx answer, with the server's own message kept whole."""

    def __init__(self, status: int, body: str) -> None:
        super().__init__(f"tracepad: HTTP {status}: {body.strip()}")
        self.status = status
        self.body = body


class TracepadConfigError(TracepadError):
    """`init` was given no host or no key, and the environment has none."""
