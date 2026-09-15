import { redirect } from '@sveltejs/kit';
import { under } from '$lib/project.svelte';
import type { PageLoad } from './$types';

// `/stats` is the dashboard's old address (spec 034 #1): every link and
// bookmark that names it lands on `/dashboard` with its query kept, so a
// window somebody shared is still the window they shared.
export const load: PageLoad = ({ params, url }) => {
	redirect(307, under(`/dashboard${url.search}`, params.project));
};
