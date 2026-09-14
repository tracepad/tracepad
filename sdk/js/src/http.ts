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

export interface RequestOptions {
  body?: unknown;
  params?: Record<string, string | number | undefined>;
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
  let url = config.host + path;
  const query = new URLSearchParams();
  for (const [name, value] of Object.entries(params ?? {})) {
    if (value !== undefined) query.set(name, String(value));
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
      signal: AbortSignal.timeout(timeout),
    });
  } catch (error) {
    throw new TracepadError(`tracepad: ${method} ${config.host}${path}: ${describe(error)}`);
  }
  const raw = await answer.text();
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
