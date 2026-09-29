/** `init` adapts to the provider it finds (spec 032 #2). */

import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';

import { trace } from '@opentelemetry/api';
import { NodeTracerProvider } from '@opentelemetry/sdk-trace-node';
import { BasicTracerProvider as BasicTracerProviderV1, InMemorySpanExporter, SimpleSpanProcessor, type SpanProcessor as SpanProcessorV1 } from 'sdk-trace-base-v1';
import { afterEach, describe, expect, test, vi } from 'vitest';

import { resolve } from '../src/config.js';
import * as tracepad from '../src/index.js';
import { HOST, KEY, fresh, registered, warnings } from './helpers.js';

fresh();

describe('a provider that takes processors after the fact (the 1.x line)', () => {
  test('is adopted through addSpanProcessor, and none is built', () => {
    const provider = new BasicTracerProviderV1();
    const exporter = new InMemorySpanExporter();
    provider.addSpanProcessor(new SimpleSpanProcessor(exporter));
    const added = vi.spyOn(provider, 'addSpanProcessor');
    trace.setGlobalTracerProvider(provider);

    tracepad.init({ host: HOST, key: KEY, export: false });

    expect(added).toHaveBeenCalledTimes(1);
    expect(added.mock.calls[0]![0]).toMatchObject({ onStart: expect.any(Function), onEnd: expect.any(Function) });
    expect(registered()).toBe(provider);
    // And our spans go through its pipeline.
    tracepad.span('step', () => undefined);
    expect(exporter.getFinishedSpans().map((s) => s.name)).toEqual(['step']);
    expect(warnings).toEqual([]);
  });

  test('exports through it: a 1.x span is read the way the 2.x exporter reads one', async () => {
    const { host, received } = await collector();
    const provider = new BasicTracerProviderV1();
    trace.setGlobalTracerProvider(provider);
    tracepad.init({ host, key: KEY });

    const parent = tracepad.span('parent-step', (step) => {
      tracepad.span('child-step', () => undefined);
      return step.spanId!;
    });
    await tracepad.flush();

    expect(received).toHaveLength(1);
    const body = received[0]!.body.toString('latin1');
    expect(body).toContain('parent-step');
    expect(body).toContain('child-step');
    // The scope, and the parent link: the two facts a 1.x span names differently.
    expect(body).toContain('tracepad');
    expect(received[0]!.body.includes(Buffer.from(parent, 'hex'))).toBe(true);
    expect(warnings).toEqual([]);
  });

  test('init after spanProcessor() attaches nothing more: the 1.x hook is not a second exporter', async () => {
    const { host, received } = await collector();
    // The 2.x type on a 1.x provider: the processor's `onEnd` reads either line's span.
    const ours = tracepad.spanProcessor({ host, key: KEY }) as unknown as SpanProcessorV1;
    const provider = new BasicTracerProviderV1({ spanProcessors: [ours] });
    const added = vi.spyOn(provider, 'addSpanProcessor');
    trace.setGlobalTracerProvider(provider);
    tracepad.init();

    tracepad.span('once', () => undefined);
    await tracepad.flush();

    expect(added).not.toHaveBeenCalled();
    expect(received).toHaveLength(1);
    expect(warnings).toEqual([]);
  });

  test('refuses environment and release with a warning: the resource is fixed', () => {
    trace.setGlobalTracerProvider(new BasicTracerProviderV1());
    tracepad.init({ host: HOST, key: KEY, environment: 'prod', export: false });
    expect(warnings).toEqual([expect.stringContaining('OTEL_RESOURCE_ATTRIBUTES')]);
  });
});

describe('a provider that takes processors in its constructor only (the 2.x line)', () => {
  test('is refused with a warning that names spanProcessor()', () => {
    new NodeTracerProvider().register();
    tracepad.init({ host: HOST, key: KEY });
    expect(warnings).toEqual([expect.stringContaining('pass tracepad.spanProcessor() to it')]);
  });

  test('spanProcessor() refuses environment and release the way init does', () => {
    tracepad.spanProcessor({ host: HOST, key: KEY, release: '1.0', export: false });
    expect(warnings).toEqual([expect.stringMatching(/^tracepad: spanProcessor\(\): environment and release are resource attributes/)]);
  });

  test('is silent when spanProcessor() was handed to it, and a bare init adopts its configuration', () => {
    new NodeTracerProvider({ spanProcessors: [tracepad.spanProcessor({ host: HOST, key: KEY })] }).register();
    tracepad.init(); // no arguments, and no TRACEPAD_* in the environment
    expect(warnings).toEqual([]);
    expect(() => tracepad.score('x', 1, { traceId: 'a'.repeat(32) })).not.toThrow();
  });

  test('an init that disagrees with spanProcessor() about the host says so', () => {
    new NodeTracerProvider({ spanProcessors: [tracepad.spanProcessor({ host: HOST, key: KEY })] }).register();
    tracepad.init({ host: 'http://elsewhere:4318' });
    expect(warnings).toEqual([expect.stringContaining('differs from the one spanProcessor() was built with')]);
  });
});

/** An OTLP collector that keeps what it was sent. */
interface Received {
  path: string;
  authorization: string | undefined;
  body: Buffer;
}
let server: Server | undefined;
afterEach(async () => {
  await new Promise((done) => (server === undefined ? done(undefined) : server.close(done)));
  server = undefined;
});
async function collector(): Promise<{ host: string; received: Received[] }> {
  const received: Received[] = [];
  server = createServer((request, response) => {
    const chunks: Buffer[] = [];
    request.on('data', (chunk: Buffer) => chunks.push(chunk));
    request.on('end', () => {
      received.push({ path: request.url!, authorization: request.headers.authorization, body: Buffer.concat(chunks) });
      response.writeHead(200).end();
    });
  });
  await new Promise<void>((listening) => server!.listen(0, '127.0.0.1', listening));
  return { host: `http://127.0.0.1:${(server.address() as AddressInfo).port}`, received };
}

