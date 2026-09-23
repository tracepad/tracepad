"""The provider adaptation, the decorators and the context managers.

The package owns no transport, no batching, no retry and no context
propagation — those are the OTel SDK's (spec 017 #1). What is here is the
ergonomics: one call that points an application at the store, and the handful
of shapes a person writes the same way every time.

The OTel SDK's own imports are deferred into `init`, which is the only
function that needs them: `import tracepad` then costs the API and the
standard library, and the exporter's transport is paid for by the application
that exports.
"""

from __future__ import annotations

import functools
import inspect
import os
import sys
import threading
from collections.abc import AsyncIterable, AsyncIterator, Callable, Iterable, Iterator, Sequence
from contextlib import contextmanager
from contextvars import ContextVar
from time import monotonic, time_ns
from typing import Any

from opentelemetry import context as otel_context
from opentelemetry import trace as otel

from . import _attributes as attrs
from ._config import VERSION, Config, adopt, resolve, resolve_timeout
from ._generation import Stream, read_response
from ._log import logger
from ._scores import flush_scores

_initialized = False
# The export a `flush` that ran out of time left running: the next one waits
# for it rather than starting another behind the processor's export lock,
# so a store that is away costs one thread, not one per request. Kept with
# the flush it runs: a provider reset since is not waited for.
_flushing: tuple[Any, threading.Thread] | None = None
_flushing_lock = threading.Lock()
# The provider `init` built, if it built one: `tracepad.testing.reset()` shuts
# it down, so that its batching exporter does not outlive the test.
_built: Any = None

#: The observation the current block opened, so that `update` can tell what
#: the application said explicitly from what the decorator captured.
_current: ContextVar[Observation | None] = ContextVar("tracepad_observation", default=None)


def init(
    host: str | None = None,
    key: str | None = None,
    *,
    environment: str | None = None,
    release: str | None = None,
    export: bool = True,
    export_timeout: float | None = None,
) -> None:
    """Point this process at a Tracepad store (spec 017 #2, #10).

    The call *adapts* to the provider it finds: when the application has set a
    global `TracerProvider` — FastAPI instrumentation, another SDK — it adds a
    batching OTLP/HTTP exporter to that provider, so our spans and theirs
    share one pipeline and one flush. Only when the global provider is still
    the API's default proxy does it build one and set it, which is what makes
    the one-liner true for a script.

    `export=False` attaches everything except the exporter, for an application
    whose traces already reach the store another way.

    `export_timeout` bounds one export, in seconds, retries included — then
    TRACEPAD_EXPORT_TIMEOUT, then five (spec 042 #3).
    """
    global _built
    if _initialized:
        logger.warning("tracepad.init() has already run; this call is a no-op")
        return
    config = resolve(host, key, environment, release)
    if not export and export_timeout is not None:
        logger.warning("tracepad.init(): export_timeout is ignored with export=False, "
                       "which adds no exporter to bound")

    from opentelemetry.sdk.trace import TracerProvider

    provider = otel.get_tracer_provider()
    if not isinstance(provider, TracerProvider):
        # After a test's reset the global is `tracepad.testing`'s follower, and
        # a tracer taken at import is bound to it: it follows the new provider
        # rather than giving way to it (spec 040 #14).
        install = getattr(provider, "follow", otel.set_tracer_provider)
        provider = _built = TracerProvider(resource=_resource(config))
        install(provider)
    elif config.environment or config.release:
        # A resource is fixed when its provider is built, and `service.version`
        # is read from the resource only (`docs/ingest.md`), so neither can be
        # added to somebody else's provider afterwards.
        logger.warning(
            "tracepad.init(): environment and release are resource attributes and this "
            "process already has a TracerProvider; set deployment.environment.name and "
            "service.version on its resource (OTEL_RESOURCE_ATTRIBUTES) instead"
        )
    _attach(provider, config, export, export_timeout)


def _attach(provider: Any, config: Config, export: bool, timeout: float | None = None) -> None:
    """What `init` adds to the provider it found or built; `tracepad.testing`
    calls it with a provider and a configuration of its own (spec 040 #1)."""
    global _initialized
    # Before the exporting one, so that every span the exporter batches
    # already carries the run and the item an eval stamped (spec 018 #3). It
    # is registered under `export=False` too: an application exporting
    # through another SDK still wants its spans stamped.
    from ._harness import RunContextProcessor

    provider.add_span_processor(RunContextProcessor())
    if export:
        provider.add_span_processor(_exporter(config, resolve_timeout(timeout)))
    adopt(config)
    _initialized = True


