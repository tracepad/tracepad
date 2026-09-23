/** Deleting traces against a real binary (spec 036 #7): one by id, one by filter. */

import { afterAll, beforeAll, describe, expect, test } from 'vitest';

import * as tracepad from '../../src/index.js';
import { fresh } from '../helpers.js';
import { BINARY, KEY, serve, traceOf, type Store } from './harness.js';

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
});
