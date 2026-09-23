/** `tracepad/testing`: the capture, the reset, the entry point (spec 040). */

import { execFileSync } from 'node:child_process';
import { dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

import { beforeAll, describe, expect, test, vi } from 'vitest';

import * as tracepad from '../src/index.js';
import { capture, reset } from '../src/testing.js';
import { fresh, registered } from './helpers.js';

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

  test('is disposed by `using`', () => {
    {
      using captured = capture();
      tracepad.span('inside', () => {});
      expect(captured.spans).toHaveLength(1);
    }
    tracepad.span('outside', (step) => expect(step.traceId).toBeUndefined());
  });
});

test('the reset undoes the registration the API allows once (spec 040 #3)', () => {
  capture();
  expect(registered().constructor.name).toBe('NodeTracerProvider');
  reset();
  expect(registered().constructor.name).toBe('NoopTracerProvider');
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
  // Both entries must reach one copy of the package's state: a `reset` that
  // cleared a second copy would leave the root's `init` standing.
  const probe = `const c = capture(); tp.span('s', () => tp.score('x', 1)); c.restore();
    console.log(JSON.stringify([c.spans.map((s) => s.name), c.scores.length]));`;
  const run = (args: string[]) => execFileSync(process.execPath, args, { cwd: root, encoding: 'utf8' }).trim();

  beforeAll(() => {
    execFileSync('npx', ['tsup', '--silent'], { cwd: root, stdio: 'ignore' });
  });

  test('imports as ESM', () => {
    const esm = `import { capture } from 'tracepad/testing'; import * as tp from 'tracepad'; ${probe}`;
    expect(run(['--input-type=module', '-e', esm])).toBe('[["s"],1]');
  });

  test('requires as CommonJS', () => {
    const cjs = `const { capture } = require('tracepad/testing'); const tp = require('tracepad'); ${probe}`;
    expect(run(['-e', cjs])).toBe('[["s"],1]');
  });
});
