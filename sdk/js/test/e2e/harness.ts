/**
 * A real binary on a temporary database, and the read API as a person calls it.
 *
 * `TRACEPAD_BINARY` names the binary; `scripts/sdk-js-test.sh` builds it and
 * sets it. Without it the end-to-end files skip, so `vitest` on its own
 * stays a unit run.
 */

import { spawn, type ChildProcess } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

export const BINARY = process.env.TRACEPAD_BINARY ?? '';
export const KEY = 'tp-sk-e2e-0000000000000000000000000000';

export interface Store {
  host: string;
  call(method: string, path: string, body?: unknown): Promise<unknown>;
  stop(): Promise<void>;
}

export async function serve(): Promise<Store> {
  const dataDir = mkdtempSync(join(tmpdir(), 'tracepad-sdk-js-'));
  const port = await freePort();
  const host = `http://127.0.0.1:${port}`;
  const process_ = spawn(BINARY, ['serve'], {
    env: {
      ...process.env,
      TRACEPAD_DATA_DIR: dataDir,
      TRACEPAD_LISTEN: `127.0.0.1:${port}`,
      TRACEPAD_PROJECTS: `e2e:tp-pk-e2e:${KEY}`,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let output = '';
  process_.stdout!.on('data', (chunk: Buffer) => (output += chunk.toString()));
  process_.stderr!.on('data', (chunk: Buffer) => (output += chunk.toString()));

  const store: Store = {
    host,
    async call(method, path, body) {
      const answer = await fetch(host + path, {
        method,
        headers: { Authorization: `Bearer ${KEY}`, 'Content-Type': 'application/json' },
        body: body === undefined ? null : JSON.stringify(body),
      });
      const text = await answer.text();
      if (!answer.ok) throw new HTTPError(answer.status, text);
      return text ? JSON.parse(text) : undefined;
    },
    async stop() {
      await terminate(process_);
      rmSync(dataDir, { recursive: true, force: true });
    },
  };
  try {
    await awaitHealth(store, process_, () => output);
  } catch (error) {
    await store.stop();
    throw error;
  }
  return store;
}

export class HTTPError extends Error {
  constructor(
    readonly status: number,
    body: string,
  ) {
    super(`HTTP ${status}: ${body}`);
  }
}

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const probe = createServer();
    probe.listen(0, '127.0.0.1', () => {
      const { port } = probe.address() as { port: number };
      probe.close(() => resolve(port));
    });
    probe.on('error', reject);
  });
}

async function awaitHealth(store: Store, process_: ChildProcess, output: () => string): Promise<void> {
  for (let i = 0; i < 100; i++) {
    if (process_.exitCode !== null) throw new Error(`the server exited: ${output()}`);
    try {
      await store.call('GET', '/health');
      return;
    } catch {
      await sleep(100);
    }
  }
  throw new Error(`the server never became healthy: ${output()}`);
}

function terminate(process_: ChildProcess): Promise<void> {
  return new Promise((resolve) => {
    if (process_.exitCode !== null) return resolve();
    process_.once('exit', () => resolve());
    process_.kill('SIGTERM');
  });
}

export function sleep(ms: number): Promise<void> {
  return new Promise((tick) => setTimeout(tick, ms));
}

export interface Observation {
  name: string;
  type: string;
  level?: string;
  status_message?: string;
  model?: string;
  usage?: Record<string, number>;
  model_parameters?: Record<string, unknown>;
  prompt?: { name: string; version: number };
  ttft_ms?: number | null;
  input?: unknown;
  output?: unknown;
  metadata?: Record<string, unknown>;
  children?: Observation[];
}

export interface Trace {
  id: string;
  name: string;
  user_id?: string;
  session_id?: string;
  tags?: string[];
  environment?: string;
  release?: string;
  version?: string;
  total_cost?: number;
  error_count?: number;
  run_id?: string;
  item_id?: string;
  observations: Observation[];
}

/** The trace, once the export has landed. */
export async function traceOf(store: Store, traceId: string, expand = '?expand=io'): Promise<Trace> {
  for (let i = 0; i < 50; i++) {
    try {
      return (await store.call('GET', `/api/v1/traces/${traceId}${expand}`)) as Trace;
    } catch (error) {
      if (!(error instanceof HTTPError) || error.status !== 404) throw error;
      await sleep(100);
    }
  }
  throw new Error(`trace ${traceId} never arrived`);
}

/** The observation tree, flattened depth-first. */
export function walk(observations: Observation[]): Observation[] {
  return observations.flatMap((o) => [o, ...walk(o.children ?? [])]);
}
