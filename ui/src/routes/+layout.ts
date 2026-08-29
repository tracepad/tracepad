import { redirect } from '@sveltejs/kit';
import { admin } from '$lib/admin.svelte';
import { auth, LOGIN_ROUTE } from '$lib/auth.svelte';
import { theme } from '$lib/theme.svelte';
import type { LayoutLoad } from './$types';

// A pure SPA (spec 006 Decision 1): the Go binary serves static files and the
// browser does all the routing. Nothing is prerendered, because every page is
// a view of data only the running server has.
export const ssr = false;
export const prerender = false;

let started = false;

/**
 * The one route guard. Every screen reads project data, so an unauthenticated
 * visit is sent to the login form with where it was going, and comes back
 * there (spec 006 #8).
 */
export const load: LayoutLoad = ({ url }) => {
	if (!started) {
		started = true;
		// Before the first navigation decision: a pre-authed URL has to
		// count as being signed in, or the link the server printed would
		// bounce off this guard.
		auth.restore();
		// The second credential, restored beside the first — the Settings
		// screen should not ask for it again on every reload (spec 007 #3).
		admin.restore();
		theme.restore();
	}
	if (!auth.authenticated && url.pathname !== LOGIN_ROUTE) {
		const next = url.pathname + url.search;
		redirect(307, `${LOGIN_ROUTE}?next=${encodeURIComponent(next)}`);
	}
};
