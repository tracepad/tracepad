// Export a trace with the Tracepad Node package itself.
//
// The fifth exporter beside the plain OpenTelemetry SDK, the Langfuse SDK
// and the Python and Go packages (spec 032 #11). It is built from `sdk/js` in this
// checkout, so what it writes is what the mapper of this same commit has to
// read; the OTel SDK underneath is whatever the package's own lockfile pins.

import { createRequire } from 'node:module';
import { writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const require = createRequire(join(dirname(fileURLToPath(import.meta.url)), '..', '..', 'sdk', 'js', 'package.json'));
const tracepad = require('./dist/index.cjs');

const [traceIdFile] = process.argv.slice(2);

tracepad.init({
  host: process.env.SMOKE_HOST,
  key: process.env.SMOKE_SECRET_KEY,
  environment: 'smoke',
  release: 'smoke-2',
});

const RESPONSE = {
  model: 'claude-sonnet-5-2026-08-01',
  choices: [{ message: { role: 'assistant', content: 'pong' } }],
  usage: { prompt_tokens: 12, completion_tokens: 6, cost: 0.0004 },
};

let traceId = '';
const workflow = tracepad.observe(
  (question) => {
    tracepad.updateTrace({
      name: 'tracepad-js-smoke',
      userId: 'smoke-user',
      sessionId: 'smoke-session',
      tags: ['smoke', 'spec-032'],
      metadata: { suite: 'smoke' },
    });
    tracepad.generation(
      'smoke-generation',
      {
        model: 'claude-sonnet-5',
        modelParameters: { temperature: 0.1, max_tokens: 96 },
        input: [{ role: 'user', content: question }],
      },
      (call) => {
        traceId = call.traceId;
        call.firstToken();
        call.end(RESPONSE);
      },
    );
    return 'pong';
  },
  { name: 'smoke-workflow' },
);

workflow('ping');
await tracepad.flush();

writeFileSync(traceIdFile, traceId);
console.log(`tracepad-js trace ${traceId} exported to ${process.env.SMOKE_HOST}`);
