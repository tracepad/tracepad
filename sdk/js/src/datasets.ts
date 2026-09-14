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
   * (`changed === 0`), so this belongs at the top of every CI run.
   */
  async putItems(items: Iterable<Item>): Promise<[number, number]> {
    const answer = await this.call('POST', '/items', { body: [...items].map(sendable) });
    return [Number(answer.version), Number(answer.changed)];
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
