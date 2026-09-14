/**
 * The examples in `docs/sdk-js.md` compile (spec 032, Testing).
 *
 * Every ```ts fence is a module of its own, given `tracepad` (this package's
 * source) and a handful of ambient names an example is allowed to assume —
 * a client, its messages, a logger — and type-checked under the package's
 * own strictness. A doc that shows a signature the package does not have
 * fails here rather than on a reader.
 */

import { readFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import ts from 'typescript';
import { expect, test } from 'vitest';

const PACKAGE = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const DOC = join(PACKAGE, '..', '..', 'docs', 'sdk-js.md');

/** What an example may take for granted without declaring it. */
const AMBIENT = `
type Completion = { model: string; choices: { message: { content: string } }[]; usage?: unknown };
type Chunk = { choices: { delta: { content?: string | null } }[]; usage?: unknown };
declare const client: {
  chat: {
    completions: {
      create(request: { stream: true } & Record<string, unknown>): Promise<AsyncIterable<Chunk>>;
      create(request: Record<string, unknown>): Promise<Completion>;
    };
  };
};
declare const messages: { role: string; content: string }[];
declare const query: string;
declare function search(query: string): Promise<string[]>;
declare const text: string;
declare const log: { warn(message: string): void };
declare const yourProcessor: import('@opentelemetry/sdk-trace-node').SpanProcessor;
declare const call: import('tracepad').Generation;
`;

function examples(): { line: number; code: string }[] {
  const lines = readFileSync(DOC, 'utf8').split('\n');
  const found: { line: number; code: string }[] = [];
  let open: number | undefined;
  let code: string[] = [];
  lines.forEach((text, index) => {
    if (open === undefined) {
      if (text.trim() === '```ts') {
        open = index + 1;
        code = [];
      }
    } else if (text.trim() === '```') {
      found.push({ line: open, code: code.join('\n') });
      open = undefined;
    } else {
      code.push(text);
    }
  });
  return found;
}

test('every ```ts example in docs/sdk-js.md type-checks against the package', () => {
  const blocks = examples();
  expect(blocks.length).toBeGreaterThan(5);

  const files = new Map<string, string>();
  for (const { line, code } of blocks) {
    const imports = code.includes("from 'tracepad'") ? '' : "import * as tracepad from 'tracepad';\n";
    files.set(join(PACKAGE, `docs-example-${line}.ts`), `${AMBIENT}${imports}${code}\n`);
  }

  const options: ts.CompilerOptions = {
    strict: true,
    target: ts.ScriptTarget.ES2022,
    module: ts.ModuleKind.NodeNext,
    moduleResolution: ts.ModuleResolutionKind.NodeNext,
    lib: ['lib.es2022.d.ts'],
    types: ['node'],
    skipLibCheck: true,
    noEmit: true,
    baseUrl: PACKAGE,
    paths: { tracepad: ['src/index.ts'] },
  };
  const host = ts.createCompilerHost(options);
  const readFile = host.readFile.bind(host);
  const fileExists = host.fileExists.bind(host);
  host.readFile = (name) => files.get(name) ?? readFile(name);
  host.fileExists = (name) => files.has(name) || fileExists(name);

  const program = ts.createProgram([...files.keys()], options, host);
  const problems = ts.getPreEmitDiagnostics(program).map((d) => {
    const where = d.file ? `${d.file.fileName.replace(PACKAGE, '')}:${d.start}` : '';
    return `${where} ${ts.flattenDiagnosticMessageText(d.messageText, '\n')}`;
  });
  expect(problems).toEqual([]);
});
