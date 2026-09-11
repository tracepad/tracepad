import { redirect } from '@sveltejs/kit';
import { auth } from '$lib/auth.svelte';
import { under } from '$lib/project.svelte';
import type { PageLoad } from './$types';

// Owners only (spec 028 #14). The tab is absent for everybody else, and this
// is the other half of that: a link somebody was sent, or an address somebody
// typed, lands on the tab they do have rather than on a screen that would
// answer 403 to every request it made.
// `await parent()` is what makes this true rather than racy: SvelteKit runs a
// page load beside its layout's, so without it this would read `auth` while
// the shell's `me` was still in flight and redirect every direct visit.
export const load: PageLoad = async ({ parent, params }) => {
	await parent();
	if (!auth.owner) redirect(307, under('/settings/project', params.project));
};
