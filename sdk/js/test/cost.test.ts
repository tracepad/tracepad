/** What the package costs when nothing records, and how long it waits (spec 042). */

import { DiagLogLevel, ROOT_CONTEXT, context, diag, trace } from '@opentelemetry/api';
import {
  BatchSpanProcessor,
  type ReadableSpan,
  type SpanExporter,
  type SpanProcessor,
} from '@opentelemetry/sdk-trace-node';
import { describe, expect, onTestFinished, test } from 'vitest';

import * as attrs from '../src/attributes.js';
import * as tracepad from '../src/index.js';
import { capture } from '../src/testing.js';
import { HOST, KEY, fresh, registered, spans, warnings } from './helpers.js';

fresh();

/** A value whose every serialisation is counted: JSON reaches it through `toJSON`. */
let dumped = 0;
const counted = () => ({ toJSON: () => (dumped++, '<counted>') });

/** Every door that serialises, each handed one counted value. */
function serialiseEverything(): void {
  dumped = 0;
  const step = tracepad.observe((thing: unknown) => thing, { name: 'step' });
  tracepad.span('s', { input: counted(), metadata: { k: counted() } }, (observation) => {
    observation.update({ output: counted(), metadata: { j: counted() } });
    tracepad.update({ input: counted() });
    tracepad.updateTrace({ metadata: { t: counted() } });
    step(counted());
    tracepad.generation('g', { model: 'm', modelParameters: { p: counted() }, input: counted() }, (call) =>
      call.end(undefined, { output: counted() }),
    );
  });
}

/** Under a caller that chose not to sample: the default sampler drops every span. */
function sampledOut<T>(fn: () => T): T {
  const parent = trace.wrapSpanContext({ traceId: 'a'.repeat(32), spanId: 'b'.repeat(16), traceFlags: 0, isRemote: true });
  return context.with(trace.setSpan(ROOT_CONTEXT, parent), fn);
}

/** OTel's diagnostic logger at debug, where the no-op says what it did; OTel's own lines are left out. */
function diagnostics(): string[] {
  const lines: string[] = [];
  const drop = () => undefined;
  diag.setLogger({ debug: (message) => void (message.startsWith('tracepad:') && lines.push(message)), verbose: drop, info: drop, warn: drop, error: drop }, DiagLogLevel.DEBUG);
  onTestFinished(() => diag.disable());
  return lines;
}

describe('the costly attributes (Decision 1)', () => {
  test('are not serialised for a span sampled out', () => {
    const captured = capture();
    sampledOut(serialiseEverything);
    expect(captured.spans).toEqual([]);
    expect(dumped).toBe(0);
  });

  test('are not serialised with tracing off', () => {
    serialiseEverything();
    expect(dumped).toBe(0);
  });

  test('are serialised once each for a span that records', () => {
    const captured = capture();
    serialiseEverything();
    expect(dumped).toBe(11); // the eleven values above, each once
    expect(captured.attributes('s')[attrs.INPUT]).toBe('"<counted>"');
  });

  test('leave the cheap ones where a sampler reads them, at start', () => {
    const seen: unknown[] = [];
    const watching: SpanProcessor = {
      onStart: (span) => void seen.push({ ...(span as unknown as ReadableSpan).attributes }),
      onEnd: () => undefined,
      forceFlush: async () => undefined,
      shutdown: async () => undefined,
    };
    const captured = capture();
    (registered() as unknown as { _activeSpanProcessor: { _spanProcessors: SpanProcessor[] } })._activeSpanProcessor._spanProcessors.push(watching);
    tracepad.generation('chat', { model: 'gpt-4o-mini', input: 'hello', metadata: { a: 1 } }, () => undefined);
    expect(seen).toEqual([{ [attrs.OBSERVATION_TYPE]: 'generation', [attrs.REQUEST_MODEL]: 'gpt-4o-mini' }]);
    expect(captured.one('chat').attributes[attrs.INPUT]).toBe('hello');
  });
});

describe('update and updateTrace (Decision 2)', () => {
  test('with tracing off, inside a block, say so at debug', () => {
    const debug = diagnostics();
    tracepad.span('off', () => {
      tracepad.update({ output: 'x' });
      tracepad.updateTrace({ userId: 'u-1' });
    });
    expect(debug).toEqual([
      'tracepad: update(): the span does not record; nothing was written',
      'tracepad: updateTrace(): the span does not record; nothing was written',
    ]);
    expect(warnings).toEqual([]);
  });

  test('under a span sampled out, say so at debug', () => {
    const captured = capture();
    const debug = diagnostics();
    sampledOut(() => tracepad.span('dropped', () => tracepad.update({ output: 'x' })));
    expect(debug).toHaveLength(1);
    expect(warnings).toEqual([]);
    expect(captured.spans).toEqual([]);
  });

  test('with tracing off, outside every block, are quiet', () => {
    tracepad.update({ output: 'x' });
    expect(warnings).toEqual([]);
  });

  test('in a process that traces, outside every block, still warn', () => {
    spans();
    tracepad.update({ output: 'x' });
    tracepad.updateTrace({ userId: 'u-1' });
    expect(warnings).toEqual([
      'tracepad: update() outside a span: nothing was written',
      'tracepad: updateTrace() outside a span: nothing was written',
    ]);
  });
});

