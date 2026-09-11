import { redirect } from '@sveltejs/kit';
import { bareTarget } from '$lib/project.svelte';
import type { PageLoad } from './$types';

// A bare path — `/traces?status=error`, from a bookmark, a chat, or a `next=`
// the login form wrote — redirects to the same path and query under the
// remembered project (spec 029 #3). Every link written before the prefix
// keeps working, and lands where the person was last looking.
//
// A rest parameter matches last, after `/p/…` and the three screens outside
// the shell, so this is exactly the paths that are none of those. A path that
// is no screen at all stays no screen under the prefix too, and 404s there.
export const load: PageLoad = async ({ parent, url }) => {
	await parent();
	redirect(307, bareTarget(url.pathname, url.search));
};
