import { error, redirect } from '@sveltejs/kit';
import { bareTarget, within } from '$lib/project.svelte';
import type { PageLoad } from './$types';

// A bare path — `/traces?status=error`, from a bookmark, a chat, or a `next=`
// the login form wrote — redirects to the same path and query under the
// remembered project (spec 029 #3). Every link written before the prefix
// keeps working, and lands where the person was last looking.
//
// A rest parameter matches last, after `/p/…` and the three screens outside
// the shell, so this is the paths that are none of those — which includes a
// path under the prefix that names no screen: `/p/{id}/nonsense`, or a bare
// `/nonsense` once it has been prefixed. That one is a 404, not another
// prefix on top of the first; the redirect is for paths that have none.
export const load: PageLoad = async ({ parent, url }) => {
	await parent();
	if (within(url.pathname) !== url.pathname) error(404);
	redirect(307, bareTarget(url.pathname, url.search));
};
