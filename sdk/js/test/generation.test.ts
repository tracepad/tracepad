/** The callback-scoped shapes and the OpenAI-compatible reader (spec 032 #5). */

import { SpanStatusCode } from '@opentelemetry/api';
import { describe, expect, expectTypeOf, test, vi } from 'vitest';

import * as attrs from '../src/attributes.js';
import * as tracepad from '../src/index.js';
import { fresh, spans, warnings } from './helpers.js';

fresh();

const ANSWER = {
  model: 'gpt-4o-mini-2026-04-01',
  choices: [{ index: 0, message: { role: 'assistant', content: 'pong' }, finish_reason: 'stop' }],
  usage: {
    prompt_tokens: 42,
    completion_tokens: 7,
    prompt_tokens_details: { cached_tokens: 30 },
    completion_tokens_details: { reasoning_tokens: 2 },
    cost: 0.00041,
  },
};

/** An answer by property, the way a client's model classes hold it. */
class Shaped {
  constructor(source: Record<string, unknown>) {
    for (const [name, value] of Object.entries(source)) {
      Object.defineProperty(this, name, {
        value: Array.isArray(value)
          ? value.map((v) => (v && typeof v === 'object' ? new Shaped(v as Record<string, unknown>) : v))
          : value && typeof value === 'object'
            ? new Shaped(value as Record<string, unknown>)
            : value,
        enumerable: false,
      });
    }
  }
}

