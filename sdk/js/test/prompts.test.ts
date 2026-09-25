/** Prompts: fetched by label, cached for as long as the server says (spec 032 #7). */

import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, test, vi } from 'vitest';

import * as tracepad from '../src/index.js';
import { HOST, KEY, fakeFetch, fresh, warnings, type Call } from './helpers.js';

fresh();

const STORED = {
  name: 'support-answer',
  version: 3,
  type: 'text',
  prompt: 'Answer {topic} for {product}.',
  labels: ['production'],
  config: { model: 'gpt-4o-mini' },
};

const CHAT = {
  ...STORED,
  type: 'chat',
  prompt: [
    { role: 'system', content: 'You help with {product}.' },
    { role: 'user', content: '{question}' },
  ],
};

function serving(answer: (call: Call) => ReturnType<Parameters<typeof fakeFetch>[0]>) {
  tracepad.init({ host: HOST, key: KEY, export: false });
  return fakeFetch(answer);
}

test('fetches by label with the bearer, and returns the Prompt', async () => {
  const calls = serving(() => ({ body: STORED, headers: { 'Cache-Control': 'max-age=60' } }));
  const prompt = await tracepad.prompt('support-answer', { label: 'production' });
  expect(calls).toEqual([expect.objectContaining({
    method: 'GET',
    url: `${HOST}/api/v1/prompts/support-answer?label=production`,
    headers: expect.objectContaining({ Authorization: `Bearer ${KEY}` }),
  })]);
  expect(prompt).toBeInstanceOf(tracepad.Prompt);
  expect(prompt.name).toBe('support-answer');
  expect(prompt.version).toBe(3);
  expect(prompt.text).toBe(STORED.prompt);
  expect(prompt.messages).toBeUndefined();
  expect(prompt.labels).toEqual(['production']);
  expect(prompt.config).toEqual({ model: 'gpt-4o-mini' });
});

test('by version, and the latest with neither', async () => {
  const calls = serving(() => ({ body: STORED }));
  await tracepad.prompt('support-answer', { version: 2 });
  await tracepad.prompt('support-answer');
  expect(calls.map((c) => c.url)).toEqual([
    `${HOST}/api/v1/prompts/support-answer?version=2`,
    `${HOST}/api/v1/prompts/support-answer`,
  ]);
});

describe('the cache', () => {
  test('serves the answer for max-age and refreshes after', async () => {
    vi.useFakeTimers();
    const calls = serving(() => ({ body: STORED, headers: { 'cache-control': 'public, max-age=60' } }));
    await tracepad.prompt('support-answer', { label: 'production' });
    await vi.advanceTimersByTimeAsync(59_000);
    await tracepad.prompt('support-answer', { label: 'production' });
    expect(calls).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1_001);
    await tracepad.prompt('support-answer', { label: 'production' });
    expect(calls).toHaveLength(2);
    // Another label is another entry.
    await tracepad.prompt('support-answer', { label: 'staging' });
    expect(calls).toHaveLength(3);
  });

  test('a header that says nothing caches nothing', async () => {
    const calls = serving(() => ({ body: STORED }));
    await tracepad.prompt('support-answer');
    await tracepad.prompt('support-answer');
    expect(calls).toHaveLength(2);
  });

  test('serves stale on a transport error or a 5xx, with a warning', async () => {
    vi.useFakeTimers();
    let answer: ReturnType<Parameters<typeof fakeFetch>[0]> = { body: STORED, headers: { 'cache-control': 'max-age=1' } };
    serving(() => answer);
    const first = await tracepad.prompt('support-answer');
    await vi.advanceTimersByTimeAsync(2_000);

    answer = { status: 503, body: 'restarting' };
    expect(await tracepad.prompt('support-answer')).toBe(first);
    answer = new TypeError('fetch failed');
    expect(await tracepad.prompt('support-answer')).toBe(first);
    expect(warnings).toEqual([
      'tracepad: serving prompt "support-answer" from a stale cache: tracepad: HTTP 503: "restarting"',
      expect.stringContaining('from a stale cache: tracepad: GET http://tracepad.test:4318/api/v1/prompts/support-answer: TypeError: fetch failed'),
    ]);
  });

  test('a 4xx is about the request and is thrown even with a cache', async () => {
    vi.useFakeTimers();
    let answer: ReturnType<Parameters<typeof fakeFetch>[0]> = { body: STORED, headers: { 'cache-control': 'max-age=1' } };
    serving(() => answer);
    await tracepad.prompt('support-answer');
    await vi.advanceTimersByTimeAsync(2_000);
    answer = { status: 404, body: { error: 'no such label' } };
    await expect(tracepad.prompt('support-answer')).rejects.toThrow(tracepad.TracepadHTTPError);
    await expect(tracepad.prompt('support-answer')).rejects.toMatchObject({ status: 404, body: '{"error":"no such label"}' });
  });

  test('rejects with nothing cached', async () => {
    serving(() => new TypeError('fetch failed'));
    await expect(tracepad.prompt('support-answer')).rejects.toThrow(tracepad.TracepadError);
    expect(warnings).toEqual([]);
  });

  test('with no init, the environment is read on the first call', async () => {
    process.env.TRACEPAD_HOST = HOST;
    process.env.TRACEPAD_API_KEY = KEY;
    const calls = fakeFetch(() => ({ body: STORED }));
    await tracepad.prompt('support-answer');
    expect(calls[0]?.headers.Authorization).toBe(`Bearer ${KEY}`);
    delete process.env.TRACEPAD_HOST;
  });

  test('with no configuration at all it rejects with TracepadConfigError', async () => {
    await expect(tracepad.prompt('support-answer')).rejects.toThrow(tracepad.TracepadConfigError);
  });
});

