import { redirect } from '@sveltejs/kit';
import { auth, INVITE_ROUTE, LOGIN_ROUTE, OUTSIDE_THE_SHELL, SETUP_ROUTE } from '$lib/auth.svelte';
import { bootstrap, needsSetup } from '$lib/session';
import { theme } from '$lib/theme.svelte';
import type { LayoutLoad } from './$types';

// A pure SPA (spec 006 Decision 1): the Go binary serves static files and the
// browser does all the routing. Nothing is prerendered, because every page is
// a view of data only the running server has.
export const ssr = false;
export const prerender = false;

let started = false;
/** Whether this server is still waiting for its first owner (Decision 9). */
let setupRequired = false;

/**
 * The one route guard, with the three outcomes of the Application contract: a
 * server with no owner sends everybody to `/setup`, nobody signed in goes to
 * `/login` with where they were going, and anything else renders the shell.
 *
 * The flag is re-asked rather than trusted while it is `true`. A boot-time
 * "this server has no owner" stops being true the moment somebody uses the
 * setup screen, and the tab that read it can be a different tab: trusting it
 * sent a person who had just signed out to `/setup`, whose only way onwards
 * is `/login`, which sent them back — a loop nothing but a reload escaped.
 * Once the answer is `false` it is never asked again.
 *
 * An invitation is let through even then: an owner the admin token invited
 * before anybody used the setup link — the only way in under
 * `TRACEPAD_SETUP=off` (Decision 32) — still has no password until the link
 * is opened, so the server keeps saying it needs setting up.
 */
export const load: LayoutLoad = async ({ url }) => {
	if (!started) {
		started = true;
		theme.restore();
		setupRequired = (await bootstrap()).setupRequired;
	}
	if (auth.signedIn) return;
	const path = url.pathname;
	if (setupRequired) setupRequired = await needsSetup();
	if (setupRequired) {
		if (path !== SETUP_ROUTE && path !== INVITE_ROUTE) redirect(307, SETUP_ROUTE);
		return;
	}
	if (!OUTSIDE_THE_SHELL.includes(path)) {
		redirect(307, `${LOGIN_ROUTE}?next=${encodeURIComponent(path + url.search)}`);
	}
};
