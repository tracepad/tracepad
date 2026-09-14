/** The streaming pass-through over a generation (spec 031 #7, #21). */

import { SpanStatusCode } from '@opentelemetry/api';
import { describe, expect, test } from 'vitest';

import * as attrs from '../src/attributes.js';
import * as tracepad from '../src/index.js';
import { fresh, spans } from './helpers.js';

fresh();

const MODEL = 'gpt-4o-mini-2026-04-01';
const USAGE = { prompt_tokens: 42, completion_tokens: 7, prompt_tokens_details: { cached_tokens: 30 }, cost: 0.00041 };

/** One chunk the way OpenAI cuts them: `usage` is `null` until the last. */
function chunk(content?: string, options: { role?: string; usage?: unknown; choices?: boolean } = {}) {
  const delta: Record<string, unknown> = {};
  if (options.role !== undefined) delta.role = options.role;
  if (content !== undefined) delta.content = content;
  return {
    model: MODEL,
    choices: options.choices === false ? [] : [{ index: 0, delta, finish_reason: null }],
    usage: options.usage ?? null,
  };
}

// The first chunk names the role with an empty content, the deltas follow,
// and a last chunk with no choices carries the usage — what
// `stream_options: {include_usage: true}` makes OpenAI send.
const CHUNKS = [chunk('', { role: 'assistant' }), chunk('po'), chunk('ng'), chunk(undefined, { usage: USAGE, choices: false })];

async function* later<T>(items: T[]): AsyncGenerator<T> {
  for (const item of items) {
    await Promise.resolve();
    yield item;
  }
}

async function collect<T>(iterable: AsyncIterable<T>): Promise<T[]> {
  const out: T[] = [];
  for await (const item of iterable) out.push(item);
  return out;
}

describe('a stream through the generation', () => {
  test('yields every chunk unchanged and ends with what it carried', async () => {
    const seen = spans();
    const out = await tracepad.generation('chat', { model: 'gpt-4o-mini' }, (call) => collect(call.stream(later(CHUNKS))));
    expect(out).toEqual(CHUNKS);
    const attributes = seen.attributes('chat');
    expect(attributes[attrs.RESPONSE_MODEL]).toBe(MODEL);
    expect(attributes['gen_ai.usage.input_tokens']).toBe(42);
    expect(attributes['gen_ai.usage.output_tokens']).toBe(7);
    expect(attributes['gen_ai.usage.cache_read_input_tokens']).toBe(30);
    expect(attributes[attrs.USAGE_COST]).toBe(0.00041);
    expect(attributes[attrs.OUTPUT]).toBe('pong');
    expect(attributes[attrs.COMPLETION_START_TIME]).toEqual(expect.any(String));
  });

  test('a sync iterable reads the same', async () => {
    const seen = spans();
    await tracepad.generation('chat', (call) => collect(call.stream(CHUNKS)));
    expect(seen.attributes('chat')[attrs.OUTPUT]).toBe('pong');
  });

  test('the first token is the first chunk with content, stamped once', async () => {
    const seen = spans();
    let stamped: string | undefined;
    await tracepad.generation('chat', async (call) => {
      let n = 0;
      for await (const _ of call.stream(later(CHUNKS))) {
        n++;
        // The role-only chunk is not a token; the first delta is.
        const at = (call.span as unknown as { attributes: Record<string, unknown> }).attributes[attrs.COMPLETION_START_TIME];
        if (n === 1) expect(at).toBeUndefined();
        if (n === 2) stamped = at as string;
        if (n > 2) expect(at).toBe(stamped);
      }
    });
    expect(seen.attributes('chat')[attrs.COMPLETION_START_TIME]).toBe(stamped);
  });

  test('a stream without usage records the model and the output and nothing else', async () => {
    const seen = spans();
    await tracepad.generation('chat', (call) => collect(call.stream(CHUNKS.slice(0, 3))));
    const attributes = seen.attributes('chat');
    expect(attributes[attrs.OUTPUT]).toBe('pong');
    expect(attributes[attrs.RESPONSE_MODEL]).toBe(MODEL);
    expect(Object.keys(attributes).filter((k) => k.startsWith(attrs.USAGE_PREFIX))).toEqual([]);
  });

  test('an explicit end mid-stream wins, and nothing ends twice', async () => {
    const seen = spans();
    await tracepad.generation('chat', async (call) => {
      for await (const piece of call.stream(later(CHUNKS))) {
        if (piece.choices[0]?.delta.content === 'po') call.end(undefined, { output: 'cut', cost: 1 });
      }
    });
    const attributes = seen.attributes('chat');
    expect(attributes[attrs.OUTPUT]).toBe('cut');
    expect(attributes[attrs.USAGE_COST]).toBe(1);
    expect(attributes).not.toHaveProperty('gen_ai.usage.input_tokens');
    expect(seen.all()).toHaveLength(1);
  });

  test('a break leaves the ending to the callback, with what was read', async () => {
    const seen = spans();
    let ended: boolean | undefined;
    await tracepad.generation('chat', async (call) => {
      for await (const piece of call.stream(later(CHUNKS))) {
        if (piece.choices[0]?.delta.content === 'po') break;
      }
      ended = seen.all().length > 0;
    });
    expect(ended).toBe(false);
    const attributes = seen.attributes('chat');
    expect(attributes[attrs.OUTPUT]).toBe('po');
    expect(attributes).not.toHaveProperty('gen_ai.usage.input_tokens');
  });

  test('a throw inside the loop is recorded on the generation, with the output so far', async () => {
    const seen = spans();
    await expect(tracepad.generation('chat', async (call) => {
      for await (const _ of call.stream(later(CHUNKS))) throw new Error('consumer failed');
    })).rejects.toThrow('consumer failed');
    const span = seen.one('chat');
    expect(span.status.code).toBe(SpanStatusCode.ERROR);
    expect(span.events.map((e) => e.name)).toEqual(['exception']);
    expect(span.attributes[attrs.OUTPUT]).toBe('');
  });

  test('a provider that raises mid-stream is the same', async () => {
    const seen = spans();
    async function* broken() {
      yield chunk('par');
      throw new Error('connection reset');
    }
    await expect(tracepad.generation('chat', (call) => collect(call.stream(broken())))).rejects.toThrow('connection reset');
    const span = seen.one('chat');
    expect(span.status).toEqual({ code: SpanStatusCode.ERROR, message: 'connection reset' });
    expect(span.attributes[attrs.OUTPUT]).toBe('par');
  });
});
