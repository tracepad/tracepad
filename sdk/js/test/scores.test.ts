/** Scores: the queue, the timer and the batch (spec 032 #6). */

import { DiagLogLevel, ROOT_CONTEXT, context, diag, trace } from '@opentelemetry/api';
import { AlwaysOffSampler, NodeTracerProvider } from '@opentelemetry/sdk-trace-node';
import { describe, expect, onTestFinished, test, vi } from 'vitest';

import * as tracepad from '../src/index.js';
import { setLogger } from '../src/log.js';
import { ScoreQueue, reset } from '../src/scores.js';
import { HOST, KEY, fakeFetch, fresh, registered, spans, warnings } from './helpers.js';

fresh();

/** A queue whose sends are recorded, standing in for the process-wide one. */
function recording(options: { batchSize?: number; interval?: number; fail?: number } = {}) {
  const sent: unknown[][] = [];
  let failures = options.fail ?? 0;
  const queue = new ScoreQueue(
    async (batch) => {
      if (failures > 0) {
        failures--;
        throw new tracepad.TracepadHTTPError(400, '{"error":"scores[0].value: not a number"}');
      }
      sent.push(batch);
    },
    options,
  );
  reset(queue);
  return { queue, sent };
}

describe('the target', () => {
  test('inside a span is its trace, and its observation on request', () => {
    spans();
    const { sent, queue } = recording();
    let ids: [string, string] | undefined;
    tracepad.span('step', (step) => {
      ids = [step.traceId!, step.spanId!];
      tracepad.score('helpful', 0.9, { comment: 'cited the source' });
      tracepad.score('grounded', 1, { dataType: 'boolean', observation: true });
      tracepad.score('verdict', { stringValue: 'pass', dataType: 'categorical', id: 'v-1' });
    });
    return queue.flush(1000).then(() => {
      expect(sent).toEqual([[
        { name: 'helpful', trace_id: ids![0], value: 0.9, comment: 'cited the source' },
        { name: 'grounded', trace_id: ids![0], observation_id: ids![1], value: 1, data_type: 'boolean' },
        { name: 'verdict', trace_id: ids![0], id: 'v-1', string_value: 'pass', data_type: 'categorical' },
      ]]);
    });
  });

  test('outside a span with no traceId throws; with one it is the target', async () => {
    spans();
    const { sent, queue } = recording();
    expect(() => tracepad.score('helpful', 1)).toThrow('no active span and no traceId');
    tracepad.score('helpful', 1, { traceId: 'a'.repeat(32), observationId: 'b'.repeat(16) });
    await queue.flush(1000);
    expect(sent).toEqual([[{ name: 'helpful', trace_id: 'a'.repeat(32), observation_id: 'b'.repeat(16), value: 1 }]]);
    expect(trace.getActiveSpan()).toBeUndefined();
  });
});

