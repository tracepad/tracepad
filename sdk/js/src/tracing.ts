/**
 * The provider adaptation, `observe`, and the three callback-scoped shapes.
 *
 * The package owns no transport, no batching, no retry and no context
 * propagation — those are the OTel SDK's (spec 032 #1). What is here is the
 * ergonomics: one call that points an application at the store, and the
 * handful of shapes a person writes the same way every time.
 */

import {
  type Attributes,
  type Context,
  type Span,
  type SpanOptions as OtelSpanOptions,
  type Tracer,
  type TracerOptions,
  type TracerProvider,
  ProxyTracerProvider,
  SpanStatusCode,
  context,
  createContextKey,
  diag,
  isSpanContextValid,
  trace,
} from '@opentelemetry/api';
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-proto';
import { defaultResource, detectResources, envDetector, resourceFromAttributes } from '@opentelemetry/resources';
import {
  BatchSpanProcessor,
  NodeTracerProvider,
  type ReadableSpan,
  type SpanProcessor,
  type Span as SdkSpan,
} from '@opentelemetry/sdk-trace-node';

import * as attrs from './attributes.js';
import { type Config, type ConfigOptions, adopt, current, exportTimeout, resolve } from './config.js';
import { type Fields, type Usage, Stream, readResponse } from './generation.js';
import { stamp } from './harness.js';
import { VERSION, describe } from './http.js';
import { type Logger, setLogger, warn } from './log.js';
import { flushScores } from './scores.js';

let initialized = false;
let handedOut = false;
/** The provider `init` built, if it built one: a test's reset shuts it down (spec 040 #14). */
let built: NodeTracerProvider | undefined;

/** No `init` or `spanProcessor`, and no provider of the application's own: every span is the API's no-op (spec 039 #7). */
export const tracingOff = (): boolean => !initialized && !handedOut && delegateOf() === undefined;

/** The observation the current callback opened, so that `update` can tell
 * what the application said explicitly from what the wrapper captured. */
const HANDLE = createContextKey('tracepad-observation');

export interface InitOptions extends ConfigOptions {
  /** `false` attaches everything except the exporter. */
  export?: boolean;
  /** One export's bound, retries included; then TRACEPAD_EXPORT_TIMEOUT (seconds), then 5000 (spec 042 #3). */
  exportTimeoutMillis?: number;
  logger?: Logger;
}

/**
 * Point this process at a Tracepad store (spec 032 #2, #8).
 *
 * The call *adapts* to the provider it finds: when the application has
 * registered a global `TracerProvider` that takes processors after the fact
 * (the 1.x line's `addSpanProcessor`), the store's exporter is added to it,
 * so our spans and theirs share one pipeline and one flush; a provider with
 * no such hook (the 2.x line) is left alone with a warning, and the way in
 * is `spanProcessor()` in its constructor. Only when there is no provider
 * does the call build a `NodeTracerProvider` and register it, which is what
 * makes the one-liner true for a script.
 */
export function init(options: InitOptions = {}): void {
  if (initialized) {
    warn('init() has already run; this call is a no-op');
    return;
  }
  if (options.logger !== undefined) setLogger(options.logger);
  const config = configFor(options);
  const found = delegateOf();
  if (found === undefined) {
    install((built = new NodeTracerProvider({
      resource: resourceFor(config),
      spanProcessors: [processor(config, options)],
    })));
  } else {
    if (config.environment !== undefined || config.release !== undefined) refuseResource('init');
    // After `spanProcessor()` the processor is in the provider already, on
    // either line: a 1.x provider has the hook too, and attaching a second
    // exporter through it would send every span twice.
    if (handedOut) {
      // nothing to attach
    } else if (typeof (found as Adoptable).addSpanProcessor === 'function') {
      (found as Adoptable).addSpanProcessor!(processor(config, options));
    } else {
      warn(
        'init(): this process already has a TracerProvider and it takes span processors in ' +
          'its constructor only; nothing was attached — pass tracepad.spanProcessor() to it',
      );
    }
  }
  adopt(config);
  initialized = true;
  process.on('beforeExit', atExit);
}

/**
 * `init`'s configuration: its own arguments over the environment — or, after
 * `spanProcessor()`, over what the processor was built with, so that a bare
 * `init()` adopts it and an argument that disagrees is said out loud rather
 * than sending the spans to one store and the scores to another.
 */
function configFor(options: ConfigOptions): Config {
  if (!handedOut) return resolve(options);
  const built = current();
  const config = resolve({ ...built, ...defined(options) });
  if (config.host !== built.host || config.key !== built.key) {
    warn(
      'init(): the host or the key differs from the one spanProcessor() was built with; ' +
        'the spans go to the processor\'s and the scores and prompts to this one',
    );
  }
  return config;
}

