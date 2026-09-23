/** `tracepad/testing`: the capture, the reset, the entry point (spec 040). */

import { execFileSync } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { mkdtempSync, rmSync, writeFileSync, readFileSync } from 'node:fs';

import { context, propagation, trace } from '@opentelemetry/api';
import { InMemorySpanExporter, NodeTracerProvider, SimpleSpanProcessor } from '@opentelemetry/sdk-trace-node';
import { afterAll, beforeAll, describe, expect, test, vi } from 'vitest';

import * as tracepad from '../src/index.js';
import { ScoreQueue, reset as resetScores } from '../src/scores.js';
import { capture, reset } from '../src/testing.js';
import { fresh } from './helpers.js';

fresh();

describe('a capture', () => {
  test('records the spans in the order they ended, and the scores', () => {
    const captured = capture();
    let traceId: string | undefined;
    tracepad.span('handler', (handler) => {
      traceId = handler.traceId;
      tracepad.span('retrieve', () => {});
      tracepad.score('helpful', 0.9);
    });
    captured.restore();

    expect(captured.spans.map((s) => s.name)).toEqual(['retrieve', 'handler']);
    expect(captured.attributes('handler')['tracepad.observation.type']).toBe('span');
    expect(captured.scores).toEqual([{ name: 'helpful', trace_id: traceId, value: 0.9 }]);
  });

  test('one() throws naming the spans there were', () => {
    const captured = capture();
    for (const name of ['a', 'a', 'b']) tracepad.span(name, () => {});

    expect(() => captured.one('a')).toThrow('want one span named "a", got ["a","a","b"]');
    expect(() => captured.one('c')).toThrow('named "c"');
  });

  test('reaches no network', async () => {
    vi.stubGlobal('fetch', () => {
      throw new Error('a capture made a network call');
    });
    const captured = capture();
    tracepad.generation('chat', { model: 'gpt-4o-mini' }, (call) => {
      call.end({ output: 'hi', usage: { input: 3, output: 1 } });
      tracepad.score('helpful', 1, { observation: true });
    });
    await tracepad.flush({ timeout: 1000 });

    expect(captured.one('chat').attributes).toBeDefined();
    expect(captured.scores).toHaveLength(1);
  });

  test('leaves a process that never initialised', () => {
    capture().restore();

    tracepad.span('after', (step) => {
      tracepad.score('helpful', 1); // a no-op, not a throw (spec 039)
      expect([step.traceId, step.spanId]).toEqual([undefined, undefined]);
    });
  });

  test('does not see the one before it', () => {
    const first = capture();
    tracepad.span('first', () => tracepad.score('one', 1));
    const second = capture();
    tracepad.span('second', () => tracepad.score('two', 2));
    second.restore();

    expect(first.spans.map((s) => s.name)).toEqual(['first']);
    expect(second.spans.map((s) => s.name)).toEqual(['second']);
    expect(second.scores.map((s) => s.name)).toEqual(['two']);
  });

  test('propagates the trace context, as an initialised process does', () => {
    capture();
    const headers: Record<string, string> = {};
    tracepad.span('call', () => propagation.inject(context.active(), headers));
    expect(headers.traceparent).toMatch(/^00-[0-9a-f]{32}-[0-9a-f]{16}-01$/);
  });

  test('is disposed by `using`', () => {
    {
      using captured = capture();
      tracepad.span('inside', () => {});
      expect(captured.spans).toHaveLength(1);
    }
    tracepad.span('outside', (step) => expect(step.traceId).toBeUndefined());
  });
});

