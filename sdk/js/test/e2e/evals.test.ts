/**
 * The harness against a real binary (spec 032, Testing).
 *
 * The loop `docs/datasets.md` prints, run end to end: declare the configs,
 * push the cases, open the run, fetch the items at the version it pinned,
 * run each case inside its block, score it, close the run — then read back
 * through the API what the store made of it.
 */

import { trace } from '@opentelemetry/api';
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-proto';
import { NodeTracerProvider, SimpleSpanProcessor } from '@opentelemetry/sdk-trace-node';
import { afterAll, beforeAll, describe, expect, test } from 'vitest';

import * as tracepad from '../../src/index.js';
import { fresh } from '../helpers.js';
import { BINARY, KEY, serve, traceOf, type Store } from './harness.js';

fresh();

const CASES: tracepad.Item[] = [
  { id: tracepad.itemId('reset'), input: { question: 'where does a span land?' }, expected_output: { answer: 'In the trace you are reading.' } },
  { id: tracepad.itemId('refund'), input: { question: 'what does a run pin?' }, expected_output: { answer: 'A dataset version.' } },
  { id: tracepad.itemId('stream'), input: { question: 'how does a stream end?' }, expected_output: { answer: 'When the last chunk does.' } },
];

const ANSWER = {
  model: 'claude-sonnet-5-2026-08-01',
  choices: [{ message: { content: 'In the trace you are reading.' } }],
  usage: { prompt_tokens: 18, completion_tokens: 9, cost: 0.0002 },
};

/** The application under test: its own span, with a generation inside. */
const answer = tracepad.observe(async (question: string) => {
  await tracepad.generation('chat', { model: 'claude-sonnet-5', input: question }, (call) => call.end(ANSWER));
  return ANSWER.choices[0]!.message.content;
}, { name: 'answer-case' });

type Json = Record<string, any>;

