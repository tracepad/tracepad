import { defineConfig } from 'tsup';

// One entry, both module systems, one declaration file each (spec 032 #1).
// The OTel packages stay external: they are dependencies, not parts of the
// bundle, so that an application's instrumentation and this package share
// one copy of the SDK and one global provider.
export default defineConfig({
  entry: ['src/index.ts'],
  format: ['esm', 'cjs'],
  dts: true,
  sourcemap: true,
  clean: true,
  target: 'node20',
  platform: 'node',
});
