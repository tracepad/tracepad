/**
 * The package against a real binary (spec 032, Testing).
 *
 * Everything below the package — the OTLP encoding, the transport, the auth,
 * the mapper's table, the columns — only breaks at a seam the unit layer
 * cannot see. So this boots the store on a temporary database, exports a
 * trace through the package's own exporter, posts a score and fetches a
 * prompt, and then reads all three back through the API a person would read
 * them with.
 */

import { trace } from '@opentelemetry/api';
import { NodeTracerProvider } from '@opentelemetry/sdk-trace-node';
import { afterAll, beforeAll, describe, expect, test } from 'vitest';

import * as tracepad from '../../src/index.js';
import { fresh } from '../helpers.js';
import { BINARY, KEY, serve, traceOf, walk, type Store } from './harness.js';

fresh();

const ANSWER = {
  model: 'claude-sonnet-5-2026-08-01',
  choices: [{ message: { role: 'assistant', content: 'Open Settings and choose Reset.' } }],
  usage: { prompt_tokens: 128, completion_tokens: 41, cost: 0.0011 },
};

describe.skipIf(!BINARY)('against a real binary', () => {
  let store: Store;
  beforeAll(async () => {
    store = await serve();
  });
  afterAll(async () => {
    await store?.stop();
  });

  test('a traced call arrives whole', async () => {
    await store.call('POST', '/api/v1/prompts/support-answer/versions', {
      type: 'text',
      prompt: 'Answer {topic}.',
      labels: ['production'],
    });
    tracepad.init({ host: store.host, key: KEY, environment: 'e2e', release: '2026.9.4' });

    const support = await tracepad.prompt('support-answer', { label: 'production' });
    expect(support.version).toBe(1);
    expect(support.compile({ topic: 'password resets' })).toBe('Answer password resets.');

    let traceId = '';
    const answer = tracepad.observe(async (question: string) => {
      tracepad.updateTrace({ name: 'support-chat', userId: 'user-4821', sessionId: 'session-77', tags: ['support', 'beta'] });
      await tracepad.generation(
        'chat-completion',
        { model: 'claude-sonnet-5', prompt: support, modelParameters: { temperature: 0.2 }, input: [{ role: 'user', content: question }] },
        async (call) => {
          call.firstToken();
          call.end(ANSWER);
          traceId = call.traceId;
        },
      );
      tracepad.score('helpful', 0.9, { comment: 'cited the source' });
      return 'done';
    }, { name: 'answer-question' });

    await answer('how do I reset my password?');
    await tracepad.flush({ timeout: 20_000 });

    const stored = await traceOf(store, traceId);
    expect(stored.name).toBe('support-chat');
    expect(stored.user_id).toBe('user-4821');
    expect(stored.session_id).toBe('session-77');
    expect([...stored.tags!].sort()).toEqual(['beta', 'support']);
    expect(stored.environment).toBe('e2e');
    expect(stored.release).toBe('2026.9.4');

    const observations = walk(stored.observations);
    expect(observations.map((o) => o.name)).toEqual(['answer-question', 'chat-completion']);
    const [root, generation] = observations;
    expect(root!.input).toEqual(['how do I reset my password?']);

    expect(generation!.type).toBe('generation');
    expect(generation!.model).toBe('claude-sonnet-5');
    expect(generation!.usage).toEqual({ input_tokens: 128, output_tokens: 41 });
    expect(generation!.model_parameters).toEqual({ temperature: 0.2 });
    expect(generation!.prompt).toEqual({ name: 'support-answer', version: 1 });
    expect(generation!.ttft_ms).not.toBeNull();
    expect(generation!.output).toBe('Open Settings and choose Reset.');

    // The cost is the one the provider charged, never a computed one.
    expect(Math.abs(stored.total_cost! - 0.0011)).toBeLessThan(1e-12);

    const { scores } = (await store.call('GET', `/api/v1/scores?trace_id=${traceId}`)) as {
      scores: { name: string; value: number; comment: string }[];
    };
    expect(scores.map((s) => [s.name, s.value, s.comment])).toEqual([['helpful', 0.9, 'cited the source']]);
  });

  test('a streamed call lands with its usage, output and time to first token', async () => {
    tracepad.init({ host: store.host, key: KEY });
    const chunks = [
      { model: ANSWER.model, choices: [{ delta: { role: 'assistant', content: '' } }] },
      { model: ANSWER.model, choices: [{ delta: { content: 'Open Settings ' } }] },
      { model: ANSWER.model, choices: [{ delta: { content: 'and choose Reset.' } }] },
      { model: ANSWER.model, choices: [], usage: ANSWER.usage },
    ];
    async function* streamed() {
      for (const chunk of chunks) {
        await new Promise((tick) => setTimeout(tick, 2));
        yield chunk;
      }
    }

    let traceId = '';
    await tracepad.generation('chat-completion', { model: 'claude-sonnet-5' }, async (call) => {
      traceId = call.traceId;
      const seen = [];
      for await (const chunk of call.stream(streamed())) seen.push(chunk);
      expect(seen).toEqual(chunks);
    });
    await tracepad.flush({ timeout: 20_000 });

    const stored = await traceOf(store, traceId);
    const [generation] = walk(stored.observations);
    expect(generation!.type).toBe('generation');
    expect(generation!.usage).toEqual({ input_tokens: 128, output_tokens: 41 });
    expect(generation!.output).toBe('Open Settings and choose Reset.');
    expect(generation!.ttft_ms).not.toBeNull();
    expect(Math.abs(stored.total_cost! - 0.0011)).toBeLessThan(1e-12);
  });

  test("the application's own spans share the trace", async () => {
    // A provider the application built, with the package's processor in its
    // constructor: one pipeline, one trace.
    new NodeTracerProvider({ spanProcessors: [tracepad.spanProcessor({ host: store.host, key: KEY })] }).register();
    tracepad.init({ host: store.host, key: KEY });
    const framework = trace.getTracer('the.framework');

    const traceId = framework.startActiveSpan('GET /answer', (request) => {
      tracepad.span('answer-question', () => undefined);
      request.end();
      return request.spanContext().traceId;
    });
    await tracepad.flush({ timeout: 20_000 });

    const stored = await traceOf(store, traceId);
    expect(walk(stored.observations).map((o) => o.name)).toEqual(['GET /answer', 'answer-question']);
    // The trace's name is the root span's: nothing claimed it (docs/ingest.md).
    expect(stored.name).toBe('GET /answer');
  });

  test('a failing step is stored as an error', async () => {
    tracepad.init({ host: store.host, key: KEY });
    const failing = tracepad.observe(function fails() {
      throw new Error('upstream timeout');
    });
    const traceId = tracepad.span('attempt', (attempt) => {
      expect(() => failing()).toThrow('upstream timeout');
      return attempt.traceId;
    });
    await tracepad.flush({ timeout: 20_000 });

    const stored = await traceOf(store, traceId);
    const failed = walk(stored.observations).find((o) => o.name === 'fails')!;
    expect(failed.level).toBe('ERROR');
    expect(failed.status_message).toContain('upstream timeout');
    expect(stored.error_count).toBe(1);
  });
});
