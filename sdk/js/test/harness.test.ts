/** The eval harness: the stamping, the run and the datasets (spec 032 #10). */

import { trace } from '@opentelemetry/api';
import type { NodeTracerProvider } from '@opentelemetry/sdk-trace-node';
import { describe, expect, test, vi } from 'vitest';

import { ITEM_ID, RUN_ID } from '../src/attributes.js';
import * as tracepad from '../src/index.js';
import { ScoreQueue, reset } from '../src/scores.js';
import { HOST, KEY, fakeFetch, fresh, registered, spans, warnings, type Call } from './helpers.js';

fresh();

const RUN = { id: 'run-1', name: 'prompt v7', dataset_version: 3, status: 'running' };

/** A store that answers the harness's calls from a script, recording them. */
function serving(script: (call: Call) => ReturnType<Parameters<typeof fakeFetch>[0]> = () => ({ body: {} })) {
  return fakeFetch((call) => {
    if (call.method === 'POST' && call.url.endsWith('/runs')) return { body: RUN };
    return script(call);
  });
}

describe('the item block', () => {
  test('stamps every span the application starts inside it, whoever started it', async () => {
    const seen = spans();
    serving();
    const run = await tracepad.dataset('golden').run('prompt v7');
    const framework = trace.getTracer('the.framework');

    const attempt = run.item({ id: 'case-1' }, (attempt) => {
      framework.startActiveSpan('GET /answer', (request) => {
        tracepad.span('answer', () => undefined);
        request.end();
      });
      return attempt;
    });
    tracepad.span('after', () => undefined);

    const [answer, request, after] = seen.spans;
    for (const span of [answer!, request!]) {
      expect(span.attributes[RUN_ID]).toBe('run-1');
      expect(span.attributes[ITEM_ID]).toBe('case-1');
    }
    expect(after!.attributes).not.toHaveProperty(RUN_ID);
    // One trace, recorded once: the framework's root began the case.
    expect(attempt.traces).toEqual([request!.spanContext().traceId]);
    expect(attempt.traceId).toBe(request!.spanContext().traceId);
    expect(attempt.attributes()).toEqual({ [RUN_ID]: 'run-1', [ITEM_ID]: 'case-1' });
  });

  test('reaches through an await and a callback, and an inner item wins for its duration', async () => {
    const seen = spans();
    serving();
    const run = await tracepad.dataset('golden').run('prompt v7');
    await run.item('outer', async () => {
      await new Promise((tick) => setTimeout(tick, 1));
      tracepad.span('one', () => undefined);
      await run.item('inner', async () => {
        await Promise.resolve();
        tracepad.span('two', () => undefined);
      });
      tracepad.span('three', () => undefined);
    });
    expect(seen.spans.map((s) => [s.name, s.attributes[ITEM_ID]])).toEqual([
      ['one', 'outer'],
      ['two', 'inner'],
      ['three', 'outer'],
    ]);
  });

  test('a case begins where the block does: a loop under a span of its own still has a trace', async () => {
    const seen = spans();
    serving();
    const run = await tracepad.dataset('golden').run('prompt v7');
    const attempts = tracepad.span('the-loop', () =>
      ['a', 'b'].map((id) =>
        run.item(id, (attempt) => {
          tracepad.span('answer', () => tracepad.span('child', () => undefined));
          return attempt;
        }),
      ),
    );
    const loop = seen.one('the-loop');
    expect(loop.attributes).not.toHaveProperty(RUN_ID);
    for (const attempt of attempts) expect(attempt.traces).toEqual([loop.spanContext().traceId]);
  });

  test('records every root the block started, in order, and scores the last', async () => {
    spans();
    serving();
    const sent: unknown[][] = [];
    reset(new ScoreQueue(async (batch) => void sent.push(batch)));
    const run = await tracepad.dataset('golden').run('prompt v7');
    const attempt = run.item('case-1', (attempt) => {
      expect(() => attempt.score('accuracy', 1)).toThrow('no trace has started inside this item block yet');
      const first = tracepad.span('try-1', (o) => o.traceId!);
      const second = tracepad.span('try-2', (o) => o.traceId!);
      expect(attempt.traces).toEqual([first, second]);
      attempt.score('accuracy', 0.5, { comment: 'second try' });
      attempt.score('verdict', { stringValue: 'pass', dataType: 'categorical' });
      attempt.score('accuracy', 0, { traceId: first }); // the first try, named
      return attempt;
    });
    await tracepad.flush();
    expect(sent).toEqual([[
      { name: 'accuracy', trace_id: attempt.traceId, value: 0.5, comment: 'second try' },
      { name: 'verdict', trace_id: attempt.traceId, string_value: 'pass', data_type: 'categorical' },
      { name: 'accuracy', trace_id: attempt.traces[0], value: 0 },
    ]]);
  });

  test('needs an item id', async () => {
    spans();
    serving();
    const run = await tracepad.dataset('golden').run('prompt v7');
    expect(() => run.item({ input: 'no id' }, () => undefined)).toThrow('needs an item id');
    expect(() => run.item('', () => undefined)).toThrow('needs an item id');
  });

  test('is stamped under export: false too', () => {
    const seen = spans(); // the helper's provider carries `spanProcessor({ export: false })`
    const run = new tracepad.Run(tracepad.dataset('golden'), RUN);
    run.item('case-1', () => tracepad.span('answer', () => undefined));
    expect(seen.one('answer').attributes[RUN_ID]).toBe('run-1');
  });
});

