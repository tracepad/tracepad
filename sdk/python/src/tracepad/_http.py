"""The REST half: `urllib`, bearer auth, no async twin (spec 017 #6).

The callers of this module are scripts and start-up code — prompts at boot,
the harness of spec 018 — where a blocking call is the honest shape. Scores
are the exception, and they get a queue and a thread of their own
(`_scores.py`) rather than a second client here.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Any

from ._config import VERSION
from ._errors import TracepadError, TracepadHTTPError

if TYPE_CHECKING:
    from ._config import Config

USER_AGENT = f"tracepad-python/{VERSION}"


@dataclass(frozen=True)
class Response:
    status: int
    body: Any = None
    headers: dict[str, str] = field(default_factory=dict)


def request(
    config: Config,
    method: str,
    path: str,
    *,
    body: Any = None,
    params: dict[str, Any] | None = None,
    timeout: float = 10.0,
) -> Response:
    """One JSON call. Raises `TracepadHTTPError` for a non-2xx answer and
    `TracepadError` for everything that never got one.

    `urllib` is imported here rather than at the top of the module: it costs
    some 20 ms of its own, and an application that only traces never makes a
    REST call at all.
    """
    import urllib.error
    import urllib.parse
    import urllib.request

    # Every name in a path is percent-encoded by its caller, and that leaves
    # the two segments encoding cannot hide: urllib sends `..` as it is and the
    # server answers with a redirect to the path it names, which is never the
    # object the caller meant. No name the store accepts is empty or dots
    # (spec 017 #19).
    if any(part in ("", ".", "..") for part in path.split("/")[1:]):
        raise TracepadError(f"tracepad: {method} {path}: an empty or dot segment names nothing")
    url = config.host + path
    if params:
        # A list is a repeated name (`tag=a&tag=b`), the listing's own grammar.
        url += "?" + urllib.parse.urlencode(params, doseq=True)
    # Raw UTF-8 like the other clients; a lone surrogate goes as its `\u` escape (spec 014 #32).
    payload = None if body is None else json.dumps(body, ensure_ascii=False).encode(
        "utf-8", "backslashreplace")
    call = urllib.request.Request(
        url,
        data=payload,
        method=method,
        headers={
            "Content-Type": "application/json",
            "User-Agent": USER_AGENT,
        },
    )
    # Unredirected: urllib copies every other header to wherever a `302`
    # points, whatever the host, so an SSO proxy or a canonical-host rule in
    # front of the store would have been handed the project's secret. The
    # store never redirects; a request that meets one arrives without the key
    # and is answered `401` (spec 017 #19).
    call.add_unredirected_header("Authorization", f"Bearer {config.key}")
    try:
        with urllib.request.urlopen(call, timeout=timeout) as answer:
            headers = {name.lower(): value for name, value in answer.headers.items()}
            return Response(answer.status, _decode(answer.read()), headers)
    except urllib.error.HTTPError as error:
        # The body is where the store names the offending field or item, and
        # it is the whole value of raising rather than logging a status.
        raise TracepadHTTPError(error.code, _text(error.read())) from None
    except (urllib.error.URLError, TimeoutError, OSError) as error:
        raise TracepadError(f"tracepad: {method} {config.host}{path}: {error}") from None


def _decode(raw: bytes) -> Any:
    if not raw:
        return None
    try:
        return json.loads(raw)
    except ValueError:
        return _text(raw)


def _text(raw: bytes) -> str:
    return raw.decode("utf-8", "replace")


def max_age(headers: dict[str, str]) -> int:
    """How long the server said this answer may be trusted, in seconds.

    A header that says nothing is zero, not a default of our own: the store
    sends `Cache-Control: private, max-age=60` on every prompt (`docs/prompts.md`), and
    inventing a window for a server that did not ask for one would cache
    against its wishes.
    """
    for directive in headers.get("cache-control", "").split(","):
        name, _, value = directive.strip().partition("=")
        if name.lower() == "max-age":
            try:
                return max(int(value), 0)
            except ValueError:
                return 0
    return 0

