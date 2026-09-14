/** `observe` wraps a function into a step of the trace (spec 032 #4). */

import { SpanStatusCode } from '@opentelemetry/api';
import { describe, expect, test } from 'vitest';

import * as attrs from '../src/attributes.js';
import * as tracepad from '../src/index.js';
import { fresh, spans, warnings, type Spans } from './helpers.js';

fresh();

describe('the four shapes', () => {
  test('a sync function: input as the positional array, output the return value', () => {
    const seen = spans();
    // The wrapper under another name: a bundler that finds the inner
    // `add` shadowed by an outer one renames it, and the span with it.
    const sum = tracepad.observe(function add(a: number, b: number) {
      return a + b;
    });
    expect(sum(2, 3)).toBe(5);
    const attributes = seen.attributes('add');
    expect(attributes[attrs.OBSERVATION_TYPE]).toBe('span');
    expect(attributes[attrs.INPUT]).toBe('[2,3]');
    expect(attributes[attrs.OUTPUT]).toBe('5');
  });

  test('an async function ends the span on settlement', async () => {
    const seen = spans();
    const fetchAnswer = tracepad.observe(async (question: string) => {
      await new Promise((tick) => setTimeout(tick, 5));
      return `${question}!`;
    }, { name: 'fetch-answer' });
    const pending = fetchAnswer('why');
    expect(seen.all()).toEqual([]);
    expect(await pending).toBe('why!');
    expect(seen.attributes('fetch-answer')[attrs.OUTPUT]).toBe('why!'); // a string as it is
  });

  test('a generator records the list of what it yielded', () => {
    const seen = spans();
    const counted = tracepad.observe(function* count(n: number) {
      for (let i = 0; i < n; i++) yield i;
      return 'done';
    });
    const out = counted(3);
    expect(seen.all()).toEqual([]); // nothing ends before the generator does
    expect([...out]).toEqual([0, 1, 2]);
    expect(seen.attributes('count')[attrs.OUTPUT]).toBe('[0,1,2]');
  });

  test('an async generator too, and each step runs inside the span', async () => {
    const seen = spans();
    const streamed = tracepad.observe(async function* stream() {
      yield 'a';
      tracepad.update({ metadata: { step: 1 } });
      yield 'b';
    });
    const out: string[] = [];
    for await (const piece of streamed()) out.push(piece);
    expect(out).toEqual(['a', 'b']);
    const attributes = seen.attributes('stream');
    expect(attributes[attrs.OUTPUT]).toBe('["a","b"]');
    expect(attributes[attrs.OBSERVATION_METADATA]).toBe('{"step":1}');
  });

  test('a generator left early ends with what came before', () => {
    const seen = spans();
    const counted = tracepad.observe(function* count() {
      yield 1;
      yield 2;
      yield 3;
    });
    for (const n of counted()) if (n === 2) break;
    expect(seen.attributes('count')[attrs.OUTPUT]).toBe('[1,2]');
  });
});

