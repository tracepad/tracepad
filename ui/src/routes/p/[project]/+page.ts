import { redirect } from '@sveltejs/kit';
import { auth } from '$lib/auth.svelte';
import { under } from '$lib/project.svelte';
import type { PageLoad } from './$types';

// `/p/{id}` with no section is the project's traces (spec 029 #4) — when the
// account reaches it. Otherwise nothing is redirected and the layout's
// not-there screen is what renders.
export const load: PageLoad = async ({ parent, params }) => {
	await parent();
	if (auth.projects.some((one) => one.id === params.project)) {
		redirect(307, under('/traces', params.project));
	}
};
