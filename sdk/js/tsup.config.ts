import { readdir, readFile, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
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
// below deletes in it, so it must be where the build wrote.
const outDir = fileURLToPath(new URL('./dist', import.meta.url));

export default defineConfig({
  entry: ['src/index.ts', 'src/testing.ts'],
  outDir,
  format: ['esm', 'cjs'],
  splitting: true,
  dts: true,
  sourcemap: true,
  clean: true,
  target: 'node20',
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
});
