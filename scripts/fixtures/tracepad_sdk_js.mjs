#!/usr/bin/env node
// Write `testdata/otlp/013-tracepad-sdk-js.pb` from the package's own exporter.
//
// Every other fixture in the corpus is built by hand in `internal/otlptest`,
// which is the right shape for reproducing somebody else's SDK. This one is
// the export our *own* Node package produces (spec 032 #12): the bytes come
// off the wire of a real `BatchSpanProcessor` pointed at a fake collector, so
// the golden beside them is evidence that the attribute names the package
// writes are the ones the mapper's table reads — and the same ones the
// Python package writes (`010-tracepad-sdk.pb`). Run it through
// `make fixtures`, which then regenerates the golden from these bytes like
// any other body.
//
// Two things are pinned so that a re-run is a no-op rather than a diff:
//
// * the ids, by a fixed generator on the provider — this is also the
//   adaptation path of Decision 2, an application that already has a
//   provider and hands it `tracepad.spanProcessor()`;
// * the clocks, by replacing `Date.now` and `performance.now` — the two
//   readings the OTel SDK's span takes — with one virtual clock: it stands
//   still for the wall clock and moves a fixed step on every monotonic
//   reading while the application runs. Every instant in the export, the
//   completion start included, is then a rank on a grid, and the bytes are
//   the SDK's own with nothing rewritten.
//
// Everything else — the attribute names, their values, the resource, the
// scope, the protobuf framing — is what the package and the OTel SDK emitted.
//
// Needs the package built: `npm ci && npm run build` in `sdk/js`.