describe('tracing off: no init in this process (spec 039)', () => {
  /** OTel's diagnostic logger at debug, where the line goes: ours exists only after init. */
  function diagnostics(): () => string[] {
    const lines: string[] = [];
    const drop = () => undefined;
    diag.setLogger({ debug: (message) => lines.push(message), verbose: drop, info: drop, warn: drop, error: drop }, DiagLogLevel.DEBUG);
    onTestFinished(() => diag.disable());
    return () => lines.filter((line) => line.startsWith('tracepad:'));
  }

  test('a score without a target is dropped with a debug line, inside a span and outside one', async () => {
    const debug = diagnostics();
    const { sent, queue } = recording();
    const ids = tracepad.span('handler', (step) => {
      tracepad.score('helpful', 1);
      return [step.traceId, step.spanId];
    });
    tracepad.score('helpful', 1, { observation: true });
    await queue.flush(1000);
    expect(sent).toEqual([]);
    expect(debug()).toEqual([
      'tracepad: score(): tracing is off; "helpful" was dropped',
      'tracepad: score(): tracing is off; "helpful" was dropped',
    ]);
    expect(warnings).toEqual([]);
    // No trace behind the span, no id (#3).
    expect(ids).toEqual([undefined, undefined]);
  });

  test('a score by id is sent as ever: that is REST, not tracing (#2)', async () => {
    const { sent, queue } = recording();
    tracepad.span('handler', () => tracepad.score('helpful', 1, { traceId: 'a'.repeat(32) }));
    await queue.flush(1000);
    expect(sent).toEqual([[{ name: 'helpful', trace_id: 'a'.repeat(32), value: 1 }]]);
  });

  test('a propagated parent is not a trace: the no-op tracer echoes the caller\'s ids', async () => {
    // A request came in with a `traceparent`, and a context manager carries it:
    // registering installs one, and disabling the tracer leaves no provider.
    new NodeTracerProvider().register();
    trace.disable();
    const debug = diagnostics();
    const { sent, queue } = recording();
    const caller = { traceId: 'a'.repeat(32), spanId: 'b'.repeat(16), traceFlags: 1, isRemote: true };
    const ids = context.with(trace.setSpanContext(ROOT_CONTEXT, caller), () =>
      tracepad.span('handler', (step) => {
        expect(step.span.spanContext().spanId).toBe('b'.repeat(16)); // the echo itself
        tracepad.score('helpful', 1);
        return [step.traceId, step.spanId];
      }),
    );
    await queue.flush(1000);
    expect(ids).toEqual([undefined, undefined]);
    expect(sent).toEqual([]);
    expect(debug()).toHaveLength(1);
  });

  test("without init, the application's own provider still scores (the spec's first edge case)", async () => {
    new NodeTracerProvider().register();
    const { sent, queue } = recording();
    let later: Promise<void> = Promise.resolve();
    const traceId = tracepad.span('handler', (step) => {
      tracepad.score('helpful', 1);
      // A continuation that scores after the span ended: its trace is as real (spec 039 #7).
      later = new Promise((done) => setTimeout(() => done(tracepad.score('late', 1)), 1));
      return step.traceId;
    });
    await later;
    await queue.flush(1000);
    expect(traceId).toMatch(/^[0-9a-f]{32}$/);
    expect(sent.flat()).toEqual([
      { name: 'helpful', trace_id: traceId, value: 1 },
      { name: 'late', trace_id: traceId, value: 1 },
    ]);
    // Tracing is on here, so outside every span is the programming error it always was.
    expect(() => tracepad.score('helpful', 1)).toThrow('no active span and no traceId');
  });

  test("without init, an unregistered provider's recording span still scores (#8)", async () => {
    // A provider wired into the framework and not registered globally; the
    // context manager comes from a registration whose tracer is then disabled.
    new NodeTracerProvider().register();
    trace.disable();
    const request = new NodeTracerProvider().getTracer('the.framework').startSpan('GET /answer');
    const { sent, queue } = recording();
    context.with(trace.setSpan(context.active(), request), () => tracepad.score('helpful', 1));
    request.end();
    await queue.flush(1000);
    expect(sent).toEqual([[{ name: 'helpful', trace_id: request.spanContext().traceId, value: 1 }]]);
  });

  test('without init, a sampled-out span keeps its ids: they are propagated whether or not kept', () => {
    new NodeTracerProvider({ sampler: new AlwaysOffSampler() }).register();
    const ids = tracepad.span('handler', (step) => {
      expect(step.span.isRecording()).toBe(false);
      return [step.traceId, step.spanId];
    });
    expect(ids[0]).toMatch(/^[0-9a-f]{32}$/);
    expect(ids[1]).toMatch(/^[0-9a-f]{16}$/);
  });

  test('with no diagnostic logger the drop is silent: nothing on stdout, nothing in our logger', () => {
    recording();
    const out = vi.spyOn(console, 'log').mockImplementation(() => undefined);
    const debug = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
    expect(() => tracepad.score('helpful', 1)).not.toThrow();
    expect(out).not.toHaveBeenCalled();
    expect(debug).not.toHaveBeenCalled();
    expect(warnings).toEqual([]);
  });
});

describe('the queue', () => {
  test('sends a full batch at once and the rest after the interval', async () => {
    vi.useFakeTimers();
    const { sent, queue } = recording({ batchSize: 3, interval: 2000 });
    for (let i = 0; i < 4; i++) queue.submit({ name: 's', trace_id: 't', value: i });
    await vi.advanceTimersByTimeAsync(0);
    expect(sent.map((b) => b.length)).toEqual([3]);
    await vi.advanceTimersByTimeAsync(1999);
    expect(sent).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(sent.map((b) => b.length)).toEqual([3, 1]);
  });

  test('batches at 100 and every 2 seconds by default', async () => {
    vi.useFakeTimers();
    const { sent, queue } = recording();
    for (let i = 0; i < 100; i++) queue.submit({ name: 's', trace_id: 't', value: i });
    await vi.advanceTimersByTimeAsync(0);
    expect(sent.map((b) => b.length)).toEqual([100]);
    queue.submit({ name: 's', trace_id: 't', value: 100 });
    await vi.advanceTimersByTimeAsync(2000);
    expect(sent.map((b) => b.length)).toEqual([100, 1]);
  });

  test('retries a refused batch once, then drops it with the server\'s message', async () => {
    const one = recording({ fail: 1 });
    one.queue.submit({ name: 's', trace_id: 't' });
    await one.queue.flush(1000);
    expect(one.sent).toHaveLength(1);
    expect(warnings).toEqual([]);

    const two = recording({ fail: 2 });
    two.queue.submit({ name: 's', trace_id: 't' });
    two.queue.submit({ name: 's', trace_id: 't' });
    await two.queue.flush(1000);
    expect(two.sent).toEqual([]);
    expect(warnings).toEqual([expect.stringContaining('2 score(s) dropped: TracepadHTTPError: tracepad: HTTP 400: {"error":"scores[0].value: not a number"}')]);
  });

  test('a closed queue says so rather than losing the score', () => {
    const { queue, sent } = recording();
    queue.close();
    queue.submit({ name: 'late', trace_id: 't' });
    expect(sent).toEqual([]);
    expect(warnings).toEqual(['tracepad: score "late" dropped, the queue is closed']);
  });

  test('flush waits for what is in flight and warns past the timeout', async () => {
    // Fake timers, not a short real one: the timeout is a clock, and a test
    // that waits it out on the real one is a test of the machine's load.
    vi.useFakeTimers();
    let release!: () => void;
    const queue = new ScoreQueue(() => new Promise<void>((done) => (release = done)));
    reset(queue);
    queue.submit({ name: 's', trace_id: 't' });
    const flushed = queue.flush(10);
    await vi.advanceTimersByTimeAsync(9);
    expect(warnings).toEqual([]);
    await vi.advanceTimersByTimeAsync(1);
    await flushed;
    expect(warnings).toEqual(['tracepad: the score queue did not drain within 10ms']);
    release();
  });

  test('posts POST /api/v1/scores with the bearer', async () => {
    const calls = fakeFetch(() => ({ status: 200, body: { scores: [] } }));
    tracepad.init({ host: HOST, key: KEY, export: false });
    tracepad.score('helpful', 1, { traceId: 'a'.repeat(32) });
    await tracepad.flush();
    expect(calls).toEqual([{
      method: 'POST',
      url: `${HOST}/api/v1/scores`,
      headers: expect.objectContaining({ Authorization: `Bearer ${KEY}`, 'Content-Type': 'application/json' }),
      body: [{ name: 'helpful', trace_id: 'a'.repeat(32), value: 1 }],
    }]);
  });
});