function defined<T extends object>(options: T): Partial<T> {
  return Object.fromEntries(Object.entries(options).filter(([, value]) => value !== undefined)) as Partial<T>;
}

interface Adoptable extends TracerProvider {
  addSpanProcessor?: (processor: SpanProcessor) => void;
  forceFlush?: () => Promise<void>;
}

/** The provider the application registered, or nothing when the global is the API's no-op. */
function delegateOf(): Adoptable | undefined {
  const delegate = registered();
  if (delegate === FOLLOWER) return FOLLOWER.target;
  return isNoop(delegate) ? undefined : (delegate as Adoptable);
}

function registered(): TracerProvider {
  const found = trace.getTracerProvider() as TracerProvider & { getDelegate?: () => TracerProvider };
  return typeof found.getDelegate === 'function' ? found.getDelegate() : found;
}

/**
 * The global provider a test's reset leaves (spec 040 #14). The API binds a
 * tracer taken before any registration — `trace.getTracer()` at import — to
 * the first provider registered, for good; bound to this one, it asks at
 * every span where spans go now, and keeps recording in every capture after.
 * Its target is the capture's provider; without one it follows a provider
 * registered since, or is the no-op the package reads as tracing off.
 */
class Follower implements TracerProvider {
  target: Adoptable | undefined;

  getTracer(name: string, version?: string, options?: TracerOptions): Tracer {
    const to = (): TracerProvider => this.target ?? (registered() === this ? NOOP : registered());
    const now = (): Tracer => to().getTracer(name, version, options);
    return {
      startSpan: (span: string, given?: OtelSpanOptions, ctx?: Context) => now().startSpan(span, given, ctx),
      startActiveSpan: ((...args: unknown[]) => {
        const tracer = now();
        return (tracer.startActiveSpan as (...args: unknown[]) => unknown).apply(tracer, args);
      }) as Tracer['startActiveSpan'],
    };
  }
}

export const FOLLOWER = new Follower();

/** Put the follower behind the API's proxy when nothing is registered yet:
 * `tracepad/testing` imported before a provider is, as a tracer taken at
 * import binds to the first provider behind the proxy, for good. */
export function follow(): void {
  if (isNoop(registered())) (trace.getTracerProvider() as ProxyTracerProvider).setDelegate(FOLLOWER);
}

/**
 * Register a provider — with the context manager and the propagator that
 * come with it — or, after a test's reset, register the follower recording
 * into it: a tracer taken at import is bound to the follower, and binds to
 * the first provider behind the API's proxy for good.
 */
export function install(provider: NodeTracerProvider): void {
  const following = registered() === FOLLOWER;
  provider.register();
  if (!following) return;
  FOLLOWER.target = provider;
  (trace.getTracerProvider() as ProxyTracerProvider).setDelegate(FOLLOWER);
}

/**
 * The API's no-op is one module-level instance, and a fresh proxy with no
 * delegate hands it out — an identity a minifier cannot rename. The name is
 * the fallback for a second copy of the API, whose singleton is its own.
 */
const NOOP = new ProxyTracerProvider().getDelegate();

function isNoop(provider: TracerProvider): boolean {
  return provider === NOOP || provider.constructor?.name === 'NoopTracerProvider';
}

function resourceFor(config: Config) {
  // `service.name` from OTEL_SERVICE_NAME or OTEL_RESOURCE_ATTRIBUTES first;
  // the process title is the fallback for when neither named the service.
  let resource = defaultResource().merge(detectResources({ detectors: [envDetector] }));
  const named: Attributes = {};
  if (String(resource.attributes[attrs.SERVICE_NAME] ?? '').startsWith('unknown_service')) {
    named[attrs.SERVICE_NAME] = process.title || 'node';
  }
  if (config.environment !== undefined) named[attrs.ENVIRONMENT] = config.environment;
  if (config.release !== undefined) named[attrs.SERVICE_VERSION] = config.release;
  if (Object.keys(named).length > 0) resource = resource.merge(resourceFromAttributes(named));
  return resource;
}

/**
 * The package's span processor: the run link stamped on every span started
 * inside an item block, and the export to the store unless told not to.
 *
 * `init` attaches it to the provider it finds or builds; a provider that
 * takes processors in its constructor only (the 2.x `NodeTracerProvider`)
 * takes this instead, and `init` after that adopts the configuration
 * without a warning.
 */
export function spanProcessor(options: InitOptions = {}): SpanProcessor {
  const config = resolve(options);
  if (config.environment !== undefined || config.release !== undefined) refuseResource('spanProcessor');
  adopt(config);
  handedOut = true;
  return processor(config, options);
}

/** A resource is fixed when its provider is built, and `service.version` is
 * read from the resource only (`docs/ingest.md`), so neither can be added to
 * somebody else's provider afterwards. */
