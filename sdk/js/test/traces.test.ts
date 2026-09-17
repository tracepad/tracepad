/** Deleting traces: the dry run, the echo, the rounds (spec 036). */

import { describe, expect, test, vi } from 'vitest';

import * as tracepad from '../src/index.js';
import { HOST, KEY, fakeFetch, fresh, type Call } from './helpers.js';

fresh();

const TRACE = '4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f';

function requests(calls: Call[]): [string, string][] {
  return calls.map((c) => [c.method, c.url.replace(HOST, '')]);
}

describe('one trace', () => {
  test('the dry run sends no confirm, and the confirm echoes the id', async () => {
    const calls = fakeFetch(() => ({ body: { dry_run: true, confirm: TRACE } }));
    tracepad.init({ host: HOST, key: KEY, export: false });
    expect(await tracepad.deleteTrace(TRACE)).toEqual({ dry_run: true, confirm: TRACE });
    await tracepad.deleteTrace(TRACE, { confirm: true });
    expect(requests(calls)).toEqual([
      ['DELETE', `/api/v1/traces/${TRACE}`],
      ['DELETE', `/api/v1/traces/${TRACE}?confirm=${TRACE}`],
    ]);
  });

  test('an unknown id rejects with the 404, never resolves undefined', async () => {
    fakeFetch(() => ({ status: 404, body: { error: 'not found' } }));
    tracepad.init({ host: HOST, key: KEY, export: false });
    await expect(tracepad.deleteTrace(TRACE, { confirm: true })).rejects.toMatchObject({
      name: 'TracepadHTTPError',
      status: 404,
    });
  });
});

describe('by filter', () => {
  test('the dry run passes the filters through, times as RFC 3339 UTC, and no confirm or limit', async () => {
    const calls = fakeFetch(() => ({ body: { dry_run: true, matched: 3 } }));
    tracepad.init({ host: HOST, key: KEY, export: false });
    const preview = await tracepad.deleteTraces(
      { to: new Date('2026-09-17T14:02:17Z'), from: '2026-09-01T00:00:00Z', environment: 'staging', tag: ['a', 'b'] },
      { limit: 5 },
    );
    expect(preview).toEqual({ dry_run: true, matched: 3 });
    expect(requests(calls)).toEqual([
      ['DELETE', '/api/v1/traces?to=2026-09-17T14%3A02%3A17.000Z&from=2026-09-01T00%3A00%3A00Z&environment=staging&tag=a&tag=b'],
    ]);
  });

  test('the confirmed call walks the rounds while `more` and sums them', async () => {
    const answers = [
      { deleted: { traces: 1000, observations: 4000, payloads: 9 }, more: true },
      { deleted: { traces: 1000, observations: 3000, payloads: 0 }, more: true },
      { deleted: { traces: 12, observations: 30, payloads: 1 }, more: false },
    ];
    const calls = fakeFetch(() => ({ body: answers.shift() }));
    const timeouts: number[] = [];
    vi.spyOn(AbortSignal, 'timeout').mockImplementation((ms) => {
      timeouts.push(ms);
      return new AbortController().signal;
    });
    tracepad.init({ host: HOST, key: KEY, export: false });
    const total = await tracepad.deleteTraces({ to: '2026-09-17T14:02:17Z', environment: 'staging' }, { confirm: 'my-project' });
    expect(total).toEqual({ deleted: { traces: 2012, observations: 7030, payloads: 10 }, rounds: 3 });
    expect(requests(calls)).toEqual(
      Array(3).fill(['DELETE', '/api/v1/traces?to=2026-09-17T14%3A02%3A17Z&environment=staging&confirm=my-project&limit=1000']),
    );
    // A round waits longer than the helper's default: the server sizes one
    // for the interface's thirty-second clock.
    expect(timeouts).toEqual([60_000, 60_000, 60_000]);
  });

  test('a round that finds nothing is one round of zero', async () => {
    fakeFetch(() => ({ body: { deleted: { traces: 0 }, more: false } }));
    tracepad.init({ host: HOST, key: KEY, export: false });
    expect(await tracepad.deleteTraces({ to: '2026-09-17T14:02:17Z' }, { confirm: 'my-project' })).toEqual({
      deleted: { traces: 0 },
      rounds: 1,
    });
  });

  test('a wrong echo is the 400 of the first round, and no second one', async () => {
    const calls = fakeFetch(() => ({ status: 400, body: { error: 'confirm' } }));
    tracepad.init({ host: HOST, key: KEY, export: false });
    await expect(tracepad.deleteTraces({ to: '2026-09-17T14:02:17Z' }, { confirm: 'wrong' })).rejects.toMatchObject({ status: 400 });
    expect(calls).toHaveLength(1);
  });
});