describe('the export timeout (Decision 3)', () => {
  const timeout = () => {
    const [ours] = (registered() as unknown as { _activeSpanProcessor: { _spanProcessors: { exporting: { _exporter: { _delegate: { _timeout: number } } } }[] } })._activeSpanProcessor._spanProcessors;
    return ours!.exporting._exporter._delegate._timeout;
  };

  test('is five seconds by default', () => {
    tracepad.init({ host: HOST, key: KEY });
    expect(timeout()).toBe(5000);
  });

  test('is read from TRACEPAD_EXPORT_TIMEOUT, in seconds', () => {
    process.env.TRACEPAD_EXPORT_TIMEOUT = '2.5';
    onTestFinished(() => void delete process.env.TRACEPAD_EXPORT_TIMEOUT);
    tracepad.init({ host: HOST, key: KEY });
    expect(timeout()).toBe(2500);
  });

  test('the option wins over the variable', () => {
    process.env.TRACEPAD_EXPORT_TIMEOUT = '2.5';
    onTestFinished(() => void delete process.env.TRACEPAD_EXPORT_TIMEOUT);
    tracepad.init({ host: HOST, key: KEY, exportTimeoutMillis: 1500 });
    expect(timeout()).toBe(1500);
  });

  test("OpenTelemetry's own variable works when neither is given", () => {
    process.env.OTEL_EXPORTER_OTLP_TRACES_TIMEOUT = '7000';
    onTestFinished(() => void delete process.env.OTEL_EXPORTER_OTLP_TRACES_TIMEOUT);
    tracepad.init({ host: HOST, key: KEY });
    expect(timeout()).toBe(7000);
  });

  test('a variable that is not seconds is ignored with a warning', () => {
    process.env.TRACEPAD_EXPORT_TIMEOUT = '5s';
    onTestFinished(() => void delete process.env.TRACEPAD_EXPORT_TIMEOUT);
    tracepad.init({ host: HOST, key: KEY });
    expect(timeout()).toBe(5000);
    expect(warnings).toEqual(['tracepad: TRACEPAD_EXPORT_TIMEOUT="5s" is not a number of seconds; it is ignored']);
  });

  test('is ignored with a warning when nothing is exported', () => {
    tracepad.init({ host: HOST, key: KEY, export: false, exportTimeoutMillis: 1000 });
    expect(warnings).toEqual([expect.stringContaining('exportTimeoutMillis is ignored with export: false')]);
  });
});

describe('flush (Decision 4)', () => {
  test('returns within its timeout against an export that hangs, and says so', async () => {
    let release = () => undefined as void;
    const hanging: SpanExporter = {
      export: (_spans, done) => void new Promise<void>((resolve) => (release = resolve)).then(() => done({ code: 0 })),
      shutdown: async () => release(),
    };
    tracepad.init({ host: HOST, key: KEY, export: false });
    const provider = registered() as unknown as { _activeSpanProcessor: { _spanProcessors: SpanProcessor[] } };
    provider._activeSpanProcessor._spanProcessors.push(new BatchSpanProcessor(hanging));
    onTestFinished(() => release());
    tracepad.span('pending', () => undefined);

    const started = performance.now();
    await tracepad.flush({ timeout: 200 });
    expect(performance.now() - started).toBeLessThan(200 + 300);
    expect(warnings).toEqual(['tracepad: flush(): the span processors did not flush within 200ms']);
  });
});

test('metadata merges by key: update adds keys and replaces only its own (Decision 5)', () => {
  const captured = spans();
  tracepad.span('step', { metadata: { a: 1, keep: 'x' } }, (step) => {
    step.update({ metadata: { b: { nested: true } } });
    tracepad.update({ metadata: { a: 3, keep: undefined, gone: null } });
  });
  const prefix = attrs.OBSERVATION_METADATA;
  const metadata = Object.entries(captured.attributes('step')).filter(([key]) => key.startsWith(prefix));
  expect(Object.fromEntries(metadata)).toEqual({
    [`${prefix}.a`]: 3,
    [`${prefix}.keep`]: 'x', // a key without a value writes nothing, and deletes nothing
    [`${prefix}.b`]: '{"nested":true}',
  });
});