function refuseResource(call: string): void {
  warn(
    `${call}(): environment and release are resource attributes and this process already ` +
      'has a TracerProvider; set deployment.environment.name and service.version on its ' +
      'resource (OTEL_RESOURCE_ATTRIBUTES) instead',
  );
}

function processor(config: Config, options: InitOptions): SpanProcessor {
  if (options.export !== false) return new TracepadProcessor(exporter(config, options.exportTimeoutMillis));
  if (options.exportTimeoutMillis !== undefined) warn('exportTimeoutMillis is ignored with export: false, which adds no exporter to bound');
  return new TracepadProcessor(undefined);
}

function exporter(config: Config, millis: number | undefined): SpanProcessor {
  const timeoutMillis = exportTimeout(millis);
  return new BatchSpanProcessor(
    new OTLPTraceExporter({
      url: `${config.host}/v1/traces`,
      headers: { Authorization: `Bearer ${config.key}` },
      ...(timeoutMillis === undefined ? {} : { timeoutMillis }),
    }),
  );
}

class TracepadProcessor implements SpanProcessor {
  constructor(private readonly exporting: SpanProcessor | undefined) {}

  onStart(span: SdkSpan, parentContext: Context): void {
    // Before the exporting half, so that every span the exporter batches
    // already carries the run and the item an eval stamped (spec 018 #3).
    // It is here under `export: false` too: an application exporting
    // through another SDK still wants its spans stamped.
    stamp(span, parentContext);
    this.exporting?.onStart(span, parentContext);
  }

  onEnd(span: ReadableSpan): void {
    this.exporting?.onEnd(readable(span));
  }

  forceFlush(): Promise<void> {
    return this.exporting?.forceFlush() ?? Promise.resolve();
  }

  shutdown(): Promise<void> {
    return this.exporting?.shutdown() ?? Promise.resolve();
  }
}

/**
 * A span of the 1.x SDK, as the 2.x exporter reads one.
 *
 * The exporter is the 2.x line's whatever provider it was attached to, and
 * it reads `instrumentationScope` and `parentSpanContext`; a 1.x span names
 * the same two facts `instrumentationLibrary` and `parentSpanId`, and an
 * export that reads them under the new names dies on every batch. A view
 * over the span, with the two under the names the exporter reads, is what
 * makes Decision 2's adoption of a 1.x provider deliver anything.
 */
function readable(span: ReadableSpan): ReadableSpan {
  const legacy = span as ReadableSpan & { instrumentationLibrary?: ReadableSpan['instrumentationScope']; parentSpanId?: string };
  if (span.instrumentationScope !== undefined || legacy.instrumentationLibrary === undefined) return span;
  const { traceId, traceFlags } = span.spanContext();
  return Object.create(span, {
    instrumentationScope: { value: legacy.instrumentationLibrary, enumerable: true },
    parentSpanContext: {
      value: legacy.parentSpanId ? { traceId, spanId: legacy.parentSpanId, traceFlags } : undefined,
      enumerable: true,
    },
  }) as ReadableSpan;
}

let exiting: Promise<void> | undefined;

/** `beforeExit` fires when the event loop drains, and again after every
 * flush that gave it more to do. A flush with nothing pending is microtasks
 * only, so the loop ends on its own — and a span another library's own
 * `beforeExit` handler opened after ours still gets its flush, rather than
 * being left in the exporter's buffer for the exit to drop. */
function atExit(): void {
  if (exiting !== undefined) return;
  exiting = flush().finally(() => {
    exiting = undefined;
  });
}

export interface FlushOptions {
  /** One budget over the scores and the spans, in milliseconds. */
  timeout?: number;
}

/**
 * Deliver everything queued: the scores, then the spans.
 *
 * Not called on `process.exit()`, which fires no `beforeExit`; a script that
 * exits that way calls this first.
 */
export async function flush({ timeout = 10_000 }: FlushOptions = {}): Promise<void> {
  const left = await flushScores(timeout);
  const provider = delegateOf();
  if (provider === undefined || typeof provider.forceFlush !== 'function') return;
  if (left <= 0) {
    warn(
      `flush(): the score queue used the whole ${timeout}ms budget; the spans were not ` +
        "flushed and are left to their exporter's own schedule",
    );
    return;
  }
  // Never rejected: the tracing path warns, and at `beforeExit` a rejection
  // would be an unhandled one — an exit code of 1 because the store was away.
  const flushed = await Promise.race([
    provider.forceFlush().then(
      () => true,
      (error: unknown) => {
        warn(`flush(): the span processors failed to flush: ${describe(error)}`);
        return true;
      },
    ),
    new Promise<boolean>((resolve) => setTimeout(() => resolve(false), left).unref()),
  ]);
  if (!flushed) warn(`flush(): the span processors did not flush within ${timeout}ms`);
}

function tracer() {
  return trace.getTracer('tracepad', VERSION);
}

