import { describe, expect, test } from 'vitest';

import { dumps, rfc3339, scalar } from '../src/attributes.js';

describe('dumps', () => {
  test('a string is sent as it is, not as a JSON string of one', () => {
    expect(dumps('Answer {topic}.')).toBe('Answer {topic}.');
  });

  test('everything else is compact JSON', () => {
    expect(dumps({ a: [1, 'two', null, true] })).toBe('{"a":[1,"two",null,true]}');
  });

  test('a bigint is a number, an Error its name and message', () => {
    expect(dumps({ n: 12n, e: new RangeError('out') })).toBe(
      '{"n":12,"e":{"name":"RangeError","message":"out"}}',
    );
  });

  test('a function, a symbol and undefined are their String()', () => {
    expect(dumps(undefined)).toBe('undefined');
    expect(dumps({ f: () => 1, s: Symbol('x') })).toBe('{"f":"() => 1","s":"Symbol(x)"}');
  });

  test('a cycle never throws', () => {
    const loop: Record<string, unknown> = {};
    loop.self = loop;
    expect(dumps(loop)).toBe('[object Object]');
  });
});

test('scalar keeps the types OTLP has and renders the rest', () => {
  expect(scalar(0.2)).toBe(0.2);
  expect(scalar('json')).toBe('json');
  expect(scalar(false)).toBe(false);
  expect(scalar({ stop: ['\n'] })).toBe('{"stop":["\\n"]}');
});

test('rfc3339 is the instant with milliseconds and a Z', () => {
  expect(rfc3339(1787738400_005)).toBe('2026-08-26T10:00:00.005Z');
});
