/**
 * Deleting traces: one by id, or every trace a listing filter matches (spec 036).
 *
 * A script's act — an eval harness that exported under the wrong key, a test
 * suite that wants its traces gone before the next run — so both calls
 * reject like every REST call here. The module is its own so that the
 * tracing path never imports it.
 */

import { current } from './config.js';
import { type Params, request } from './http.js';

type Json = Record<string, unknown>;

/** The most traces one confirmed round may take: the API's own bound. */
const ROUND = 1000;
/** How long one confirmed round may take: the server sizes a round for the
 * interface's thirty-second clock, and this leaves room over it. */
const ROUND_TIMEOUT = 60_000;

/**
 * The trace listing's filters, by their API names (`docs/api.md`); `to` is
 * required so that the set is closed. A time is a `Date` or an RFC 3339
 * string; `tag` is repeatable. A name the API does not know is its 400,
 * rejected — the package keeps no list of its own.
 */
export interface TraceFilter {
  to: Date | string;
  from?: Date | string;
  tag?: string[];
  [name: string]: string | number | string[] | Date | undefined;
}

export interface DeleteTracesOptions {
  /** The project's name, the echo the API asks for; empty is the dry run. */
  confirm?: string;
  /** The most traces one round takes (1 to 1000); the loop is unbounded. */
  limit?: number;
}

/**
 * Delete one trace and everything attached to it.
 *
 * Without `confirm` this is the API's dry run, and the answer is its preview
 * (`would_delete`, `affected_runs`, `confirm`, `note`). With `confirm: true`
 * the id is sent as the echo the API asks for, and the answer says what
 * went. An unknown id rejects with a 404 `TracepadHTTPError` either way.
 */
export function deleteTrace(id: string, { confirm = false }: { confirm?: boolean } = {}): Promise<Json> {
  return call(`/api/v1/traces/${encodeURIComponent(id)}`, confirm ? { confirm: id } : {});
}

/**
 * Delete every trace the filter matches that started before `to`.
 *
 * With no `confirm` this is one dry run, and the answer is the API's preview
 * (`matched`, `would_delete`, `affected_runs`, `oldest`, `confirm`, `note`).
 * With the project's name it deletes in rounds of at most `limit` traces,
 * repeating while the API says `more`, and resolves with the total:
 * `{ deleted: { traces, observations, scores, payloads, annotation_items }, rounds }`.
 * A round that fails rejects as it is — the rounds before it are done and
 * consistent, and a repeat continues.
 */
export async function deleteTraces(
  filter: TraceFilter,
  { confirm = '', limit = ROUND }: DeleteTracesOptions = {},
): Promise<Json> {
  const params: Params = {};
  for (const [name, value] of Object.entries(filter)) params[name] = value instanceof Date ? value.toISOString() : value;
  if (!confirm) return call('/api/v1/traces', params);
  Object.assign(params, { confirm, limit });
  const deleted: Record<string, number> = {};
  let rounds = 0;
  for (;;) {
    const answer = await call('/api/v1/traces', params, ROUND_TIMEOUT);
    rounds += 1;
    for (const [kind, count] of Object.entries((answer.deleted as Record<string, number> | undefined) ?? {})) {
      deleted[kind] = (deleted[kind] ?? 0) + count;
    }
    if (!answer.more) return { deleted, rounds };
  }
}

async function call(path: string, params: Params, timeout = 10_000): Promise<Json> {
  return ((await request(current(), 'DELETE', path, { params, timeout })).body as Json) ?? {};
}