describe('compile', () => {
  test('substitutes in the text', async () => {
    serving(() => ({ body: STORED }));
    const prompt = await tracepad.prompt('support-answer');
    expect(prompt.compile({ topic: 'refunds', product: 'Tracepad' })).toBe('Answer refunds for Tracepad.');
  });

  test('and in every message, keeping the other fields', async () => {
    serving(() => ({ body: CHAT }));
    const prompt = await tracepad.prompt('support-answer');
    expect(prompt.text).toBeUndefined();
    expect(prompt.compile({ product: 'Tracepad', question: 'why?' })).toEqual([
      { role: 'system', content: 'You help with Tracepad.' },
      { role: 'user', content: 'why?' },
    ]);
  });

  test('doubled braces are the braces themselves', async () => {
    serving(() => ({ body: { ...STORED, prompt: 'Reply as {{"answer": "{answer}"}} for {product}.' } }));
    const prompt = await tracepad.prompt('support-answer');
    expect(prompt.compile({ answer: 'yes', product: 'Tracepad' })).toBe('Reply as {"answer": "yes"} for Tracepad.');
  });

  test('a placeholder with no variable throws', async () => {
    serving(() => ({ body: STORED }));
    const prompt = await tracepad.prompt('support-answer');
    expect(() => prompt.compile({ topic: 'refunds' })).toThrow('placeholder {product} has no variable');
  });

  // The Python and Go packages run the same table (sdk/python/tests/test_prompts.py,
  // sdk/go/prompts_test.go): one stored text, compiled with string variables,
  // must be one prompt, whichever package reads it.
  const table = join(dirname(fileURLToPath(import.meta.url)), '..', '..', '..', 'testdata', 'prompts', 'compile.json');
  const { cases } = JSON.parse(readFileSync(table, 'utf8')) as {
    cases: {
      name: string;
      text?: string;
      messages?: tracepad.Message[];
      variables: Record<string, string>;
      compiled?: unknown;
      error?: string;
    }[];
  };

  test.each(cases)('reads what the Python package reads: $name', (c) => {
    const prompt = new tracepad.Prompt(c.messages !== undefined
      ? { name: 'p', version: 1, type: 'chat', messages: c.messages }
      : { name: 'p', version: 1, type: 'text', text: c.text ?? '' });
    if (c.error !== undefined) {
      expect(() => prompt.compile(c.variables)).toThrow(c.error);
    } else {
      expect(prompt.compile(c.variables)).toEqual(c.compiled);
    }
  });
});