def tracing_off() -> bool:
    """No `init` and no working provider of the application's own: every span is the
    API's no-op — an invalid context, or a propagated caller's echoed (spec 039 #7).
    `OTEL_SDK_DISABLED` makes the SDK's own provider hand out no-op tracers."""
    no_op = (otel.ProxyTracerProvider, otel.NoOpTracerProvider)
    disabled = os.environ.get("OTEL_SDK_DISABLED", "").strip().lower() == "true"
    return not _initialized and (disabled or isinstance(otel.get_tracer_provider(), no_op))


def _resource(config: Config) -> Any:
    from opentelemetry.sdk.resources import Resource

    attributes: dict[str, Any] = {}
    if config.environment:
        attributes[attrs.ENVIRONMENT] = config.environment
    if config.release:
        attributes[attrs.SERVICE_VERSION] = config.release
    resource = Resource.create(attributes)
    if resource.attributes.get(attrs.SERVICE_NAME) == "unknown_service":
        # `Resource.create` has already read OTEL_SERVICE_NAME and
        # OTEL_RESOURCE_ATTRIBUTES; the process name is the fallback for when
        # neither of them named the service.
        resource = resource.merge(Resource({attrs.SERVICE_NAME: _process_name()}))
    return resource


def _process_name() -> str:
    return os.path.basename(sys.argv[0]).removesuffix(".py") or "python"


def _exporter(config: Config, timeout: float | None) -> Any:
    from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
    from opentelemetry.sdk.trace.export import BatchSpanProcessor

    return BatchSpanProcessor(
        OTLPSpanExporter(
            endpoint=config.traces_endpoint,
            headers={"Authorization": f"Bearer {config.key}"},
            timeout=timeout,
        )
    )


def flush(timeout: float = 10.0) -> None:
    """Deliver everything queued: the scores, then the spans.

    The timeout is one budget over both, and the spans are the half that
    matters more — so a score queue that spent all of it is said out loud
    rather than leaving `force_flush` a deadline of zero, which returns
    at once and exports nothing (found in review of PR #35).

    OpenTelemetry's batch processor exports synchronously on `force_flush` and
    never reads the deadline it is given, so the flush runs on a thread of its
    own that is waited for what is left of the budget; past that, the export
    finishes in the background (spec 042 #4).
    """
    left = flush_scores(timeout)
    force = getattr(otel.get_tracer_provider(), "force_flush", None)
    if force is None:
        return
    if left <= 0:
        logger.warning(
            "tracepad.flush(): the score queue used the whole %ss budget; the spans were "
            "not flushed and are left to their exporter's own schedule", timeout
        )
        return
    global _flushing
    deadline = monotonic() + left
    while True:
        # A flush still running from before is waited for first; then this
        # flush's own, for the spans ended since (found in review of PR #83).
        with _flushing_lock:
            ours = _flushing is None or _flushing[0] != force or not _flushing[1].is_alive()
            if ours:
                _flushing = (force, threading.Thread(target=force, daemon=True,
                                                     args=(int(left * 1000),)))
                _flushing[1].start()
            exporting = _flushing[1]
        exporting.join(max(0.0, deadline - monotonic()))
        if exporting.is_alive():
            logger.warning(
                "tracepad.flush(): the spans were not exported within the %ss budget; the "
                "export goes on in the background", timeout
            )
            return
        if ours:
            return


def _tracer() -> otel.Tracer:
    return otel.get_tracer("tracepad", VERSION)


class Observation:
    """One step of a trace: a thin handle over the OTel span, which is `.span`."""

    def __init__(self, span: otel.Span) -> None:
        self.span = span
        # Whether a trace is behind it, decided at open (spec 039 #3, #7).
        self._traced = span.get_span_context().is_valid and not tracing_off()
        self._ended = False
        # What the application named itself, which the capture of Decision 4
        # must not overwrite afterwards.
        self._explicit: set[str] = set()

    @property
    def trace_id(self) -> str | None:
        """The trace's 32-hex id, or `None` with no trace behind the span —
        tracing off, as an `Attempt` has none before it runs (spec 039 #3)."""
        return format(self.span.get_span_context().trace_id, "032x") if self._traced else None

    @property
    def span_id(self) -> str | None:
        """The span's 16-hex id, or `None` as `trace_id`."""
        return format(self.span.get_span_context().span_id, "016x") if self._traced else None

    def _finish(self, end_time: int | None = None) -> None:
        """End the span once, with what it has: how a block leaves."""
        if not self._ended:
            self._ended = True
            self.span.end(end_time)

    def update(
        self,
        *,
        name: str | None = None,
        input: Any = None,
        output: Any = None,
        metadata: dict[str, Any] | None = None,
        level: str | None = None,
        status_message: str | None = None,
        type: str | None = None,
    ) -> None:
        """Write observation attributes on this span; `metadata` adds its keys to
        the ones the span carries (spec 042 #5)."""
        span = self.span
        if name is not None:
            span.update_name(name)
        if output is not None:
            self._explicit.add("output")
        _costly(span, input=input, output=output, metadata=metadata)
        _set(span, attrs.OBSERVATION_LEVEL, level)
        _set(span, attrs.OBSERVATION_STATUS_MESSAGE, status_message)
        _set(span, attrs.OBSERVATION_TYPE, _kind(type))


