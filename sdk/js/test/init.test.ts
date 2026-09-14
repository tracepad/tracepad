/** `init` adapts to the provider it finds (spec 032 #2). */

import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';

import { trace } from '@opentelemetry/api';
import { NodeTracerProvider } from '@opentelemetry/sdk-trace-node';
import { BasicTracerProvider as BasicTracerProviderV1, InMemorySpanExporter, SimpleSpanProcessor } from 'sdk-trace-base-v1';
import { afterEach, describe, expect, test, vi } from 'vitest';

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

  test('is silent when spanProcessor() was handed to it', () => {
    new NodeTracerProvider({ spanProcessors: [tracepad.spanProcessor({ host: HOST, key: KEY })] }).register();
    tracepad.init({ host: HOST, key: KEY });
    expect(warnings).toEqual([]);
  });
});

describe('no provider', () => {
  let server: Server | undefined;
  afterEach(async () => {
    await new Promise((done) => (server === undefined ? done(undefined) : server.close(done)));
  });

  test('builds one, registers it, and exports to {host}/v1/traces with the bearer', async () => {
    const received: { path: string; authorization: string | undefined; body: Buffer }[] = [];
    server = createServer((request, response) => {
      const chunks: Buffer[] = [];
      request.on('data', (chunk: Buffer) => chunks.push(chunk));
      request.on('end', () => {
        received.push({ path: request.url!, authorization: request.headers.authorization, body: Buffer.concat(chunks) });
        response.writeHead(200).end();
      });
    });
    await new Promise<void>((listening) => server!.listen(0, '127.0.0.1', listening));
    const host = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
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
    process.env.TRACEPAD_HOST = 'http://from-env:4318/';
    process.env.TRACEPAD_API_KEY = 'tp-sk-env';
    process.env.TRACEPAD_ENVIRONMENT = 'staging';
    tracepad.init({ key: KEY, export: false });
    const resource = (registered() as unknown as { _resource: { attributes: Record<string, unknown> } })._resource;
    expect(resource.attributes['deployment.environment.name']).toBe('staging');
  });

  test('the logger override takes the warnings', () => {
    const lines: string[] = [];
    tracepad.init({ host: HOST, key: KEY, export: false, logger: { warn: (m) => lines.push(m) } });
    tracepad.update({ level: 'ERROR' });
    expect(lines).toEqual(['tracepad: update() outside a span: nothing was written']);
  });
});