describe('flush', () => {
  test('drains the scores, then the provider', async () => {
    const order: string[] = [];
    const queue = new ScoreQueue(async () => {
      order.push('scores');
    });
    reset(queue);
    tracepad.init({ host: HOST, key: KEY, export: false });
    const provider = registered() as NodeTracerProvider;
    provider.forceFlush = async () => {
      order.push('spans');
    };
    tracepad.score('helpful', 1, { traceId: 'a'.repeat(32) });
    await tracepad.flush();
    expect(order).toEqual(['scores', 'spans']);
  });

  test('a score queue that ate the budget leaves the spans to their schedule, and says so', async () => {
    // Fake timers: `flush` reads what is left of the budget off `performance.now()`
    // after a real 10ms timer, and a timer may fire a hair before the clock says
    // 10 — then `left` is positive and the spans are forced, which is the flake
    // this replaced. Faked, the clock and the timer are the same one.
    vi.useFakeTimers();
    const queue = new ScoreQueue(() => new Promise<void>(() => undefined));
    reset(queue);
    tracepad.init({ host: HOST, key: KEY, export: false });
    const forced = vi.fn(async () => undefined);
    (registered() as NodeTracerProvider).forceFlush = forced;
    tracepad.score('helpful', 1, { traceId: 'a'.repeat(32) });
    const flushed = tracepad.flush({ timeout: 10 });
    await vi.advanceTimersByTimeAsync(10);
    await flushed;
    expect(forced).not.toHaveBeenCalled();
    expect(warnings).toEqual([
      'tracepad: the score queue did not drain within 10ms',
      expect.stringContaining('used the whole 10ms budget'),
    ]);
  });

  test('never rejects: a provider whose flush fails is warned about', async () => {
    tracepad.init({ host: HOST, key: KEY, export: false });
    (registered() as NodeTracerProvider).forceFlush = async () => {
      throw new Error('collector down');
    };
    const unhandled: unknown[] = [];
    const listener = (reason: unknown) => unhandled.push(reason);
    process.on('unhandledRejection', listener);
    try {
      await expect(tracepad.flush()).resolves.toBeUndefined();
      process.emit('beforeExit', 0);
      await new Promise((tick) => setTimeout(tick, 10));
    } finally {
      process.off('unhandledRejection', listener);
    }
    expect(unhandled).toEqual([]);
    expect(warnings).toEqual([
      'tracepad: flush(): the span processors failed to flush: Error: collector down',
      'tracepad: flush(): the span processors failed to flush: Error: collector down',
    ]);
  });

  test('runs at beforeExit on its own, once per firing, and again for what a later handler left', async () => {
    const queue = new ScoreQueue(async () => undefined);
    reset(queue);
    const flushed = vi.spyOn(queue, 'flush');
    tracepad.init({ host: HOST, key: KEY, export: false });
    process.emit('beforeExit', 0);
    process.emit('beforeExit', 0); // while the first flush is still in flight
    await new Promise((tick) => setTimeout(tick, 0));
    expect(flushed).toHaveBeenCalledTimes(1);
    process.emit('beforeExit', 0); // the loop drained again: another library's handler may have queued more
    await new Promise((tick) => setTimeout(tick, 0));
    expect(flushed).toHaveBeenCalledTimes(2);
  });
});