class Generation(Observation):
    """An observation that called a model."""

    def __init__(self, span: otel.Span, capture_output: bool = True) -> None:
        super().__init__(span)
        self._first_token = False
        self._capture_output = capture_output
        # What `stream` has read so far, folded into `end` when the stream is
        # over — or when the block is left with the stream unfinished.
        self._stream: Stream | None = None

    def first_token(self) -> None:
        """Stamp the moment the first token came back. Only the first call counts."""
        if self._first_token:
            return
        self._first_token = True
        self.span.set_attribute(attrs.COMPLETION_START_TIME, attrs.rfc3339(time_ns()))

    def stream(self, chunks: Iterable[Any]) -> Iterator[Any]:
        """Pass a streamed OpenAI-compatible answer through (spec 031 #7).

        Yields every chunk unchanged. The first chunk with content stamps the
        first token; the content deltas are joined into the output; the model
        and the `usage` — cost included — are taken from the chunks that carry
        them, which for `usage` is the last one, when the provider was asked
        to send it (`docs/sdk-python.md`). When the stream is exhausted the
        generation ends with all of that, as if `end(response=…)` had been
        given the whole answer. A stream that is closed early, or that raises,
        leaves the ending to the block around it, which records the exception
        and ends the span with what the stream gathered; an `end` you call
        yourself before the stream is over wins, and nothing ends twice.
        """
        # Nothing is gathered for a span that does not record (spec 042 #1).
        stream = self._stream = Stream() if self.span.is_recording() else None
        for chunk in chunks:
            if stream is not None and stream.take(chunk):
                self.first_token()
            yield chunk
        if not self._ended:
            self.end()

    async def astream(self, chunks: AsyncIterable[Any]) -> AsyncIterator[Any]:
        """`stream`, over an async stream: `async for chunk in call.astream(…)`."""
        stream = self._stream = Stream() if self.span.is_recording() else None
        async for chunk in chunks:
            if stream is not None and stream.take(chunk):
                self.first_token()
            yield chunk
        if not self._ended:
            self.end()

    def end(
        self,
        response: Any = None,
        *,
        model: str | None = None,
        usage: dict[str, Any] | None = None,
        cost: float | None = None,
        output: Any = None,
    ) -> None:
        """Record the result and end the span.

        `response` is read as an OpenAI-compatible answer (spec 017 #5); every
        explicit argument wins over what was read. With no `response`, what
        `stream` gathered stands in for it. Leaving the block without calling
        this ends the span with what it has.
        """
        if not self.span.is_recording():  # nothing to read for (spec 042 #1)
            Observation._finish(self)
            return
        if response is None and self._stream is not None:
            response = self._stream.response()
        fields = read_response(response) if response is not None else {}
        if model is not None:
            fields["model"] = model
        if usage is not None:
            fields["usage"] = usage
        if cost is not None:
            fields["cost"] = cost
        if output is not None:
            fields["output"] = output
        elif "output" in self._explicit:
            fields.pop("output", None)
        if not self._capture_output:
            fields.pop("output", None)

        span = self.span
        _set(span, attrs.RESPONSE_MODEL, fields.get("model"))
        for name, count in (fields.get("usage") or {}).items():
            _set(span, attrs.USAGE_PREFIX + name, count)
        _set(span, attrs.USAGE_COST, fields.get("cost"))
        if "output" in fields:
            _set(span, attrs.OUTPUT, attrs.dumps(fields["output"]))
        Observation._finish(self)

    def _finish(self, end_time: int | None = None) -> None:
        # A block left while a stream is under way still records what the
        # stream read: the output so far, and the usage if it got that far.
        if self._stream is not None and not self._ended:
            self.end()
        super()._finish(end_time)