describe('the run', () => {
  test('opens pinned to a version, and finish flushes before it posts', async () => {
    const order: string[] = [];
    reset(new ScoreQueue(async () => void order.push('scores')));
    const calls = serving((call) => {
      order.push(`${call.method} ${new URL(call.url).pathname}`);
      return { body: { id: 'run-1', status: 'finished', summary: { traces: 1 } } };
    });
    tracepad.init({ host: HOST, key: KEY, export: false });
    (registered() as NodeTracerProvider).forceFlush = async () => void order.push('spans');

    const run = await tracepad.dataset('golden').run('prompt v7', { metadata: { prompt: 'v7' }, id: 'run-1', datasetVersion: 3 });
    expect(calls[0]).toMatchObject({
      method: 'POST',
      url: `${HOST}/api/v1/datasets/golden/runs`,
      body: { name: 'prompt v7', metadata: { prompt: 'v7' }, id: 'run-1', dataset_version: 3 },
    });
    expect([run.id, run.name, run.datasetVersion, run.dataset.name]).toEqual(['run-1', 'prompt v7', 3, 'golden']);

    tracepad.score('accuracy', 1, { traceId: 'a'.repeat(32) });
    const closed = await run.finish();
    expect(closed).toMatchObject({ status: 'finished' });
    expect(order).toEqual(['scores', 'spans', 'POST /api/v1/runs/run-1/finish']);
    expect(calls.at(-1)!.body).toEqual({});
    expect(await run.get()).toMatchObject({ summary: { traces: 1 } });
  });

  test('fail posts the reason, and wrap chooses between the two', async () => {
    const calls = serving();
    tracepad.init({ host: HOST, key: KEY, export: false });
    const golden = tracepad.dataset('golden');

    await expect((await golden.run('a')).wrap(async () => {
      throw new TypeError('judge timed out');
    })).rejects.toThrow('judge timed out');
    expect(calls.at(-1)).toMatchObject({ url: `${HOST}/api/v1/runs/run-1/finish`, body: { status: 'failed', error: 'TypeError: judge timed out' } });

    expect(await (await golden.run('b')).wrap(async (run) => run.id)).toBe('run-1');
    expect(calls.at(-1)).toMatchObject({ url: `${HOST}/api/v1/runs/run-1/finish`, body: {} });

    // A run closed by hand inside wrap is not closed twice.
    const before = calls.length;
    await (await golden.run('c')).wrap(async (run) => {
      await run.fail('by hand');
    });
    expect(calls.length - before).toBe(2); // the open, one close
  });

  test('wrap rethrows the harness\'s own error when the close is refused, and says so', async () => {
    serving((call) => (call.url.endsWith('/finish') ? { status: 503, body: 'away' } : { body: {} }));
    tracepad.init({ host: HOST, key: KEY, export: false });
    const run = await tracepad.dataset('golden').run('a');
    await expect(run.wrap(async () => {
      throw new TypeError('judge timed out');
    })).rejects.toThrow('judge timed out');
    expect(warnings).toEqual([expect.stringMatching(/^tracepad: run\.wrap\(\): the run could not be failed and stays running: TracepadHTTPError: /)]);
  });

  test('is await using-compatible: disposal is finish, once', async () => {
    const calls = serving();
    tracepad.init({ host: HOST, key: KEY, export: false });
    {
      await using run = await tracepad.dataset('golden').run('prompt v7');
      expect(run.id).toBe('run-1');
    }
    expect(calls.map((c) => new URL(c.url).pathname)).toEqual(['/api/v1/datasets/golden/runs', '/api/v1/runs/run-1/finish']);
    {
      await using run = await tracepad.dataset('golden').run('prompt v7');
      await run.finish();
    }
    expect(calls.filter((c) => c.url.endsWith('/finish'))).toHaveLength(2);
  });

  test('a refused finish leaves the run open for fail', async () => {
    let refuse = true;
    serving(() => (refuse ? { status: 500, body: 'no' } : { body: {} }));
    tracepad.init({ host: HOST, key: KEY, export: false });
    const run = await tracepad.dataset('golden').run('prompt v7');
    await expect(run.finish()).rejects.toThrow(tracepad.TracepadHTTPError);
    refuse = false;
    await expect(run.wrap(async () => undefined)).resolves.toBeUndefined();
  });

  test('items sends no limit of its own, and takes one', async () => {
    const calls = serving(() => ({ body: { items: [{ id: 'x' }], next_cursor: null } }));
    tracepad.init({ host: HOST, key: KEY, export: false });
    const run = await tracepad.dataset('golden').run('prompt v7');
    const rows = [];
    for await (const row of run.items()) rows.push(row);
    for await (const row of run.items({ unknown: true, limit: 20 })) rows.push(row);
    expect(rows).toEqual([{ id: 'x' }, { id: 'x' }]);
    expect(calls.slice(1).map((c) => c.url)).toEqual([
      `${HOST}/api/v1/runs/run-1/items`,
      `${HOST}/api/v1/runs/run-1/items?limit=20&unknown=true`,
    ]);
  });
});