describe('what the span carries', () => {
  test('the name falls back to the function name, then "anonymous"', () => {
    const seen = spans();
    tracepad.observe(() => 1)();
    tracepad.observe(function named() {})();
    tracepad.observe(() => 2, { name: 'given' })();
    expect(seen.all().map((s) => s.name)).toEqual(['anonymous', 'named', 'given']);
  });

  test('the wrapper keeps the name and forwards this', () => {
    spans();
    class Bot {
      prefix = '> ';
      say = tracepad.observe(function (this: Bot, text: string) {
        return this.prefix + text;
      });
    }
    expect(new Bot().say('hi')).toBe('> hi');
    function original() {}
    expect(tracepad.observe(original).name).toBe('original');
  });

  test('the type is written, and an unknown one warns', () => {
    const seen = spans();
    tracepad.observe(() => 1, { type: 'retriever' })();
    expect(seen.attributes()[attrs.OBSERVATION_TYPE]).toBe('retriever');
    tracepad.update({ type: 'widget' as never });
    expect(warnings).toEqual([expect.stringContaining('outside a span')]);
  });

  test('the opt-outs leave the attributes unset', () => {
    const seen = spans();
    tracepad.observe((secret: string) => secret.length, { captureInput: false, captureOutput: false })('hunter2');
    const attributes = seen.attributes();
    expect(attributes).not.toHaveProperty(attrs.INPUT);
    expect(attributes).not.toHaveProperty(attrs.OUTPUT);
  });

  test('update inside replaces what would be captured', () => {
    const seen = spans();
    tracepad.observe(function step(secret: string) {
      tracepad.update({ input: { redacted: true }, output: 'said', name: 'renamed' });
      return secret;
    })('hunter2');
    const attributes = seen.attributes('renamed');
    expect(attributes[attrs.INPUT]).toBe('{"redacted":true}');
    expect(attributes[attrs.OUTPUT]).toBe('said');
  });

  test('type generation reads the return value as a response', () => {
    const seen = spans();
    const call = tracepad.observe(
      () => ({ model: 'gpt-4o-mini-2026', choices: [{ message: { content: 'pong' } }], usage: { prompt_tokens: 3, completion_tokens: 1 } }),
      { type: 'generation', name: 'chat' },
    );
    call();
    const attributes = seen.attributes('chat');
    expect(attributes[attrs.RESPONSE_MODEL]).toBe('gpt-4o-mini-2026');
    expect(attributes['gen_ai.usage.input_tokens']).toBe(3);
    expect(attributes[attrs.OUTPUT]).toBe('pong');
  });

  test('an argument that is not JSON is a string, never a failure', () => {
    const seen = spans();
    const loop: Record<string, unknown> = {};
    loop.self = loop;
    expect(tracepad.observe((x: unknown) => x === loop)(loop)).toBe(true);
    expect(seen.attributes()[attrs.INPUT]).toBe('[object Object]');
  });
});

describe('the error path', () => {
  function failed(seen: Spans, name: string) {
    const span = seen.one(name);
    expect(span.status).toEqual({ code: SpanStatusCode.ERROR, message: 'upstream timeout' });
    expect(span.events.map((e) => [e.name, e.attributes?.['exception.message']])).toEqual([['exception', 'upstream timeout']]);
  }

  test('a throw ends the span ERROR with the event, and propagates unchanged', () => {
    const seen = spans();
    const boom = new Error('upstream timeout');
    const failing = tracepad.observe(function fails() {
      throw boom;
    });
    expect(() => failing()).toThrow(boom);
    failed(seen, 'fails');
  });

  test('a rejection the same', async () => {
    const seen = spans();
    const failing = tracepad.observe(async function fails() {
      throw new Error('upstream timeout');
    });
    await expect(failing()).rejects.toThrow('upstream timeout');
    failed(seen, 'fails');
  });

  test('a generator that throws mid-way keeps what it yielded', () => {
    const seen = spans();
    const failing = tracepad.observe(function* fails() {
      yield 1;
      throw new Error('upstream timeout');
    });
    expect(() => [...failing()]).toThrow('upstream timeout');
    failed(seen, 'fails');
    expect(seen.attributes('fails')[attrs.OUTPUT]).toBe('[1]');
  });
});

test('nested steps are parented, and the trace context reaches an await', async () => {
  const seen = spans();
  const innermost = tracepad.observe(async function inner() {
    await Promise.resolve();
    return tracepad.span('deepest', (o) => o.traceId);
  });
  const outermost = tracepad.observe(async function outer() {
    return innermost();
  });
  const traceId = await outermost();
  const [deepest, innerSpan, outerSpan] = seen.all();
  expect([deepest!.name, innerSpan!.name, outerSpan!.name]).toEqual(['deepest', 'inner', 'outer']);
  expect(deepest!.parentSpanContext?.spanId).toBe(innerSpan!.spanContext().spanId);
  expect(innerSpan!.parentSpanContext?.spanId).toBe(outerSpan!.spanContext().spanId);
  expect(outerSpan!.spanContext().traceId).toBe(traceId);
});
