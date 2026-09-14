/**
 * A clean process for every test.
 *
 * `init`, the global `TracerProvider` and the score queue are all
 * process-wide by design — the OTel API refuses to set a provider twice on
 * purpose — so the suite resets them between tests rather than pretending
 * they are not.
 */

import { context, trace, type ProxyTracerProvider, type TracerProvider } from '@opentelemetry/api';
import {
  InMemorySpanExporter,
  NodeTracerProvider,
  SimpleSpanProcessor,
  type ReadableSpan,
} from '@opentelemetry/sdk-trace-node';
import { afterEach, beforeEach, vi } from 'vitest';

import * as config from '../src/config.js';
import { setLogger } from '../src/log.js';
import * as prompts from '../src/prompts.js';
import * as scores from '../src/scores.js';
import * as tracing from '../src/tracing.js';

export const HOST = 'http://tracepad.test:4318';
export const KEY = 'tp-sk-test';

/** Every line the package warned with, in order. */
export const warnings: string[] = [];

export function fresh(): void {
  beforeEach(() => {
    for (const variable of [
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
    setLogger({ warn: (message) => warnings.push(message) });
    reset();
  });
  afterEach(() => {
    reset();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });
}

function reset(): void {
  trace.disable();
  context.disable();
  tracing.reset();
  config.forget();
  prompts.forget();
  scores.reset();
}

/** The provider registered with the API, behind its proxy. */
export function registered(): TracerProvider {
  return (trace.getTracerProvider() as ProxyTracerProvider).getDelegate();
}

/** A provider of our own, carrying the package's processor, exporting into memory. */
export function spans(): Spans {
  const exporter = new InMemorySpanExporter();
  const provider = new NodeTracerProvider({
    spanProcessors: [
      new SimpleSpanProcessor(exporter),
      tracing.spanProcessor({ host: HOST, key: KEY, export: false }),
    ],
  });
  provider.register();
  tracing.init({ host: HOST, key: KEY });
  return new Spans(exporter);
}

export class Spans {
  constructor(private readonly exporter: InMemorySpanExporter) {}

  all(): ReadableSpan[] {
    return this.exporter.getFinishedSpans();
  }

  one(name?: string): ReadableSpan {
    const found = this.all().filter((s) => name === undefined || s.name === name);
    if (found.length !== 1) {
      throw new Error(`want one span named ${name}, got ${this.all().map((s) => s.name)}`);
    }
    return found[0]!;
  }

  attributes(name?: string): Record<string, unknown> {
    return { ...this.one(name).attributes };
  }
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