def update(
    *,
    name: str | None = None,
    input: Any = None,
    output: Any = None,
    metadata: dict[str, Any] | None = None,
    level: str | None = None,
    status_message: str | None = None,
    type: str | None = None,
) -> None:
    """Write observation attributes on the current span (spec 017 #11)."""
    span = otel.get_current_span()
    if not span.is_recording():
        _unwritten("update")
        return
    _observation_of(span).update(
        name=name, input=input, output=output, metadata=metadata,
        level=level, status_message=status_message, type=type,
    )


def update_trace(
    *,
    name: str | None = None,
    user_id: str | None = None,
    session_id: str | None = None,
    tags: Sequence[str] | None = None,
    metadata: dict[str, Any] | None = None,
    version: str | None = None,
) -> None:
    """Write trace-level attributes on the current span (spec 017 #11).

    A request handler rarely holds the root span — the framework does — and
    the one thing it knows is who the user is. These land where the handler
    stands, and the mapper resolves them for the trace. `version` is the
    version of this trace's own logic — a pipeline revision, a prompt bundle,
    an experiment arm — beside `release`, the deployment's, set once at
    `init` (spec 038 #3).
    """
    span = otel.get_current_span()
    if not span.is_recording():
        _unwritten("update_trace")
        return
    _set(span, attrs.TRACE_NAME, name)
    _set(span, attrs.USER_ID, user_id)
    _set(span, attrs.SESSION_ID, session_id)
    if tags is not None:
        _set(span, attrs.TRACE_TAGS, attrs.dumps(list(tags)))
    if metadata is not None:
        _set(span, attrs.TRACE_METADATA, attrs.dumps(metadata))
    _set(span, attrs.TRACE_VERSION, version or None)


def _unwritten(call: str) -> None:
    """Warn only for the mistake: a process that traces, and no span anywhere in
    context. A span that does not record is tracing off or a sampler's choice,
    a configuration rather than an error, and says so at debug (spec 042 #2)."""
    span = otel.get_current_span()
    if tracing_off() or _current.get() is not None or span.get_span_context().is_valid:
        logger.debug("tracepad.%s(): the span does not record; nothing was written", call)
    else:
        logger.warning("tracepad.%s() outside a span: nothing was written", call)


def _observation_of(span: otel.Span) -> Observation:
    """The handle the block is standing in, or a fresh one over this span.

    `update` is documented to act on the *current* span, whoever started it —
    a framework's, another SDK's — and only when that span is the one this
    package opened does the handle exist to remember what was said explicitly.
    """
    current = _current.get()
    if current is not None and current.span is span:
        return current
    return Observation(span)


def _set(span: otel.Span, key: str, value: Any) -> None:
    if value is not None:
        span.set_attribute(key, value)


def _costly(
    span: otel.Span,
    input: Any = None,
    output: Any = None,
    metadata: dict[str, Any] | None = None,
    parameters: dict[str, Any] | None = None,
) -> None:
    """The attributes that cost a serialisation, written only on a span that
    records: with tracing off, or the span sampled out, nothing is serialised
    (spec 042 #1)."""
    if not span.is_recording():
        return
    _set(span, attrs.INPUT, None if input is None else attrs.dumps(input))
    _set(span, attrs.OUTPUT, None if output is None else attrs.dumps(output))
    if metadata is not None:
        span.set_attributes(attrs.metadata(metadata))
    for name, value in (parameters or {}).items():
        _set(span, attrs.REQUEST_PREFIX + name, attrs.scalar(value))


#: The spellings already warned about, so that a step in a loop says it once
#: (spec 038 #5). A spelling is remembered only after `init`, and at most 256
#: of them: past that a new one warns every time, which is what a kind
#: computed at run time is.
_warned_kinds: set[str] = set()


def _kind(type: str | None) -> str | None:
    """Pass a step's kind through, warning when the store will not classify by it.

    An empty kind is no kind — `None`, and the caller's default stands, as Go's
    `WithType("")` (spec 038 #8). A spelling outside the ten is not refused:
    the mapper keeps it in the observation's metadata and classifies the span
    by its heuristics. The warning is given once per spelling, and only when a
    step is written, after the application has configured its logging.
    """
    if not type:
        return None
    if type not in attrs.OBSERVATION_TYPES and type not in _warned_kinds:
        if _initialized and len(_warned_kinds) < 256:
            _warned_kinds.add(type)
        logger.warning(
            "tracepad: %r is not one of the observation types the store "
            "classifies by; it will be kept in the observation's metadata", type
        )
    return type


