import { redirect } from '@sveltejs/kit';
import { bareTarget } from '$lib/project.svelte';
import type { PageLoad } from './$types';

// The root is not a screen: the pre-authed URL the server prints lands here,
// and it goes to the remembered project's dashboard (spec 029 #3, spec 034
// #1) — or to the no-projects screen when the account reaches none. `await
// parent()` is what makes `me` readable here: the guard has run, or has
// redirected.
export const load: PageLoad = async ({ parent }) => {
	await parent();
	redirect(307, bareTarget('/dashboard'));
};