/** The clock the spans are on, as epoch milliseconds. */
function now(): number {
  return performance.timeOrigin + performance.now();
}

export interface UpdateFields {
  name?: string;
  input?: unknown;
  output?: unknown;
  metadata?: Record<string, unknown>;
  level?: attrs.Level;
  statusMessage?: string;
  type?: attrs.ObservationType;
}

/** One step of a trace: a thin handle over the OTel span, which is `.span`. */
export class Observation {
  readonly span: Span;
  /** @internal */
  ended = false;
  /** @internal What the application named itself, which the capture of
   * Decision 4 must not overwrite afterwards. */
  readonly explicit = new Set<string>();
  /** A trace behind it, decided at open (spec 039 #3, #7). */
  private readonly traced: boolean;

  constructor(span: Span) {
    this.span = span;
    this.traced = isSpanContextValid(span.spanContext()) && !tracingOff();
  }

  /** The trace's id, or `undefined` with no trace behind the span — tracing off (spec 039 #3). */
  get traceId(): string | undefined {
    return this.traced ? this.span.spanContext().traceId : undefined;
  }

  get spanId(): string | undefined {
    return this.traced ? this.span.spanContext().spanId : undefined;
  }

  /** @internal End the span once, with what it has: how a callback leaves. */
  finish(endTime?: number): void {
    if (this.ended) return;
    this.ended = true;
    this.span.end(endTime);
  }

  /** Write observation attributes on this span; `metadata` adds its keys to the ones the span carries (spec 042 #5). */
  update(fields: UpdateFields): void {
    const { span } = this;
    if (fields.name !== undefined) span.updateName(fields.name);
    if (fields.output !== undefined) this.explicit.add('output');
    costly(span, fields);
    set(span, attrs.OBSERVATION_LEVEL, fields.level);
    set(span, attrs.OBSERVATION_STATUS_MESSAGE, fields.statusMessage);
    set(span, attrs.OBSERVATION_TYPE, kind(fields.type));
  }
}

export interface EndFields {
  model?: string;
  usage?: Usage;
  cost?: number;
  output?: unknown;
}

/** An observation that called a model. */
export class Generation extends Observation {
  private firstTokenAt = false;
  private readonly captureOutput: boolean;
  /** What `stream` has read so far, folded into `end` when the stream is
   * over — or when the callback returns with the stream unfinished. */
  private streamed: Stream | undefined;

  constructor(span: Span, captureOutput = true) {
    super(span);
    this.captureOutput = captureOutput;
  }

  /** Stamp the moment the first token came back. Only the first call counts. */
  firstToken(): void {
    if (this.firstTokenAt) return;
    this.firstTokenAt = true;
    this.span.setAttribute(attrs.COMPLETION_START_TIME, attrs.rfc3339(now()));
  }

  /**
   * Pass a streamed OpenAI-compatible answer through (spec 031 #7, #21).
   *
   * Yields every chunk unchanged. The first chunk with content stamps the
   * first token; the content deltas are joined into the output; the model
   * and the `usage` — cost included — are taken from the chunks that carry
   * them. When the stream is exhausted the generation ends with all of that,
   * as if `end(response)` had been given the whole answer. A stream left
   * early — `break`, or a throw — leaves the ending to the callback around
   * it, which ends the span with what the stream gathered; an `end` you call
   * yourself before the stream is over wins, and nothing ends twice.
   */
  async *stream<C>(chunks: Iterable<C> | AsyncIterable<C>): AsyncGenerator<C, void, undefined> {
    const stream = (this.streamed = new Stream());
    for await (const chunk of chunks) {
      if (stream.take(chunk)) this.firstToken();
      yield chunk;
    }
    if (!this.ended) this.end();
  }

  /**
   * Record the result and end the span.
   *
   * `response` is read as an OpenAI-compatible answer (spec 017 #5); every
   * explicit field wins over what was read. With no `response`, what
   * `stream` gathered stands in for it. Returning from the callback without
   * calling this ends the span with what it has.
   */
  end(response?: unknown, fields: EndFields = {}): void {
    if (!this.span.isRecording()) return Observation.prototype.finish.call(this); // nothing to read for (spec 042 #1)
    if (response === undefined && this.streamed !== undefined) response = this.streamed.response();
    const read: Fields = response === undefined ? {} : readResponse(response);
    if (fields.model !== undefined) read.model = fields.model;
    if (fields.usage !== undefined) read.usage = fields.usage;
    if (fields.cost !== undefined) read.cost = fields.cost;
    if (fields.output !== undefined) read.output = fields.output;
    else if (this.explicit.has('output')) delete read.output;
    if (!this.captureOutput) delete read.output;

    const { span } = this;
    set(span, attrs.RESPONSE_MODEL, read.model);
    for (const [name, count] of Object.entries(read.usage ?? {})) {
      set(span, attrs.USAGE_PREFIX + name, count);
    }
    set(span, attrs.USAGE_COST, read.cost);
    if ('output' in read) span.setAttribute(attrs.OUTPUT, attrs.dumps(read.output));
    Observation.prototype.finish.call(this);
  }

