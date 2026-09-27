/**
 * Deleting traces against a real binary (spec 036 #7): one by id, one by
 * filter, and never with a key whose scopes do not hold `write` (spec 045 #16).
 */

import { afterAll, beforeAll, describe, expect, test } from 'vitest';

import * as tracepad from '../../src/index.js';
import { fresh } from '../helpers.js';
import { BINARY, KEY, mintKey, serve, traceOf, type Store } from './harness.js';

fresh();

describe.skipIf(!BINARY)('against a real binary', () => {
  let store: Store;
  beforeAll(async () => {
    store = await serve();
  });
  afterAll(async () => {
    await store?.stop();
  });

  test('two traces go, one by id and one by filter, and the listing no longer has them', async () => {
    tracepad.init({ host: store.host, key: KEY });
    const ids = ['first', 'second'].map((name) =>
      tracepad.span(name, (step) => {
        tracepad.updateTrace({ name: 'doomed', tags: ['doomed'] });
        return step.traceId!;
      }),
    );
    await tracepad.flush({ timeout: 20_000 });
    const [byId, byFilter] = await Promise.all(ids.map((id) => traceOf(store, id).then((t) => t.id)));

    const preview = await tracepad.deleteTrace(byId!);
    expect(preview).toMatchObject({ dry_run: true, confirm: byId, would_delete: { traces: 1 } });
    expect(await tracepad.deleteTrace(byId!, { confirm: true })).toMatchObject({ deleted: { traces: 1 }, id: byId });

    const to = new Date(Date.now() + 60_000);
    expect(await tracepad.deleteTraces({ to, tag: ['doomed'] })).toMatchObject({ matched: 1, confirm: 'e2e' });
    expect(await tracepad.deleteTraces({ to, tag: ['doomed'] }, { confirm: 'e2e' })).toMatchObject({
      deleted: { traces: 1, observations: 1 },
      rounds: 1,
    });

    expect(((await store.call('GET', '/api/v1/traces?tag=doomed')) as { traces: unknown[] }).traces).toEqual([]);
    await expect(tracepad.deleteTrace(byFilter!)).rejects.toMatchObject({ status: 404 });
  });

  test('an ingest key covers the production path and not deletion (spec 045 #16)', async () => {
    await store.call('POST', '/api/v1/prompts/ingest-answer/versions', {
      type: 'text',
      prompt: 'Answer {topic}.',
      labels: ['production'],
    });
    tracepad.init({ host: store.host, key: await mintKey(store, 'ingest') });

    expect((await tracepad.prompt('ingest-answer', { label: 'production' })).version).toBe(1);
    const traceId = tracepad.span('ingest-only', (step) => {
      tracepad.updateTrace({ tags: ['ingest-only'] });
      tracepad.score('helpful', 1);
      return step.traceId!;
    });
    await tracepad.flush({ timeout: 20_000 });
    await traceOf(store, traceId);
    const { scores } = (await store.call('GET', `/api/v1/scores?trace_id=${traceId}`)) as {
      scores: { name: string; value: number }[];
    };
    expect(scores.map((s) => [s.name, s.value])).toEqual([['helpful', 1]]);

    const to = new Date(Date.now() + 60_000);
    await expect(tracepad.deleteTraces({ to, tag: ['ingest-only'] }, { confirm: 'e2e' })).rejects.toMatchObject({
      status: 403,
      message: expect.stringContaining("this key's scopes are ingest; DELETE /api/v1/traces needs write"),
    });
    expect((await traceOf(store, traceId)).id).toBe(traceId);
  });
});
