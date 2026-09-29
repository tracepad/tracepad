/**
 * A clean process for every test.
 *
 * `init`, the global `TracerProvider` and the score queue are all
 * process-wide by design — the OTel API refuses to set a provider twice on
 * purpose — so the suite resets them between tests rather than pretending
 * they are not. The reset and the capture are the public `tracepad/testing`
 * (spec 040 #7): the suite exercises the helpers an application tests with,
 * and the environment scrub, the logger and the scripted `fetch` stay here.
 */

import { type ProxyTracerProvider, type TracerProvider, trace } from '@opentelemetry/api';
import { afterEach, beforeEach, vi } from 'vitest';

import { adopt } from '../src/config.js';
import { setLogger } from '../src/log.js';
import { type Capture, capture, reset } from '../src/testing.js';
import { FOLLOWER } from '../src/tracing.js';

export const HOST = 'http://tracepad.test:4318';
export const KEY = 'tp-sk-test';

/** Every line the package warned with, in order. */
export const warnings: string[] = [];

const intoWarnings = { warn: (message: string) => warnings.push(message) };

export function fresh(): void {
  beforeEach(() => {
    for (const variable of [
      'TRACEPAD_URL',
      'TRACEPAD_HOST',
      'TRACEPAD_API_KEY',
      'TRACEPAD_ENVIRONMENT',
      'TRACEPAD_RELEASE',
      'OTEL_SERVICE_NAME',
      'OTEL_RESOURCE_ATTRIBUTES',
    ]) {
      delete process.env[variable];
    }
    warnings.length = 0;
    reset();
    setLogger(intoWarnings);
  });
  afterEach(() => {
    reset();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });
}

/** The provider registered with the API, behind its proxy — and behind the
 * follower a reset leaves there, the one it records into. */
export function registered(): TracerProvider {
  const delegate = (trace.getTracerProvider() as ProxyTracerProvider).getDelegate();
  return delegate === FOLLOWER ? (FOLLOWER.target ?? delegate) : delegate;
}

/** A provider of our own, carrying the package's processor, recording into
 * memory; the configuration is the suite's, whatever placeholder the capture uses. */
export function spans(): Capture {
  const captured = capture();
  adopt({ host: HOST, key: KEY });
  setLogger(intoWarnings);
  return captured;
}

/** A `fetch` that answers from a script, recording every call. */
export interface Call {
  method: string;
  url: string;
  headers: Record<string, string>;
  body: unknown;
}

export function fakeFetch(
  answer: (call: Call) => { status?: number; body?: unknown; headers?: Record<string, string> } | Error,
): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal('fetch', async (url: string, init: RequestInit) => {
    const call: Call = {
      method: init.method ?? 'GET',
      url,
      headers: { ...(init.headers as Record<string, string>) },
      body: typeof init.body === 'string' ? JSON.parse(init.body) : undefined,
    };
    calls.push(call);
    const reply = answer(call);
    if (reply instanceof Error) throw reply;
    const { status = 200, body, headers = {} } = reply;
    return new Response(body === undefined ? null : JSON.stringify(body), { status, headers });
  });
  return calls;
}
