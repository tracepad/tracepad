import { redirect } from '@sveltejs/kit';
import { auth } from '$lib/auth.svelte';
import { project } from '$lib/project.svelte';
import type { LayoutLoad } from './$types';

// Everything inside the shell is under here (spec 029 #1). Opening a screen
// under `/p/{id}` remembers `{id}` for the next bare path (#3) — while the
// account can reach it: an id it cannot is the not-there screen, which the
// layout renders, and not a project to come back to. An account that reaches
// nothing at all is sent to `/p` (#4), where the one thing it can do is
// waiting. `await parent()` is what makes `me` readable here rather than racy.
export const load: LayoutLoad = async ({ parent, params }) => {
	await parent();
	if (auth.projects.length === 0) redirect(307, '/p');
	if (auth.projects.some((one) => one.id === params.project)) project.remember(params.project);
};