def _generation_attributes(model: str | None, prompt: Any) -> dict[str, Any]:
    """What a generation starts with: the cheap ones, which a sampler may read."""
    attributes: dict[str, Any] = {attrs.OBSERVATION_TYPE: "generation"}
    if model is not None:
        attributes[attrs.REQUEST_MODEL] = model
    if prompt is not None:
        attributes[attrs.PROMPT_NAME] = getattr(prompt, "name", prompt)
        version = getattr(prompt, "version", None)
        if version is not None:
            attributes[attrs.PROMPT_VERSION] = version
    return attributes


@contextmanager
def _open(
    name: str,
    attributes: dict[str, Any],
    factory: Callable[[otel.Span], Observation] = Observation,
    *,
    start_time: int | None = None,
    end_time: int | None = None,
    **costly: Any,
) -> Iterator[Any]:
    """Start a span with the cheap attributes, add the costly ones if it
    records (spec 042 #1), hand out its observation, and end it once."""
    span = _tracer().start_span(name, attributes=attributes, start_time=start_time)
    _costly(span, **costly)
    handle = factory(span)
    token = _current.set(handle)
    try:
        # `use_span` makes it the current span, records an exception as the
        # OTel event the mapper reads as an error, and re-raises unchanged.
        with otel.use_span(span, end_on_exit=False, record_exception=True,
                           set_status_on_exception=True):
            yield handle
    finally:
        _current.reset(token)
        handle._finish(end_time)


@contextmanager
def _stepping(handle: Observation) -> Iterator[None]:
    """Make the span current for one step of a generator, and no longer.

    A generator runs in the context of whoever advances it, so a block held
    open across a `yield` leaves the span attached to the *consumer*: the
    caller's next span becomes a child of a generator it merely touched, and
    two generators consumed in turn restore each other's contexts out of order
    — after which a later span is parented to a span that has ended (found in
    review of PR #35). Attaching per step is what keeps the stack a stack.
    """
    token = otel_context.attach(otel.set_span_in_context(handle.span))
    current = _current.set(handle)
    try:
        yield
    finally:
        _current.reset(current)
        otel_context.detach(token)


def _failed(handle: Observation, error: BaseException) -> None:
    """Record an exception the way `use_span` does for the other shapes."""
    handle.span.record_exception(error)
    handle.span.set_status(otel.Status(otel.StatusCode.ERROR, str(error)))


def span(
    name: str,
    *,
    input: Any = None,
    metadata: dict[str, Any] | None = None,
    type: str | None = "span",
) -> Any:
    """A step of the trace, as a context manager over an `Observation`.

    `type` is the step's kind — `"retriever"`, `"tool"`, `"agent"`… — as
    `@observe(type=…)` takes it (spec 038 #1): the kind is known when the step
    opens, so it is written then rather than corrected by `update` after. An
    empty kind is the default, and `"generation"` opens what `generation()`
    opens, the one kind with a handle of its own — as the decorator does
    (spec 038 #8).
    """
    kind = _kind(type) or "span"
    if kind == "generation":
        return generation(name, input=input, metadata=metadata)
    return _open(name, {attrs.OBSERVATION_TYPE: kind}, input=input, metadata=metadata)


def event(name: str, *, input: Any = None, metadata: dict[str, Any] | None = None) -> Any:
    """A zero-duration observation: something that happened, not something that took time."""
    at = time_ns()
    return _open(name, {attrs.OBSERVATION_TYPE: "event"}, start_time=at, end_time=at,
                 input=input, metadata=metadata)


def generation(
    name: str,
    *,
    model: str | None = None,
    prompt: Any = None,
    model_parameters: dict[str, Any] | None = None,
    input: Any = None,
    metadata: dict[str, Any] | None = None,
) -> Any:
    """A call to a model, as a context manager over a `Generation`."""
    return _open(name, _generation_attributes(model, prompt), Generation,
                 input=input, metadata=metadata, parameters=model_parameters)