  /** @internal */
  override finish(endTime?: number): void {
    // A callback that returned while a stream is under way still records
    // what the stream read: the output so far, and the usage if it got there.
    if (this.streamed !== undefined && !this.ended) this.end();
    super.finish(endTime);
  }
}

/** Write observation attributes on the current span (spec 032 #9). */
export function update(fields: UpdateFields): void {
  const span = trace.getActiveSpan();
  if (span === undefined || !span.isRecording()) return unwritten('update', span);
  observationOf(span).update(fields);
}

export interface TraceFields {
  name?: string;
  userId?: string;
  sessionId?: string;
  tags?: string[];
  metadata?: Record<string, unknown>;
  /**
   * The version of this trace's own logic — a pipeline revision, a prompt
   * bundle, an experiment arm — beside `release`, the deployment's, set once
   * at `init` (spec 038 #3).
   */
  version?: string;
}

/**
 * Write trace-level attributes on the current span (spec 032 #9).
 *
 * A request handler rarely holds the root span — the framework does — and
 * the one thing it knows is who the user is. These land where the handler
 * stands, and the mapper resolves them for the trace.
 */
export function updateTrace(fields: TraceFields): void {
  const span = trace.getActiveSpan();
  if (span === undefined || !span.isRecording()) return unwritten('updateTrace', span);
  set(span, attrs.TRACE_NAME, fields.name);
  set(span, attrs.USER_ID, fields.userId);
  set(span, attrs.SESSION_ID, fields.sessionId);
  if (fields.tags !== undefined) span.setAttribute(attrs.TRACE_TAGS, attrs.dumps([...fields.tags]));
  if (fields.metadata !== undefined) span.setAttribute(attrs.TRACE_METADATA, attrs.dumps(fields.metadata));
  set(span, attrs.TRACE_VERSION, fields.version || undefined);
}

/**
 * A warning only for the mistake: a process that traces, and no span in
 * context at all. A span that does not record is tracing off or a sampler's
 * choice — a configuration, not an error — and says so at debug (spec 042 #2).
 */
function unwritten(call: string, span: Span | undefined): void {
  if (span === undefined && !tracingOff()) warn(`${call}() outside a span: nothing was written`);
  else diag.debug(`tracepad: ${call}(): the span does not record; nothing was written`);
}

/**
 * The handle the callback is standing in, or a fresh one over this span.
 *
 * `update` is documented to act on the *current* span, whoever started it —
 * a framework's, another SDK's — and only when that span is the one this
 * package opened does the handle exist to remember what was said explicitly.
 */
function observationOf(span: Span): Observation {
  const handle = context.active().getValue(HANDLE);
  if (handle instanceof Observation && handle.span === span) return handle;
  return new Observation(span);
}

function set(span: Span, key: string, value: string | number | boolean | undefined): void {
  if (value !== undefined) span.setAttribute(key, value);
}

/** The attributes that cost a serialisation. */
interface Costly {
  input?: unknown;
  output?: unknown;
  metadata?: Record<string, unknown>;
  parameters?: Record<string, unknown> | undefined;
}

/**
 * Written after the span exists and only when it records: with tracing off,
 * or the span sampled out, nothing is serialised (spec 042 #1). Metadata is
 * one attribute per key, so that a later write adds keys rather than
 * replacing the lot; a key without a value writes nothing (spec 042 #5).
 */
function costly(span: Span, fields: Costly): void {
  if (!span.isRecording()) return;
  if (fields.input !== undefined) span.setAttribute(attrs.INPUT, attrs.dumps(fields.input));
  if (fields.output !== undefined) span.setAttribute(attrs.OUTPUT, attrs.dumps(fields.output));
  for (const [key, value] of Object.entries(fields.metadata ?? {})) {
    if (value != null) span.setAttribute(`${attrs.OBSERVATION_METADATA}.${key}`, attrs.scalar(value));
  }
  for (const [name, value] of Object.entries(fields.parameters ?? {})) {
    span.setAttribute(attrs.REQUEST_PREFIX + name, attrs.scalar(value));
  }
}

/**
 * The kinds already warned about, so that a step in a loop says it once
 * (spec 038 #5). A kind is remembered only once `init` or `spanProcessor`
 * has said where warnings go, and at most 256 of them: past that a new one
 * warns every time, which is what a kind computed at run time is.
 */
const warnedKinds = new Set<string>();

function firstTime(key: string): boolean {
  if (warnedKinds.has(key)) return false;
  if ((initialized || handedOut) && warnedKinds.size < 256) warnedKinds.add(key);
  return true;
}

