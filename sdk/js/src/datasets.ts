/**
 * Datasets and their items (spec 032 #10, spec 018 #1).
 *
 * A `Dataset` has made no request: `tracepad.dataset("x")` is a name, not a
 * network call. Everything below is async and rejects with `TracepadError`
 * — the harness is a script, and a script wants a value or an exception.
 * The read side is the server's JSON as plain objects (spec 018 #8): a
 * model layer is a place to start disagreeing with it.
 */

import { current } from './config.js';
import { PAGE, Run, pages } from './harness.js';
import { type RequestOptions, request } from './http.js';

type Json = Record<string, unknown>;

/** One case, as the API holds it. Tracepad never reads inside `input`, and
 * neither does the type: the payloads are whatever the harness put there. */
export interface Item {
  id?: string;
  input?: any;
  expected_output?: any;
  metadata?: any;
  /** The version this row was written at. Read back, never sent. */
  version?: number;
  source_trace_id?: string;
  source_observation_id?: string;
}

export interface RunOptions {
  metadata?: unknown;
  id?: string;
  datasetVersion?: number;
}

/** A named set of cases, and the runs over it. */
export class Dataset {
  readonly name: string;
  private readonly path: string;

  constructor(name: string) {
    this.name = name;
    this.path = `/api/v1/datasets/${encodeURIComponent(name)}`;
  }

  /** Create it, or replace its description and metadata. */
  create(fields: { description?: string; metadata?: unknown } = {}): Promise<Json> {
    const body: Json = {};
    if (fields.description !== undefined) body.description = fields.description;
    if (fields.metadata !== undefined) body.metadata = fields.metadata;
    return this.call('PUT', '', { body });
  }

  /**
   * Push the cases whole, and report `[version, changed]`.
   *
   * The same cases again write nothing and leave the version where it was
   * (`changed === 0`), so this belongs at the top of every CI run. A list
   * longer than the 10,000 items one request takes is sent as consecutive
   * writes of that many, each its own version (spec 018 #15): `version` is
   * the last one's, `changed` the sum, and a failure leaves the writes before
   * it in place.
   */
  async putItems(items: Iterable<Item>): Promise<[number, number]> {
    const body = [...items].map(sendable);
    if (body.length > MAX_ITEMS_PER_WRITE) refuseRepeatedIds(body);
    let version = 0;
    let changed = 0;
    // An empty list is sent all the same: the server says what is wrong.
    for (let start = 0; start < Math.max(body.length, 1); start += MAX_ITEMS_PER_WRITE) {
      const answer = await this.call('POST', '/items', { body: body.slice(start, start + MAX_ITEMS_PER_WRITE) });
      version = Number(answer.version);
      changed += Number(answer.changed);
    }
    return [version, changed];
  }

  /** The cases at a version — the run's, not "the current one" — over every page. */
  items(version?: number): AsyncIterable<Item> {
    return pages(`${this.path}/items`, { limit: PAGE, version }, 'items') as AsyncIterable<Item>;
  }

  /** Open a run, pinned to a version it hands back (spec 018 #2). */
  async run(name: string, options: RunOptions = {}): Promise<Run> {
    const body: Json = { name };
    if (options.metadata !== undefined) body.metadata = options.metadata;
    if (options.id !== undefined) body.id = options.id;
    if (options.datasetVersion !== undefined) body.dataset_version = options.datasetVersion;
    return new Run(this, await this.call('POST', '/runs', { body }));
  }

  /** This dataset's runs, newest first. */
  runs(): AsyncIterable<Json> {
    return pages(`${this.path}/runs`, { limit: PAGE }, 'runs');
  }

  /** Delete it with its items and runs. The name must be echoed. */
  delete(confirm: string): Promise<Json> {
    return this.call('DELETE', '', { params: { confirm } });
  }

  private async call(method: string, path: string, options: RequestOptions): Promise<Json> {
    return ((await request(current(), method, this.path + path, options)).body as Json) ?? {};
  }
}

/** The most items one `POST …/items` takes (spec 014 #34). */
const MAX_ITEMS_PER_WRITE = 10_000;

/** One request refuses an id given twice; split across two, the second would
 * quietly become an edit of the first. So the whole list is checked before
 * any of it is sent. */
function refuseRepeatedIds(body: Json[]): void {
  const seen = new Map<unknown, number>();
  body.forEach((item, index) => {
    // A null id is no id: the server generates one, as it does for none.
    if (item.id === undefined || item.id === null) return;
    const first = seen.get(item.id);
    if (first !== undefined) {
      throw new Error(`tracepad: putItems: the item at index ${index} repeats id ${String(item.id)} of the item at index ${first}`);
    }
    seen.set(item.id, index);
  });
}

/** The fields `POST …/items` takes; the server refuses any other. */
const SENT = ['id', 'input', 'expected_output', 'metadata', 'source_trace_id', 'source_observation_id'] as const;

/** What goes on the wire: the item's fields and no others — a row read back
 * from `items()` carries the version it was written at, its `seq` and its
 * timestamps too, and pushing it whole would be refused as unknown fields. */
function sendable(item: Item): Json {
  const body: Json = {};
  for (const name of SENT) {
    const value = (item as Json)[name];
    if (value !== undefined) body[name] = value;
  }
  return body;
}

/** A dataset by name. No request is made here. */
export function dataset(name: string): Dataset {
  return new Dataset(name);
}