describe.skipIf(!BINARY)('the harness against a real binary', () => {
  let store: Store;
  beforeAll(async () => {
    store = await serve();
  });
  afterAll(async () => {
    await store?.stop();
  });

  /** One pass of the whole loop: the run, and the traces each attempt made. */
  async function aRun(name: string): Promise<[Json, string[][]]> {
    await tracepad.scoreConfigs([
      { name: 'accuracy', data_type: 'numeric', direction: 'higher', min: 0, max: 1 },
      { name: 'verdict', data_type: 'categorical', categories: ['pass', 'fail'] },
    ]);
    const golden = tracepad.dataset('support-golden');
    await golden.putItems(CASES);

    const traces: string[][] = [];
    const run = await golden.run(name, { metadata: { prompt: 'support-answer@7' } });
    await run.wrap(async () => {
      for await (const item of golden.items(run.datasetVersion)) {
        await run.item(item, async (attempt) => {
          // Two roots per case: the application answers twice.
          const produced = await answer((item.input as { question: string }).question);
          await answer('again?');
          traces.push([...attempt.traces]);
          attempt.score('accuracy', produced ? 1 : 0);
          attempt.score('verdict', { stringValue: 'pass', dataType: 'categorical' });
        });
      }
    });
    return [await run.get(), traces];
  }

  test('the whole loop', async () => {
    tracepad.init({ host: store.host, key: KEY, environment: 'eval' });

    const [first, traces] = await aRun('prompt v7 / claude-sonnet-5');
    expect(first.status).toBe('finished');
    expect(first.dataset).toBe('support-golden');
    const { summary } = first;
    expect(summary.items).toEqual({ total: 3, covered: 3, missing: 0, unknown: 0 });
    expect(summary.traces.count).toBe(6);
    expect(summary.traces.error_count).toBe(0);
    expect(Object.keys(summary.scores).sort()).toEqual(['accuracy', 'verdict']);
    expect(summary.scores.accuracy.count).toBe(3);
    expect(summary.scores.accuracy.direction).toBe('higher');
    expect(summary.scores.verdict.distribution).toEqual({ pass: 3 });
    expect(summary.models).toEqual(['claude-sonnet-5']);

    // Every trace carries the run and the item, on spans the harness never
    // touched: the processor wrote them at `onStart`. Two roots per attempt,
    // the score on the last.
    expect(traces.map((t) => t.length)).toEqual([2, 2, 2]);
    const ids = new Set(CASES.map((c) => c.id));
    for (const attempt of traces) {
      for (const traceId of attempt) {
        const stored = await traceOf(store, traceId, '');
        expect(stored.run_id).toBe(first.id);
        expect(ids.has(stored.item_id!)).toBe(true);
      }
    }
    const { items } = (await store.call('GET', `/api/v1/runs/${first.id}/items`)) as { items: Json[] };
    expect(items).toHaveLength(3);
    for (const item of items) {
      expect(item.attempts).toHaveLength(2);
      const scored = item.attempts.filter((a: Json) => a.scores.length > 0);
      expect(scored.map((a: Json) => a.trace_id)).toEqual([traces.find((t) => t.includes(scored[0].trace_id))!.at(-1)]);
    }

    // A second run over the same cases, and the comparison the store computes.
    const [second] = await aRun('prompt v8 / claude-sonnet-5');
    const verdicts = await tracepad.compare(first.id, second.id);
    expect((verdicts.a as Json).id).toBe(first.id);
    expect((verdicts.items as Json[]).map((row) => row.in)).toEqual(['both', 'both', 'both']);
    expect((verdicts.items as Json[]).every((row) => 'accuracy' in row.scores)).toBe(true);
  });

  test('the same cases again change nothing, and are read back at their version', async () => {
    tracepad.init({ host: store.host, key: KEY });
    const golden = tracepad.dataset('idempotent');
    const [firstVersion, changed] = await golden.putItems(CASES);
    const [againVersion, again] = await golden.putItems(CASES);
    expect(changed).toBe(3);
    expect([againVersion, again]).toEqual([firstVersion, 0]);

    const stored = [];
    for await (const item of golden.items()) stored.push(item);
    expect(stored.map((item) => item.id)).toEqual(CASES.map((c) => c.id));
    expect(new Set(stored.map((item) => item.version))).toEqual(new Set([firstVersion]));
  });

  test('a run that threw inside wrap is closed as failed', async () => {
    tracepad.init({ host: store.host, key: KEY });
    const golden = tracepad.dataset('failing');
    await golden.putItems(CASES.slice(0, 1));
    const run = await golden.run('doomed');
    await expect(run.wrap(async () => {
      throw new Error('judge timed out');
    })).rejects.toThrow('judge');
    const closed = (await store.call('GET', `/api/v1/runs/${run.id}`)) as Json;
    expect(closed.status).toBe('failed');
    expect(closed.error).toContain('judge timed out');
  });

  test("another SDK's spans are stamped without our exporter", async () => {
    // `spanProcessor({ export: false })` beside the application's own exporter.
    const application = new NodeTracerProvider({
      spanProcessors: [
        new SimpleSpanProcessor(new OTLPTraceExporter({ url: `${store.host}/v1/traces`, headers: { Authorization: `Bearer ${KEY}` } })),
        tracepad.spanProcessor({ host: store.host, key: KEY, export: false }),
      ],
    });
    application.register();
    tracepad.init({ host: store.host, key: KEY, export: false });

    const golden = tracepad.dataset('borrowed');
    await golden.putItems(CASES.slice(0, 1));
    const framework = trace.getTracer('the.framework');
    await using run = await golden.run('over another exporter');
    let traceId = '';
    for await (const item of golden.items(run.datasetVersion)) {
      run.item(item, (attempt) => {
        framework.startActiveSpan('GET /answer', (request) => request.end());
        traceId = attempt.traceId!;
      });
    }
    await application.forceFlush();
    const stored = await traceOf(store, traceId, '');
    expect(stored.run_id).toBe(run.id);
    expect(stored.item_id).toBe(CASES[0]!.id);
    expect(stored.name).toBe('GET /answer');
  });
});