import { createServer } from 'node:http';
import { createRequire } from 'node:module';
import { writeFileSync } from 'node:fs';
import { dirname, join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..', '..');
const PACKAGE = join(ROOT, 'sdk', 'js');
const TARGET = join(ROOT, 'testdata', 'otlp', '013-tracepad-sdk-js.pb');

// The package and the OTel SDK it depends on, resolved from `sdk/js` so that
// this script has no dependencies of its own.
const require = createRequire(join(PACKAGE, 'package.json'));
const tracepad = require('./dist/index.cjs');
const { NodeTracerProvider } = require('@opentelemetry/sdk-trace-node');
const { resourceFromAttributes } = require('@opentelemetry/resources');

// 2026-08-26T10:00:00Z, the base every fixture in the corpus shares.
const BASE = 1787738400_000;
const STEP = 10; // 10 ms between one instant and the next

const ANSWER = {
  model: 'gpt-5-mini-2026-08-07',
  choices: [{ message: { role: 'assistant', content: 'When the last chunk does.' } }],
  usage: {
    prompt_tokens: 96,
    completion_tokens: 33,
    prompt_tokens_details: { cached_tokens: 64 },
    completion_tokens_details: { reasoning_tokens: 8 },
    cost: 0.0007,
  },
};

// The second trace's answer, on a model name unrelated to every other name
// in the corpus (see `tracepad_sdk.py` for why).
const REWRITTEN = {
  model: 'gpt-5-nano',
  choices: [{ message: { content: 'How does a stream end?' } }],
  usage: { prompt_tokens: 15, completion_tokens: 7 },
};

/** Ids counted up from one, so the fixture names the same spans forever. */
class FixedIds {
  traces = 0;
  spans = 0;
  generateTraceId() {
    return `a0b1c2d3e4f5a6b7c8d9eb${String(++this.traces).padStart(10, '0')}`;
  }
  generateSpanId() {
    return `2b3c4d5e6f70${String(++this.spans).padStart(4, '0')}`;
  }
}

/** The two traces the fixture is of: every row of the Ingest contract.
 *
 * The question, the answer, the prompt, the observation kind and the tags are
 * this fixture's own; the user and the session are the Python fixture's, so
 * that the two packages read as two services of one application rather than
 * as two more accounts in the corpus the interface's suite counts. */
function application() {
  const search = tracepad.observe(
    (query) => {
      tracepad.update({ level: 'WARNING', statusMessage: 'one result', metadata: { attempt: 2 } });
      return ['the streams page'];
    },
    { type: 'retriever', name: 'docs-search' },
  );
  // The wrapper reading a response, rather than a callback ending one.
  const rewrite = tracepad.observe(() => REWRITTEN, { type: 'generation', name: 'rewrite-question' });

  const docs = new tracepad.Prompt({ name: 'docs-answer', version: 4, type: 'text', text: 'Answer {topic}.' });
  const question = 'how does a stream end?';

  tracepad.span('answer-question', { input: { question } }, () => {
    tracepad.updateTrace({
      name: 'help-chat',
      userId: 'user-9001',
      sessionId: 'session-91',
      tags: ['docs', 'node'],
      metadata: { channel: 'cli' },
    });
    search('stream');
    tracepad.event('cache.miss', { metadata: { key: 'docs-answer' } }, () => undefined);
    tracepad.generation(
      'chat-completion',
      {
        model: 'gpt-5-mini',
        prompt: docs,
        modelParameters: { temperature: 0.2, max_tokens: 512 },
        input: [{ role: 'user', content: question }],
      },
      (call) => {
        call.firstToken();
        call.end(ANSWER);
      },
    );
  });

  // A second trace, on the wrapper's own path.
  tracepad.span('prepare-question', () => {
    tracepad.updateTrace({ name: 'help-rewrite', userId: 'user-9001', sessionId: 'session-91' });
    rewrite('how does stream end');
  });
}

/** Run `fn` on a clock that moves one step per monotonic reading, then restore the real one. */
function onTheGrid(fn) {
  let virtual = BASE;
  const real = { date: Date.now, performance: performance.now };
  Date.now = () => virtual;
  performance.now = () => {
    const at = virtual;
    virtual += STEP;
    return at - performance.timeOrigin;
  };
  try {
    fn();
  } finally {
    Date.now = real.date;
    performance.now = real.performance;
  }
}

async function main() {
  const bodies = [];
  const server = createServer((request, response) => {
    const chunks = [];
    request.on('data', (chunk) => chunks.push(chunk));
    request.on('end', () => {
      if (request.headers.authorization !== 'Bearer tp-sk-fixture') {
        throw new Error(`unexpected authorization: ${request.headers.authorization}`);
      }
      bodies.push(Buffer.concat(chunks));
      response.writeHead(200).end();
    });
  });
  await new Promise((listening) => server.listen(0, '127.0.0.1', listening));
  const host = `http://127.0.0.1:${server.address().port}`;

  // The application's own provider, which takes the package's processor in
  // its constructor (spec 032 #2). Its resource is written out rather than
  // discovered, so that the SDK's version does not churn the golden on every
  // regeneration.
  const provider = new NodeTracerProvider({
    resource: resourceFromAttributes({
      'service.name': 'docs-bot',
      'service.version': '2026.9.14',
      'deployment.environment.name': 'production',
      'telemetry.sdk.language': 'nodejs',
      'telemetry.sdk.name': 'opentelemetry',
    }),
    idGenerator: new FixedIds(),
    spanProcessors: [tracepad.spanProcessor({ host, key: 'tp-sk-fixture' })],
  });
  provider.register();
  tracepad.init({ host, key: 'tp-sk-fixture' });

  onTheGrid(application);
  await tracepad.flush({ timeout: 10_000 });
  await provider.shutdown();
  await new Promise((closed) => server.close(closed));

  if (bodies.length !== 1) {
    // One batch, or the spans would not be one body.
    console.error(`expected one export, got ${bodies.length}`);
    return 1;
  }
  writeFileSync(TARGET, bodies[0]);
  console.log(`wrote ${relative(ROOT, TARGET)} (${bodies[0].length} bytes)`);
  return 0;
}

process.exitCode = await main();
