/**
 * Testing an application's instrumentation (spec 040).
 *
 *     import { capture } from 'tracepad/testing';
 *
 *     test('the answer is traced', async () => {
 *       using captured = capture(); // or restore() in afterEach
 *       await answer('why is the sky blue');
 *       expect(captured.one('answer').attributes['tracepad.observation.type']).toBe('span');
 *       expect(captured.scores[0]?.name).toBe('helpful');
 *     });
 *
 * `capture()` gives a fresh, initialised process that records instead of
 * exporting; `reset()` gives one that never initialised — tracing off, as
 * spec 039 defines it. Everything the package keeps process-wide is reset
 * here and nowhere else, OpenTelemetry's globals included, which the API
 * lets a process register once and a suite has to register once per test
 * (spec 040 #3). A subpath of its own, so no application bundle carries it.
 */

import { type Attributes, context, propagation } from '@opentelemetry/api';
import {
  InMemorySpanExporter,
  NodeTracerProvider,
  type ReadableSpan,
  SimpleSpanProcessor,
} from '@opentelemetry/sdk-trace-node';

import * as config from './config.js';
import { setLogger } from './log.js';
import * as prompts from './prompts.js';
import * as scores from './scores.js';
import * as tracing from './tracing.js';

tracing.follow();

// A reserved name (RFC 2606), so nothing resolves it: export is off and the
// queue does not post, and a REST call a test forgot to stub fails loudly.
const HOST = 'http://tracepad.test:4318';
const KEY = 'tp-sk-test';

/** Return the process to never initialised: no `init`, no global provider, no
 * cached configuration or prompts, no score queue, the default logger (spec 040 #2). */
export function reset(): void {
  tracing.reset();
  context.disable();
  propagation.disable();
  config.forget();
  prompts.forget();
  scores.reset();
  setLogger(console);
}

/** What the code under test traced and scored, since `capture()`. */
export class Capture implements Disposable {
  /** The bodies `score()` would have posted, in order. */
  readonly scores: Record<string, unknown>[] = [];
  private readonly exporter = new InMemorySpanExporter();

  /** @internal `capture()` is the way in. */
  constructor() {
    reset();
    // A configuration of its own, not the environment's: nothing is sent, and
    // TRACEPAD_ENVIRONMENT would only draw the resource warning.
    const own = { host: HOST, key: KEY, environment: '', release: '' };
    // Behind the follower, so a tracer the application took at import records
    // here too; registered, so an app's own registration is refused as in any
    // process that has one (spec 040 #14).
    tracing.install(new NodeTracerProvider({
      spanProcessors: [new SimpleSpanProcessor(this.exporter), tracing.spanProcessor({ ...own, export: false })],
    }));
    tracing.init(own);
    // A batch of one is sent the moment it is queued, and this send is synchronous.
    scores.reset(new scores.ScoreQueue(async (batch) => void this.scores.push(...batch), { batchSize: 1 }));
  }

  /** Every finished span, in the order it ended. */
  get spans(): ReadableSpan[] {
    return [...this.exporter.getFinishedSpans()];
  }

  /** The single finished span of that name; throws naming what there was. */
  one(name: string): ReadableSpan {
    const found = this.spans.filter((span) => span.name === name);
    if (found.length !== 1) {
      throw new Error(`want one span named ${JSON.stringify(name)}, got ${JSON.stringify(this.spans.map((s) => s.name))}`);
    }
    return found[0]!;
  }

  attributes(name: string): Attributes {
    return { ...this.one(name).attributes };
  }

  /** Reset the process again; the spans and scores stay readable. */
  restore(): void {
    reset();
  }

  [Symbol.dispose](): void {
    reset();
  }
}

/** Reset the process, then trace into memory until `restore()` (spec 040 #1). */
export function capture(): Capture {
  return new Capture();
}
