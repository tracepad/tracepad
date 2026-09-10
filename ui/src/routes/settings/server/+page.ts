import { redirect } from '@sveltejs/kit';
import { auth } from '$lib/auth.svelte';
import type { PageLoad } from './$types';

// Owners only (spec 028 #14). The tab is absent for everybody else, and this
// is the other half of that: a link somebody was sent, or an address somebody
// typed, lands on the tab they do have rather than on a screen that would
// answer 403 to every request it made.
export const load: PageLoad = () => {
	if (!auth.owner) redirect(307, '/settings/project');
};
