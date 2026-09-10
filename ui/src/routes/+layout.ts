import { redirect } from '@sveltejs/kit';
import { auth, LOGIN_ROUTE, OUTSIDE_THE_SHELL, SETUP_ROUTE } from '$lib/auth.svelte';
import { bootstrap } from '$lib/session';
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
 * `setupRequired` stops mattering the moment somebody is signed in, which is
 * how setting up an owner leaves the screen it happened on: setup signs the
 * new owner in, so the next navigation is a session and not a redirect back.
 */
export const load: LayoutLoad = async ({ url }) => {
	if (!started) {
		started = true;
		theme.restore();
		setupRequired = (await bootstrap()).setupRequired;
	}
	if (auth.signedIn) return;
	const path = url.pathname;
	if (setupRequired) {
		if (path !== SETUP_ROUTE) redirect(307, SETUP_ROUTE);
		return;
	}
	if (!OUTSIDE_THE_SHELL.includes(path)) {
		redirect(307, `${LOGIN_ROUTE}?next=${encodeURIComponent(path + url.search)}`);
	}
};
