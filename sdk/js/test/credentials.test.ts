/** The key and the path: where the one goes, and what the other names. */

import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { inspect } from 'node:util';

import { afterEach, describe, expect, test } from 'vitest';

import { current, resolve } from '../src/config.js';
import { request } from '../src/http.js';
import * as tracepad from '../src/index.js';
import { HOST, KEY, fakeFetch, fresh } from './helpers.js';

fresh();

describe('the key', () => {
  test('is read but never listed: not in inspect, console.log or JSON', () => {
    const config = resolve({ host: HOST, key: KEY });

    expect(config.key).toBe(KEY);
    expect(inspect(config, { depth: 5 })).not.toContain(KEY);
    expect(JSON.stringify(config)).not.toContain(KEY);
    expect(Object.keys(config)).toEqual(['host']);
  });

  test('survives init after spanProcessor, which spreads the configuration', () => {
    tracepad.spanProcessor({ host: HOST, key: KEY, export: false });
    tracepad.init();

    expect(current().key).toBe(KEY);
  });

  interface Seen {
    path: string;
    authorization: string | undefined;
  }
  const servers: Server[] = [];
  afterEach(async () => {
    await Promise.all(servers.splice(0).map((s) => new Promise((done) => s.close(done))));
  });
  async function store(hopTo = ''): Promise<{ url: string; seen: Seen[] }> {
    const seen: Seen[] = [];
    const server = createServer((req, res) => {
      seen.push({ path: req.url!, authorization: req.headers.authorization });
      if (req.url!.startsWith('/hop/')) res.writeHead(302, { Location: hopTo + req.url!.slice(4) }).end();
      else res.writeHead(200, { 'Content-Type': 'application/json' }).end('{}');
    });
    servers.push(server);
    await new Promise<void>((listening) => server.listen(0, '127.0.0.1', listening));
    return { url: `http://127.0.0.1:${(server.address() as AddressInfo).port}`, seen };
  }

  test('arrives nowhere a redirect to another origin points: fetch drops it, and this pins that', async () => {
    const elsewhere = await store();
    const origin = await store(elsewhere.url);

    await request({ host: origin.url, key: KEY }, 'GET', '/hop/api/v1/prompts/n');

    expect(origin.seen).toEqual([{ path: '/hop/api/v1/prompts/n', authorization: `Bearer ${KEY}` }]);
    expect(elsewhere.seen).toEqual([{ path: '/api/v1/prompts/n', authorization: undefined }]);
  });
});

describe('a name in a path', () => {
  test.each([
    ['x?confirm=x#', 'x%3Fconfirm%3Dx%23'],
    ['a/b', 'a%2Fb'],
    ['50%', '50%25'],
    ['with space', 'with%20space'],
    ['ünï', '%C3%BCn%C3%AF'],
  ])('%j is one segment', async (name, segment) => {
    const calls = fakeFetch(() => ({ body: { name, version: 1, type: 'text', prompt: 'hi', labels: [] } }));
    tracepad.init({ host: HOST, key: KEY, export: false });

    await tracepad.dataset(name).delete('');
    await tracepad.prompt(name);
    await tracepad.scoreConfigs([{ name, data_type: 'boolean' }]);
    await tracepad.compare(name, name);

    expect(calls.map((c) => [c.method, c.url.replace(HOST, '')])).toEqual([
      ['DELETE', `/api/v1/datasets/${segment}?confirm=`],
      ['GET', `/api/v1/prompts/${segment}`],
      ['PUT', `/api/v1/score-configs/${segment}`],
      ['GET', `/api/v1/runs/${segment}/compare/${segment}`],
    ]);
  });

  test.each(['', '.', '..'])('%j is refused before the wire', async (name) => {
    const calls = fakeFetch(() => ({ body: {} }));
    tracepad.init({ host: HOST, key: KEY, export: false });

    await expect(tracepad.dataset(name).delete(name)).rejects.toThrow(/empty or dot segment/);
    await expect(tracepad.prompt(name)).rejects.toThrow(/empty or dot segment/);
    expect(calls).toEqual([]);
  });
});