describe('the reset', () => {
  test('keeps a tracer taken at import recording in every capture (spec 040 #14)', () => {
    const tracer = trace.getTracer('app'); // before any provider, as a module does
    for (const name of ['first', 'second']) {
      const captured = capture();
      tracer.startActiveSpan(name, (root) => {
        tracepad.span('step', () => {});
        root.end();
      });
      captured.restore();
      expect(captured.spans.map((s) => s.name)).toEqual(['step', name]);
      expect(captured.one('step').parentSpanContext?.spanId).toBe(captured.one(name).spanContext().spanId);
    }
  });

  test('keeps a tracer first used under init recording in later captures', () => {
    const tracer = trace.getTracer('app');
    tracepad.init({ host: 'http://tracepad.test:4318', key: 'tp-sk-test', export: false });
    expect(tracer.startSpan('under init').isRecording()).toBe(true);
    reset();
    expect(tracer.startSpan('off').isRecording()).toBe(false);
    const captured = capture();
    tracer.startSpan('captured').end();
    expect(captured.spans.map((s) => s.name)).toEqual(['captured']);
  });

  test('undoes the registration the API allows once, which a capture holds (spec 040 #3)', () => {
    const own = () => new NodeTracerProvider({ spanProcessors: [new SimpleSpanProcessor(new InMemorySpanExporter())] });
    const captured = capture();
    expect(trace.setGlobalTracerProvider(own())).toBe(false); // an app factory's, refused
    tracepad.span('answer', () => {});
    expect(captured.spans.map((s) => s.name)).toEqual(['answer']);
    captured.restore();
    const tracer = trace.getTracer('app');
    expect(trace.setGlobalTracerProvider(own())).toBe(true);
    expect(tracer.startSpan('app').isRecording()).toBe(true);
  });

  test('shuts down the provider init built', () => {
    const shutdown = vi.spyOn(NodeTracerProvider.prototype, 'shutdown');
    tracepad.init({ host: 'http://tracepad.test:4318', key: 'tp-sk-test' });
    reset();
    expect(shutdown).toHaveBeenCalledOnce();
  });

  test('stops the queue it replaced, and drops what it held', async () => {
    const sent: unknown[] = [];
    resetScores(new ScoreQueue(async (batch) => void sent.push(batch), { interval: 10 }));
    tracepad.score('pending', 1, { traceId: 'a'.repeat(32) });
    reset();
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(sent).toEqual([]);
  });
});

test('a capture reads nothing from the environment', () => {
  process.env.TRACEPAD_ENVIRONMENT = 'ci';
  process.env.TRACEPAD_RELEASE = '1.2.3';
  const warned = vi.spyOn(console, 'warn');
  capture();
  expect(warned).not.toHaveBeenCalled();
});

test('reset() alone is tracing off', () => {
  tracepad.init({ host: 'http://tracepad.test:4318', key: 'tp-sk-test', export: false });
  reset();

  tracepad.span('off', (step) => {
    tracepad.score('helpful', 1);
    expect(step.traceId).toBeUndefined();
  });
});

describe('the entry point, built', () => {
  const root = dirname(dirname(fileURLToPath(import.meta.url)));
  // A package of its own under node_modules, so the build leaves `dist/`
  // alone and the OTel packages still resolve from where it sits.
  let built = '';
  // Both entries must reach one copy of the package's state: a `reset` that
  // cleared a second copy would leave the root's `init` standing.
  const probe = `const c = capture(); tp.span('s', () => tp.score('x', 1)); c.restore();
    console.log(JSON.stringify([c.spans.map((s) => s.name), c.scores.length]));`;
  const run = (args: string[]) => execFileSync(process.execPath, args, { cwd: built, encoding: 'utf8' }).trim();

  beforeAll(() => {
    built = mkdtempSync(join(root, 'node_modules', '.tracepad-build-'));
    writeFileSync(join(built, 'package.json'), readFileSync(join(root, 'package.json')));
    execFileSync('npx', ['tsup', '--out-dir', join(built, 'dist')], { cwd: root, stdio: 'pipe' });
  });
  afterAll(() => rmSync(built, { recursive: true, force: true }));

  test('imports as ESM', () => {
    const esm = `import { capture } from 'tracepad/testing'; import * as tp from 'tracepad'; ${probe}`;
    expect(run(['--input-type=module', '-e', esm])).toBe('[["s"],1]');
  });

  test('requires as CommonJS', () => {
    const cjs = `const { capture } = require('tracepad/testing'); const tp = require('tracepad'); ${probe}`;
    expect(run(['-e', cjs])).toBe('[["s"],1]');
  });
});