/**
 * Pass a step's kind through: an empty one is no kind, and the caller's
 * default stands (spec 038 #8); one outside the ten is kept in metadata by
 * the mapper, and warned about as `warnedKinds` says.
 */
function kind(type: string | undefined): string | undefined {
  if (!type) return undefined;
  if (!(attrs.OBSERVATION_TYPES as readonly string[]).includes(type) && firstTime(type)) {
    warn(
      `${JSON.stringify(type)} is not one of the observation types the store ` +
        "classifies by; it will be kept in the observation's metadata",
    );
  }
  return type;
}

/** What every step may be opened with. `event` takes these alone. */
export interface ObservationOptions {
  input?: unknown;
  metadata?: Record<string, unknown>;
}

export interface SpanOptions extends ObservationOptions {
  /**
   * The step's kind — `'retriever'`, `'tool'`, `'agent'`… — as `observe`
   * takes it; `'span'` by default (spec 038 #1). `event` and `generation`
   * name their kind by being called, so neither takes one. `'generation'` is
   * not one here: its handle is `generation()`'s (spec 038 #8).
   */
  type?: Exclude<attrs.ObservationType, 'generation'>;
}

export interface GenerationOptions extends ObservationOptions {
  model?: string;
  /** A `Prompt`, or anything with a `name` and a `version`, or a name. */
  prompt?: { name: string; version?: number } | string;
  modelParameters?: Record<string, unknown>;
}

/** What a generation starts with: the cheap attributes, which a sampler may read. */
function generationAttributes(options: GenerationOptions): Attributes {
  const attributes: Attributes = { [attrs.OBSERVATION_TYPE]: 'generation' };
  if (options.model !== undefined) attributes[attrs.REQUEST_MODEL] = options.model;
  if (options.prompt !== undefined) {
    const { prompt } = options;
    attributes[attrs.PROMPT_NAME] = typeof prompt === 'string' ? prompt : prompt.name;
    if (typeof prompt !== 'string' && prompt.version !== undefined) {
      attributes[attrs.PROMPT_VERSION] = prompt.version;
    }
  }
  return attributes;
}

type Callback<H, T> = (handle: H) => T;

function isThenable(value: unknown): value is PromiseLike<unknown> {
  return value !== null && typeof value === 'object' && typeof (value as PromiseLike<unknown>).then === 'function';
}

/** Record an exception the way the OTel SDK's own helpers do. */
function failed(handle: Observation, error: unknown): void {
  const message = error instanceof Error ? error.message : String(error);
  handle.span.recordException(error instanceof Error ? error : message);
  handle.span.setStatus({ code: SpanStatusCode.ERROR, message });
}

/** Start a span in the active context with the cheap attributes, add the
 * costly ones if it records, and the context the callback runs in. */
function begin<H extends Observation>(
  name: string,
  attributes: Attributes,
  make: (span: Span) => H,
  startTime?: number,
  fields: Costly = {},
): [H, Context] {
  const options = startTime === undefined ? { attributes } : { attributes, startTime };
  const span = tracer().startSpan(name, options, context.active());
  costly(span, fields);
  const handle = make(span);
  return [handle, trace.setSpan(context.active(), span).setValue(HANDLE, handle)];
}

/**
 * Run `fn` inside a span and end the span once: on return, on settlement
 * when it returned a promise, or on a throw — recorded, and rethrown.
 */
function open<H extends Observation, T>(
  name: string,
  attributes: Attributes,
  make: (span: Span) => H,
  fn: Callback<H, T>,
  at?: number,
  fields?: Costly,
): T {
  const [handle, ctx] = begin(name, attributes, make, at, fields);
  let result: T;
  try {
    result = context.with(ctx, fn, undefined, handle);
  } catch (error) {
    failed(handle, error);
    handle.finish(at);
    throw error;
  }
  if (isThenable(result)) {
    return Promise.resolve(result).then(
      (value) => {
        handle.finish(at);
        return value;
      },
      (error: unknown) => {
        failed(handle, error);
        handle.finish(at);
        throw error;
      },
    ) as T;
  }
  handle.finish(at);
  return result;
}

/** The options may be omitted: `span(name, fn)` is `span(name, {}, fn)`. */
function split<O, H, T>(options: O | Callback<H, T>, fn?: Callback<H, T>): [O | undefined, Callback<H, T>] {
  return typeof options === 'function' ? [undefined, options as Callback<H, T>] : [options, fn!];
}

export function span<T>(name: string, fn: Callback<Observation, T>): T;
export function span<T>(name: string, options: SpanOptions, fn: Callback<Observation, T>): T;
/** A step of the trace, as a callback over an `Observation`. */
export function span<T>(name: string, options: SpanOptions | Callback<Observation, T>, fn?: Callback<Observation, T>): T {
  const [given, callback] = split(options, fn);
  // An empty kind is the default, and the one kind with a handle of its own
  // opens that handle, whatever else the options carry — what `observe`
  // does with it (spec 038 #8).
  const type = kind(given?.type) ?? 'span';
  if (type === 'generation') {
    const { type: _generation, ...rest } = given as GenerationOptions & { type: string };
    return generation(name, rest, callback);
  }
  return open(name, { [attrs.OBSERVATION_TYPE]: type }, (s) => new Observation(s), callback, undefined, given);
}

