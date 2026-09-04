"""Configuration: the arguments of `init`, then the environment (spec 017 #10).

Standard OpenTelemetry variables — `OTEL_SERVICE_NAME`,
`OTEL_RESOURCE_ATTRIBUTES`, the exporter's and the batch processor's own — are
honoured by the OTel SDK as they are. The package neither reads them nor sets
them: the SDK already implements them, with its own defaults, and a second
implementation would be a second set of answers.
"""

from __future__ import annotations

import os
from dataclasses import dataclass

from ._errors import TracepadConfigError

VERSION = "0.1.0"


@dataclass(frozen=True)
class Config:
    """Where the store is, who we are to it, and what to call this deploy."""

    host: str
    key: str
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
