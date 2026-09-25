"""Configuration: the arguments of `init`, then the environment (spec 017 #10).

Standard OpenTelemetry variables — `OTEL_SERVICE_NAME`,
`OTEL_RESOURCE_ATTRIBUTES`, the exporter's and the batch processor's own — are
honoured by the OTel SDK as they are. The package neither reads them nor sets
them: the SDK already implements them, with its own defaults, and a second
implementation would be a second set of answers.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field

from ._errors import TracepadConfigError
from ._log import logger

VERSION = "0.1.0"

#: The exporter's per-export timeout in seconds when nothing names one (spec
#: 042 #3): OpenTelemetry's ten, its retries inside them, is long for a thread
#: that flushes at the end of a request.
EXPORT_TIMEOUT = 5.0


@dataclass(frozen=True)
class Config:
    """Where the store is, who we are to it, and what to call this deploy."""

    host: str
    # Out of the repr: an error tracker renders a frame's locals with it, and
    # every REST call raises from a frame holding this (spec 017 #19).
    key: str = field(repr=False)
    environment: str | None = None
    release: str | None = None

    @property
    def traces_endpoint(self) -> str:
        return f"{self.host}/v1/traces"


def resolve(
    host: str | None = None,
    key: str | None = None,
    environment: str | None = None,
    release: str | None = None,
) -> Config:
    """Build a Config, raising when the two required values are nowhere."""
    host = _pick(host, "TRACEPAD_HOST").rstrip("/")
    key = _pick(key, "TRACEPAD_API_KEY")
    missing = [name for name, value in (("host", host), ("key", key)) if not value]
    if missing:
        raise TracepadConfigError(
            "tracepad: no "
            + " and no ".join(missing)
            + "; pass them to tracepad.init() or set TRACEPAD_HOST and TRACEPAD_API_KEY"
        )
    return Config(
        host=host,
        key=key,
        environment=_pick(environment, "TRACEPAD_ENVIRONMENT") or None,
        release=_pick(release, "TRACEPAD_RELEASE") or None,
    )


def resolve_timeout(argument: float | None) -> float | None:
    """The argument, then TRACEPAD_EXPORT_TIMEOUT, then five seconds — or `None`,
    which leaves the exporter to OpenTelemetry's own variable when one is set."""
    for name, given in (("export_timeout", argument),
                        ("TRACEPAD_EXPORT_TIMEOUT", os.environ.get("TRACEPAD_EXPORT_TIMEOUT"))):
        if given is None or (isinstance(given, str) and not given.strip()):
            continue
        try:
            seconds = float(given)
        except (TypeError, ValueError):
            seconds = 0.0
        if 0 < seconds < float("inf"):
            return seconds
        logger.warning("tracepad: %s=%r is not a positive number of seconds; it is ignored",
                       name, given)
    if any(os.environ.get(f"OTEL_EXPORTER_OTLP{kind}_TIMEOUT") for kind in ("_TRACES", "")):
        return None
    return EXPORT_TIMEOUT


def _pick(argument: str | None, variable: str) -> str:
    return (argument if argument is not None else os.environ.get(variable, "")).strip()


_current: Config | None = None


def adopt(config: Config) -> None:
    global _current
    _current = config


def current() -> Config:
    """The configuration the REST calls use.

    `init` is the ordinary way to set it, and a script that only fetches a
    prompt should not have to call it: with no `init`, the environment is
    read on the first call and the same `TracepadConfigError` is raised when
    it says nothing.
    """
    if _current is None:
        adopt(resolve())
    assert _current is not None
    return _current


def forget() -> None:
    """Drop the process-wide configuration. For tests."""
    global _current
    _current = None
