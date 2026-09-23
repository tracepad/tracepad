"""Scores: a queue, a thread and a batch (spec 017 #6, #7).

Scores are written from inside request handlers, and the OTel exporter has
already shown what the right shape is there. A scoring failure must not fail
the request that produced the trace, so a batch the server refused is logged
and dropped rather than raised — the same asymmetry the exporter has. One
retry, not more: a 400 is deterministic, and a queue that retries forever is
a memory leak with a log line.
"""

from __future__ import annotations

import atexit
import queue
import threading
import time
from collections.abc import Callable
from typing import Any

from opentelemetry import trace as otel

from . import _config
from ._http import request
from ._log import logger

BATCH_SIZE = 100
INTERVAL = 2.0

_STOP = object()


class _Flush:
    __slots__ = ("done",)

    def __init__(self) -> None:
        self.done = threading.Event()


class ScoreQueue:
    """A background sender: up to `batch_size` scores every `interval` seconds."""

    def __init__(
        self,
        send: Callable[[list[dict[str, Any]]], None],
        *,
        batch_size: int = BATCH_SIZE,
        interval: float = INTERVAL,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        self._send = send
        self._batch_size = batch_size
        self._interval = interval
        self._clock = clock
        self._queue: queue.SimpleQueue[Any] = queue.SimpleQueue()
        self._thread: threading.Thread | None = None
        self._lock = threading.Lock()
        self._closed = self._dropped = False

    def submit(self, score: dict[str, Any]) -> None:
        with self._lock:
            if self._closed:
                # At interpreter exit the thread is gone. Saying so is the
                # point: a score that went nowhere quietly is the failure
                # this API is worst at surfacing.
                logger.warning("tracepad: score %r dropped, the queue is closed",
                               score.get("name"))
                return
            if self._thread is None:
                self._thread = threading.Thread(
                    target=self._run, name="tracepad-scores", daemon=True
                )
                self._thread.start()
        self._queue.put(score)

    def flush(self, timeout: float) -> None:
        """Wait until everything queued before this call has been sent."""
        with self._lock:
            if self._thread is None or self._closed or not self._thread.is_alive():
                return
        marker = _Flush()
        self._queue.put(marker)
        if not marker.done.wait(timeout):
            logger.warning("tracepad: the score queue did not drain within %ss", timeout)

    def close(self, timeout: float = 5.0) -> None:
        with self._lock:
            thread, self._closed = self._thread, True
        if thread is None:
            return
        self._queue.put(_STOP)
        thread.join(timeout)

    def drop(self) -> None:
        """Stop without sending what is queued."""
        with self._lock:
            self._closed = self._dropped = True
            thread = self._thread
        if thread is not None:
            self._queue.put(_STOP)

    def _run(self) -> None:
        while True:
            batch: list[dict[str, Any]] = []
            flushes: list[_Flush] = []
            stopping = self._collect(batch, flushes)
            if batch and not self._dropped:
                self._deliver(batch)
            for marker in flushes:
                marker.done.set()
            if stopping:
                return

    def _collect(self, batch: list[dict[str, Any]], flushes: list[_Flush]) -> bool:
        """Fill one batch: it closes when it is full, when the interval is up,
        when a flush cuts it, or when the process is going away."""
        deadline: float | None = None
        while len(batch) < self._batch_size:
            wait = None if deadline is None else deadline - self._clock()
            if wait is not None and wait <= 0:
                break
            try:
                item = self._queue.get(timeout=wait)
            except queue.Empty:
                break
            if item is _STOP:
                return True
            if isinstance(item, _Flush):
                flushes.append(item)
                break
            batch.append(item)
            if deadline is None:
                deadline = self._clock() + self._interval
        return False

    def _deliver(self, batch: list[dict[str, Any]]) -> None:
        # Anything, not just a `TracepadError`: a score value the JSON encoder
        # refuses raises a `TypeError` here, and an exception that escaped
        # would end the only thread there is — after which every later score
        # is lost in silence and every `flush` waits out its whole timeout
        # (found in review of PR #35). The batch is dropped; the sender lives.
        for attempt in (1, 2):
            try:
                self._send(batch)
                return
            except Exception as error:
                if attempt == 1:
                    continue
                logger.warning("tracepad: %d score(s) dropped: %r", len(batch), error)


def _post(batch: list[dict[str, Any]]) -> None:
    request(_config.current(), "POST", "/api/v1/scores", body=batch)


_queue_lock = threading.Lock()
_scores: ScoreQueue | None = None


def queue_of() -> ScoreQueue:
    global _scores
    with _queue_lock:
        if _scores is None:
            _scores = ScoreQueue(_post)
            atexit.register(_at_exit)
        return _scores


def _at_exit() -> None:
    if _scores is not None:
        _scores.flush(5.0)
        _scores.close()


def reset(replacement: ScoreQueue | None = None) -> None:
    """Replace the process-wide queue, for `tracepad.testing`. The one replaced
    stops and drops what it holds: posted later, it would go to whatever store
    the next test configured (spec 040 #14)."""
    global _scores
    with _queue_lock:
        replaced, _scores = _scores, replacement
    if replaced is not None:
        replaced.drop()


def flush_scores(timeout: float) -> float:
    """Drain the queue, and report how much of the timeout is left."""
    started = time.monotonic()
    if _scores is not None:
        _scores.flush(timeout)
    return timeout - (time.monotonic() - started)


def score(
    name: str,
    value: float | None = None,
    *,
    string_value: str | None = None,
    data_type: str | None = None,
    comment: str | None = None,
    id: str | None = None,
    trace_id: str | None = None,
    observation_id: str | None = None,
    observation: bool = False,
) -> None:
    """Score the trace (or the observation) in flight (spec 017 #7).

    With no target given, the target is the active span's trace — and its span
    id too when `observation=True`. Outside a span, with no `trace_id`, this
    raises: a score that silently went nowhere is the failure this API is
    worst at surfacing, and a programming error visible at the call site is
    the one exception to "the tracing path never raises". Where nothing traces —
    no `init`, no provider of the application's own, no recording span — the
    same call is a no-op with a debug line: every span is a no-op there, inside
    a block as outside one, and the call site is not wrong (spec 039 #1, #8). A
    `trace_id` given scores either way.
    """
    if trace_id is None:
        from ._tracing import tracing_off  # here: `_tracing` imports this module

        # A no-op global echoes a propagated parent, which never records; a live
        # span from a provider the application wired itself does.
        if tracing_off() and not otel.get_current_span().is_recording():
            logger.debug("tracepad.score(): tracing is off (no init); %r was dropped", name)
            return
        context = otel.get_current_span().get_span_context()
        if not context.is_valid:
            raise ValueError(
                "tracepad.score(): no active span and no trace_id; pass trace_id=…"
            )
        trace_id = format(context.trace_id, "032x")
        if observation and observation_id is None:
            observation_id = format(context.span_id, "016x")

    body: dict[str, Any] = {"name": name, "trace_id": trace_id}
    for key, given in (
        ("id", id),
        ("observation_id", observation_id),
        ("value", value),
        ("string_value", string_value),
        ("data_type", data_type),
        ("comment", comment),
    ):
        if given is not None:
            body[key] = given
    queue_of().submit(body)
