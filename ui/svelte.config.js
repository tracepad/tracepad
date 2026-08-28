import adapter from '@sveltejs/adapter-static';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/**
 * A pure SPA (spec 006 Decision 1): no SSR, no prerendering, everything is
 * compiled to static files the Go binary embeds and serves from `/`.
 * `fallback` is the single HTML entry every deep link lands on.
 *
 * @type {import('@sveltejs/kit').Config}
 */
export default {
	preprocess: vitePreprocess(),
	kit: {
		adapter: adapter({ pages: 'dist', assets: 'dist', fallback: 'index.html' }),
		// The app is served from the binary's root; nothing is version-polled
		// because a reload is how a new binary's UI arrives.
		version: { pollInterval: 0 }
	}
};
