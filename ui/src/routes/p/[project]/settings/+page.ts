import { redirect } from '@sveltejs/kit';
import { under } from '$lib/project.svelte';
import type { PageLoad } from './$types';

// `/p/{id}/settings` is the three tabs, and the first of them is the project
// on screen (spec 028, Application contract). Redirecting rather than
// rendering keeps one address per tab, which is what makes a tab a link.
export const load: PageLoad = ({ params }) => {
	redirect(307, under('/settings/project', params.project));
};
