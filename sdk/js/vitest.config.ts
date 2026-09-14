import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    include: ['test/**/*.test.ts'],
    // Process-wide state — the global provider, the score queue, `init` —
    // is reset between tests inside one process; across files it must not
    // leak, so every file is its own worker.
    isolate: true,
    testTimeout: 20_000,
    hookTimeout: 60_000,
  },
});