/** Warn about a kind given to a shape that names its own: it is not written. */
function ownKind(options: ObservationOptions | undefined, shape: string): void {
  const type = (options as { type?: unknown } | undefined)?.type;
  if (type !== undefined && type !== '' && type !== shape && firstTime(`${shape}:${String(type)}`)) {
    warn(`${shape}() takes no type; its kind is ${JSON.stringify(shape)} and ${JSON.stringify(type)} is ignored`);
  }
}

export function event<T>(name: string, fn: Callback<Observation, T>): T;
export function event<T>(name: string, options: ObservationOptions, fn: Callback<Observation, T>): T;
/** A zero-duration observation: something that happened, not something that took time. */
export function event<T>(name: string, options: ObservationOptions | Callback<Observation, T>, fn?: Callback<Observation, T>): T {
  const [given, callback] = split(options, fn);
  ownKind(given, 'event');
  return open(name, { [attrs.OBSERVATION_TYPE]: 'event' }, (s) => new Observation(s), callback, now(), given);
}

export function generation<T>(name: string, fn: Callback<Generation, T>): T;
export function generation<T>(name: string, options: GenerationOptions, fn: Callback<Generation, T>): T;
/** A call to a model, as a callback over a `Generation`. */
export function generation<T>(name: string, options: GenerationOptions | Callback<Generation, T>, fn?: Callback<Generation, T>): T {
  const [given, callback] = split(options, fn);
  ownKind(given, 'generation');
  const opened = given ?? {};
  return open(name, generationAttributes(opened), (s) => new Generation(s), callback, undefined, { ...opened, parameters: opened.modelParameters });
}

export interface ObserveOptions {
  /** The span's name; the function's own by default, then `"anonymous"`. */
  name?: string;
  /** One of the ten kinds `docs/ingest.md` lists; `"span"` by default. */
  type?: attrs.ObservationType;
  captureInput?: boolean;
  captureOutput?: boolean;
}

type AnyFunction = (...args: any[]) => any;

/**
 * Make a function a step of the trace (spec 032 #4).
 *
 * The call's arguments become the observation's `input`, as the positional
 * array they are — parameter names do not survive a bundler — and its
 * return value the `output`; `update({...})` from inside replaces either.
 * `type: "generation"` reads the return value as an OpenAI-compatible
 * response. A function returning a promise ends the span on settlement; a
 * generator or an async generator records the list of what it yielded and
 * ends when it is done. A thrown error or a rejection ends the span with
 * status `ERROR`, the exception recorded, and propagates unchanged.
 */
export function observe<F extends AnyFunction>(fn: F, options: ObserveOptions = {}): F {
  const { name = fn.name || 'anonymous', captureInput = true, captureOutput = true } = options;
  const type = options.type || 'span';
  const isGeneration = type === 'generation';
  // Checked per call rather than here, usually at module load, before
  // `init({ logger })` has said where warnings go; `kind` says it once.
  const attributes = () => {
    kind(type);
    return isGeneration ? generationAttributes({}) : { [attrs.OBSERVATION_TYPE]: type };
  };
  const make = (s: Span) => (isGeneration ? new Generation(s, captureOutput) : new Observation(s));
  const enter = (handle: Observation, args: unknown[]) => {
    if (captureInput && handle.span.isRecording()) handle.span.setAttribute(attrs.INPUT, attrs.dumps(args));
  };
  // What the function said about itself wins over what was captured from it
  // (spec 017 #4). A generator's result is the list of what it yielded,
  // which is not a model's answer however the step is typed — so the reader
  // of Decision 5 is only asked about a value returned whole.
  const leave = (handle: Observation, result: unknown, asResponse = true) => {
    if (isGeneration && asResponse) (handle as Generation).end(result);
    else if (captureOutput && result !== undefined && !handle.explicit.has('output') && handle.span.isRecording()) {
      handle.span.setAttribute(attrs.OUTPUT, attrs.dumps(result));
    }
  };

  // Which shape a call is decided by what it returns, not by how the
  // function was declared: a bound generator function, or an async
  // generator downleveled by a compiler, is a plain function that returns
  // an iterator, and it is the iterator that is traced to its end.
  const wrapper = function (this: unknown, ...args: unknown[]) {
    const [handle, ctx] = begin(name, attributes(), make);
    let result: unknown;
    try {
      result = context.with(ctx, () => {
        enter(handle, args);
        return fn.apply(this, args);
      });
    } catch (error) {
      failed(handle, error);
      handle.finish();
      throw error;
    }
    if (isThenable(result)) {
      return Promise.resolve(result).then(
        (value) => {
          leave(handle, value);
          handle.finish();
          return value;
        },
        (error: unknown) => {
          failed(handle, error);
          handle.finish();
          throw error;
        },
      );
    }
    if (isAsyncIterator(result)) return driveAsync(handle, ctx, result, leave);
    if (isIterator(result)) return drive(handle, ctx, result, leave);
    leave(handle, result);
    handle.finish();
    return result;
  };
  Object.defineProperty(wrapper, 'name', { value: fn.name, configurable: true });
  return wrapper as F;
}

