/**
 * Scores: a queue, a timer and a batch (spec 032 #6).
 *
 * Scores are written from inside request handlers, and the OTel exporter has
 * already shown what the right shape is there. A scoring failure must not
 * fail the request that produced the trace, so a batch the server refused is
 * logged and dropped rather than thrown — the same asymmetry the exporter
 * has. One retry, not more: a 400 is deterministic, and a queue that retries
 * forever is a memory leak with a log line. The timer is `unref`'d, so a
 * program that is done is not held open by it.
 */

import { diag, isSpanContextValid, trace } from '@opentelemetry/api';

import { current } from './config.js';
import { describe, request } from './http.js';
import { warn } from './log.js';
import { tracingOff } from './tracing.js';

export const BATCH_SIZE = 100;
export const INTERVAL = 2000;

export type ScoreBody = Record<string, unknown>;

export interface QueueOptions {
  batchSize?: number;
  interval?: number;
}

/** A background sender: up to `batchSize` scores every `interval` milliseconds. */
export class ScoreQueue {
  private readonly send: (batch: ScoreBody[]) => Promise<void>;
  private readonly batchSize: number;
  private readonly interval: number;
  private queued: ScoreBody[] = [];
  private timer: NodeJS.Timeout | undefined;
  private readonly inFlight = new Set<Promise<void>>();
  private closed = false;

  constructor(send: (batch: ScoreBody[]) => Promise<void>, options: QueueOptions = {}) {
    this.send = send;
    this.batchSize = options.batchSize ?? BATCH_SIZE;
    this.interval = options.interval ?? INTERVAL;
  }

  submit(score: ScoreBody): void {
    if (this.closed) {
      // Saying so is the point: a score that went nowhere quietly is the
      // failure this API is worst at surfacing.
      warn(`score ${JSON.stringify(score.name)} dropped, the queue is closed`);
      return;
    }
    this.queued.push(score);
    if (this.queued.length >= this.batchSize) {
      this.drain();
    } else if (this.timer === undefined) {
      this.timer = setTimeout(() => this.drain(), this.interval);
      this.timer.unref();
    }
  }

  /** Send everything queued so far, and wait for every batch in flight. */
  async flush(timeout: number): Promise<void> {
    this.drain();
    if (this.inFlight.size === 0) return;
    const done = await Promise.race([
      Promise.all(this.inFlight).then(() => true),
      new Promise<boolean>((resolve) => setTimeout(() => resolve(false), timeout).unref()),
    ]);
    if (!done) warn(`the score queue did not drain within ${timeout}ms`);
  }

  close(): void {
    this.closed = true;
  }

  /** Stop without sending what is queued. */
  drop(): void {
    this.closed = true;
    this.queued = [];
    clearTimeout(this.timer);
  }

  private drain(): void {
    if (this.timer !== undefined) {
      clearTimeout(this.timer);
      this.timer = undefined;
    }
    while (this.queued.length > 0) {
      const batch = this.queued.splice(0, this.batchSize);
      const delivery = this.deliver(batch).finally(() => this.inFlight.delete(delivery));
      this.inFlight.add(delivery);
    }
  }

  private async deliver(batch: ScoreBody[]): Promise<void> {
    // Anything, not just a `TracepadError`: whatever the send throws, the
    // batch is dropped and the queue lives.
    for (let attempt = 1; attempt <= 2; attempt++) {
      try {
        await this.send(batch);
        return;
      } catch (error) {
        if (attempt === 2) warn(`${batch.length} score(s) dropped: ${describe(error)}`);
      }
    }
  }
}

async function post(batch: ScoreBody[]): Promise<void> {
  await request(current(), 'POST', '/api/v1/scores', { body: batch });
}

let queue: ScoreQueue | undefined;

export function queueOf(): ScoreQueue {
  if (queue === undefined) queue = new ScoreQueue(post);
  return queue;
}

/** Replace the process-wide queue, for `tracepad/testing`. The one replaced
 * stops and drops what it holds: sent later, it would go to whatever store
 * the next test configured (spec 040 #14). */
export function reset(replacement?: ScoreQueue): void {
  queue?.drop();
  queue = replacement;
}

/** Drain the queue, and report how many milliseconds of the timeout are left. */
export async function flushScores(timeout: number): Promise<number> {
  const started = performance.now();
  if (queue !== undefined) await queue.flush(timeout);
  return timeout - (performance.now() - started);
}

export interface ScoreFields {
  stringValue?: string;
  dataType?: 'numeric' | 'categorical' | 'boolean' | 'text';
  comment?: string;
  id?: string;
  traceId?: string;
  observationId?: string;
  /** Target the active observation too, not only its trace. */
  observation?: boolean;
}

/**
 * Score the trace (or the observation) in flight (spec 017 #7).
 *
 * With no target given, the target is the active span's trace — and its
 * span id too when `observation` is true. Outside a span, with no `traceId`,
 * this throws: a score that silently went nowhere is the failure this API is
 * worst at surfacing, and a programming error visible at the call site is
 * the one exception to "the tracing path never throws".
 */
export function score(name: string, value?: number | ScoreFields, fields: ScoreFields = {}): void {
  if (typeof value === 'object') {
    fields = value;
    value = undefined;
  }
  let { traceId, observationId } = fields;
  if (traceId === undefined) {
    // A no-op global echoes a propagated parent, which never records; a live span of the app's own
    // provider does. OTel's `diag` gets the line: no logger of ours exists before `init` (spec 039 #8).
    const span = trace.getActiveSpan();
    if (tracingOff() && !span?.isRecording()) return diag.debug(`tracepad: score(): tracing is off; ${JSON.stringify(name)} was dropped`);
    const active = span?.spanContext();
    if (active === undefined || !isSpanContextValid(active)) {
      throw new Error('tracepad: score(): no active span and no traceId; pass traceId');
    }
    traceId = active.traceId;
    if (fields.observation && observationId === undefined) observationId = active.spanId;
  }
  const body: ScoreBody = { name, trace_id: traceId };
  for (const [key, given] of [
    ['id', fields.id],
    ['observation_id', observationId],
    ['value', value],
    ['string_value', fields.stringValue],
    ['data_type', fields.dataType],
    ['comment', fields.comment],
  ] as const) {
    if (given !== undefined) body[key] = given;
  }
  queueOf().submit(body);
}