describe('the dataset', () => {
  test('makes no request until asked, then walks every page of its items', async () => {
    const calls = fakeFetch((call) => {
      const url = new URL(call.url);
      const cursor = url.searchParams.get('cursor');
      if (cursor === null) return { body: { items: [{ id: 'a', input: 1, version: 2 }], next_cursor: 'c1' } };
      if (cursor === 'c1') return { body: { items: [{ id: 'b', input: 2, version: 3 }], next_cursor: '' } };
      throw new Error(`unexpected cursor ${cursor}`);
    });
    tracepad.init({ host: HOST, key: KEY, export: false });
    const golden = tracepad.dataset('golden');
    expect(calls).toEqual([]);
    const items = [];
    for await (const item of golden.items(3)) items.push(item);
    expect(items).toEqual([{ id: 'a', input: 1, version: 2 }, { id: 'b', input: 2, version: 3 }]);
    expect(calls.map((c) => c.url)).toEqual([
      `${HOST}/api/v1/datasets/golden/items?limit=500&version=3`,
      `${HOST}/api/v1/datasets/golden/items?limit=500&version=3&cursor=c1`,
    ]);
  });

  test('putItems sends the cases\' fields and nothing the store added, and reports the tick', async () => {
    const calls = fakeFetch(() => ({ body: { version: 4, changed: 1 } }));
    tracepad.init({ host: HOST, key: KEY, export: false });
    // The first is a row as `items()` yields it: what the store adds to a
    // case is not what `POST` takes back.
    const readBack = { id: 'a', input: { q: 1 }, expected_output: 'x', version: 3, seq: 7, created_at: '2026-09-14T00:00:00Z', archived: false };
    const result = await tracepad.dataset('golden').putItems([
      readBack as tracepad.Item,
      { input: 'bare', metadata: undefined },
    ]);
    expect(result).toEqual([4, 1]);
    expect(calls[0]).toMatchObject({
      method: 'POST',
      url: `${HOST}/api/v1/datasets/golden/items`,
      body: [{ id: 'a', input: { q: 1 }, expected_output: 'x' }, { input: 'bare' }],
    });
    expect(Object.keys((calls[0]!.body as unknown[])[0] as object)).toEqual(['id', 'input', 'expected_output']);
  });

  test('putItems sends a long list in writes the server takes, in order', async () => {
    // The server takes at most 10,000 items a request (spec 014 #34).
    const answers = [{ version: 5, changed: 10_000 }, { version: 5, changed: 0 }, { version: 6, changed: 1 }];
    const calls = fakeFetch(() => ({ body: answers.shift() }));
    tracepad.init({ host: HOST, key: KEY, export: false });
    const cases = Array.from({ length: 20_001 }, (_, n) => ({ input: n }));
    expect(await tracepad.dataset('golden').putItems(cases)).toEqual([6, 10_001]);
    expect(calls.map((c) => (c.body as unknown[]).length)).toEqual([10_000, 10_000, 1]);
    expect(calls.flatMap((c) => c.body as unknown[])).toEqual(cases);
  });

  test('putItems refuses a long list that repeats an id, sending nothing', async () => {
    // One request would be refused whole for it; split, the second would
    // quietly become an edit of the first.
    const calls = fakeFetch(() => ({ body: { version: 1, changed: 1 } }));
    tracepad.init({ host: HOST, key: KEY, export: false });
    const cases: tracepad.Item[] = Array.from({ length: 10_001 }, (_, n) => ({ input: n }));
    cases[3]!.id = 'a';
    cases[10_000]!.id = 'a';
    await expect(tracepad.dataset('golden').putItems(cases)).rejects.toThrow(/index 10000 .* index 3/);
    expect(calls).toEqual([]);
  });

  test('putItems reads a null id as none, not as a repeated one', async () => {
    const calls = fakeFetch(() => ({ body: { version: 1, changed: 1 } }));
    tracepad.init({ host: HOST, key: KEY, export: false });
    const cases = Array.from({ length: 10_001 }, (_, n) => ({ id: null, input: n }));
    await tracepad.dataset('golden').putItems(cases as unknown as tracepad.Item[]);
    expect(calls).toHaveLength(2);
  });

  test('putItems sends an empty list for the server to refuse', async () => {
    const calls = fakeFetch(() => ({ status: 400, body: { error: 'no items in the request' } }));
    tracepad.init({ host: HOST, key: KEY, export: false });
    await expect(tracepad.dataset('golden').putItems([])).rejects.toThrow(/no items/);
    expect(calls.map((c) => c.body)).toEqual([[]]);
  });

  test('create, runs and delete are the endpoints, and the name is echoed', async () => {
    const calls = fakeFetch(() => ({ body: { runs: [{ id: 'r' }], next_cursor: null } }));
    tracepad.init({ host: HOST, key: KEY, export: false });
    const golden = tracepad.dataset('support/golden');
    await golden.create({ description: 'the cases', metadata: { owner: 'qa' } });
    const runs = [];
    for await (const run of golden.runs()) runs.push(run);
    expect(runs).toEqual([{ id: 'r' }]);
    await golden.delete('support/golden');
    expect(calls.map((c) => [c.method, c.url.replace(HOST, ''), c.body])).toEqual([
      ['PUT', '/api/v1/datasets/support%2Fgolden', { description: 'the cases', metadata: { owner: 'qa' } }],
      ['GET', '/api/v1/datasets/support%2Fgolden/runs?limit=500', undefined],
      ['DELETE', '/api/v1/datasets/support%2Fgolden?confirm=support%2Fgolden', undefined],
    ]);
  });
});

