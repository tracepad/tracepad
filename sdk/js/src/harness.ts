/**
 * The eval harness: a run, the block that stamps it, and the scores (spec 032 #10).
 *
 * Spec 014 made an eval a loop any script can run with `curl`. This is that
 * loop in Node, and the one thing it owns that a `curl` recipe cannot is the
 * stamping: inside `run.item(case, fn)` every span the *application* starts
 * — its own, a framework's, another SDK's — carries the run and the item,
 * because the package's span processor writes them at `onStart` rather than
 * the harness touching a span it does not hold (spec 018 #3).
 *
 * The store executes nothing, and neither does this: running the cases is
 * the user's program.
 */

import { createHash } from 'node:crypto';

import { type Context, context, createContextKey } from '@opentelemetry/api';
import type { Span } from '@opentelemetry/sdk-trace-node';

import { ITEM_ID, RUN_ID } from './attributes.js';
import { current } from './config.js';
import type { Dataset, Item } from './datasets.js';
import { TracepadError, TracepadHTTPError, request } from './http.js';
import { type ScoreFields, score } from './scores.js';
import { flush } from './tracing.js';

/** Where the current attempt sits. The OTel context is what `context.with`
 * carries through an `await` and into a callback, and what a span started
 * with an explicit context would not see in an ambient variable of ours. */
const ATTEMPT = createContextKey('tracepad-attempt');

export const PAGE = 500;

type Json = Record<string, unknown>;

/** Write the run and the item on a span started inside an item block. */
export function stamp(span: Span, parentContext: Context): void {
  const attempt = parentContext.getValue(ATTEMPT);
  if (!(attempt instanceof Attempt)) return;
  span.setAttribute(RUN_ID, attempt.runId);
  span.setAttribute(ITEM_ID, attempt.itemId);
  attempt.saw(span);
}

/** One case, once: what the block stamped and what it produced. */
export class Attempt {
  readonly runId: string;
  readonly itemId: string;
  /** Every trace the block started, in order. A case run three times inside
   * one block is three traces of one item, and the run's summary counts
   * them all (spec 014 #2). */
  readonly traces: string[] = [];
  private readonly spans = new Set<string>();

  constructor(runId: string, itemId: string) {
    this.runId = runId;
    this.itemId = itemId;
  }

  /** The last trace to start — the one a harness would quote. */
  get traceId(): string | undefined {
    return this.traces[this.traces.length - 1];
  }

  /** @internal Record a span the processor stamped, and the trace it began.
   *
   * A case begins where the *block* does, not where the trace does (spec 018
   * #13): a span whose parent is not itself inside the block starts the
   * case, so a harness whose loop already runs under a span of its own still
   * has a trace to score. One trace is recorded once, however many spans of
   * it the block opened. */
  saw(span: Span): void {
    const { traceId, spanId } = span.spanContext();
    const parent = parentOf(span);
    const inside = parent !== undefined && this.spans.has(parent);
    this.spans.add(spanId);
    if (inside) return;
    if (!this.traces.includes(traceId)) this.traces.push(traceId);
  }

  /** The two attributes, for a service the block cannot reach: they do not
   * propagate with the trace context, by design (spec 014 #2). */
  attributes(): Record<string, string> {
    return { [RUN_ID]: this.runId, [ITEM_ID]: this.itemId };
  }

  /** Score the trace this attempt produced (spec 018 #4). */
  score(name: string, value?: number | ScoreFields, fields: ScoreFields = {}): void {
    if (this.traceId === undefined) {
      throw new Error(
        `tracepad: no trace has started inside this item block yet (${this.itemId}); ` +
          'score after the application has run, or pass traceId to score()',
      );
    }
    if (typeof value === 'object') {
      fields = value;
      value = undefined;
    }
    score(name, value, { ...fields, traceId: this.traceId });
  }
}

/** The 2.x span names its parent's context; the 1.x one its id. */
function parentOf(span: Span): string | undefined {
  const { parentSpanContext, parentSpanId } = span as Span & { parentSpanId?: string };
  return parentSpanContext?.spanId ?? parentSpanId;
}

export interface CloseOptions {
  /** The flush's budget before the close is posted, in milliseconds. */
  timeout?: number;
}

export interface RunItemsOptions {
  unknown?: boolean;
  limit?: number;
}

/** An open run: the version it pinned, the blocks it stamps, and its close. */
export class Run {
  readonly dataset: Dataset;
  readonly id: string;
  readonly name: string;
  /** The version the harness must fetch by — one number by construction (spec 014 #7). */
  readonly datasetVersion: number;
  private closed = false;

  constructor(dataset: Dataset, body: Json) {
    this.dataset = dataset;
    this.id = String(body.id);
    this.name = typeof body.name === 'string' ? body.name : '';
    this.datasetVersion = Number(body.dataset_version);
  }

  /**
   * Run one case: everything started inside carries the run and the item.
   *
   * The block opens no span of its own — a root span the harness opened
   * would make every eval trace look like a trace of the harness. It reaches
   * as far as the OTel context does: through an `await`, into a callback,
   * and not into a request another process answers.
   */
  item<T>(case_: Item | Json | string, fn: (attempt: Attempt) => T): T {
    const attempt = new Attempt(this.id, caseId(case_));
    return context.with(context.active().setValue(ATTEMPT, attempt), fn, undefined, attempt);
  }

  /** Deliver everything the run produced, then close it as finished. */
  finish(options: CloseOptions = {}): Promise<Json> {
    return this.close({}, options);
  }

  fail(error: unknown, options: CloseOptions = {}): Promise<Json> {
    return this.close({ status: 'failed', error: describe(error) }, options);
  }