def observe(
    fn: Callable[..., Any] | None = None,
    *,
    name: str | None = None,
    type: str | None = "span",
    capture_input: bool = True,
    capture_output: bool = True,
) -> Any:
    """Make a function a step of the trace, with or without parentheses.

    The call's arguments become the observation's `input` and its return value
    the `output` (spec 017 #4): the first trace a newcomer sees must have
    something in it, and the store is self-hosted — the privacy argument for
    an empty default belongs to a SaaS. `tracepad.update(...)` from inside
    replaces either.

    `type="generation"` reads the return value as an OpenAI-compatible
    response (spec 017 #5). Sync and `async` functions are wrapped, and so are
    generators of both kinds: the span ends when the generator is exhausted or
    closed, and its `output` is the list of what it yielded.
    """

    def decorate(fn: Callable[..., Any]) -> Callable[..., Any]:
        signature = inspect.signature(fn) if capture_input else None
        label = name or fn.__name__
        kind = type or "span"
        is_generation = kind == "generation"

        def attributes() -> dict[str, Any]:
            # Checked per call rather than here, at import time, before the
            # application has configured its logging; `_kind` says it once.
            _kind(kind)
            if is_generation:
                return _generation_attributes(None, None)
            return {attrs.OBSERVATION_TYPE: kind}

        def make(span: otel.Span) -> Observation:
            return Generation(span, capture_output) if is_generation else Observation(span)

        def start() -> Any:
            return _open(label, attributes(), make)

        def begin() -> Observation:
            return make(_tracer().start_span(label, attributes=attributes()))

        def enter(observation: Observation, args: tuple[Any, ...], kwargs: dict[str, Any]) -> None:
            if signature is not None and observation.span.is_recording():
                _set(observation.span, attrs.INPUT,
                     attrs.dumps(_arguments(signature, args, kwargs)))

        def leave(observation: Any, result: Any, *, as_response: bool = True) -> None:
            # What the function said about itself wins over what was captured
            # from it (spec 017 #4): `update` replaces, it is not replaced. A
            # generator's result is the list of what it yielded, which is not
            # a model's answer however the step is typed — so the reader of
            # Decision 5 is only asked about a value returned whole.
            if is_generation and as_response:
                observation.end(response=result)
            elif (capture_output and "output" not in observation._explicit
                  and observation.span.is_recording()):
                _set(observation.span, attrs.OUTPUT, attrs.dumps(result))

        if inspect.isasyncgenfunction(fn):

            @functools.wraps(fn)
            async def async_generator(*args: Any, **kwargs: Any) -> AsyncIterator[Any]:
                observation = begin()
                yielded: list[Any] = []
                try:
                    with _stepping(observation):
                        enter(observation, args, kwargs)
                        steps = fn(*args, **kwargs).__aiter__()
                    while True:
                        with _stepping(observation):
                            try:
                                value = await steps.__anext__()
                            except StopAsyncIteration:
                                break
                        yielded.append(value)
                        yield value
                except Exception as error:
                    _failed(observation, error)
                    raise
                finally:
                    leave(observation, yielded, as_response=False)
                    observation._finish()

            return async_generator

        if inspect.isgeneratorfunction(fn):

            @functools.wraps(fn)
            def generator(*args: Any, **kwargs: Any) -> Iterator[Any]:
                observation = begin()
                yielded: list[Any] = []
                try:
                    with _stepping(observation):
                        enter(observation, args, kwargs)
                        steps = iter(fn(*args, **kwargs))
                    while True:
                        with _stepping(observation):
                            try:
                                value = next(steps)
                            except StopIteration:
                                break
                        yielded.append(value)
                        yield value
                except Exception as error:
                    _failed(observation, error)
                    raise
                finally:
                    leave(observation, yielded, as_response=False)
                    observation._finish()

            return generator

        if inspect.iscoroutinefunction(fn):

            @functools.wraps(fn)
            async def coroutine(*args: Any, **kwargs: Any) -> Any:
                with start() as observation:
                    enter(observation, args, kwargs)
                    result = await fn(*args, **kwargs)
                    leave(observation, result)
                    return result

            return coroutine

        @functools.wraps(fn)
        def wrapper(*args: Any, **kwargs: Any) -> Any:
            with start() as observation:
                enter(observation, args, kwargs)
                result = fn(*args, **kwargs)
                leave(observation, result)
                return result

        return wrapper

    return decorate if fn is None else decorate(fn)


def _arguments(
    signature: inspect.Signature, args: tuple[Any, ...], kwargs: dict[str, Any]
) -> dict[str, Any]:
    """The call's arguments by parameter name, with `self`/`cls` dropped."""
    try:
        bound = signature.bind_partial(*args, **kwargs)
    except TypeError:  # the call is about to fail on its own terms
        return {"args": list(args), "kwargs": kwargs}
    return {
        parameter: value
        for parameter, value in bound.arguments.items()
        if parameter not in ("self", "cls")
    }