type Leave = (handle: Observation, result: unknown, asResponse?: boolean) => void;

function isIterator(value: unknown): value is Iterator<unknown> {
  return steps(value, Symbol.iterator);
}

function isAsyncIterator(value: unknown): value is AsyncIterator<unknown> {
  return steps(value, Symbol.asyncIterator);
}

/** An iterator: it is its own iterable and has a `next`. An array is not one. */
function steps(value: unknown, iterable: symbol): boolean {
  if (value === null || typeof value !== 'object') return false;
  const object = value as Record<symbol | string, unknown>;
  return typeof object[iterable] === 'function' && typeof object.next === 'function';
}

/**
 * Drive an iterator the function returned, inside the span, to its end.
 *
 * Each step runs inside the span's context, and no longer: a generator runs
 * in the context of whoever advances it. What the consumer sends and throws
 * goes through, so does the return value, and a consumer that stops early
 * closes the inner iterator too, so that its own cleanup runs. The output
 * is the list of what was yielded (spec 032 #4).
 */
function* drive(handle: Observation, ctx: Context, steps: Iterator<unknown>, leave: Leave): Generator<unknown, unknown, unknown> {
  const yielded: unknown[] = [];
  try {
    let step = context.with(ctx, () => steps.next());
    while (!step.done) {
      yielded.push(step.value);
      let sent: unknown;
      try {
        sent = yield step.value;
      } catch (thrown) {
        step = context.with(ctx, () => rethrow(steps, thrown));
        continue;
      }
      step = context.with(ctx, () => steps.next(sent));
    }
    return step.value;
  } catch (error) {
    failed(handle, error);
    throw error;
  } finally {
    if (steps.return !== undefined) context.with(ctx, () => steps.return!());
    leave(handle, yielded, false);
    handle.finish();
  }
}

async function* driveAsync(handle: Observation, ctx: Context, steps: AsyncIterator<unknown>, leave: Leave): AsyncGenerator<unknown, unknown, unknown> {
  const yielded: unknown[] = [];
  try {
    let step = await context.with(ctx, () => steps.next());
    while (!step.done) {
      yielded.push(step.value);
      let sent: unknown;
      try {
        sent = yield step.value;
      } catch (thrown) {
        step = await context.with(ctx, () => rethrow(steps, thrown));
        continue;
      }
      step = await context.with(ctx, () => steps.next(sent));
    }
    return step.value;
  } catch (error) {
    failed(handle, error);
    throw error;
  } finally {
    if (steps.return !== undefined) await context.with(ctx, () => steps.return!());
    leave(handle, yielded, false);
    handle.finish();
  }
}

/** Hand a consumer's `throw()` to the inner generator, or throw it where the generator would. */
function rethrow<S extends Iterator<unknown> | AsyncIterator<unknown>>(steps: S, thrown: unknown): ReturnType<NonNullable<S['throw']>> {
  if (steps.throw === undefined) throw thrown;
  return steps.throw(thrown) as ReturnType<NonNullable<S['throw']>>;
}

/** Forget that `init` ran, for `tracepad/testing`: shut down what it built,
 * and leave nothing registered and the follower behind the API's proxy. */
export function reset(): void {
  initialized = false;
  warnedKinds.clear();
  handedOut = false;
  exiting = undefined;
  process.off('beforeExit', atExit);
  // Its spans go to the store the test configured, as at exit; a failed
  // export is warned about, never an unhandled rejection.
  built?.shutdown().catch((error: unknown) => warn(`reset: the provider init built failed to shut down: ${describe(error)}`));
  built = FOLLOWER.target = undefined;
  // The registration is undone the way the API's own `unregisterGlobal` does
  // it: `trace.disable()` would also replace the API's proxy, and the tracers
  // taken from the old one would follow nothing. The package's test of the
  // reset fails if the API moves its globals.
  const globals = (globalThis as Record<symbol, Record<string, unknown> | undefined>)[Symbol.for('opentelemetry.js.api.1')];
  if (globals !== undefined) delete globals.trace;
  (trace.getTracerProvider() as ProxyTracerProvider).setDelegate(FOLLOWER);
}