describe('no provider', () => {
  test('builds one, registers it, and exports to {host}/v1/traces with the bearer', async () => {
    const { host, received } = await collector();
    process.title = 'support-bot';

    tracepad.init({ host, key: KEY, environment: 'production', release: '2026.9.4' });
    expect(registered()).toBeInstanceOf(NodeTracerProvider);
    tracepad.span('answer-question', () => undefined);
    await tracepad.flush();

    expect(received).toHaveLength(1);
    const [batch] = received;
    expect(batch!.path).toBe('/v1/traces');
    expect(batch!.authorization).toBe(`Bearer ${KEY}`);
    // Protobuf strings are the bytes themselves: the resource and the span
    // are readable in the body without decoding it.
    const body = batch!.body.toString('latin1');
    for (const expected of ['answer-question', 'service.name', 'support-bot', 'deployment.environment.name', 'production', 'service.version', '2026.9.4']) {
      expect(body).toContain(expected);
    }
    expect(warnings).toEqual([]);
  });

  test('is told apart by identity, not by a class name a minifier renames', () => {
    trace.disable(); // a process no test reset ran in: the API's own no-op
    const noop = registered();
    const named = Object.getOwnPropertyDescriptor(noop.constructor, 'name')!;
    Object.defineProperty(noop.constructor, 'name', { value: 'ln', configurable: true });
    try {
      tracepad.init({ host: HOST, key: KEY, export: false });
    } finally {
      Object.defineProperty(noop.constructor, 'name', named);
    }
    expect(registered()).toBeInstanceOf(NodeTracerProvider);
    expect(warnings).toEqual([]);
  });

  test('names the service from OTEL_SERVICE_NAME before the process title', () => {
    process.env.OTEL_SERVICE_NAME = 'named-by-env';
    tracepad.init({ host: HOST, key: KEY, export: false });
    const resource = (registered() as unknown as { _resource: { attributes: Record<string, unknown> } })._resource;
    expect(resource.attributes['service.name']).toBe('named-by-env');
  });
});

test('a second init is a no-op with a warning', () => {
  tracepad.init({ host: HOST, key: KEY, export: false });
  const first = registered();
  tracepad.init({ host: 'http://elsewhere', key: KEY, export: false });
  expect(registered()).toBe(first);
  expect(warnings).toEqual(['tracepad: init() has already run; this call is a no-op']);
});

describe('configuration', () => {
  test('with no host and no key throws TracepadConfigError', () => {
    expect(() => tracepad.init()).toThrow(tracepad.TracepadConfigError);
    expect(() => tracepad.init()).toThrow('no host and no key');
    expect(() => tracepad.init({ host: HOST })).toThrow('no key;');
  });

  test('reads the environment, and the arguments win over it', () => {
    process.env.TRACEPAD_URL = 'http://from-env:4318/';
    process.env.TRACEPAD_API_KEY = 'tp-sk-env';
    process.env.TRACEPAD_ENVIRONMENT = 'staging';
    tracepad.init({ key: KEY, export: false });
    const resource = (registered() as unknown as { _resource: { attributes: Record<string, unknown> } })._resource;
    expect(resource.attributes['deployment.environment.name']).toBe('staging');
  });

  test('TRACEPAD_HOST is a deprecated synonym for TRACEPAD_URL: it works, warns once, and loses to it', () => {
    process.env.TRACEPAD_API_KEY = KEY;
    process.env.TRACEPAD_URL = 'http://from-url:4318/';
    process.env.TRACEPAD_HOST = 'http://from-host:4318';
    expect(resolve().host).toBe('http://from-url:4318');
    expect(warnings).toEqual([]);

    delete process.env.TRACEPAD_URL;
    expect(resolve().host).toBe('http://from-host:4318');
    expect(resolve().host).toBe('http://from-host:4318');
    expect(warnings).toEqual([
      'tracepad: TRACEPAD_HOST is deprecated; set TRACEPAD_URL, which the CLI and the server read too',
    ]);
    expect(resolve({ host: 'http://argument:4318' }).host).toBe('http://argument:4318');
    // An untyped caller's null falls back to the environment, as `??` had it.
    expect(resolve({ host: null as unknown as string }).host).toBe('http://from-host:4318');
  });

  test('spanProcessor applies the logger, so its deprecated-host warning goes there and is not spent', () => {
    process.env.TRACEPAD_API_KEY = KEY;
    process.env.TRACEPAD_HOST = 'http://from-host:4318';
    const lines: string[] = [];
    tracepad.spanProcessor({ logger: { warn: (m) => lines.push(m) } });
    expect(lines).toEqual([
      'tracepad: TRACEPAD_HOST is deprecated; set TRACEPAD_URL, which the CLI and the server read too',
    ]);
    expect(warnings).toEqual([]);
  });

  test('the logger override takes the warnings', () => {
    const lines: string[] = [];
    tracepad.init({ host: HOST, key: KEY, export: false, logger: { warn: (m) => lines.push(m) } });
    tracepad.update({ level: 'ERROR' });
    expect(lines).toEqual(['tracepad: update() outside a span: nothing was written']);
  });
});