describe('the rest', () => {
  test('scoreConfigs PUTs each in order and rethrows with the name', async () => {
    const calls = fakeFetch((call) => (call.url.endsWith('/verdict') ? { status: 400, body: { error: 'categories: required' } } : { body: {} }));
    tracepad.init({ host: HOST, key: KEY, export: false });
    await expect(tracepad.scoreConfigs([
      { name: 'accuracy', data_type: 'numeric', direction: 'higher', min: 0, max: 1 },
      { name: 'verdict', data_type: 'categorical' },
      { name: 'never', data_type: 'boolean' },
    ])).rejects.toMatchObject({ status: 400, body: 'score config "verdict": {"error":"categories: required"}' });
    expect(calls.map((c) => [c.method, c.url.replace(HOST, ''), c.body])).toEqual([
      ['PUT', '/api/v1/score-configs/accuracy', { data_type: 'numeric', direction: 'higher', min: 0, max: 1 }],
      ['PUT', '/api/v1/score-configs/verdict', { data_type: 'categorical' }],
    ]);
  });

  test('a transport failure in scoreConfigs names the config too', async () => {
    fakeFetch(() => new TypeError('fetch failed'));
    tracepad.init({ host: HOST, key: KEY, export: false });
    await expect(tracepad.scoreConfigs([{ name: 'accuracy', data_type: 'numeric' }])).rejects.toThrow('score config "accuracy": tracepad: PUT');
  });

  test('itemId is sha256 cut to 32, and compare is the server\'s answer whole', async () => {
    expect(tracepad.itemId('cases/refund.json')).toMatch(/^[0-9a-f]{32}$/);
    expect(tracepad.itemId('a')).toBe('ca978112ca1bbdcafac231b39a23dc4d');
    const calls = fakeFetch(() => ({ body: { a: { id: 'x' }, b: { id: 'y' }, scores: [] } }));
    tracepad.init({ host: HOST, key: KEY, export: false });
    expect(await tracepad.compare('x', 'y')).toEqual({ a: { id: 'x' }, b: { id: 'y' }, scores: [] });
    expect(calls[0]!.url).toBe(`${HOST}/api/v1/runs/x/compare/y`);
    expect(warnings).toEqual([]);
  });
});
