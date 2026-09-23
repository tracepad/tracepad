import { defineConfig } from 'tsup';

// Both module systems, one declaration file each (spec 032 #1). The OTel
// packages stay external: they are dependencies, not parts of the bundle, so
// that an application's instrumentation and this package share one copy of
// the SDK and one global provider. The second entry is `tracepad/testing`
// (spec 040 #5): split into shared chunks in both formats — CommonJS too —
// so that its `reset` reaches the same module state the root entry uses.
export default defineConfig({
  entry: ['src/index.ts', 'src/testing.ts'],
  format: ['esm', 'cjs'],
  splitting: true,
  dts: true,
  sourcemap: true,
  clean: true,
  target: 'node20',
  platform: 'node',
});
