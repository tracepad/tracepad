/**
 * The REST half: the global `fetch`, bearer auth, and the errors it rejects with.
 *
 * Failure semantics are split by path (spec 032 #8). The tracing path never
 * throws into application code and logs instead; the REST path — `prompt`,
 * `flush`, the harness's clients — rejects, because a caller that asked for
 * a value must not be handed `undefined` with a log line nobody reads.
 */

import type { Config } from './config.js';
import pkg from '../package.json' with { type: 'json' };

export const VERSION: string = pkg.version;

const USER_AGENT = `tracepad-js/${VERSION}`;

/** Anything the REST path could not do. */
export class TracepadError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'TracepadError';
  }
}

/** A non-2xx answer, with the server's own message kept whole. */
export class TracepadHTTPError extends TracepadError {
  readonly status: number;
  readonly body: string;

  constructor(status: number, body: string) {
    super(`tracepad: HTTP ${status}: ${body.trim()}`);
    this.name = 'TracepadHTTPError';
    this.status = status;
    this.body = body;
  }
}

/** `init` was given no host or no key, and the environment has none. */
export class TracepadConfigError extends TracepadError {
  constructor(message: string) {
    super(message);
    this.name = 'TracepadConfigError';
  }
}

export interface Response {
  status: number;
  body: unknown;
  headers: Headers;
}

/** Query parameters; a list is sent as a repeated name (`tag=a&tag=b`). */
export type Params = Record<string, string | number | string[] | undefined>;

export interface RequestOptions {
  body?: unknown;
  params?: Params;
  timeout?: number;
}

/**
 * One JSON call. Rejects with `TracepadHTTPError` for a non-2xx answer and
 * `TracepadError` for everything that never got one.
 */
export async function request(
  config: Config,
  method: string,
  path: string,
  { body, params, timeout = 10_000 }: RequestOptions = {},
): Promise<Response> {
  // Every name in a path is encoded by its caller, and that leaves the two
  // segments encoding cannot hide: the URL parser resolves `..` before the
  // request leaves, so it would name another object. No name the store
  // accepts is empty or dots (spec 032 #17).
  if (path.split('/').slice(1).some((part) => part === '' || part === '.' || part === '..')) {
    throw new TracepadError(`tracepad: ${method} ${path}: an empty or dot segment names nothing`);
  }
  let url = config.host + path;
  const query = new URLSearchParams();
  for (const [name, value] of Object.entries(params ?? {})) {
    for (const one of Array.isArray(value) ? value : [value]) {
      if (one !== undefined) query.append(name, String(one));
    }
  }
  if (query.size > 0) url += `?${query}`;

  let answer: globalThis.Response;
  try {
    answer = await fetch(url, {
      method,
      headers: {
        Authorization: `Bearer ${config.key}`,
        'Content-Type': 'application/json',
        'User-Agent': USER_AGENT,
      },
      body: body === undefined ? null : JSON.stringify(body),
      // A write is never re-sent where a redirect points: `fetch` turns a
      // POST into a GET on 301–303, and a listing's `200` would read as the
      // batch delivered (spec 032 #17).
      redirect: method === 'GET' || method === 'HEAD' ? 'follow' : 'manual',
      signal: AbortSignal.timeout(timeout),
    });
  } catch (error) {
    throw new TracepadError(`tracepad: ${method} ${config.host}${path}: ${describe(error)}`);
  }
  const raw = await answer.text();
  if (answer.status >= 300 && answer.status < 400) {
    const location = answer.headers.get('location') ?? 'nowhere';
    throw new TracepadHTTPError(answer.status, `a redirect to ${location} is not followed for ${method}; point the host at the store itself`);
  }
  if (!answer.ok) {
    // The body is where the store names the offending field or item, and
    // it is the whole value of rejecting rather than logging a status.
    throw new TracepadHTTPError(answer.status, raw);
  }
  return { status: answer.status, body: decode(raw), headers: answer.headers };
}

function decode(raw: string): unknown {
  if (raw === '') return undefined;
  try {
    return JSON.parse(raw);
  } catch {
    return raw;
  }
}

/** An error as one line: `fetch` wraps the real cause one level down. */
export function describe(error: unknown): string {
  if (error instanceof Error) {
    const cause = error.cause instanceof Error ? `: ${error.cause.message}` : '';
    return `${error.name}: ${error.message}${cause}`;
  }
  return String(error);
}

/**
 * How long the server said this answer may be trusted, in seconds.
 *
 * A header that says nothing is zero, not a default of our own: the store
 * sends `Cache-Control: max-age=60` on every prompt (`docs/prompts.md`), and
 * inventing a window for a server that did not ask for one would cache
 * against its wishes.
 */
export function maxAge(headers: Headers): number {
  for (const directive of (headers.get('cache-control') ?? '').split(',')) {
    const [name, value] = directive.trim().split('=');
    if (name?.toLowerCase() === 'max-age') {
      const seconds = Number.parseInt(value ?? '', 10);
      return Number.isFinite(seconds) ? Math.max(seconds, 0) : 0;
    }
  }
  return 0;
}