describe('span, event and generation', () => {
  test('the callback runs inside the span and the span ends on return', () => {
    const seen = spans();
    const result = tracepad.span('retrieve', { input: { query: 'q' }, metadata: { hits: 2 } }, (step) => {
      expect(step.traceId).toMatch(/^[0-9a-f]{32}$/);
      expect(step.spanId).toMatch(/^[0-9a-f]{16}$/);
      expect(seen.spans).toEqual([]);
      step.update({ output: ['doc'] });
      return 'ok';
    });
    expect(result).toBe('ok');
    const attributes = seen.attributes('retrieve');
    expect(attributes[attrs.OBSERVATION_TYPE]).toBe('span');
    expect(attributes[attrs.INPUT]).toBe('{"query":"q"}');
    expect(attributes[`${attrs.OBSERVATION_METADATA}.hits`]).toBe(2);
    expect(attributes[attrs.OUTPUT]).toBe('["doc"]');
  });

  test('the options may be omitted', () => {
    const seen = spans();
    expect(tracepad.span('bare', () => 1)).toBe(1);
    expect(seen.attributes('bare')).toEqual({ [attrs.OBSERVATION_TYPE]: 'span' });
  });

  test('a promise ends the span on settlement, and a rejection records the error', async () => {
    const seen = spans();
    const pending = tracepad.span('later', async () => {
      await new Promise((tick) => setTimeout(tick, 5));
      return 'done';
    });
    expect(seen.spans).toEqual([]);
    expect(await pending).toBe('done');
    expect(seen.one('later').ended).toBe(true);

    await expect(tracepad.span('fails', async () => {
      throw new Error('no');
    })).rejects.toThrow('no');
    expect(seen.one('fails').status).toEqual({ code: SpanStatusCode.ERROR, message: 'no' });
  });

  test('a throw ends the span ERROR and propagates', () => {
    const seen = spans();
    expect(() => tracepad.span('fails', () => {
      throw new TypeError('bad');
    })).toThrow(TypeError);
    const span = seen.one('fails');
    expect(span.status.code).toBe(SpanStatusCode.ERROR);
    expect(span.events[0]?.attributes?.['exception.type']).toBe('TypeError');
  });

  test('a span takes its kind when it opens, and an unknown one warns once per spelling', () => {
    // Spec 038 #1: one call, not a span retyped by update afterwards.
    const seen = spans();
    tracepad.span('docs-search', { type: 'retriever' }, () => undefined);
    expect(seen.attributes('docs-search')[attrs.OBSERVATION_TYPE]).toBe('retriever');
    expect(warnings).toEqual([]);

    // Written all the same: the mapper keeps it in the observation's metadata.
    // Once per spelling, whichever door: a step in a loop says it once.
    for (let i = 0; i < 3; i++) {
      tracepad.span('lookup', { type: 'retreiver' as never }, () => tracepad.update({ type: 'retreiver' as never }));
    }
    // Not at wrap time, usually module load, before `init({ logger })`: on the call.
    const lookup = tracepad.observe(function lookupAgain() {}, { type: 'retriver' as never });
    expect(warnings).toHaveLength(1);
    lookup();
    lookup();
    expect(seen.spans.map((s) => s.attributes[attrs.OBSERVATION_TYPE])).toEqual([
      'retriever', 'retreiver', 'retreiver', 'retreiver', 'retriver', 'retriver',
    ]);
    expect(warnings).toEqual([
      expect.stringContaining('"retreiver" is not one of the observation types'),
      expect.stringContaining('"retriver" is not one of the observation types'),
    ]);
  });

  test('a kind is remembered only once a logger could have been named', () => {
    // Before `init`, the warning goes to the default sink and is said again
    // after it, where the application's logger hears it (spec 038 #5).
    const early = tracepad.observe(function early() {}, { type: 'retreiver' as never });
    early();
    early();
    expect(warnings).toHaveLength(2);
    spans();
    early();
    early();
    expect(warnings).toHaveLength(3);
  });

  test('an empty kind is the default, at every door', () => {
    // As Go's `WithType("")`: forwarding an optional kind opens a plain step,
    // and `update` leaves the kind where it was.
    const seen = spans();
    tracepad.span('blank', { type: '' as never }, () => tracepad.update({ type: '' as never }));
    tracepad.observe(function decorated() {}, { type: '' as never })();
    expect(seen.attributes('blank')[attrs.OBSERVATION_TYPE]).toBe('span');
    expect(seen.attributes('decorated')[attrs.OBSERVATION_TYPE]).toBe('span');
    expect(warnings).toEqual([]);
  });

  test('span as a generation hands out a generation, with every option it was given', () => {
    // Spec 038 #8: the one kind with a handle of its own, as `observe` does.
    // Not in the type — `generation()` is the way in — but a caller without
    // types gets the handle and loses none of the generation's options.
    const seen = spans();
    const options = { type: 'generation', model: 'gpt-4o-mini', prompt: { name: 'answer', version: 3 }, metadata: { attempt: 1 } };
    tracepad.span('chat', options as never, (call) => {
      (call as tracepad.Generation).end({ model: 'gpt-4o-mini-2026-04-01' });
    });
    expect(seen.attributes('chat')).toMatchObject({
      [attrs.OBSERVATION_TYPE]: 'generation',
      [attrs.REQUEST_MODEL]: 'gpt-4o-mini',
      [attrs.PROMPT_NAME]: 'answer',
      [attrs.PROMPT_VERSION]: 3,
      [`${attrs.OBSERVATION_METADATA}.attempt`]: 1,
      [attrs.RESPONSE_MODEL]: 'gpt-4o-mini-2026-04-01',
    });
    expect(warnings).toEqual([]);
  });

  test('only span takes a kind: event and generation name theirs by being called', () => {
    expectTypeOf<tracepad.GenerationOptions>().not.toHaveProperty('type');
    expectTypeOf<tracepad.ObservationOptions>().not.toHaveProperty('type');
    expectTypeOf<tracepad.SpanOptions>().toHaveProperty('type');
    // @ts-expect-error — `generation()` is the way to a generation
    tracepad.span('chat', { type: 'generation' }, () => undefined);
    const seen = spans();
    // @ts-expect-error — a generation's kind is `generation`
    tracepad.generation('chat', { type: 'tool' }, () => undefined);
    // A `SpanOptions` value still compiles for `event` (its base is a
    // supertype), so the kind it carries is said out loud, not dropped — and
    // the shape's own kind is nothing to say.
    const options: tracepad.SpanOptions = { type: 'tool' };
    tracepad.event('cache.miss', options, () => undefined);
    tracepad.event('cache.miss', options, () => undefined); // once, as any kind warning
    tracepad.event('cache.hit', { type: 'event' } as tracepad.SpanOptions, () => undefined);
    tracepad.event('cache.hit', { type: '' } as never, () => undefined);
    expect(seen.attributes('chat')[attrs.OBSERVATION_TYPE]).toBe('generation');
    expect(seen.spans.filter((s) => s.name !== 'chat').map((s) => s.attributes[attrs.OBSERVATION_TYPE])).toEqual([
      'event', 'event', 'event', 'event',
    ]);
    expect(warnings).toEqual([
      'tracepad: generation() takes no type; its kind is "generation" and "tool" is ignored',
      'tracepad: event() takes no type; its kind is "event" and "tool" is ignored',
    ]);
  });
  test('a generation takes metadata when it opens', () => {
    const seen = spans();
    tracepad.generation('chat', { model: 'gpt-4o-mini', metadata: { attempt: 2 } }, () => undefined);
    expect(seen.attributes('chat')[`${attrs.OBSERVATION_METADATA}.attempt`]).toBe(2);
  });

  test('an event takes no time', () => {
    const seen = spans();
    tracepad.event('cache.miss', { metadata: { key: 'k' } }, () => undefined);
    const event = seen.one('cache.miss');
    expect(event.attributes[attrs.OBSERVATION_TYPE]).toBe('event');
    expect(event.startTime).toEqual(event.endTime);
  });

  test('update and updateTrace act on the current span, whoever opened it', () => {
    const seen = spans();
    tracepad.span('handler', () => {
      tracepad.updateTrace({
        name: 'support-chat', userId: 'u-42', sessionId: 's-7', tags: ['support'], metadata: { channel: 'web' },
        version: 'retrieval-v2',
      });
      tracepad.update({ level: 'WARNING', statusMessage: 'retried once' });
    });
    expect(seen.attributes('handler')).toMatchObject({
      [attrs.TRACE_NAME]: 'support-chat',
      [attrs.USER_ID]: 'u-42',
      [attrs.SESSION_ID]: 's-7',
      [attrs.TRACE_TAGS]: '["support"]',
      [attrs.TRACE_METADATA]: '{"channel":"web"}',
      [attrs.TRACE_VERSION]: 'retrieval-v2',
      [attrs.OBSERVATION_LEVEL]: 'WARNING',
      [attrs.OBSERVATION_STATUS_MESSAGE]: 'retried once',
    });
  });

  test('an empty version is no version, as in Go', () => {
    const seen = spans();
    tracepad.span('handler', () => tracepad.updateTrace({ version: '' }));
    expect(seen.attributes('handler')[attrs.TRACE_VERSION]).toBeUndefined();
  });

  test('outside a span both warn and write nothing', () => {
    spans();
    tracepad.update({ level: 'ERROR' });
    tracepad.updateTrace({ userId: 'u' });
    expect(warnings).toEqual([
      'tracepad: update() outside a span: nothing was written',
      'tracepad: updateTrace() outside a span: nothing was written',
    ]);
  });
});