  private async close(body: Json, { timeout = 30_000 }: CloseOptions): Promise<Json> {
    // The flush comes first so that `run.get()` on the next line is over
    // every trace and score the run produced (spec 018 #5). A late span
    // still links, so a flush that timed out is a number read early.
    await flush({ timeout });
    const closed = await request(current(), 'POST', `/api/v1/runs/${this.id}/finish`, { body });
    // Only now: a `finish` the store refused has not closed anything.
    this.closed = true;
    return (closed.body as Json) ?? {};
  }

  /** The run with its summary, as the server computes it (spec 018 #8). */
  async get(): Promise<Json> {
    return ((await request(current(), 'GET', `/api/v1/runs/${this.id}`)).body as Json) ?? {};
  }

  /**
   * The run's cases with the attempts made at each.
   *
   * Unlike a dataset's items, these are budget-checked: every row inlines
   * its input, its output and its scores, so the store's page has to fit
   * `TRACEPAD_RESPONSE_BUDGET_BYTES` and a page of 500 will not. The
   * server's own default is what this asks for; `limit` is for a caller who
   * knows its rows are small (spec 018 #14).
   */
  items(options: RunItemsOptions = {}): AsyncIterable<Json> {
    const params: Record<string, string | number | undefined> = {};
    if (options.limit !== undefined) params.limit = options.limit;
    if (options.unknown) params.unknown = 'true';
    return pages(`/api/v1/runs/${this.id}/items`, params, 'items');
  }

  /**
   * Run the harness's loop and close the run on the way out: `finished` on a
   * clean return, `failed` with the error on a throw, which is rethrown.
   * For a setup without `await using`; the same thing that would do.
   */
  async wrap<T>(fn: (run: this) => T | Promise<T>): Promise<T> {
    try {
      const result = await fn(this);
      if (!this.closed) await this.finish();
      return result;
    } catch (error) {
      if (!this.closed) await this.fail(error);
      throw error;
    }
  }

  /** `await using run = await golden.run(...)`: `finish` at the end of the block. */
  async [Symbol.asyncDispose](): Promise<void> {
    // A run left `running` is reported as such forever (spec 014 #8), and
    // the harness that crashed between the last case and `finish` is exactly
    // the one that forgot to write the `catch`. Disposal cannot see the
    // error, so this is `finish`; `wrap` is the shape that can `fail`.
    if (!this.closed) await this.finish();
  }
}

function describe(error: unknown): string {
  return error instanceof Error ? `${error.name}: ${error.message}` : String(error);
}

export interface ScoreConfig {
  name: string;
  data_type: 'numeric' | 'categorical' | 'boolean' | 'text';
  direction?: 'higher' | 'lower';
  min?: number;
  max?: number;
  categories?: string[];
  description?: string;
}

/**
 * Declare what the run's score names mean, before it runs a case.
 *
 * Sequential and loud (spec 018 #6): a `numeric` without a `direction`
 * should stop the job at the top rather than fail every score batch quietly
 * in the queue. `PUT` is idempotent, so this is safe on every CI run.
 */
export async function scoreConfigs(configs: Iterable<ScoreConfig>): Promise<void> {
  for (const { name, ...body } of configs) {
    try {
      await request(current(), 'PUT', `/api/v1/score-configs/${encodeURIComponent(name)}`, { body });
    } catch (error) {
      // The name in the message, and the status and the body kept: a caller
      // that catches this is entitled to what the store said.
      if (error instanceof TracepadHTTPError) {
        throw new TracepadHTTPError(error.status, `score config ${JSON.stringify(name)}: ${error.body}`);
      }
      throw new TracepadError(`tracepad: score config ${JSON.stringify(name)}: ${(error as Error).message}`);
    }
  }
}

/**
 * The item id of a case, however the harness is holding it: an `Item`, the
 * API's row, or a bare id. Anything else throws here rather than being
 * stamped as an attribute the store cannot read.
 */
function caseId(case_: Item | Json | string): string {
  const found = typeof case_ === 'string' ? case_ : case_.id;
  if (typeof found !== 'string' || found === '') {
    throw new Error(`tracepad: run.item() needs an item id, got ${JSON.stringify(case_)}`);
  }
  return found;
}

/**
 * A stable item or run id from a natural key (spec 018 #7): the same
 * derivation `docs/scores.md` shows for score ids, so that two harnesses
 * hashing the same key agree.
 */
export function itemId(key: string): string {
  return createHash('sha256').update(key).digest('hex').slice(0, 32);
}

/** Two runs side by side, exactly as the server computes it (spec 014 #18). */
export async function compare(a: string, b: string): Promise<Json> {
  return ((await request(current(), 'GET', `/api/v1/runs/${a}/compare/${b}`)).body as Json) ?? {};
}

/**
 * Walk a cursor-paged listing to its end.
 *
 * The loop lives here because this is where `docs/datasets.md` warns a
 * hand-written harness goes wrong: a pass that silently stopped at the first
 * page would be recorded as a whole run over a fraction of the cases. The
 * first request omits `cursor` rather than sending it empty, which is a 400
 * everywhere in this API.
 */
export async function* pages(
  path: string,
  params: Record<string, string | number | undefined>,
  key: string,
): AsyncGenerator<Json, void, undefined> {
  const query = { ...params };
  for (;;) {
    const answer = ((await request(current(), 'GET', path, { params: query })).body as Json) ?? {};
    const rows = answer[key];
    if (Array.isArray(rows)) yield* rows as Json[];
    const cursor = answer.next_cursor;
    if (typeof cursor !== 'string' || cursor === '') return;
    query.cursor = cursor;
  }
}
