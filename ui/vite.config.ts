import { sveltekit } from '@sveltejs/kit/vite';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'vitest/config';

export default defineConfig({
	plugins: [tailwindcss(), sveltekit()],
	build: {
		// The bundle ships inside a binary that is downloaded once; a source
		// map would double its size for a debugging session nobody has.
		sourcemap: false
	},
	server: {
		// `npm run dev` talks to a locally running `tracepad serve`, so the
		// dev server behaves like the binary does: one origin, no CORS.
		proxy: { '/api': 'http://localhost:4318' }
	},
	// Component tests import Svelte the way a browser does; without this the
	// server condition wins and mounting a component fails.
	resolve: process.env.VITEST ? { conditions: ['browser'] } : undefined,
	test: {
		environment: 'jsdom',
		globals: true,
		include: ['src/**/*.test.ts'],
		setupFiles: ['src/tests/setup.ts']
	}
});