describe('generation', () => {
  test('writes the request side, and end(response) reads the table of spec 017 #5', () => {
    const seen = spans();
    tracepad.generation(
      'chat',
      { model: 'gpt-4o-mini', modelParameters: { temperature: 0.2, stop: ['\n'] }, input: [{ role: 'user', content: 'ping' }], prompt: { name: 'support', version: 3 } },
      (call) => call.end(ANSWER),
    );
    expect(seen.attributes('chat')).toEqual({
      [attrs.OBSERVATION_TYPE]: 'generation',
      [attrs.REQUEST_MODEL]: 'gpt-4o-mini',
      'gen_ai.request.temperature': 0.2,
      'gen_ai.request.stop': '["\\n"]',
      [attrs.INPUT]: '[{"role":"user","content":"ping"}]',
      [attrs.PROMPT_NAME]: 'support',
      [attrs.PROMPT_VERSION]: 3,
      [attrs.RESPONSE_MODEL]: 'gpt-4o-mini-2026-04-01',
      'gen_ai.usage.input_tokens': 42,
      'gen_ai.usage.output_tokens': 7,
      'gen_ai.usage.cache_read_input_tokens': 30,
      'gen_ai.usage.reasoning_tokens': 2,
      [attrs.USAGE_COST]: 0.00041,
      [attrs.OUTPUT]: 'pong',
    });
  });

  test('reads an object that answers by property, and a prompt by name', () => {
    const seen = spans();
    tracepad.generation('chat', { prompt: 'support' }, (call) => call.end(new Shaped(ANSWER)));
    const attributes = seen.attributes('chat');
    expect(attributes[attrs.RESPONSE_MODEL]).toBe(ANSWER.model);
    expect(attributes['gen_ai.usage.cache_read_input_tokens']).toBe(30);
    expect(attributes[attrs.OUTPUT]).toBe('pong');
    expect(attributes[attrs.PROMPT_NAME]).toBe('support');
    expect(attributes).not.toHaveProperty(attrs.PROMPT_VERSION);
  });

  test('explicit fields win over what was read, and usage keys go verbatim', () => {
    const seen = spans();
    tracepad.generation('chat', (call) =>
      call.end(ANSWER, { model: 'claude-sonnet-5', usage: { input_tokens: 1, cache_creation_tokens: 9 }, cost: 0.5, output: { text: 'other' } }),
    );
    const attributes = seen.attributes('chat');
    expect(attributes[attrs.RESPONSE_MODEL]).toBe('claude-sonnet-5');
    expect(attributes['gen_ai.usage.input_tokens']).toBe(1);
    expect(attributes['gen_ai.usage.cache_creation_tokens']).toBe(9);
    expect(attributes).not.toHaveProperty('gen_ai.usage.output_tokens');
    expect(attributes[attrs.USAGE_COST]).toBe(0.5);
    expect(attributes[attrs.OUTPUT]).toBe('{"text":"other"}');
  });

  test('no usage means no usage and no cost — never $0', () => {
    const seen = spans();
    tracepad.generation('chat', (call) => call.end({ model: 'm', choices: [{ message: { content: 'x' } }] }));
    const attributes = seen.attributes('chat');
    expect(Object.keys(attributes).filter((k) => k.startsWith(attrs.USAGE_PREFIX))).toEqual([]);
    expect(attributes[attrs.OUTPUT]).toBe('x');
  });

  test('returning without end ends the span with what it has; end twice ends once', () => {
    const seen = spans();
    tracepad.generation('chat', { model: 'm' }, (call) => {
      call.update({ output: 'said' });
    });
    expect(seen.attributes('chat')[attrs.OUTPUT]).toBe('said');
    const end = vi.fn();
    tracepad.generation('twice', (call) => {
      call.span.end = end;
      call.end();
      call.end();
    });
    expect(end).toHaveBeenCalledTimes(1);
  });

  test('update({output}) inside is not replaced by the response', () => {
    const seen = spans();
    tracepad.generation('chat', (call) => {
      call.update({ output: 'mine' });
      call.end(ANSWER);
    });
    expect(seen.attributes('chat')[attrs.OUTPUT]).toBe('mine');
  });

  test('firstToken stamps the completion start once', () => {
    const seen = spans();
    let stamps = 0;
    tracepad.generation('chat', (call) => {
      const setAttribute = call.span.setAttribute.bind(call.span);
      call.span.setAttribute = (key, value) => {
        if (key === attrs.COMPLETION_START_TIME) stamps++;
        return setAttribute(key, value);
      };
      call.firstToken();
      call.firstToken();
    });
    expect(stamps).toBe(1);
    expect(seen.attributes('chat')[attrs.COMPLETION_START_TIME]).toMatch(/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$/);
  });
});