describe('found in review of PR #83', () => {
  const timeout = () => {
    const [ours] = (registered() as unknown as { _activeSpanProcessor: { _spanProcessors: { exporting: { _exporter: { _delegate: { _timeout: number } } } }[] } })._activeSpanProcessor._spanProcessors;
    return ours!.exporting._exporter._delegate._timeout;
  };

  test.each([0, -1, Number.NaN, 3e9])('exportTimeoutMillis %s is ignored with a warning, and init does not throw', (given) => {
    tracepad.init({ host: HOST, key: KEY, exportTimeoutMillis: given });
    expect(timeout()).toBe(5000);
    expect(warnings).toEqual([`tracepad: exportTimeoutMillis=${String(given)} is not a positive number of milliseconds; it is ignored`]);
  });

  test('a TRACEPAD_EXPORT_TIMEOUT past what a timer holds is ignored with a warning', () => {
    process.env.TRACEPAD_EXPORT_TIMEOUT = '3000000';
    onTestFinished(() => void delete process.env.TRACEPAD_EXPORT_TIMEOUT);
    tracepad.init({ host: HOST, key: KEY });
    expect(timeout()).toBe(5000);
    expect(warnings).toEqual([expect.stringContaining('TRACEPAD_EXPORT_TIMEOUT="3000000" is not a number of seconds')]);
  });

  test('a metadata value that encodes as a JSON string is that string, without its quotes', () => {
    const captured = spans();
    tracepad.span('step', { metadata: { at: new Date(Date.UTC(2026, 8, 23, 10)) } }, () => undefined);
    expect(captured.attributes('step')[`${attrs.OBSERVATION_METADATA}.at`]).toBe('2026-09-23T10:00:00.000Z');
  });

  test.each([['a', 'b'], 'nightly'])('metadata that is no object is written whole: %j', (given) => {
    const captured = spans();
    tracepad.span('step', { metadata: given as unknown as Record<string, unknown> }, () => undefined);
    const metadata = Object.entries(captured.attributes('step')).filter(([key]) => key.startsWith(attrs.OBSERVATION_METADATA));
    expect(Object.fromEntries(metadata)).toEqual({ [attrs.OBSERVATION_METADATA]: attrs.dumps(given) });
  });
});

describe('found in the second review of PR #83', () => {
  const metadataOf = (captured: ReturnType<typeof spans>, name: string) =>
    Object.fromEntries(Object.entries(captured.attributes(name)).filter(([key]) => key.startsWith(attrs.OBSERVATION_METADATA)));

  test('metadata past the key cap, or with an empty key, is written whole, and the step keeps its own attributes', () => {
    const captured = spans();
    const many = Object.fromEntries(Array.from({ length: attrs.MAX_METADATA_KEYS + 1 }, (_, i) => [`k${i}`, i]));
    tracepad.generation('chat', { model: 'm', metadata: many }, () => undefined);
    tracepad.span('empty', { metadata: { '': 1, a: 2 } }, () => undefined);
    expect(metadataOf(captured, 'chat')).toEqual({ [attrs.OBSERVATION_METADATA]: attrs.dumps(many) });
    expect(captured.attributes('chat')[attrs.OBSERVATION_TYPE]).toBe('generation');
    expect(metadataOf(captured, 'empty')).toEqual({ [attrs.OBSERVATION_METADATA]: '{"":1,"a":2}' });
  });

  test('an object that is no plain record is taken as it encodes; a bigint is a number', () => {
    const captured = spans();
    class Tagged {
      toJSON() {
        return { tag: 'x' };
      }
    }
    tracepad.span('tagged', { metadata: new Tagged() as unknown as Record<string, unknown> }, () => undefined);
    tracepad.span('dated', { metadata: new Date(Date.UTC(2026, 8, 23)) as unknown as Record<string, unknown> }, () => undefined);
    tracepad.span('big', { metadata: { id: 10n } }, () => undefined);
    expect(metadataOf(captured, 'tagged')).toEqual({ [`${attrs.OBSERVATION_METADATA}.tag`]: 'x' });
    expect(metadataOf(captured, 'dated')).toEqual({ [attrs.OBSERVATION_METADATA]: '"2026-09-23T00:00:00.000Z"' });
    expect(metadataOf(captured, 'big')).toEqual({ [`${attrs.OBSERVATION_METADATA}.id`]: 10 });
  });

  test('a stream gathers nothing for a span that does not record', async () => {
    const captured = capture();
    const chunks = [{ model: 'm', choices: [{ delta: { content: 'hi' } }] }];
    await sampledOut(() =>
      tracepad.generation('chat', async (call) => {
        const seen = [];
        for await (const chunk of call.stream(chunks)) seen.push(chunk);
        expect(seen).toEqual(chunks);
        expect((call as unknown as { streamed: unknown }).streamed).toBeUndefined();
      }),
    );
    expect(captured.spans).toEqual([]);
  });
});
