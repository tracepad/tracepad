import { readdir, readFile, rm, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { defineConfig } from 'tsup';

// Both module systems, one declaration file each (spec 032 #1). The OTel
// packages stay external: they are dependencies, not parts of the bundle, so
// that an application's instrumentation and this package share one copy of
// the SDK and one global provider. The second entry is `tracepad/testing`
// (spec 040 #5): split into shared chunks in both formats — CommonJS too —
// so that its `reset` reaches the same module state the root entry uses.
//
// `dist` is found from this file, not from the working directory: the clean-up
// below deletes in it, so it must be where the build wrote. `--out-dir` on the
// command line overrides it — the suite builds into a directory of its own —
// and tsup hands the command line's flags to this function, so the clean-up
// follows it there, resolved from the working directory as tsup resolves it.
const dist = fileURLToPath(new URL('./dist', import.meta.url));

export default defineConfig((flags) => {
  const outDir = flags.outDir ? resolve(flags.outDir) : dist;
  return {
    entry: ['src/index.ts', 'src/testing.ts'],
    outDir,
    format: ['esm', 'cjs'],
    splitting: true,
    // The declarations are written by TypeScript 6's API (`typescript` is the
    // alias to `@typescript/typescript6`; `tsc` is 7, which has no stable API
    // yet), and tsup's declaration pass always sets `baseUrl`, an option 6.0
    // deprecates. Silenced for that pass only: the package's own tsconfig sets
    // no `baseUrl`, and `tsc` 7 checks it with nothing silenced.
    dts: { compilerOptions: { ignoreDeprecations: '6.0' } },
    sourcemap: true,
    clean: true,
    target: 'node22',
    platform: 'node',
    // Maps for the ESM build only: the CommonJS pass re-bundles tsup's split
    // output, and its map names that output by absolute path — the machine the
    // package was built on, published with it (spec 032 #18). The pass needs a
    // map to read, so the CommonJS ones are removed once the build is done.
    async onSuccess() {
      for (const name of await readdir(outDir, { recursive: true })) {
        const file = join(outDir, name);
        if (file.endsWith('.cjs.map')) await rm(file);
        else if (file.endsWith('.cjs')) {
          const code = await readFile(file, 'utf8');
          await writeFile(file, code.replace(/\n\/\/# sourceMappingURL=\S+\s*$/, '\n'));
        }
      }
    },
  };
});
